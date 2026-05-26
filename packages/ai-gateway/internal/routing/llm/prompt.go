package llm

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/inputstaging"
)

// DefaultSystemPrompt is the built-in system message template used when
// SmartConfig.SystemPrompt is empty. Callers substitute {modelCatalog}
// before handing the result to Decider.Decide; the {modelCatalog}
// placeholder is intentionally not substituted inside this package
// because building the catalog requires data the smart strategy holds
// (its candidate Model rows).
const DefaultSystemPrompt = `You are an AI model router for an enterprise gateway. Select the best model for the user's request.

## Available Models
The catalog is compact JSON: p = provider id, m = models for that provider; each model has i = the model code (Model.code, e.g. "gpt-4o" — the only value you may return as modelId). Optional: ip/op = input/output USD per 1M tokens, f = capability tags, mx/mo = max context and max output tokens.

{modelCatalog}

## Selection Rules
1. Analyze the task: coding, analysis, creative writing, Q&A, translation, math, reasoning
2. Match capabilities: images → vision, tools → function_calling, long text → large context (use f, mx, and mo when deciding)
3. Cost: simple tasks → cheapest capable model; complex tasks → most capable
4. If uncertain, prefer the most capable model
5. modelId must match some catalog entry's i (Model.code) exactly—same characters. Do not return any id not listed as an i value.

## Output Format
Return ONLY valid JSON: {"modelId":"<exact ID from list>","reason":"<brief explanation>"}`

// requestBody is the chat-completions request body sent to the router
// LLM. Canonical OpenAI shape; the provider adapter translates per
// upstream wire format.
type requestBody struct {
	Model       string    `json:"model"`
	Messages    []message `json:"messages"`
	Temperature float64   `json:"temperature"`
	MaxTokens   int       `json:"max_tokens"`
	Stream      bool      `json:"stream"`
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// routerContextLimit is the default model context window assumed for the
// router LLM when no explicit limit is configured. Smart routing is a
// single-turn classification task, so a generous but bounded limit keeps
// the router-LLM prompt cost predictable.
const routerContextLimit = 8192

// routerReserveOutput is the number of tokens reserved for the router
// LLM's own response (a short JSON object). 256 is well above the
// typical router reply size ("{"modelId":"...","reason":"..."}") and
// leaves ~7 936 tokens for the conversation context.
const routerReserveOutput = 256

// BuildRequestBody constructs the OpenAI-shape body for the router-LLM
// call. SystemPrompt arrives pre-prepared (catalog already substituted
// by the caller); UserMessages have already been filtered to role=user
// by the smart strategy. The router LLM prompt is "here is the
// model catalog (system) and here is what the user asked for (user
// messages); pick the best target".
//
// Each normcore.Message can carry multimodal content blocks; this
// builder concatenates ContentText blocks per message and elides
// non-text content (image_ref, tool_use, tool_result, reasoning) —
// the router does not benefit from seeing binary refs or tool plumbing
// and ignoring them keeps the prompt small.
//
// Truncation uses inputstaging.Plan with StrategyLastUser: the router
// LLM is a classification task that needs only the most recent user
// question. On overflow (the single user message alone exceeds the
// budget), a warn is logged and the message is used as-is — the
// smart strategy's fallback path recovers on the next request cycle.
func BuildRequestBody(routerProviderModelID string, req Request) requestBody {
	return buildRequestBodyWithLogger(routerProviderModelID, req, slog.Default())
}

// buildRequestBodyWithLogger is the testable implementation; tests supply a
// discard logger to suppress output while still exercising the overflow path.
func buildRequestBodyWithLogger(routerProviderModelID string, req Request, logger *slog.Logger) requestBody {
	systemPrompt := req.SystemPrompt
	if systemPrompt == "" {
		systemPrompt = DefaultSystemPrompt
	}

	messages := []message{
		{Role: "system", Content: systemPrompt},
	}

	// Project user messages to flat text, dropping empty projections.
	stagingMsgs := make([]inputstaging.Message, 0, len(req.UserMessages))
	for _, m := range req.UserMessages {
		text := textOf(m)
		if text == "" {
			continue
		}
		stagingMsgs = append(stagingMsgs, inputstaging.Message{Role: "user", Content: text})
	}

	if len(stagingMsgs) == 0 {
		return requestBody{
			Model:       routerProviderModelID,
			Messages:    messages,
			Temperature: req.Temperature,
			MaxTokens:   req.MaxTokens,
			Stream:      false,
		}
	}

	// Apply inputstaging.Plan with StrategyLastUser: the router LLM
	// makes a classification decision on what the user most recently
	// asked for; it does not need the full conversation history.
	plan, planErr := inputstaging.Plan(inputstaging.PlanInput{
		Messages:          stagingMsgs,
		ModelContextLimit: routerContextLimit,
		Strategy:          inputstaging.StrategyLastUser,
		ReserveOutput:     routerReserveOutput,
	})
	if planErr != nil {
		// Strategy is a constant so this branch is unreachable in practice;
		// guard defensively and fall through with whatever messages we have.
		logger.Warn("smart: inputstaging.Plan error", "error", planErr)
	} else if plan.OverflowKind != inputstaging.OverflowNone {
		logger.Warn("smart: router-LLM input overflow after inputstaging",
			"overflow_kind", string(plan.OverflowKind),
			"input_tokens", plan.InputTokens,
			"budget", routerContextLimit-routerReserveOutput,
		)
	}

	var planned []message
	if planErr == nil && len(plan.Messages) > 0 {
		for _, m := range plan.Messages {
			planned = append(planned, message{Role: m.Role, Content: m.Content})
		}
	} else {
		// Fallback: use all staged messages as-is.
		for _, m := range stagingMsgs {
			planned = append(planned, message{Role: m.Role, Content: m.Content})
		}
	}
	messages = append(messages, planned...)

	return requestBody{
		Model:       routerProviderModelID,
		Messages:    messages,
		Temperature: req.Temperature,
		MaxTokens:   req.MaxTokens,
		Stream:      false,
	}
}

// textOf concatenates every ContentText block inside a canonical
// Message into a single string. Non-text content (image_ref, tool_use,
// tool_result, reasoning) is skipped — the router LLM prompt is flat
// chat and gains nothing from seeing binary refs or tool plumbing.
func textOf(m normcore.Message) string {
	var b strings.Builder
	for _, c := range m.Content {
		if c.Type == normcore.ContentText && c.Text != "" {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(c.Text)
		}
	}
	return b.String()
}

// codeBlockRe matches markdown code blocks (```json ... ``` or ``` ... ```).
var codeBlockRe = regexp.MustCompile("(?s)```(?:json)?\\s*\n?(.*?)\n?```")

// modelIDRe extracts modelId and reason from a JSON-like pattern as a
// last-resort fallback when the router LLM returns non-JSON text.
var modelIDRe = regexp.MustCompile(`\{\s*"modelId"\s*:\s*"([^"]+)"\s*,\s*"reason"\s*:\s*"([^"]*?)"\s*\}`)

// ParseResponse extracts a Decision from the router-LLM's response
// body (the raw chat-completions response). Tries: direct JSON parse of
// the content -> markdown code-block extraction -> regex (modelId only).
func ParseResponse(respBody string) (Decision, error) {
	var resp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(respBody), &resp); err != nil {
		return Decision{}, fmt.Errorf("failed to parse response envelope: %w", err)
	}
	if len(resp.Choices) == 0 {
		return Decision{}, fmt.Errorf("no choices in response")
	}
	content := strings.TrimSpace(resp.Choices[0].Message.Content)
	if content == "" {
		return Decision{}, fmt.Errorf("empty content in response")
	}

	if d, ok := tryParseRouterJSON(content); ok {
		return d, nil
	}

	matches := codeBlockRe.FindStringSubmatch(content)
	if len(matches) >= 2 {
		if d, ok := tryParseRouterJSON(strings.TrimSpace(matches[1])); ok {
			return d, nil
		}
	}

	regexMatches := modelIDRe.FindStringSubmatch(content)
	if len(regexMatches) >= 3 {
		return Decision{ModelID: regexMatches[1], Reason: regexMatches[2]}, nil
	}

	return Decision{}, fmt.Errorf("could not extract modelId from router response")
}

type routerJSONResponse struct {
	ModelID    string `json:"modelId"`
	ProviderID string `json:"providerId,omitempty"`
	Reason     string `json:"reason"`
}

func tryParseRouterJSON(s string) (Decision, bool) {
	var r routerJSONResponse
	if err := json.Unmarshal([]byte(s), &r); err != nil {
		return Decision{}, false
	}
	if r.ModelID == "" {
		return Decision{}, false
	}
	if r.Reason == "" {
		r.Reason = "no reason provided"
	}
	return Decision(r), true
}

package llm

import (
	"fmt"
	"github.com/goccy/go-json"
	"log/slog"
	"regexp"
	"strings"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/inputstaging"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// DefaultSystemPrompt is the built-in system message template used when
// SmartConfig.SystemPrompt is empty. Callers substitute {modelCatalog}
// before handing the result to Decider.Decide; the {modelCatalog}
// placeholder is intentionally not substituted inside this package
// because building the catalog requires data the smart strategy holds
// (its candidate Model rows).
const DefaultSystemPrompt = `You are an AI model router for an enterprise gateway. Select the best model for the user's request.

## Available Models
The catalog is compact JSON: p = provider id, m = models for that provider; each model has i = the model code (Model.code, e.g. "gpt-4o" — the only value you may return as modelId). Optional: ip/op = input/output USD per 1M tokens, f = capability tags, in = input modalities the model accepts BEYOND text (absent means text only), mx/mo = max context and max output tokens.

{modelCatalog}

## Selection Rules
1. Analyze the task: coding, analysis, creative writing, Q&A, translation, math, reasoning
2. Match capabilities: images → in contains image, audio → in contains audio, tools → function_calling, long text → large context (use in, f, mx, and mo when deciding)
3. Cost: simple tasks → cheapest capable model; complex tasks → most capable
4. If uncertain, prefer the most capable model
5. Recency: within each provider, models are listed newest-generation-first. When two candidates are equally capable and both fit, prefer the newest — the one listed earlier, i.e. the higher version number in its code i (e.g. prefer -4-8 over -4-7 over -4-6, and 5.5 over 5.4)
6. modelId must match some catalog entry's i (Model.code) exactly—same characters. Do not return any id not listed as an i value.

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

// routerDefaultContextLimit is the context window assumed for the router
// LLM only when the router model declares no maxContextTokens. When the
// catalog carries the real window it arrives via Request.RouterContextLimit.
const routerDefaultContextLimit = 8192

// routerReserveOutput is the number of tokens reserved for the router
// LLM's own response (a short JSON object). 256 is well above the
// typical router reply size ("{"modelId":"...","reason":"..."}").
const routerReserveOutput = 256

// routerInputCap bounds the conversation tokens sent to the router
// regardless of how large the router model's window is: routing is a
// classification task whose signal saturates quickly, so a giant router
// window must not inflate per-request router cost.
const routerInputCap = 4096

// routerMinInputBudget is the conversation budget floor when the system
// prompt (catalog + rules) consumes the router window. Some user content
// always ships — the call may still fail upstream, but the trace and the
// overflow metric surface the misconfiguration.
const routerMinInputBudget = 256

// BuildRequestBody constructs the OpenAI-shape body for the router-LLM
// call. SystemPrompt arrives pre-prepared (catalog already substituted
// by the caller); Messages carry the user+assistant conversation already
// filtered by the smart strategy. The router LLM prompt is "here is the
// model catalog (system) and here is the recent conversation; pick the
// best target".
//
// Each normcore.Message can carry multimodal content blocks; this
// builder concatenates ContentText blocks per message and elides
// non-text content (image_ref, tool_use, tool_result, reasoning) —
// the router does not benefit from seeing binary refs or tool plumbing
// and ignoring them keeps the prompt small.
//
// The conversation budget is the router model's real window minus the
// measured system prompt (the catalog grows with the model table) minus
// the output reserve, capped at routerInputCap. Staging uses
// StrategyRecentTurns so follow-up turns ("continue", "try again")
// arrive with the context that defines them, and inputstaging's default
// budget enforcement guarantees the call never carries an over-limit
// prompt — an oversized newest turn is tail-truncated, not sent as-is.
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

	// Project conversation messages to flat text, dropping empty
	// projections and preserving roles for turn-pairing in RecentTurns.
	stagingMsgs := make([]inputstaging.Message, 0, len(req.Messages))
	for _, m := range req.Messages {
		text := textOf(m)
		if text == "" {
			continue
		}
		stagingMsgs = append(stagingMsgs, inputstaging.Message{Role: string(m.Role), Content: text})
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

	// Conversation budget: real router window − measured system prompt
	// (catalog + rules + metadata line) − output reserve, capped so a
	// giant router window doesn't inflate cost, floored so some user
	// content always ships.
	limit := req.RouterContextLimit
	if limit <= 0 {
		limit = routerDefaultContextLimit
	}
	// Conservative estimate: the system prompt is SUBTRACTED from the router
	// window, so under-counting it inflates the conversation budget and risks
	// overflowing the router LLM. Over-counting only trims a little user
	// content — the safe direction for a fit decision.
	inputBudget := limit - inputstaging.EstimateTokensConservative(systemPrompt) - routerReserveOutput
	if inputBudget > routerInputCap {
		inputBudget = routerInputCap
	}
	if inputBudget < routerMinInputBudget {
		logger.Warn("smart: router system prompt consumes the router context window; flooring conversation budget",
			"router_context_limit", limit,
			"floor", routerMinInputBudget,
		)
		InputOverflowTotal.WithLabelValues("system_prompt_budget_floor").Inc()
		inputBudget = routerMinInputBudget
	}

	plan, planErr := inputstaging.Plan(inputstaging.PlanInput{
		Messages:          stagingMsgs,
		ModelContextLimit: inputBudget,
		Strategy:          inputstaging.StrategyRecentTurns,
	})
	if planErr != nil {
		// Strategy is a constant so this branch is unreachable in practice;
		// guard defensively and fall through with whatever messages we have.
		logger.Warn("smart: inputstaging.Plan error", "error", planErr)
	} else if plan.OverflowKind != inputstaging.OverflowNone {
		// Debug, not Warn: a bounded overflow is expected steady-state on
		// long conversations; the counter carries the operator signal.
		logger.Debug("smart: router-LLM input bounded by budget enforcement",
			"overflow_kind", string(plan.OverflowKind),
			"input_tokens", plan.InputTokens,
			"budget", inputBudget,
		)
		InputOverflowTotal.WithLabelValues(string(plan.OverflowKind)).Inc()
	}

	var planned []message
	if planErr == nil && len(plan.Messages) > 0 {
		// The conversation must start with a user turn: an unpaired
		// leading assistant message (its user partner projected to empty
		// text) would make Anthropic-shape router providers reject the
		// call. Trim to the first user turn; if none survives, degrade to
		// the system-only body below — the pre-existing no-content shape.
		msgs := plan.Messages
		for len(msgs) > 0 && msgs[0].Role != "user" {
			msgs = msgs[1:]
		}
		for _, m := range msgs {
			planned = append(planned, message{Role: m.Role, Content: m.Content})
		}
	} else if planErr != nil {
		// Defensive fallback for the unreachable Plan error: forward the
		// staged messages untouched.
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
	// Field-by-field, not a struct conversion: Decision carries the router
	// call's own usage / cost / serving-provider fields, which the wire
	// response never supplies — the decider stamps those after parsing.
	return Decision{
		ModelID:    r.ModelID,
		ProviderID: r.ProviderID,
		Reason:     r.Reason,
	}, true
}

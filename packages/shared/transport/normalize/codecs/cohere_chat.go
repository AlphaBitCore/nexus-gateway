package codecs

import (
	"context"
	"fmt"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/locator"
	"github.com/goccy/go-json"
	"strings"
)

// CohereChatNormalizer handles Cohere's /v2/chat surface — both
// non-streaming JSON responses and SSE streamed responses.
//
// Cohere v2 chat response shape (relevant subset):
//
//	{
//	  "id": "...",
//	  "model": "...",
//	  "finish_reason": "...",
//	  "message": {
//	    "role": "assistant",
//	    "content": [{"type": "text", "text": "..."}],
//	    "tool_plan": "...",
//	    "tool_calls": [...]
//	  },
//	  "usage": {
//	    "billed_units": {"input_tokens": N, "output_tokens": N},
//	    "tokens":       {"input_tokens": N, "output_tokens": N}
//	  }
//	}
//
// Usage extraction follows the canonical convention (OpenAI-aligned):
//   - PromptTokens     ← usage.tokens.input_tokens
//   - CompletionTokens ← usage.tokens.output_tokens
//   - TotalTokens      ← PromptTokens + CompletionTokens
//
// Cohere does not report cache or reasoning tokens (no prompt cache
// product as of 2026-05); CacheReadTokens, CacheCreationTokens, and
// ReasoningTokens stay nil.
//
// `message.tool_plan` is Cohere's reasoning trace; projected as a
// core.ContentReasoning block so downstream audit / hooks see the
// chain-of-thought alongside visible content.
type CohereChatNormalizer struct{}

// NewCohereChatNormalizer returns a stateless normalizer instance.
func NewCohereChatNormalizer() *CohereChatNormalizer { return &CohereChatNormalizer{} }

// ID is the metric / log label.
func (n *CohereChatNormalizer) ID() string { return "cohere-chat" }

// Normalize routes by Meta.Direction.
func (n *CohereChatNormalizer) Normalize(_ context.Context, raw []byte, meta core.Meta) (core.NormalizedPayload, error) {
	if len(raw) == 0 {
		return zeroCohere(meta), fmt.Errorf("cohere-chat: empty body: %w", core.ErrUnsupported)
	}
	var p core.NormalizedPayload
	var err error
	switch meta.Direction {
	case core.DirectionRequest:
		p, err = n.normalizeRequest(raw, meta)
	case core.DirectionResponse:
		p, err = n.normalizeResponse(raw, meta)
	default:
		return zeroCohere(meta), fmt.Errorf("cohere-chat: direction %q not supported: %w", meta.Direction, core.ErrUnsupported)
	}
	if err == nil {
		p.Confidence = core.ScoreTier1Confidence(raw, cohereChatFieldSpec(meta.Direction))
		if p.DetectedSpec == "" {
			p.DetectedSpec = "cohere-chat"
		}
	}
	return p, err
}

// cohereChatFieldSpec returns the declared top-level wire keys for the
// Cohere /v2/chat surface in direction d.
func cohereChatFieldSpec(d core.Direction) core.FieldSpec {
	if d == core.DirectionRequest {
		return core.FieldSpec{
			Required: []string{"model", "messages"},
			Optional: []string{
				"stream", "tools", "temperature", "p", "k", "max_tokens",
				"stop_sequences", "frequency_penalty", "presence_penalty",
				"seed", "response_format", "safety_mode", "citation_options",
				"tool_choice",
			},
		}
	}
	return core.FieldSpec{
		Required: []string{"id", "model", "message", "usage", "finish_reason"},
		Optional: []string{
			"meta", "logprobs", "tool_plan", "tool_calls", "citations",
		},
	}
}

type cohereRequest struct {
	Model    string          `json:"model"`
	Messages []cohereMessage `json:"messages"`
	Stream   bool            `json:"stream,omitempty"`
}

type cohereMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

func (n *CohereChatNormalizer) normalizeRequest(raw []byte, meta core.Meta) (core.NormalizedPayload, error) {
	var req cohereRequest
	if err := decodeLenient(raw, &req); err != nil {
		return zeroCohere(meta), fmt.Errorf("cohere-chat: request unmarshal: %w", err)
	}
	if len(req.Messages) == 0 {
		return zeroCohere(meta), fmt.Errorf("cohere-chat: missing messages[]: %w", core.ErrUnsupported)
	}
	out := core.NormalizedPayload{
		Kind:             core.KindAIChat,
		NormalizeVersion: core.SchemaVersion,
		Protocol:         "cohere-chat",
		Model:            firstNonEmpty(req.Model, meta.Model),
		Stream:           req.Stream,
	}
	for i, m := range req.Messages {
		// Cohere v2 content is a string OR an array of parts. Treating it
		// as string-only dumped the whole array as raw JSON in one text
		// block, which lost image parts entirely and made ordinary
		// multi-part text requests unreadable.
		var s string
		if err := json.Unmarshal(m.Content, &s); err == nil {
			out.Messages = append(out.Messages, core.Message{
				Role:    roleFromString(m.Role),
				Content: []core.ContentBlock{{Type: core.ContentText, Text: s}},
			})
			continue
		}
		base := locator.JoinPath("messages", i) + ".content"
		var parts []map[string]any
		if err := json.Unmarshal(m.Content, &parts); err != nil {
			out.Messages = append(out.Messages, core.Message{
				Role:    roleFromString(m.Role),
				Content: []core.ContentBlock{{Type: core.ContentText, Text: payloadSafeRaw(m.Content)}},
			})
			continue
		}
		blocks := make([]core.ContentBlock, 0, len(parts))
		for pi, p := range parts {
			blocks = append(blocks, cohereRequestPart(p, locator.JoinPath(base, pi)))
		}
		out.Messages = append(out.Messages, core.Message{
			Role:    roleFromString(m.Role),
			Content: blocks,
		})
	}
	return out, nil
}

// cohereRequestPart projects one Cohere v2 request content part. The shape
// mirrors OpenAI's — `text` and `image_url` — so the same custody rules
// apply: a data URI is captured, a web URL is an external reference.
func cohereRequestPart(part map[string]any, path string) core.ContentBlock {
	switch t, _ := part["type"].(string); t {
	case "image_url":
		iu, ok := part["image_url"].(map[string]any)
		if !ok {
			return mediaBlock(&core.MediaRef{Modality: core.ModalityImage, Source: core.MediaAbsent})
		}
		urlStr, _ := iu["url"].(string)
		return mediaBlock(inlineOrExternal(urlStr, locator.JoinSuffix(path, "image_url.url"), core.ModalityImage))
	case "text", "":
		s, _ := part["text"].(string)
		return core.ContentBlock{Type: core.ContentText, Text: s}
	default:
		return core.ContentBlock{Type: core.ContentText, Text: payloadSafeJSON(part)}
	}
}

type cohereResponse struct {
	ID           string         `json:"id"`
	Model        string         `json:"model"`
	FinishReason string         `json:"finish_reason"`
	Message      *cohereRespMsg `json:"message,omitempty"`
	Usage        *cohereUsage   `json:"usage,omitempty"`
}

type cohereRespMsg struct {
	Role      string              `json:"role"`
	Content   []cohereContentPart `json:"content,omitempty"`
	ToolPlan  string              `json:"tool_plan,omitempty"`
	ToolCalls json.RawMessage     `json:"tool_calls,omitempty"`
}

type cohereContentPart struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type cohereUsage struct {
	BilledUnits *struct {
		InputTokens  int `json:"input_tokens,omitempty"`
		OutputTokens int `json:"output_tokens,omitempty"`
	} `json:"billed_units,omitempty"`
	Tokens *struct {
		InputTokens  int `json:"input_tokens,omitempty"`
		OutputTokens int `json:"output_tokens,omitempty"`
	} `json:"tokens,omitempty"`
	// CachedTokens is Cohere's prompt-cache read count, a sibling of
	// billed_units and tokens rather than a member of either. It was not
	// parsed at all, so every Cohere turn reported zero cache reads and the
	// Traffic drawer showed no cache benefit on a provider that has one —
	// observed live at 992 cached of 1431 input tokens on a single call.
	CachedTokens int `json:"cached_tokens,omitempty"`
}

func (n *CohereChatNormalizer) normalizeResponse(raw []byte, meta core.Meta) (core.NormalizedPayload, error) {
	var resp cohereResponse
	if err := decodeLenient(raw, &resp); err != nil {
		return zeroCohere(meta), fmt.Errorf("cohere-chat: response unmarshal: %w", err)
	}
	out := core.NormalizedPayload{
		Kind:             core.KindAIChat,
		NormalizeVersion: core.SchemaVersion,
		Protocol:         "cohere-chat",
		Model:            firstNonEmpty(resp.Model, meta.Model),
		FinishReason:     resp.FinishReason,
	}
	// Extract Usage FIRST so usage-only bodies still surface tokens
	// (mirrors the openai_chat normalizer's behaviour).
	if resp.Usage != nil {
		out.Usage = cohereUsageToCanonical(resp.Usage)
	}
	if resp.Message == nil {
		return out, fmt.Errorf("cohere-chat: no message in response: %w", core.ErrUnsupported)
	}
	var blocks []core.ContentBlock
	if resp.Message.ToolPlan != "" {
		blocks = append(blocks, core.ContentBlock{Type: core.ContentReasoning, Text: resp.Message.ToolPlan})
	}
	var text strings.Builder
	for _, part := range resp.Message.Content {
		if part.Type == "text" {
			text.WriteString(part.Text)
		}
	}
	if t := text.String(); t != "" {
		blocks = append(blocks, core.ContentBlock{Type: core.ContentText, Text: t})
	}
	out.Messages = []core.Message{{
		Role:         roleFromString(resp.Message.Role),
		Content:      blocks,
		FinishReason: resp.FinishReason,
	}}
	return out, nil
}

// cohereUsageToCanonical projects Cohere's usage block into the canonical
// Usage struct, on the BILLED basis.
//
// This reverses an earlier decision to prefer `tokens` as the "true counts".
// Cohere's own documentation settles which basis is the charge:
//
//	"the billed input and output tokens are the tokens that you're actually
//	 billed for. The reason these values can be different from the overall
//	 `tokens` value is that there are situations in which Cohere adds tokens
//	 under the hood, and there are others in which a particular model has been
//	 trained to do so (i.e. when outputting special tokens). Since these are
//	 tokens you don't have control over, you are not charged for them."
//
// The gap is not academic. One observed call reported billed_units.input 31
// against tokens.input 1431 — a 46x spread — and chatCostFormula prices
// PromptTokens directly, so charging on `tokens` billed the caller for
// tokens Cohere charged nobody for.
//
// Reporting moves with cost on purpose. A caller shown 1431 and charged for
// 31 cannot reconcile their own invoice, and a gateway whose usage and cost
// disagree is worse than one that reports the smaller honest number.
//
// `tokens` remains the fallback: a response that omits billed_units still
// yields counts rather than nothing.
func cohereUsageToCanonical(u *cohereUsage) *core.Usage {
	out := &core.Usage{}
	var inp, outp int
	switch {
	case u.BilledUnits != nil:
		inp = u.BilledUnits.InputTokens
		outp = u.BilledUnits.OutputTokens
	case u.Tokens != nil:
		inp = u.Tokens.InputTokens
		outp = u.Tokens.OutputTokens
	default:
		return nil
	}
	if inp == 0 && outp == 0 && u.CachedTokens == 0 {
		return nil
	}
	setIntPtr(&out.PromptTokens, inp)
	setIntPtr(&out.CompletionTokens, outp)
	// CacheReadTokens is a SUBSET marker over PromptTokens, not a deduction
	// from it — the same convention every other adapter follows (OpenAI's
	// prompt_tokens_details.cached_tokens, DeepSeek's prompt_cache_hit_tokens).
	// Subtracting here would make Cohere the one provider whose prompt count
	// means something different from the rest.
	setIntPtr(&out.CacheReadTokens, u.CachedTokens)
	if inp != 0 || outp != 0 {
		tot := inp + outp
		out.TotalTokens = &tot
	}
	return out
}

func zeroCohere(meta core.Meta) core.NormalizedPayload {
	return core.NormalizedPayload{
		Kind:             core.KindAIChat,
		NormalizeVersion: core.SchemaVersion,
		Protocol:         "cohere-chat",
		Model:            meta.Model,
	}
}

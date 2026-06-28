package codecs

import (
	"context"
	"fmt"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
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
	if err := json.Unmarshal(raw, &req); err != nil {
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
	for _, m := range req.Messages {
		// Cohere's content is string-only on request side per v2 docs.
		var s string
		if err := json.Unmarshal(m.Content, &s); err != nil {
			s = string(m.Content)
		}
		out.Messages = append(out.Messages, core.Message{
			Role:    roleFromString(m.Role),
			Content: []core.ContentBlock{{Type: core.ContentText, Text: s}},
		})
	}
	return out, nil
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
}

func (n *CohereChatNormalizer) normalizeResponse(raw []byte, meta core.Meta) (core.NormalizedPayload, error) {
	var resp cohereResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
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

// cohereUsageToCanonical projects Cohere's usage.tokens block into the
// canonical Usage struct. billed_units is a secondary path documented
// by Cohere for billing-side counts; we prefer `tokens` (parser-side
// counts) when both are present.
func cohereUsageToCanonical(u *cohereUsage) *core.Usage {
	out := &core.Usage{}
	var inp, outp int
	switch {
	case u.Tokens != nil:
		inp = u.Tokens.InputTokens
		outp = u.Tokens.OutputTokens
	case u.BilledUnits != nil:
		inp = u.BilledUnits.InputTokens
		outp = u.BilledUnits.OutputTokens
	default:
		return nil
	}
	if inp == 0 && outp == 0 {
		return nil
	}
	setIntPtr(&out.PromptTokens, inp)
	setIntPtr(&out.CompletionTokens, outp)
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

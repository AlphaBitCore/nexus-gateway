// openai_projection.go — project a core.NormalizedPayload to OpenAI
// chat-completion JSON bytes.
//
// Tier-1 normalizers (anthropic_messages, gemini_generate, openai_chat,
// cohere_chat, replicate, …) all produce the same core.NormalizedPayload
// shape on the response side. The ai-gateway response codec used to
// hand-roll a JSON walk per provider (each spec_*/codec.go DecodeResponse
// did its own provider→OpenAI projection ~80-100 LOC). This helper
// concentrates that projection in one place so:
//
//   1. Cross-component callers (the codec for non-OpenAI providers, the
//      cache-HIT replay path, /v1/estimate dry-run encoders) all see the
//      same structural mapping — no drift between providers in what
//      core.ContentReasoning becomes (`reasoning_content`), how tool_calls[]
//      are shaped, finish_reason normalisation, etc.
//
//   2. Adding a new non-OpenAI provider becomes (a) write a Tier-1
//      normalizer (core.NormalizedPayload output) + (b) wire-metadata
//      extraction in the codec. No provider needs to re-implement the
//      block-walk loop.
//
// What this helper does NOT own:
//
//   - Wire-level metadata (response id, created timestamp, model name,
//     provider-specific stop_reason vocabulary). The codec is the
//     authoritative source for these — they're sniffed from the raw
//     upstream body and passed in via [ProjectionWireMetadata]. Each
//     provider knows its own field names; the projector doesn't.
//
//   - Usage. Usage comes from [providers.ExtractUsage] via shared/
//     normalize Tier-1 (see provider-adapter-architecture.md §3a Rule 8).
//     Callers pass the extracted Usage pointer in via
//     [ProjectionWireMetadata] so the projection step and the Usage
//     extraction step stay structurally separate.

package codecs

import (
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"time"
)

// ProjectionWireMetadata carries the provider-specific identifiers the
// projector needs to fill the OpenAI envelope (id, created, model,
// finish_reason). These are not represented in core.NormalizedPayload
// because core.NormalizedPayload is the audit-shape contract; the codec
// extracts them from the raw upstream response body and passes them
// in. Empty fields fall back to sensible defaults (see [ProjectToOpenAIChatCompletion]).
type ProjectionWireMetadata struct {
	// ID is the upstream response id. Empty → "openai-chat-<unix-nano>"
	// fallback so OpenAI SDK clients always see a non-empty id.
	ID string
	// Model is the upstream model identifier. Empty stays empty (the
	// codec normally has it; downstream readers tolerate empty).
	Model string
	// Created is the response-creation unix timestamp. 0 → time.Now().Unix()
	// fallback (Anthropic Messages API does not return one).
	Created int64
	// Object defaults to "chat.completion" when empty.
	Object string
	// FinishReason is the OpenAI-vocabulary finish reason (e.g. "stop",
	// "length", "tool_calls", "content_filter"). The codec maps from
	// its provider's native vocabulary BEFORE passing in.
	FinishReason string
	// Usage is the Usage block to embed in the response envelope.
	// Nil → no "usage" key on the wire (matches OpenAI's behaviour for
	// streamed responses without usage opt-in).
	Usage *core.Usage
}

// ProjectToOpenAIChatCompletion emits an OpenAI chat-completion JSON
// body from a normalized response payload + wire metadata.
//
// Mapping (Content blocks → OpenAI message fields):
//
//   - core.ContentText      → concatenated into message.content (string)
//   - core.ContentReasoning → concatenated into message.reasoning_content
//     (OpenAI o-series + Deepseek/Moonshot/Kimi convention)
//   - core.ContentToolUse   → message.tool_calls[] entry. CallID falls back
//     to a synthesised "call_<sha1[:10]>" derived from (name + args)
//     when the upstream did not assign one (Gemini's functionCall has
//     no native id field).
//   - core.ContentImageRef / core.ContentToolResult → ignored on the response
//     path; Tier-1 response normalizers don't emit them, and the OpenAI
//     chat-completion response shape has no field for them.
//
// Multi-candidate (N-sampling) support: EVERY assistant message in
// payload.Messages becomes a `choices[]` entry in order, so providers
// that return multiple candidates (Gemini's candidates[] with
// candidateCount>1) project losslessly. Single-assistant payloads
// (Anthropic Messages, OpenAI non-N requests) produce the standard
// one-choice shape.
//
// Empty assistant message (no text + no tool_calls + no reasoning) is
// valid: the resulting message.content is the empty string, which is
// what a dry-run / refused / abstained response wants. A payload with
// zero assistant messages produces exactly one empty choice (defensive
// fallback for malformed upstream responses).
func ProjectToOpenAIChatCompletion(payload core.NormalizedPayload, meta ProjectionWireMetadata) ([]byte, error) {
	created := meta.Created
	if created == 0 {
		created = time.Now().Unix()
	}
	object := meta.Object
	if object == "" {
		object = "chat.completion"
	}
	id := meta.ID
	if id == "" {
		id = fmt.Sprintf("openai-chat-%d", time.Now().UnixNano())
	}

	out := map[string]any{
		"id":      id,
		"object":  object,
		"created": created,
		"model":   meta.Model,
		"choices": projectChoices(payload, meta.FinishReason),
	}
	if meta.Usage != nil {
		out["usage"] = projectUsage(meta.Usage)
	}
	return json.Marshal(out)
}

// projectChoices builds the choices[] array. One entry per assistant
// message; zero assistant messages → one empty choice.
//
// meta.FinishReason wins over the per-message FinishReason ONLY for the
// first choice — multi-candidate responses keep each candidate's own
// finishReason (Gemini's SAFETY vs STOP per-candidate, for example,
// would lose information otherwise).
func projectChoices(payload core.NormalizedPayload, metaFinishReason string) []any {
	var assistants []*core.Message
	for i := range payload.Messages {
		if payload.Messages[i].Role == core.RoleAssistant {
			assistants = append(assistants, &payload.Messages[i])
		}
	}
	if len(assistants) == 0 {
		// Defensive: no assistant message → one empty choice with the
		// caller-supplied finish reason (or "stop" fallback).
		fr := metaFinishReason
		if fr == "" {
			fr = "stop"
		}
		return []any{
			map[string]any{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": ""},
				"finish_reason": fr,
			},
		}
	}

	choices := make([]any, 0, len(assistants))
	for i, a := range assistants {
		text, reasoning, toolCalls := projectAssistantBlocks(a.Content)
		message := map[string]any{
			"role":    "assistant",
			"content": text,
		}
		if reasoning != "" {
			message["reasoning_content"] = reasoning
		}
		if len(toolCalls) > 0 {
			message["tool_calls"] = toolCalls
			// OpenAI convention: tool-call-only assistant turns set
			// content to JSON null (not empty string) so strict SDK
			// validators don't reject the response.
			if text == "" {
				message["content"] = nil
			}
		}

		finishReason := a.FinishReason
		// meta.FinishReason wins only for the first choice — for multi-
		// candidate responses the per-candidate reason is authoritative.
		if i == 0 && metaFinishReason != "" {
			finishReason = metaFinishReason
		}
		if finishReason == "" {
			finishReason = "stop"
		}

		choices = append(choices, map[string]any{
			"index":         i,
			"message":       message,
			"finish_reason": finishReason,
		})
	}
	return choices
}

// projectAssistantBlocks walks an assistant message's content blocks
// and returns the (text, reasoning, toolCalls) triple in OpenAI shape.
func projectAssistantBlocks(blocks []core.ContentBlock) (text, reasoning string, toolCalls []any) {
	for _, b := range blocks {
		switch b.Type {
		case core.ContentText:
			text += b.Text
		case core.ContentReasoning:
			reasoning += b.Text
		case core.ContentToolUse:
			if b.ToolUse == nil {
				continue
			}
			args := "{}"
			if len(b.ToolUse.Input) > 0 {
				if raw, err := json.Marshal(b.ToolUse.Input); err == nil {
					args = string(raw)
				}
			}
			id := b.ToolUse.CallID
			if id == "" {
				// Synthesise a stable id from (name + args). Matches
				// the existing spec_gemini codec convention and keeps
				// SDK clients that index tool_calls by id consistent
				// across replays.
				h := sha1.Sum([]byte(b.ToolUse.Name + "\x00" + args))
				id = fmt.Sprintf("call_%x", h)[:15]
			}
			toolCalls = append(toolCalls, map[string]any{
				"id":   id,
				"type": "function",
				"function": map[string]any{
					"name":      b.ToolUse.Name,
					"arguments": args,
				},
			})
		}
	}
	return text, reasoning, toolCalls
}

// projectUsage converts a canonical Usage to the OpenAI chat-completion
// usage block. Mirrors the wire shape that openai_chat.go's
// extractCanonicalUsage produces on the inbound side so projecting +
// extracting a Usage round-trips losslessly.
func projectUsage(u *core.Usage) map[string]any {
	out := map[string]any{}
	if u.PromptTokens != nil {
		out["prompt_tokens"] = *u.PromptTokens
	}
	if u.CompletionTokens != nil {
		out["completion_tokens"] = *u.CompletionTokens
	}
	if u.TotalTokens != nil {
		out["total_tokens"] = *u.TotalTokens
	}
	if u.CacheReadTokens != nil {
		out["prompt_tokens_details"] = map[string]any{
			"cached_tokens": *u.CacheReadTokens,
		}
	}
	if u.ReasoningTokens != nil {
		out["completion_tokens_details"] = map[string]any{
			"reasoning_tokens": *u.ReasoningTokens,
		}
	}
	return out
}

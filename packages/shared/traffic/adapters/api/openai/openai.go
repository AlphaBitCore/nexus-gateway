// Package openai implements the openai-compat adapter for extracting content
// from OpenAI-compatible chat/completions, embeddings, and responses endpoints.
// Also covers Mistral, DeepSeek, and other OpenAI-wire-compatible APIs.
package openai

import (
	"context"
	"strings"

	"github.com/tidwall/gjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

// Adapter implements the openai-compat content extraction.
type Adapter struct{}

// ID returns the adapter identifier.
func (a *Adapter) ID() string { return "openai-compat" }

// Configure is a no-op for the openai-compat adapter (no custom config needed).
func (a *Adapter) Configure(_ map[string]any) error { return nil }

// ExtractRequest routes to the appropriate extractor based on path.
func (a *Adapter) ExtractRequest(_ context.Context, body []byte, path string) (traffic.NormalizedContent, error) {
	switch {
	case strings.Contains(path, "/chat/completions"):
		return extractChatRequest(body)
	case strings.Contains(path, "/embeddings"):
		return extractEmbeddingsRequest(body)
	case strings.Contains(path, "/responses"):
		return extractResponsesCreate(body)
	default:
		return traffic.NormalizedContent{}, traffic.ErrUnknownSchema
	}
}

// ExtractResponse extracts assistant content from a chat/completions response.
func (a *Adapter) ExtractResponse(_ context.Context, body []byte, path string) (traffic.NormalizedContent, error) {
	switch {
	case strings.Contains(path, "/chat/completions"):
		return extractChatResponse(body)
	case strings.Contains(path, "/embeddings"):
		// Embeddings responses are float arrays, not text — nothing to extract.
		return traffic.NormalizedContent{}, nil
	case strings.Contains(path, "/responses"):
		return extractResponsesResponse(body)
	default:
		return traffic.NormalizedContent{}, traffic.ErrUnknownSchema
	}
}

// ExtractStreamChunk extracts delta content from a single SSE chunk.
func (a *Adapter) ExtractStreamChunk(_ context.Context, chunk []byte, _ string) (traffic.NormalizedContent, error) {
	return extractStreamDelta(chunk)
}

// extractChatRequest pulls user messages from a chat/completions request
// and surfaces every audit-relevant field:
//   - Text content (string and array-of-parts) → Segments
//   - Assistant tool_calls and legacy function_call echoed in history →
//     ToolCallSegments
//   - Top-level `tools` / `functions` definitions → Metadata
//   - `model` → Metadata
func extractChatRequest(body []byte) (traffic.NormalizedContent, error) {
	if !gjson.ValidBytes(body) {
		return traffic.NormalizedContent{}, traffic.ErrMalformed
	}

	messages := gjson.GetBytes(body, "messages")
	if !messages.Exists() {
		return traffic.NormalizedContent{}, traffic.ErrUnknownSchema
	}

	var segments, toolCalls []string
	messages.ForEach(func(_, msg gjson.Result) bool {
		content := msg.Get("content")
		if content.Type == gjson.String {
			segments = append(segments, content.Str)
		} else if content.IsArray() {
			// Array of content parts (vision, etc.)
			content.ForEach(func(_, part gjson.Result) bool {
				if part.Get("type").Str == "text" {
					segments = append(segments, part.Get("text").Str)
				}
				return true
			})
		}
		// Assistant `tool_calls` (current spec) echoed in conversation history.
		if tc := msg.Get("tool_calls"); tc.IsArray() {
			tc.ForEach(func(_, call gjson.Result) bool {
				toolCalls = append(toolCalls, call.Raw)
				return true
			})
		}
		// Legacy single `function_call` (pre-2024 OpenAI shape, still
		// appears when older clients construct the request).
		if fc := msg.Get("function_call"); fc.Exists() && fc.IsObject() {
			toolCalls = append(toolCalls, fc.Raw)
		}
		return true
	})

	meta := map[string]string{}
	if model := gjson.GetBytes(body, "model"); model.Exists() {
		meta["model"] = model.Str
	}
	if tools := gjson.GetBytes(body, "tools"); tools.IsArray() {
		meta["tools"] = tools.Raw
	}
	if fns := gjson.GetBytes(body, "functions"); fns.IsArray() {
		meta["functions"] = fns.Raw
	}

	return traffic.NormalizedContent{
		Segments:         segments,
		ToolCallSegments: toolCalls,
		Metadata:         meta,
	}, nil
}

// extractEmbeddingsRequest pulls input text from an embeddings request.
func extractEmbeddingsRequest(body []byte) (traffic.NormalizedContent, error) {
	if !gjson.ValidBytes(body) {
		return traffic.NormalizedContent{}, traffic.ErrMalformed
	}

	input := gjson.GetBytes(body, "input")
	if !input.Exists() {
		return traffic.NormalizedContent{}, traffic.ErrUnknownSchema
	}

	var segments []string
	if input.Type == gjson.String {
		segments = append(segments, input.Str)
	} else if input.IsArray() {
		input.ForEach(func(_, item gjson.Result) bool {
			if item.Type == gjson.String {
				segments = append(segments, item.Str)
			}
			return true
		})
	}

	meta := map[string]string{}
	if model := gjson.GetBytes(body, "model"); model.Exists() {
		meta["model"] = model.Str
	}

	return traffic.NormalizedContent{
		Segments: segments,
		Metadata: meta,
	}, nil
}

// extractResponsesCreate pulls input items from a responses.create request.
// Top-level `tools` definitions land on Metadata; the input array's text
// parts land on Segments. Any input item carrying type=function_call (a
// model-generated tool invocation echoed back in a multi-turn conversation)
// is captured on ToolCallSegments.
func extractResponsesCreate(body []byte) (traffic.NormalizedContent, error) {
	if !gjson.ValidBytes(body) {
		return traffic.NormalizedContent{}, traffic.ErrMalformed
	}

	input := gjson.GetBytes(body, "input")
	if !input.Exists() {
		return traffic.NormalizedContent{}, traffic.ErrUnknownSchema
	}

	var segments, toolCalls []string
	if input.Type == gjson.String {
		segments = append(segments, input.Str)
	} else if input.IsArray() {
		input.ForEach(func(_, item gjson.Result) bool {
			// Function-call echo items in the input list.
			if item.Get("type").Str == "function_call" {
				toolCalls = append(toolCalls, item.Raw)
				return true
			}
			content := item.Get("content")
			if content.Type == gjson.String {
				segments = append(segments, content.Str)
			} else if content.IsArray() {
				content.ForEach(func(_, part gjson.Result) bool {
					if part.Get("type").Str == "input_text" || part.Get("type").Str == "text" {
						segments = append(segments, part.Get("text").Str)
					}
					return true
				})
			}
			return true
		})
	}

	meta := map[string]string{}
	if model := gjson.GetBytes(body, "model"); model.Exists() {
		meta["model"] = model.Str
	}
	if tools := gjson.GetBytes(body, "tools"); tools.IsArray() {
		meta["tools"] = tools.Raw
	}
	if instructions := gjson.GetBytes(body, "instructions"); instructions.Exists() && instructions.Type == gjson.String {
		// System-style instructions belong with the prompt for compliance scans.
		segments = append([]string{instructions.Str}, segments...)
	}

	return traffic.NormalizedContent{
		Segments:         segments,
		ToolCallSegments: toolCalls,
		Metadata:         meta,
	}, nil
}

// extractChatResponse pulls assistant message content from a response.
//
// Per choice the slot order is:
//  1. message.content (text — empty for tool-only responses)
//  2. message.refusal (structured-outputs / o1 declines, present)
//
// Tool calls (`message.tool_calls` and legacy `message.function_call`)
// land on ToolCallSegments — kept off Segments because the rewrite walk
// only edits text slots.
//
// Response-wide metadata (`finish_reason`, `system_fingerprint`,
// `service_tier`, model echo) is captured into Metadata.
func extractChatResponse(body []byte) (traffic.NormalizedContent, error) {
	if !gjson.ValidBytes(body) {
		return traffic.NormalizedContent{}, traffic.ErrMalformed
	}

	var segments, toolCalls []string
	choices := gjson.GetBytes(body, "choices")
	if !choices.Exists() {
		return traffic.NormalizedContent{}, traffic.ErrUnknownSchema
	}

	finishReasons := []string{}
	choices.ForEach(func(_, choice gjson.Result) bool {
		if c := choice.Get("message.content"); c.Exists() && c.Type == gjson.String {
			segments = append(segments, c.Str)
		}
		if r := choice.Get("message.refusal"); r.Exists() && r.Type == gjson.String && r.Str != "" {
			segments = append(segments, r.Str)
		}
		// Current-spec tool calls.
		if tc := choice.Get("message.tool_calls"); tc.IsArray() {
			tc.ForEach(func(_, call gjson.Result) bool {
				toolCalls = append(toolCalls, call.Raw)
				return true
			})
		}
		// Legacy function_call (pre-tool-calls spec).
		if fc := choice.Get("message.function_call"); fc.Exists() && fc.IsObject() {
			toolCalls = append(toolCalls, fc.Raw)
		}
		if fr := choice.Get("finish_reason"); fr.Exists() && fr.Type == gjson.String && fr.Str != "" {
			finishReasons = append(finishReasons, fr.Str)
		}
		return true
	})

	meta := map[string]string{}
	if m := gjson.GetBytes(body, "model"); m.Exists() && m.Type == gjson.String {
		meta["model"] = m.Str
	}
	if sf := gjson.GetBytes(body, "system_fingerprint"); sf.Exists() && sf.Type == gjson.String && sf.Str != "" {
		meta["system_fingerprint"] = sf.Str
	}
	if st := gjson.GetBytes(body, "service_tier"); st.Exists() && st.Type == gjson.String && st.Str != "" {
		meta["service_tier"] = st.Str
	}
	if len(finishReasons) > 0 {
		meta["finish_reason"] = strings.Join(finishReasons, ",")
	}

	return traffic.NormalizedContent{
		Segments:         segments,
		ToolCallSegments: toolCalls,
		Metadata:         meta,
	}, nil
}

// extractResponsesResponse pulls output text from a responses API response.
// Output items of type=function_call land on ToolCallSegments so audit
// captures function invocations the model emitted in this turn.
func extractResponsesResponse(body []byte) (traffic.NormalizedContent, error) {
	if !gjson.ValidBytes(body) {
		return traffic.NormalizedContent{}, traffic.ErrMalformed
	}

	var segments, toolCalls []string
	output := gjson.GetBytes(body, "output")
	if output.IsArray() {
		output.ForEach(func(_, item gjson.Result) bool {
			itemType := item.Get("type").Str
			if itemType == "function_call" {
				toolCalls = append(toolCalls, item.Raw)
				return true
			}
			content := item.Get("content")
			if content.IsArray() {
				content.ForEach(func(_, part gjson.Result) bool {
					if part.Get("type").Str == "output_text" {
						segments = append(segments, part.Get("text").Str)
					}
					return true
				})
			}
			return true
		})
	}

	meta := map[string]string{}
	if m := gjson.GetBytes(body, "model"); m.Exists() && m.Type == gjson.String {
		meta["model"] = m.Str
	}
	if status := gjson.GetBytes(body, "status"); status.Exists() && status.Type == gjson.String && status.Str != "" {
		meta["status"] = status.Str
	}
	if id := gjson.GetBytes(body, "id"); id.Exists() && id.Type == gjson.String && id.Str != "" {
		meta["id"] = id.Str
	}

	return traffic.NormalizedContent{
		Segments:         segments,
		ToolCallSegments: toolCalls,
		Metadata:         meta,
	}, nil
}

// extractStreamDelta extracts content from a streaming SSE chunk.
//
//   - choices[0].delta.content        → Segments
//   - choices[0].delta.refusal        → Segments (assistant-visible)
//   - choices[0].delta.reasoning_content → ReasoningSegments
//   - choices[0].delta.tool_calls     → ToolCallSegments (per-call delta)
//   - choices[0].delta.function_call  → ToolCallSegments (legacy alias)
//   - choices[0].finish_reason        → Metadata["finish_reason"]
//
// Each entry in ToolCallSegments is the per-chunk delta as raw JSON; the
// streaming pipeline accumulates them across chunks so the final view is
// the complete tool_call object.
func extractStreamDelta(chunk []byte) (traffic.NormalizedContent, error) {
	if !gjson.ValidBytes(chunk) {
		return traffic.NormalizedContent{}, traffic.ErrMalformed
	}

	var segments, reasoning, toolCalls []string

	delta := gjson.GetBytes(chunk, "choices.0.delta")
	if c := delta.Get("content"); c.Exists() && c.Type == gjson.String && c.Str != "" {
		segments = append(segments, c.Str)
	}
	if rf := delta.Get("refusal"); rf.Exists() && rf.Type == gjson.String && rf.Str != "" {
		segments = append(segments, rf.Str)
	}
	if rc := delta.Get("reasoning_content"); rc.Exists() && rc.Type == gjson.String && rc.Str != "" {
		reasoning = append(reasoning, rc.Str)
	}
	if tc := delta.Get("tool_calls"); tc.IsArray() {
		tc.ForEach(func(_, call gjson.Result) bool {
			toolCalls = append(toolCalls, call.Raw)
			return true
		})
	}
	if fc := delta.Get("function_call"); fc.Exists() && fc.IsObject() {
		toolCalls = append(toolCalls, fc.Raw)
	}

	meta := map[string]string{}
	if fr := gjson.GetBytes(chunk, "choices.0.finish_reason"); fr.Exists() && fr.Type == gjson.String && fr.Str != "" {
		meta["finish_reason"] = fr.Str
	}

	return traffic.NormalizedContent{
		Segments:          segments,
		ReasoningSegments: reasoning,
		ToolCallSegments:  toolCalls,
		Metadata:          meta,
	}, nil
}

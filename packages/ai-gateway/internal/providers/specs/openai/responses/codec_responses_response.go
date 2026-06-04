// Package responses — codec_responses_response.go is the non-stream
// half of the Responses-API codec. Translates Responses-API response
// bodies (the wire that flows back from POST /v1/responses) to and from
// the canonical chat-completions response shape.
//
// Two directions:
//
//	DecodeResponsesResponse:  Responses-API wire → canonical chat-completions
//	                           wire bytes (+ Usage). Used by the auto-upgrade
//	                           path: we sent /v1/responses upstream, now
//	                           reshape the response back to the
//	                           chat-completions shape the calling client
//	                           expects.
//
//	EncodeResponsesResponse:  canonical chat-completions response →
//	                           Responses-API wire. Used on the cross-format
//	                           path: a non-OpenAI provider's canonical
//	                           response is re-shaped into Responses output[]
//	                           for a /v1/responses client.
//
// Streaming half lives in stream_responses.go.
package responses

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/canonicalext"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specutil"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// DecodeResponsesResponse converts a Responses-API non-streaming response
// body into a canonical chat-completions response body. Returns the
// canonical bytes + the extracted Usage envelope (populated from the
// Responses-shape usage block via specutil.ExtractOpenAIUsage, which
// recognises input_tokens / output_tokens / input_tokens_details +
// output_tokens_details).
//
// Field mapping:
//
//	output[type=message].content[type=output_text]  → choices[0].message.content
//	output[type=message].content[type=refusal]      → choices[0].message.refusal
//	output[type=function_call]                      → choices[0].message.tool_calls[]
//	output[type=reasoning].summary[type=summary_text] → choices[0].message.reasoning_content
//	output[type=web_search_call|file_search_call|...] → nexus.ext.openai.responses.builtin_tool_calls[]
//	status                                          → choices[0].finish_reason (mapped)
//	incomplete_details.reason                       → also mapped into finish_reason
//	id / object / created_at / model                → echoed onto canonical top-level
//	usage                                           → canonical Usage (specutil)
func DecodeResponsesResponse(raw []byte) ([]byte, provcore.Usage, error) {
	if len(raw) == 0 {
		return nil, provcore.Usage{}, fmt.Errorf("spec_openai: empty Responses-API response body")
	}
	if !gjson.ValidBytes(raw) {
		return nil, provcore.Usage{}, fmt.Errorf("spec_openai: Responses-API response body is not valid JSON")
	}

	// Usage extraction via provcore.ExtractUsage (shared/normalize Tier-1
	// normalizer). The Responses-API alias chain (input_tokens, output_tokens,
	// input_tokens_details.cached_tokens, output_tokens_details.reasoning_tokens)
	// is handled there.
	usage := provcore.ExtractUsage(raw, provcore.FormatOpenAI)

	// Top-level identity-ish fields that survive verbatim.
	out := []byte(`{}`)
	if v := gjson.GetBytes(raw, "id"); v.Exists() {
		out, _ = sjson.SetBytes(out, "id", v.Value())
	} else {
		out, _ = sjson.SetBytes(out, "id", "chatcmpl-"+fmt.Sprint(time.Now().UnixNano()))
	}
	out, _ = sjson.SetBytes(out, "object", "chat.completion")
	if v := gjson.GetBytes(raw, "created_at"); v.Exists() {
		out, _ = sjson.SetBytes(out, "created", v.Int())
	} else {
		out, _ = sjson.SetBytes(out, "created", time.Now().Unix())
	}
	if v := gjson.GetBytes(raw, "model"); v.Exists() {
		out, _ = sjson.SetBytes(out, "model", v.String())
	}

	// Build choices[0].message by walking output[].
	msg := map[string]any{"role": "assistant"}
	var contentParts []string
	var refusal string
	var toolCalls []map[string]any
	var reasoningParts []string
	var builtinCalls []any

	output := gjson.GetBytes(raw, "output")
	output.ForEach(func(_, item gjson.Result) bool {
		switch item.Get("type").String() {
		case "message":
			item.Get("content").ForEach(func(_, part gjson.Result) bool {
				switch part.Get("type").String() {
				case "output_text", "text":
					contentParts = append(contentParts, part.Get("text").String())
				case "refusal":
					refusal = part.Get("refusal").String()
				}
				return true
			})
		case "function_call":
			toolCalls = append(toolCalls, map[string]any{
				"id":   firstNonEmpty(item.Get("call_id").String(), item.Get("id").String()),
				"type": "function",
				"function": map[string]any{
					"name":      item.Get("name").String(),
					"arguments": item.Get("arguments").String(),
				},
			})
		case "reasoning":
			item.Get("summary").ForEach(func(_, sp gjson.Result) bool {
				if sp.Get("type").String() == "summary_text" {
					reasoningParts = append(reasoningParts, sp.Get("text").String())
				}
				return true
			})
		case "web_search_call", "file_search_call", "image_generation_call",
			"mcp_call", "computer_call", "code_interpreter_call",
			"custom_tool_call", "apply_patch_tool_call", "function_shell_tool_call":
			// Built-in tool call echo — preserve verbatim under
			// nexus.ext for round-trip integrity.
			var copy any
			_ = json.Unmarshal([]byte(item.Raw), &copy)
			builtinCalls = append(builtinCalls, copy)
		}
		return true
	})

	if len(contentParts) > 0 {
		msg["content"] = strings.Join(contentParts, "")
	} else {
		msg["content"] = nil // explicit null per chat-completions when no text content
	}
	if refusal != "" {
		msg["refusal"] = refusal
	}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
	}
	if len(reasoningParts) > 0 {
		msg["reasoning_content"] = strings.Join(reasoningParts, "")
	}

	finishReason := mapResponsesStatusToFinishReason(
		gjson.GetBytes(raw, "status").String(),
		gjson.GetBytes(raw, "incomplete_details.reason").String(),
		len(toolCalls) > 0,
	)
	choice := map[string]any{
		"index":         0,
		"message":       msg,
		"finish_reason": finishReason,
	}
	out, _ = sjson.SetBytes(out, "choices", []any{choice})

	// Echo usage in canonical chat-completions shape.
	if usageBlock := buildCanonicalUsage(usage, gjson.GetBytes(raw, "usage")); usageBlock != nil {
		out, _ = sjson.SetBytes(out, "usage", usageBlock)
	}

	if len(builtinCalls) > 0 {
		out, _ = canonicalext.Set(out, extProvider, extResponsesKey+".builtin_tool_calls", builtinCalls)
	}

	// Preserve the original Responses status + id so EncodeResponsesResponse
	// can restore them on the inverse trip.
	if v := gjson.GetBytes(raw, "status"); v.Exists() {
		out, _ = canonicalext.Set(out, extProvider, extResponsesKey+".status", v.String())
	}
	if v := gjson.GetBytes(raw, "id"); v.Exists() {
		out, _ = canonicalext.Set(out, extProvider, extResponsesKey+".id", v.String())
	}
	if v := gjson.GetBytes(raw, "incomplete_details"); v.Exists() {
		out, _ = canonicalext.Set(out, extProvider, extResponsesKey+".incomplete_details", v.Value())
	}

	return out, usage, nil
}

// EncodeResponsesResponse converts a canonical chat-completions response
// body into a Responses-API response body. Used on the cross-format
// egress path: when /v1/responses ingress routes to a non-OpenAI target
// (Anthropic, Gemini, …), the target's canonical response is re-shaped
// into Responses output[] before being returned to the client.
//
// Inverse contract relative to DecodeResponsesResponse. requestID, when
// non-empty, is used to synthesize the response id (resp_<requestID>)
// in the absence of a preserved id from the ext namespace.
func EncodeResponsesResponse(canonical []byte, requestID, modelOverride string) ([]byte, error) {
	if len(canonical) == 0 {
		return nil, fmt.Errorf("spec_openai: empty canonical response for Responses-API encode")
	}
	if !gjson.ValidBytes(canonical) {
		return nil, fmt.Errorf("spec_openai: canonical response is not valid JSON")
	}

	out := []byte(`{}`)
	// id — prefer ext-preserved value (round-trip); else synth.
	id := canonicalext.Get(canonical, extProvider, extResponsesKey+".id").String()
	if id == "" {
		if requestID != "" {
			id = "resp_" + requestID
		} else {
			id = fmt.Sprintf("resp_%d", time.Now().UnixNano())
		}
	}
	out, _ = sjson.SetBytes(out, "id", id)
	out, _ = sjson.SetBytes(out, "object", "response")

	if v := gjson.GetBytes(canonical, "created"); v.Exists() {
		out, _ = sjson.SetBytes(out, "created_at", v.Int())
	} else {
		out, _ = sjson.SetBytes(out, "created_at", time.Now().Unix())
	}

	model := modelOverride
	if model == "" {
		model = gjson.GetBytes(canonical, "model").String()
	}
	if model != "" {
		out, _ = sjson.SetBytes(out, "model", model)
	}

	// Status: prefer ext-preserved (Decode→Encode round-trip), else
	// derive from finish_reason.
	status := canonicalext.Get(canonical, extProvider, extResponsesKey+".status").String()
	finish := gjson.GetBytes(canonical, "choices.0.finish_reason").String()
	if status == "" {
		status = mapFinishReasonToResponsesStatus(finish)
	}
	out, _ = sjson.SetBytes(out, "status", status)
	if status == "incomplete" {
		// Either restore from ext or synthesize from finish.
		if ext := canonicalext.Get(canonical, extProvider, extResponsesKey+".incomplete_details"); ext.Exists() {
			out, _ = sjson.SetBytes(out, "incomplete_details", ext.Value())
		} else if reason := mapFinishReasonToResponsesIncompleteReason(finish); reason != "" {
			out, _ = sjson.SetBytes(out, "incomplete_details", map[string]any{"reason": reason})
		}
	}

	// Build output[] from the canonical assistant message.
	msg := gjson.GetBytes(canonical, "choices.0.message")
	var outputs []any

	// Reasoning content (if present) emits a reasoning item first so the
	// item order matches OpenAI's emission order (reasoning before
	// message — verified against captured response payloads).
	if r := msg.Get("reasoning_content").String(); r != "" {
		outputs = append(outputs, map[string]any{
			"type":    "reasoning",
			"id":      "rs_" + id,
			"summary": []any{map[string]any{"type": "summary_text", "text": r}},
		})
	}

	// Text content + refusal → a single message output item with the
	// appropriate content parts.
	var contentParts []any
	if c := msg.Get("content"); c.Type == gjson.String {
		if s := c.String(); s != "" {
			contentParts = append(contentParts, map[string]any{"type": "output_text", "text": s})
		}
	}
	if rf := msg.Get("refusal").String(); rf != "" {
		contentParts = append(contentParts, map[string]any{"type": "refusal", "refusal": rf})
	}
	if len(contentParts) > 0 {
		outputs = append(outputs, map[string]any{
			"type":    "message",
			"id":      "msg_" + id,
			"role":    "assistant",
			"status":  "completed",
			"content": contentParts,
		})
	}

	// Tool calls → function_call output items.
	msg.Get("tool_calls").ForEach(func(_, tc gjson.Result) bool {
		outputs = append(outputs, map[string]any{
			"type":      "function_call",
			"id":        "fc_" + tc.Get("id").String(),
			"call_id":   tc.Get("id").String(),
			"name":      tc.Get("function.name").String(),
			"arguments": tc.Get("function.arguments").String(),
			"status":    "completed",
		})
		return true
	})

	// Built-in tool calls preserved in ext (round-trip from Decode).
	if b := canonicalext.Get(canonical, extProvider, extResponsesKey+".builtin_tool_calls"); b.IsArray() {
		b.ForEach(func(_, item gjson.Result) bool {
			var copy any
			_ = json.Unmarshal([]byte(item.Raw), &copy)
			outputs = append(outputs, copy)
			return true
		})
	}

	out, _ = sjson.SetBytes(out, "output", outputs)

	// Usage in Responses shape (input_tokens / output_tokens / ...).
	if usageBlock := buildResponsesUsage(canonical); usageBlock != nil {
		out, _ = sjson.SetBytes(out, "usage", usageBlock)
	}

	return out, nil
}

// mapResponsesStatusToFinishReason converts Responses-API status +
// incomplete_details.reason into a chat-completions finish_reason.
// Per OpenAI's documented mapping:
//
//	completed                                  → stop (or tool_calls when output had function_call items)
//	incomplete (max_output_tokens)             → length
//	incomplete (content_filter)                → content_filter
//	failed / errored                           → stop (canonical has no error-class finish_reason)
//	in_progress / queued                       → stop (defensive; non-stream should never see these)
func mapResponsesStatusToFinishReason(status, incompleteReason string, hadToolCalls bool) string {
	switch status {
	case "completed":
		if hadToolCalls {
			return "tool_calls"
		}
		return "stop"
	case "incomplete":
		switch incompleteReason {
		case "max_output_tokens":
			return "length"
		case "content_filter":
			return "content_filter"
		default:
			return "length"
		}
	case "failed", "errored":
		return "stop"
	default:
		return "stop"
	}
}

// mapFinishReasonToResponsesStatus is the inverse: canonical
// finish_reason → Responses status.
func mapFinishReasonToResponsesStatus(finish string) string {
	switch finish {
	case "length", "max_tokens", "content_filter":
		return "incomplete"
	case "", "stop", "tool_calls":
		return "completed"
	default:
		return "completed"
	}
}

// mapFinishReasonToResponsesIncompleteReason maps a finish_reason that
// implies incomplete status to the Responses incomplete_details.reason
// field. Returns "" when finish_reason does not imply incomplete.
func mapFinishReasonToResponsesIncompleteReason(finish string) string {
	switch finish {
	case "length", "max_tokens":
		return "max_output_tokens"
	case "content_filter":
		return "content_filter"
	default:
		return ""
	}
}

// buildCanonicalUsage projects a Responses-shape usage block onto a
// chat-completions-shape usage object. Returns nil when the source has
// no usage info (preserving "absent" semantics).
func buildCanonicalUsage(u provcore.Usage, raw gjson.Result) map[string]any {
	if u.PromptTokens == nil && u.CompletionTokens == nil && u.TotalTokens == nil &&
		u.CacheReadTokens == nil && u.ReasoningTokens == nil {
		return nil
	}
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
		out["prompt_tokens_details"] = map[string]any{"cached_tokens": *u.CacheReadTokens}
	}
	if u.ReasoningTokens != nil {
		out["completion_tokens_details"] = map[string]any{"reasoning_tokens": *u.ReasoningTokens}
	}
	return out
}

// buildResponsesUsage emits the Responses-API-shape usage block from a
// canonical chat-completions response. Returns nil when no usage info
// is present.
func buildResponsesUsage(canonical []byte) map[string]any {
	usageRoot := gjson.GetBytes(canonical, "usage")
	if !usageRoot.Exists() {
		return nil
	}
	canonical_u := specutil.ExtractOpenAIUsage(usageRoot)
	if canonical_u.PromptTokens == nil && canonical_u.CompletionTokens == nil &&
		canonical_u.TotalTokens == nil && canonical_u.CacheReadTokens == nil &&
		canonical_u.ReasoningTokens == nil {
		return nil
	}
	out := map[string]any{}
	if canonical_u.PromptTokens != nil {
		out["input_tokens"] = *canonical_u.PromptTokens
	}
	if canonical_u.CompletionTokens != nil {
		out["output_tokens"] = *canonical_u.CompletionTokens
	}
	if canonical_u.TotalTokens != nil {
		out["total_tokens"] = *canonical_u.TotalTokens
	}
	if canonical_u.CacheReadTokens != nil {
		out["input_tokens_details"] = map[string]any{"cached_tokens": *canonical_u.CacheReadTokens}
	}
	if canonical_u.ReasoningTokens != nil {
		out["output_tokens_details"] = map[string]any{"reasoning_tokens": *canonical_u.ReasoningTokens}
	}
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

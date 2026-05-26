// Package responses — codec_responses.go translates between OpenAI's
// /v1/responses request/response shape (FormatOpenAIResponses) and the
// canonical chat-completions shape (FormatOpenAI) that the rest of the
// gateway operates on. Per provider-adapter-architecture.md §3a Rule 1,
// canonical = OpenAI chat-completions; Responses-API is a sibling ingress
// format whose codec lives in this file.
//
// Two directions:
//
//	DecodeResponsesRequest:  Responses-API wire bytes → canonical chat-completions
//	                         (request). Invoked by canonicalbridge.IngressChatToCanonical
//	                         when the routing target does NOT natively support the
//	                         "responses-api" shape (cross-format path).
//
//	EncodeResponsesRequest:  canonical chat-completions → Responses-API wire bytes
//	                         (request). Invoked by the preferResponsesAPI
//	                         auto-upgrade path when /v1/chat/completions ingress
//	                         is rewritten to /v1/responses upstream.
//
// Response-side codec (Responses → canonical + canonical → Responses on
// egress) lives in codec_responses_response.go.
package responses

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/canonicalext"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// extProvider is the nexus.ext namespace key used by Responses-API
// extensions. Per §3a Rule 4: provider-specific Responses-only fields
// (previous_response_id, store, truncation, include, native built-in
// tools) ride along inside nexus.ext.openai.responses.<key> on the
// canonical body so they survive a round-trip and can be inspected
// centrally by cross-format guards.
const extProvider = "openai"
const extResponsesKey = "responses"

// builtinToolTypes lists Responses-API tool types whose execution is
// owned by OpenAI's Responses runtime. Caller-defined `function` tools
// are NOT in this list — they round-trip through canonical without
// special handling. Extending this list requires updating
// responses_builtin_tools.go and keeping the two lists in lockstep.
var builtinToolTypes = map[string]struct{}{
	"web_search":           {},
	"web_search_preview":   {},
	"file_search":          {},
	"computer_use_preview": {},
	"image_generation":     {},
	"mcp":                  {},
	"code_interpreter":     {},
	"custom":               {},
	"apply_patch":          {},
	"tool_search":          {},
	"function_shell":       {},
}

// DecodeResponsesRequest converts a Responses-API request body into a
// canonical chat-completions request body. The output is parseable by
// any OpenAI-shape consumer (routing rule evaluator, hook pipeline,
// audit envelope, cache key builder) without per-ingress branches.
//
// Responses-only fields (previous_response_id, store, truncation, include,
// native built-in tools) land under nexus.ext.openai.responses.* so
// EncodeResponsesRequest restores them on the inverse trip.
//
// Pre-conditions: raw is valid JSON. Empty raw returns an error rather
// than producing an empty canonical body, matching the contract of every
// other Encode/Decode helper in this package.
func DecodeResponsesRequest(raw []byte) ([]byte, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("spec_openai: empty Responses-API request body")
	}
	if !gjson.ValidBytes(raw) {
		return nil, fmt.Errorf("spec_openai: Responses-API request body is not valid JSON")
	}

	// Start with an empty JSON object; build up canonical chat-completions
	// shape by selectively projecting fields from raw.
	out := []byte(`{}`)

	// 1. model — direct copy.
	if v := gjson.GetBytes(raw, "model"); v.Exists() {
		out, _ = sjson.SetBytes(out, "model", v.Value())
	}

	// 2. input → messages. Two shapes:
	//    a) input: "string"          → [{role:"user", content:string}]
	//    b) input: [<input items>]   → mapped per-item
	messages := buildMessagesFromInput(raw)
	if len(messages) > 0 {
		// Pre-pend any system message synthesized from instructions.
		if inst := strings.TrimSpace(gjson.GetBytes(raw, "instructions").String()); inst != "" {
			// Only prepend if the first existing message is not already system.
			if len(messages) == 0 || messages[0]["role"] != "system" {
				messages = append([]map[string]any{{"role": "system", "content": inst}}, messages...)
			}
		}
		out, _ = sjson.SetBytes(out, "messages", messages)
	} else if inst := strings.TrimSpace(gjson.GetBytes(raw, "instructions").String()); inst != "" {
		// No input but instructions present — still emit the system message
		// so downstream sees a non-empty messages array.
		out, _ = sjson.SetBytes(out, "messages", []map[string]any{{"role": "system", "content": inst}})
	}

	// 3. Direct scalar passthroughs.
	for _, kv := range []struct{ from, to string }{
		{"temperature", "temperature"},
		{"top_p", "top_p"},
		{"stream", "stream"},
		{"parallel_tool_calls", "parallel_tool_calls"},
		{"metadata", "metadata"},
	} {
		if v := gjson.GetBytes(raw, kv.from); v.Exists() {
			out, _ = sjson.SetBytes(out, kv.to, v.Value())
		}
	}

	// 4. max_output_tokens → max_completion_tokens.
	if v := gjson.GetBytes(raw, "max_output_tokens"); v.Exists() {
		out, _ = sjson.SetBytes(out, "max_completion_tokens", v.Int())
	}

	// 5. reasoning.effort → top-level reasoning_effort. OpenAI chat-
	//    completions already accepts this field name on gpt-5.x / o-series;
	//    keeping the canonical name aligned removes an alias hop in the
	//    router + audit.
	if v := gjson.GetBytes(raw, "reasoning.effort"); v.Exists() {
		out, _ = sjson.SetBytes(out, "reasoning_effort", v.String())
	}

	// 6. tools[] partition: caller-defined function tools stay in
	//    canonical.tools[]; native built-ins move to
	//    nexus.ext.openai.responses.builtin_tools[].
	if toolsArr := gjson.GetBytes(raw, "tools"); toolsArr.IsArray() {
		var canonicalTools []any
		var builtins []any
		toolsArr.ForEach(func(_, item gjson.Result) bool {
			tt := item.Get("type").String()
			if _, isBuiltin := builtinToolTypes[tt]; isBuiltin {
				var copy any
				_ = json.Unmarshal([]byte(item.Raw), &copy)
				builtins = append(builtins, copy)
				return true
			}
			// Function tool: Responses wraps as {type:"function", name, parameters, description, strict}
			// flat OR as {type:"function", function:{name,...}}. Chat-completions wants the latter.
			canonicalTools = append(canonicalTools, normalizeFunctionTool(item))
			return true
		})
		if len(canonicalTools) > 0 {
			out, _ = sjson.SetBytes(out, "tools", canonicalTools)
		}
		if len(builtins) > 0 {
			out, _ = canonicalext.Set(out, extProvider, extResponsesKey+".builtin_tools", builtins)
		}
	}

	// 7. tool_choice — Responses-API and chat-completions use the same shape.
	if v := gjson.GetBytes(raw, "tool_choice"); v.Exists() {
		out, _ = sjson.SetBytes(out, "tool_choice", v.Value())
	}

	// 8. text.format → response_format. Responses wraps as
	//    {text:{format:{type:"json_schema", json_schema:{...}}}}; canonical
	//    chat-completions wants the inner object directly under
	//    response_format.
	if v := gjson.GetBytes(raw, "text.format"); v.Exists() {
		out, _ = sjson.SetBytes(out, "response_format", v.Value())
	}

	// 9. Stateful + opaque Responses-only fields → nexus.ext.openai.responses.*
	//    so EncodeResponsesRequest restores them on the inverse trip.
	for _, key := range []string{"previous_response_id", "store", "truncation", "include"} {
		if v := gjson.GetBytes(raw, key); v.Exists() {
			out, _ = canonicalext.Set(out, extProvider, extResponsesKey+"."+key, v.Value())
		}
	}

	// 10. Preserve the original "instructions" string so EncodeResponsesRequest
	//     restores it instead of leaving a phantom system message on the
	//     inverse trip.
	if inst := strings.TrimSpace(gjson.GetBytes(raw, "instructions").String()); inst != "" {
		out, _ = canonicalext.Set(out, extProvider, extResponsesKey+".instructions", inst)
	}

	return out, nil
}

// EncodeResponsesRequest converts a canonical chat-completions body
// back into a Responses-API request body. Used by the preferResponsesAPI
// auto-upgrade path: /v1/chat/completions ingress → canonical (no-op for
// OpenAI identity codec) → this encoder → upstream POST /v1/responses.
//
// Inverse contract relative to DecodeResponsesRequest: a body that has
// gone through Decode then this Encode produces semantically equivalent
// Responses-API output (modulo JSON key ordering and the presence of
// nexus.ext extensions which Encode consumes and strips from output).
func EncodeResponsesRequest(canonical []byte) ([]byte, error) {
	if len(canonical) == 0 {
		return nil, fmt.Errorf("spec_openai: empty canonical body for Responses-API encode")
	}
	if !gjson.ValidBytes(canonical) {
		return nil, fmt.Errorf("spec_openai: canonical body is not valid JSON")
	}

	out := []byte(`{}`)

	// 1. model.
	if v := gjson.GetBytes(canonical, "model"); v.Exists() {
		out, _ = sjson.SetBytes(out, "model", v.Value())
	}

	// 2. messages → input. Strip the leading system message (it becomes
	//    Responses-API's `instructions` field, restored from the ext
	//    namespace if recorded; otherwise hoist the first system content).
	messages := gjson.GetBytes(canonical, "messages").Array()
	var input []any
	var instructions string
	if origInst := canonicalext.Get(canonical, extProvider, extResponsesKey+".instructions").String(); origInst != "" {
		instructions = origInst
	}
	for i, m := range messages {
		role := m.Get("role").String()
		if i == 0 && role == "system" {
			// When instructions were preserved in the ext namespace
			// (round-trip from Decode), the leading system message is an
			// artifact of the decode-side hoist and must be stripped from
			// `input` to avoid a duplicate. When ext had no instructions
			// (e.g. the canonical body came from another ingress and
			// happened to start with a system message), hoist that system
			// content into `instructions` so the Responses target sees
			// the high-level guidance through the natural field.
			if instructions == "" {
				instructions = m.Get("content").String()
			}
			continue
		}
		input = append(input, responsesInputItemFromMessage(m))
	}
	if instructions != "" {
		out, _ = sjson.SetBytes(out, "instructions", instructions)
	}
	if len(input) > 0 {
		out, _ = sjson.SetBytes(out, "input", input)
	}

	// 3. Direct scalar passthroughs.
	for _, kv := range []struct{ from, to string }{
		{"temperature", "temperature"},
		{"top_p", "top_p"},
		{"stream", "stream"},
		{"parallel_tool_calls", "parallel_tool_calls"},
		{"metadata", "metadata"},
	} {
		if v := gjson.GetBytes(canonical, kv.from); v.Exists() {
			out, _ = sjson.SetBytes(out, kv.to, v.Value())
		}
	}

	// 4. max_completion_tokens → max_output_tokens.
	if v := gjson.GetBytes(canonical, "max_completion_tokens"); v.Exists() {
		out, _ = sjson.SetBytes(out, "max_output_tokens", v.Int())
	}

	// 5. reasoning_effort → reasoning.effort.
	if v := gjson.GetBytes(canonical, "reasoning_effort"); v.Exists() {
		out, _ = sjson.SetBytes(out, "reasoning.effort", v.String())
	}

	// 6. tools[] + built-ins from ext.
	var tools []any
	if a := gjson.GetBytes(canonical, "tools"); a.IsArray() {
		a.ForEach(func(_, item gjson.Result) bool {
			tools = append(tools, responsesToolFromCanonical(item))
			return true
		})
	}
	if b := canonicalext.Get(canonical, extProvider, extResponsesKey+".builtin_tools"); b.IsArray() {
		b.ForEach(func(_, item gjson.Result) bool {
			var copy any
			_ = json.Unmarshal([]byte(item.Raw), &copy)
			tools = append(tools, copy)
			return true
		})
	}
	if len(tools) > 0 {
		out, _ = sjson.SetBytes(out, "tools", tools)
	}

	// 7. tool_choice.
	if v := gjson.GetBytes(canonical, "tool_choice"); v.Exists() {
		out, _ = sjson.SetBytes(out, "tool_choice", v.Value())
	}

	// 8. response_format → text.format.
	if v := gjson.GetBytes(canonical, "response_format"); v.Exists() {
		out, _ = sjson.SetBytes(out, "text.format", v.Value())
	}

	// 9. Restore stateful + opaque fields from ext.
	for _, key := range []string{"previous_response_id", "store", "truncation", "include"} {
		if v := canonicalext.Get(canonical, extProvider, extResponsesKey+"."+key); v.Exists() {
			out, _ = sjson.SetBytes(out, key, v.Value())
		}
	}

	return out, nil
}

// buildMessagesFromInput converts the Responses-API `input` field into a
// canonical messages array. Handles the two valid shapes:
//   - string shorthand: input = "hello" → [{role:"user", content:"hello"}]
//   - input-items array: each item is either an input_message ({role,
//     content:[...]}) or a function_call_output ({type:"function_call_output",
//     call_id, output}). Other item types (function_call echoes, reasoning
//     echoes from prior turns when previous_response_id is unwound) are
//     preserved verbatim under content for the corresponding role so the
//     hook + router pipeline can still extract text via gjson selectors
//     identical to chat-completions.
func buildMessagesFromInput(raw []byte) []map[string]any {
	input := gjson.GetBytes(raw, "input")
	if !input.Exists() {
		return nil
	}
	// Shape (a): string shorthand.
	if input.Type == gjson.String {
		return []map[string]any{{"role": "user", "content": input.String()}}
	}
	// Shape (b): array of input items.
	if !input.IsArray() {
		return nil
	}
	var msgs []map[string]any
	input.ForEach(func(_, item gjson.Result) bool {
		// function_call_output items map to tool-role messages so the hook
		// pipeline / router see a uniform message stream.
		if item.Get("type").String() == "function_call_output" {
			msgs = append(msgs, map[string]any{
				"role":         "tool",
				"tool_call_id": item.Get("call_id").String(),
				"content":      item.Get("output").String(),
			})
			return true
		}
		// Default: input_message with role + content[].
		role := item.Get("role").String()
		if role == "" {
			role = "user"
		}
		var content any
		c := item.Get("content")
		if c.Type == gjson.String {
			content = c.String()
		} else if c.IsArray() {
			var parts []map[string]any
			c.ForEach(func(_, part gjson.Result) bool {
				parts = append(parts, normalizeInputContentPart(part))
				return true
			})
			content = parts
		}
		msgs = append(msgs, map[string]any{"role": role, "content": content})
		return true
	})
	return msgs
}

// normalizeInputContentPart maps Responses-API content-part types
// (input_text, input_image, input_audio, input_file) into the canonical
// chat-completions content-part shapes (text, image_url, …) so the
// canonical body parses identically to a multi-modal /v1/chat/completions
// request. Unknown part types pass through verbatim with a warning so
// the hook pipeline can still see their bytes.
func normalizeInputContentPart(part gjson.Result) map[string]any {
	t := part.Get("type").String()
	switch t {
	case "input_text", "text", "output_text":
		// output_text appears when an assistant turn is echoed in the input
		// array (e.g. when the client unwinds previous_response_id into a
		// multi-turn input list). Treat as plain text on the canonical side.
		return map[string]any{"type": "text", "text": part.Get("text").String()}
	case "refusal":
		// Echo of a prior assistant refusal; surface as text so hooks see
		// the bytes. No canonical refusal content type today.
		return map[string]any{"type": "text", "text": part.Get("refusal").String()}
	case "input_image":
		// Responses uses image_url either as a top-level string or as
		// {image_url:{url:"..."}}. Canonical uses the latter form. Prefer
		// the nested .url shape so an object value is unwrapped instead of
		// stringified — gjson.Result.String() on an object returns the
		// raw JSON literal, which would land as canonical image_url.url =
		// `{"url":"..."}` rather than the bare URL.
		url := part.Get("image_url.url").String()
		if url == "" {
			url = part.Get("image_url").String()
		}
		return map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}}
	case "input_audio":
		return map[string]any{"type": "input_audio", "input_audio": part.Get("input_audio").Value()}
	case "input_file":
		// No canonical chat-completions equivalent today; surface as a
		// text part with a marker so hooks see something parseable.
		canonicalext.WarnOnce(extProvider, "responses.input_file")
		return map[string]any{"type": "text", "text": fmt.Sprintf("[nexus: input_file %s preserved]", part.Get("filename").String())}
	default:
		canonicalext.WarnOnce(extProvider, "responses.unknown_content_part_"+t)
		var raw any
		_ = json.Unmarshal([]byte(part.Raw), &raw)
		if m, ok := raw.(map[string]any); ok {
			return m
		}
		return map[string]any{"type": t}
	}
}

// normalizeFunctionTool re-shapes a Responses-API function-tool entry
// into the canonical chat-completions function-tool shape. Two Responses
// variants observed:
//
//	(A) {type:"function", name, description, parameters, strict}     // flat
//	(B) {type:"function", function:{name, description, parameters, strict}}
//
// Canonical chat-completions accepts only (B). We normalize (A) → (B);
// (B) passes through unchanged.
func normalizeFunctionTool(item gjson.Result) map[string]any {
	if item.Get("function").Exists() {
		var copy map[string]any
		_ = json.Unmarshal([]byte(item.Raw), &copy)
		return copy
	}
	// Flat shape — hoist into a nested `function` object.
	fn := map[string]any{}
	for _, k := range []string{"name", "description", "parameters", "strict"} {
		if v := item.Get(k); v.Exists() {
			fn[k] = v.Value()
		}
	}
	return map[string]any{"type": "function", "function": fn}
}

// responsesInputItemFromMessage converts a canonical chat-completions
// message into a Responses-API input item. Used by EncodeResponsesRequest
// on the auto-upgrade path.
func responsesInputItemFromMessage(m gjson.Result) any {
	role := m.Get("role").String()
	if role == "tool" {
		return map[string]any{
			"type":    "function_call_output",
			"call_id": m.Get("tool_call_id").String(),
			"output":  m.Get("content").String(),
		}
	}
	content := m.Get("content")
	if content.Type == gjson.String {
		// Plain text shorthand → input_text content part.
		return map[string]any{
			"role": role,
			"content": []any{
				map[string]any{"type": "input_text", "text": content.String()},
			},
		}
	}
	if content.IsArray() {
		var parts []any
		content.ForEach(func(_, part gjson.Result) bool {
			parts = append(parts, responsesContentPartFromCanonical(part))
			return true
		})
		return map[string]any{"role": role, "content": parts}
	}
	return map[string]any{"role": role}
}

func responsesContentPartFromCanonical(part gjson.Result) map[string]any {
	t := part.Get("type").String()
	switch t {
	case "text":
		return map[string]any{"type": "input_text", "text": part.Get("text").String()}
	case "image_url":
		url := part.Get("image_url.url").String()
		if url == "" {
			url = part.Get("image_url").String()
		}
		return map[string]any{"type": "input_image", "image_url": url}
	default:
		var raw any
		_ = json.Unmarshal([]byte(part.Raw), &raw)
		if m, ok := raw.(map[string]any); ok {
			return m
		}
		return map[string]any{"type": t}
	}
}

// responsesToolFromCanonical inverts normalizeFunctionTool — emits the
// Responses-API flat shape (A) on the auto-upgrade path. Both shapes (A)
// and (B) are accepted by /v1/responses; flat is the modern recommended
// form.
func responsesToolFromCanonical(item gjson.Result) map[string]any {
	if !item.Get("function").Exists() {
		// Already flat or non-function type — pass through.
		var raw map[string]any
		_ = json.Unmarshal([]byte(item.Raw), &raw)
		return raw
	}
	out := map[string]any{"type": "function"}
	for _, k := range []string{"name", "description", "parameters", "strict"} {
		if v := item.Get("function." + k); v.Exists() {
			out[k] = v.Value()
		}
	}
	return out
}

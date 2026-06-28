// Package responses_test covers the Responses-API codec:
// DecodeResponsesRequest, EncodeResponsesRequest, DecodeResponsesResponse,
// EncodeResponsesResponse, and supporting helpers.
//
// Named failure modes:
//   - Empty/invalid JSON bodies → error, not panic
//   - String shorthand input → single user message
//   - input array with function_call_output → tool message
//   - instructions → system message prepended
//   - max_output_tokens → max_completion_tokens rename
//   - reasoning.effort → reasoning_effort rename
//   - Built-in tool types → nexus.ext namespace
//   - Function tool flat shape (A) → nested shape (B)
//   - Responses response → canonical choices[0] shape
//   - Canonical → Responses output[] shape
//   - Status mapping: completed/incomplete/failed/unknown
//   - finish_reason mapping inverse
//   - IsResponsesBuiltinTool, IsModelSupportedOnResponses
package responses_test

import (
	"github.com/goccy/go-json"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/openai/responses"
)

func TestDecodeResponsesRequest_emptyBody_returnsError(t *testing.T) {
	_, err := responses.DecodeResponsesRequest(nil)
	if err == nil {
		t.Error("expected error for nil body")
	}
	_, err = responses.DecodeResponsesRequest([]byte{})
	if err == nil {
		t.Error("expected error for empty body")
	}
}

func TestDecodeResponsesRequest_invalidJSON_returnsError(t *testing.T) {
	_, err := responses.DecodeResponsesRequest([]byte(`not-json`))
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestDecodeResponsesRequest_stringInput_singleUserMessage(t *testing.T) {
	raw := []byte(`{"model":"gpt-5","input":"hello world"}`)
	out, err := responses.DecodeResponsesRequest(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	msgs := gjson.GetBytes(out, "messages").Array()
	if len(msgs) != 1 {
		t.Fatalf("messages: got %d, want 1", len(msgs))
	}
	if msgs[0].Get("role").String() != "user" {
		t.Errorf("role: got %q, want user", msgs[0].Get("role").String())
	}
	if msgs[0].Get("content").String() != "hello world" {
		t.Errorf("content: got %q", msgs[0].Get("content").String())
	}
}

func TestDecodeResponsesRequest_instructions_prependsSystemMessage(t *testing.T) {
	raw := []byte(`{"model":"gpt-5","instructions":"You are helpful.","input":"hi"}`)
	out, err := responses.DecodeResponsesRequest(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	msgs := gjson.GetBytes(out, "messages").Array()
	if len(msgs) != 2 {
		t.Fatalf("messages: got %d, want 2", len(msgs))
	}
	if msgs[0].Get("role").String() != "system" {
		t.Errorf("first message role: got %q, want system", msgs[0].Get("role").String())
	}
	if msgs[0].Get("content").String() != "You are helpful." {
		t.Errorf("system content: got %q", msgs[0].Get("content").String())
	}
}

func TestDecodeResponsesRequest_instructionsOnlyNoInput_systemMessageEmitted(t *testing.T) {
	raw := []byte(`{"model":"gpt-5","instructions":"Be concise."}`)
	out, err := responses.DecodeResponsesRequest(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	msgs := gjson.GetBytes(out, "messages").Array()
	if len(msgs) != 1 || msgs[0].Get("role").String() != "system" {
		t.Errorf("expected single system message, got %v", msgs)
	}
}

func TestDecodeResponsesRequest_maxOutputTokens_renamedToMaxCompletionTokens(t *testing.T) {
	raw := []byte(`{"model":"gpt-5","input":"hi","max_output_tokens":512}`)
	out, err := responses.DecodeResponsesRequest(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	v := gjson.GetBytes(out, "max_completion_tokens")
	if !v.Exists() || v.Int() != 512 {
		t.Errorf("max_completion_tokens: got %v, want 512", v)
	}
	if gjson.GetBytes(out, "max_output_tokens").Exists() {
		t.Error("max_output_tokens should not appear in canonical output")
	}
}

func TestDecodeResponsesRequest_reasoningEffort_lifted(t *testing.T) {
	raw := []byte(`{"model":"gpt-5","input":"hi","reasoning":{"effort":"high"}}`)
	out, err := responses.DecodeResponsesRequest(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	v := gjson.GetBytes(out, "reasoning_effort")
	if v.String() != "high" {
		t.Errorf("reasoning_effort: got %q, want high", v.String())
	}
}

func TestDecodeResponsesRequest_scalarPassthroughs(t *testing.T) {
	raw := []byte(`{"model":"gpt-5","input":"hi","temperature":0.7,"top_p":0.9,"stream":true,"parallel_tool_calls":false}`)
	out, err := responses.DecodeResponsesRequest(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gjson.GetBytes(out, "temperature").Float() != 0.7 {
		t.Errorf("temperature not preserved")
	}
	if gjson.GetBytes(out, "top_p").Float() != 0.9 {
		t.Errorf("top_p not preserved")
	}
	if !gjson.GetBytes(out, "stream").Bool() {
		t.Errorf("stream not preserved")
	}
	if gjson.GetBytes(out, "parallel_tool_calls").Bool() != false {
		// parallel_tool_calls=false should still be present
		if !gjson.GetBytes(out, "parallel_tool_calls").Exists() {
			t.Errorf("parallel_tool_calls not preserved")
		}
	}
}

func TestDecodeResponsesRequest_textFormat_renamedToResponseFormat(t *testing.T) {
	raw := []byte(`{"model":"gpt-5","input":"hi","text":{"format":{"type":"json_object"}}}`)
	out, err := responses.DecodeResponsesRequest(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gjson.GetBytes(out, "response_format.type").String() != "json_object" {
		t.Errorf("response_format.type not remapped: %s", gjson.GetBytes(out, "response_format").Raw)
	}
}

func TestDecodeResponsesRequest_builtinTools_movedToExt(t *testing.T) {
	raw := []byte(`{"model":"gpt-5","input":"hi","tools":[{"type":"web_search"},{"type":"function","name":"my_fn","parameters":{}}]}`)
	out, err := responses.DecodeResponsesRequest(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Function tool should be in canonical.tools
	canonicalTools := gjson.GetBytes(out, "tools").Array()
	if len(canonicalTools) != 1 {
		t.Errorf("canonical tools: got %d, want 1", len(canonicalTools))
	}
	// web_search should be in nexus.ext
	builtins := gjson.GetBytes(out, "nexus.ext.openai.responses.builtin_tools").Array()
	if len(builtins) != 1 {
		t.Errorf("builtin_tools: got %d, want 1", len(builtins))
	}
	if builtins[0].Get("type").String() != "web_search" {
		t.Errorf("builtin_tools[0].type: got %q", builtins[0].Get("type").String())
	}
}

func TestDecodeResponsesRequest_functionToolFlatShape_normalizedToNested(t *testing.T) {
	raw := []byte(`{"model":"gpt-5","input":"hi","tools":[{"type":"function","name":"do_thing","description":"does it","parameters":{"type":"object"}}]}`)
	out, err := responses.DecodeResponsesRequest(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	tools := gjson.GetBytes(out, "tools").Array()
	if len(tools) != 1 {
		t.Fatalf("tools: got %d, want 1", len(tools))
	}
	// Should be wrapped in {type:"function", function:{name,...}}
	if tools[0].Get("function.name").String() != "do_thing" {
		t.Errorf("function.name: got %q, want do_thing", tools[0].Get("function.name").String())
	}
}

func TestDecodeResponsesRequest_functionToolNestedShape_passesThrough(t *testing.T) {
	raw := []byte(`{"model":"gpt-5","input":"hi","tools":[{"type":"function","function":{"name":"nested_fn","parameters":{}}}]}`)
	out, err := responses.DecodeResponsesRequest(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	tools := gjson.GetBytes(out, "tools").Array()
	if len(tools) != 1 {
		t.Fatalf("tools: got %d", len(tools))
	}
	if tools[0].Get("function.name").String() != "nested_fn" {
		t.Errorf("function.name: got %q", tools[0].Get("function.name").String())
	}
}

func TestDecodeResponsesRequest_inputArray_functionCallOutput_toolMessage(t *testing.T) {
	raw := []byte(`{"model":"gpt-5","input":[{"type":"function_call_output","call_id":"call-1","output":"result"}]}`)
	out, err := responses.DecodeResponsesRequest(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	msgs := gjson.GetBytes(out, "messages").Array()
	if len(msgs) != 1 {
		t.Fatalf("messages: got %d, want 1", len(msgs))
	}
	if msgs[0].Get("role").String() != "tool" {
		t.Errorf("role: got %q, want tool", msgs[0].Get("role").String())
	}
	if msgs[0].Get("tool_call_id").String() != "call-1" {
		t.Errorf("tool_call_id: got %q", msgs[0].Get("tool_call_id").String())
	}
}

func TestDecodeResponsesRequest_inputArray_contentParts_mapped(t *testing.T) {
	raw := []byte(`{"model":"gpt-5","input":[{"role":"user","content":[{"type":"input_text","text":"hello"},{"type":"input_image","image_url":{"url":"https://example.com/img.png"}}]}]}`)
	out, err := responses.DecodeResponsesRequest(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	msgs := gjson.GetBytes(out, "messages").Array()
	if len(msgs) != 1 {
		t.Fatalf("messages: got %d, want 1", len(msgs))
	}
	content := msgs[0].Get("content").Array()
	if len(content) != 2 {
		t.Fatalf("content parts: got %d, want 2", len(content))
	}
	if content[0].Get("type").String() != "text" {
		t.Errorf("part[0].type: got %q, want text", content[0].Get("type").String())
	}
	if content[1].Get("type").String() != "image_url" {
		t.Errorf("part[1].type: got %q, want image_url", content[1].Get("type").String())
	}
}

func TestDecodeResponsesRequest_statefulFields_preservedInExt(t *testing.T) {
	raw := []byte(`{"model":"gpt-5","input":"hi","previous_response_id":"resp_abc","store":true,"truncation":"auto"}`)
	out, err := responses.DecodeResponsesRequest(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gjson.GetBytes(out, "nexus.ext.openai.responses.previous_response_id").String() != "resp_abc" {
		t.Errorf("previous_response_id not in ext: %s", gjson.GetBytes(out, "nexus").Raw)
	}
}

func TestDecodeResponsesRequest_noInput_noInstructions_noMessages(t *testing.T) {
	raw := []byte(`{"model":"gpt-5","temperature":0.5}`)
	out, err := responses.DecodeResponsesRequest(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gjson.GetBytes(out, "messages").Exists() {
		t.Errorf("messages should not be present when input and instructions are absent")
	}
}

func TestEncodeResponsesRequest_emptyBody_returnsError(t *testing.T) {
	_, err := responses.EncodeResponsesRequest(nil)
	if err == nil {
		t.Error("expected error for nil body")
	}
	_, err = responses.EncodeResponsesRequest([]byte{})
	if err == nil {
		t.Error("expected error for empty body")
	}
}

func TestEncodeResponsesRequest_invalidJSON_returnsError(t *testing.T) {
	_, err := responses.EncodeResponsesRequest([]byte(`{bad json`))
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestEncodeResponsesRequest_roundTrip_decodeEncode(t *testing.T) {
	original := []byte(`{"model":"gpt-5","input":"hello","temperature":0.8,"max_output_tokens":256,"reasoning":{"effort":"medium"}}`)
	canonical, err := responses.DecodeResponsesRequest(original)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	encoded, err := responses.EncodeResponsesRequest(canonical)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	// Check key fields restored correctly.
	if gjson.GetBytes(encoded, "model").String() != "gpt-5" {
		t.Errorf("model not preserved: %s", encoded)
	}
	if gjson.GetBytes(encoded, "max_output_tokens").Int() != 256 {
		t.Errorf("max_output_tokens: got %d", gjson.GetBytes(encoded, "max_output_tokens").Int())
	}
	if gjson.GetBytes(encoded, "reasoning.effort").String() != "medium" {
		t.Errorf("reasoning.effort: got %q", gjson.GetBytes(encoded, "reasoning.effort").String())
	}
}

func TestEncodeResponsesRequest_systemMessageBecomesInstructions(t *testing.T) {
	canonical := []byte(`{"model":"gpt-5","messages":[{"role":"system","content":"Be helpful."},{"role":"user","content":"hi"}]}`)
	out, err := responses.EncodeResponsesRequest(canonical)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gjson.GetBytes(out, "instructions").String() != "Be helpful." {
		t.Errorf("instructions: got %q", gjson.GetBytes(out, "instructions").String())
	}
	input := gjson.GetBytes(out, "input").Array()
	if len(input) != 1 {
		t.Fatalf("input: got %d, want 1", len(input))
	}
}

func TestEncodeResponsesRequest_toolMessage_functionCallOutput(t *testing.T) {
	canonical := []byte(`{"model":"gpt-5","messages":[{"role":"tool","tool_call_id":"call-1","content":"result"}]}`)
	out, err := responses.EncodeResponsesRequest(canonical)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	input := gjson.GetBytes(out, "input").Array()
	if len(input) != 1 {
		t.Fatalf("input: got %d, want 1", len(input))
	}
	if input[0].Get("type").String() != "function_call_output" {
		t.Errorf("type: got %q, want function_call_output", input[0].Get("type").String())
	}
}

func TestEncodeResponsesRequest_responseFormat_toTextFormat(t *testing.T) {
	canonical := []byte(`{"model":"gpt-5","messages":[{"role":"user","content":"hi"}],"response_format":{"type":"json_object"}}`)
	out, err := responses.EncodeResponsesRequest(canonical)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gjson.GetBytes(out, "text.format.type").String() != "json_object" {
		t.Errorf("text.format.type: got %q", gjson.GetBytes(out, "text.format").Raw)
	}
}

func TestEncodeResponsesRequest_functionTools_flatOutput(t *testing.T) {
	canonical := []byte(`{"model":"gpt-5","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"do_it","parameters":{"type":"object"}}}]}`)
	out, err := responses.EncodeResponsesRequest(canonical)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	tools := gjson.GetBytes(out, "tools").Array()
	if len(tools) != 1 {
		t.Fatalf("tools: got %d, want 1", len(tools))
	}
	// responsesToolFromCanonical flattens nested → flat
	if tools[0].Get("name").String() != "do_it" {
		t.Errorf("tool[0].name: got %q, want do_it", tools[0].Get("name").String())
	}
}

func TestEncodeResponsesRequest_builtinToolsFromExt_appendedToTools(t *testing.T) {
	// After a decode that placed web_search in ext, encode should restore it.
	original := []byte(`{"model":"gpt-5","input":"hi","tools":[{"type":"web_search"}]}`)
	canonical, err := responses.DecodeResponsesRequest(original)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	encoded, err := responses.EncodeResponsesRequest(canonical)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	tools := gjson.GetBytes(encoded, "tools").Array()
	if len(tools) != 1 {
		t.Fatalf("tools: got %d, want 1", len(tools))
	}
	if tools[0].Get("type").String() != "web_search" {
		t.Errorf("tools[0].type: got %q, want web_search", tools[0].Get("type").String())
	}
}

func TestEncodeResponsesRequest_statefulFieldsRestoredFromExt(t *testing.T) {
	original := []byte(`{"model":"gpt-5","input":"hi","previous_response_id":"resp_xyz","store":true}`)
	canonical, err := responses.DecodeResponsesRequest(original)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	encoded, err := responses.EncodeResponsesRequest(canonical)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if gjson.GetBytes(encoded, "previous_response_id").String() != "resp_xyz" {
		t.Errorf("previous_response_id not restored: %s", encoded)
	}
}

func TestEncodeResponsesRequest_multipleMessages_inputArray_contentParts(t *testing.T) {
	canonical := []byte(`{"model":"gpt-5","messages":[{"role":"user","content":[{"type":"text","text":"hello"},{"type":"image_url","image_url":{"url":"https://example.com/img.png"}}]}]}`)
	out, err := responses.EncodeResponsesRequest(canonical)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	input := gjson.GetBytes(out, "input").Array()
	if len(input) != 1 {
		t.Fatalf("input: got %d, want 1", len(input))
	}
	content := input[0].Get("content").Array()
	if len(content) != 2 {
		t.Fatalf("content parts: got %d, want 2", len(content))
	}
	if content[0].Get("type").String() != "input_text" {
		t.Errorf("part[0].type: got %q, want input_text", content[0].Get("type").String())
	}
	if content[1].Get("type").String() != "input_image" {
		t.Errorf("part[1].type: got %q, want input_image", content[1].Get("type").String())
	}
}

func TestEncodeResponsesRequest_emptyMessages_noInput(t *testing.T) {
	canonical := []byte(`{"model":"gpt-5","temperature":0.5}`)
	out, err := responses.EncodeResponsesRequest(canonical)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gjson.GetBytes(out, "input").Exists() {
		t.Errorf("input should not be present when no messages: %s", out)
	}
}

func TestDecodeResponsesResponse_emptyBody_returnsError(t *testing.T) {
	_, _, err := responses.DecodeResponsesResponse(nil)
	if err == nil {
		t.Error("expected error for nil body")
	}
}

func TestDecodeResponsesResponse_invalidJSON_returnsError(t *testing.T) {
	_, _, err := responses.DecodeResponsesResponse([]byte(`invalid`))
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestDecodeResponsesResponse_basicTextMessage(t *testing.T) {
	raw := []byte(`{"id":"resp_1","object":"response","created_at":1700000000,"model":"gpt-5","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"hello there"}]}],"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}`)
	out, usage, err := responses.DecodeResponsesResponse(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// id echoed
	if gjson.GetBytes(out, "id").String() != "resp_1" {
		t.Errorf("id: got %q", gjson.GetBytes(out, "id").String())
	}
	// object is chat.completion
	if gjson.GetBytes(out, "object").String() != "chat.completion" {
		t.Errorf("object: got %q", gjson.GetBytes(out, "object").String())
	}
	// content merged
	content := gjson.GetBytes(out, "choices.0.message.content").String()
	if content != "hello there" {
		t.Errorf("content: got %q", content)
	}
	// finish_reason from completed
	if gjson.GetBytes(out, "choices.0.finish_reason").String() != "stop" {
		t.Errorf("finish_reason: got %q", gjson.GetBytes(out, "choices.0.finish_reason").String())
	}
	// usage
	if usage.PromptTokens == nil || *usage.PromptTokens != 10 {
		t.Errorf("PromptTokens: got %v, want 10", usage.PromptTokens)
	}
	if usage.CompletionTokens == nil || *usage.CompletionTokens != 5 {
		t.Errorf("CompletionTokens: got %v, want 5", usage.CompletionTokens)
	}
}

func TestDecodeResponsesResponse_functionCall_toolCallsInMessage(t *testing.T) {
	raw := []byte(`{"id":"resp_2","status":"completed","model":"gpt-5","output":[{"type":"function_call","call_id":"call-1","name":"do_thing","arguments":"{\"x\":1}"}]}`)
	out, _, err := responses.DecodeResponsesResponse(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	toolCalls := gjson.GetBytes(out, "choices.0.message.tool_calls").Array()
	if len(toolCalls) != 1 {
		t.Fatalf("tool_calls: got %d, want 1", len(toolCalls))
	}
	if toolCalls[0].Get("id").String() != "call-1" {
		t.Errorf("tool_calls[0].id: got %q", toolCalls[0].Get("id").String())
	}
	if toolCalls[0].Get("function.name").String() != "do_thing" {
		t.Errorf("tool_calls[0].function.name: got %q", toolCalls[0].Get("function.name").String())
	}
	// finish_reason = tool_calls when hadToolCalls=true
	if gjson.GetBytes(out, "choices.0.finish_reason").String() != "tool_calls" {
		t.Errorf("finish_reason: got %q, want tool_calls", gjson.GetBytes(out, "choices.0.finish_reason").String())
	}
}

func TestDecodeResponsesResponse_reasoning_reasoningContent(t *testing.T) {
	raw := []byte(`{"id":"resp_3","status":"completed","model":"gpt-5","output":[{"type":"reasoning","summary":[{"type":"summary_text","text":"Thinking..."}]},{"type":"message","content":[{"type":"output_text","text":"answer"}]}]}`)
	out, _, err := responses.DecodeResponsesResponse(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gjson.GetBytes(out, "choices.0.message.reasoning_content").String() != "Thinking..." {
		t.Errorf("reasoning_content: got %q", gjson.GetBytes(out, "choices.0.message.reasoning_content").String())
	}
}

func TestDecodeResponsesResponse_statusIncomplete_length(t *testing.T) {
	raw := []byte(`{"id":"resp_4","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"model":"gpt-5","output":[]}`)
	out, _, err := responses.DecodeResponsesResponse(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gjson.GetBytes(out, "choices.0.finish_reason").String() != "length" {
		t.Errorf("finish_reason: got %q, want length", gjson.GetBytes(out, "choices.0.finish_reason").String())
	}
}

func TestDecodeResponsesResponse_statusIncomplete_contentFilter(t *testing.T) {
	raw := []byte(`{"id":"resp_5","status":"incomplete","incomplete_details":{"reason":"content_filter"},"model":"gpt-5","output":[]}`)
	out, _, err := responses.DecodeResponsesResponse(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gjson.GetBytes(out, "choices.0.finish_reason").String() != "content_filter" {
		t.Errorf("finish_reason: got %q, want content_filter", gjson.GetBytes(out, "choices.0.finish_reason").String())
	}
}

func TestDecodeResponsesResponse_statusFailed_stop(t *testing.T) {
	raw := []byte(`{"id":"resp_6","status":"failed","model":"gpt-5","output":[]}`)
	out, _, err := responses.DecodeResponsesResponse(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gjson.GetBytes(out, "choices.0.finish_reason").String() != "stop" {
		t.Errorf("finish_reason: got %q, want stop", gjson.GetBytes(out, "choices.0.finish_reason").String())
	}
}

func TestDecodeResponsesResponse_unknownStatus_stop(t *testing.T) {
	raw := []byte(`{"id":"resp_7","status":"in_progress","model":"gpt-5","output":[]}`)
	out, _, err := responses.DecodeResponsesResponse(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gjson.GetBytes(out, "choices.0.finish_reason").String() != "stop" {
		t.Errorf("finish_reason for unknown: got %q, want stop", gjson.GetBytes(out, "choices.0.finish_reason").String())
	}
}

func TestDecodeResponsesResponse_builtinToolCall_preservedInExt(t *testing.T) {
	raw := []byte(`{"id":"resp_8","status":"completed","model":"gpt-5","output":[{"type":"web_search_call","id":"ws-1"},{"type":"message","content":[{"type":"output_text","text":"result"}]}]}`)
	out, _, err := responses.DecodeResponsesResponse(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	builtin := gjson.GetBytes(out, "nexus.ext.openai.responses.builtin_tool_calls").Array()
	if len(builtin) != 1 {
		t.Errorf("builtin_tool_calls: got %d, want 1", len(builtin))
	}
}

func TestDecodeResponsesResponse_refusal_inMessage(t *testing.T) {
	raw := []byte(`{"id":"resp_9","status":"completed","model":"gpt-5","output":[{"type":"message","content":[{"type":"refusal","refusal":"I cannot do that."}]}]}`)
	out, _, err := responses.DecodeResponsesResponse(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gjson.GetBytes(out, "choices.0.message.refusal").String() != "I cannot do that." {
		t.Errorf("refusal: got %q", gjson.GetBytes(out, "choices.0.message.refusal").String())
	}
}

func TestDecodeResponsesResponse_missingId_syntheticId(t *testing.T) {
	raw := []byte(`{"status":"completed","model":"gpt-5","output":[]}`)
	out, _, err := responses.DecodeResponsesResponse(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	id := gjson.GetBytes(out, "id").String()
	if !strings.HasPrefix(id, "chatcmpl-") {
		t.Errorf("synthetic id: got %q, want chatcmpl-*", id)
	}
}

func TestEncodeResponsesResponse_emptyBody_returnsError(t *testing.T) {
	_, err := responses.EncodeResponsesResponse(nil, "", "")
	if err == nil {
		t.Error("expected error for nil body")
	}
	_, err = responses.EncodeResponsesResponse([]byte{}, "", "")
	if err == nil {
		t.Error("expected error for empty body")
	}
}

func TestEncodeResponsesResponse_invalidJSON_returnsError(t *testing.T) {
	_, err := responses.EncodeResponsesResponse([]byte(`bad`), "", "")
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestEncodeResponsesResponse_basicTextCanonical_outputMessage(t *testing.T) {
	canonical := []byte(`{"id":"chat-1","object":"chat.completion","created":1700000000,"model":"gpt-5","choices":[{"index":0,"message":{"role":"assistant","content":"Hello!"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`)
	out, err := responses.EncodeResponsesResponse(canonical, "req-1", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gjson.GetBytes(out, "object").String() != "response" {
		t.Errorf("object: got %q, want response", gjson.GetBytes(out, "object").String())
	}
	output := gjson.GetBytes(out, "output").Array()
	if len(output) != 1 {
		t.Fatalf("output: got %d items, want 1", len(output))
	}
	if output[0].Get("type").String() != "message" {
		t.Errorf("output[0].type: got %q, want message", output[0].Get("type").String())
	}
	contentParts := output[0].Get("content").Array()
	if len(contentParts) != 1 || contentParts[0].Get("type").String() != "output_text" {
		t.Errorf("content parts unexpected: %v", output[0].Get("content").Raw)
	}
	// Usage in Responses shape
	if gjson.GetBytes(out, "usage.input_tokens").Int() != 5 {
		t.Errorf("usage.input_tokens: got %d", gjson.GetBytes(out, "usage.input_tokens").Int())
	}
	if gjson.GetBytes(out, "usage.output_tokens").Int() != 3 {
		t.Errorf("usage.output_tokens: got %d", gjson.GetBytes(out, "usage.output_tokens").Int())
	}
}

func TestEncodeResponsesResponse_finishReasonLength_statusIncomplete(t *testing.T) {
	canonical := []byte(`{"choices":[{"message":{"role":"assistant","content":"partial"},"finish_reason":"length"}]}`)
	out, err := responses.EncodeResponsesResponse(canonical, "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gjson.GetBytes(out, "status").String() != "incomplete" {
		t.Errorf("status: got %q, want incomplete", gjson.GetBytes(out, "status").String())
	}
	if gjson.GetBytes(out, "incomplete_details.reason").String() != "max_output_tokens" {
		t.Errorf("incomplete_details.reason: got %q", gjson.GetBytes(out, "incomplete_details.reason").String())
	}
}

func TestEncodeResponsesResponse_finishReasonContentFilter_statusIncomplete(t *testing.T) {
	canonical := []byte(`{"choices":[{"message":{"role":"assistant","content":""},"finish_reason":"content_filter"}]}`)
	out, err := responses.EncodeResponsesResponse(canonical, "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gjson.GetBytes(out, "status").String() != "incomplete" {
		t.Errorf("status: got %q, want incomplete", gjson.GetBytes(out, "status").String())
	}
	if gjson.GetBytes(out, "incomplete_details.reason").String() != "content_filter" {
		t.Errorf("incomplete_details.reason: got %q", gjson.GetBytes(out, "incomplete_details.reason").String())
	}
}

func TestEncodeResponsesResponse_toolCalls_functionCallOutput(t *testing.T) {
	canonical := []byte(`{"choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"id":"tc-1","type":"function","function":{"name":"my_fn","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
	out, err := responses.EncodeResponsesResponse(canonical, "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	output := gjson.GetBytes(out, "output").Array()
	// There's content=null (no message output) + function_call
	hasFunctionCall := false
	for _, item := range output {
		if item.Get("type").String() == "function_call" {
			hasFunctionCall = true
			if item.Get("name").String() != "my_fn" {
				t.Errorf("function_call name: got %q", item.Get("name").String())
			}
		}
	}
	if !hasFunctionCall {
		t.Errorf("expected function_call in output: %s", gjson.GetBytes(out, "output").Raw)
	}
}

func TestEncodeResponsesResponse_reasoningContent_reasoningItem(t *testing.T) {
	canonical := []byte(`{"choices":[{"message":{"role":"assistant","content":"answer","reasoning_content":"Thinking step by step."},"finish_reason":"stop"}]}`)
	out, err := responses.EncodeResponsesResponse(canonical, "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	output := gjson.GetBytes(out, "output").Array()
	// Reasoning should appear before message
	hasReasoning := false
	for _, item := range output {
		if item.Get("type").String() == "reasoning" {
			hasReasoning = true
			summary := item.Get("summary").Array()
			if len(summary) != 1 || summary[0].Get("type").String() != "summary_text" {
				t.Errorf("reasoning summary: %s", item.Get("summary").Raw)
			}
		}
	}
	if !hasReasoning {
		t.Errorf("expected reasoning item in output: %s", gjson.GetBytes(out, "output").Raw)
	}
}

func TestEncodeResponsesResponse_requestIDUsedForSyntheticId(t *testing.T) {
	canonical := []byte(`{"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`)
	out, err := responses.EncodeResponsesResponse(canonical, "req-abc", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gjson.GetBytes(out, "id").String() != "resp_req-abc" {
		t.Errorf("id: got %q, want resp_req-abc", gjson.GetBytes(out, "id").String())
	}
}

func TestEncodeResponsesResponse_modelOverride(t *testing.T) {
	canonical := []byte(`{"model":"gpt-4o","choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`)
	out, err := responses.EncodeResponsesResponse(canonical, "", "gpt-5-override")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gjson.GetBytes(out, "model").String() != "gpt-5-override" {
		t.Errorf("model: got %q, want gpt-5-override", gjson.GetBytes(out, "model").String())
	}
}

func TestEncodeResponsesResponse_builtinToolCallsFromExt_restored(t *testing.T) {
	// First decode a Responses response with a builtin tool call
	raw := []byte(`{"id":"resp_x","status":"completed","model":"gpt-5","output":[{"type":"web_search_call","id":"ws-1"},{"type":"message","content":[{"type":"output_text","text":"result"}]}]}`)
	canonical, _, err := responses.DecodeResponsesResponse(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Encode back to Responses shape
	encoded, err := responses.EncodeResponsesResponse(canonical, "", "")
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	output := gjson.GetBytes(encoded, "output").Array()
	hasWebSearch := false
	for _, item := range output {
		if item.Get("type").String() == "web_search_call" {
			hasWebSearch = true
		}
	}
	if !hasWebSearch {
		t.Errorf("web_search_call not restored in output: %s", gjson.GetBytes(encoded, "output").Raw)
	}
}

func TestEncodeResponsesResponse_refusal_refusalContentPart(t *testing.T) {
	canonical := []byte(`{"choices":[{"message":{"role":"assistant","content":null,"refusal":"I cannot help with that."},"finish_reason":"stop"}]}`)
	out, err := responses.EncodeResponsesResponse(canonical, "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	output := gjson.GetBytes(out, "output").Array()
	for _, item := range output {
		if item.Get("type").String() == "message" {
			content := item.Get("content").Array()
			for _, part := range content {
				if part.Get("type").String() == "refusal" {
					if part.Get("refusal").String() != "I cannot help with that." {
						t.Errorf("refusal content: got %q", part.Get("refusal").String())
					}
					return
				}
			}
		}
	}
	t.Error("refusal content part not found in output")
}

func TestEncodeResponsesResponse_noUsage_usageAbsent(t *testing.T) {
	canonical := []byte(`{"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`)
	out, err := responses.EncodeResponsesResponse(canonical, "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gjson.GetBytes(out, "usage").Exists() {
		t.Errorf("usage should be absent when canonical has no usage: %s", out)
	}
}

// normalizeInputContentPart coverage (via buildMessagesFromInput)

func TestDecodeResponsesRequest_inputAudio_preserved(t *testing.T) {
	raw := []byte(`{"model":"gpt-5","input":[{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"base64data","format":"wav"}}]}]}`)
	out, err := responses.DecodeResponsesRequest(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	msgs := gjson.GetBytes(out, "messages").Array()
	if len(msgs) != 1 {
		t.Fatalf("messages: %d", len(msgs))
	}
	content := msgs[0].Get("content").Array()
	if len(content) != 1 || content[0].Get("type").String() != "input_audio" {
		t.Errorf("audio part type: %v", content)
	}
}

func TestDecodeResponsesRequest_outputText_treatedAsText(t *testing.T) {
	raw := []byte(`{"model":"gpt-5","input":[{"role":"assistant","content":[{"type":"output_text","text":"echoed"}]}]}`)
	out, err := responses.DecodeResponsesRequest(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	msgs := gjson.GetBytes(out, "messages").Array()
	content := msgs[0].Get("content").Array()
	if len(content) != 1 || content[0].Get("type").String() != "text" {
		t.Errorf("output_text part: %v", content)
	}
}

func TestDecodeResponsesRequest_refusalPart_treatedAsText(t *testing.T) {
	raw := []byte(`{"model":"gpt-5","input":[{"role":"assistant","content":[{"type":"refusal","refusal":"Cannot help."}]}]}`)
	out, err := responses.DecodeResponsesRequest(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	msgs := gjson.GetBytes(out, "messages").Array()
	content := msgs[0].Get("content").Array()
	if len(content) != 1 || content[0].Get("type").String() != "text" {
		t.Errorf("refusal part type: %v", content)
	}
	if content[0].Get("text").String() != "Cannot help." {
		t.Errorf("refusal text: got %q", content[0].Get("text").String())
	}
}

func TestDecodeResponsesRequest_inputFile_preservedAsTextMarker(t *testing.T) {
	raw := []byte(`{"model":"gpt-5","input":[{"role":"user","content":[{"type":"input_file","filename":"report.pdf"}]}]}`)
	out, err := responses.DecodeResponsesRequest(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	msgs := gjson.GetBytes(out, "messages").Array()
	content := msgs[0].Get("content").Array()
	if len(content) != 1 || content[0].Get("type").String() != "text" {
		t.Errorf("input_file part: %v", content)
	}
	if !strings.Contains(content[0].Get("text").String(), "report.pdf") {
		t.Errorf("input_file marker: got %q", content[0].Get("text").String())
	}
}

func TestDecodeResponsesRequest_unknownContentPart_passesThrough(t *testing.T) {
	raw := []byte(`{"model":"gpt-5","input":[{"role":"user","content":[{"type":"future_part","data":"something"}]}]}`)
	out, err := responses.DecodeResponsesRequest(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	msgs := gjson.GetBytes(out, "messages").Array()
	content := msgs[0].Get("content").Array()
	if len(content) != 1 {
		t.Fatalf("content parts: got %d", len(content))
	}
	// Unknown type passes through verbatim (map passthrough)
	if content[0].Get("type").String() != "future_part" {
		t.Errorf("unknown part type not passed through: %s", content[0].Raw)
	}
}

func TestDecodeResponsesRequest_inputImageWithTopLevelURL(t *testing.T) {
	// image_url as flat string (not nested object)
	raw := []byte(`{"model":"gpt-5","input":[{"role":"user","content":[{"type":"input_image","image_url":"https://example.com/img.jpg"}]}]}`)
	out, err := responses.DecodeResponsesRequest(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	msgs := gjson.GetBytes(out, "messages").Array()
	content := msgs[0].Get("content").Array()
	if len(content) != 1 || content[0].Get("type").String() != "image_url" {
		t.Errorf("image_url part: %v", content)
	}
}

func TestDecodeResponsesRequest_inputArrayEmptyRole_defaultsToUser(t *testing.T) {
	raw := []byte(`{"model":"gpt-5","input":[{"content":"hi"}]}`)
	out, err := responses.DecodeResponsesRequest(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	msgs := gjson.GetBytes(out, "messages").Array()
	if msgs[0].Get("role").String() != "user" {
		t.Errorf("role: got %q, want user", msgs[0].Get("role").String())
	}
}

func TestDecodeResponsesRequest_inputStringContent_directString(t *testing.T) {
	// content is a plain string (not array) in the input item
	raw := []byte(`{"model":"gpt-5","input":[{"role":"user","content":"simple string"}]}`)
	out, err := responses.DecodeResponsesRequest(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	msgs := gjson.GetBytes(out, "messages").Array()
	if msgs[0].Get("content").String() != "simple string" {
		t.Errorf("content: got %q, want simple string", msgs[0].Get("content").String())
	}
}

func TestIsResponsesBuiltinTool_knownTypes_true(t *testing.T) {
	known := []string{
		"web_search", "web_search_preview", "file_search",
		"computer_use_preview", "image_generation", "mcp",
		"code_interpreter", "custom", "apply_patch",
		"tool_search", "function_shell",
	}
	for _, tt := range known {
		if !responses.IsResponsesBuiltinTool(tt) {
			t.Errorf("%q: expected IsResponsesBuiltinTool=true", tt)
		}
	}
}

func TestIsResponsesBuiltinTool_functionType_false(t *testing.T) {
	if responses.IsResponsesBuiltinTool("function") {
		t.Error("function: expected IsResponsesBuiltinTool=false")
	}
}

func TestIsResponsesBuiltinTool_unknownType_false(t *testing.T) {
	if responses.IsResponsesBuiltinTool("unknown_tool_xyz") {
		t.Error("unknown_tool_xyz: expected false")
	}
}

// JSON serialization guard

func TestDecodeResponsesRequest_outputIsValidJSON(t *testing.T) {
	raw := []byte(`{"model":"gpt-5","input":[{"role":"user","content":"test"}],"temperature":0.5}`)
	out, err := responses.DecodeResponsesRequest(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !json.Valid(out) {
		t.Errorf("output is not valid JSON: %s", out)
	}
}

func TestEncodeResponsesRequest_outputIsValidJSON(t *testing.T) {
	canonical := []byte(`{"model":"gpt-5","messages":[{"role":"user","content":"hi"}]}`)
	out, err := responses.EncodeResponsesRequest(canonical)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !json.Valid(out) {
		t.Errorf("output is not valid JSON: %s", out)
	}
}

func TestDecodeResponsesResponse_outputIsValidJSON(t *testing.T) {
	raw := []byte(`{"id":"r1","status":"completed","model":"gpt-5","output":[{"type":"message","content":[{"type":"output_text","text":"hi"}]}]}`)
	out, _, err := responses.DecodeResponsesResponse(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !json.Valid(out) {
		t.Errorf("output is not valid JSON: %s", out)
	}
}

// responsesContentPartFromCanonical coverage (via EncodeResponsesRequest)

func TestEncodeResponsesRequest_imageURLContent_becomesInputImage(t *testing.T) {
	// canonical content part with type=image_url goes through responsesContentPartFromCanonical
	canonical := []byte(`{"model":"gpt-5","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/pic.png"}}]}]}`)
	out, err := responses.EncodeResponsesRequest(canonical)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	input := gjson.GetBytes(out, "input").Array()
	if len(input) != 1 {
		t.Fatalf("input: %d", len(input))
	}
	content := input[0].Get("content").Array()
	if len(content) != 1 || content[0].Get("type").String() != "input_image" {
		t.Errorf("part type: %s", content[0].Get("type").String())
	}
	if content[0].Get("image_url").String() != "https://example.com/pic.png" {
		t.Errorf("image_url: got %q", content[0].Get("image_url").String())
	}
}

func TestEncodeResponsesRequest_unknownContentPartType_passesThrough(t *testing.T) {
	canonical := []byte(`{"model":"gpt-5","messages":[{"role":"user","content":[{"type":"video","src":"https://example.com/v.mp4"}]}]}`)
	out, err := responses.EncodeResponsesRequest(canonical)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	input := gjson.GetBytes(out, "input").Array()
	content := input[0].Get("content").Array()
	if len(content) != 1 || content[0].Get("type").String() != "video" {
		t.Errorf("unknown part type: %s", content[0].Raw)
	}
}

// responsesToolFromCanonical: non-function tool passthrough

func TestEncodeResponsesRequest_nonFunctionTool_passesThrough(t *testing.T) {
	// A tool without a "function" key (e.g. already flat or unexpected shape) passes through unchanged.
	canonical := []byte(`{"model":"gpt-5","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"custom","name":"my_custom"}]}`)
	out, err := responses.EncodeResponsesRequest(canonical)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	tools := gjson.GetBytes(out, "tools").Array()
	if len(tools) != 1 || tools[0].Get("type").String() != "custom" {
		t.Errorf("tools: %s", gjson.GetBytes(out, "tools").Raw)
	}
}

// mapFinishReasonToResponsesStatus coverage

func TestEncodeResponsesResponse_finishReasonMaxTokens_statusIncomplete(t *testing.T) {
	canonical := []byte(`{"choices":[{"message":{"role":"assistant","content":"partial"},"finish_reason":"max_tokens"}]}`)
	out, err := responses.EncodeResponsesResponse(canonical, "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gjson.GetBytes(out, "status").String() != "incomplete" {
		t.Errorf("status: got %q, want incomplete", gjson.GetBytes(out, "status").String())
	}
	if gjson.GetBytes(out, "incomplete_details.reason").String() != "max_output_tokens" {
		t.Errorf("incomplete_details.reason: got %q", gjson.GetBytes(out, "incomplete_details.reason").String())
	}
}

func TestEncodeResponsesResponse_finishReasonToolCalls_statusCompleted(t *testing.T) {
	canonical := []byte(`{"choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"id":"tc-1","type":"function","function":{"name":"fn","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
	out, err := responses.EncodeResponsesResponse(canonical, "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gjson.GetBytes(out, "status").String() != "completed" {
		t.Errorf("status: got %q, want completed", gjson.GetBytes(out, "status").String())
	}
}

func TestEncodeResponsesResponse_finishReasonUnknown_statusCompleted(t *testing.T) {
	canonical := []byte(`{"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"some_future_reason"}]}`)
	out, err := responses.EncodeResponsesResponse(canonical, "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gjson.GetBytes(out, "status").String() != "completed" {
		t.Errorf("status: got %q, want completed for unknown finish_reason", gjson.GetBytes(out, "status").String())
	}
}

func TestDecodeResponsesResponse_incompleteSomeOtherReason_length(t *testing.T) {
	// incomplete_details.reason is not max_output_tokens or content_filter → default "length"
	raw := []byte(`{"id":"resp_x","status":"incomplete","incomplete_details":{"reason":"server_error"},"model":"gpt-5","output":[]}`)
	out, _, err := responses.DecodeResponsesResponse(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gjson.GetBytes(out, "choices.0.finish_reason").String() != "length" {
		t.Errorf("finish_reason: got %q, want length", gjson.GetBytes(out, "choices.0.finish_reason").String())
	}
}

// buildCanonicalUsage: cache and reasoning tokens

func TestDecodeResponsesResponse_usageWithCacheAndReasoning(t *testing.T) {
	// Responses-API may include input_tokens_details.cached_tokens and output_tokens_details.reasoning_tokens
	raw := []byte(`{"id":"r1","status":"completed","model":"gpt-5","output":[],"usage":{"input_tokens":100,"output_tokens":50,"total_tokens":150,"input_tokens_details":{"cached_tokens":20},"output_tokens_details":{"reasoning_tokens":10}}}`)
	out, usage, err := responses.DecodeResponsesResponse(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if usage.CacheReadTokens == nil || *usage.CacheReadTokens != 20 {
		t.Errorf("CacheReadTokens: got %v, want 20", usage.CacheReadTokens)
	}
	if usage.ReasoningTokens == nil || *usage.ReasoningTokens != 10 {
		t.Errorf("ReasoningTokens: got %v, want 10", usage.ReasoningTokens)
	}
	// canonical usage block includes these fields
	if gjson.GetBytes(out, "usage.prompt_tokens_details.cached_tokens").Int() != 20 {
		t.Errorf("canonical prompt_tokens_details.cached_tokens: got %d", gjson.GetBytes(out, "usage.prompt_tokens_details.cached_tokens").Int())
	}
}

// buildResponsesUsage with cache+reasoning tokens

func TestEncodeResponsesResponse_usageWithCacheAndReasoning(t *testing.T) {
	canonical := []byte(`{"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":50,"total_tokens":150,"prompt_tokens_details":{"cached_tokens":20},"completion_tokens_details":{"reasoning_tokens":15}}}`)
	out, err := responses.EncodeResponsesResponse(canonical, "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gjson.GetBytes(out, "usage.input_tokens_details.cached_tokens").Int() != 20 {
		t.Errorf("cached_tokens: got %d", gjson.GetBytes(out, "usage.input_tokens_details.cached_tokens").Int())
	}
	if gjson.GetBytes(out, "usage.output_tokens_details.reasoning_tokens").Int() != 15 {
		t.Errorf("reasoning_tokens: got %d", gjson.GetBytes(out, "usage.output_tokens_details.reasoning_tokens").Int())
	}
}

// firstNonEmpty coverage: use call_id fallback to id

func TestDecodeResponsesResponse_functionCallWithIdFallback(t *testing.T) {
	// No call_id field — falls back to id field via firstNonEmpty
	raw := []byte(`{"id":"resp_10","status":"completed","model":"gpt-5","output":[{"type":"function_call","id":"fc-primary","name":"my_fn","arguments":"{}"}]}`)
	out, _, err := responses.DecodeResponsesResponse(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	toolCalls := gjson.GetBytes(out, "choices.0.message.tool_calls").Array()
	if len(toolCalls) != 1 {
		t.Fatalf("tool_calls: got %d", len(toolCalls))
	}
	// call_id is empty, id="fc-primary" → firstNonEmpty picks id
	if toolCalls[0].Get("id").String() != "fc-primary" {
		t.Errorf("id: got %q, want fc-primary", toolCalls[0].Get("id").String())
	}
}

// responsesInputItemFromMessage: empty content (no string, no array)

func TestEncodeResponsesRequest_emptyContentMessage_roleOnly(t *testing.T) {
	// message with content that is neither string nor array (null/absent)
	canonical := []byte(`{"model":"gpt-5","messages":[{"role":"assistant"}]}`)
	out, err := responses.EncodeResponsesRequest(canonical)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	input := gjson.GetBytes(out, "input").Array()
	if len(input) != 1 {
		t.Fatalf("input: got %d", len(input))
	}
	if input[0].Get("role").String() != "assistant" {
		t.Errorf("role: got %q", input[0].Get("role").String())
	}
}

package openai_test

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/openai"
	"github.com/tidwall/gjson"
)

// TestDecodeResponsesResponse_TextCompleted pins the simplest happy path:
// {output:[{type:message, content:[{type:output_text,text:"hi"}]}], status:"completed"}.
// Canonical must surface choices[0].message.content = "hi" and
// finish_reason = "stop".
func TestDecodeResponsesResponse_TextCompleted(t *testing.T) {
	in := []byte(`{
		"id": "resp_abc",
		"object": "response",
		"created_at": 1747353600,
		"status": "completed",
		"model": "gpt-5.2",
		"output": [{
			"type": "message",
			"id": "msg_1",
			"role": "assistant",
			"content": [{"type":"output_text","text":"hello world"}]
		}],
		"usage": {"input_tokens": 5, "output_tokens": 2, "total_tokens": 7}
	}`)
	out, usage, err := openai.DecodeResponsesResponse(in)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := gjson.GetBytes(out, "object").String(); got != "chat.completion" {
		t.Errorf("object = %q, want chat.completion", got)
	}
	if got := gjson.GetBytes(out, "model").String(); got != "gpt-5.2" {
		t.Errorf("model = %q", got)
	}
	if got := gjson.GetBytes(out, "choices.0.message.role").String(); got != "assistant" {
		t.Errorf("role = %q", got)
	}
	if got := gjson.GetBytes(out, "choices.0.message.content").String(); got != "hello world" {
		t.Errorf("content = %q; body=%s", got, string(out))
	}
	if got := gjson.GetBytes(out, "choices.0.finish_reason").String(); got != "stop" {
		t.Errorf("finish_reason = %q", got)
	}
	// Usage extraction must populate via the input_tokens / output_tokens aliases.
	if usage.PromptTokens == nil || *usage.PromptTokens != 5 {
		t.Errorf("PromptTokens: %v", usage.PromptTokens)
	}
	if usage.CompletionTokens == nil || *usage.CompletionTokens != 2 {
		t.Errorf("CompletionTokens: %v", usage.CompletionTokens)
	}
}

// TestDecodeResponsesResponse_FunctionCall pins
// output:[{type:function_call, ...}] → choices[0].message.tool_calls[]
// and finish_reason = "tool_calls".
func TestDecodeResponsesResponse_FunctionCall(t *testing.T) {
	in := []byte(`{
		"id": "resp_2",
		"status": "completed",
		"model": "gpt-5.2",
		"output": [{
			"type": "function_call",
			"id": "fc_1",
			"call_id": "call_abc",
			"name": "get_weather",
			"arguments": "{\"city\":\"Tokyo\"}"
		}],
		"usage": {"input_tokens": 8, "output_tokens": 3, "total_tokens": 11}
	}`)
	out, _, err := openai.DecodeResponsesResponse(in)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := gjson.GetBytes(out, "choices.0.message.tool_calls.0.id").String(); got != "call_abc" {
		t.Errorf("tool_calls[0].id = %q (want call_abc); body=%s", got, string(out))
	}
	if got := gjson.GetBytes(out, "choices.0.message.tool_calls.0.type").String(); got != "function" {
		t.Errorf("tool_calls[0].type = %q", got)
	}
	if got := gjson.GetBytes(out, "choices.0.message.tool_calls.0.function.name").String(); got != "get_weather" {
		t.Errorf("tool_calls[0].function.name = %q", got)
	}
	if got := gjson.GetBytes(out, "choices.0.finish_reason").String(); got != "tool_calls" {
		t.Errorf("finish_reason = %q (want tool_calls — output contained function_call)", got)
	}
}

// TestDecodeResponsesResponse_Reasoning pins
// output:[{type:reasoning, summary:[{type:summary_text, text:"…"}]}, {type:message, ...}]
// → choices[0].message.reasoning_content (accumulated) + .content.
func TestDecodeResponsesResponse_Reasoning(t *testing.T) {
	in := []byte(`{
		"id": "resp_3",
		"status": "completed",
		"model": "gpt-5.2",
		"output": [
			{"type":"reasoning","summary":[{"type":"summary_text","text":"thinking step 1. "},{"type":"summary_text","text":"thinking step 2."}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"final answer"}]}
		],
		"usage": {"input_tokens": 10, "output_tokens": 20, "output_tokens_details": {"reasoning_tokens": 15}}
	}`)
	out, usage, err := openai.DecodeResponsesResponse(in)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := gjson.GetBytes(out, "choices.0.message.reasoning_content").String(); got != "thinking step 1. thinking step 2." {
		t.Errorf("reasoning_content = %q; body=%s", got, string(out))
	}
	if got := gjson.GetBytes(out, "choices.0.message.content").String(); got != "final answer" {
		t.Errorf("content = %q", got)
	}
	if usage.ReasoningTokens == nil || *usage.ReasoningTokens != 15 {
		t.Errorf("ReasoningTokens: %v (want 15 via output_tokens_details alias)", usage.ReasoningTokens)
	}
}

// TestDecodeResponsesResponse_IncompleteLength pins status:"incomplete"
// + incomplete_details.reason:"max_output_tokens" → finish_reason:"length".
func TestDecodeResponsesResponse_IncompleteLength(t *testing.T) {
	in := []byte(`{
		"id": "resp_4",
		"status": "incomplete",
		"incomplete_details": {"reason": "max_output_tokens"},
		"model": "gpt-5.2",
		"output": [{"type":"message","role":"assistant","content":[{"type":"output_text","text":"partial"}]}],
		"usage": {"input_tokens": 100, "output_tokens": 100, "total_tokens": 200}
	}`)
	out, _, err := openai.DecodeResponsesResponse(in)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := gjson.GetBytes(out, "choices.0.finish_reason").String(); got != "length" {
		t.Errorf("finish_reason = %q, want length", got)
	}
	// incomplete_details preserved in ext for round-trip.
	if got := gjson.GetBytes(out, "nexus.ext.openai.responses.incomplete_details.reason").String(); got != "max_output_tokens" {
		t.Errorf("incomplete_details not preserved: %s", string(out))
	}
}

// TestDecodeResponsesResponse_BuiltinToolCallPreserved pins that
// built-in tool calls (web_search_call etc.) ride through canonical
// under nexus.ext.openai.responses.builtin_tool_calls[] so the inverse
// EncodeResponsesResponse restores them.
func TestDecodeResponsesResponse_BuiltinToolCallPreserved(t *testing.T) {
	in := []byte(`{
		"id": "resp_5",
		"status": "completed",
		"model": "gpt-5.2",
		"output": [
			{"type":"web_search_call","id":"ws_1","query":"latest news"},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"here is the news"}]}
		]
	}`)
	out, _, err := openai.DecodeResponsesResponse(in)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	calls := gjson.GetBytes(out, "nexus.ext.openai.responses.builtin_tool_calls")
	if !calls.IsArray() || len(calls.Array()) != 1 {
		t.Fatalf("expected 1 builtin call preserved; body=%s", string(out))
	}
	if got := calls.Array()[0].Get("type").String(); got != "web_search_call" {
		t.Errorf("builtin_tool_calls[0].type = %q", got)
	}
}

// TestEncodeResponsesResponse_RoundTrip exercises the bidirectional
// contract for the auto-upgrade path: a Responses non-stream response
// that is Decoded then Encoded again produces a semantically equivalent
// Responses body (same id, status, output items, usage shape).
func TestEncodeResponsesResponse_RoundTrip(t *testing.T) {
	in := []byte(`{
		"id": "resp_rt_1",
		"object": "response",
		"created_at": 1747353600,
		"status": "completed",
		"model": "gpt-5.2",
		"output": [
			{"type":"reasoning","summary":[{"type":"summary_text","text":"think..."}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}
		],
		"usage": {
			"input_tokens": 5, "output_tokens": 2, "total_tokens": 7,
			"input_tokens_details": {"cached_tokens": 1},
			"output_tokens_details": {"reasoning_tokens": 1}
		}
	}`)
	canon, _, err := openai.DecodeResponsesResponse(in)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	out, err := openai.EncodeResponsesResponse(canon, "", "")
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if got := gjson.GetBytes(out, "id").String(); got != "resp_rt_1" {
		t.Errorf("id lost in round-trip: %q; body=%s", got, string(out))
	}
	if got := gjson.GetBytes(out, "object").String(); got != "response" {
		t.Errorf("object = %q, want response", got)
	}
	if got := gjson.GetBytes(out, "status").String(); got != "completed" {
		t.Errorf("status = %q", got)
	}
	if got := gjson.GetBytes(out, "model").String(); got != "gpt-5.2" {
		t.Errorf("model = %q", got)
	}
	outputs := gjson.GetBytes(out, "output").Array()
	if len(outputs) != 2 {
		t.Fatalf("expected 2 output items (reasoning + message), got %d; body=%s", len(outputs), string(out))
	}
	if got := outputs[0].Get("type").String(); got != "reasoning" {
		t.Errorf("output[0].type = %q (want reasoning, order must match upstream)", got)
	}
	if got := outputs[0].Get("summary.0.text").String(); got != "think..." {
		t.Errorf("output[0].summary[0].text = %q", got)
	}
	if got := outputs[1].Get("type").String(); got != "message" {
		t.Errorf("output[1].type = %q (want message)", got)
	}
	if got := outputs[1].Get("content.0.text").String(); got != "hi" {
		t.Errorf("output[1].content[0].text = %q", got)
	}
	// Usage shape on Responses egress uses input_tokens / output_tokens.
	if got := gjson.GetBytes(out, "usage.input_tokens").Int(); got != 5 {
		t.Errorf("usage.input_tokens = %d, want 5", got)
	}
	if got := gjson.GetBytes(out, "usage.output_tokens").Int(); got != 2 {
		t.Errorf("usage.output_tokens = %d, want 2", got)
	}
	if got := gjson.GetBytes(out, "usage.input_tokens_details.cached_tokens").Int(); got != 1 {
		t.Errorf("usage.input_tokens_details.cached_tokens = %d, want 1", got)
	}
	if got := gjson.GetBytes(out, "usage.output_tokens_details.reasoning_tokens").Int(); got != 1 {
		t.Errorf("usage.output_tokens_details.reasoning_tokens = %d, want 1", got)
	}
}

// TestEncodeResponsesResponse_FromChatCompletions exercises the
// cross-format egress path: a canonical chat-completions response that
// did NOT originate from a Decode (i.e. no ext data) — common case when
// the upstream is Anthropic / Gemini. Encoder must synthesize a sensible
// Responses body with status, output items, and usage shape.
func TestEncodeResponsesResponse_FromChatCompletions(t *testing.T) {
	canonical := []byte(`{
		"id": "chatcmpl_xyz",
		"object": "chat.completion",
		"created": 1747353700,
		"model": "claude-sonnet-4-6",
		"choices": [{
			"index": 0,
			"message": {"role":"assistant","content":"Hello from Claude"},
			"finish_reason": "stop"
		}],
		"usage": {"prompt_tokens": 7, "completion_tokens": 4, "total_tokens": 11}
	}`)
	out, err := openai.EncodeResponsesResponse(canonical, "req_synth", "")
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if got := gjson.GetBytes(out, "id").String(); got != "resp_req_synth" {
		t.Errorf("id should be synthesized from requestID; got %q", got)
	}
	if got := gjson.GetBytes(out, "object").String(); got != "response" {
		t.Errorf("object = %q", got)
	}
	if got := gjson.GetBytes(out, "status").String(); got != "completed" {
		t.Errorf("status = %q, want completed", got)
	}
	if got := gjson.GetBytes(out, "model").String(); got != "claude-sonnet-4-6" {
		t.Errorf("model = %q, want claude-sonnet-4-6", got)
	}
	// Single message output item.
	outputs := gjson.GetBytes(out, "output").Array()
	if len(outputs) != 1 {
		t.Fatalf("expected 1 output item, got %d; body=%s", len(outputs), string(out))
	}
	if got := outputs[0].Get("type").String(); got != "message" {
		t.Errorf("output[0].type = %q", got)
	}
	if got := outputs[0].Get("content.0.text").String(); got != "Hello from Claude" {
		t.Errorf("output[0].content[0].text = %q", got)
	}
	// Usage in Responses shape.
	if got := gjson.GetBytes(out, "usage.input_tokens").Int(); got != 7 {
		t.Errorf("usage.input_tokens = %d (translated from prompt_tokens)", got)
	}
	if got := gjson.GetBytes(out, "usage.output_tokens").Int(); got != 4 {
		t.Errorf("usage.output_tokens = %d (translated from completion_tokens)", got)
	}
}

// TestEncodeResponsesResponse_LengthIncomplete pins the inverse of
// the IncompleteLength decode case.
func TestEncodeResponsesResponse_LengthIncomplete(t *testing.T) {
	canonical := []byte(`{
		"id": "chatcmpl_1",
		"choices": [{
			"index": 0,
			"message": {"role":"assistant","content":"partial"},
			"finish_reason": "length"
		}],
		"usage": {"prompt_tokens": 1, "completion_tokens": 100, "total_tokens": 101}
	}`)
	out, err := openai.EncodeResponsesResponse(canonical, "req1", "gpt-5.2")
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if got := gjson.GetBytes(out, "status").String(); got != "incomplete" {
		t.Errorf("status = %q, want incomplete", got)
	}
	if got := gjson.GetBytes(out, "incomplete_details.reason").String(); got != "max_output_tokens" {
		t.Errorf("incomplete_details.reason = %q", got)
	}
}

// TestEncodeResponsesResponse_ModelOverride pins that the modelOverride
// argument wins when canonical lacks model (or you want to expose the
// routing-resolved customer-facing alias rather than the provider-side
// model).
func TestEncodeResponsesResponse_ModelOverride(t *testing.T) {
	canonical := []byte(`{
		"choices": [{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]
	}`)
	out, err := openai.EncodeResponsesResponse(canonical, "req1", "my-alias")
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if got := gjson.GetBytes(out, "model").String(); got != "my-alias" {
		t.Errorf("model = %q (modelOverride should win)", got)
	}
}

// TestDecodeResponsesResponse_ErrorEmpty + ErrorInvalidJSON.
func TestDecodeResponsesResponse_Errors(t *testing.T) {
	if _, _, err := openai.DecodeResponsesResponse(nil); err == nil {
		t.Error("expected error on nil body")
	}
	if _, _, err := openai.DecodeResponsesResponse([]byte("not json")); err == nil {
		t.Error("expected error on invalid JSON")
	}
}

package openai_test

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/openai"
	"github.com/tidwall/gjson"
)

// TestDecodeResponsesRequest_TextShorthand pins shape (a):
// input as a bare string → canonical messages = [{role:"user", content:"…"}].
func TestDecodeResponsesRequest_TextShorthand(t *testing.T) {
	in := []byte(`{"model":"gpt-5.2","input":"Write a haiku."}`)
	out, err := openai.DecodeResponsesRequest(in)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if gjson.GetBytes(out, "model").String() != "gpt-5.2" {
		t.Errorf("model: %s", string(out))
	}
	if got := gjson.GetBytes(out, "messages.0.role").String(); got != "user" {
		t.Errorf("messages.0.role = %q, want user; body=%s", got, string(out))
	}
	if got := gjson.GetBytes(out, "messages.0.content").String(); got != "Write a haiku." {
		t.Errorf("messages.0.content = %q", got)
	}
}

// TestDecodeResponsesRequest_Instructions pins that `instructions` is
// hoisted into a leading system message.
func TestDecodeResponsesRequest_Instructions(t *testing.T) {
	in := []byte(`{"model":"gpt-5.2","instructions":"Be terse.","input":"What is 2+2?"}`)
	out, err := openai.DecodeResponsesRequest(in)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := gjson.GetBytes(out, "messages.0.role").String(); got != "system" {
		t.Errorf("messages.0.role = %q, want system", got)
	}
	if got := gjson.GetBytes(out, "messages.0.content").String(); got != "Be terse." {
		t.Errorf("system content: %q", got)
	}
	if got := gjson.GetBytes(out, "messages.1.role").String(); got != "user" {
		t.Errorf("messages.1.role = %q, want user", got)
	}
	// nexus.ext.openai.responses.instructions preserves the original for
	// EncodeResponsesRequest round-trip.
	if got := gjson.GetBytes(out, "nexus.ext.openai.responses.instructions").String(); got != "Be terse." {
		t.Errorf("ext.instructions preservation missing: %s", string(out))
	}
}

// TestDecodeResponsesRequest_InputArray pins shape (b):
// input as an array of input_message items.
func TestDecodeResponsesRequest_InputArray(t *testing.T) {
	in := []byte(`{
		"model": "gpt-5.2",
		"input": [
			{"role":"user","content":[{"type":"input_text","text":"Q1"}]},
			{"role":"assistant","content":[{"type":"output_text","text":"A1"}]},
			{"role":"user","content":[{"type":"input_text","text":"Q2"}]}
		]
	}`)
	out, err := openai.DecodeResponsesRequest(in)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	msgs := gjson.GetBytes(out, "messages")
	if !msgs.IsArray() || len(msgs.Array()) != 3 {
		t.Fatalf("expected 3 messages, got %d; body=%s", len(msgs.Array()), string(out))
	}
	wantRoles := []string{"user", "assistant", "user"}
	for i, role := range wantRoles {
		if got := gjson.GetBytes(out, "messages."+itoa(i)+".role").String(); got != role {
			t.Errorf("messages[%d].role = %q, want %q", i, got, role)
		}
	}
	// First user message: content should be an array with a single text part.
	if got := gjson.GetBytes(out, "messages.0.content.0.type").String(); got != "text" {
		t.Errorf("messages.0.content.0.type = %q, want text", got)
	}
	if got := gjson.GetBytes(out, "messages.0.content.0.text").String(); got != "Q1" {
		t.Errorf("messages.0.content.0.text = %q", got)
	}
}

// TestDecodeResponsesRequest_MaxOutputTokens pins the
// max_output_tokens → max_completion_tokens rename.
func TestDecodeResponsesRequest_MaxOutputTokens(t *testing.T) {
	in := []byte(`{"model":"gpt-5.2","input":"hi","max_output_tokens":1024}`)
	out, err := openai.DecodeResponsesRequest(in)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := gjson.GetBytes(out, "max_completion_tokens").Int(); got != 1024 {
		t.Errorf("max_completion_tokens = %d, want 1024", got)
	}
	if gjson.GetBytes(out, "max_output_tokens").Exists() {
		t.Errorf("Responses-only max_output_tokens leaked into canonical: %s", string(out))
	}
}

// TestDecodeResponsesRequest_ReasoningEffort pins reasoning.effort →
// reasoning_effort (top-level, matching OpenAI chat-completions).
func TestDecodeResponsesRequest_ReasoningEffort(t *testing.T) {
	in := []byte(`{"model":"gpt-5.2","input":"prove 2+2=4","reasoning":{"effort":"high"}}`)
	out, err := openai.DecodeResponsesRequest(in)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := gjson.GetBytes(out, "reasoning_effort").String(); got != "high" {
		t.Errorf("reasoning_effort = %q, want high; body=%s", got, string(out))
	}
}

// TestDecodeResponsesRequest_FunctionTools pins (A)-shape function tool
// normalization to (B)-shape canonical chat-completions.
func TestDecodeResponsesRequest_FunctionTools(t *testing.T) {
	in := []byte(`{
		"model": "gpt-5.2",
		"input": "weather?",
		"tools": [
			{"type":"function","name":"get_weather","description":"...","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}
		]
	}`)
	out, err := openai.DecodeResponsesRequest(in)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := gjson.GetBytes(out, "tools.0.type").String(); got != "function" {
		t.Errorf("tools[0].type = %q, want function", got)
	}
	if got := gjson.GetBytes(out, "tools.0.function.name").String(); got != "get_weather" {
		t.Errorf("tools[0].function.name = %q, want get_weather; body=%s", got, string(out))
	}
	if got := gjson.GetBytes(out, "tools.0.function.parameters.required.0").String(); got != "city" {
		t.Errorf("tools[0].function.parameters.required[0] = %q", got)
	}
	// Built-ins list must be empty (none provided).
	if gjson.GetBytes(out, "nexus.ext.openai.responses.builtin_tools").Exists() {
		t.Errorf("ext.builtin_tools must NOT be set when no built-in tools are provided")
	}
}

// TestDecodeResponsesRequest_BuiltinTools pins partition: function tools
// stay in canonical.tools[]; built-in types move to nexus.ext.openai.responses.builtin_tools[].
func TestDecodeResponsesRequest_BuiltinTools(t *testing.T) {
	in := []byte(`{
		"model": "gpt-5.2",
		"input": "search the web",
		"tools": [
			{"type":"web_search"},
			{"type":"function","name":"my_func","parameters":{}}
		]
	}`)
	out, err := openai.DecodeResponsesRequest(in)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := gjson.GetBytes(out, "tools").Array(); len(got) != 1 {
		t.Errorf("canonical.tools should have 1 entry (function only), got %d; body=%s", len(got), string(out))
	}
	if got := gjson.GetBytes(out, "tools.0.function.name").String(); got != "my_func" {
		t.Errorf("canonical.tools[0].function.name = %q", got)
	}
	builtins := gjson.GetBytes(out, "nexus.ext.openai.responses.builtin_tools").Array()
	if len(builtins) != 1 {
		t.Fatalf("ext.builtin_tools should have 1 entry, got %d", len(builtins))
	}
	if got := builtins[0].Get("type").String(); got != "web_search" {
		t.Errorf("ext.builtin_tools[0].type = %q", got)
	}
}

// TestDecodeResponsesRequest_StatefulFields pins that
// previous_response_id, store, truncation, include all land under
// nexus.ext.openai.responses.* so the cross-format guard can reject
// them and EncodeResponsesRequest restores them.
func TestDecodeResponsesRequest_StatefulFields(t *testing.T) {
	in := []byte(`{
		"model": "gpt-5.2",
		"input": "follow up",
		"previous_response_id": "resp_abc123",
		"store": true,
		"truncation": "auto",
		"include": ["reasoning.summary"]
	}`)
	out, err := openai.DecodeResponsesRequest(in)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := gjson.GetBytes(out, "nexus.ext.openai.responses.previous_response_id").String(); got != "resp_abc123" {
		t.Errorf("previous_response_id not preserved: %s", string(out))
	}
	if got := gjson.GetBytes(out, "nexus.ext.openai.responses.store").Bool(); !got {
		t.Errorf("store not preserved")
	}
	if got := gjson.GetBytes(out, "nexus.ext.openai.responses.truncation").String(); got != "auto" {
		t.Errorf("truncation not preserved: %q", got)
	}
	if got := gjson.GetBytes(out, "nexus.ext.openai.responses.include.0").String(); got != "reasoning.summary" {
		t.Errorf("include not preserved: %s", string(out))
	}
	// And the stateful fields must NOT leak into canonical top-level
	// (or the chat-completions target would reject them).
	for _, k := range []string{"previous_response_id", "store", "truncation", "include"} {
		if gjson.GetBytes(out, k).Exists() {
			t.Errorf("stateful field %q leaked into canonical top-level; body=%s", k, string(out))
		}
	}
}

// TestDecodeResponsesRequest_ResponseFormat pins text.format →
// response_format.
func TestDecodeResponsesRequest_ResponseFormat(t *testing.T) {
	in := []byte(`{
		"model": "gpt-5.2",
		"input": "extract",
		"text": {"format": {"type":"json_schema","json_schema":{"name":"meeting","schema":{"type":"object"}}}}
	}`)
	out, err := openai.DecodeResponsesRequest(in)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := gjson.GetBytes(out, "response_format.type").String(); got != "json_schema" {
		t.Errorf("response_format.type = %q; body=%s", got, string(out))
	}
	if got := gjson.GetBytes(out, "response_format.json_schema.name").String(); got != "meeting" {
		t.Errorf("response_format.json_schema.name = %q", got)
	}
}

// TestDecodeResponsesRequest_StreamPassthrough pins stream:true passes through.
func TestDecodeResponsesRequest_StreamPassthrough(t *testing.T) {
	in := []byte(`{"model":"gpt-5.2","input":"hi","stream":true}`)
	out, err := openai.DecodeResponsesRequest(in)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !gjson.GetBytes(out, "stream").Bool() {
		t.Errorf("stream did not survive: %s", string(out))
	}
}

// TestDecodeResponsesRequest_ErrorEmpty pins that empty body returns
// error (matches existing codec contracts).
func TestDecodeResponsesRequest_ErrorEmpty(t *testing.T) {
	if _, err := openai.DecodeResponsesRequest(nil); err == nil {
		t.Error("expected error on nil body")
	}
	if _, err := openai.DecodeResponsesRequest([]byte{}); err == nil {
		t.Error("expected error on empty body")
	}
	if _, err := openai.DecodeResponsesRequest([]byte("not json")); err == nil {
		t.Error("expected error on invalid JSON")
	}
}

// TestEncodeResponsesRequest_RoundTrip exercises the bidirectional
// contract for the auto-upgrade path: a Responses request that is
// decoded then re-encoded produces a semantically equivalent Responses
// body (same model, instructions, input text, tools).
func TestEncodeResponsesRequest_RoundTrip(t *testing.T) {
	in := []byte(`{
		"model": "gpt-5.2",
		"instructions": "Be terse.",
		"input": [{"role":"user","content":[{"type":"input_text","text":"hi"}]}],
		"max_output_tokens": 200,
		"reasoning": {"effort": "high"},
		"tools": [{"type":"function","name":"get_weather","parameters":{"type":"object"}}],
		"previous_response_id": "resp_abc",
		"store": true
	}`)
	canon, err := openai.DecodeResponsesRequest(in)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	out, err := openai.EncodeResponsesRequest(canon)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if got := gjson.GetBytes(out, "model").String(); got != "gpt-5.2" {
		t.Errorf("model lost; body=%s", string(out))
	}
	if got := gjson.GetBytes(out, "instructions").String(); got != "Be terse." {
		t.Errorf("instructions lost; body=%s", string(out))
	}
	// input array: first item should be a user role with input_text content.
	if got := gjson.GetBytes(out, "input.0.role").String(); got != "user" {
		t.Errorf("input[0].role lost; body=%s", string(out))
	}
	if got := gjson.GetBytes(out, "input.0.content.0.type").String(); got != "input_text" {
		t.Errorf("input[0].content[0].type lost; body=%s", string(out))
	}
	if got := gjson.GetBytes(out, "input.0.content.0.text").String(); got != "hi" {
		t.Errorf("input[0].content[0].text lost; body=%s", string(out))
	}
	if got := gjson.GetBytes(out, "max_output_tokens").Int(); got != 200 {
		t.Errorf("max_output_tokens lost; body=%s", string(out))
	}
	if got := gjson.GetBytes(out, "reasoning.effort").String(); got != "high" {
		t.Errorf("reasoning.effort lost; body=%s", string(out))
	}
	// Tool re-encoded back to flat-shape (responsesToolFromCanonical default).
	if got := gjson.GetBytes(out, "tools.0.type").String(); got != "function" {
		t.Errorf("tools[0].type lost; body=%s", string(out))
	}
	if got := gjson.GetBytes(out, "tools.0.name").String(); got != "get_weather" {
		t.Errorf("tools[0].name lost; body=%s", string(out))
	}
	if got := gjson.GetBytes(out, "previous_response_id").String(); got != "resp_abc" {
		t.Errorf("previous_response_id lost; body=%s", string(out))
	}
	if !gjson.GetBytes(out, "store").Bool() {
		t.Errorf("store lost; body=%s", string(out))
	}
}

// TestEncodeResponsesRequest_ErrorEmpty mirrors the decode error
// contract.
func TestEncodeResponsesRequest_ErrorEmpty(t *testing.T) {
	if _, err := openai.EncodeResponsesRequest(nil); err == nil {
		t.Error("expected error on nil body")
	}
	if _, err := openai.EncodeResponsesRequest([]byte("not json")); err == nil {
		t.Error("expected error on invalid JSON")
	}
}

// itoa avoids strconv import noise in a tiny test helper.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	n := i
	if n < 0 {
		n = -n
	}
	var buf [20]byte
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	if i < 0 {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}

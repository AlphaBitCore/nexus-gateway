package openai

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

func TestExtractRequest_ChatCompletions(t *testing.T) {
	body := []byte(`{
		"model": "gpt-4",
		"messages": [
			{"role": "system", "content": "You are helpful."},
			{"role": "user", "content": "Hello, world!"}
		]
	}`)

	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/v1/chat/completions")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(nc.Segments) != 2 {
		t.Fatalf("expected 2 segments, got %d", len(nc.Segments))
	}
	if nc.Segments[0] != "You are helpful." {
		t.Errorf("segment[0] = %q", nc.Segments[0])
	}
	if nc.Segments[1] != "Hello, world!" {
		t.Errorf("segment[1] = %q", nc.Segments[1])
	}
	if nc.Metadata["model"] != "gpt-4" {
		t.Errorf("model = %q", nc.Metadata["model"])
	}
}

func TestExtractRequest_ChatCompletions_ArrayContent(t *testing.T) {
	body := []byte(`{
		"model": "gpt-4-vision",
		"messages": [
			{"role": "user", "content": [
				{"type": "text", "text": "What is this image?"},
				{"type": "image_url", "image_url": {"url": "https://example.com/img.png"}}
			]}
		]
	}`)

	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/v1/chat/completions")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(nc.Segments) != 1 {
		t.Fatalf("expected 1 text segment, got %d", len(nc.Segments))
	}
	if nc.Segments[0] != "What is this image?" {
		t.Errorf("segment = %q", nc.Segments[0])
	}
}

func TestExtractRequest_Embeddings(t *testing.T) {
	body := []byte(`{
		"model": "text-embedding-3-small",
		"input": ["Hello", "World"]
	}`)

	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/v1/embeddings")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(nc.Segments) != 2 {
		t.Fatalf("expected 2 segments, got %d", len(nc.Segments))
	}
}

func TestExtractRequest_EmbeddingsString(t *testing.T) {
	body := []byte(`{"model": "text-embedding-3-small", "input": "single string"}`)

	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/v1/embeddings")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "single string" {
		t.Errorf("segments = %v", nc.Segments)
	}
}

func TestExtractRequest_ResponsesCreate(t *testing.T) {
	body := []byte(`{
		"model": "gpt-4",
		"input": "Tell me a joke"
	}`)

	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/v1/responses")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "Tell me a joke" {
		t.Errorf("segments = %v", nc.Segments)
	}
}

func TestExtractRequest_UnknownPath(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), []byte(`{}`), "/v1/files")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("expected ErrUnknownSchema, got %v", err)
	}
}

func TestExtractRequest_MalformedJSON(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), []byte(`not json`), "/v1/chat/completions")
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("expected ErrMalformed, got %v", err)
	}
}

func TestExtractRequest_MissingMessages(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), []byte(`{"model":"gpt-4"}`), "/v1/chat/completions")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("expected ErrUnknownSchema for missing messages, got %v", err)
	}
}

func TestExtractResponse_ChatCompletions(t *testing.T) {
	body := []byte(`{
		"choices": [
			{"message": {"role": "assistant", "content": "Hello! How can I help?"}}
		]
	}`)

	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), body, "/v1/chat/completions")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "Hello! How can I help?" {
		t.Errorf("segments = %v", nc.Segments)
	}
}

// TestExtractResponse_ChatCompletions_Refusal covers the audit gap: a
// structured-outputs / o1 refusal must surface on Segments after the
// content slot so compliance hooks scan the assistant's decline message
// just like normal content.
func TestExtractResponse_ChatCompletions_Refusal(t *testing.T) {
	body := []byte(`{
		"choices": [
			{"message": {"role": "assistant", "content": null, "refusal": "I cannot help with that."}}
		]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), body, "/v1/chat/completions")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "I cannot help with that." {
		t.Errorf("Segments=%v want refusal text", nc.Segments)
	}
}

// TestExtractResponse_ChatCompletions_ContentAndRefusal pins the slot
// order: per-choice content first, then refusal — matches the rewrite
// walk so a compliance hook can edit either slot independently.
func TestExtractResponse_ChatCompletions_ContentAndRefusal(t *testing.T) {
	body := []byte(`{
		"choices": [
			{"message": {"role": "assistant", "content": "Partial answer.", "refusal": "Cannot continue."}}
		]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), body, "/v1/chat/completions")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 2 || nc.Segments[0] != "Partial answer." || nc.Segments[1] != "Cannot continue." {
		t.Errorf("Segments=%v want [content, refusal]", nc.Segments)
	}
}

func TestExtractStreamChunk(t *testing.T) {
	chunk := []byte(`{"choices":[{"delta":{"content":"Hello"}}]}`)

	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/v1/chat/completions")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "Hello" {
		t.Errorf("segments = %v", nc.Segments)
	}
}

func TestExtractStreamChunk_EmptyDelta(t *testing.T) {
	chunk := []byte(`{"choices":[{"delta":{}}]}`)

	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/v1/chat/completions")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("expected 0 segments for empty delta, got %d", len(nc.Segments))
	}
}

// TestExtractStreamChunk_ReasoningContent covers the audit gap: DeepSeek-
// reasoner / OpenAI o1/o3 stream `delta.reasoning_content` alongside (or
// instead of) `delta.content`. The reasoning text must land on
// ReasoningSegments, not on Segments — same convention as Anthropic
// thinking_delta.
func TestExtractStreamChunk_ReasoningContent(t *testing.T) {
	a := &Adapter{}
	chunk := []byte(`{"choices":[{"delta":{"reasoning_content":"Step 1 — recall …"}}]}`)
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/v1/chat/completions")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("Segments must not absorb reasoning_content: %v", nc.Segments)
	}
	if len(nc.ReasoningSegments) != 1 || nc.ReasoningSegments[0] != "Step 1 — recall …" {
		t.Errorf("ReasoningSegments=%v", nc.ReasoningSegments)
	}
}

// TestExtractStreamChunk_ContentAndReasoningTogether confirms that when
// both fields appear (visible answer plus reasoning), each goes to its
// own slot. Some providers stream them in the same chunk near the end.
func TestExtractStreamChunk_ContentAndReasoningTogether(t *testing.T) {
	a := &Adapter{}
	chunk := []byte(`{"choices":[{"delta":{"content":"Done.","reasoning_content":"Final check …"}}]}`)
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/v1/chat/completions")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "Done." {
		t.Errorf("Segments=%v", nc.Segments)
	}
	if len(nc.ReasoningSegments) != 1 || nc.ReasoningSegments[0] != "Final check …" {
		t.Errorf("ReasoningSegments=%v", nc.ReasoningSegments)
	}
}

// TestExtractStreamChunk_DeltaRefusal pins that streaming structured-
// outputs / o1 refusal fragments land on Segments alongside content.
func TestExtractStreamChunk_DeltaRefusal(t *testing.T) {
	a := &Adapter{}
	chunk := []byte(`{"choices":[{"delta":{"refusal":"I cannot help"}}]}`)
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/v1/chat/completions")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "I cannot help" {
		t.Errorf("Segments=%v want refusal", nc.Segments)
	}
}

func TestAdapter_ID(t *testing.T) {
	a := &Adapter{}
	if a.ID() != "openai-compat" {
		t.Errorf("ID = %q", a.ID())
	}
}

func TestAdapter_Configure(t *testing.T) {
	a := &Adapter{}
	if err := a.Configure(nil); err != nil {
		t.Errorf("Configure(nil) = %v", err)
	}
	if err := a.Configure(map[string]any{"foo": "bar"}); err != nil {
		t.Errorf("Configure(map) = %v", err)
	}
}

// Tool-call / function-call coverage (compliance-critical: assistant
// tool_use must be visible to hooks for MCP tool-name detection, PII
// leakage scans, and audit accounting separate from text completions).

// TestExtractRequest_ToolCallsInHistory covers an assistant message in the
// conversation history that carries tool_calls (current spec) — those
// invocations land on ToolCallSegments so a compliance hook scanning
// prior tool arguments still sees them.
func TestExtractRequest_ToolCallsInHistory(t *testing.T) {
	body := []byte(`{
		"model": "gpt-4o",
		"messages": [
			{"role": "user", "content": "what is the weather in NYC?"},
			{"role": "assistant", "content": null, "tool_calls": [
				{"id": "call_abc", "type": "function", "function": {"name": "get_weather", "arguments": "{\"city\":\"NYC\"}"}}
			]},
			{"role": "tool", "tool_call_id": "call_abc", "content": "72F sunny"}
		]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/v1/chat/completions")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.ToolCallSegments) != 1 {
		t.Fatalf("ToolCallSegments len=%d want 1", len(nc.ToolCallSegments))
	}
	if !strings.Contains(nc.ToolCallSegments[0], `"get_weather"`) {
		t.Errorf("ToolCallSegments[0]=%q missing function name", nc.ToolCallSegments[0])
	}
	if !strings.Contains(nc.ToolCallSegments[0], `\"city\":\"NYC\"`) {
		t.Errorf("ToolCallSegments[0]=%q missing arguments", nc.ToolCallSegments[0])
	}
}

// TestExtractRequest_LegacyFunctionCall covers the deprecated single
// `function_call` shape (pre-2024). Some clients still send this; we
// must capture it so older traffic doesn't slip past compliance.
func TestExtractRequest_LegacyFunctionCall(t *testing.T) {
	body := []byte(`{
		"model": "gpt-3.5-turbo",
		"messages": [
			{"role": "user", "content": "weather"},
			{"role": "assistant", "content": null, "function_call": {"name": "get_weather", "arguments": "{\"city\":\"NYC\"}"}}
		]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/v1/chat/completions")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.ToolCallSegments) != 1 {
		t.Fatalf("ToolCallSegments len=%d want 1", len(nc.ToolCallSegments))
	}
	if !strings.Contains(nc.ToolCallSegments[0], `"get_weather"`) {
		t.Errorf("ToolCallSegments[0]=%q missing function name", nc.ToolCallSegments[0])
	}
}

// TestExtractRequest_ToolDefinitionsInMetadata pins that the tools array
// (definitions, not invocations) lands in Metadata["tools"] as raw JSON
// so audit can record "what was the assistant authorized to call".
func TestExtractRequest_ToolDefinitionsInMetadata(t *testing.T) {
	body := []byte(`{
		"model": "gpt-4o",
		"messages": [{"role": "user", "content": "hi"}],
		"tools": [
			{"type": "function", "function": {"name": "get_weather", "parameters": {"type": "object"}}}
		]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/v1/chat/completions")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	tools := nc.Metadata["tools"]
	if !strings.Contains(tools, `"get_weather"`) {
		t.Errorf("Metadata[tools]=%q missing definition", tools)
	}
	// Tools definitions are NOT invocations — must not leak into ToolCallSegments.
	if len(nc.ToolCallSegments) != 0 {
		t.Errorf("ToolCallSegments=%v should stay empty for definition-only request", nc.ToolCallSegments)
	}
}

// TestExtractRequest_Extra covers the safety-net path: unrecognised
// top-level fields land in Extra so a future spec addition (a brand-new
// field the adapter has not been updated for) still reaches compliance
// hooks doing defence-in-depth scans.
func TestExtractRequest_Extra(t *testing.T) {
	body := []byte(`{
		"model": "gpt-4o",
		"messages": [{"role": "user", "content": "hi"}],
		"x_future_field": {"sensitive": "this must not be silently dropped"}
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/v1/chat/completions")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	xff, ok := nc.Extra["x_future_field"]
	if !ok {
		t.Fatalf("Extra missing x_future_field; Extra=%v", nc.Extra)
	}
	if !strings.Contains(xff, "sensitive") {
		t.Errorf("Extra[x_future_field]=%q missing payload", xff)
	}
	// Known keys must not bleed into Extra.
	for _, known := range []string{"model", "messages", "tools"} {
		if _, ok := nc.Extra[known]; ok {
			t.Errorf("Extra leaked known key %q: %q", known, nc.Extra[known])
		}
	}
}

// TestExtractResponse_ToolCalls covers the response-side audit gap: an
// assistant turn that calls a tool returns content=null + tool_calls; the
// invocation must reach ToolCallSegments.
func TestExtractResponse_ToolCalls(t *testing.T) {
	body := []byte(`{
		"id": "chatcmpl-abc", "object": "chat.completion", "model": "gpt-4o",
		"choices": [{
			"index": 0,
			"message": {"role": "assistant", "content": null, "tool_calls": [
				{"id": "call_xyz", "type": "function", "function": {"name": "send_email", "arguments": "{\"to\":\"a@b.com\"}"}}
			]},
			"finish_reason": "tool_calls"
		}]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), body, "/v1/chat/completions")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.ToolCallSegments) != 1 {
		t.Fatalf("ToolCallSegments len=%d want 1", len(nc.ToolCallSegments))
	}
	if !strings.Contains(nc.ToolCallSegments[0], `"send_email"`) {
		t.Errorf("ToolCallSegments[0]=%q missing name", nc.ToolCallSegments[0])
	}
	if nc.Metadata["finish_reason"] != "tool_calls" {
		t.Errorf("finish_reason=%q want tool_calls", nc.Metadata["finish_reason"])
	}
}

// TestExtractResponse_LegacyFunctionCall covers the deprecated single
// function_call shape on the response side.
func TestExtractResponse_LegacyFunctionCall(t *testing.T) {
	body := []byte(`{
		"choices": [{
			"message": {"role": "assistant", "content": null, "function_call": {"name": "send_email", "arguments": "{\"to\":\"x@y.com\"}"}},
			"finish_reason": "function_call"
		}]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), body, "/v1/chat/completions")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.ToolCallSegments) != 1 || !strings.Contains(nc.ToolCallSegments[0], `"send_email"`) {
		t.Errorf("ToolCallSegments=%v", nc.ToolCallSegments)
	}
}

// TestExtractResponse_SystemFingerprint pins that system_fingerprint is
// captured into Metadata so audit can correlate model deployment across
// requests (fingerprint changes when OpenAI rolls a model out).
func TestExtractResponse_SystemFingerprint(t *testing.T) {
	body := []byte(`{
		"model": "gpt-4o",
		"system_fingerprint": "fp_abc123",
		"service_tier": "default",
		"choices": [{"message": {"role": "assistant", "content": "hi"}, "finish_reason": "stop"}]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), body, "/v1/chat/completions")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if nc.Metadata["system_fingerprint"] != "fp_abc123" {
		t.Errorf("system_fingerprint=%q", nc.Metadata["system_fingerprint"])
	}
	if nc.Metadata["service_tier"] != "default" {
		t.Errorf("service_tier=%q", nc.Metadata["service_tier"])
	}
	if nc.Metadata["finish_reason"] != "stop" {
		t.Errorf("finish_reason=%q", nc.Metadata["finish_reason"])
	}
}

// TestExtractResponse_MultiChoiceFinishReason pins that with multiple
// choices (n>1), all finish_reasons land in Metadata comma-joined so
// hooks see the full picture.
func TestExtractResponse_MultiChoiceFinishReason(t *testing.T) {
	body := []byte(`{
		"choices": [
			{"message": {"role": "assistant", "content": "first"}, "finish_reason": "stop"},
			{"message": {"role": "assistant", "content": null, "tool_calls": [{"id":"x","type":"function","function":{"name":"f","arguments":"{}"}}]}, "finish_reason": "tool_calls"}
		]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), body, "/v1/chat/completions")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if nc.Metadata["finish_reason"] != "stop,tool_calls" {
		t.Errorf("finish_reason=%q want stop,tool_calls", nc.Metadata["finish_reason"])
	}
}

// TestExtractResponse_Extra covers the response-side defence-in-depth:
// unrecognised top-level fields (e.g. a future `citations` block) reach
// Extra instead of being silently dropped.
func TestExtractResponse_Extra(t *testing.T) {
	body := []byte(`{
		"choices": [{"message": {"role": "assistant", "content": "hi"}, "finish_reason": "stop"}],
		"x_audit_payload": {"new": "spec field"}
	}`)
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), body, "/v1/chat/completions")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if _, ok := nc.Extra["x_audit_payload"]; !ok {
		t.Errorf("Extra missing x_audit_payload: %v", nc.Extra)
	}
}

// TestExtractStreamChunk_ToolCallDelta covers per-chunk tool_call delta:
// each streaming chunk carries one or more tool_call partials (function
// arguments stream char-by-char). The chunk delta lands on
// ToolCallSegments and the streaming pipeline accumulates across chunks.
func TestExtractStreamChunk_ToolCallDelta(t *testing.T) {
	chunk := []byte(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"send_email","arguments":""}}]}}]}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/v1/chat/completions")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.ToolCallSegments) != 1 || !strings.Contains(nc.ToolCallSegments[0], `"send_email"`) {
		t.Errorf("ToolCallSegments=%v", nc.ToolCallSegments)
	}
}

// TestExtractStreamChunk_ToolCallArgumentsDelta pins the streaming
// partial-arguments shape: subsequent chunks carry only argument fragments.
// Each fragment must reach ToolCallSegments so the pipeline can accumulate.
func TestExtractStreamChunk_ToolCallArgumentsDelta(t *testing.T) {
	chunk := []byte(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"to\":\"a@b.com\"}"}}]}}]}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/v1/chat/completions")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.ToolCallSegments) != 1 {
		t.Fatalf("ToolCallSegments len=%d", len(nc.ToolCallSegments))
	}
	if !strings.Contains(nc.ToolCallSegments[0], `\"to\":\"a@b.com\"`) {
		t.Errorf("ToolCallSegments[0]=%q missing argument fragment", nc.ToolCallSegments[0])
	}
}

// TestExtractStreamChunk_FinishReason pins that the terminal chunk's
// finish_reason reaches Metadata so the pipeline knows why the stream ended.
func TestExtractStreamChunk_FinishReason(t *testing.T) {
	chunk := []byte(`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/v1/chat/completions")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if nc.Metadata["finish_reason"] != "tool_calls" {
		t.Errorf("finish_reason=%q", nc.Metadata["finish_reason"])
	}
}

// TestExtractStreamChunk_LegacyFunctionCallDelta covers the deprecated
// single function_call streaming shape; some older clients still emit it.
func TestExtractStreamChunk_LegacyFunctionCallDelta(t *testing.T) {
	chunk := []byte(`{"choices":[{"delta":{"function_call":{"name":"f","arguments":"{}"}}}]}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/v1/chat/completions")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.ToolCallSegments) != 1 || !strings.Contains(nc.ToolCallSegments[0], `"f"`) {
		t.Errorf("ToolCallSegments=%v", nc.ToolCallSegments)
	}
}

// TestExtractRequest_ResponsesAPI_FunctionCallEcho covers an echo of a
// previously-emitted tool invocation in the responses API input list.
func TestExtractRequest_ResponsesAPI_FunctionCallEcho(t *testing.T) {
	body := []byte(`{
		"model": "gpt-4o",
		"input": [
			{"role": "user", "content": "weather"},
			{"type": "function_call", "id": "fc_1", "name": "get_weather", "arguments": "{\"city\":\"NYC\"}"},
			{"type": "function_call_output", "call_id": "fc_1", "output": "72F"}
		]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/v1/responses")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.ToolCallSegments) != 1 || !strings.Contains(nc.ToolCallSegments[0], `"get_weather"`) {
		t.Errorf("ToolCallSegments=%v", nc.ToolCallSegments)
	}
}

// TestExtractRequest_ResponsesAPI_InstructionsBecomePrompt pins that the
// top-level `instructions` (responses-API system prompt) lands at the
// front of Segments so compliance scans see the system prompt content.
func TestExtractRequest_ResponsesAPI_InstructionsBecomePrompt(t *testing.T) {
	body := []byte(`{
		"model": "gpt-4o",
		"instructions": "You are a careful assistant.",
		"input": "hello"
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/v1/responses")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 2 {
		t.Fatalf("Segments len=%d want 2", len(nc.Segments))
	}
	if nc.Segments[0] != "You are a careful assistant." {
		t.Errorf("Segments[0]=%q want instructions first", nc.Segments[0])
	}
	if nc.Segments[1] != "hello" {
		t.Errorf("Segments[1]=%q", nc.Segments[1])
	}
}

// TestExtractResponse_ResponsesAPI_FunctionCallOutput covers function_call
// output items in /v1/responses non-streaming responses landing on
// ToolCallSegments.
func TestExtractResponse_ResponsesAPI_FunctionCallOutput(t *testing.T) {
	body := []byte(`{
		"id": "resp_1", "object": "response", "model": "gpt-4o", "status": "completed",
		"output": [
			{"type": "function_call", "id": "fc_2", "name": "send_email", "arguments": "{\"to\":\"x@y.com\"}"}
		]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), body, "/v1/responses")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.ToolCallSegments) != 1 || !strings.Contains(nc.ToolCallSegments[0], `"send_email"`) {
		t.Errorf("ToolCallSegments=%v", nc.ToolCallSegments)
	}
	if nc.Metadata["status"] != "completed" {
		t.Errorf("status=%q", nc.Metadata["status"])
	}
}

// Embeddings + chat-request edge paths.

func TestExtractRequest_Embeddings_Malformed(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), []byte(`{bad`), "/v1/embeddings")
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("err=%v want ErrMalformed", err)
	}
}

func TestExtractRequest_Embeddings_MissingInput(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), []byte(`{"model":"emb"}`), "/v1/embeddings")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

// `functions` legacy field (pre-tool_calls spec) must land in Metadata
// alongside the `tools` modern field so audit captures both shapes.
func TestExtractRequest_ChatCompletions_LegacyFunctionsMetadata(t *testing.T) {
	body := []byte(`{
		"model":"gpt-3.5-turbo",
		"messages":[{"role":"user","content":"hi"}],
		"functions":[{"name":"get_weather","parameters":{}}]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/v1/chat/completions")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if !strings.Contains(nc.Metadata["functions"], `"get_weather"`) {
		t.Errorf("Metadata[functions]=%q missing legacy decl", nc.Metadata["functions"])
	}
}

// extractStreamDelta — malformed JSON.

func TestExtractStreamChunk_Malformed(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractStreamChunk(context.Background(), []byte(`{bad`), "/v1/chat/completions")
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("err=%v want ErrMalformed", err)
	}
}

// ExtractResponse path dispatch — /embeddings is a no-op (float arrays
// have no text to extract), unknown paths return ErrUnknownSchema.

func TestExtractResponse_EmbeddingsNoOp(t *testing.T) {
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(),
		[]byte(`{"data":[{"embedding":[0.1,0.2]}]}`), "/v1/embeddings")
	if err != nil {
		t.Fatalf("err=%v want nil for embeddings", err)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("Segments=%v want empty (embeddings have no text)", nc.Segments)
	}
}

func TestExtractResponse_UnknownPath(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), []byte(`{}`), "/v1/files")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

func TestExtractResponse_MalformedJSON(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), []byte(`{bad`), "/v1/chat/completions")
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("err=%v want ErrMalformed", err)
	}
}

func TestExtractResponse_MissingChoices(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), []byte(`{"model":"gpt-4o"}`), "/v1/chat/completions")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

// extractResponsesCreate edge paths.

// Malformed JSON on /v1/responses must return ErrMalformed.
func TestExtractRequest_ResponsesCreate_Malformed(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), []byte(`{bad`), "/v1/responses")
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("err=%v want ErrMalformed", err)
	}
}

func TestExtractRequest_ResponsesCreate_MissingInput(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), []byte(`{"model":"gpt-4"}`), "/v1/responses")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

// Array input where each item.content is a plain string (not an array of
// parts). The extractor must walk both string-content and array-content
// shapes within the same input list.
func TestExtractRequest_ResponsesCreate_ArrayStringContent(t *testing.T) {
	body := []byte(`{
		"model":"gpt-4o",
		"input":[
			{"role":"user","content":"hello"},
			{"role":"user","content":[{"type":"input_text","text":"world"}]}
		]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/v1/responses")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 2 || nc.Segments[0] != "hello" || nc.Segments[1] != "world" {
		t.Errorf("Segments=%v want [hello world]", nc.Segments)
	}
}

// `tools` array at the top of a /v1/responses body must land in Metadata
// so audit captures the model's authorized function scope.
func TestExtractRequest_ResponsesCreate_ToolsMetadata(t *testing.T) {
	body := []byte(`{
		"model":"gpt-4o",
		"input":"hi",
		"tools":[{"type":"function","name":"get_weather","parameters":{}}]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/v1/responses")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if !strings.Contains(nc.Metadata["tools"], `"get_weather"`) {
		t.Errorf("Metadata[tools]=%q missing decl", nc.Metadata["tools"])
	}
}

// extractResponsesResponse edge paths.

func TestExtractResponse_ResponsesAPI_Malformed(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), []byte(`{bad`), "/v1/responses")
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("err=%v want ErrMalformed", err)
	}
}

// Responses API: text output items surface as Segments via output_text parts;
// the id field lands in Metadata.
func TestExtractResponse_ResponsesAPI_OutputTextAndID(t *testing.T) {
	body := []byte(`{
		"id":"resp_xyz","model":"gpt-4o","status":"completed",
		"output":[
			{"type":"message","content":[
				{"type":"output_text","text":"final answer"}
			]}
		]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), body, "/v1/responses")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "final answer" {
		t.Errorf("Segments=%v", nc.Segments)
	}
	if nc.Metadata["id"] != "resp_xyz" {
		t.Errorf("id=%q", nc.Metadata["id"])
	}
	if nc.Metadata["model"] != "gpt-4o" {
		t.Errorf("model=%q", nc.Metadata["model"])
	}
}

// Normalize — Tier-1 dispatch via the unified extract helper.

// TestExtractRequest_AssistantToolOnlyMessage pins behavior for an
// assistant message with content=null and only tool_calls (typical
// agent-loop request). Must not error and must capture the invocation.
func TestExtractRequest_AssistantToolOnlyMessage(t *testing.T) {
	body := []byte(`{
		"model": "gpt-4o",
		"messages": [
			{"role": "user", "content": "schedule meeting"},
			{"role": "assistant", "content": null, "tool_calls": [
				{"id": "c1", "type": "function", "function": {"name": "create_event", "arguments": "{}"}}
			]}
		]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/v1/chat/completions")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "schedule meeting" {
		t.Errorf("Segments=%v", nc.Segments)
	}
	if len(nc.ToolCallSegments) != 1 {
		t.Errorf("ToolCallSegments=%v", nc.ToolCallSegments)
	}
}

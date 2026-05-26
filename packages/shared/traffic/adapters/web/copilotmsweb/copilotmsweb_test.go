package copilotmsweb

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// Fixtures cover the three plausible Copilot wire shapes documented in
// the package: Modern Copilot (author/text), Legacy Sydney (arguments
// envelope), and OpenAI-compat (role/content). Tests verify the
// adapter detects each shape correctly and extracts content + tool
// invocations to the right slots.

func TestAdapter_ID(t *testing.T) {
	a := &Adapter{}
	if a.ID() != "copilot-ms-web" {
		t.Errorf("ID=%q", a.ID())
	}
}

func TestAdapter_Configure(t *testing.T) {
	a := &Adapter{}
	if err := a.Configure(nil); err != nil {
		t.Errorf("Configure(nil)=%v", err)
	}
}

// Modern Copilot shape (author / text)

func TestExtractRequest_ModernCopilotShape(t *testing.T) {
	body := []byte(`{
		"messages": [
			{"author":"user","text":"Why is the sky blue?","contentType":"Text"}
		],
		"conversationId":"conv-1",
		"session_id":"sess-1",
		"model":"copilot-4-turbo"
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/c/api/chat")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "Why is the sky blue?" {
		t.Errorf("Segments=%v", nc.Segments)
	}
	if nc.Metadata["model"] != "copilot-4-turbo" {
		t.Errorf("model=%q", nc.Metadata["model"])
	}
	if nc.Metadata["conversationId"] != "conv-1" {
		t.Errorf("conversationId=%q", nc.Metadata["conversationId"])
	}
}

// OpenAI-compatible shape (role / content)

func TestExtractRequest_OpenAICompatShape(t *testing.T) {
	body := []byte(`{
		"messages": [
			{"role":"user","content":"Hello from OpenAI-compat shape"}
		]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/chat/completions")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "Hello from OpenAI-compat shape" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestExtractRequest_OpenAICompatToolCalls(t *testing.T) {
	body := []byte(`{
		"messages": [
			{"role":"user","content":"weather"},
			{"role":"assistant","content":null,"tool_calls":[
				{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{}"}}
			]}
		]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/chat/completions")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.ToolCallSegments) != 1 || !strings.Contains(nc.ToolCallSegments[0], `"get_weather"`) {
		t.Errorf("ToolCallSegments=%v", nc.ToolCallSegments)
	}
}

// Legacy Sydney/Bing shape (arguments envelope)

func TestExtractRequest_LegacySydneyShape(t *testing.T) {
	body := []byte(`{
		"arguments": [{
			"source":"cib",
			"optionsSets":["nlu_direct_response_filter","deepleo"],
			"isStartOfSession":true,
			"message":{"author":"user","text":"Tell me about quantum computing","messageType":"Chat"}
		}],
		"invocationId":"42",
		"target":"chat",
		"type":4
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/sydney/ChatHub")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "Tell me about quantum computing" {
		t.Errorf("Segments=%v", nc.Segments)
	}
	if nc.Metadata["invocationId"] != "42" {
		t.Errorf("invocationId=%q", nc.Metadata["invocationId"])
	}
	if nc.Metadata["target"] != "chat" {
		t.Errorf("target=%q", nc.Metadata["target"])
	}
}

func TestExtractRequest_LegacySydneyShape_PreviousMessages(t *testing.T) {
	// Sydney sometimes carries prior turns in `previousMessages`.
	body := []byte(`{
		"arguments": [{
			"message":{"author":"user","text":"new turn"},
			"previousMessages":[
				{"author":"user","text":"prior turn 1"},
				{"author":"bot","text":"prior reply 1"}
			]
		}]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/sydney/ChatHub")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 3 {
		t.Fatalf("Segments len=%d want 3", len(nc.Segments))
	}
}

// Defensive paths

func TestExtractRequest_Extra(t *testing.T) {
	body := []byte(`{
		"messages":[{"author":"user","text":"hi","contentType":"Text"}],
		"x_future_field":{"sensitive":"data"}
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/c/api/chat")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if x, ok := nc.Extra["x_future_field"]; !ok || !strings.Contains(x, "sensitive") {
		t.Errorf("Extra=%v missing x_future_field", nc.Extra)
	}
}

func TestExtractRequest_UnknownShape(t *testing.T) {
	body := []byte(`{"foo":"bar"}`)
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), body, "/c/api/chat")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

func TestExtractRequest_EmptyBody(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), nil, "/c/api/chat")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

func TestExtractRequest_Malformed(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), []byte(`not json`), "/c/api/chat")
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("err=%v want ErrMalformed", err)
	}
}

// ExtractStreamChunk (three shapes)

func TestExtractStreamChunk_OpenAICompatDelta(t *testing.T) {
	chunk := []byte(`{"choices":[{"delta":{"content":"Hello"}}]}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/api/chat/completions")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "Hello" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestExtractStreamChunk_OpenAICompatToolCallDelta(t *testing.T) {
	chunk := []byte(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"f","arguments":""}}]}}]}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/api/chat/completions")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.ToolCallSegments) != 1 {
		t.Errorf("ToolCallSegments=%v", nc.ToolCallSegments)
	}
}

func TestExtractStreamChunk_OpenAICompatFinishReason(t *testing.T) {
	chunk := []byte(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/api/chat/completions")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if nc.Metadata["finish_reason"] != "stop" {
		t.Errorf("finish_reason=%q", nc.Metadata["finish_reason"])
	}
}

func TestExtractStreamChunk_SydneyUpdate(t *testing.T) {
	// Sydney WebSocket frame type=1 is an update with messages array.
	chunk := []byte(`{"type":1,"target":"update","arguments":[{"messages":[{"author":"bot","text":"streamed delta"}]}]}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/sydney/ChatHub")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "streamed delta" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestExtractStreamChunk_PlainTextChunk(t *testing.T) {
	chunk := []byte(`{"text":"plain text chunk"}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/c/api/chat")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "plain text chunk" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestExtractStreamChunk_DefensiveOnInvalidJSON(t *testing.T) {
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), []byte(`not json`), "/c/api/chat")
	if err != nil {
		t.Errorf("err=%v want nil for invalid JSON", err)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestExtractStreamChunk_EmptyChunk(t *testing.T) {
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), nil, "/c/api/chat")
	if err != nil {
		t.Errorf("err=%v", err)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

// ExtractResponse — error envelope + OpenAI-compat happy path

func TestExtractResponse_ErrorEnvelope(t *testing.T) {
	body := []byte(`{"error":{"code":"unauthenticated","message":"401 unauthorized"}}`)
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), body, "/c/api/chat")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "401 unauthorized" {
		t.Errorf("Segments=%v", nc.Segments)
	}
	if nc.Metadata["error"] != "true" {
		t.Errorf("error=%q", nc.Metadata["error"])
	}
}

func TestExtractResponse_OpenAICompatHappyPath(t *testing.T) {
	body := []byte(`{
		"model":"copilot-4",
		"choices":[{"message":{"role":"assistant","content":"hi","tool_calls":[
			{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}
		]}}]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), body, "/api/chat/completions")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "hi" {
		t.Errorf("Segments=%v", nc.Segments)
	}
	if len(nc.ToolCallSegments) != 1 || !strings.Contains(nc.ToolCallSegments[0], `"f"`) {
		t.Errorf("ToolCallSegments=%v", nc.ToolCallSegments)
	}
	if nc.Metadata["model"] != "copilot-4" {
		t.Errorf("model=%q", nc.Metadata["model"])
	}
}

func TestExtractResponse_EmptyBody(t *testing.T) {
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), nil, "/c/api/chat")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestExtractResponse_NonErrorJSON(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), []byte(`{"foo":"bar"}`), "/c/api/chat")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

// DetectRequestMeta + DetectResponseUsage + Rewrite contracts

func TestDetectRequestMeta(t *testing.T) {
	body := []byte(`{"messages":[{"author":"user","text":"hi"}],"model":"copilot-4"}`)
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost, "https://copilot.microsoft.com/c/api/chat", nil)
	meta := a.DetectRequestMeta(r, body)
	if meta.Provider != "copilot-ms-web" {
		t.Errorf("Provider=%q", meta.Provider)
	}
	if meta.Model != "copilot-4" {
		t.Errorf("Model=%q", meta.Model)
	}
}

func TestDetectResponseUsage_NonLLMSentinel(t *testing.T) {
	a := &Adapter{}
	usage := a.DetectResponseUsage(nil, []byte(`{}`))
	if usage.Status != traffic.UsageStatusNonLLM {
		t.Errorf("Status=%q", usage.Status)
	}
}

func TestRewriteRequestBody_Unsupported(t *testing.T) {
	a := &Adapter{}
	body := []byte(`{"messages":[{"author":"user","text":"hi"}]}`)
	out, n, err := a.RewriteRequestBody(context.Background(), body, "/c/api/chat", traffic.NormalizedContent{})
	if !errors.Is(err, traffic.ErrRewriteUnsupported) {
		t.Errorf("err=%v want ErrRewriteUnsupported", err)
	}
	if n != 0 || string(out) != string(body) {
		t.Errorf("body modified")
	}
}

func TestRewriteResponseBody_Unsupported(t *testing.T) {
	a := &Adapter{}
	body := []byte(`{}`)
	out, n, err := a.RewriteResponseBody(context.Background(), body, "/c/api/chat", traffic.NormalizedContent{})
	if !errors.Is(err, traffic.ErrRewriteUnsupported) {
		t.Errorf("err=%v want ErrRewriteUnsupported", err)
	}
	if n != 0 || string(out) != string(body) {
		t.Errorf("body modified")
	}
}

// extractMessagesShape — gap-closing branches

// TestExtractRequest_OpenAICompatContentArray covers the multi-part
// content path: content is an array of {type:"text", text:"..."} parts.
func TestExtractRequest_OpenAICompatContentArray(t *testing.T) {
	body := []byte(`{
		"messages":[
			{"role":"user","content":[
				{"type":"text","text":"part one"},
				{"type":"image_url","image_url":{"url":"https://x"}},
				{"type":"text","text":"part two"}
			]}
		]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/chat/completions")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 2 || nc.Segments[0] != "part one" || nc.Segments[1] != "part two" {
		t.Errorf("Segments=%v want [part one, part two]", nc.Segments)
	}
}

// TestExtractRequest_OpenAICompatFunctionCall covers the legacy
// function_call object echo branch (extractMessagesShape lines 111-113).
func TestExtractRequest_OpenAICompatFunctionCall(t *testing.T) {
	body := []byte(`{
		"messages":[
			{"role":"user","content":"weather"},
			{"role":"assistant","content":null,
			 "function_call":{"name":"get_weather","arguments":"{\"city\":\"NYC\"}"}}
		]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/chat/completions")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.ToolCallSegments) != 1 || !strings.Contains(nc.ToolCallSegments[0], `"get_weather"`) {
		t.Errorf("ToolCallSegments=%v", nc.ToolCallSegments)
	}
}

// TestExtractRequest_AllMetadataFields exercises all four metadata
// stamping paths simultaneously: model, conversationId, conversation_id
// (snake_case), session_id, and tools (lines 128-130, 134-136).
func TestExtractRequest_AllMetadataFields(t *testing.T) {
	body := []byte(`{
		"messages":[{"author":"user","text":"hello"}],
		"model":"copilot-4",
		"conversationId":"conv-camel",
		"conversation_id":"conv-snake",
		"session_id":"sess-1",
		"tools":[{"type":"function","function":{"name":"f"}}]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/c/api/chat")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if nc.Metadata["model"] != "copilot-4" {
		t.Errorf("model=%q", nc.Metadata["model"])
	}
	if nc.Metadata["conversationId"] != "conv-camel" {
		t.Errorf("conversationId=%q", nc.Metadata["conversationId"])
	}
	if nc.Metadata["conversation_id"] != "conv-snake" {
		t.Errorf("conversation_id=%q", nc.Metadata["conversation_id"])
	}
	if nc.Metadata["session_id"] != "sess-1" {
		t.Errorf("session_id=%q", nc.Metadata["session_id"])
	}
	if !strings.Contains(nc.Metadata["tools"], `"f"`) {
		t.Errorf("tools=%q", nc.Metadata["tools"])
	}
}

// ExtractResponse — error-message string branch

// TestExtractResponse_TopLevelMessageError covers the `message` string
// error branch (lines 197-202) — older Copilot error envelopes used
// `{"message":"..."}` without an `error` wrapper.
func TestExtractResponse_TopLevelMessageError(t *testing.T) {
	body := []byte(`{"message":"rate limited"}`)
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), body, "/c/api/chat")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "rate limited" {
		t.Errorf("Segments=%v", nc.Segments)
	}
	if nc.Metadata["error"] != "true" {
		t.Errorf("error=%q", nc.Metadata["error"])
	}
}

// TestExtractResponse_Malformed covers the gjson.ValidBytes guard
// (lines 188-190).
func TestExtractResponse_Malformed(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), []byte(`not json`), "/c/api/chat")
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("err=%v want ErrMalformed", err)
	}
}

// ExtractStreamChunk — unrecognised-shape fall-through

// TestExtractStreamChunk_UnrecognisedShape covers the final return at
// line 295: valid JSON, none of the three known shapes match.
func TestExtractStreamChunk_UnrecognisedShape(t *testing.T) {
	chunk := []byte(`{"event":"keepalive","timestamp":1234}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/c/api/chat")
	if err != nil {
		t.Errorf("err=%v want nil for unrecognised shape", err)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("Segments=%v want empty", nc.Segments)
	}
}

// DetectRequestMeta — invalid-JSON early return

// TestDetectRequestMeta_InvalidJSONBody covers the early-return at
// lines 302-304 when the body fails ValidBytes.
func TestDetectRequestMeta_InvalidJSONBody(t *testing.T) {
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost, "https://copilot.microsoft.com/c/api/chat", nil)
	meta := a.DetectRequestMeta(r, []byte(`not json`))
	if meta.Provider != "copilot-ms-web" {
		t.Errorf("Provider=%q", meta.Provider)
	}
	if meta.Model != "" {
		t.Errorf("Model=%q want empty for invalid JSON body", meta.Model)
	}
}

// Normalize (Tier-1 spec dispatch)

// TestNormalize_RequestChatShape pins that an openai-chat-shaped
// request body claims Tier 1 via the openai-chat spec and stamps
// DetectedSpec = "copilot-ms-web".
func TestNormalize_RequestChatShape(t *testing.T) {
	body := []byte(`{
		"model":"copilot-4",
		"messages":[
			{"role":"system","content":"You are Copilot."},
			{"role":"user","content":"hello"}
		]
	}`)
	a := &Adapter{}
	payload, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType:  "copilot-ms-web",
		Direction:    normalize.DirectionRequest,
		ContentType:  "application/json",
		EndpointPath: "/api/chat/completions",
	})
	if err != nil {
		t.Fatalf("Normalize err=%v", err)
	}
	if payload.Kind != normalize.KindAIChat {
		t.Errorf("Kind=%v want ai-chat", payload.Kind)
	}
	if payload.DetectedSpec != "copilot-ms-web" {
		t.Errorf("DetectedSpec=%q want copilot-ms-web", payload.DetectedSpec)
	}
}

// TestNormalize_UnrecognisedShape verifies that a body matching neither
// the request nor response specs returns ErrUnsupported so the
// Coordinator can fall through.
func TestNormalize_UnrecognisedShape(t *testing.T) {
	body := []byte(`{"foo":"bar","baz":42}`)
	a := &Adapter{}
	_, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType: "copilot-ms-web",
		Direction:   normalize.DirectionRequest,
		ContentType: "application/json",
	})
	if !errors.Is(err, normalize.ErrUnsupported) {
		t.Errorf("err=%v want ErrUnsupported", err)
	}
}

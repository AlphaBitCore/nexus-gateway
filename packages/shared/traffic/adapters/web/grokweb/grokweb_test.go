package grokweb

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

func TestAdapter_ID(t *testing.T) {
	a := &Adapter{}
	if a.ID() != "grok-web" {
		t.Errorf("ID=%q", a.ID())
	}
}

func TestAdapter_Configure(t *testing.T) {
	a := &Adapter{}
	if err := a.Configure(nil); err != nil {
		t.Errorf("Configure(nil)=%v", err)
	}
}

func TestExtractRequest_OpenAICompatMessages(t *testing.T) {
	body := []byte(`{
		"model":"grok-3",
		"messages":[{"role":"user","content":"why is the sky blue"}]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/conversations/new")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "why is the sky blue" {
		t.Errorf("Segments=%v", nc.Segments)
	}
	if nc.Metadata["model"] != "grok-3" {
		t.Errorf("model=%q", nc.Metadata["model"])
	}
}

func TestExtractRequest_PromptShape(t *testing.T) {
	body := []byte(`{"prompt":"explain quantum entanglement","conversation_id":"c-1"}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/v2/chat")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "explain quantum entanglement" {
		t.Errorf("Segments=%v", nc.Segments)
	}
	if nc.Metadata["conversation_id"] != "c-1" {
		t.Errorf("conversation_id=%q", nc.Metadata["conversation_id"])
	}
}

func TestExtractRequest_ToolCalls(t *testing.T) {
	body := []byte(`{
		"messages":[
			{"role":"assistant","content":null,"tool_calls":[
				{"id":"c1","type":"function","function":{"name":"search_web","arguments":"{\"q\":\"x\"}"}}
			]}
		]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/chat")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.ToolCallSegments) != 1 || !strings.Contains(nc.ToolCallSegments[0], `"search_web"`) {
		t.Errorf("ToolCallSegments=%v", nc.ToolCallSegments)
	}
}

func TestExtractRequest_BinaryBody(t *testing.T) {
	body := []byte{0x00, 0x42, 0x00}
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), body, "/api/x")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v", err)
	}
}

func TestExtractRequest_Empty(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), nil, "/api/x")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v", err)
	}
}

func TestExtractRequest_Malformed(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), []byte(`{not valid`), "/api/x")
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("err=%v", err)
	}
}

func TestExtractRequest_JSONNoKnownFields(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), []byte(`{"foo":"bar"}`), "/api/x")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v", err)
	}
}

func TestExtractStreamChunk_OpenAICompat(t *testing.T) {
	chunk := []byte(`{"choices":[{"delta":{"content":"Hello"}}]}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/api/chat/stream")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "Hello" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestExtractStreamChunk_PlainText(t *testing.T) {
	chunk := []byte(`{"text":"streaming text"}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/api/chat/stream")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "streaming text" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestExtractStreamChunk_ResultMessage(t *testing.T) {
	chunk := []byte(`{"result":{"message":"answer chunk"}}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/api/chat/stream")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "answer chunk" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestExtractStreamChunk_BinaryFrame(t *testing.T) {
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), []byte{0x00, 0x42}, "/api/x")
	if err != nil {
		t.Errorf("err=%v want nil", err)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestExtractResponse_ErrorEnvelope(t *testing.T) {
	body := []byte(`{"error":{"message":"rate limited"}}`)
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), body, "/api/chat")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "rate limited" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestExtractResponse_MessageString(t *testing.T) {
	body := []byte(`{"message":"plain"}`)
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), body, "/api/chat")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "plain" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestDetectRequestMeta(t *testing.T) {
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost, "https://grok.com/api/chat", nil)
	meta := a.DetectRequestMeta(r, []byte(`{"model":"grok-3"}`))
	if meta.Provider != "grok-web" {
		t.Errorf("Provider=%q", meta.Provider)
	}
	if meta.Model != "grok-3" {
		t.Errorf("Model=%q", meta.Model)
	}
}

func TestDetectResponseUsage_NonLLM(t *testing.T) {
	a := &Adapter{}
	if a.DetectResponseUsage(nil, []byte(`{}`)).Status != traffic.UsageStatusNonLLM {
		t.Errorf("want non_llm")
	}
}

func TestRewrite_Unsupported(t *testing.T) {
	a := &Adapter{}
	body := []byte(`{}`)
	if _, _, err := a.RewriteRequestBody(context.Background(), body, "/x", traffic.NormalizedContent{}); !errors.Is(err, traffic.ErrRewriteUnsupported) {
		t.Errorf("Request rewrite err=%v", err)
	}
	if _, _, err := a.RewriteResponseBody(context.Background(), body, "/x", traffic.NormalizedContent{}); !errors.Is(err, traffic.ErrRewriteUnsupported) {
		t.Errorf("Response rewrite err=%v", err)
	}
}

// TestExtractRequest_ContentArrayParts pins the OpenAI-vision-style
// content-as-array shape (parts with type=text flattened). Also covers
// per-message top-level "text" field appended after content parts.
func TestExtractRequest_ContentArrayParts(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":[
		{"type":"text","text":"first"},
		{"type":"image_url","image_url":{"url":"https://x/y.png"}},
		{"type":"text","text":"second"}
	],"text":"third"}],"model":"grok-3"}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/chat")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	want := []string{"first", "second", "third"}
	if len(nc.Segments) != len(want) {
		t.Fatalf("Segments=%v want %v", nc.Segments, want)
	}
	for i := range want {
		if nc.Segments[i] != want[i] {
			t.Errorf("Segments[%d]=%q want %q", i, nc.Segments[i], want[i])
		}
	}
}

// TestExtractResponse_Empty pins benign empty body.
func TestExtractResponse_Empty(t *testing.T) {
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), nil, "/api/x")
	if err != nil {
		t.Errorf("err=%v want nil", err)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

// TestExtractResponse_BinaryPayload pins non-JSON-prefix.
func TestExtractResponse_BinaryPayload(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), []byte{0x00, 0xff, 'x'}, "/api/x")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

// TestExtractResponse_Malformed pins JSON-prefix invalid JSON.
func TestExtractResponse_Malformed(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), []byte(`{"oops":`), "/api/x")
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("err=%v want ErrMalformed", err)
	}
}

// TestExtractResponse_UnknownJSON pins fallthrough for valid JSON
// without recognised envelope keys.
func TestExtractResponse_UnknownJSON(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), []byte(`{"foo":"bar"}`), "/api/x")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

// TestExtractStreamChunk_Empty pins empty chunk.
func TestExtractStreamChunk_Empty(t *testing.T) {
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), nil, "/api/stream")
	if err != nil {
		t.Errorf("err=%v", err)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

// TestExtractStreamChunk_Malformed pins JSON-prefix invalid JSON.
func TestExtractStreamChunk_Malformed(t *testing.T) {
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), []byte(`{"oops":`), "/api/x")
	if err != nil {
		t.Errorf("err=%v want nil", err)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

// TestExtractStreamChunk_ContentField pins the top-level `content` field.
func TestExtractStreamChunk_ContentField(t *testing.T) {
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), []byte(`{"content":"piece"}`), "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "piece" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

// TestExtractStreamChunk_DeltaToolCalls pins streamed tool_calls.
func TestExtractStreamChunk_DeltaToolCalls(t *testing.T) {
	chunk := []byte(`{"choices":[{"delta":{"tool_calls":[
		{"index":0,"id":"call_a","function":{"name":"search_web"}}
	]}}]}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/api/stream")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.ToolCallSegments) != 1 || !strings.Contains(nc.ToolCallSegments[0], "search_web") {
		t.Errorf("ToolCallSegments=%v", nc.ToolCallSegments)
	}
}

// TestDetectRequestMeta_InvalidJSON pins early-return when body not JSON.
func TestDetectRequestMeta_InvalidJSON(t *testing.T) {
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost, "https://grok.com/api/chat", nil)
	meta := a.DetectRequestMeta(r, []byte(`not json`))
	if meta.Provider != "grok-web" {
		t.Errorf("Provider=%q", meta.Provider)
	}
	if meta.Model != "" {
		t.Errorf("Model=%q want empty", meta.Model)
	}
}

// Normalize (Tier-1 spec dispatch)

func TestNormalize_RequestChatShape(t *testing.T) {
	body := []byte(`{
		"model": "grok-3",
		"messages": [
			{"role": "system", "content": "You are xAI's Grok."},
			{"role": "user", "content": "why is the sky blue"}
		]
	}`)
	a := &Adapter{}
	payload, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType:  "grok-web",
		Direction:    normalize.DirectionRequest,
		ContentType:  "application/json",
		EndpointPath: "/api/chat",
	})
	if err != nil {
		t.Fatalf("Normalize err: %v", err)
	}
	if payload.Kind != normalize.KindAIChat {
		t.Errorf("Kind=%v want ai-chat", payload.Kind)
	}
	if payload.DetectedSpec != "grok-web" {
		t.Errorf("DetectedSpec=%q want grok-web", payload.DetectedSpec)
	}
	if payload.Model != "grok-3" {
		t.Errorf("Model=%q", payload.Model)
	}
}

func TestNormalize_ResponseNonStream(t *testing.T) {
	body := []byte(`{
		"id": "chatcmpl-g",
		"object": "chat.completion",
		"model": "grok-3",
		"choices": [
			{"index":0,"message":{"role":"assistant","content":"because rayleigh scattering"},"finish_reason":"stop"}
		],
		"usage": {"prompt_tokens": 5, "completion_tokens": 4, "total_tokens": 9}
	}`)
	a := &Adapter{}
	payload, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType: "grok-web",
		Direction:   normalize.DirectionResponse,
		ContentType: "application/json",
	})
	if err != nil {
		t.Fatalf("Normalize err: %v", err)
	}
	if payload.DetectedSpec != "grok-web" {
		t.Errorf("DetectedSpec=%q", payload.DetectedSpec)
	}
}

func TestNormalize_UnrecognisedShape_FallsThrough(t *testing.T) {
	a := &Adapter{}
	_, err := a.Normalize(context.Background(), []byte(`{"foo":"bar"}`), normalize.Meta{
		AdapterType: "grok-web",
		Direction:   normalize.DirectionRequest,
		ContentType: "application/json",
	})
	if !errors.Is(err, normalize.ErrUnsupported) {
		t.Errorf("err=%v want ErrUnsupported", err)
	}
}

// Internal helpers

func TestLooksLikeJSON(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want bool
	}{
		{"empty", []byte(``), false},
		{"only-whitespace", []byte("  \t\n\r"), false},
		{"object", []byte(`{"a":1}`), true},
		{"array", []byte(`[1,2,3]`), true},
		{"leading-whitespace-object", []byte("  \n\t {\"a\":1}"), true},
		{"text-prefix", []byte(`hello`), false},
		{"control-byte-prefix", []byte{0x00, '{'}, false},
	}
	for _, c := range cases {
		if got := looksLikeJSON(c.in); got != c.want {
			t.Errorf("%s: looksLikeJSON(%q)=%v want %v", c.name, c.in, got, c.want)
		}
	}
}

package mistralweb

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
	if a.ID() != "mistral-web" {
		t.Errorf("ID=%q", a.ID())
	}
}

func TestExtractRequest_OpenAICompatMessages(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"explain rust ownership"}],"model":"mistral-large"}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/chat")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "explain rust ownership" {
		t.Errorf("Segments=%v", nc.Segments)
	}
	if nc.Metadata["model"] != "mistral-large" {
		t.Errorf("model=%q", nc.Metadata["model"])
	}
}

func TestExtractRequest_PromptShape(t *testing.T) {
	body := []byte(`{"prompt":"hi","chat_uuid":"u-1"}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/chat")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "hi" {
		t.Errorf("Segments=%v", nc.Segments)
	}
	if nc.Metadata["chat_uuid"] != "u-1" {
		t.Errorf("chat_uuid=%q", nc.Metadata["chat_uuid"])
	}
}

func TestExtractRequest_ToolCalls(t *testing.T) {
	body := []byte(`{"messages":[{"role":"assistant","content":null,"tool_calls":[
		{"id":"c1","type":"function","function":{"name":"search","arguments":"{}"}}
	]}]}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/chat")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.ToolCallSegments) != 1 || !strings.Contains(nc.ToolCallSegments[0], `"search"`) {
		t.Errorf("ToolCallSegments=%v", nc.ToolCallSegments)
	}
}

func TestExtractRequest_BinaryBody(t *testing.T) {
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), []byte{0x00, 0x42}, "/api/x")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v", err)
	}
	if _, ok := nc.Extra["binary_preview"]; !ok {
		t.Errorf("Extra missing binary_preview")
	}
}

func TestExtractRequest_Extra(t *testing.T) {
	body := []byte(`{"prompt":"hi","x_mistral_field":"sensitive"}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if _, ok := nc.Extra["x_mistral_field"]; !ok {
		t.Errorf("Extra=%v missing x_mistral_field", nc.Extra)
	}
}

func TestExtractStreamChunk_OpenAICompat(t *testing.T) {
	chunk := []byte(`{"choices":[{"delta":{"content":"hello"}}]}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/api/stream")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "hello" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestExtractStreamChunk_PlainText(t *testing.T) {
	chunk := []byte(`{"text":"chunk"}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/api/stream")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "chunk" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestExtractResponse_ErrorEnvelope(t *testing.T) {
	body := []byte(`{"error":{"message":"forbidden"}}`)
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), body, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "forbidden" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestDetectRequestMeta(t *testing.T) {
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost, "https://chat.mistral.ai/api/chat", nil)
	meta := a.DetectRequestMeta(r, []byte(`{"model":"mistral-large"}`))
	if meta.Provider != "mistral-web" {
		t.Errorf("Provider=%q", meta.Provider)
	}
	if meta.Model != "mistral-large" {
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

// TestAdapter_Configure covers both nil and non-nil config paths.
func TestAdapter_Configure(t *testing.T) {
	a := &Adapter{}
	if err := a.Configure(nil); err != nil {
		t.Errorf("Configure(nil)=%v", err)
	}
	if err := a.Configure(map[string]any{"foo": "bar"}); err != nil {
		t.Errorf("Configure(map)=%v", err)
	}
}

// TestExtractRequest_Empty pins that an empty body returns ErrUnknownSchema.
func TestExtractRequest_Empty(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), nil, "/api/x")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

// TestExtractRequest_Malformed pins JSON-prefix body that fails parse.
func TestExtractRequest_Malformed(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), []byte(`{not valid`), "/api/x")
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("err=%v want ErrMalformed", err)
	}
}

// TestExtractRequest_JSONNoKnownFields pins valid JSON with no extractable
// content / tool calls — falls through to ErrUnknownSchema with the
// unknown fields collected into Extra.
func TestExtractRequest_JSONNoKnownFields(t *testing.T) {
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), []byte(`{"foo":"bar"}`), "/api/x")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
	if _, ok := nc.Extra["foo"]; !ok {
		t.Errorf("Extra=%v missing foo", nc.Extra)
	}
}

// TestExtractRequest_ContentArrayParts pins the OpenAI-vision-style
// content-as-array shape: each {"type":"text","text":"..."} part is
// flattened into Segments.
func TestExtractRequest_ContentArrayParts(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":[
		{"type":"text","text":"part one"},
		{"type":"image_url","image_url":{"url":"https://x/y.png"}},
		{"type":"text","text":"part two"}
	]}],"model":"mistral-large","tools":[{"name":"search"}]}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/chat")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	want := []string{"part one", "part two"}
	if len(nc.Segments) != len(want) {
		t.Fatalf("Segments=%v want %v", nc.Segments, want)
	}
	for i := range want {
		if nc.Segments[i] != want[i] {
			t.Errorf("Segments[%d]=%q want %q", i, nc.Segments[i], want[i])
		}
	}
	if nc.Metadata["tools"] == "" || !strings.Contains(nc.Metadata["tools"], "search") {
		t.Errorf("tools metadata=%q want raw array containing search", nc.Metadata["tools"])
	}
}

// TestExtractResponse_Empty pins benign empty body — no error, no segments.
func TestExtractResponse_Empty(t *testing.T) {
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), nil, "/api/x")
	if err != nil {
		t.Errorf("err=%v want nil", err)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("Segments=%v want empty", nc.Segments)
	}
}

// TestExtractResponse_BinaryPayload pins that a non-JSON-prefix body
// returns ErrUnknownSchema.
func TestExtractResponse_BinaryPayload(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), []byte{0x00, 0xff, 'x'}, "/api/x")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

// TestExtractResponse_Malformed pins JSON-prefix that fails gjson parse.
func TestExtractResponse_Malformed(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), []byte(`{"oops":`), "/api/x")
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("err=%v want ErrMalformed", err)
	}
}

// TestExtractResponse_MessageString pins the bare `message` envelope.
func TestExtractResponse_MessageString(t *testing.T) {
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), []byte(`{"message":"plain msg"}`), "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "plain msg" {
		t.Errorf("Segments=%v", nc.Segments)
	}
	if nc.Metadata["error"] != "true" {
		t.Errorf("error metadata=%q", nc.Metadata["error"])
	}
}

// TestExtractResponse_DetailString pins the FastAPI-style `detail` envelope.
func TestExtractResponse_DetailString(t *testing.T) {
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), []byte(`{"detail":"forbidden"}`), "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "forbidden" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

// TestExtractResponse_UnknownJSON pins fallthrough for valid JSON
// with no recognised envelope keys.
func TestExtractResponse_UnknownJSON(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), []byte(`{"foo":"bar"}`), "/api/x")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

// TestExtractStreamChunk_Empty pins empty chunk returns nil/no-error.
func TestExtractStreamChunk_Empty(t *testing.T) {
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), nil, "/api/stream")
	if err != nil {
		t.Errorf("err=%v want nil", err)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

// TestExtractStreamChunk_BinaryFrame pins non-JSON binary frame — no
// error, no segments (audit pipeline tolerates noise here).
func TestExtractStreamChunk_BinaryFrame(t *testing.T) {
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), []byte{0x00, 0x42}, "/api/x")
	if err != nil {
		t.Errorf("err=%v", err)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

// TestExtractStreamChunk_Malformed pins JSON-prefix but invalid JSON.
func TestExtractStreamChunk_Malformed(t *testing.T) {
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), []byte(`{"oops":`), "/api/x")
	if err != nil {
		t.Errorf("err=%v want nil (lenient)", err)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

// TestExtractStreamChunk_ContentField pins the top-level content shape.
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
		{"index":0,"id":"call_a","function":{"name":"do_thing"}}
	]}}]}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/api/stream")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.ToolCallSegments) != 1 || !strings.Contains(nc.ToolCallSegments[0], "do_thing") {
		t.Errorf("ToolCallSegments=%v", nc.ToolCallSegments)
	}
}

// TestDetectRequestMeta_InvalidJSON pins the early-return path when
// body is not valid JSON: Provider still stamped, Model empty.
func TestDetectRequestMeta_InvalidJSON(t *testing.T) {
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost, "https://chat.mistral.ai/api/chat", nil)
	meta := a.DetectRequestMeta(r, []byte(`not json`))
	if meta.Provider != "mistral-web" {
		t.Errorf("Provider=%q", meta.Provider)
	}
	if meta.Model != "" {
		t.Errorf("Model=%q want empty", meta.Model)
	}
}

// Normalize (Tier-1 spec dispatch)

func TestNormalize_RequestChatShape(t *testing.T) {
	body := []byte(`{
		"model": "mistral-large",
		"messages": [
			{"role": "system", "content": "You are an assistant."},
			{"role": "user", "content": "explain rust ownership"}
		]
	}`)
	a := &Adapter{}
	payload, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType:  "mistral-web",
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
	if payload.DetectedSpec != "mistral-web" {
		t.Errorf("DetectedSpec=%q want mistral-web", payload.DetectedSpec)
	}
	if payload.Model != "mistral-large" {
		t.Errorf("Model=%q want mistral-large", payload.Model)
	}
	if len(payload.Messages) < 1 {
		t.Fatalf("Messages empty: %+v", payload.Messages)
	}
}

func TestNormalize_ResponseNonStream(t *testing.T) {
	body := []byte(`{
		"id": "chatcmpl-m",
		"object": "chat.completion",
		"model": "mistral-large",
		"choices": [
			{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}
		],
		"usage": {"prompt_tokens": 4, "completion_tokens": 2, "total_tokens": 6}
	}`)
	a := &Adapter{}
	payload, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType: "mistral-web",
		Direction:   normalize.DirectionResponse,
		ContentType: "application/json",
	})
	if err != nil {
		t.Fatalf("Normalize err: %v", err)
	}
	if payload.Kind != normalize.KindAIChat {
		t.Errorf("Kind=%v want ai-chat", payload.Kind)
	}
	if payload.DetectedSpec != "mistral-web" {
		t.Errorf("DetectedSpec=%q want mistral-web", payload.DetectedSpec)
	}
}

func TestNormalize_UnrecognisedShape_FallsThrough(t *testing.T) {
	body := []byte(`{"foo":"bar","baz":42}`)
	a := &Adapter{}
	_, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType: "mistral-web",
		Direction:   normalize.DirectionRequest,
		ContentType: "application/json",
	})
	if !errors.Is(err, normalize.ErrUnsupported) {
		t.Errorf("err=%v want ErrUnsupported", err)
	}
}

// Internal helpers — looksLikeJSON + preview

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
		{"leading-whitespace-array", []byte("\r\n[1]"), true},
		{"text-prefix", []byte(`hello`), false},
		{"number-prefix", []byte(`42`), false},
		{"control-byte-prefix", []byte{0x00, '{'}, false},
	}
	for _, c := range cases {
		if got := looksLikeJSON(c.in); got != c.want {
			t.Errorf("%s: looksLikeJSON(%q)=%v want %v", c.name, c.in, got, c.want)
		}
	}
}

func TestPreview(t *testing.T) {
	t.Run("short-printable-passthrough", func(t *testing.T) {
		if got := preview([]byte("hello world")); got != "hello world" {
			t.Errorf("got=%q", got)
		}
	})
	t.Run("preserves-newline-and-tab", func(t *testing.T) {
		if got := preview([]byte("a\nb\tc")); got != "a\nb\tc" {
			t.Errorf("got=%q", got)
		}
	})
	t.Run("scrubs-control-bytes", func(t *testing.T) {
		if got := preview([]byte{'a', 0x07, 'b', 0x0d, 'c', 0x1b, 'd'}); got != "a.b.c.d" {
			t.Errorf("got=%q", got)
		}
	})
	t.Run("scrubs-high-bytes", func(t *testing.T) {
		if got := preview([]byte{'a', 0x7f, 'b', 0x80, 'c', 0xff, 'd'}); got != "a.b.c.d" {
			t.Errorf("got=%q", got)
		}
	})
	t.Run("truncates-over-256-bytes", func(t *testing.T) {
		body := make([]byte, 300)
		for i := range body {
			body[i] = 'A'
		}
		if got := preview(body); len(got) != 256 {
			t.Errorf("len=%d want 256", len(got))
		}
	})
}

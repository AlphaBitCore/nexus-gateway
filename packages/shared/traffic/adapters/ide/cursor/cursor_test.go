package cursor

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

// Cursor's wire format is heavily proprietary; the adapter is
// defensive (see package doc). Tests cover JSON shapes the adapter
// recognises plus the binary-preview safety net for protobuf bodies.

func TestAdapter_ID(t *testing.T) {
	a := &Adapter{}
	if a.ID() != "cursor" {
		t.Errorf("ID=%q", a.ID())
	}
}

func TestAdapter_Configure(t *testing.T) {
	a := &Adapter{}
	if err := a.Configure(nil); err != nil {
		t.Errorf("Configure(nil)=%v", err)
	}
}

// JSON request shapes

func TestExtractRequest_OpenAICompatMessages(t *testing.T) {
	body := []byte(`{
		"model":"gpt-4o",
		"messages":[
			{"role":"user","content":"refactor this function"}
		]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/aiserver.v1.AiService/StreamChat")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "refactor this function" {
		t.Errorf("Segments=%v", nc.Segments)
	}
	if nc.Metadata["model"] != "gpt-4o" {
		t.Errorf("model=%q", nc.Metadata["model"])
	}
}

func TestExtractRequest_OpenAICompatToolCalls(t *testing.T) {
	body := []byte(`{
		"messages":[
			{"role":"user","content":"send email"},
			{"role":"assistant","content":null,"tool_calls":[
				{"id":"c1","type":"function","function":{"name":"send_email","arguments":"{}"}}
			]}
		]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/chat")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.ToolCallSegments) != 1 || !strings.Contains(nc.ToolCallSegments[0], `"send_email"`) {
		t.Errorf("ToolCallSegments=%v", nc.ToolCallSegments)
	}
}

func TestExtractRequest_PromptField(t *testing.T) {
	body := []byte(`{"prompt":"explain this code","conversation_id":"conv-1"}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/inline-edit")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "explain this code" {
		t.Errorf("Segments=%v", nc.Segments)
	}
	if nc.Metadata["conversation_id"] != "conv-1" {
		t.Errorf("conversation_id=%q", nc.Metadata["conversation_id"])
	}
}

func TestExtractRequest_TextField(t *testing.T) {
	body := []byte(`{"text":"a long enough text to count as a real prompt for the adapter"}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/composer")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestExtractRequest_AuthorTextShape(t *testing.T) {
	body := []byte(`{"messages":[{"author":"user","text":"hello cursor"}]}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/chat")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "hello cursor" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestExtractRequest_Extra(t *testing.T) {
	body := []byte(`{"prompt":"hi","x_cursor_field":{"sensitive":"trace"}}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/chat")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if x, ok := nc.Extra["x_cursor_field"]; !ok || !strings.Contains(x, "sensitive") {
		t.Errorf("Extra=%v missing x_cursor_field", nc.Extra)
	}
}

// Defensive paths

func TestExtractRequest_BinaryProtobufBody(t *testing.T) {
	// Simulated protobuf-style body: starts with a binary length prefix.
	body := []byte{0x00, 0x00, 0x00, 0x00, 0x05, 0x68, 0x65, 0x6c, 0x6c, 0x6f}
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/aiserver.v1.AiService/StreamChat")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
	// Binary preview must be captured for offline analysis.
	if _, ok := nc.Extra["binary_preview"]; !ok {
		t.Errorf("Extra=%v missing binary_preview safety net", nc.Extra)
	}
}

func TestExtractRequest_JSONWithoutKnownFields(t *testing.T) {
	body := []byte(`{"foo":"bar","baz":42}`)
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), body, "/api/x")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

func TestExtractRequest_EmptyBody(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), nil, "/api/x")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

func TestExtractRequest_MalformedJSON(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), []byte(`{not valid`), "/api/x")
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("err=%v want ErrMalformed", err)
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
	chunk := []byte(`{"text":"streamed delta"}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/api/chat/stream")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "streamed delta" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestExtractStreamChunk_BinaryFrame(t *testing.T) {
	// gRPC-Web framed binary chunk: do not error.
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), []byte{0x00, 0x42, 0x00}, "/api/chat/stream")
	if err != nil {
		t.Errorf("err=%v want nil for binary chunk (fail-open)", err)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestExtractStreamChunk_EmptyChunk(t *testing.T) {
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), nil, "/api/chat/stream")
	if err != nil {
		t.Errorf("err=%v", err)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestExtractResponse_ErrorEnvelope(t *testing.T) {
	body := []byte(`{"error":{"code":"PERMISSION_DENIED","message":"quota exceeded"}}`)
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), body, "/api/chat")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "quota exceeded" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestExtractResponse_OpenAICompatHappyPath(t *testing.T) {
	body := []byte(`{"choices":[{"message":{"content":"hi"}}]}`)
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), body, "/api/chat")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "hi" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestExtractResponse_ProtobufSkipped(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), []byte{0x00, 0x00, 0x00, 0x05}, "/api/chat")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

// DetectRequestMeta + DetectResponseUsage + Rewrite contracts

func TestDetectRequestMeta_BearerToken(t *testing.T) {
	body := []byte(`{"model":"claude-3-5-sonnet","prompt":"hi"}`)
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost, "https://api2.cursor.sh/aiserver.v1.AiService/StreamChat", nil)
	r.Header.Set("Authorization", "Bearer cursor_session_token_xyz")
	meta := a.DetectRequestMeta(r, body)
	if meta.Provider != "cursor" {
		t.Errorf("Provider=%q", meta.Provider)
	}
	if meta.ApiKeyClass != "cursor-bearer" {
		t.Errorf("ApiKeyClass=%q", meta.ApiKeyClass)
	}
	if meta.ApiKeyFingerprint == "" {
		t.Errorf("ApiKeyFingerprint should be set")
	}
	if meta.Model != "claude-3-5-sonnet" {
		t.Errorf("Model=%q", meta.Model)
	}
}

func TestDetectRequestMeta_NoAuth(t *testing.T) {
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost, "https://api2.cursor.sh/", nil)
	meta := a.DetectRequestMeta(r, []byte(`{}`))
	if meta.Provider != "cursor" {
		t.Errorf("Provider=%q", meta.Provider)
	}
	if meta.ApiKeyClass != "" {
		t.Errorf("ApiKeyClass=%q want empty", meta.ApiKeyClass)
	}
}

func TestDetectResponseUsage_NonLLM(t *testing.T) {
	a := &Adapter{}
	if a.DetectResponseUsage(nil, []byte(`{}`)).Status != traffic.UsageStatusNonLLM {
		t.Errorf("want non_llm")
	}
}

func TestRewriteRequestBody_Unsupported(t *testing.T) {
	a := &Adapter{}
	body := []byte(`{}`)
	out, n, err := a.RewriteRequestBody(context.Background(), body, "/api/x", traffic.NormalizedContent{})
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
	out, n, err := a.RewriteResponseBody(context.Background(), body, "/api/x", traffic.NormalizedContent{})
	if !errors.Is(err, traffic.ErrRewriteUnsupported) {
		t.Errorf("err=%v want ErrRewriteUnsupported", err)
	}
	if n != 0 || string(out) != string(body) {
		t.Errorf("body modified")
	}
}

// looksLikeJSON / preview helpers

func TestLooksLikeJSON(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{`{"a":1}`, true},
		{`[1,2,3]`, true},
		{"  \n\t{", true},
		{`abc`, false},
		{string([]byte{0x00, 0x01}), false},
		{``, false},
	}
	for _, c := range cases {
		got := looksLikeJSON([]byte(c.in))
		if got != c.want {
			t.Errorf("looksLikeJSON(%q)=%v want %v", c.in, got, c.want)
		}
	}
}

func TestPreview_Truncates(t *testing.T) {
	body := bytesRepeat('a', 1024)
	p := preview(body)
	if len(p) > 256 {
		t.Errorf("preview length=%d > 256", len(p))
	}
}

func TestPreview_StripsControlChars(t *testing.T) {
	body := []byte{'a', 0x00, 'b', 0x01, 'c'}
	p := preview(body)
	for _, ch := range p {
		if ch < 0x20 && ch != '\n' && ch != '\t' {
			t.Errorf("preview contains control char: %q", p)
			break
		}
	}
}

func bytesRepeat(b byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return out
}

// TestExtractStreamChunk_PathGatesProtobuf pins that a raw protobuf frame is
// decoded as StreamChatResponse ONLY on a known chat path. The same frame on
// /agent.v1.AgentService/Run (different frame schema) must yield nothing rather
// than the garbage field-1 byte — this is the "j" regression.
func TestExtractStreamChunk_PathGatesProtobuf(t *testing.T) {
	a := &Adapter{}
	// protobuf: field 1 (bytes) = "j"  -> [0x0a, 0x01, 'j']
	frame := []byte{0x0a, 0x01, 'j'}

	run, err := a.ExtractStreamChunk(context.Background(), frame, "/agent.v1.AgentService/Run")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(run.Segments) != 0 {
		t.Fatalf("Run path leaked garbage segments %q; want none (frame schema differs)", run.Segments)
	}

	chat, err := a.ExtractStreamChunk(context.Background(), frame, "/aiserver.v1.AiService/StreamChat")
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if len(chat.Segments) != 1 || chat.Segments[0] != "j" {
		t.Fatalf("chat path segments = %q; want [j] (StreamChatResponse field 1)", chat.Segments)
	}
}

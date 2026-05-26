package tabnine

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// Configure (was 0% — no test asserted the no-op contract).

func TestAdapter_Configure(t *testing.T) {
	a := &Adapter{}
	if err := a.Configure(nil); err != nil {
		t.Errorf("Configure(nil)=%v", err)
	}
	if err := a.Configure(map[string]any{"foo": "bar"}); err != nil {
		t.Errorf("Configure(map)=%v", err)
	}
}

// ExtractRequest extra branches (empty / malformed / extra metadata / array
// content / author-text shape).

func TestExtractRequest_EmptyBody(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), nil, "/api/x")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Fatalf("err=%v want ErrUnknownSchema", err)
	}
}

func TestExtractRequest_MalformedJSON(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), []byte(`{bad`), "/api/x")
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Fatalf("err=%v want ErrMalformed", err)
	}
}

func TestExtractRequest_JSONWithoutKnownFields(t *testing.T) {
	body := []byte(`{"foo":"bar","baz":42}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/x")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Fatalf("err=%v want ErrUnknownSchema", err)
	}
	// Extra surfaces unknown top-level fields for offline analysis.
	if _, ok := nc.Extra["foo"]; !ok {
		t.Errorf("Extra=%v missing foo", nc.Extra)
	}
}

func TestExtractRequest_ContentArrayParts(t *testing.T) {
	body := []byte(`{"messages":[
		{"role":"user","content":[
			{"type":"text","text":"part A"},
			{"type":"image","image":{}},
			{"type":"text","text":"part B"}
		]}
	]}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/chat")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 2 || nc.Segments[0] != "part A" || nc.Segments[1] != "part B" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestExtractRequest_AuthorTextShape(t *testing.T) {
	body := []byte(`{"messages":[{"author":"user","text":"copilot-style"}]}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "copilot-style" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestExtractRequest_ConversationIDMetadata(t *testing.T) {
	body := []byte(`{"prompt":"hi","conversation_id":"c-99","model":"tabnine-protected"}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if nc.Metadata["conversation_id"] != "c-99" {
		t.Errorf("conversation_id=%q", nc.Metadata["conversation_id"])
	}
	if nc.Metadata["model"] != "tabnine-protected" {
		t.Errorf("model=%q", nc.Metadata["model"])
	}
}

func TestExtractRequest_AllPromptKeys(t *testing.T) {
	// All four top-level prompt-style fields populated.
	body := []byte(`{"prompt":"p","query":"q","text":"t","input":"i"}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 4 {
		t.Errorf("Segments=%v want 4", nc.Segments)
	}
}

// ExtractResponse — all branches (empty, binary, malformed JSON, top-level
// message error, choices happy path + tool_calls, unknown JSON).

func TestExtractResponse_EmptyBody(t *testing.T) {
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), nil, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestExtractResponse_BinaryBody(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), []byte{0x00, 0x42}, "/api/x")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Fatalf("err=%v want ErrUnknownSchema", err)
	}
}

func TestExtractResponse_MalformedJSON(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), []byte(`{bad`), "/api/x")
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Fatalf("err=%v want ErrMalformed", err)
	}
}

func TestExtractResponse_TopLevelMessageError(t *testing.T) {
	body := []byte(`{"message":"rate limited"}`)
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), body, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "rate limited" {
		t.Errorf("Segments=%v", nc.Segments)
	}
	if nc.Metadata["error"] != "true" {
		t.Errorf("missing error meta")
	}
}

func TestExtractResponse_ChoicesHappyPath(t *testing.T) {
	body := []byte(`{"choices":[{"message":{"content":"hi"}}]}`)
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), body, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "hi" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestExtractResponse_ChoicesWithToolCalls(t *testing.T) {
	body := []byte(`{"choices":[{"message":{"content":"call","tool_calls":[
		{"id":"c1","type":"function","function":{"name":"do","arguments":"{}"}}
	]}}]}`)
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), body, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.ToolCallSegments) != 1 || !strings.Contains(nc.ToolCallSegments[0], `"do"`) {
		t.Errorf("ToolCallSegments=%v", nc.ToolCallSegments)
	}
}

func TestExtractResponse_UnknownJSON(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), []byte(`{"foo":"bar"}`), "/api/x")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Fatalf("err=%v want ErrUnknownSchema", err)
	}
}

// ExtractStreamChunk — empty / binary / malformed JSON / plain-text /
// content / tool_calls branches.

func TestExtractStreamChunk_EmptyChunk(t *testing.T) {
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), nil, "/api/stream")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestExtractStreamChunk_BinaryChunk(t *testing.T) {
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), []byte{0x00, 0x42}, "/api/stream")
	if err != nil {
		t.Fatalf("err=%v want nil (fail-open)", err)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestExtractStreamChunk_MalformedJSON(t *testing.T) {
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), []byte(`{bad`), "/api/stream")
	if err != nil {
		t.Fatalf("err=%v want nil (fail-open)", err)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestExtractStreamChunk_PlainTextAndContent(t *testing.T) {
	chunk := []byte(`{"text":"a","content":"b"}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/api/stream")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 2 || nc.Segments[0] != "a" || nc.Segments[1] != "b" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestExtractStreamChunk_DeltaToolCalls(t *testing.T) {
	chunk := []byte(`{"choices":[{"delta":{"tool_calls":[
		{"id":"c1","type":"function","function":{"name":"x","arguments":"{}"}}
	]}}]}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/api/stream")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.ToolCallSegments) != 1 || !strings.Contains(nc.ToolCallSegments[0], `"x"`) {
		t.Errorf("ToolCallSegments=%v", nc.ToolCallSegments)
	}
}

// DetectRequestMeta — no-auth path.

func TestDetectRequestMeta_NoAuth(t *testing.T) {
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost, "https://api.tabnine.com/", nil)
	meta := a.DetectRequestMeta(r, []byte(`{}`))
	if meta.Provider != "tabnine" {
		t.Errorf("Provider=%q", meta.Provider)
	}
	if meta.ApiKeyClass != "" {
		t.Errorf("ApiKeyClass=%q want empty", meta.ApiKeyClass)
	}
}

func TestDetectRequestMeta_NilRequest(t *testing.T) {
	a := &Adapter{}
	meta := a.DetectRequestMeta(nil, []byte(`{"model":"tabnine-x"}`))
	if meta.Model != "tabnine-x" {
		t.Errorf("Model=%q", meta.Model)
	}
	if meta.ApiKeyClass != "" {
		t.Errorf("ApiKeyClass=%q", meta.ApiKeyClass)
	}
}

func TestDetectRequestMeta_EmptyBearer(t *testing.T) {
	// Authorization header with just "Bearer " (no token) — adapter
	// must not set ApiKeyClass.
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost, "https://api.tabnine.com/", nil)
	r.Header.Set("Authorization", "Bearer  ") // trimmed → empty
	meta := a.DetectRequestMeta(r, []byte(`{}`))
	if meta.ApiKeyClass != "" {
		t.Errorf("ApiKeyClass=%q want empty for trimmed-empty token", meta.ApiKeyClass)
	}
}

// looksLikeJSON / preview — fill the small gaps.

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
		{"   ", false}, // whitespace-only → loop runs but never returns inside; falls through to false
	}
	for _, c := range cases {
		got := looksLikeJSON([]byte(c.in))
		if got != c.want {
			t.Errorf("looksLikeJSON(%q)=%v want %v", c.in, got, c.want)
		}
	}
}

func TestPreview_Truncates(t *testing.T) {
	body := make([]byte, 1024)
	for i := range body {
		body[i] = 'a'
	}
	p := preview(body)
	if len(p) > 256 {
		t.Errorf("preview length=%d > 256", len(p))
	}
}

func TestPreview_StripsControlChars(t *testing.T) {
	body := []byte{'a', 0x00, 'b', 0x01, 'c'}
	p := preview(body)
	if p != "a.b.c" {
		t.Errorf("preview=%q want a.b.c", p)
	}
}

func TestPreview_StripsHighBytes(t *testing.T) {
	body := []byte{'a', 0xff, 'b', 0x80, 'c'}
	p := preview(body)
	if p != "a.b.c" {
		t.Errorf("preview=%q want a.b.c", p)
	}
}

func TestPreview_PreservesTabsAndNewlines(t *testing.T) {
	body := []byte{'a', '\t', 'b', '\n', 'c'}
	p := preview(body)
	if p != "a\tb\nc" {
		t.Errorf("preview=%q want a\\tb\\nc", p)
	}
}

// Normalize — Tier 1 contract. Tabnine delegates to extract.NormalizeForAdapter
// with openai-chat spec hints. We exercise (a) an OpenAI-chat JSON request
// that the probe should recognise and (b) an unrelated body that should
// fall through to ErrUnsupported per the doc-comment contract.

func TestNormalize_OpenAIChatRequest(t *testing.T) {
	body := []byte(`{"model":"tabnine-protected","messages":[
		{"role":"user","content":"complete this function"}
	]}`)
	a := &Adapter{}
	payload, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType: "tabnine",
		Direction:   normalize.DirectionRequest,
		ContentType: "application/json",
	})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if payload.Kind != normalize.KindAIChat {
		t.Fatalf("Kind=%v want KindAIChat", payload.Kind)
	}
	if payload.DetectedSpec != "tabnine" {
		t.Errorf("DetectedSpec=%q want tabnine", payload.DetectedSpec)
	}
	if payload.Model != "tabnine-protected" {
		t.Errorf("Model=%q", payload.Model)
	}
	if len(payload.Messages) != 1 {
		t.Fatalf("Messages=%d want 1", len(payload.Messages))
	}
}

func TestNormalize_NonChatJSON_BelowConfidence(t *testing.T) {
	// Non-OpenAI shape JSON — pattern probe scores under 0.5 and should
	// return ErrUnsupported per doc-comment contract.
	body := []byte(`{"foo":"bar","baz":42}`)
	a := &Adapter{}
	_, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType: "tabnine",
		Direction:   normalize.DirectionRequest,
		ContentType: "application/json",
	})
	if err == nil {
		t.Fatal("expected ErrUnsupported for low-confidence body")
	}
}

package githubcopilotweb

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// Identity + configuration

func TestAdapter_ID(t *testing.T) {
	a := &Adapter{}
	if a.ID() != "github-copilot-web" {
		t.Errorf("ID=%q want github-copilot-web", a.ID())
	}
}

func TestAdapter_Configure(t *testing.T) {
	a := &Adapter{}
	if err := a.Configure(nil); err != nil {
		t.Errorf("Configure(nil)=%v", err)
	}
	if err := a.Configure(map[string]any{"foo": "bar"}); err != nil {
		t.Errorf("Configure(map)=%v", err)
	}
}

func TestExtractRequest_Messages(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"explain this PR"}],"model":"gpt-4o","thread_id":"t-1"}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/copilot/api/chat")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "explain this PR" {
		t.Errorf("Segments=%v", nc.Segments)
	}
	if nc.Metadata["model"] != "gpt-4o" {
		t.Errorf("model=%q", nc.Metadata["model"])
	}
	if nc.Metadata["thread_id"] != "t-1" {
		t.Errorf("thread_id=%q", nc.Metadata["thread_id"])
	}
}

// TestExtractRequest_MessagesArray pins multi-turn capture.
func TestExtractRequest_MessagesArray(t *testing.T) {
	body := []byte(`{
		"messages":[
			{"role":"user","content":"first turn"},
			{"role":"assistant","content":"reply"},
			{"role":"user","content":"follow-up"}
		]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/copilot/api/chat")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	want := []string{"first turn", "reply", "follow-up"}
	if len(nc.Segments) != len(want) {
		t.Fatalf("Segments len=%d want %d: %v", len(nc.Segments), len(want), nc.Segments)
	}
	for i := range want {
		if nc.Segments[i] != want[i] {
			t.Errorf("Segments[%d]=%q want %q", i, nc.Segments[i], want[i])
		}
	}
}

// TestExtractRequest_EmptyContentSkipped pins that messages with
// empty-string content do NOT contribute a phantom Segment.
func TestExtractRequest_EmptyContentSkipped(t *testing.T) {
	body := []byte(`{
		"messages":[
			{"role":"user","content":""},
			{"role":"user","content":"real text"}
		]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/copilot/api/chat")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "real text" {
		t.Errorf("Segments=%v want [real text]", nc.Segments)
	}
}

// TestExtractRequest_NonStringContentSkipped pins that a message whose
// `content` is structured (array) does NOT crash the adapter; only
// string content is captured.
func TestExtractRequest_NonStringContentSkipped(t *testing.T) {
	body := []byte(`{
		"messages":[
			{"role":"user","content":[{"type":"text","text":"structured"}]},
			{"role":"user","content":"plain string"}
		]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/copilot/api/chat")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "plain string" {
		t.Errorf("Segments=%v want [plain string] only", nc.Segments)
	}
}

// TestExtractRequest_PromptAliases pins that every alias key (prompt,
// query, text, input) contributes a segment when present and non-empty.
func TestExtractRequest_PromptAliases(t *testing.T) {
	body := []byte(`{
		"prompt":"one",
		"query":"two",
		"text":"three",
		"input":"four"
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/copilot/api/chat")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	want := []string{"one", "two", "three", "four"}
	if len(nc.Segments) != len(want) {
		t.Fatalf("Segments len=%d want %d: %v", len(nc.Segments), len(want), nc.Segments)
	}
	for i := range want {
		if nc.Segments[i] != want[i] {
			t.Errorf("Segments[%d]=%q want %q", i, nc.Segments[i], want[i])
		}
	}
}

// TestExtractRequest_EmptyPromptAliasesSkipped pins that empty alias
// values do NOT produce phantom segments.
func TestExtractRequest_EmptyPromptAliasesSkipped(t *testing.T) {
	body := []byte(`{"prompt":"","query":"","text":"real text","input":""}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/copilot/api/chat")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "real text" {
		t.Errorf("Segments=%v want [real text]", nc.Segments)
	}
}

// TestExtractRequest_ThreadIdCamelCase pins that the adapter accepts
// the camelCase `threadId` alias (Copilot's main wire shape) in
// addition to snake_case `thread_id`.
func TestExtractRequest_ThreadIdCamelCase(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"hi"}],"threadId":"abc-123"}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/copilot/api/chat")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if nc.Metadata["thread_id"] != "abc-123" {
		t.Errorf("thread_id=%q want abc-123 (camelCase alias projected)", nc.Metadata["thread_id"])
	}
}

func TestExtractRequest_Empty(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), nil, "/copilot/api/chat")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v", err)
	}
}

// TestExtractRequest_BinaryBody pins that a non-JSON payload returns
// ErrUnknownSchema with a sanitised binary preview in Extra.
func TestExtractRequest_BinaryBody(t *testing.T) {
	a := &Adapter{}
	body := []byte{0x00, 0x01, 0x02, 0x7f, 0x80, 0xff, 'h', 'i'}
	nc, err := a.ExtractRequest(context.Background(), body, "/copilot/api/chat")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
	prev, ok := nc.Extra["binary_preview"]
	if !ok {
		t.Fatalf("Extra missing binary_preview: %v", nc.Extra)
	}
	if !strings.Contains(prev, "hi") {
		t.Errorf("binary_preview=%q want to contain 'hi'", prev)
	}
}

// TestExtractRequest_Malformed pins ErrMalformed for body bytes that
// pass looksLikeJSON but fail gjson.ValidBytes.
func TestExtractRequest_Malformed(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), []byte(`{"messages":`), "/copilot/api/chat")
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("err=%v want ErrMalformed", err)
	}
}

// TestExtractRequest_UnknownJSON pins ErrUnknownSchema for a valid
// JSON body without any recognised Copilot fields. Extra carries
// unparsed keys for downstream hooks.
func TestExtractRequest_UnknownJSON(t *testing.T) {
	body := []byte(`{"foo":"bar","baz":42}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/copilot/api/chat")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
	if _, ok := nc.Extra["foo"]; !ok {
		t.Errorf("Extra=%v missing foo on unknown-schema path", nc.Extra)
	}
}

func TestExtractResponse_EmptyBody(t *testing.T) {
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), nil, "/copilot/api/chat")
	if err != nil {
		t.Errorf("err=%v want nil (empty body benign)", err)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

// TestExtractResponse_Malformed pins ErrMalformed for invalid JSON.
func TestExtractResponse_Malformed(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), []byte(`{not json`), "/copilot/api/chat")
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("err=%v want ErrMalformed", err)
	}
}

// TestExtractResponse_ContentField pins extraction from a top-level
// `content` string field.
func TestExtractResponse_ContentField(t *testing.T) {
	a := &Adapter{}
	body := []byte(`{"content":"the answer is 42"}`)
	nc, err := a.ExtractResponse(context.Background(), body, "/copilot/api/chat")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "the answer is 42" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

// TestExtractResponse_TextField pins extraction from a top-level
// `text` field — Copilot uses several wrapper shapes.
func TestExtractResponse_TextField(t *testing.T) {
	a := &Adapter{}
	body := []byte(`{"text":"hi from copilot"}`)
	nc, err := a.ExtractResponse(context.Background(), body, "/copilot/api/chat")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "hi from copilot" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

// TestExtractResponse_MessageField pins the `message` envelope shape.
func TestExtractResponse_MessageField(t *testing.T) {
	a := &Adapter{}
	body := []byte(`{"message":"conversation not found"}`)
	nc, err := a.ExtractResponse(context.Background(), body, "/copilot/api/chat")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "conversation not found" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

// TestExtractResponse_ErrorField pins extraction from a top-level
// `error` string field (GitHub's error envelopes sometimes pass the
// error message as a plain string).
func TestExtractResponse_ErrorField(t *testing.T) {
	a := &Adapter{}
	body := []byte(`{"error":"unauthenticated"}`)
	nc, err := a.ExtractResponse(context.Background(), body, "/copilot/api/chat")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "unauthenticated" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

// TestExtractResponse_UnknownJSON pins ErrUnknownSchema fall-through.
func TestExtractResponse_UnknownJSON(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), []byte(`{"foo":"bar"}`), "/copilot/api/chat")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

func TestExtractStreamChunk_OpenAIStyle(t *testing.T) {
	chunk := []byte(`{"choices":[{"delta":{"content":"hello "}}]}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/copilot/api/chat/stream")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "hello " {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

// TestExtractStreamChunk_TopLevelKeys pins the simpler chunk shapes
// where Copilot emits `content`, `text`, `delta`, or `token` at the
// top level. All non-empty values must contribute to Segments in the
// adapter's iteration order.
func TestExtractStreamChunk_TopLevelKeys(t *testing.T) {
	chunk := []byte(`{"content":"alpha","text":"beta","delta":"gamma","token":"delta"}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/copilot/api/chat/stream")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	want := []string{"alpha", "beta", "gamma", "delta"}
	if len(nc.Segments) != len(want) {
		t.Fatalf("Segments len=%d want %d: %v", len(nc.Segments), len(want), nc.Segments)
	}
	for i := range want {
		if nc.Segments[i] != want[i] {
			t.Errorf("Segments[%d]=%q want %q", i, nc.Segments[i], want[i])
		}
	}
}

// TestExtractStreamChunk_EmptyTopLevelKeysSkipped pins that empty
// string values for top-level keys do NOT produce phantom Segments.
func TestExtractStreamChunk_EmptyTopLevelKeysSkipped(t *testing.T) {
	chunk := []byte(`{"content":"","text":"","delta":"","token":"real token"}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/copilot/api/chat/stream")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "real token" {
		t.Errorf("Segments=%v want [real token] only", nc.Segments)
	}
}

// TestExtractStreamChunk_DefensiveOnNonJSON pins fail-open: zero-len /
// non-JSON / invalid-JSON / whitespace-only chunks return a clean
// empty payload with no error.
func TestExtractStreamChunk_DefensiveOnNonJSON(t *testing.T) {
	a := &Adapter{}
	cases := [][]byte{
		nil,
		[]byte(`not json at all`),
		[]byte(`{"oops":`),
		[]byte(`[DONE]`),
		[]byte(`  `),
	}
	for i, c := range cases {
		nc, err := a.ExtractStreamChunk(context.Background(), c, "/copilot/api/chat/stream")
		if err != nil {
			t.Errorf("case %d err=%v want nil (fail-open)", i, err)
		}
		if len(nc.Segments) != 0 {
			t.Errorf("case %d non-empty Segments: %+v", i, nc)
		}
	}
}

// DetectRequestMeta + DetectResponseUsage

func TestDetectRequestMeta(t *testing.T) {
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost, "https://github.com/copilot/api/chat", nil)
	meta := a.DetectRequestMeta(r, []byte(`{"model":"gpt-4o"}`))
	if meta.Provider != "github-copilot-web" {
		t.Errorf("Provider=%q", meta.Provider)
	}
	if meta.Model != "gpt-4o" {
		t.Errorf("Model=%q", meta.Model)
	}
}

// TestDetectRequestMeta_InvalidJSON pins fail-open: a garbage body
// must not panic; Provider stays set, Model stays empty.
func TestDetectRequestMeta_InvalidJSON(t *testing.T) {
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost, "https://github.com/copilot/api/chat", nil)
	meta := a.DetectRequestMeta(r, []byte(`not json`))
	if meta.Provider != "github-copilot-web" {
		t.Errorf("Provider=%q", meta.Provider)
	}
	if meta.Model != "" {
		t.Errorf("Model=%q want empty for invalid body", meta.Model)
	}
}

// TestDetectResponseUsage_NonLLMSentinel pins the non-LLM marker.
// github.com/copilot's wire format does not expose token stats in the
// shapes captured so far.
func TestDetectResponseUsage_NonLLMSentinel(t *testing.T) {
	a := &Adapter{}
	usage := a.DetectResponseUsage(nil, []byte(`{}`))
	if usage.Status != traffic.UsageStatusNonLLM {
		t.Errorf("Status=%q want non_llm", usage.Status)
	}
}

// Rewrite contracts (must return ErrRewriteUnsupported)

func TestRewriteRequestBody_Unsupported(t *testing.T) {
	a := &Adapter{}
	body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	out, n, err := a.RewriteRequestBody(context.Background(), body, "/copilot/api/chat", traffic.NormalizedContent{})
	if !errors.Is(err, traffic.ErrRewriteUnsupported) {
		t.Errorf("err=%v want ErrRewriteUnsupported", err)
	}
	if n != 0 {
		t.Errorf("n=%d want 0", n)
	}
	if string(out) != string(body) {
		t.Errorf("body must be returned unchanged on rewrite-unsupported")
	}
}

func TestRewriteResponseBody_Unsupported(t *testing.T) {
	a := &Adapter{}
	body := []byte(`{"content":"hi"}`)
	out, n, err := a.RewriteResponseBody(context.Background(), body, "/copilot/api/chat", traffic.NormalizedContent{})
	if !errors.Is(err, traffic.ErrRewriteUnsupported) {
		t.Errorf("err=%v want ErrRewriteUnsupported", err)
	}
	if n != 0 {
		t.Errorf("n=%d want 0", n)
	}
	if string(out) != string(body) {
		t.Errorf("body must be returned unchanged")
	}
}

// Normalize (Tier-1 spec dispatch)

// TestNormalize_RequestChatShape pins openai-chat shape recognition
// with DetectedSpec = "github-copilot-web".
func TestNormalize_RequestChatShape(t *testing.T) {
	body := []byte(`{
		"model":"gpt-4o",
		"messages":[
			{"role":"system","content":"You are GitHub Copilot."},
			{"role":"user","content":"explain this PR"}
		]
	}`)
	a := &Adapter{}
	payload, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType:  "github-copilot-web",
		Direction:    normalize.DirectionRequest,
		ContentType:  "application/json",
		EndpointPath: "/copilot/api/chat",
	})
	if err != nil {
		t.Fatalf("Normalize err: %v", err)
	}
	if payload.Kind != normalize.KindAIChat {
		t.Errorf("Kind=%v want ai-chat", payload.Kind)
	}
	if payload.DetectedSpec != "github-copilot-web" {
		t.Errorf("DetectedSpec=%q want github-copilot-web", payload.DetectedSpec)
	}
	if payload.Confidence < 0.5 {
		t.Errorf("Confidence=%v want >= 0.5", payload.Confidence)
	}
}

// TestNormalize_ResponseNonStream pins response-side scoring against
// the shared OpenAI Chat codec the adapter delegates to.
func TestNormalize_ResponseNonStream(t *testing.T) {
	body := []byte(`{
		"id":"chatcmpl-copilot",
		"object":"chat.completion",
		"model":"gpt-4o",
		"choices":[
			{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}
		],
		"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}
	}`)
	a := &Adapter{}
	payload, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType: "github-copilot-web",
		Direction:   normalize.DirectionResponse,
		ContentType: "application/json",
	})
	if err != nil {
		t.Fatalf("Normalize err: %v", err)
	}
	if payload.Kind != normalize.KindAIChat {
		t.Errorf("Kind=%v want ai-chat", payload.Kind)
	}
	if payload.DetectedSpec != "github-copilot-web" {
		t.Errorf("DetectedSpec=%q want github-copilot-web", payload.DetectedSpec)
	}
}

// TestNormalize_UnrecognisedShape_FallsThrough returns ErrUnsupported
// so the Coordinator can fall through to Tier 2 / Tier 3.
func TestNormalize_UnrecognisedShape_FallsThrough(t *testing.T) {
	body := []byte(`{"foo":"bar","baz":42}`)
	a := &Adapter{}
	_, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType: "github-copilot-web",
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
		{"string-prefix", []byte(`"x"`), false},
		{"control-byte-prefix", []byte{0x00, '{'}, false},
	}
	for _, c := range cases {
		if got := looksLikeJSON(c.in); got != c.want {
			t.Errorf("%s: looksLikeJSON(%q)=%v want %v", c.name, c.in, got, c.want)
		}
	}
}

// TestPreview pins the binary-safe sanitisation contract.
func TestPreview(t *testing.T) {
	t.Run("short-printable-passthrough", func(t *testing.T) {
		if got := preview([]byte("hello world")); got != "hello world" {
			t.Errorf("got=%q want 'hello world'", got)
		}
	})
	t.Run("preserves-newline-and-tab", func(t *testing.T) {
		got := preview([]byte("a\nb\tc"))
		if got != "a\nb\tc" {
			t.Errorf("got=%q want 'a\\nb\\tc'", got)
		}
	})
	t.Run("scrubs-control-bytes", func(t *testing.T) {
		got := preview([]byte{'a', 0x07, 'b', 0x0d, 'c', 0x1b, 'd'})
		if got != "a.b.c.d" {
			t.Errorf("got=%q want 'a.b.c.d'", got)
		}
	})
	t.Run("scrubs-high-bytes", func(t *testing.T) {
		got := preview([]byte{'a', 0x7f, 'b', 0x80, 'c', 0xff, 'd'})
		if got != "a.b.c.d" {
			t.Errorf("got=%q want 'a.b.c.d'", got)
		}
	})
	t.Run("truncates-over-256-bytes", func(t *testing.T) {
		body := make([]byte, 300)
		for i := range body {
			body[i] = 'A'
		}
		got := preview(body)
		if len(got) != 256 {
			t.Errorf("len=%d want 256", len(got))
		}
	})
	t.Run("empty-input", func(t *testing.T) {
		if got := preview(nil); got != "" {
			t.Errorf("got=%q want empty", got)
		}
	})
}

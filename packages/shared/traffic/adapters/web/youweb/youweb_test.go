package youweb

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// Identity + configuration

func TestAdapter_ID(t *testing.T) {
	a := &Adapter{}
	if a.ID() != "you-web" {
		t.Errorf("ID=%q want you-web", a.ID())
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

// TestExtractRequest_Q pins the You.com-specific `q` search alias plus
// `chatMode` metadata.
func TestExtractRequest_Q(t *testing.T) {
	body := []byte(`{"q":"climate change","chatMode":"smart"}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "climate change" {
		t.Errorf("Segments=%v", nc.Segments)
	}
	if nc.Metadata["chatMode"] != "smart" {
		t.Errorf("chatMode=%q want smart", nc.Metadata["chatMode"])
	}
}

// TestExtractRequest_MessagesArray covers the openai-chat-like shape.
func TestExtractRequest_MessagesArray(t *testing.T) {
	body := []byte(`{
		"messages": [
			{"role":"user","content":"summarise the doc"},
			{"role":"assistant","content":"Sure — here's the summary."},
			{"role":"user","content":"now translate to spanish"}
		],
		"model": "you-pro"
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	want := []string{"summarise the doc", "Sure — here's the summary.", "now translate to spanish"}
	if len(nc.Segments) != len(want) {
		t.Fatalf("Segments len=%d want %d: %v", len(nc.Segments), len(want), nc.Segments)
	}
	for i := range want {
		if nc.Segments[i] != want[i] {
			t.Errorf("Segments[%d]=%q want %q", i, nc.Segments[i], want[i])
		}
	}
	if nc.Metadata["model"] != "you-pro" {
		t.Errorf("model=%q want you-pro", nc.Metadata["model"])
	}
}

// TestExtractRequest_PromptAliases pins every alias in the You.com
// adapter: the generic OpenAI quartet (`prompt`/`query`/`text`/`input`)
// plus the You-specific `q` short form. All non-empty values must
// contribute a segment in declaration order.
func TestExtractRequest_PromptAliases(t *testing.T) {
	body := []byte(`{
		"prompt":"one",
		"query":"two",
		"q":"three",
		"text":"four",
		"input":"five"
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	want := []string{"one", "two", "three", "four", "five"}
	if len(nc.Segments) != len(want) {
		t.Fatalf("Segments len=%d want %d: %v", len(nc.Segments), len(want), nc.Segments)
	}
	for i := range want {
		if nc.Segments[i] != want[i] {
			t.Errorf("Segments[%d]=%q want %q", i, nc.Segments[i], want[i])
		}
	}
}

// TestExtractRequest_EmptyPromptAliasesSkipped pins that alias keys
// with empty string values do NOT contribute phantom segments.
func TestExtractRequest_EmptyPromptAliasesSkipped(t *testing.T) {
	body := []byte(`{"prompt":"","query":"","q":"","text":"real text","input":""}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "real text" {
		t.Errorf("Segments=%v want [real text]", nc.Segments)
	}
}

// TestExtractRequest_NonStringContentSkipped pins that a message
// whose `content` is structured (array/object) does not crash; only
// string content is captured.
func TestExtractRequest_NonStringContentSkipped(t *testing.T) {
	body := []byte(`{
		"messages": [
			{"role":"user","content":[{"type":"text","text":"structured"}]},
			{"role":"user","content":"plain string"}
		]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "plain string" {
		t.Errorf("Segments=%v want [plain string] only", nc.Segments)
	}
}

// TestExtractRequest_ModelMetaMissing pins that an absent `model` key
// does NOT leave a phantom empty value in Metadata.
func TestExtractRequest_ModelMetaMissing(t *testing.T) {
	body := []byte(`{"prompt":"hi"}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if _, ok := nc.Metadata["model"]; ok {
		t.Errorf("model key present in Metadata=%v want absent", nc.Metadata)
	}
	if _, ok := nc.Metadata["chatMode"]; ok {
		t.Errorf("chatMode key present in Metadata=%v want absent", nc.Metadata)
	}
}

func TestExtractRequest_Empty(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), nil, "/api/x")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

// TestExtractRequest_BinaryBody pins that a non-JSON binary payload
// returns ErrUnknownSchema.
func TestExtractRequest_BinaryBody(t *testing.T) {
	body := []byte{0x00, 0x01, 0x02, 0x7f, 0x80, 0xff, 'h', 'i', 0x05}
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), body, "/api/x")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

// TestExtractRequest_Malformed pins ErrMalformed for bytes that look
// like JSON but fail to parse.
func TestExtractRequest_Malformed(t *testing.T) {
	body := []byte(`{"prompt": "missing close-brace`)
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), body, "/api/x")
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("err=%v want ErrMalformed", err)
	}
}

// TestExtractRequest_UnknownJSON pins ErrUnknownSchema when the body
// is valid JSON but carries no recognised you-web fields.
func TestExtractRequest_UnknownJSON(t *testing.T) {
	body := []byte(`{"foo":"bar","baz":42}`)
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), body, "/api/x")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

func TestExtractResponse_EmptyBody(t *testing.T) {
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), nil, "/api/x")
	if err != nil {
		t.Errorf("err=%v want nil (empty body is benign)", err)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

// TestExtractResponse_Malformed pins ErrMalformed for invalid JSON.
func TestExtractResponse_Malformed(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), []byte(`{"oops":`), "/api/x")
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("err=%v want ErrMalformed", err)
	}
}

// TestExtractResponse_ErrorEnvelope pins the OpenAI-style
// `error.message` shape.
func TestExtractResponse_ErrorEnvelope(t *testing.T) {
	body := []byte(`{"error":{"message":"rate limit exceeded","type":"rate_limit","code":"quota"}}`)
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), body, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "rate limit exceeded" {
		t.Errorf("Segments=%v want [rate limit exceeded]", nc.Segments)
	}
	if nc.Metadata["error"] != "true" {
		t.Errorf("error metadata=%q want true", nc.Metadata["error"])
	}
}

// TestExtractResponse_UnknownJSON pins fall-through to ErrUnknownSchema
// when valid JSON has no error envelope.
func TestExtractResponse_UnknownJSON(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), []byte(`{"foo":"bar"}`), "/api/x")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

// TestExtractResponse_EmptyErrorMessageFallsThrough pins that an
// `error` envelope with an empty message string does NOT short-
// circuit — falls through to unknown-schema.
func TestExtractResponse_EmptyErrorMessageFallsThrough(t *testing.T) {
	body := []byte(`{"error":{"message":""}}`)
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), body, "/api/x")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema (empty error message)", err)
	}
}

// TestExtractStreamChunk_YouChatToken pins the You-specific SSE token
// field for the chat answer stream.
func TestExtractStreamChunk_YouChatToken(t *testing.T) {
	chunk := []byte(`{"youChatToken":"hello "}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/api/stream")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "hello " {
		t.Errorf("Segments=%v want [hello ]", nc.Segments)
	}
}

// TestExtractStreamChunk_TopLevelKeys covers every top-level fan-out
// key — text, content, answer — plus youChatToken, in declaration
// order. Empty values for any of them are skipped.
func TestExtractStreamChunk_TopLevelKeys(t *testing.T) {
	chunk := []byte(`{
		"text":"streamed token",
		"content":"second",
		"answer":"third",
		"youChatToken":"fourth"
	}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/api/stream")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	want := []string{"streamed token", "second", "third", "fourth"}
	if len(nc.Segments) != len(want) {
		t.Fatalf("Segments len=%d want %d: %v", len(nc.Segments), len(want), nc.Segments)
	}
	for i := range want {
		if nc.Segments[i] != want[i] {
			t.Errorf("Segments[%d]=%q want %q", i, nc.Segments[i], want[i])
		}
	}
}

// TestExtractStreamChunk_EmptyKeysSkipped pins that empty string
// values for any of the fan-out keys are dropped.
func TestExtractStreamChunk_EmptyKeysSkipped(t *testing.T) {
	chunk := []byte(`{"text":"","content":"only","answer":"","youChatToken":""}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/api/stream")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "only" {
		t.Errorf("Segments=%v want [only]", nc.Segments)
	}
}

// TestExtractStreamChunk_DefensiveOnNonJSON pins fail-open: non-JSON,
// invalid-JSON, marker frames, and whitespace-only chunks return a
// clean empty payload with no error.
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
		nc, err := a.ExtractStreamChunk(context.Background(), c, "/api/stream")
		if err != nil {
			t.Errorf("case %d err=%v want nil (fail-open)", i, err)
		}
		if len(nc.Segments) != 0 || len(nc.ToolCallSegments) != 0 {
			t.Errorf("case %d non-empty content: %+v", i, nc)
		}
	}
}

// DetectRequestMeta + DetectResponseUsage

// TestDetectRequestMeta pins that you-web stamps Provider but does
// NOT extract Model from the body (You.com's wire is undocumented).
func TestDetectRequestMeta(t *testing.T) {
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost, "https://chat.you.com/api/x", nil)
	meta := a.DetectRequestMeta(r, []byte(`{"model":"you-pro"}`))
	if meta.Provider != "you-web" {
		t.Errorf("Provider=%q want you-web", meta.Provider)
	}
	if meta.Model != "" {
		t.Errorf("Model=%q want empty (you-web does not read body model)", meta.Model)
	}
}

func TestDetectResponseUsage_NonLLMSentinel(t *testing.T) {
	a := &Adapter{}
	usage := a.DetectResponseUsage(nil, []byte(`{}`))
	if usage.Status != traffic.UsageStatusNonLLM {
		t.Errorf("Status=%q want non_llm", usage.Status)
	}
	if usage.PromptTokens != nil || usage.CompletionTokens != nil {
		t.Errorf("token pointers must be nil for you-web; got %+v", usage)
	}
}

// Rewrite contracts (must return ErrRewriteUnsupported)

func TestRewriteRequestBody_Unsupported(t *testing.T) {
	a := &Adapter{}
	body := []byte(`{"prompt":"hi"}`)
	out, n, err := a.RewriteRequestBody(context.Background(), body, "/api/x", traffic.NormalizedContent{})
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
	body := []byte(`{"error":{"message":"x"}}`)
	out, n, err := a.RewriteResponseBody(context.Background(), body, "/api/x", traffic.NormalizedContent{})
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

// TestNormalize_RequestChatShape pins that an openai-chat-shaped body
// claims Tier 1 via the shared OpenAI Chat codec and stamps DetectedSpec to
// the adapter ID directly (no "pattern:" prefix).
func TestNormalize_RequestChatShape(t *testing.T) {
	body := []byte(`{
		"model": "you-pro",
		"messages": [
			{"role":"system","content":"You are a search assistant."},
			{"role":"user","content":"who won the 2024 world cup"}
		]
	}`)
	a := &Adapter{}
	payload, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType:  "you-web",
		Direction:    normalize.DirectionRequest,
		ContentType:  "application/json",
		EndpointPath: "/api/x",
	})
	if err != nil {
		t.Fatalf("Normalize err: %v", err)
	}
	if payload.Kind != normalize.KindAIChat {
		t.Errorf("Kind=%v want ai-chat", payload.Kind)
	}
	if payload.DetectedSpec != "you-web" {
		t.Errorf("DetectedSpec=%q want you-web", payload.DetectedSpec)
	}
	if payload.Model != "you-pro" {
		t.Errorf("Model=%q want you-pro", payload.Model)
	}
	if len(payload.Messages) < 1 {
		t.Fatalf("Messages empty: %+v", payload.Messages)
	}
	if payload.Confidence < 0.5 {
		t.Errorf("Confidence=%v want >= 0.5", payload.Confidence)
	}
}

// TestNormalize_UnrecognisedShape_FallsThrough verifies an unrecognised
// body returns ErrUnsupported so the Coordinator falls through to Tier 2.
func TestNormalize_UnrecognisedShape_FallsThrough(t *testing.T) {
	body := []byte(`{"foo":"bar","baz":42}`)
	a := &Adapter{}
	_, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType: "you-web",
		Direction:   normalize.DirectionRequest,
		ContentType: "application/json",
	})
	if !errors.Is(err, normalize.ErrUnsupported) {
		t.Errorf("err=%v want ErrUnsupported", err)
	}
}

// Internal helpers — looksLikeJSON

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

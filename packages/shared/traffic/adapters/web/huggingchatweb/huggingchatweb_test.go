package huggingchatweb

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
	if a.ID() != "huggingchat-web" {
		t.Errorf("ID=%q want huggingchat-web", a.ID())
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

// TestExtractRequest_Inputs pins the simplest HuggingChat shape — a
// top-level `inputs` string is one of the prompt aliases scanned in the
// for-loop. Conversation `id` is captured in metadata.
func TestExtractRequest_Inputs(t *testing.T) {
	body := []byte(`{"inputs":"What is recursion?","id":"conv-1"}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/chat/conversation/conv-1")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "What is recursion?" {
		t.Errorf("Segments=%v", nc.Segments)
	}
	if nc.Metadata["id"] != "conv-1" {
		t.Errorf("id metadata=%q want conv-1", nc.Metadata["id"])
	}
	// `inputs` and `id` are in requestKnownKeys — must NOT leak into Extra.
	if _, ok := nc.Extra["inputs"]; ok {
		t.Errorf("inputs leaked into Extra: %v", nc.Extra)
	}
	if _, ok := nc.Extra["id"]; ok {
		t.Errorf("id leaked into Extra: %v", nc.Extra)
	}
}

// TestExtractRequest_MessagesArray pins the openai-chat-like shape used
// on /chat/conversation/<id>: a `messages` array with string content.
func TestExtractRequest_MessagesArray(t *testing.T) {
	body := []byte(`{
		"messages":[
			{"role":"user","content":"first"},
			{"role":"assistant","content":"reply"},
			{"role":"user","content":"third"}
		],
		"model":"meta-llama/Meta-Llama-3-70B-Instruct"
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/chat/conversation/c-x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	want := []string{"first", "reply", "third"}
	if len(nc.Segments) != len(want) {
		t.Fatalf("Segments len=%d want %d", len(nc.Segments), len(want))
	}
	for i := range want {
		if nc.Segments[i] != want[i] {
			t.Errorf("Segments[%d]=%q want %q", i, nc.Segments[i], want[i])
		}
	}
	if nc.Metadata["model"] != "meta-llama/Meta-Llama-3-70B-Instruct" {
		t.Errorf("model=%q", nc.Metadata["model"])
	}
}

// TestExtractRequest_PromptAliases pins that every alias key in the
// scan loop (`prompt`, `query`, `text`, `input`, `inputs`) contributes
// a segment when present and non-empty.
func TestExtractRequest_PromptAliases(t *testing.T) {
	body := []byte(`{
		"prompt":"one",
		"query":"two",
		"text":"three",
		"input":"four",
		"inputs":"five"
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/chat/x")
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

// TestExtractRequest_EmptyAliasesSkipped pins that an alias key with an
// empty string value does NOT contribute a phantom segment.
func TestExtractRequest_EmptyAliasesSkipped(t *testing.T) {
	body := []byte(`{"prompt":"","query":"","text":"real","input":"","inputs":""}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/chat/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "real" {
		t.Errorf("Segments=%v want [real]", nc.Segments)
	}
}

// TestExtractRequest_NonStringContentSkipped pins that structured
// content (array/object multimodal parts) does not crash; only string
// content is captured. Defence-in-depth as HuggingChat evolves.
func TestExtractRequest_NonStringContentSkipped(t *testing.T) {
	body := []byte(`{
		"messages":[
			{"role":"user","content":[{"type":"text","text":"structured"}]},
			{"role":"user","content":"plain"}
		]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/chat/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "plain" {
		t.Errorf("Segments=%v want [plain]", nc.Segments)
	}
}

// TestExtractRequest_ModelMetaMissing pins that an absent `model` field
// leaves the metadata map empty without nil-map panic or phantom value.
func TestExtractRequest_ModelMetaMissing(t *testing.T) {
	body := []byte(`{"prompt":"hi"}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/chat/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if _, ok := nc.Metadata["model"]; ok {
		t.Errorf("model key present in Metadata=%v want absent", nc.Metadata)
	}
}

// TestExtractRequest_IDMetaMissing pins symmetry: `id` only stamped
// when present and non-empty.
func TestExtractRequest_IDMetaMissing(t *testing.T) {
	body := []byte(`{"prompt":"hi","id":""}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/chat/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if _, ok := nc.Metadata["id"]; ok {
		t.Errorf("id key present in Metadata=%v want absent", nc.Metadata)
	}
}

// TestExtractRequest_ExtraCapturesUnknownFields pins the safety net:
// fields outside requestKnownKeys land in Extra so hooks can see them.
func TestExtractRequest_ExtraCapturesUnknownFields(t *testing.T) {
	body := []byte(`{
		"prompt":"hi",
		"x_hf_secret":{"v":"sensitive"},
		"experimental":true
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/chat/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	x, ok := nc.Extra["x_hf_secret"]
	if !ok || !strings.Contains(x, "sensitive") {
		t.Errorf("Extra=%v missing x_hf_secret", nc.Extra)
	}
	if _, ok := nc.Extra["experimental"]; !ok {
		t.Errorf("Extra=%v missing experimental", nc.Extra)
	}
	if _, ok := nc.Extra["prompt"]; ok {
		t.Errorf("prompt must not leak into Extra")
	}
}

func TestExtractRequest_Empty(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), nil, "/chat/x")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

// TestExtractRequest_BinaryBody pins that a non-JSON binary payload
// returns ErrUnknownSchema and stamps a sanitised preview into Extra.
func TestExtractRequest_BinaryBody(t *testing.T) {
	body := []byte{0x00, 0x01, 0x7f, 0x80, 0xff, 'h', 'i', 0x05}
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/chat/x")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
	prev, ok := nc.Extra["binary_preview"]
	if !ok {
		t.Fatalf("Extra missing binary_preview: %v", nc.Extra)
	}
	if !strings.Contains(prev, "hi") {
		t.Errorf("binary_preview=%q missing 'hi'", prev)
	}
	if strings.ContainsAny(prev, "\x00\x01\x7f\x80\xff") { //nolint:staticcheck // SA1011: intentional bad-UTF8 test fixture
		t.Errorf("binary_preview=%q must scrub control bytes", prev)
	}
}

// TestExtractRequest_Malformed pins ErrMalformed for body bytes that
// begin like JSON but are not parseable.
func TestExtractRequest_Malformed(t *testing.T) {
	body := []byte(`{"prompt":"unclosed`)
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), body, "/chat/x")
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("err=%v want ErrMalformed", err)
	}
}

// TestExtractRequest_UnknownJSON pins ErrUnknownSchema for valid JSON
// without recognised fields; Extra still populated for triage.
func TestExtractRequest_UnknownJSON(t *testing.T) {
	body := []byte(`{"foo":"bar","baz":42}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/chat/x")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
	if _, ok := nc.Extra["foo"]; !ok {
		t.Errorf("Extra=%v missing foo on unknown-schema path", nc.Extra)
	}
}

func TestExtractResponse_EmptyBody(t *testing.T) {
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), nil, "/chat/x")
	if err != nil {
		t.Errorf("err=%v want nil (empty body benign)", err)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

// TestExtractResponse_Malformed pins ErrMalformed for non-JSON body.
func TestExtractResponse_Malformed(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), []byte(`{not json`), "/chat/x")
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("err=%v want ErrMalformed", err)
	}
}

// TestExtractResponse_BareMessage pins the simpler `message` top-level
// shape — HuggingChat returns error envelopes in this form (e.g.
// "conversation not found"). Surfaced as a segment with error metadata.
func TestExtractResponse_BareMessage(t *testing.T) {
	body := []byte(`{"message":"conversation not found"}`)
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), body, "/chat/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "conversation not found" {
		t.Errorf("Segments=%v", nc.Segments)
	}
	if nc.Metadata["error"] != "true" {
		t.Errorf("error metadata=%q want true", nc.Metadata["error"])
	}
}

// TestExtractResponse_TopLevelErrorString pins the alternate
// `error: "..."` top-level shape.
func TestExtractResponse_TopLevelErrorString(t *testing.T) {
	body := []byte(`{"error":"rate limit exceeded"}`)
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), body, "/chat/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "rate limit exceeded" {
		t.Errorf("Segments=%v", nc.Segments)
	}
	if nc.Metadata["error"] != "true" {
		t.Errorf("error metadata=%q want true", nc.Metadata["error"])
	}
}

// TestExtractResponse_UnknownJSON pins the fall-through: JSON with
// neither `message` nor `error` returns ErrUnknownSchema.
func TestExtractResponse_UnknownJSON(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), []byte(`{"foo":"bar"}`), "/chat/x")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

// TestExtractResponse_EmptyMessageFallsThrough pins that an empty
// `message` string does NOT short-circuit — falls through to unknown
// rather than emitting an empty segment.
func TestExtractResponse_EmptyMessageFallsThrough(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), []byte(`{"message":""}`), "/chat/x")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema (empty message)", err)
	}
}

// TestExtractResponse_EmptyErrorFallsThrough pins symmetry for the
// `error` string variant.
func TestExtractResponse_EmptyErrorFallsThrough(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), []byte(`{"error":""}`), "/chat/x")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema (empty error)", err)
	}
}

// TestExtractStreamChunk_Stream pins HuggingChat's `stream` event type:
// a JSON-line frame whose `token` field carries one token's text.
func TestExtractStreamChunk_Stream(t *testing.T) {
	chunk := []byte(`{"type":"stream","token":"Hello "}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/chat/conversation/c-1")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "Hello " {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

// TestExtractStreamChunk_StreamEmptyToken pins that a `stream` event
// with an empty `token` does NOT add a phantom segment.
func TestExtractStreamChunk_StreamEmptyToken(t *testing.T) {
	chunk := []byte(`{"type":"stream","token":""}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/chat/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("Segments=%v want empty", nc.Segments)
	}
}

// TestExtractStreamChunk_FinalAnswer pins the `finalAnswer` event type
// that carries the completed response in `text`.
func TestExtractStreamChunk_FinalAnswer(t *testing.T) {
	chunk := []byte(`{"type":"finalAnswer","text":"Recursion is a function calling itself."}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/chat/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "Recursion is a function calling itself." {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

// TestExtractStreamChunk_FinalAnswerEmptyText pins that an empty
// `text` on a finalAnswer event does NOT add a phantom segment.
func TestExtractStreamChunk_FinalAnswerEmptyText(t *testing.T) {
	chunk := []byte(`{"type":"finalAnswer","text":""}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/chat/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("Segments=%v want empty", nc.Segments)
	}
}

// TestExtractStreamChunk_UnknownType pins that an event whose `type`
// string isn't recognised yields no segments but still returns cleanly
// (no fallback scan — the typed-frame branch returns early).
func TestExtractStreamChunk_UnknownType(t *testing.T) {
	chunk := []byte(`{"type":"ping","text":"should not be captured"}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/chat/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("Segments=%v want empty for unknown type (early return)", nc.Segments)
	}
}

// TestExtractStreamChunk_FallbackTopLevelText pins the no-type-field
// fallback: a chunk that omits `type` scans the known field list and
// captures `text` / `content` / `token` values.
func TestExtractStreamChunk_FallbackTopLevelText(t *testing.T) {
	chunk := []byte(`{"text":"streamed bit"}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/chat/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "streamed bit" {
		t.Errorf("Segments=%v want [streamed bit]", nc.Segments)
	}
}

// TestExtractStreamChunk_FallbackAllFields pins that the fallback loop
// gathers every known field name in order.
func TestExtractStreamChunk_FallbackAllFields(t *testing.T) {
	chunk := []byte(`{"text":"A","content":"B","token":"C"}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/chat/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	want := []string{"A", "B", "C"}
	if len(nc.Segments) != len(want) {
		t.Fatalf("Segments len=%d want %d: %v", len(nc.Segments), len(want), nc.Segments)
	}
	for i := range want {
		if nc.Segments[i] != want[i] {
			t.Errorf("Segments[%d]=%q want %q", i, nc.Segments[i], want[i])
		}
	}
}

// TestExtractStreamChunk_FallbackEmptyFieldsSkipped pins symmetry:
// empty strings in the fallback fields do not add phantom segments.
func TestExtractStreamChunk_FallbackEmptyFieldsSkipped(t *testing.T) {
	chunk := []byte(`{"text":"","content":"keep","token":""}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/chat/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "keep" {
		t.Errorf("Segments=%v want [keep]", nc.Segments)
	}
}

// TestExtractStreamChunk_DefensiveOnNonJSON pins fail-open: non-JSON,
// invalid-JSON, empty/whitespace chunks all return a clean empty
// payload with no error (the wire is undocumented, can't error).
func TestExtractStreamChunk_DefensiveOnNonJSON(t *testing.T) {
	a := &Adapter{}
	cases := [][]byte{
		nil,
		[]byte(``),
		[]byte(`not json`),
		[]byte(`{"oops":`),
		[]byte(`[DONE]`),
		[]byte("  \t"),
	}
	for i, c := range cases {
		nc, err := a.ExtractStreamChunk(context.Background(), c, "/chat/x")
		if err != nil {
			t.Errorf("case %d err=%v want nil (fail-open)", i, err)
		}
		if len(nc.Segments) != 0 {
			t.Errorf("case %d non-empty segments: %+v", i, nc.Segments)
		}
	}
}

// DetectRequestMeta + DetectResponseUsage

func TestDetectRequestMeta_ProviderAndModel(t *testing.T) {
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost, "https://huggingface.co/chat/conversation/x", nil)
	meta := a.DetectRequestMeta(r, []byte(`{"model":"meta-llama/Meta-Llama-3-70B-Instruct"}`))
	if meta.Provider != "huggingchat-web" {
		t.Errorf("Provider=%q want huggingchat-web", meta.Provider)
	}
	if meta.Model != "meta-llama/Meta-Llama-3-70B-Instruct" {
		t.Errorf("Model=%q", meta.Model)
	}
}

func TestDetectRequestMeta_InvalidJSONBody(t *testing.T) {
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost, "https://huggingface.co/chat/x", nil)
	meta := a.DetectRequestMeta(r, []byte(`not json`))
	if meta.Provider != "huggingchat-web" {
		t.Errorf("Provider=%q", meta.Provider)
	}
	if meta.Model != "" {
		t.Errorf("Model=%q want empty", meta.Model)
	}
}

func TestDetectRequestMeta_EmptyBody(t *testing.T) {
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost, "https://huggingface.co/chat/x", nil)
	meta := a.DetectRequestMeta(r, nil)
	if meta.Provider != "huggingchat-web" {
		t.Errorf("Provider=%q", meta.Provider)
	}
	if meta.Model != "" {
		t.Errorf("Model=%q want empty for nil body", meta.Model)
	}
}

func TestDetectResponseUsage_NonLLMSentinel(t *testing.T) {
	a := &Adapter{}
	usage := a.DetectResponseUsage(nil, []byte(`{}`))
	if usage.Status != traffic.UsageStatusNonLLM {
		t.Errorf("Status=%q want non_llm", usage.Status)
	}
	if usage.PromptTokens != nil || usage.CompletionTokens != nil {
		t.Errorf("token pointers must be nil for huggingchat-web; got %+v", usage)
	}
}

// Rewrite contracts (must return ErrRewriteUnsupported)

func TestRewriteRequestBody_Unsupported(t *testing.T) {
	a := &Adapter{}
	body := []byte(`{"prompt":"hi"}`)
	out, n, err := a.RewriteRequestBody(context.Background(), body, "/chat/x", traffic.NormalizedContent{})
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

func TestRewriteResponseBody_Unsupported(t *testing.T) {
	a := &Adapter{}
	body := []byte(`{"message":"x"}`)
	out, n, err := a.RewriteResponseBody(context.Background(), body, "/chat/x", traffic.NormalizedContent{})
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

// TestNormalize_RequestChatShape pins that an openai-chat-shaped
// request body is accepted by the Tier-1 normalizer and stamped with
// DetectedSpec = "huggingchat-web".
func TestNormalize_RequestChatShape(t *testing.T) {
	body := []byte(`{
		"model":"meta-llama/Meta-Llama-3-70B-Instruct",
		"messages":[
			{"role":"system","content":"You are HuggingChat."},
			{"role":"user","content":"explain compose"}
		]
	}`)
	a := &Adapter{}
	payload, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType:  "huggingchat-web",
		Direction:    normalize.DirectionRequest,
		ContentType:  "application/json",
		EndpointPath: "/chat/x",
	})
	if err != nil {
		t.Fatalf("Normalize err: %v", err)
	}
	if payload.Kind != normalize.KindAIChat {
		t.Errorf("Kind=%v want ai-chat", payload.Kind)
	}
	if payload.DetectedSpec != "huggingchat-web" {
		t.Errorf("DetectedSpec=%q want huggingchat-web", payload.DetectedSpec)
	}
	if payload.Model != "meta-llama/Meta-Llama-3-70B-Instruct" {
		t.Errorf("Model=%q", payload.Model)
	}
	if len(payload.Messages) < 1 {
		t.Fatalf("Messages empty: %+v", payload.Messages)
	}
	if payload.Confidence < 0.5 {
		t.Errorf("Confidence=%v want >= 0.5", payload.Confidence)
	}
}

// TestNormalize_ResponseNonStream pins response-side scoring.
func TestNormalize_ResponseNonStream(t *testing.T) {
	body := []byte(`{
		"id":"chatcmpl-hf",
		"object":"chat.completion",
		"model":"meta-llama/Meta-Llama-3-70B-Instruct",
		"choices":[
			{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}
		],
		"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}
	}`)
	a := &Adapter{}
	payload, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType: "huggingchat-web",
		Direction:   normalize.DirectionResponse,
		ContentType: "application/json",
	})
	if err != nil {
		t.Fatalf("Normalize err: %v", err)
	}
	if payload.Kind != normalize.KindAIChat {
		t.Errorf("Kind=%v want ai-chat", payload.Kind)
	}
	if payload.DetectedSpec != "huggingchat-web" {
		t.Errorf("DetectedSpec=%q want huggingchat-web", payload.DetectedSpec)
	}
}

// TestNormalize_UnrecognisedShape_FallsThrough verifies a body matching
// neither spec returns ErrUnsupported so the Coordinator can fall
// through to Tier 2.
func TestNormalize_UnrecognisedShape_FallsThrough(t *testing.T) {
	body := []byte(`{"foo":"bar","baz":42}`)
	a := &Adapter{}
	_, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType: "huggingchat-web",
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

// TestPreview pins the binary-safe sanitisation contract:
//   - control bytes < 0x20 (except \n and \t) become '.'
//   - bytes > 0x7e become '.'
//   - printable ASCII passes through
//   - inputs longer than 256 bytes are truncated to 256
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
			t.Errorf("len=%d want 256 (truncated)", len(got))
		}
	})
	t.Run("exactly-256-bytes-passes-through", func(t *testing.T) {
		body := make([]byte, 256)
		for i := range body {
			body[i] = 'B'
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

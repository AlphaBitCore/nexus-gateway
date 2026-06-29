package v0web

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
	if a.ID() != "v0-web" {
		t.Errorf("ID=%q want v0-web", a.ID())
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

// TestExtractRequest_PromptOnly pins the simplest v0.dev request shape:
// a top-level `prompt` string. Captures the segment + model metadata.
func TestExtractRequest_PromptOnly(t *testing.T) {
	body := []byte(`{"prompt":"build a landing page","model":"v0-1","framework":"react","session_id":"sess-x"}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "build a landing page" {
		t.Errorf("Segments=%v", nc.Segments)
	}
	if nc.Metadata["model"] != "v0-1" {
		t.Errorf("model metadata=%q want v0-1", nc.Metadata["model"])
	}
}

// TestExtractRequest_MessagesArray pins the openai-chat-like shape
// where v0 sends a `messages` array with `content` strings.
func TestExtractRequest_MessagesArray(t *testing.T) {
	body := []byte(`{
		"messages": [
			{"role": "user", "content": "describe a checkout flow"},
			{"role": "assistant", "content": "Sure — here's the layout."},
			{"role": "user", "content": "make it dark mode"}
		],
		"model": "v0-1.5-md"
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	want := []string{
		"describe a checkout flow",
		"Sure — here's the layout.",
		"make it dark mode",
	}
	if len(nc.Segments) != len(want) {
		t.Fatalf("Segments len=%d want %d", len(nc.Segments), len(want))
	}
	for i := range want {
		if nc.Segments[i] != want[i] {
			t.Errorf("Segments[%d]=%q want %q", i, nc.Segments[i], want[i])
		}
	}
	if nc.Metadata["model"] != "v0-1.5-md" {
		t.Errorf("model=%q", nc.Metadata["model"])
	}
}

// TestExtractRequest_MessagesAndPromptCombined covers a body that
// includes both a `messages` array and one of the prompt-alias fields
// (`query`). Both sources must contribute segments — losing either
// would silently lose audit content.
func TestExtractRequest_MessagesAndPromptCombined(t *testing.T) {
	body := []byte(`{
		"messages": [{"role":"user","content":"first turn"}],
		"query": "follow-up via query field"
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	want := []string{"first turn", "follow-up via query field"}
	if len(nc.Segments) != len(want) {
		t.Fatalf("Segments len=%d want %d: %v", len(nc.Segments), len(want), nc.Segments)
	}
	for i := range want {
		if nc.Segments[i] != want[i] {
			t.Errorf("Segments[%d]=%q want %q", i, nc.Segments[i], want[i])
		}
	}
}

// TestExtractRequest_PromptAliases pins that every alias key in the
// adapter's compatibility list (`prompt`, `query`, `text`, `input`)
// contributes a segment when present and non-empty.
func TestExtractRequest_PromptAliases(t *testing.T) {
	body := []byte(`{
		"prompt": "one",
		"query": "two",
		"text": "three",
		"input": "four"
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/x")
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

// TestExtractRequest_EmptyPromptAliasesSkipped pins that an alias key
// with an empty string value does NOT contribute a phantom segment.
func TestExtractRequest_EmptyPromptAliasesSkipped(t *testing.T) {
	body := []byte(`{"prompt":"","query":"","text":"real text","input":""}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "real text" {
		t.Errorf("Segments=%v want [real text]", nc.Segments)
	}
}

// TestExtractRequest_ToolCalls pins that messages[].tool_calls land in
// ToolCallSegments verbatim (raw JSON) so the compliance pipeline can
// inspect tool-use payloads.
func TestExtractRequest_ToolCalls(t *testing.T) {
	body := []byte(`{
		"messages": [
			{"role":"assistant","content":"calling a tool","tool_calls":[
				{"id":"call_a","function":{"name":"create_component","arguments":"{\"kind\":\"button\"}"}},
				{"id":"call_b","function":{"name":"add_state","arguments":"{}"}}
			]}
		]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "calling a tool" {
		t.Errorf("Segments=%v", nc.Segments)
	}
	if len(nc.ToolCallSegments) != 2 {
		t.Fatalf("ToolCallSegments len=%d want 2", len(nc.ToolCallSegments))
	}
	if !strings.Contains(nc.ToolCallSegments[0], "create_component") {
		t.Errorf("ToolCallSegments[0]=%q missing tool name", nc.ToolCallSegments[0])
	}
	if !strings.Contains(nc.ToolCallSegments[1], "add_state") {
		t.Errorf("ToolCallSegments[1]=%q missing tool name", nc.ToolCallSegments[1])
	}
}

// TestExtractRequest_ToolCallsOnlyNoSegments pins that a body whose
// only audit content is tool_calls still returns a populated payload
// (not ErrUnknownSchema). The current code checks
// `len(segments)==0 && len(toolCalls)==0` for the unknown-schema path,
// so a tool-only frame must be recognised.
func TestExtractRequest_ToolCallsOnlyNoSegments(t *testing.T) {
	body := []byte(`{
		"messages": [
			{"role":"assistant","tool_calls":[
				{"id":"call_a","function":{"name":"only_tool"}}
			]}
		]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/x")
	if err != nil {
		t.Fatalf("err=%v want nil (tool_calls present)", err)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("Segments=%v want empty", nc.Segments)
	}
	if len(nc.ToolCallSegments) != 1 {
		t.Errorf("ToolCallSegments len=%d want 1", len(nc.ToolCallSegments))
	}
}

// TestExtractRequest_NonStringContentSkipped pins that a message
// whose `content` is structured (array or object, e.g. multimodal
// parts) does NOT crash the adapter; only string content is captured.
// This is defence-in-depth — v0 may evolve to multimodal content
// blocks like the public OpenAI shape.
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

// TestExtractRequest_ModelMetaMissing pins that the model meta map is
// returned without a `model` key when the body omits one. (Metadata
// is constructed inside the happy-path branch, so we want to confirm
// no nil-map panic and no phantom empty value.)
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
}

func TestExtractRequest_Empty(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), nil, "/api/x")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

// TestExtractRequest_BinaryBody pins that a non-JSON binary payload
// (form upload, octet-stream, etc.) returns ErrUnknownSchema.
func TestExtractRequest_BinaryBody(t *testing.T) {
	body := []byte{0x00, 0x01, 0x02, 0x7f, 0x80, 0xff, 'h', 'i', 0x05}
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), body, "/api/x")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

// TestExtractRequest_Malformed pins ErrMalformed for body bytes that
// begin like JSON but are not parseable.
func TestExtractRequest_Malformed(t *testing.T) {
	body := []byte(`{"prompt": "missing close-brace`)
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), body, "/api/x")
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("err=%v want ErrMalformed", err)
	}
}

// TestExtractRequest_UnknownJSON pins ErrUnknownSchema when the body
// is valid JSON but carries no recognised v0 fields.
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

// TestExtractResponse_Malformed: body starts as JSON but fails to
// parse — must be classified as malformed.
func TestExtractResponse_Malformed(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), []byte(`{"oops":`), "/api/x")
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("err=%v want ErrMalformed", err)
	}
}

// TestExtractResponse_BinaryPayload: a body that fails the JSON
// prefix check AND is not parseable as JSON — classified as
// malformed (matches the gjson.ValidBytes==false branch inside the
// combined !looksLikeJSON || !valid condition).
func TestExtractResponse_BinaryPayload(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), []byte{0x00, 0xff, 'x'}, "/api/x")
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("err=%v want ErrMalformed", err)
	}
}

// TestExtractResponse_JSONScalarNotObject: a body that gjson considers
// valid JSON (a scalar like a number or a string) but whose first
// non-whitespace byte is neither '{' nor '['. This is the only way to
// reach the ErrUnknownSchema branch nested inside the !looksLikeJSON
// || !valid condition (looksLikeJSON==false yet gjson.ValidBytes==true).
func TestExtractResponse_JSONScalarNotObject(t *testing.T) {
	a := &Adapter{}
	for _, body := range [][]byte{
		[]byte(`42`),
		[]byte(`"a string"`),
		[]byte(`true`),
		[]byte(`null`),
	} {
		_, err := a.ExtractResponse(context.Background(), body, "/api/x")
		if !errors.Is(err, traffic.ErrUnknownSchema) {
			t.Errorf("body=%q err=%v want ErrUnknownSchema", body, err)
		}
	}
}

// TestExtractResponse_ErrorEnvelope pins the OpenAI-style
// `error.message` shape: the adapter exposes the message as a
// segment and stamps the error metadata flag.
func TestExtractResponse_ErrorEnvelope(t *testing.T) {
	body := []byte(`{"error":{"message":"rate limit exceeded","type":"rate_limit_exceeded","code":"quota_exceeded"}}`)
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

// TestExtractResponse_BareMessage pins the simpler `message` field
// (some v0 endpoints return a top-level message). Also stamps the
// error metadata flag — per the adapter contract, a non-streamed
// JSON response is by definition not the main reply path on v0.
func TestExtractResponse_BareMessage(t *testing.T) {
	body := []byte(`{"message":"conversation not found"}`)
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), body, "/api/x")
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

// TestExtractResponse_UnknownJSON pins the fall-through: valid JSON
// without an error envelope nor a top-level message is unknown
// schema (v0 streams successful responses, so non-error JSON here
// is unexpected).
func TestExtractResponse_UnknownJSON(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), []byte(`{"foo":"bar"}`), "/api/x")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

// TestExtractResponse_EmptyErrorMessageFallsThrough pins that an
// envelope with `error` but an empty message string does NOT short-
// circuit — falls through to unknown-schema rather than emitting an
// empty error segment.
func TestExtractResponse_EmptyErrorMessageFallsThrough(t *testing.T) {
	body := []byte(`{"error":{"message":""}}`)
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), body, "/api/x")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema (empty error message)", err)
	}
}

// TestExtractStreamChunk_DeltaContent pins the openai-chat-SSE delta
// shape: `choices[0].delta.content` carries one token's text.
func TestExtractStreamChunk_DeltaContent(t *testing.T) {
	chunk := []byte(`{"choices":[{"delta":{"content":"Hello"}}]}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "Hello" {
		t.Errorf("Segments=%v want [Hello]", nc.Segments)
	}
}

// TestExtractStreamChunk_DeltaToolCalls pins that streamed tool-use
// frames land in ToolCallSegments verbatim.
func TestExtractStreamChunk_DeltaToolCalls(t *testing.T) {
	chunk := []byte(`{"choices":[{"delta":{"tool_calls":[
		{"index":0,"id":"call_a","function":{"name":"create_component"}},
		{"index":1,"id":"call_b","function":{"name":"add_state"}}
	]}}]}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.ToolCallSegments) != 2 {
		t.Fatalf("ToolCallSegments len=%d want 2", len(nc.ToolCallSegments))
	}
	if !strings.Contains(nc.ToolCallSegments[0], "create_component") {
		t.Errorf("ToolCallSegments[0]=%q missing tool name", nc.ToolCallSegments[0])
	}
}

// TestExtractStreamChunk_TopLevelText pins the simpler shape where a
// chunk is just `{"text": "...token..."}` — v0 sometimes emits this
// alongside the openai-delta shape.
func TestExtractStreamChunk_TopLevelText(t *testing.T) {
	chunk := []byte(`{"text":"streamed token"}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "streamed token" {
		t.Errorf("Segments=%v want [streamed token]", nc.Segments)
	}
}

// TestExtractStreamChunk_DeltaAndTopLevelTextCombined pins that both
// sources can contribute in a single chunk — `delta.content` first,
// then top-level `text`.
func TestExtractStreamChunk_DeltaAndTopLevelTextCombined(t *testing.T) {
	chunk := []byte(`{"choices":[{"delta":{"content":"from delta"}}],"text":"from text"}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	want := []string{"from delta", "from text"}
	if len(nc.Segments) != len(want) {
		t.Fatalf("Segments len=%d want %d: %v", len(nc.Segments), len(want), nc.Segments)
	}
	for i := range want {
		if nc.Segments[i] != want[i] {
			t.Errorf("Segments[%d]=%q want %q", i, nc.Segments[i], want[i])
		}
	}
}

// TestExtractStreamChunk_EmptyDeltaContent pins that delta.content="" does
// NOT produce an empty Segments entry.
func TestExtractStreamChunk_EmptyDeltaContent(t *testing.T) {
	chunk := []byte(`{"choices":[{"delta":{"content":""}}]}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("empty content leaked into Segments=%v", nc.Segments)
	}
}

// TestExtractStreamChunk_EmptyChunk: zero-length chunk is a no-op.
func TestExtractStreamChunk_EmptyChunk(t *testing.T) {
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), nil, "/api/x")
	if err != nil {
		t.Errorf("err=%v want nil", err)
	}
	if len(nc.Segments) != 0 || len(nc.ToolCallSegments) != 0 {
		t.Errorf("non-empty content: %+v", nc)
	}
}

// TestExtractStreamChunk_DefensiveOnNonJSON pins fail-open: v0's wire
// is undocumented and SSE telemetry frames or marker bytes must not
// error. Non-JSON / invalid-JSON / non-object chunks return a clean
// empty payload.
func TestExtractStreamChunk_DefensiveOnNonJSON(t *testing.T) {
	a := &Adapter{}
	cases := [][]byte{
		[]byte(`not json at all`),
		[]byte(`{"oops":`),
		[]byte(`[DONE]`),
		[]byte(`  `),
	}
	for i, c := range cases {
		nc, err := a.ExtractStreamChunk(context.Background(), c, "/api/x")
		if err != nil {
			t.Errorf("case %d err=%v want nil (fail-open)", i, err)
		}
		if len(nc.Segments) != 0 || len(nc.ToolCallSegments) != 0 {
			t.Errorf("case %d non-empty content: %+v", i, nc)
		}
	}
}

// TestExtractStreamChunk_DeltaNotObject: when `choices[0].delta` is
// not an object (e.g. a finish-only frame), the chunk should yield no
// content but must not error.
func TestExtractStreamChunk_DeltaNotObject(t *testing.T) {
	chunk := []byte(`{"choices":[{"finish_reason":"stop"}]}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("Segments leaked: %v", nc.Segments)
	}
}

// DetectRequestMeta + DetectResponseUsage

func TestDetectRequestMeta_ProviderAndModel(t *testing.T) {
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost, "https://v0.dev/api/x", nil)
	meta := a.DetectRequestMeta(r, []byte(`{"model":"v0-1.5-md"}`))
	if meta.Provider != "v0-web" {
		t.Errorf("Provider=%q want v0-web", meta.Provider)
	}
	if meta.Model != "v0-1.5-md" {
		t.Errorf("Model=%q want v0-1.5-md", meta.Model)
	}
}

func TestDetectRequestMeta_InvalidJSONBody(t *testing.T) {
	// Adapter must never panic on garbage input — Provider stays set,
	// Model stays empty.
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost, "https://v0.dev/api/x", nil)
	meta := a.DetectRequestMeta(r, []byte(`not json`))
	if meta.Provider != "v0-web" {
		t.Errorf("Provider=%q", meta.Provider)
	}
	if meta.Model != "" {
		t.Errorf("Model=%q want empty", meta.Model)
	}
}

func TestDetectRequestMeta_EmptyBody(t *testing.T) {
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost, "https://v0.dev/api/x", nil)
	meta := a.DetectRequestMeta(r, nil)
	if meta.Provider != "v0-web" {
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
		t.Errorf("token pointers must be nil for v0-web; got %+v", usage)
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

// TestNormalize_RequestChatShape pins that an openai-chat-shaped
// request body claims Tier 1 via the shared OpenAI Chat codec and stamps
// DetectedSpec = "v0-web" — the per-adapter caller stamps the
// adapter ID directly (no "pattern:" prefix).
func TestNormalize_RequestChatShape(t *testing.T) {
	body := []byte(`{
		"model": "v0-1.5-md",
		"messages": [
			{"role": "system", "content": "You are a v0 codegen agent."},
			{"role": "user", "content": "build a navbar"}
		]
	}`)
	a := &Adapter{}
	payload, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType:  "v0-web",
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
	if payload.DetectedSpec != "v0-web" {
		t.Errorf("DetectedSpec=%q want v0-web (no pattern: prefix for adapter caller)", payload.DetectedSpec)
	}
	if payload.Model != "v0-1.5-md" {
		t.Errorf("Model=%q want v0-1.5-md", payload.Model)
	}
	if len(payload.Messages) < 1 {
		t.Fatalf("Messages empty, want at least 1: %+v", payload.Messages)
	}
	if payload.Confidence < 0.5 {
		t.Errorf("Confidence=%v want >= 0.5", payload.Confidence)
	}
}

// TestNormalize_ResponseNonStream pins response-side scoring against
// the shared OpenAI Chat codec the adapter delegates to.
func TestNormalize_ResponseNonStream(t *testing.T) {
	body := []byte(`{
		"id": "chatcmpl-v0",
		"object": "chat.completion",
		"model": "v0-1.5-md",
		"choices": [
			{"index":0,"message":{"role":"assistant","content":"<button>OK</button>"},"finish_reason":"stop"}
		],
		"usage": {"prompt_tokens": 7, "completion_tokens": 4, "total_tokens": 11}
	}`)
	a := &Adapter{}
	payload, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType: "v0-web",
		Direction:   normalize.DirectionResponse,
		ContentType: "application/json",
	})
	if err != nil {
		t.Fatalf("Normalize err: %v", err)
	}
	if payload.Kind != normalize.KindAIChat {
		t.Errorf("Kind=%v want ai-chat", payload.Kind)
	}
	if payload.DetectedSpec != "v0-web" {
		t.Errorf("DetectedSpec=%q want v0-web", payload.DetectedSpec)
	}
}

// TestNormalize_UnrecognisedShape_FallsThrough verifies that a body
// matching neither the request nor response specs returns
// ErrUnsupported so the Coordinator can fall through to Tier 2.
func TestNormalize_UnrecognisedShape_FallsThrough(t *testing.T) {
	body := []byte(`{"foo":"bar","baz":42}`)
	a := &Adapter{}
	_, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType: "v0-web",
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

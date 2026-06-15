package devinweb

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
	if a.ID() != "devin-web" {
		t.Errorf("ID=%q want devin-web", a.ID())
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

// TestExtractRequest_Goal pins the simplest devin shape: a `goal`
// alias plus `task_id` metadata. Both `goal` and `task` are
// devin-specific prompt aliases on top of the generic openai-like keys.
func TestExtractRequest_Goal(t *testing.T) {
	body := []byte(`{"goal":"Refactor authentication module","task_id":"task-1"}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "Refactor authentication module" {
		t.Errorf("Segments=%v", nc.Segments)
	}
	if nc.Metadata["task_id"] != "task-1" {
		t.Errorf("task_id=%q want task-1", nc.Metadata["task_id"])
	}
	// `task_id` is in requestKnownKeys; must not leak into Extra.
	if _, ok := nc.Extra["task_id"]; ok {
		t.Errorf("task_id leaked to Extra=%v", nc.Extra)
	}
}

// TestExtractRequest_MessagesArray covers the openai-chat-like shape
// with a `messages` array of string-content turns.
func TestExtractRequest_MessagesArray(t *testing.T) {
	body := []byte(`{
		"messages": [
			{"role":"user","content":"plan migration"},
			{"role":"assistant","content":"Step 1: audit current auth"},
			{"role":"user","content":"start with the JWT path"}
		],
		"model": "devin-1",
		"task_id": "t-42"
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	want := []string{"plan migration", "Step 1: audit current auth", "start with the JWT path"}
	if len(nc.Segments) != len(want) {
		t.Fatalf("Segments len=%d want %d: %v", len(nc.Segments), len(want), nc.Segments)
	}
	for i := range want {
		if nc.Segments[i] != want[i] {
			t.Errorf("Segments[%d]=%q want %q", i, nc.Segments[i], want[i])
		}
	}
	if nc.Metadata["model"] != "devin-1" {
		t.Errorf("model=%q want devin-1", nc.Metadata["model"])
	}
	if nc.Metadata["task_id"] != "t-42" {
		t.Errorf("task_id=%q want t-42", nc.Metadata["task_id"])
	}
}

// TestExtractRequest_PromptAliases covers every devin prompt alias:
// the generic OpenAI quartet (`prompt`/`query`/`text`/`input`) plus the
// devin-specific `goal`/`task` pair. All non-empty alias values must
// contribute a segment in declaration order.
func TestExtractRequest_PromptAliases(t *testing.T) {
	body := []byte(`{
		"prompt":"one",
		"query":"two",
		"text":"three",
		"input":"four",
		"goal":"five",
		"task":"six"
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	want := []string{"one", "two", "three", "four", "five", "six"}
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
// with empty string values do not contribute phantom segments.
func TestExtractRequest_EmptyPromptAliasesSkipped(t *testing.T) {
	body := []byte(`{"prompt":"","query":"","text":"real text","input":"","goal":"","task":""}`)
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
// inspect Devin's tool-use payloads.
func TestExtractRequest_ToolCalls(t *testing.T) {
	body := []byte(`{
		"messages": [
			{"role":"assistant","content":"calling a tool","tool_calls":[
				{"id":"call_a","function":{"name":"run_shell","arguments":"{\"cmd\":\"ls\"}"}},
				{"id":"call_b","function":{"name":"edit_file","arguments":"{}"}}
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
	if !strings.Contains(nc.ToolCallSegments[0], "run_shell") {
		t.Errorf("ToolCallSegments[0]=%q missing tool name", nc.ToolCallSegments[0])
	}
	if !strings.Contains(nc.ToolCallSegments[1], "edit_file") {
		t.Errorf("ToolCallSegments[1]=%q missing tool name", nc.ToolCallSegments[1])
	}
}

// TestExtractRequest_ToolCallsOnlyNoSegments pins that tool-only
// frames return a populated payload rather than ErrUnknownSchema.
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

// TestExtractRequest_NonStringContentSkipped pins that structured
// (non-string) `content` values do not crash the adapter.
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

// TestExtractRequest_ModelMetaMissing pins that no `model` key in the
// body means no `model` key in metadata (no phantom empty value).
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
	if _, ok := nc.Metadata["task_id"]; ok {
		t.Errorf("task_id key present in Metadata=%v want absent", nc.Metadata)
	}
}

// TestExtractRequest_ExtraCapturesUnknownFields pins the safety net:
// fields outside requestKnownKeys must reach NormalizedContent.Extra.
func TestExtractRequest_ExtraCapturesUnknownFields(t *testing.T) {
	body := []byte(`{
		"prompt": "hi",
		"x_devin_telemetry": {"trace": "abc"},
		"experimental_flag": true
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	x, ok := nc.Extra["x_devin_telemetry"]
	if !ok || !strings.Contains(x, "abc") {
		t.Errorf("Extra=%v missing x_devin_telemetry", nc.Extra)
	}
	if _, ok := nc.Extra["experimental_flag"]; !ok {
		t.Errorf("Extra=%v missing experimental_flag", nc.Extra)
	}
	if _, ok := nc.Extra["prompt"]; ok {
		t.Errorf("prompt must not leak into Extra (it is consumed)")
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
// returns ErrUnknownSchema and stamps a sanitised preview into Extra.
func TestExtractRequest_BinaryBody(t *testing.T) {
	body := []byte{0x00, 0x01, 0x02, 0x7f, 0x80, 0xff, 'h', 'i', 0x05}
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/x")
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
	if strings.ContainsAny(prev, "\x00\x01\x7f\x80\xff") { //nolint:staticcheck // SA1011: intentional bad-UTF8 test fixture
		t.Errorf("binary_preview=%q must scrub control bytes", prev)
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
// is valid JSON but carries no recognised devin fields.
func TestExtractRequest_UnknownJSON(t *testing.T) {
	body := []byte(`{"foo":"bar","baz":42}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/x")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
	if _, ok := nc.Extra["foo"]; !ok {
		t.Errorf("Extra=%v missing foo on unknown-schema path", nc.Extra)
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
	body := []byte(`{"error":{"message":"task failed","type":"agent_error"}}`)
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), body, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "task failed" {
		t.Errorf("Segments=%v want [task failed]", nc.Segments)
	}
	if nc.Metadata["error"] != "true" {
		t.Errorf("error metadata=%q want true", nc.Metadata["error"])
	}
}

// TestExtractResponse_UnknownJSON pins fall-through to ErrUnknownSchema
// when valid JSON carries no error envelope.
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

// TestExtractStreamChunk_Thought pins the devin-specific `thought`
// key — Devin streams its agent reasoning trace via top-level keys.
func TestExtractStreamChunk_Thought(t *testing.T) {
	chunk := []byte(`{"thought":"I should explore the auth module structure."}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/api/stream")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "I should explore the auth module structure." {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

// TestExtractStreamChunk_DeltaContent pins the openai-chat SSE shape:
// `choices[0].delta.content`.
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
		{"index":0,"id":"call_a","function":{"name":"run_shell"}},
		{"index":1,"id":"call_b","function":{"name":"edit_file"}}
	]}}]}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.ToolCallSegments) != 2 {
		t.Fatalf("ToolCallSegments len=%d want 2", len(nc.ToolCallSegments))
	}
	if !strings.Contains(nc.ToolCallSegments[0], "run_shell") {
		t.Errorf("ToolCallSegments[0]=%q missing tool name", nc.ToolCallSegments[0])
	}
}

// TestExtractStreamChunk_DeltaEmptyContentSkipped pins that an empty
// delta.content does not produce an empty Segments entry.
func TestExtractStreamChunk_DeltaEmptyContentSkipped(t *testing.T) {
	chunk := []byte(`{"choices":[{"delta":{"content":""}}]}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("empty delta.content leaked: %v", nc.Segments)
	}
}

// TestExtractStreamChunk_TopLevelKeys covers all four non-delta
// top-level fan-out keys: text, content, delta, action. (`thought`
// is covered by a dedicated test above.) Note the adapter returns
// EARLY from the delta-object branch, so this body cannot also
// contain a delta object.
func TestExtractStreamChunk_TopLevelKeys(t *testing.T) {
	chunk := []byte(`{
		"text":"streamed token",
		"content":"second token",
		"delta":"third token",
		"action":"shell:ls"
	}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	want := []string{"streamed token", "second token", "third token", "shell:ls"}
	if len(nc.Segments) != len(want) {
		t.Fatalf("Segments len=%d want %d: %v", len(nc.Segments), len(want), nc.Segments)
	}
	for i := range want {
		if nc.Segments[i] != want[i] {
			t.Errorf("Segments[%d]=%q want %q", i, nc.Segments[i], want[i])
		}
	}
}

// TestExtractStreamChunk_TopLevelEmptyKeysSkipped pins that empty
// string values for any top-level fan-out key are skipped.
func TestExtractStreamChunk_TopLevelEmptyKeysSkipped(t *testing.T) {
	chunk := []byte(`{"text":"","content":"only","delta":"","thought":"","action":""}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "only" {
		t.Errorf("Segments=%v want [only]", nc.Segments)
	}
}

// TestExtractStreamChunk_DeltaNotObject pins that a chunk whose
// choices[0] omits a delta (e.g. a finish-only frame) does not crash
// — the adapter falls through to the top-level key fan-out which
// also finds nothing.
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

// TestExtractStreamChunk_DefensiveOnNonJSON pins fail-open: non-JSON,
// invalid-JSON, marker frames, and whitespace-only chunks return a
// clean empty payload.
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
		nc, err := a.ExtractStreamChunk(context.Background(), c, "/api/x")
		if err != nil {
			t.Errorf("case %d err=%v want nil (fail-open)", i, err)
		}
		if len(nc.Segments) != 0 || len(nc.ToolCallSegments) != 0 {
			t.Errorf("case %d non-empty content: %+v", i, nc)
		}
	}
}

// DetectRequestMeta + DetectResponseUsage

// TestDetectRequestMeta pins that devin-web stamps Provider but does
// NOT extract Model from the body (devin's wire is undocumented;
// adapter keeps Model empty until live fixtures arrive).
func TestDetectRequestMeta(t *testing.T) {
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost, "https://app.devin.ai/api/x", nil)
	meta := a.DetectRequestMeta(r, []byte(`{"model":"devin-1"}`))
	if meta.Provider != "devin-web" {
		t.Errorf("Provider=%q want devin-web", meta.Provider)
	}
	if meta.Model != "" {
		t.Errorf("Model=%q want empty (devin-web does not read body model)", meta.Model)
	}
}

func TestDetectResponseUsage_NonLLMSentinel(t *testing.T) {
	a := &Adapter{}
	usage := a.DetectResponseUsage(nil, []byte(`{}`))
	if usage.Status != traffic.UsageStatusNonLLM {
		t.Errorf("Status=%q want non_llm", usage.Status)
	}
	if usage.PromptTokens != nil || usage.CompletionTokens != nil {
		t.Errorf("token pointers must be nil for devin-web; got %+v", usage)
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
		"model": "devin-1",
		"messages": [
			{"role":"system","content":"You are a software-engineering agent."},
			{"role":"user","content":"refactor the auth module"}
		]
	}`)
	a := &Adapter{}
	payload, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType:  "devin-web",
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
	if payload.DetectedSpec != "devin-web" {
		t.Errorf("DetectedSpec=%q want devin-web", payload.DetectedSpec)
	}
	if payload.Model != "devin-1" {
		t.Errorf("Model=%q want devin-1", payload.Model)
	}
	if len(payload.Messages) < 1 {
		t.Fatalf("Messages empty: %+v", payload.Messages)
	}
	if payload.Confidence < 0.5 {
		t.Errorf("Confidence=%v want >= 0.5", payload.Confidence)
	}
}

// TestNormalize_UnrecognisedShape_FallsThrough verifies that an
// unrecognised body returns ErrUnsupported so the Coordinator falls
// through to Tier 2.
func TestNormalize_UnrecognisedShape_FallsThrough(t *testing.T) {
	body := []byte(`{"foo":"bar","baz":42}`)
	a := &Adapter{}
	_, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType: "devin-web",
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

// TestPreview pins the binary-safe sanitisation contract: control
// bytes (except \n and \t) become '.', bytes > 0x7e become '.',
// printable ASCII passes through, inputs are capped at 256 bytes.
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
			t.Errorf("got=%q want 'a.b.c.d' (>0x7e scrubbed)", got)
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
	t.Run("empty-input", func(t *testing.T) {
		if got := preview(nil); got != "" {
			t.Errorf("got=%q want empty", got)
		}
	})
}

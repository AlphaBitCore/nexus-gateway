package boltweb

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
	if a.ID() != "bolt-web" {
		t.Errorf("ID=%q want bolt-web", a.ID())
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

// TestExtractRequest_PromptOnly pins the simplest bolt.new request
// shape: a top-level `prompt` string. Captures the segment + model
// metadata and stamps Extra with the remaining top-level fields so
// unparsed content reaches downstream hooks.
func TestExtractRequest_PromptOnly(t *testing.T) {
	body := []byte(`{"prompt":"build a chat app","model":"claude-3-5-sonnet","project_id":"p-1","session_id":"sess-x"}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "build a chat app" {
		t.Errorf("Segments=%v", nc.Segments)
	}
	if nc.Metadata["model"] != "claude-3-5-sonnet" {
		t.Errorf("model metadata=%q want claude-3-5-sonnet", nc.Metadata["model"])
	}
	// `project_id` and `session_id` belong to requestKnownKeys → not in Extra.
	if _, ok := nc.Extra["project_id"]; ok {
		t.Errorf("project_id should be consumed, not in Extra: %v", nc.Extra)
	}
	if _, ok := nc.Extra["session_id"]; ok {
		t.Errorf("session_id should be consumed, not in Extra: %v", nc.Extra)
	}
}

// TestExtractRequest_MessagesArray pins the openai-chat-like shape
// where bolt.new sends a `messages` array.
func TestExtractRequest_MessagesArray(t *testing.T) {
	body := []byte(`{
		"messages": [
			{"role": "user", "content": "scaffold a vite app"},
			{"role": "assistant", "content": "Sure — building."},
			{"role": "user", "content": "add tailwind"}
		],
		"model": "claude-3-5-sonnet"
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	want := []string{
		"scaffold a vite app",
		"Sure — building.",
		"add tailwind",
	}
	if len(nc.Segments) != len(want) {
		t.Fatalf("Segments len=%d want %d", len(nc.Segments), len(want))
	}
	for i := range want {
		if nc.Segments[i] != want[i] {
			t.Errorf("Segments[%d]=%q want %q", i, nc.Segments[i], want[i])
		}
	}
	if nc.Metadata["model"] != "claude-3-5-sonnet" {
		t.Errorf("model=%q", nc.Metadata["model"])
	}
}

// TestExtractRequest_MessagesAndPromptCombined covers a body that
// includes both a `messages` array and one of the prompt-alias fields
// (`query`). Both sources must contribute segments.
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
// ToolCallSegments verbatim so the compliance pipeline can inspect
// tool-use payloads.
func TestExtractRequest_ToolCalls(t *testing.T) {
	body := []byte(`{
		"messages": [
			{"role":"assistant","content":"calling a tool","tool_calls":[
				{"id":"call_a","function":{"name":"write_file","arguments":"{\"path\":\"index.ts\"}"}},
				{"id":"call_b","function":{"name":"run_npm","arguments":"{}"}}
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
	if !strings.Contains(nc.ToolCallSegments[0], "write_file") {
		t.Errorf("ToolCallSegments[0]=%q missing tool name", nc.ToolCallSegments[0])
	}
	if !strings.Contains(nc.ToolCallSegments[1], "run_npm") {
		t.Errorf("ToolCallSegments[1]=%q missing tool name", nc.ToolCallSegments[1])
	}
}

// TestExtractRequest_ToolCallsOnlyNoSegments pins that a body whose
// only audit content is tool_calls still returns a populated payload
// (not ErrUnknownSchema).
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
// whose `content` is structured (array, e.g. multimodal parts) does
// NOT crash; only string content is captured.
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
// returned without a `model` key when the body omits one.
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

// TestExtractRequest_ExtraCapturesUnknownFields pins that fields
// outside the requestKnownKeys list reach NormalizedContent.Extra —
// safety net against silent data loss when bolt.new ships a new field.
func TestExtractRequest_ExtraCapturesUnknownFields(t *testing.T) {
	body := []byte(`{
		"prompt": "hi",
		"x_new_bolt_field": {"sensitive": "secret_value"},
		"experimental_flag": true
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	x, ok := nc.Extra["x_new_bolt_field"]
	if !ok || !strings.Contains(x, "secret_value") {
		t.Errorf("Extra=%v missing x_new_bolt_field", nc.Extra)
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
// returns ErrUnknownSchema and stamps a sanitised binary preview into
// Extra for triage.
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
// is valid JSON but carries no recognised bolt fields — the response
// still includes Extra so hooks can see the foreign payload.
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

// TestExtractResponse_Malformed: body starts as JSON but fails to
// parse — must be classified as malformed.
func TestExtractResponse_Malformed(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), []byte(`{"oops":`), "/api/x")
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("err=%v want ErrMalformed", err)
	}
}

// TestExtractResponse_BinaryPayload: a body that fails gjson.ValidBytes
// (binary noise) → ErrMalformed under the response path (validity is
// checked before looksLikeJSON).
func TestExtractResponse_BinaryPayload(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), []byte{0x00, 0xff, 'x'}, "/api/x")
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("err=%v want ErrMalformed", err)
	}
}

// TestExtractResponse_JSONScalarNotObject: a body that gjson considers
// valid JSON (a scalar) but whose first non-whitespace byte is neither
// '{' nor '['. Reaches the !looksLikeJSON branch after the validity
// check passes → ErrUnknownSchema.
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
// `error.message` shape: the adapter exposes the message as a segment
// and stamps the error metadata flag.
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

// TestExtractResponse_UnknownJSON pins the fall-through: valid JSON
// without an error envelope is unknown schema (bolt streams successful
// responses, so non-error JSON here is unexpected).
func TestExtractResponse_UnknownJSON(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), []byte(`{"foo":"bar"}`), "/api/x")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

// TestExtractResponse_EmptyErrorMessageFallsThrough pins that an
// envelope with `error` but empty message string does NOT short-
// circuit — falls through to unknown-schema rather than emitting an
// empty segment.
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

// TestExtractStreamChunk_TopLevelText pins the simpler shape where a
// chunk is just `{"text": "...token..."}`.
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

// TestExtractStreamChunk_EmptyDeltaContent pins that delta.content=""
// does NOT produce an empty Segments entry.
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

// TestExtractStreamChunk_EmptyTopLevelText pins that text="" does NOT
// produce a phantom segment.
func TestExtractStreamChunk_EmptyTopLevelText(t *testing.T) {
	chunk := []byte(`{"text":""}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("empty text leaked into Segments=%v", nc.Segments)
	}
}

// TestExtractStreamChunk_EmptyChunk: zero-length chunk is a no-op.
func TestExtractStreamChunk_EmptyChunk(t *testing.T) {
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), nil, "/api/x")
	if err != nil {
		t.Errorf("err=%v want nil", err)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("non-empty content: %+v", nc)
	}
}

// TestExtractStreamChunk_DefensiveOnNonJSON pins fail-open: non-JSON /
// invalid-JSON / non-object chunks return a clean empty payload.
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
		if len(nc.Segments) != 0 {
			t.Errorf("case %d non-empty content: %+v", i, nc)
		}
	}
}

// TestExtractStreamChunk_DeltaNotObject: when `choices[0].delta` is
// missing (e.g. a finish-only frame), the chunk should yield no
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
	r, _ := http.NewRequest(http.MethodPost, "https://bolt.new/api/x", nil)
	meta := a.DetectRequestMeta(r, []byte(`{"model":"claude-3-5-sonnet"}`))
	if meta.Provider != "bolt-web" {
		t.Errorf("Provider=%q want bolt-web", meta.Provider)
	}
	if meta.Model != "claude-3-5-sonnet" {
		t.Errorf("Model=%q want claude-3-5-sonnet", meta.Model)
	}
}

func TestDetectRequestMeta_InvalidJSONBody(t *testing.T) {
	// Adapter must never panic on garbage input — Provider stays set,
	// Model stays empty.
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost, "https://bolt.new/api/x", nil)
	meta := a.DetectRequestMeta(r, []byte(`not json`))
	if meta.Provider != "bolt-web" {
		t.Errorf("Provider=%q", meta.Provider)
	}
	if meta.Model != "" {
		t.Errorf("Model=%q want empty", meta.Model)
	}
}

func TestDetectRequestMeta_EmptyBody(t *testing.T) {
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost, "https://bolt.new/api/x", nil)
	meta := a.DetectRequestMeta(r, nil)
	if meta.Provider != "bolt-web" {
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
		t.Errorf("token pointers must be nil for bolt-web; got %+v", usage)
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
// DetectedSpec = "bolt-web".
func TestNormalize_RequestChatShape(t *testing.T) {
	body := []byte(`{
		"model": "claude-3-5-sonnet",
		"messages": [
			{"role": "system", "content": "You are a bolt fullstack codegen agent."},
			{"role": "user", "content": "scaffold a vite app"}
		]
	}`)
	a := &Adapter{}
	payload, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType:  "bolt-web",
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
	if payload.DetectedSpec != "bolt-web" {
		t.Errorf("DetectedSpec=%q want bolt-web (no pattern: prefix for adapter caller)", payload.DetectedSpec)
	}
	if payload.Model != "claude-3-5-sonnet" {
		t.Errorf("Model=%q want claude-3-5-sonnet", payload.Model)
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
		"id": "chatcmpl-bolt",
		"object": "chat.completion",
		"model": "claude-3-5-sonnet",
		"choices": [
			{"index":0,"message":{"role":"assistant","content":"<button>OK</button>"},"finish_reason":"stop"}
		],
		"usage": {"prompt_tokens": 7, "completion_tokens": 4, "total_tokens": 11}
	}`)
	a := &Adapter{}
	payload, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType: "bolt-web",
		Direction:   normalize.DirectionResponse,
		ContentType: "application/json",
	})
	if err != nil {
		t.Fatalf("Normalize err: %v", err)
	}
	if payload.Kind != normalize.KindAIChat {
		t.Errorf("Kind=%v want ai-chat", payload.Kind)
	}
	if payload.DetectedSpec != "bolt-web" {
		t.Errorf("DetectedSpec=%q want bolt-web", payload.DetectedSpec)
	}
}

// TestNormalize_UnrecognisedShape_FallsThrough verifies a body matching
// no known spec returns ErrUnsupported so the Coordinator can fall
// through to Tier 2.
func TestNormalize_UnrecognisedShape_FallsThrough(t *testing.T) {
	body := []byte(`{"foo":"bar","baz":42}`)
	a := &Adapter{}
	_, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType: "bolt-web",
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
			t.Errorf("got=%q want 'hello world'", got)
		}
	})
	t.Run("preserves-newline-and-tab", func(t *testing.T) {
		got := preview([]byte("a\nb\tc"))
		if got != "a\nb\tc" {
			t.Errorf("got=%q want 'a\\nb\\tc' (newline+tab preserved)", got)
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

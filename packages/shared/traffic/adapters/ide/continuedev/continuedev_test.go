package continuedev

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
	if a.ID() != "continue-dev" {
		t.Errorf("ID=%q", a.ID())
	}
}

func TestAdapter_Configure(t *testing.T) {
	a := &Adapter{}
	if err := a.Configure(nil); err != nil {
		t.Errorf("Configure(nil)=%v", err)
	}
	if err := a.Configure(map[string]any{"ignored": "value"}); err != nil {
		t.Errorf("Configure(map)=%v", err)
	}
}

func TestExtractRequest_Messages(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"refactor"}],"model":"hub-llama-3"}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "refactor" {
		t.Errorf("Segments=%v", nc.Segments)
	}
	if nc.Metadata["model"] != "hub-llama-3" {
		t.Errorf("model=%q", nc.Metadata["model"])
	}
}

func TestExtractRequest_MessagesWithToolCalls(t *testing.T) {
	body := []byte(`{
		"messages":[
			{"role":"user","content":"call something"},
			{"role":"assistant","content":null,"tool_calls":[
				{"id":"c1","type":"function","function":{"name":"do_thing","arguments":"{}"}}
			]}
		],
		"model":"hub-router-1"
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/v1/chat/completions")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "call something" {
		t.Errorf("Segments=%v", nc.Segments)
	}
	if len(nc.ToolCallSegments) != 1 || !strings.Contains(nc.ToolCallSegments[0], `"do_thing"`) {
		t.Errorf("ToolCallSegments=%v", nc.ToolCallSegments)
	}
}

func TestExtractRequest_Prompt(t *testing.T) {
	body := []byte(`{"prompt":"summarise this"}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "summarise this" {
		t.Errorf("Segments=%v", nc.Segments)
	}
	// `prompt` is consumed; `model` absent so Metadata should be empty map.
	if nc.Metadata == nil {
		t.Errorf("Metadata=nil; want empty map")
	}
	if _, ok := nc.Metadata["model"]; ok {
		t.Errorf("Metadata.model unexpectedly set")
	}
}

func TestExtractRequest_AllPromptLikeFields(t *testing.T) {
	// Exercise every entry in the prompt/query/text/input loop so we know
	// all four code paths add segments — not just `prompt`.
	body := []byte(`{
		"prompt":"P",
		"query":"Q",
		"text":"T",
		"input":"I"
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	got := strings.Join(nc.Segments, ",")
	if got != "P,Q,T,I" {
		t.Errorf("Segments=%q want \"P,Q,T,I\"", got)
	}
}

func TestExtractRequest_EmptyPromptStringIgnored(t *testing.T) {
	// Empty `prompt` should not produce a segment — covers the `v.Str != ""`
	// branch. Need another signal field so we don't fall through to
	// ErrUnknownSchema; use a model field plus a messages array.
	body := []byte(`{"prompt":"","messages":[{"role":"user","content":"real"}]}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "real" {
		t.Errorf("Segments=%v; empty prompt must not contribute", nc.Segments)
	}
}

func TestExtractRequest_ToolCallsOnly(t *testing.T) {
	// Pure tool-call message (no string content anywhere) — exercises the
	// `len(segments) == 0 && len(toolCalls) > 0` non-error path.
	body := []byte(`{"messages":[
		{"role":"assistant","content":null,"tool_calls":[
			{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}
		]}
	]}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("Segments=%v want empty", nc.Segments)
	}
	if len(nc.ToolCallSegments) != 1 {
		t.Errorf("ToolCallSegments=%v", nc.ToolCallSegments)
	}
}

func TestExtractRequest_ExtraSurfacesUnknownKeys(t *testing.T) {
	// `session_id` is in the requestKnownKeys list, so it's filtered out
	// of Extra. `telemetry_id` is unknown → must surface in Extra.
	body := []byte(`{"prompt":"hi","telemetry_id":"abc-123","session_id":"sess-1"}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if v, ok := nc.Extra["telemetry_id"]; !ok || !strings.Contains(v, "abc-123") {
		t.Errorf("Extra=%v missing telemetry_id", nc.Extra)
	}
	if _, ok := nc.Extra["session_id"]; ok {
		t.Errorf("Extra=%v leaked known key session_id", nc.Extra)
	}
}

func TestExtractRequest_EmptyBody(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), nil, "/api/x")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

func TestExtractRequest_BinaryBody(t *testing.T) {
	// Protobuf-shaped payload — not JSON.
	body := []byte{0x00, 0x00, 0x00, 0x05, 0x68, 0x65, 0x6c, 0x6c, 0x6f}
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/grpc.Service/Method")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
	if _, ok := nc.Extra["binary_preview"]; !ok {
		t.Errorf("Extra=%v missing binary_preview", nc.Extra)
	}
}

func TestExtractRequest_MalformedJSON(t *testing.T) {
	// Looks-like-JSON (starts with `{`) but invalid → ErrMalformed.
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), []byte(`{not valid`), "/api/x")
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("err=%v want ErrMalformed", err)
	}
}

func TestExtractRequest_JSONWithoutKnownFields(t *testing.T) {
	// Valid JSON object that doesn't include any known prompt-bearing key,
	// no messages, no tool calls — ErrUnknownSchema with Extra surfacing
	// the unknown keys (covers the second ErrUnknownSchema return path).
	body := []byte(`{"foo":"bar","baz":42}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/x")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
	if _, ok := nc.Extra["foo"]; !ok {
		t.Errorf("Extra=%v missing foo", nc.Extra)
	}
	if _, ok := nc.Extra["baz"]; !ok {
		t.Errorf("Extra=%v missing baz", nc.Extra)
	}
}

func TestExtractRequest_MessageContentNonString(t *testing.T) {
	// `content` as an array (OpenAI content-parts shape) shouldn't crash
	// or add a segment, since this adapter only handles string content.
	// Pair with a sibling user message whose content IS a string so we
	// reach a successful, non-error return.
	body := []byte(`{"messages":[
		{"role":"user","content":[{"type":"text","text":"parts"}]},
		{"role":"user","content":"plain"}
	]}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "plain" {
		t.Errorf("Segments=%v; non-string content must be skipped", nc.Segments)
	}
}

func TestExtractResponse_EmptyBody(t *testing.T) {
	// Empty response body → zero NormalizedContent + nil error.
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), nil, "/api/x")
	if err != nil {
		t.Errorf("err=%v want nil", err)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestExtractResponse_BinaryBody(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), []byte{0x00, 0x42}, "/api/x")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

func TestExtractResponse_MalformedJSON(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), []byte(`{bad`), "/api/x")
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("err=%v want ErrMalformed", err)
	}
}

func TestExtractResponse_ErrorMessageEnvelope(t *testing.T) {
	body := []byte(`{"error":{"code":"FORBIDDEN","message":"hub denied"}}`)
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), body, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "hub denied" {
		t.Errorf("Segments=%v", nc.Segments)
	}
	if nc.Metadata["error"] != "true" {
		t.Errorf("Metadata.error=%q want true", nc.Metadata["error"])
	}
}

func TestExtractResponse_TopLevelMessage(t *testing.T) {
	// `message` at the top level (no `error` envelope) is also surfaced
	// as an error-tagged segment — covers the second branch.
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
		t.Errorf("Metadata.error=%q want true", nc.Metadata["error"])
	}
}

func TestExtractResponse_UnknownShape(t *testing.T) {
	// No `error.message`, no top-level `message` → ErrUnknownSchema.
	body := []byte(`{"status":"ok","result":[]}`)
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), body, "/api/x")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

func TestExtractResponse_EmptyErrorMessage(t *testing.T) {
	// `error.message` exists but is empty → falls through to second branch
	// (also empty `message`) → ErrUnknownSchema.
	body := []byte(`{"error":{"code":"X","message":""}}`)
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), body, "/api/x")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

func TestExtractStreamChunk_OpenAIDeltaContent(t *testing.T) {
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

func TestExtractStreamChunk_OpenAIDeltaToolCalls(t *testing.T) {
	chunk := []byte(`{"choices":[{"delta":{"tool_calls":[
		{"index":0,"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}
	]}}]}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/api/chat/stream")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("Segments=%v want empty", nc.Segments)
	}
	if len(nc.ToolCallSegments) != 1 || !strings.Contains(nc.ToolCallSegments[0], `"f"`) {
		t.Errorf("ToolCallSegments=%v", nc.ToolCallSegments)
	}
}

func TestExtractStreamChunk_DeltaPresentButEmpty(t *testing.T) {
	// Choices/delta present but no content/tool_calls (final "stop" chunk).
	chunk := []byte(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/api/chat/stream")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 0 || len(nc.ToolCallSegments) != 0 {
		t.Errorf("got Segments=%v ToolCallSegments=%v want both empty", nc.Segments, nc.ToolCallSegments)
	}
}

func TestExtractStreamChunk_EmptyContentStringSkipped(t *testing.T) {
	// `content` is empty string — must not append a segment (covers the
	// `c.Str != ""` branch).
	chunk := []byte(`{"choices":[{"delta":{"content":""}}]}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/api/chat/stream")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("Segments=%v; empty content must not contribute", nc.Segments)
	}
}

func TestExtractStreamChunk_NoDelta(t *testing.T) {
	// JSON without choices[0].delta — exits silently with zero content.
	chunk := []byte(`{"id":"chatcmpl-1","object":"chat.completion.chunk"}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/api/chat/stream")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 0 || len(nc.ToolCallSegments) != 0 {
		t.Errorf("got Segments=%v ToolCallSegments=%v", nc.Segments, nc.ToolCallSegments)
	}
}

func TestExtractStreamChunk_EmptyOrBinary(t *testing.T) {
	a := &Adapter{}
	// Empty chunk.
	if nc, err := a.ExtractStreamChunk(context.Background(), nil, "/api/x"); err != nil || len(nc.Segments) != 0 {
		t.Errorf("empty chunk: err=%v segs=%v", err, nc.Segments)
	}
	// Binary chunk.
	if nc, err := a.ExtractStreamChunk(context.Background(), []byte{0x00, 0xff}, "/api/x"); err != nil || len(nc.Segments) != 0 {
		t.Errorf("binary chunk: err=%v segs=%v", err, nc.Segments)
	}
	// Invalid JSON chunk.
	if nc, err := a.ExtractStreamChunk(context.Background(), []byte(`{bad`), "/api/x"); err != nil || len(nc.Segments) != 0 {
		t.Errorf("invalid chunk: err=%v segs=%v", err, nc.Segments)
	}
}

// DetectRequestMeta + DetectResponseUsage

func TestDetectRequestMeta_ModelExtracted(t *testing.T) {
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost, "https://hub.continue.dev/api/x", nil)
	meta := a.DetectRequestMeta(r, []byte(`{"model":"hub-model"}`))
	if meta.Provider != "continue-dev" {
		t.Errorf("Provider=%q", meta.Provider)
	}
	if meta.Model != "hub-model" {
		t.Errorf("Model=%q", meta.Model)
	}
}

func TestDetectRequestMeta_NoModel(t *testing.T) {
	// Valid JSON but `model` not a string → Model stays empty.
	a := &Adapter{}
	meta := a.DetectRequestMeta(nil, []byte(`{"foo":"bar"}`))
	if meta.Provider != "continue-dev" {
		t.Errorf("Provider=%q", meta.Provider)
	}
	if meta.Model != "" {
		t.Errorf("Model=%q want empty", meta.Model)
	}
}

func TestDetectRequestMeta_InvalidJSON(t *testing.T) {
	// Invalid body → Provider still set, Model empty (covers ValidBytes false branch).
	a := &Adapter{}
	meta := a.DetectRequestMeta(nil, []byte(`not json`))
	if meta.Provider != "continue-dev" {
		t.Errorf("Provider=%q", meta.Provider)
	}
	if meta.Model != "" {
		t.Errorf("Model=%q want empty", meta.Model)
	}
}

func TestDetectResponseUsage_AlwaysNonLLM(t *testing.T) {
	// Adapter doesn't decode token usage — must always return non_llm.
	a := &Adapter{}
	if a.DetectResponseUsage(nil, nil).Status != traffic.UsageStatusNonLLM {
		t.Errorf("nil body: want non_llm")
	}
	if a.DetectResponseUsage(nil, []byte(`{"usage":{"total_tokens":123}}`)).Status != traffic.UsageStatusNonLLM {
		t.Errorf("with usage body: want non_llm (adapter is upload-only)")
	}
}

// Rewrite contracts (rewrites are unsupported — adapter is read-only)

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
		t.Errorf("body mutated: in=%q out=%q", body, out)
	}
}

func TestRewriteResponseBody_Unsupported(t *testing.T) {
	a := &Adapter{}
	body := []byte(`{"ok":true}`)
	out, n, err := a.RewriteResponseBody(context.Background(), body, "/api/x", traffic.NormalizedContent{})
	if !errors.Is(err, traffic.ErrRewriteUnsupported) {
		t.Errorf("err=%v want ErrRewriteUnsupported", err)
	}
	if n != 0 {
		t.Errorf("n=%d want 0", n)
	}
	if string(out) != string(body) {
		t.Errorf("body mutated: in=%q out=%q", body, out)
	}
}

// looksLikeJSON + preview helpers

func TestLooksLikeJSON(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want bool
	}{
		{"object", []byte(`{"a":1}`), true},
		{"array", []byte(`[1,2,3]`), true},
		{"object with leading whitespace", []byte("\n\t  {\"a\":1}"), true},
		{"array with leading CR", []byte("\r[1]"), true},
		{"string-only", []byte(`"hello"`), false},
		{"number-only", []byte(`42`), false},
		{"binary leading byte", []byte{0x00, '{'}, false},
		{"whitespace only", []byte(" \t\n\r"), false},
		{"empty", []byte(``), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := looksLikeJSON(tc.in); got != tc.want {
				t.Errorf("looksLikeJSON(%q)=%v want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestPreview_TruncatesAt256(t *testing.T) {
	body := make([]byte, 1024)
	for i := range body {
		body[i] = 'a'
	}
	p := preview(body)
	if len(p) != 256 {
		t.Errorf("len(preview)=%d want 256", len(p))
	}
}

func TestPreview_ShortBodyUntouchedExceptControlChars(t *testing.T) {
	// Length < 256: no truncation. Control bytes < 0x20 (except \n/\t) and
	// high bytes > 0x7e get mapped to '.'.
	body := []byte{'a', 0x00, 'b', '\n', 'c', 0xff, 'd', '\t', 'e', 0x1f, 'f'}
	p := preview(body)
	want := "a.b\nc.d\te.f"
	if p != want {
		t.Errorf("preview=%q want %q", p, want)
	}
}

func TestPreview_PreservesPrintableASCII(t *testing.T) {
	body := []byte("Hello, world!")
	if p := preview(body); p != "Hello, world!" {
		t.Errorf("preview=%q want unchanged", p)
	}
}

// Normalize (normalize.go) — delegates to extract.NormalizeForAdapter

func TestNormalize_OpenAIChatRequest(t *testing.T) {
	body := []byte(`{
		"model":"hub-llama-3",
		"messages":[{"role":"user","content":"hello hub"}]
	}`)
	a := &Adapter{}
	payload, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType:  adapterID,
		Direction:    normalize.DirectionRequest,
		ContentType:  "application/json",
		EndpointPath: "/v1/chat/completions",
	})
	if err != nil {
		t.Fatalf("Normalize err=%v", err)
	}
	if payload.Kind != normalize.KindAIChat {
		t.Errorf("Kind=%v want ai-chat", payload.Kind)
	}
	if payload.DetectedSpec != adapterID {
		t.Errorf("DetectedSpec=%q want %q", payload.DetectedSpec, adapterID)
	}
	if payload.Model != "hub-llama-3" {
		t.Errorf("Model=%q", payload.Model)
	}
	if len(payload.Messages) != 1 || payload.Messages[0].Role != normalize.RoleUser {
		t.Errorf("Messages=%+v", payload.Messages)
	}
}

func TestNormalize_NonChatBodyErrors(t *testing.T) {
	// Body doesn't match openai-chat shape — Tier-1 fall-through error so
	// the upstream coordinator advances to Tier 2 / 3.
	body := []byte(`{"foo":"bar","count":42}`)
	a := &Adapter{}
	_, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType: adapterID,
		Direction:   normalize.DirectionRequest,
		ContentType: "application/json",
	})
	if err == nil {
		t.Fatal("expected error for non-chat body")
	}
}

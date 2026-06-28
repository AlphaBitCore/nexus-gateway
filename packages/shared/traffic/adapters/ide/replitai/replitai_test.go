package replitai

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
	if a.ID() != "replit-ai" {
		t.Errorf("ID=%q want replit-ai", a.ID())
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

// TestExtractRequest_Messages pins the openai-chat-like shape used by
// Replit Agent: `messages` array with string content. `repl_id` is
// surfaced into metadata.
func TestExtractRequest_Messages(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"build a tic-tac-toe game"}],"repl_id":"r-1","model":"replit-code-v1.5"}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/ai/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "build a tic-tac-toe game" {
		t.Errorf("Segments=%v", nc.Segments)
	}
	if nc.Metadata["repl_id"] != "r-1" {
		t.Errorf("repl_id=%q want r-1", nc.Metadata["repl_id"])
	}
	if nc.Metadata["model"] != "replit-code-v1.5" {
		t.Errorf("model=%q", nc.Metadata["model"])
	}
}

// TestExtractRequest_MessagesWithToolCalls pins that messages[].tool_calls
// land verbatim in ToolCallSegments so the compliance pipeline can
// inspect tool-use payloads.
func TestExtractRequest_MessagesWithToolCalls(t *testing.T) {
	body := []byte(`{
		"messages":[
			{"role":"user","content":"call something"},
			{"role":"assistant","content":null,"tool_calls":[
				{"id":"call_a","type":"function","function":{"name":"run_shell","arguments":"{\"cmd\":\"ls\"}"}},
				{"id":"call_b","type":"function","function":{"name":"write_file","arguments":"{}"}}
			]}
		],
		"model":"replit-router-1"
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/ai/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "call something" {
		t.Errorf("Segments=%v", nc.Segments)
	}
	if len(nc.ToolCallSegments) != 2 {
		t.Fatalf("ToolCallSegments len=%d want 2", len(nc.ToolCallSegments))
	}
	if !strings.Contains(nc.ToolCallSegments[0], "run_shell") {
		t.Errorf("ToolCallSegments[0]=%q missing tool name", nc.ToolCallSegments[0])
	}
	if !strings.Contains(nc.ToolCallSegments[1], "write_file") {
		t.Errorf("ToolCallSegments[1]=%q missing tool name", nc.ToolCallSegments[1])
	}
}

// TestExtractRequest_ToolCallsOnlyNoSegments pins that a frame whose
// only audit content is tool_calls still returns a populated payload
// (not ErrUnknownSchema). The adapter checks
// `len(segments)==0 && len(toolCalls)==0` for the unknown branch.
func TestExtractRequest_ToolCallsOnlyNoSegments(t *testing.T) {
	body := []byte(`{
		"messages":[
			{"role":"assistant","tool_calls":[
				{"id":"call_a","function":{"name":"only_tool"}}
			]}
		]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/ai/x")
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

// TestExtractRequest_PromptAliases pins that every alias key (`prompt`,
// `query`, `text`, `input`) contributes a segment when present.
// Note: replitai does NOT include `inputs` in its alias loop.
func TestExtractRequest_PromptAliases(t *testing.T) {
	body := []byte(`{
		"prompt":"one",
		"query":"two",
		"text":"three",
		"input":"four"
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/ai/x")
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

// TestExtractRequest_EmptyAliasesSkipped pins that an alias key with
// an empty value does NOT add a phantom segment.
func TestExtractRequest_EmptyAliasesSkipped(t *testing.T) {
	body := []byte(`{"prompt":"","query":"","text":"real","input":""}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/ai/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "real" {
		t.Errorf("Segments=%v want [real]", nc.Segments)
	}
}

// TestExtractRequest_NonStringContentSkipped pins that structured
// content (array/object) does not crash; only string content captured.
func TestExtractRequest_NonStringContentSkipped(t *testing.T) {
	body := []byte(`{
		"messages":[
			{"role":"user","content":[{"type":"text","text":"structured"}]},
			{"role":"user","content":"plain"}
		]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/ai/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "plain" {
		t.Errorf("Segments=%v want [plain]", nc.Segments)
	}
}

// TestExtractRequest_ModelMetaMissing pins that an absent `model`
// leaves the metadata map empty.
func TestExtractRequest_ModelMetaMissing(t *testing.T) {
	body := []byte(`{"prompt":"hi"}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/ai/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if _, ok := nc.Metadata["model"]; ok {
		t.Errorf("model key present: %v", nc.Metadata)
	}
}

// TestExtractRequest_ReplIDMetaMissing pins symmetry: `repl_id` only
// stamped when present and non-empty.
func TestExtractRequest_ReplIDMetaMissing(t *testing.T) {
	body := []byte(`{"prompt":"hi","repl_id":""}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/ai/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if _, ok := nc.Metadata["repl_id"]; ok {
		t.Errorf("repl_id key present: %v", nc.Metadata)
	}
}

func TestExtractRequest_Empty(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), nil, "/api/ai/x")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

// TestExtractRequest_BinaryBody pins that a non-JSON binary payload
// returns ErrUnknownSchema.
func TestExtractRequest_BinaryBody(t *testing.T) {
	body := []byte{0x00, 0x01, 0x7f, 0x80, 0xff, 'h', 'i', 0x05}
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), body, "/api/ai/x")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

// TestExtractRequest_Malformed pins ErrMalformed for body bytes that
// begin like JSON but are not parseable.
func TestExtractRequest_Malformed(t *testing.T) {
	body := []byte(`{"prompt":"unclosed`)
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), body, "/api/ai/x")
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("err=%v want ErrMalformed", err)
	}
}

// TestExtractRequest_UnknownJSON pins ErrUnknownSchema for valid JSON
// without recognised fields.
func TestExtractRequest_UnknownJSON(t *testing.T) {
	body := []byte(`{"foo":"bar","baz":42}`)
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), body, "/api/ai/x")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

func TestExtractResponse_EmptyBody(t *testing.T) {
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), nil, "/api/ai/x")
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
	_, err := a.ExtractResponse(context.Background(), []byte(`{not json`), "/api/ai/x")
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("err=%v want ErrMalformed", err)
	}
}

// TestExtractResponse_ErrorEnvelope pins the OpenAI-style
// `error.message` shape: surfaced as a segment with error metadata.
func TestExtractResponse_ErrorEnvelope(t *testing.T) {
	body := []byte(`{"error":{"message":"quota exceeded","type":"rate_limit","code":"quota_exceeded"}}`)
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), body, "/api/ai/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "quota exceeded" {
		t.Errorf("Segments=%v want [quota exceeded]", nc.Segments)
	}
	if nc.Metadata["error"] != "true" {
		t.Errorf("error metadata=%q want true", nc.Metadata["error"])
	}
}

// TestExtractResponse_UnknownJSON pins the fall-through: JSON without
// an error envelope returns ErrUnknownSchema.
func TestExtractResponse_UnknownJSON(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), []byte(`{"foo":"bar"}`), "/api/ai/x")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

// TestExtractResponse_EmptyErrorMessageFallsThrough pins that an
// envelope with `error` but empty message falls through to unknown
// rather than emitting an empty error segment.
func TestExtractResponse_EmptyErrorMessageFallsThrough(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), []byte(`{"error":{"message":""}}`), "/api/ai/x")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema (empty error message)", err)
	}
}

// TestExtractStreamChunk_OpenAICompat pins the openai-chat-SSE delta
// shape: `choices[0].delta.content` carries one token's text.
func TestExtractStreamChunk_OpenAICompat(t *testing.T) {
	chunk := []byte(`{"choices":[{"delta":{"content":"Hello"}}]}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/api/ai/stream")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "Hello" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

// TestExtractStreamChunk_DeltaToolCalls pins that streamed tool-use
// frames inside delta.tool_calls land in ToolCallSegments.
func TestExtractStreamChunk_DeltaToolCalls(t *testing.T) {
	chunk := []byte(`{"choices":[{"delta":{"tool_calls":[
		{"index":0,"id":"call_a","function":{"name":"run_shell"}},
		{"index":1,"id":"call_b","function":{"name":"write_file"}}
	]}}]}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/api/ai/stream")
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

// TestExtractStreamChunk_EmptyDeltaContent pins that delta.content=""
// does NOT produce an empty Segments entry.
func TestExtractStreamChunk_EmptyDeltaContent(t *testing.T) {
	chunk := []byte(`{"choices":[{"delta":{"content":""}}]}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/api/ai/stream")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("empty content leaked into Segments=%v", nc.Segments)
	}
}

// TestExtractStreamChunk_DeltaNotObject: a finish-only frame with
// delta absent — should yield no content but not error.
func TestExtractStreamChunk_DeltaNotObject(t *testing.T) {
	chunk := []byte(`{"choices":[{"finish_reason":"stop"}]}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/api/ai/stream")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("Segments leaked: %v", nc.Segments)
	}
}

// TestExtractStreamChunk_FallbackTopLevelText pins the no-openai-delta
// fallback: top-level `text` is captured by the alias scan.
func TestExtractStreamChunk_FallbackTopLevelText(t *testing.T) {
	chunk := []byte(`{"text":"streamed token"}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/api/ai/stream")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "streamed token" {
		t.Errorf("Segments=%v want [streamed token]", nc.Segments)
	}
}

// TestExtractStreamChunk_FallbackAllFields pins that the fallback loop
// captures `text`, `content`, and `delta` (the three known aliases)
// in order when openai-delta isn't present.
func TestExtractStreamChunk_FallbackAllFields(t *testing.T) {
	chunk := []byte(`{"text":"A","content":"B","delta":"C"}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/api/ai/stream")
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
// empty strings in fallback fields do not add phantom segments.
func TestExtractStreamChunk_FallbackEmptyFieldsSkipped(t *testing.T) {
	chunk := []byte(`{"text":"","content":"keep","delta":""}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/api/ai/stream")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "keep" {
		t.Errorf("Segments=%v want [keep]", nc.Segments)
	}
}

// TestExtractStreamChunk_DefensiveOnNonJSON pins fail-open: non-JSON,
// invalid-JSON, empty/whitespace chunks all return a clean empty
// payload with no error.
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
		nc, err := a.ExtractStreamChunk(context.Background(), c, "/api/ai/stream")
		if err != nil {
			t.Errorf("case %d err=%v want nil (fail-open)", i, err)
		}
		if len(nc.Segments) != 0 || len(nc.ToolCallSegments) != 0 {
			t.Errorf("case %d non-empty content: %+v", i, nc)
		}
	}
}

// DetectRequestMeta + DetectResponseUsage

func TestDetectRequestMeta(t *testing.T) {
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost, "https://replit.com/api/ai/x", nil)
	meta := a.DetectRequestMeta(r, []byte(`{"model":"replit-code-v1.5"}`))
	if meta.Provider != "replit-ai" {
		t.Errorf("Provider=%q want replit-ai", meta.Provider)
	}
	// Model is not exposed by replit-ai's DetectRequestMeta.
	if meta.Model != "" {
		t.Errorf("Model=%q want empty (replit-ai does not expose model in DetectRequestMeta)", meta.Model)
	}
}

func TestDetectResponseUsage_NonLLMSentinel(t *testing.T) {
	a := &Adapter{}
	usage := a.DetectResponseUsage(nil, []byte(`{}`))
	if usage.Status != traffic.UsageStatusNonLLM {
		t.Errorf("Status=%q want non_llm", usage.Status)
	}
	if usage.PromptTokens != nil || usage.CompletionTokens != nil {
		t.Errorf("token pointers must be nil; got %+v", usage)
	}
}

// Rewrite contracts (must return ErrRewriteUnsupported)

func TestRewriteRequestBody_Unsupported(t *testing.T) {
	a := &Adapter{}
	body := []byte(`{"prompt":"hi"}`)
	out, n, err := a.RewriteRequestBody(context.Background(), body, "/api/ai/x", traffic.NormalizedContent{})
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
	body := []byte(`{"error":{"message":"x"}}`)
	out, n, err := a.RewriteResponseBody(context.Background(), body, "/api/ai/x", traffic.NormalizedContent{})
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

// TestNormalize_RequestChatShape pins that openai-chat-shaped request
// is accepted and stamped with DetectedSpec = "replit-ai".
func TestNormalize_RequestChatShape(t *testing.T) {
	body := []byte(`{
		"model":"replit-code-v1.5",
		"messages":[
			{"role":"system","content":"You are Replit Agent."},
			{"role":"user","content":"refactor"}
		]
	}`)
	a := &Adapter{}
	payload, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType:  "replit-ai",
		Direction:    normalize.DirectionRequest,
		ContentType:  "application/json",
		EndpointPath: "/api/ai/x",
	})
	if err != nil {
		t.Fatalf("Normalize err: %v", err)
	}
	if payload.Kind != normalize.KindAIChat {
		t.Errorf("Kind=%v want ai-chat", payload.Kind)
	}
	if payload.DetectedSpec != "replit-ai" {
		t.Errorf("DetectedSpec=%q want replit-ai", payload.DetectedSpec)
	}
	if payload.Model != "replit-code-v1.5" {
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
		"id":"chatcmpl-replit",
		"object":"chat.completion",
		"model":"replit-code-v1.5",
		"choices":[
			{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}
		],
		"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}
	}`)
	a := &Adapter{}
	payload, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType: "replit-ai",
		Direction:   normalize.DirectionResponse,
		ContentType: "application/json",
	})
	if err != nil {
		t.Fatalf("Normalize err: %v", err)
	}
	if payload.Kind != normalize.KindAIChat {
		t.Errorf("Kind=%v want ai-chat", payload.Kind)
	}
	if payload.DetectedSpec != "replit-ai" {
		t.Errorf("DetectedSpec=%q want replit-ai", payload.DetectedSpec)
	}
}

// TestNormalize_UnrecognisedShape_FallsThrough verifies a body matching
// neither spec returns ErrUnsupported so the Coordinator can fall
// through to Tier 2.
func TestNormalize_UnrecognisedShape_FallsThrough(t *testing.T) {
	body := []byte(`{"foo":"bar","baz":42}`)
	a := &Adapter{}
	_, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType: "replit-ai",
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

package deepseekweb

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
	if a.ID() != "deepseek-web" {
		t.Errorf("ID=%q want deepseek-web", a.ID())
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
	body := []byte(`{"messages":[{"role":"user","content":"explain quicksort"}],"model":"deepseek-chat"}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/v0/chat/completions")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "explain quicksort" {
		t.Errorf("Segments=%v", nc.Segments)
	}
	if nc.Metadata["model"] != "deepseek-chat" {
		t.Errorf("model=%q want deepseek-chat", nc.Metadata["model"])
	}
}

// TestExtractRequest_MessagesArray pins multi-turn capture: every
// non-empty content string must land in Segments in arrival order.
func TestExtractRequest_MessagesArray(t *testing.T) {
	body := []byte(`{
		"messages":[
			{"role":"user","content":"first turn"},
			{"role":"assistant","content":"reply"},
			{"role":"user","content":"follow-up"}
		],
		"model":"deepseek-chat"
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/x")
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

// TestExtractRequest_MultimodalContentArray pins the multimodal shape:
// content is an array of parts and only `type:text` parts contribute
// to Segments. Defence-in-depth for vision-style payloads.
func TestExtractRequest_MultimodalContentArray(t *testing.T) {
	body := []byte(`{
		"messages":[
			{"role":"user","content":[
				{"type":"text","text":"describe this"},
				{"type":"image_url","image_url":{"url":"https://example.com/x.png"}},
				{"type":"text","text":"in one sentence"}
			]}
		]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	want := []string{"describe this", "in one sentence"}
	if len(nc.Segments) != len(want) {
		t.Fatalf("Segments len=%d want %d: %v", len(nc.Segments), len(want), nc.Segments)
	}
	for i := range want {
		if nc.Segments[i] != want[i] {
			t.Errorf("Segments[%d]=%q want %q", i, nc.Segments[i], want[i])
		}
	}
}

// TestExtractRequest_ToolCallsInHistory pins assistant tool_calls in
// the request history land in ToolCallSegments verbatim.
func TestExtractRequest_ToolCallsInHistory(t *testing.T) {
	body := []byte(`{
		"messages":[
			{"role":"user","content":"weather"},
			{"role":"assistant","tool_calls":[
				{"id":"c1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"NYC\"}"}}
			]}
		]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.ToolCallSegments) != 1 || !strings.Contains(nc.ToolCallSegments[0], `"get_weather"`) {
		t.Errorf("ToolCallSegments=%v", nc.ToolCallSegments)
	}
}

func TestExtractRequest_PromptShape(t *testing.T) {
	body := []byte(`{"prompt":"explain quicksort","chat_id":"chat-1"}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "explain quicksort" {
		t.Errorf("Segments=%v", nc.Segments)
	}
	if nc.Metadata["chat_id"] != "chat-1" {
		t.Errorf("chat_id=%q", nc.Metadata["chat_id"])
	}
}

// TestExtractRequest_PromptAliases pins that every alias key
// (prompt, query, text, input) contributes a segment when present and
// non-empty.
func TestExtractRequest_PromptAliases(t *testing.T) {
	body := []byte(`{
		"prompt":"one",
		"query":"two",
		"text":"three",
		"input":"four"
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
// with an empty value does NOT contribute a phantom segment.
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

// TestExtractRequest_ReasoningContentInHistory pins DeepSeek's
// reasoner trace surfacing — the load-bearing distinction from a
// vanilla openai-chat adapter.
func TestExtractRequest_ReasoningContentInHistory(t *testing.T) {
	body := []byte(`{"messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":"answer","reasoning_content":"prior reasoning trace"}
	]}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.ReasoningSegments) != 1 || nc.ReasoningSegments[0] != "prior reasoning trace" {
		t.Errorf("ReasoningSegments=%v", nc.ReasoningSegments)
	}
}

// TestExtractRequest_ReasoningOnlyNoSegments pins that a body whose
// only audit content is reasoning_content (no segments, no tool_calls)
// is NOT classified as unknown — reasoning alone counts.
func TestExtractRequest_ReasoningOnlyNoSegments(t *testing.T) {
	body := []byte(`{"messages":[
		{"role":"assistant","reasoning_content":"chain of thought"}
	]}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/x")
	if err != nil {
		t.Fatalf("err=%v want nil (reasoning_content present)", err)
	}
	if len(nc.ReasoningSegments) != 1 {
		t.Errorf("ReasoningSegments=%v", nc.ReasoningSegments)
	}
}

// TestExtractRequest_ToolCallsOnlyNoSegments pins that a tool-only
// frame is recognised (not classified as unknown-schema).
func TestExtractRequest_ToolCallsOnlyNoSegments(t *testing.T) {
	body := []byte(`{
		"messages":[
			{"role":"assistant","tool_calls":[
				{"id":"c1","type":"function","function":{"name":"only_tool"}}
			]}
		]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/x")
	if err != nil {
		t.Fatalf("err=%v want nil (tool_calls present)", err)
	}
	if len(nc.ToolCallSegments) != 1 {
		t.Errorf("ToolCallSegments len=%d want 1", len(nc.ToolCallSegments))
	}
}

func TestExtractRequest_BinaryBody(t *testing.T) {
	a := &Adapter{}
	body := []byte{0x00, 0x01, 0x02, 0x7f, 0x80, 0xff, 'h', 'i'}
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
}

func TestExtractRequest_Empty(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), nil, "/api/x")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

// TestExtractRequest_Malformed pins ErrMalformed for body bytes that
// begin like JSON but fail to parse.
func TestExtractRequest_Malformed(t *testing.T) {
	a := &Adapter{}
	body := []byte(`{"prompt":"missing close-brace`)
	_, err := a.ExtractRequest(context.Background(), body, "/api/x")
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("err=%v want ErrMalformed", err)
	}
}

// TestExtractRequest_UnknownJSON pins ErrUnknownSchema for a valid
// JSON body without any recognised DeepSeek fields. Extra carries the
// unparsed keys for downstream hooks.
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
		t.Errorf("err=%v want nil (empty body benign)", err)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

// TestExtractResponse_NonJSONBody pins ErrUnknownSchema for a body
// whose first byte fails the JSON prefix check.
func TestExtractResponse_NonJSONBody(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), []byte(`not json at all`), "/api/x")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

// TestExtractResponse_Malformed pins ErrMalformed for body bytes that
// pass looksLikeJSON but fail gjson.ValidBytes.
func TestExtractResponse_Malformed(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), []byte(`{"oops":`), "/api/x")
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("err=%v want ErrMalformed", err)
	}
}

// TestExtractResponse_ErrorEnvelope pins the OpenAI-style
// `error.message` shape — the adapter exposes the message as a segment
// and stamps an error metadata flag.
func TestExtractResponse_ErrorEnvelope(t *testing.T) {
	body := []byte(`{"error":{"message":"rate limit exceeded","type":"rate_limit_exceeded"}}`)
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
// path. Also stamps the error metadata flag.
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
// without an error envelope nor a top-level message is unknown schema.
func TestExtractResponse_UnknownJSON(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), []byte(`{"foo":"bar"}`), "/api/x")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

func TestExtractStreamChunk_Content(t *testing.T) {
	chunk := []byte(`{"choices":[{"delta":{"content":"Hello"}}]}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/api/stream")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "Hello" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

// TestExtractStreamChunk_ReasoningContentDelta pins DeepSeek's
// reasoner stream: a delta with reasoning_content only must land in
// ReasoningSegments and NOT leak an empty Segment.
func TestExtractStreamChunk_ReasoningContentDelta(t *testing.T) {
	chunk := []byte(`{"choices":[{"delta":{"reasoning_content":"step 1","content":""}}]}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/api/stream")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.ReasoningSegments) != 1 || nc.ReasoningSegments[0] != "step 1" {
		t.Errorf("ReasoningSegments=%v", nc.ReasoningSegments)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("Segments=%v want empty for reasoning-only delta", nc.Segments)
	}
}

// TestExtractStreamChunk_ToolCallDelta pins streamed tool-use frames
// land in ToolCallSegments verbatim.
func TestExtractStreamChunk_ToolCallDelta(t *testing.T) {
	chunk := []byte(`{"choices":[{"delta":{"tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]}}]}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/api/stream")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.ToolCallSegments) != 1 || !strings.Contains(nc.ToolCallSegments[0], `"f"`) {
		t.Errorf("ToolCallSegments=%v", nc.ToolCallSegments)
	}
}

// TestExtractStreamChunk_TopLevelTextAndContent pins the alternate
// chunk shape where openai-style choices.0.delta is absent and the
// chunk carries top-level `text` and `content` fields.
func TestExtractStreamChunk_TopLevelTextAndContent(t *testing.T) {
	chunk := []byte(`{"text":"alpha","content":"beta"}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/api/stream")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	want := []string{"alpha", "beta"}
	if len(nc.Segments) != len(want) {
		t.Fatalf("Segments len=%d want %d: %v", len(nc.Segments), len(want), nc.Segments)
	}
	for i := range want {
		if nc.Segments[i] != want[i] {
			t.Errorf("Segments[%d]=%q want %q", i, nc.Segments[i], want[i])
		}
	}
}

// TestExtractStreamChunk_DefensiveOnNonJSON pins fail-open: non-JSON,
// malformed-JSON, marker bytes, and whitespace-only chunks return a
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
		if len(nc.Segments) != 0 || len(nc.ToolCallSegments) != 0 || len(nc.ReasoningSegments) != 0 {
			t.Errorf("case %d non-empty content: %+v", i, nc)
		}
	}
}

// DetectRequestMeta + DetectResponseUsage

func TestDetectRequestMeta(t *testing.T) {
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost, "https://chat.deepseek.com/api/x", nil)
	meta := a.DetectRequestMeta(r, []byte(`{"model":"deepseek-reasoner"}`))
	if meta.Provider != "deepseek-web" {
		t.Errorf("Provider=%q", meta.Provider)
	}
	if meta.Model != "deepseek-reasoner" {
		t.Errorf("Model=%q", meta.Model)
	}
}

// TestDetectRequestMeta_InvalidJSON pins fail-open: a garbage body must
// not panic; Provider stays set, Model stays empty.
func TestDetectRequestMeta_InvalidJSON(t *testing.T) {
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost, "https://chat.deepseek.com/api/x", nil)
	meta := a.DetectRequestMeta(r, []byte(`not json`))
	if meta.Provider != "deepseek-web" {
		t.Errorf("Provider=%q", meta.Provider)
	}
	if meta.Model != "" {
		t.Errorf("Model=%q want empty for invalid body", meta.Model)
	}
}

// TestDetectResponseUsage_NonLLMSentinel pins the non-LLM marker.
// DeepSeek's consumer web wire format is undocumented so token stats
// are not extractable.
func TestDetectResponseUsage_NonLLMSentinel(t *testing.T) {
	a := &Adapter{}
	usage := a.DetectResponseUsage(nil, []byte(`{}`))
	if usage.Status != traffic.UsageStatusNonLLM {
		t.Errorf("Status=%q want non_llm", usage.Status)
	}
}

// Rewrite contracts (must return ErrRewriteUnsupported)

func TestRewrite_Unsupported(t *testing.T) {
	a := &Adapter{}
	body := []byte(`{}`)
	out, n, err := a.RewriteRequestBody(context.Background(), body, "/x", traffic.NormalizedContent{})
	if !errors.Is(err, traffic.ErrRewriteUnsupported) {
		t.Errorf("Request rewrite err=%v", err)
	}
	if n != 0 {
		t.Errorf("n=%d", n)
	}
	if string(out) != string(body) {
		t.Errorf("body must be returned unchanged on rewrite-unsupported")
	}

	out, n, err = a.RewriteResponseBody(context.Background(), body, "/x", traffic.NormalizedContent{})
	if !errors.Is(err, traffic.ErrRewriteUnsupported) {
		t.Errorf("Response rewrite err=%v", err)
	}
	if n != 0 {
		t.Errorf("n=%d", n)
	}
	if string(out) != string(body) {
		t.Errorf("body must be returned unchanged on rewrite-unsupported")
	}
}

// Normalize (Tier-1 spec dispatch)

// TestNormalize_RequestChatShape pins openai-chat shape recognition
// with DetectedSpec = "deepseek-web".
func TestNormalize_RequestChatShape(t *testing.T) {
	body := []byte(`{
		"model":"deepseek-chat",
		"messages":[
			{"role":"system","content":"You are DeepSeek."},
			{"role":"user","content":"explain quicksort"}
		]
	}`)
	a := &Adapter{}
	payload, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType:  "deepseek-web",
		Direction:    normalize.DirectionRequest,
		ContentType:  "application/json",
		EndpointPath: "/api/v0/chat/completions",
	})
	if err != nil {
		t.Fatalf("Normalize err: %v", err)
	}
	if payload.Kind != normalize.KindAIChat {
		t.Errorf("Kind=%v want ai-chat", payload.Kind)
	}
	if payload.DetectedSpec != "deepseek-web" {
		t.Errorf("DetectedSpec=%q want deepseek-web", payload.DetectedSpec)
	}
	if payload.Confidence < 0.5 {
		t.Errorf("Confidence=%v want >= 0.5", payload.Confidence)
	}
}

// TestNormalize_ResponseNonStream pins response-side scoring against
// the openai-chat-nonstream spec.
func TestNormalize_ResponseNonStream(t *testing.T) {
	body := []byte(`{
		"id":"chatcmpl-deepseek",
		"object":"chat.completion",
		"model":"deepseek-chat",
		"choices":[
			{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}
		],
		"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}
	}`)
	a := &Adapter{}
	payload, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType: "deepseek-web",
		Direction:   normalize.DirectionResponse,
		ContentType: "application/json",
	})
	if err != nil {
		t.Fatalf("Normalize err: %v", err)
	}
	if payload.Kind != normalize.KindAIChat {
		t.Errorf("Kind=%v want ai-chat", payload.Kind)
	}
	if payload.DetectedSpec != "deepseek-web" {
		t.Errorf("DetectedSpec=%q want deepseek-web", payload.DetectedSpec)
	}
}

// TestNormalize_UnrecognisedShape_FallsThrough returns ErrUnsupported
// so the Coordinator can fall through to Tier 2 / Tier 3.
func TestNormalize_UnrecognisedShape_FallsThrough(t *testing.T) {
	body := []byte(`{"foo":"bar","baz":42}`)
	a := &Adapter{}
	_, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType: "deepseek-web",
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
// bytes < 0x20 (except \n and \t) become '.', bytes > 0x7e become '.',
// printable ASCII passes through, inputs longer than 256 bytes are
// truncated to 256.
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

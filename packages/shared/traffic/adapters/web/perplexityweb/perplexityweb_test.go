package perplexityweb

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
	if a.ID() != "perplexity-web" {
		t.Errorf("ID=%q", a.ID())
	}
}

func TestAdapter_Configure(t *testing.T) {
	a := &Adapter{}
	if err := a.Configure(nil); err != nil {
		t.Errorf("Configure(nil)=%v", err)
	}
}

func TestExtractRequest_QueryShape(t *testing.T) {
	body := []byte(`{"query":"how does photosynthesis work","search_focus":"academic"}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/perplexity_ask")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "how does photosynthesis work" {
		t.Errorf("Segments=%v", nc.Segments)
	}
	if nc.Metadata["search_focus"] != "academic" {
		t.Errorf("search_focus=%q", nc.Metadata["search_focus"])
	}
}

func TestExtractRequest_OpenAICompatMessages(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"explain entropy"}],"model":"sonar"}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/chat/completions")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "explain entropy" {
		t.Errorf("Segments=%v", nc.Segments)
	}
	if nc.Metadata["model"] != "sonar" {
		t.Errorf("model=%q", nc.Metadata["model"])
	}
}

func TestExtractRequest_ToolCalls(t *testing.T) {
	body := []byte(`{"messages":[{"role":"assistant","content":null,"tool_calls":[
		{"id":"c1","type":"function","function":{"name":"search","arguments":"{}"}}
	]}]}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.ToolCallSegments) != 1 || !strings.Contains(nc.ToolCallSegments[0], `"search"`) {
		t.Errorf("ToolCallSegments=%v", nc.ToolCallSegments)
	}
}

func TestExtractRequest_BinaryBody(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), []byte{0x00, 0x42}, "/api/x")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v", err)
	}
}

func TestExtractRequest_Empty(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), nil, "/api/x")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v", err)
	}
}

func TestExtractRequest_Malformed(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), []byte(`{not valid`), "/api/x")
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("err=%v", err)
	}
}

func TestExtractStreamChunk_OpenAICompat(t *testing.T) {
	chunk := []byte(`{"choices":[{"delta":{"content":"answer"}}]}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/api/stream")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "answer" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestExtractStreamChunk_AnswerField(t *testing.T) {
	chunk := []byte(`{"answer":"streamed answer chunk"}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/api/stream")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "streamed answer chunk" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestExtractStreamChunk_ChunksArray(t *testing.T) {
	chunk := []byte(`{"chunks":[{"text":"part1"},{"text":"part2"}]}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/api/stream")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 2 || nc.Segments[0] != "part1" || nc.Segments[1] != "part2" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestExtractStreamChunk_BinaryFrame(t *testing.T) {
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), []byte{0x00, 0x42}, "/api/x")
	if err != nil {
		t.Errorf("err=%v want nil", err)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestExtractResponse_ErrorEnvelope(t *testing.T) {
	body := []byte(`{"error":{"message":"unauthorised"}}`)
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), body, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "unauthorised" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestExtractResponse_DetailString(t *testing.T) {
	body := []byte(`{"detail":"forbidden"}`)
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), body, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "forbidden" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestDetectRequestMeta(t *testing.T) {
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost, "https://www.perplexity.ai/api/chat", nil)
	meta := a.DetectRequestMeta(r, []byte(`{"model":"sonar-large"}`))
	if meta.Provider != "perplexity-web" {
		t.Errorf("Provider=%q", meta.Provider)
	}
	if meta.Model != "sonar-large" {
		t.Errorf("Model=%q", meta.Model)
	}
}

func TestDetectResponseUsage_NonLLM(t *testing.T) {
	a := &Adapter{}
	if a.DetectResponseUsage(nil, []byte(`{}`)).Status != traffic.UsageStatusNonLLM {
		t.Errorf("want non_llm")
	}
}

func TestRewrite_Unsupported(t *testing.T) {
	a := &Adapter{}
	body := []byte(`{}`)
	if _, _, err := a.RewriteRequestBody(context.Background(), body, "/x", traffic.NormalizedContent{}); !errors.Is(err, traffic.ErrRewriteUnsupported) {
		t.Errorf("Request rewrite err=%v", err)
	}
	if _, _, err := a.RewriteResponseBody(context.Background(), body, "/x", traffic.NormalizedContent{}); !errors.Is(err, traffic.ErrRewriteUnsupported) {
		t.Errorf("Response rewrite err=%v", err)
	}
}

// TestExtractRequest_ContentArrayParts pins OpenAI-vision-style
// content-as-array shape: each text part is flattened into Segments.
// Also exercises conversation_id metadata stamping.
func TestExtractRequest_ContentArrayParts(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":[
		{"type":"text","text":"first"},
		{"type":"image_url","image_url":{"url":"https://x/y.png"}},
		{"type":"text","text":"second"}
	]}],"model":"sonar-large","conversation_id":"conv-x"}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/chat")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	want := []string{"first", "second"}
	if len(nc.Segments) != len(want) {
		t.Fatalf("Segments=%v want %v", nc.Segments, want)
	}
	for i := range want {
		if nc.Segments[i] != want[i] {
			t.Errorf("Segments[%d]=%q want %q", i, nc.Segments[i], want[i])
		}
	}
	if nc.Metadata["conversation_id"] != "conv-x" {
		t.Errorf("conversation_id=%q", nc.Metadata["conversation_id"])
	}
	if nc.Metadata["model"] != "sonar-large" {
		t.Errorf("model=%q", nc.Metadata["model"])
	}
}

// TestExtractRequest_JSONNoKnownFields pins valid JSON with no content.
func TestExtractRequest_JSONNoKnownFields(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), []byte(`{"foo":"bar"}`), "/api/x")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

// TestExtractResponse_Empty pins benign empty body.
func TestExtractResponse_Empty(t *testing.T) {
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), nil, "/api/x")
	if err != nil {
		t.Errorf("err=%v want nil", err)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

// TestExtractResponse_BinaryPayload pins non-JSON-prefix returns ErrUnknownSchema.
func TestExtractResponse_BinaryPayload(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), []byte{0x00, 0xff, 'x'}, "/api/x")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

// TestExtractResponse_Malformed pins JSON-prefix that fails gjson parse.
func TestExtractResponse_Malformed(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), []byte(`{"oops":`), "/api/x")
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("err=%v want ErrMalformed", err)
	}
}

// TestExtractResponse_MessageString pins the bare `message` envelope.
func TestExtractResponse_MessageString(t *testing.T) {
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), []byte(`{"message":"not found"}`), "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "not found" {
		t.Errorf("Segments=%v", nc.Segments)
	}
	if nc.Metadata["error"] != "true" {
		t.Errorf("error metadata=%q", nc.Metadata["error"])
	}
}

// TestExtractResponse_UnknownJSON pins fallthrough.
func TestExtractResponse_UnknownJSON(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), []byte(`{"foo":"bar"}`), "/api/x")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

// TestExtractStreamChunk_Empty pins empty chunk.
func TestExtractStreamChunk_Empty(t *testing.T) {
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), nil, "/api/stream")
	if err != nil {
		t.Errorf("err=%v", err)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

// TestExtractStreamChunk_Malformed pins JSON-prefix invalid JSON.
func TestExtractStreamChunk_Malformed(t *testing.T) {
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), []byte(`{"oops":`), "/api/x")
	if err != nil {
		t.Errorf("err=%v want nil", err)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

// TestExtractStreamChunk_ContentField pins the top-level `content` field.
func TestExtractStreamChunk_ContentField(t *testing.T) {
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), []byte(`{"content":"piece"}`), "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "piece" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

// TestExtractStreamChunk_TextField pins the top-level `text` field.
func TestExtractStreamChunk_TextField(t *testing.T) {
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), []byte(`{"text":"chunk"}`), "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "chunk" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

// TestExtractStreamChunk_DeltaToolCalls pins streamed tool_calls in delta.
func TestExtractStreamChunk_DeltaToolCalls(t *testing.T) {
	chunk := []byte(`{"choices":[{"delta":{"tool_calls":[
		{"index":0,"id":"call_a","function":{"name":"web_search"}}
	]}}]}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/api/stream")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.ToolCallSegments) != 1 || !strings.Contains(nc.ToolCallSegments[0], "web_search") {
		t.Errorf("ToolCallSegments=%v", nc.ToolCallSegments)
	}
}

// TestDetectRequestMeta_InvalidJSON pins early-return when body not JSON.
func TestDetectRequestMeta_InvalidJSON(t *testing.T) {
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost, "https://www.perplexity.ai/api/chat", nil)
	meta := a.DetectRequestMeta(r, []byte(`not json`))
	if meta.Provider != "perplexity-web" {
		t.Errorf("Provider=%q", meta.Provider)
	}
	if meta.Model != "" {
		t.Errorf("Model=%q want empty", meta.Model)
	}
}

// Normalize (Tier-1 spec dispatch)

func TestNormalize_RequestChatShape(t *testing.T) {
	body := []byte(`{
		"model": "sonar-large",
		"messages": [
			{"role": "system", "content": "You are a search assistant."},
			{"role": "user", "content": "what is photosynthesis"}
		]
	}`)
	a := &Adapter{}
	payload, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType:  "perplexity-web",
		Direction:    normalize.DirectionRequest,
		ContentType:  "application/json",
		EndpointPath: "/api/chat",
	})
	if err != nil {
		t.Fatalf("Normalize err: %v", err)
	}
	if payload.Kind != normalize.KindAIChat {
		t.Errorf("Kind=%v want ai-chat", payload.Kind)
	}
	if payload.DetectedSpec != "perplexity-web" {
		t.Errorf("DetectedSpec=%q want perplexity-web", payload.DetectedSpec)
	}
	if payload.Model != "sonar-large" {
		t.Errorf("Model=%q", payload.Model)
	}
}

func TestNormalize_ResponseNonStream(t *testing.T) {
	body := []byte(`{
		"id": "chatcmpl-p",
		"object": "chat.completion",
		"model": "sonar-large",
		"choices": [
			{"index":0,"message":{"role":"assistant","content":"answer"},"finish_reason":"stop"}
		],
		"usage": {"prompt_tokens": 3, "completion_tokens": 1, "total_tokens": 4}
	}`)
	a := &Adapter{}
	payload, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType: "perplexity-web",
		Direction:   normalize.DirectionResponse,
		ContentType: "application/json",
	})
	if err != nil {
		t.Fatalf("Normalize err: %v", err)
	}
	if payload.DetectedSpec != "perplexity-web" {
		t.Errorf("DetectedSpec=%q", payload.DetectedSpec)
	}
}

func TestNormalize_UnrecognisedShape_FallsThrough(t *testing.T) {
	a := &Adapter{}
	_, err := a.Normalize(context.Background(), []byte(`{"foo":"bar"}`), normalize.Meta{
		AdapterType: "perplexity-web",
		Direction:   normalize.DirectionRequest,
		ContentType: "application/json",
	})
	if !errors.Is(err, normalize.ErrUnsupported) {
		t.Errorf("err=%v want ErrUnsupported", err)
	}
}

// Internal helpers

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
		{"text-prefix", []byte(`hello`), false},
		{"control-byte-prefix", []byte{0x00, '{'}, false},
	}
	for _, c := range cases {
		if got := looksLikeJSON(c.in); got != c.want {
			t.Errorf("%s: looksLikeJSON(%q)=%v want %v", c.name, c.in, got, c.want)
		}
	}
}

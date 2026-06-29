package codeium

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
	if a.ID() != "codeium" {
		t.Errorf("ID=%q", a.ID())
	}
}

func TestAdapter_Configure(t *testing.T) {
	a := &Adapter{}
	if err := a.Configure(nil); err != nil {
		t.Errorf("Configure(nil)=%v", err)
	}
}

// JSON request shapes

func TestExtractRequest_OpenAICompatMessages(t *testing.T) {
	body := []byte(`{
		"model":"claude-3-5-sonnet",
		"messages":[
			{"role":"user","content":"explain this code block"}
		]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/exa.language_server_pb.LanguageServerService/GetCompletions")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "explain this code block" {
		t.Errorf("Segments=%v", nc.Segments)
	}
	if nc.Metadata["model"] != "claude-3-5-sonnet" {
		t.Errorf("model=%q", nc.Metadata["model"])
	}
}

func TestExtractRequest_OpenAICompatToolCalls(t *testing.T) {
	body := []byte(`{
		"messages":[
			{"role":"assistant","content":null,"tool_calls":[
				{"id":"c1","type":"function","function":{"name":"refactor","arguments":"{}"}}
			]}
		]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/chat")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.ToolCallSegments) != 1 || !strings.Contains(nc.ToolCallSegments[0], `"refactor"`) {
		t.Errorf("ToolCallSegments=%v", nc.ToolCallSegments)
	}
}

func TestExtractRequest_PromptField(t *testing.T) {
	body := []byte(`{"prompt":"complete this function","session_id":"s1"}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/completions")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "complete this function" {
		t.Errorf("Segments=%v", nc.Segments)
	}
	if nc.Metadata["session_id"] != "s1" {
		t.Errorf("session_id=%q", nc.Metadata["session_id"])
	}
}

func TestExtractRequest_AuthorTextShape(t *testing.T) {
	body := []byte(`{"messages":[{"author":"user","text":"hello windsurf"}]}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/chat")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "hello windsurf" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

// Defensive paths

func TestExtractRequest_BinaryProtobufBody(t *testing.T) {
	body := []byte{0x00, 0x00, 0x00, 0x00, 0x05, 0x68, 0x65, 0x6c, 0x6c, 0x6f}
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), body, "/exa.language_server_pb.LanguageServerService/GetCompletions")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

func TestExtractRequest_JSONWithoutKnownFields(t *testing.T) {
	body := []byte(`{"foo":"bar"}`)
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), body, "/api/x")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

func TestExtractRequest_EmptyBody(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), nil, "/api/x")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v", err)
	}
}

func TestExtractRequest_MalformedJSON(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), []byte(`{not valid`), "/api/x")
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("err=%v want ErrMalformed", err)
	}
}

func TestExtractStreamChunk_OpenAICompat(t *testing.T) {
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

func TestExtractStreamChunk_PlainText(t *testing.T) {
	chunk := []byte(`{"text":"streamed delta"}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/api/chat/stream")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "streamed delta" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestExtractStreamChunk_BinaryFrame(t *testing.T) {
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), []byte{0x00, 0x42}, "/api/chat/stream")
	if err != nil {
		t.Errorf("err=%v want nil", err)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestExtractStreamChunk_EmptyChunk(t *testing.T) {
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), nil, "/api/chat/stream")
	if err != nil {
		t.Errorf("err=%v", err)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestExtractResponse_ErrorEnvelope(t *testing.T) {
	body := []byte(`{"error":{"code":"PERMISSION","message":"forbidden"}}`)
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), body, "/api/chat")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "forbidden" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestExtractResponse_OpenAICompatHappyPath(t *testing.T) {
	body := []byte(`{"choices":[{"message":{"content":"refactor done","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]}}]}`)
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), body, "/api/chat")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "refactor done" {
		t.Errorf("Segments=%v", nc.Segments)
	}
	if len(nc.ToolCallSegments) != 1 {
		t.Errorf("ToolCallSegments=%v", nc.ToolCallSegments)
	}
}

func TestExtractResponse_ProtobufSkipped(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), []byte{0x00, 0x00}, "/api/chat")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

// DetectRequestMeta + DetectResponseUsage + Rewrite contracts

func TestDetectRequestMeta_BearerToken(t *testing.T) {
	body := []byte(`{"model":"claude-3-5-sonnet","prompt":"hi"}`)
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost, "https://server.codeium.com/exa.language_server_pb.LanguageServerService/GetCompletions", nil)
	r.Header.Set("Authorization", "Bearer codeium_service_key_xyz")
	meta := a.DetectRequestMeta(r, body)
	if meta.Provider != "codeium" {
		t.Errorf("Provider=%q", meta.Provider)
	}
	if meta.ApiKeyClass != "codeium-bearer" {
		t.Errorf("ApiKeyClass=%q", meta.ApiKeyClass)
	}
	if meta.ApiKeyFingerprint == "" {
		t.Errorf("ApiKeyFingerprint should be set")
	}
	if meta.Model != "claude-3-5-sonnet" {
		t.Errorf("Model=%q", meta.Model)
	}
}

func TestDetectResponseUsage_NonLLM(t *testing.T) {
	a := &Adapter{}
	if a.DetectResponseUsage(nil, []byte(`{}`)).Status != traffic.UsageStatusNonLLM {
		t.Errorf("want non_llm")
	}
}

func TestRewriteRequestBody_Unsupported(t *testing.T) {
	a := &Adapter{}
	body := []byte(`{}`)
	out, n, err := a.RewriteRequestBody(context.Background(), body, "/api/x", traffic.NormalizedContent{})
	if !errors.Is(err, traffic.ErrRewriteUnsupported) {
		t.Errorf("err=%v want ErrRewriteUnsupported", err)
	}
	if n != 0 || string(out) != string(body) {
		t.Errorf("body modified")
	}
}

func TestRewriteResponseBody_Unsupported(t *testing.T) {
	a := &Adapter{}
	body := []byte(`{}`)
	out, n, err := a.RewriteResponseBody(context.Background(), body, "/api/x", traffic.NormalizedContent{})
	if !errors.Is(err, traffic.ErrRewriteUnsupported) {
		t.Errorf("err=%v want ErrRewriteUnsupported", err)
	}
	if n != 0 || string(out) != string(body) {
		t.Errorf("body modified")
	}
}

func TestLooksLikeJSON(t *testing.T) {
	if !looksLikeJSON([]byte(`{"a":1}`)) {
		t.Errorf("JSON not detected")
	}
	if looksLikeJSON([]byte{0x00}) {
		t.Errorf("binary detected as JSON")
	}
}

// Additional coverage: branches not exercised by the original suite.

// Exercise the messages[].content as parts-array branch (line 72-79) and the
// conversation_id metadata branch (line 109-111) in ExtractRequest.
func TestExtractRequest_ContentPartsAndConversationID(t *testing.T) {
	body := []byte(`{
		"messages":[
			{"role":"user","content":[
				{"type":"text","text":"first part"},
				{"type":"image_url","image_url":{"url":"x"}},
				{"type":"text","text":"second part"}
			]}
		],
		"conversation_id":"conv-abc"
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/chat")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 2 || nc.Segments[0] != "first part" || nc.Segments[1] != "second part" {
		t.Errorf("Segments=%v want [first part second part]", nc.Segments)
	}
	if nc.Metadata["conversation_id"] != "conv-abc" {
		t.Errorf("conversation_id=%q", nc.Metadata["conversation_id"])
	}
}

// ExtractResponse: empty body returns zero NormalizedContent + nil error.
func TestExtractResponse_EmptyBody(t *testing.T) {
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), nil, "/api/chat")
	if err != nil {
		t.Errorf("err=%v want nil", err)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("Segments=%v want empty", nc.Segments)
	}
}

// ExtractResponse: looks-like-JSON but invalid → ErrMalformed.
func TestExtractResponse_MalformedJSON(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), []byte(`{bad json`), "/api/chat")
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("err=%v want ErrMalformed", err)
	}
}

// ExtractResponse: top-level `message` (no error envelope, no choices) is
// surfaced as an error-tagged segment (line 142-147).
func TestExtractResponse_TopLevelMessage(t *testing.T) {
	body := []byte(`{"message":"rate limited"}`)
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), body, "/api/chat")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "rate limited" {
		t.Errorf("Segments=%v", nc.Segments)
	}
	if nc.Metadata["error"] != "true" {
		t.Errorf("error metadata=%q want true", nc.Metadata["error"])
	}
}

// ExtractResponse: valid JSON object with neither error/message/choices →
// final ErrUnknownSchema return (line 164).
func TestExtractResponse_UnknownShape(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), []byte(`{"status":"ok"}`), "/api/chat")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

// ExtractStreamChunk: invalid JSON chunk fails open with empty segments
// (line 176-178).
func TestExtractStreamChunk_InvalidJSON(t *testing.T) {
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), []byte(`{bad`), "/api/chat/stream")
	if err != nil {
		t.Errorf("err=%v want nil (fail-open)", err)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("Segments=%v want empty", nc.Segments)
	}
}

// ExtractStreamChunk: delta.tool_calls array (line 187-191).
func TestExtractStreamChunk_DeltaToolCalls(t *testing.T) {
	chunk := []byte(`{"choices":[{"delta":{"tool_calls":[
		{"index":0,"id":"c1","type":"function","function":{"name":"do","arguments":"{}"}}
	]}}]}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/api/chat/stream")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.ToolCallSegments) != 1 || !strings.Contains(nc.ToolCallSegments[0], `"do"`) {
		t.Errorf("ToolCallSegments=%v", nc.ToolCallSegments)
	}
}

// ExtractStreamChunk: top-level `content` field (line 199-201) — JSON without
// choices/delta, with `content` populated.
func TestExtractStreamChunk_TopLevelContent(t *testing.T) {
	chunk := []byte(`{"content":"streamed chunk"}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/api/chat/stream")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "streamed chunk" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

// looksLikeJSON: whitespace-only input must return false (line 249).
func TestLooksLikeJSON_WhitespaceOnly(t *testing.T) {
	if looksLikeJSON([]byte(" \t\n\r")) {
		t.Errorf("whitespace-only must not be JSON")
	}
}

// looksLikeJSON: leading whitespace followed by `{` returns true — exercises
// the continue branch (line 244-245).
func TestLooksLikeJSON_LeadingWhitespace(t *testing.T) {
	if !looksLikeJSON([]byte("\n\t  {\"a\":1}")) {
		t.Errorf("leading whitespace + { must be JSON")
	}
}

// Normalize (normalize.go) — delegates to extract.NormalizeForAdapter.

func TestNormalize_OpenAIChatRequest(t *testing.T) {
	body := []byte(`{
		"model":"claude-3-5-sonnet",
		"messages":[{"role":"user","content":"hello codeium"}]
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
	if payload.DetectedSpec != adapterID {
		t.Errorf("DetectedSpec=%q want %q", payload.DetectedSpec, adapterID)
	}
}

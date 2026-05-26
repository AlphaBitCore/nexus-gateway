package claudeweb

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

// Fixtures derived from public reverse-engineering of claude.ai
// /api/organizations/<org>/chat_conversations/<conv>/completion. Bytes
// are de-credentialled (UUIDs and tokens stubbed). The adapter handles
// both the legacy `completion` event shape and the Anthropic-Messages-
// API-style `content_block_delta` shape because claude.ai has migrated
// between the two over time.

func TestAdapter_ID(t *testing.T) {
	a := &Adapter{}
	if a.ID() != "claude-web" {
		t.Errorf("ID=%q", a.ID())
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

func TestExtractRequest_PromptShape(t *testing.T) {
	body := []byte(`{
		"prompt": "Why is the sky blue?",
		"timezone": "America/Los_Angeles",
		"parent_message_uuid": "00000000-0000-0000-0000-000000000001",
		"conversation_uuid": "00000000-0000-0000-0000-000000000002",
		"model": "claude-sonnet-4-6"
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/organizations/x/chat_conversations/y/completion")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "Why is the sky blue?" {
		t.Errorf("Segments=%v", nc.Segments)
	}
	if nc.Metadata["model"] != "claude-sonnet-4-6" {
		t.Errorf("model=%q", nc.Metadata["model"])
	}
	if nc.Metadata["conversation_uuid"] != "00000000-0000-0000-0000-000000000002" {
		t.Errorf("conversation_uuid=%q", nc.Metadata["conversation_uuid"])
	}
	if nc.Metadata["parent_message_uuid"] != "00000000-0000-0000-0000-000000000001" {
		t.Errorf("parent_message_uuid=%q", nc.Metadata["parent_message_uuid"])
	}
}

func TestExtractRequest_AttachmentsExtractedContent(t *testing.T) {
	// claude.ai inlines text attachments with extracted_content; this
	// content reaches Segments so PII / secrets in uploaded files are
	// scanned by compliance hooks.
	body := []byte(`{
		"prompt": "Summarize the attached doc.",
		"attachments": [
			{"file_name": "private.txt", "extracted_content": "Customer SSN: 123-45-6789"}
		]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/.../completion")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 2 {
		t.Fatalf("Segments len=%d want 2", len(nc.Segments))
	}
	if nc.Segments[0] != "Summarize the attached doc." {
		t.Errorf("Segments[0]=%q", nc.Segments[0])
	}
	if nc.Segments[1] != "Customer SSN: 123-45-6789" {
		t.Errorf("Segments[1]=%q must include extracted_content", nc.Segments[1])
	}
}

func TestExtractRequest_FilesExtractedContent(t *testing.T) {
	body := []byte(`{
		"prompt": "Question",
		"files": [
			{"file_name": "report.pdf", "file_uuid": "f1", "extracted_content": "Q4 revenue: $1.2M"}
		]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/.../completion")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 2 || nc.Segments[1] != "Q4 revenue: $1.2M" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestExtractRequest_FutureMessagesShape(t *testing.T) {
	// Forward-compat: if claude.ai migrates to Messages-API-style request
	// bodies, the adapter still extracts text content from messages.
	body := []byte(`{
		"messages": [
			{"role": "user", "content": [{"type": "text", "text": "future shape"}]}
		]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/.../completion")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "future shape" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestExtractRequest_Extra(t *testing.T) {
	body := []byte(`{
		"prompt": "hi",
		"x_future_field": {"sensitive": "data"}
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/.../completion")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	x, ok := nc.Extra["x_future_field"]
	if !ok || !strings.Contains(x, "sensitive") {
		t.Errorf("Extra=%v missing x_future_field", nc.Extra)
	}
	if _, ok := nc.Extra["prompt"]; ok {
		t.Errorf("prompt must not leak into Extra")
	}
}

func TestExtractRequest_MissingPromptAndMessages(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), []byte(`{"timezone":"UTC"}`), "/api/.../completion")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

func TestExtractRequest_Malformed(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), []byte(`not json`), "/api/.../completion")
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("err=%v want ErrMalformed", err)
	}
}

// ExtractStreamChunk — legacy `completion` shape

func TestExtractStreamChunk_LegacyCompletionEvent(t *testing.T) {
	chunk := []byte(`{"completion":"Hello, ","stop_reason":null,"model":"claude-sonnet-4-6","truncated":false}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/api/.../completion")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "Hello, " {
		t.Errorf("Segments=%v", nc.Segments)
	}
	if nc.Metadata["model"] != "claude-sonnet-4-6" {
		t.Errorf("model=%q", nc.Metadata["model"])
	}
}

func TestExtractStreamChunk_LegacyCompletionWithStopReason(t *testing.T) {
	chunk := []byte(`{"completion":"","stop_reason":"end_turn","model":"claude-sonnet-4-6"}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/api/.../completion")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	// Empty completion + stop_reason: nothing on Segments, but stop_reason
	// path is governed by the (2) Anthropic-Messages-API shape;
	// legacy completion emits text only when the completion field is
	// non-empty. Empty-completion chunks return empty content.
	if len(nc.Segments) != 0 {
		t.Errorf("Segments=%v want empty for empty completion", nc.Segments)
	}
}

// ExtractStreamChunk — Anthropic-Messages-API shape

func TestExtractStreamChunk_TextDelta(t *testing.T) {
	chunk := []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/api/.../completion")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "Hello" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestExtractStreamChunk_ThinkingDelta(t *testing.T) {
	chunk := []byte(`{"type":"content_block_delta","index":1,"delta":{"type":"thinking_delta","thinking":"Internal trace…"}}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/api/.../completion")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.ReasoningSegments) != 1 || nc.ReasoningSegments[0] != "Internal trace…" {
		t.Errorf("ReasoningSegments=%v", nc.ReasoningSegments)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("Segments must not absorb thinking: %v", nc.Segments)
	}
}

func TestExtractStreamChunk_ContentBlockStart_ToolUse(t *testing.T) {
	chunk := []byte(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"web_search","input":{}}}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/api/.../completion")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.ToolCallSegments) != 1 || !strings.Contains(nc.ToolCallSegments[0], `"web_search"`) {
		t.Errorf("ToolCallSegments=%v", nc.ToolCallSegments)
	}
}

func TestExtractStreamChunk_InputJsonDelta(t *testing.T) {
	chunk := []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"query\":\"weather"}}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/api/.../completion")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.ToolCallSegments) != 1 || !strings.Contains(nc.ToolCallSegments[0], `partial_json`) {
		t.Errorf("ToolCallSegments=%v", nc.ToolCallSegments)
	}
}

func TestExtractStreamChunk_MessageDeltaStopReason(t *testing.T) {
	chunk := []byte(`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null}}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/api/.../completion")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if nc.Metadata["stop_reason"] != "end_turn" {
		t.Errorf("stop_reason=%q", nc.Metadata["stop_reason"])
	}
}

func TestExtractStreamChunk_FilteredFrames(t *testing.T) {
	cases := [][]byte{
		[]byte(`{"type":"ping"}`),
		[]byte(`{"type":"message_start","message":{}}`),
		[]byte(`{"type":"content_block_stop","index":0}`),
		[]byte(`{"type":"message_stop"}`),
		[]byte(`{"type":"error","error":{"type":"rate_limit"}}`),
	}
	a := &Adapter{}
	for i, c := range cases {
		nc, err := a.ExtractStreamChunk(context.Background(), c, "/api/.../completion")
		if err != nil {
			t.Fatalf("case %d err=%v", i, err)
		}
		if len(nc.Segments) != 0 || len(nc.ReasoningSegments) != 0 || len(nc.ToolCallSegments) != 0 {
			t.Errorf("case %d emitted content: %+v", i, nc)
		}
	}
}

func TestExtractStreamChunk_DefensiveOnInvalidJSON(t *testing.T) {
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), []byte(`not json`), "/api/.../completion")
	if err != nil {
		t.Errorf("err=%v want nil for invalid JSON (fail-open)", err)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestExtractStreamChunk_EmptyChunk(t *testing.T) {
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), nil, "/api/.../completion")
	if err != nil {
		t.Errorf("err=%v want nil for empty chunk", err)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

// ExtractResponse — error path coverage

func TestExtractResponse_ErrorEnvelope(t *testing.T) {
	body := []byte(`{"error":{"type":"rate_limit_error","message":"Rate limit exceeded"}}`)
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), body, "/api/.../completion")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "Rate limit exceeded" {
		t.Errorf("Segments=%v", nc.Segments)
	}
	if nc.Metadata["error"] != "true" {
		t.Errorf("error=%q", nc.Metadata["error"])
	}
}

func TestExtractResponse_DetailString(t *testing.T) {
	body := []byte(`{"detail":"forbidden"}`)
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), body, "/api/.../completion")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "forbidden" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestExtractResponse_PlainMessageString(t *testing.T) {
	body := []byte(`{"message":"organization disabled"}`)
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), body, "/api/.../completion")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "organization disabled" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestExtractResponse_EmptyBody(t *testing.T) {
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), nil, "/api/.../completion")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestExtractResponse_Malformed(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), []byte(`not json`), "/api/.../completion")
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("err=%v want ErrMalformed", err)
	}
}

func TestExtractResponse_NonErrorBufferedJSON(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), []byte(`{"foo":"bar"}`), "/api/.../completion")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

// DetectRequestMeta + DetectResponseUsage

func TestDetectRequestMeta_ProviderAndModel(t *testing.T) {
	body := []byte(`{"prompt":"hi","model":"claude-sonnet-4-6"}`)
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost, "https://claude.ai/api/.../completion", nil)
	meta := a.DetectRequestMeta(r, body)
	if meta.Provider != "claude-web" {
		t.Errorf("Provider=%q want claude-web", meta.Provider)
	}
	if meta.Model != "claude-sonnet-4-6" {
		t.Errorf("Model=%q", meta.Model)
	}
}

func TestDetectRequestMeta_InvalidBody(t *testing.T) {
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost, "https://claude.ai/api/.../completion", nil)
	meta := a.DetectRequestMeta(r, []byte(`not json`))
	if meta.Provider != "claude-web" {
		t.Errorf("Provider=%q", meta.Provider)
	}
	if meta.Model != "" {
		t.Errorf("Model=%q want empty", meta.Model)
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
	out, n, err := a.RewriteRequestBody(context.Background(), body, "/api/.../completion", traffic.NormalizedContent{})
	if !errors.Is(err, traffic.ErrRewriteUnsupported) {
		t.Errorf("err=%v want ErrRewriteUnsupported", err)
	}
	if n != 0 || string(out) != string(body) {
		t.Errorf("n=%d body modified", n)
	}
}

func TestRewriteResponseBody_Unsupported(t *testing.T) {
	a := &Adapter{}
	body := []byte(`{"detail":"err"}`)
	out, n, err := a.RewriteResponseBody(context.Background(), body, "/api/.../completion", traffic.NormalizedContent{})
	if !errors.Is(err, traffic.ErrRewriteUnsupported) {
		t.Errorf("err=%v want ErrRewriteUnsupported", err)
	}
	if n != 0 || string(out) != string(body) {
		t.Errorf("n=%d body modified", n)
	}
}

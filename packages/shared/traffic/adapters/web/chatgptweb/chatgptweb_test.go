package chatgptweb

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

// Fixtures below are derived from a captured 2026-04 chatgpt.com
// /backend-api/f/conversation session — see streaming/extract/
// chatgpt_web.go for the same protocol shape used by the streaming
// extractor. Bytes are de-credentialled (UUIDs randomised, JWTs
// truncated).

func TestAdapter_ID(t *testing.T) {
	a := &Adapter{}
	if a.ID() != "chatgpt-web" {
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

// TestExtractRequest_PartsShape covers the current (2024+) chatgpt.com
// request body where messages[].content.parts is an array of strings.
func TestExtractRequest_PartsShape(t *testing.T) {
	body := []byte(`{
		"action": "next",
		"messages": [
			{
				"id": "aaaa-1111",
				"author": {"role": "user"},
				"content": {"content_type": "text", "parts": ["Why is the sky blue?"]}
			}
		],
		"conversation_id": "00000000-0000-4000-8000-000000000001",
		"parent_message_id": "bbbb-2222",
		"model": "auto",
		"model_slug": "gpt-5"
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/backend-api/f/conversation")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "Why is the sky blue?" {
		t.Errorf("Segments=%v", nc.Segments)
	}
	if nc.Metadata["model_slug"] != "gpt-5" {
		t.Errorf("model_slug=%q", nc.Metadata["model_slug"])
	}
	if nc.Metadata["conversation_id"] != "00000000-0000-4000-8000-000000000001" {
		t.Errorf("conversation_id=%q", nc.Metadata["conversation_id"])
	}
	if nc.Metadata["action"] != "next" {
		t.Errorf("action=%q", nc.Metadata["action"])
	}
}

// TestExtractRequest_LegacyTextShape covers the older shape where
// messages[].content.text is a single string. Some older clients still
// emit this when paired with very-early chatgpt.com endpoints.
func TestExtractRequest_LegacyTextShape(t *testing.T) {
	body := []byte(`{
		"messages": [
			{"author": {"role": "user"}, "content": {"text": "Older shape prompt"}}
		]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/backend-api/conversation")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "Older shape prompt" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

// TestExtractRequest_MultiTurnConversation covers a follow-up turn that
// includes prior assistant content alongside the new user prompt.
func TestExtractRequest_MultiTurnConversation(t *testing.T) {
	body := []byte(`{
		"messages": [
			{"author": {"role": "user"}, "content": {"content_type":"text","parts": ["Question one."]}},
			{"author": {"role": "assistant"}, "content": {"content_type":"text","parts": ["Answer one."]}},
			{"author": {"role": "user"}, "content": {"content_type":"text","parts": ["Question two."]}}
		]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/backend-api/f/conversation")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	want := []string{"Question one.", "Answer one.", "Question two."}
	if len(nc.Segments) != len(want) {
		t.Fatalf("Segments len=%d want %d", len(nc.Segments), len(want))
	}
	for i := range want {
		if nc.Segments[i] != want[i] {
			t.Errorf("Segments[%d]=%q want %q", i, nc.Segments[i], want[i])
		}
	}
}

func TestExtractRequest_MissingMessages(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), []byte(`{"conversation_id":"x"}`), "/backend-api/f/conversation")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

func TestExtractRequest_Malformed(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), []byte(`not json`), "/backend-api/f/conversation")
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("err=%v want ErrMalformed", err)
	}
}

func TestExtractStreamChunk_InputMessageEcho(t *testing.T) {
	chunk := []byte(`{"type":"input_message","input_message":{"id":"x","author":{"role":"user"},"content":{"content_type":"text","parts":["echoed prompt"]}}}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/backend-api/f/conversation")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "echoed prompt" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestExtractStreamChunk_AppendOp(t *testing.T) {
	// Single-op shape: {p, o, v} — the typical chatgpt.com per-token delta.
	chunk := []byte(`{"p":"/message/content/parts/0","o":"append","v":"Hello"}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/backend-api/f/conversation")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "Hello" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestExtractStreamChunk_ReplaceOpOnContent(t *testing.T) {
	// `replace` on content/parts: rarer (used at end of stream to set a
	// final status string). Treat string values as completion content.
	chunk := []byte(`{"p":"/message/content/parts/0","o":"replace","v":"final"}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/backend-api/f/conversation")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "final" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestExtractStreamChunk_PatchBatchOp(t *testing.T) {
	// `{"o":"patch","v":[…]}` wraps multiple sub-ops in one frame.
	chunk := []byte(`{"o":"patch","v":[
		{"p":"/message/content/parts/0","o":"append","v":"one "},
		{"p":"/message/content/parts/0","o":"append","v":"two"},
		{"p":"/message/status","o":"replace","v":"finished_successfully"}
	]}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/backend-api/f/conversation")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 2 {
		t.Fatalf("Segments len=%d want 2", len(nc.Segments))
	}
	if nc.Segments[0] != "one " || nc.Segments[1] != "two" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestExtractStreamChunk_AddInitialAssistantFrame(t *testing.T) {
	// Initial assistant message: `add` op with empty path and a value
	// containing message.content.parts (sometimes pre-populated).
	chunk := []byte(`{"o":"add","p":"","v":{"message":{"id":"asst-1","author":{"role":"assistant"},"content":{"content_type":"text","parts":["seed text"]}}}}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/backend-api/f/conversation")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "seed text" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestExtractStreamChunk_TelemetryFramesSkipped(t *testing.T) {
	// All these frame types are routing/telemetry — no audit content.
	cases := [][]byte{
		[]byte(`{"type":"resume_conversation_token","resume_conversation_token":"ey..."}`),
		[]byte(`{"type":"message_marker","message_id":"x"}`),
		[]byte(`{"type":"server_ste_metadata","x":"y"}`),
		[]byte(`{"type":"conversation_detail_metadata"}`),
		[]byte(`{"type":"message_stream_complete"}`),
		[]byte(`{"type":"moderation","is_blocked":false}`),
	}
	a := &Adapter{}
	for i, c := range cases {
		nc, err := a.ExtractStreamChunk(context.Background(), c, "/backend-api/f/conversation")
		if err != nil {
			t.Fatalf("case %d err=%v", i, err)
		}
		if len(nc.Segments) != 0 || len(nc.ToolCallSegments) != 0 {
			t.Errorf("case %d emitted content: %+v", i, nc)
		}
	}
}

func TestExtractStreamChunk_MarkersSkipped(t *testing.T) {
	a := &Adapter{}
	for _, c := range [][]byte{[]byte(`"v1"`), []byte(`[DONE]`), []byte(``)} {
		nc, err := a.ExtractStreamChunk(context.Background(), c, "/backend-api/f/conversation")
		if err != nil {
			t.Fatalf("err=%v for %q", err, c)
		}
		if len(nc.Segments) != 0 {
			t.Errorf("marker %q produced content: %v", c, nc.Segments)
		}
	}
}

// TestExtractStreamChunk_ToolUseObject covers tool-use payloads embedded
// inside content/parts as object-typed values. Best-effort capture: the
// chatgpt.com tool-emission shape is undocumented and may evolve, so the
// adapter recognises a few `type` values and forwards the raw object as
// a ToolCallSegment for hooks to inspect.
func TestExtractStreamChunk_ToolUseObject(t *testing.T) {
	chunk := []byte(`{"p":"/message/content/parts/0","o":"append","v":{"type":"tool_use","name":"web_search","input":{"query":"weather NYC"}}}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/backend-api/f/conversation")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.ToolCallSegments) != 1 {
		t.Fatalf("ToolCallSegments len=%d want 1", len(nc.ToolCallSegments))
	}
	if !strings.Contains(nc.ToolCallSegments[0], `"web_search"`) {
		t.Errorf("ToolCallSegments[0]=%q missing tool name", nc.ToolCallSegments[0])
	}
}

// TestExtractStreamChunk_ObjectWithText covers an object value whose
// `type` is not a tool variant but whose `text` field carries content;
// the text falls back into Segments.
func TestExtractStreamChunk_ObjectWithText(t *testing.T) {
	chunk := []byte(`{"p":"/message/content/parts/0","o":"append","v":{"type":"text","text":"object-wrapped text"}}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/backend-api/f/conversation")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "object-wrapped text" {
		t.Errorf("Segments=%v", nc.Segments)
	}
	if len(nc.ToolCallSegments) != 0 {
		t.Errorf("ToolCallSegments must stay empty: %v", nc.ToolCallSegments)
	}
}

func TestExtractStreamChunk_NonContentPathSkipped(t *testing.T) {
	// `replace` on /message/status (or any non-content path) is not
	// audit content — a `finished_successfully` marker.
	chunk := []byte(`{"p":"/message/status","o":"replace","v":"finished_successfully"}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/backend-api/f/conversation")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("non-content path leaked: %v", nc.Segments)
	}
}

func TestExtractStreamChunk_DefensiveOnInvalidJSON(t *testing.T) {
	// chatgpt.com protocol is not a public contract — unexpected non-JSON
	// frames must not error (fail-open posture per package doc).
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), []byte(`not json`), "/backend-api/f/conversation")
	if err != nil {
		t.Errorf("err=%v want nil for invalid JSON (fail-open)", err)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

// ExtractResponse — error path coverage

func TestExtractResponse_ErrorDetailString(t *testing.T) {
	body := []byte(`{"detail":"Too many requests"}`)
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), body, "/backend-api/f/conversation")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "Too many requests" {
		t.Errorf("Segments=%v", nc.Segments)
	}
	if nc.Metadata["error"] != "true" {
		t.Errorf("error metadata=%q", nc.Metadata["error"])
	}
}

func TestExtractResponse_ErrorDetailObject(t *testing.T) {
	body := []byte(`{"detail":{"message":"Conversation not found","code":"conversation_not_found"}}`)
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), body, "/backend-api/f/conversation")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "Conversation not found" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestExtractResponse_OpenAIErrorEnvelope(t *testing.T) {
	// chatgpt.com sometimes returns the OpenAI public-API error envelope
	// for certain auth / quota errors.
	body := []byte(`{"error":{"message":"rate limit","type":"rate_limit_exceeded","code":"quota_exceeded"}}`)
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), body, "/backend-api/f/conversation")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "rate limit" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestExtractResponse_EmptyBody(t *testing.T) {
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), nil, "/backend-api/f/conversation")
	if err != nil {
		t.Fatalf("err=%v want nil for empty body", err)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestExtractResponse_MalformedBody(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), []byte(`not json`), "/backend-api/f/conversation")
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("err=%v want ErrMalformed", err)
	}
}

func TestExtractResponse_NonErrorBufferedJSON(t *testing.T) {
	// A non-error JSON body buffered into ExtractResponse: chatgpt.com
	// streams successful responses, so a non-error JSON here is unknown
	// schema — we do not try to re-parse it as SSE (callers should feed
	// frames via ExtractStreamChunk instead).
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), []byte(`{"foo":"bar"}`), "/backend-api/f/conversation")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

// DetectRequestMeta + DetectResponseUsage

func TestDetectRequestMeta_ProviderAndModel(t *testing.T) {
	body := []byte(`{"model":"auto","model_slug":"gpt-5","conversation_id":"x"}`)
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/f/conversation", nil)
	meta := a.DetectRequestMeta(r, body)
	if meta.Provider != "chatgpt-web" {
		t.Errorf("Provider=%q want chatgpt-web", meta.Provider)
	}
	if meta.Model != "auto" {
		t.Errorf("Model=%q want auto", meta.Model)
	}
}

func TestDetectRequestMeta_FallsBackToModelSlug(t *testing.T) {
	// When `model` is absent, fall back to `model_slug`.
	body := []byte(`{"model_slug":"gpt-5"}`)
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/f/conversation", nil)
	meta := a.DetectRequestMeta(r, body)
	if meta.Model != "gpt-5" {
		t.Errorf("Model=%q want gpt-5", meta.Model)
	}
}

func TestDetectRequestMeta_InvalidBody(t *testing.T) {
	// Adapter must never panic on bad input — Provider stays set.
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/f/conversation", nil)
	meta := a.DetectRequestMeta(r, []byte(`not json`))
	if meta.Provider != "chatgpt-web" {
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
		t.Errorf("token pointers must be nil for chatgpt-web; got %+v", usage)
	}
}

// Rewrite contracts (must return ErrRewriteUnsupported)

func TestRewriteRequestBody_Unsupported(t *testing.T) {
	a := &Adapter{}
	body := []byte(`{"messages":[{"author":{"role":"user"},"content":{"parts":["hi"]}}]}`)
	out, n, err := a.RewriteRequestBody(context.Background(), body, "/backend-api/f/conversation", traffic.NormalizedContent{})
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
	body := []byte(`{"detail":"err"}`)
	out, n, err := a.RewriteResponseBody(context.Background(), body, "/backend-api/f/conversation", traffic.NormalizedContent{})
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

package gemini

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

func TestAdapter_ID(t *testing.T) {
	a := &Adapter{}
	if a.ID() != "gemini" {
		t.Errorf("ID = %q", a.ID())
	}
}

func TestAdapter_Configure(t *testing.T) {
	a := &Adapter{}
	if err := a.Configure(nil); err != nil {
		t.Errorf("Configure(nil) = %v", err)
	}
	if err := a.Configure(map[string]any{"foo": "bar"}); err != nil {
		t.Errorf("Configure(map) = %v", err)
	}
}

func TestExtractRequest_Basic(t *testing.T) {
	body := []byte(`{
		"contents": [
			{"role": "user", "parts": [{"text": "Hello, Gemini!"}]},
			{"role": "model", "parts": [{"text": "Hi there!"}]}
		]
	}`)

	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/v1beta/models/gemini-pro:generateContent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(nc.Segments) != 2 {
		t.Fatalf("expected 2 segments, got %d", len(nc.Segments))
	}
	if nc.Segments[0] != "Hello, Gemini!" {
		t.Errorf("segment[0] = %q", nc.Segments[0])
	}
	if nc.Segments[1] != "Hi there!" {
		t.Errorf("segment[1] = %q", nc.Segments[1])
	}
}

func TestExtractRequest_SystemInstruction(t *testing.T) {
	body := []byte(`{
		"systemInstruction": {
			"parts": [{"text": "You are a helpful assistant."}]
		},
		"contents": [
			{"role": "user", "parts": [{"text": "Hello"}]}
		]
	}`)

	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/v1beta/models/gemini-pro:generateContent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(nc.Segments) != 2 {
		t.Fatalf("expected 2 segments (system + user), got %d", len(nc.Segments))
	}
	if nc.Segments[0] != "You are a helpful assistant." {
		t.Errorf("segment[0] = %q", nc.Segments[0])
	}
	if nc.Segments[1] != "Hello" {
		t.Errorf("segment[1] = %q", nc.Segments[1])
	}
}

func TestExtractRequest_MultipleParts(t *testing.T) {
	body := []byte(`{
		"contents": [
			{"role": "user", "parts": [
				{"text": "Part 1"},
				{"text": "Part 2"},
				{"inline_data": {"mime_type": "image/png", "data": "..."}}
			]}
		]
	}`)

	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/v1beta/models/gemini-pro:generateContent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(nc.Segments) != 2 {
		t.Fatalf("expected 2 text segments, got %d", len(nc.Segments))
	}
}

func TestExtractRequest_MalformedJSON(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), []byte(`not json`), "/v1beta/models/gemini-pro:generateContent")
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("expected ErrMalformed, got %v", err)
	}
}

// TestExtractRequest_FunctionResponseText covers the audit gap: tool
// returns sent back to the model arrive in functionResponse parts. The
// extractor pulls text from both shapes:
//   - response: "<string>"        (legacy / unwrapped)
//   - response: {"result": "<string>"}  (the wrapper our spec_gemini
//     codec emits when canonical content was a plain string)
//
// Object responses with arbitrary fields are skipped (no compliance
// hook can know which field is user content).
func TestExtractRequest_FunctionResponseText(t *testing.T) {
	t.Run("result_wrapper", func(t *testing.T) {
		body := []byte(`{
			"contents":[
				{"role":"user","parts":[{"text":"weather?"}]},
				{"role":"model","parts":[{"functionCall":{"name":"get_weather","args":{}}}]},
				{"role":"user","parts":[{"functionResponse":{"name":"get_weather","response":{"result":"customer addr 1 Main St"}}}]}
			]
		}`)
		a := &Adapter{}
		nc, err := a.ExtractRequest(context.Background(), body, "/v1beta/models/gemini-pro:generateContent")
		if err != nil {
			t.Fatal(err)
		}
		// Segments: ["weather?", "customer addr 1 Main St"].
		if len(nc.Segments) != 2 || nc.Segments[1] != "customer addr 1 Main St" {
			t.Fatalf("Segments=%v", nc.Segments)
		}
	})
	t.Run("string_response", func(t *testing.T) {
		body := []byte(`{
			"contents":[
				{"role":"user","parts":[{"functionResponse":{"name":"f","response":"raw text result"}}]}
			]
		}`)
		a := &Adapter{}
		nc, err := a.ExtractRequest(context.Background(), body, "/v1beta/models/gemini-pro:generateContent")
		if err != nil {
			t.Fatal(err)
		}
		if len(nc.Segments) != 1 || nc.Segments[0] != "raw text result" {
			t.Fatalf("Segments=%v", nc.Segments)
		}
	})
	t.Run("object_response_without_result_skipped", func(t *testing.T) {
		body := []byte(`{
			"contents":[
				{"role":"user","parts":[{"functionResponse":{"name":"f","response":{"temp_f":62,"humidity":45}}}]}
			]
		}`)
		a := &Adapter{}
		nc, err := a.ExtractRequest(context.Background(), body, "/v1beta/models/gemini-pro:generateContent")
		if err != nil {
			t.Fatal(err)
		}
		// Structured object without the `result` field is opaque —
		// extractor cannot tell which fields hold user content, so we
		// emit no segment.
		if len(nc.Segments) != 0 {
			t.Fatalf("Segments=%v want empty", nc.Segments)
		}
	})
}

func TestExtractRequest_MissingContents(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), []byte(`{"model":"gemini-pro"}`), "/v1beta/models/gemini-pro:generateContent")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("expected ErrUnknownSchema, got %v", err)
	}
}

func TestExtractResponse(t *testing.T) {
	body := []byte(`{
		"candidates": [
			{"content": {"parts": [{"text": "Hello! I am Gemini."}], "role": "model"}}
		]
	}`)

	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), body, "/v1beta/models/gemini-pro:generateContent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "Hello! I am Gemini." {
		t.Errorf("segments = %v", nc.Segments)
	}
}

func TestExtractResponse_MalformedJSON(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), []byte(`{bad`), "/v1beta/models/gemini-pro:generateContent")
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("expected ErrMalformed, got %v", err)
	}
}

func TestExtractResponse_MissingCandidates(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), []byte(`{"promptFeedback":{}}`), "/v1beta/models/gemini-pro:generateContent")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("expected ErrUnknownSchema, got %v", err)
	}
}

func TestExtractStreamChunk_WithText(t *testing.T) {
	chunk := []byte(`{
		"candidates": [
			{"content": {"parts": [{"text": "Hello"}], "role": "model"}}
		]
	}`)

	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/v1beta/models/gemini-pro:streamGenerateContent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "Hello" {
		t.Errorf("segments = %v", nc.Segments)
	}
}

func TestExtractStreamChunk_NoCandidates(t *testing.T) {
	chunk := []byte(`{"promptFeedback": {"blockReason": "SAFETY"}}`)

	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/v1beta/models/gemini-pro:streamGenerateContent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("expected 0 segments, got %d", len(nc.Segments))
	}
}

func TestExtractStreamChunk_MalformedJSON(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractStreamChunk(context.Background(), []byte(`not json`), "/v1beta/models/gemini-pro:streamGenerateContent")
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("expected ErrMalformed, got %v", err)
	}
}

// Function-call coverage (parts[].functionCall must reach ToolCallSegments
// for compliance hooks scanning MCP tool detection / PII in tool args).

// TestExtractRequest_FunctionCallInHistory pins that an assistant turn
// echoed in conversation history with a functionCall part lands on
// ToolCallSegments.
func TestExtractRequest_FunctionCallInHistory(t *testing.T) {
	body := []byte(`{
		"contents": [
			{"role":"user","parts":[{"text":"weather in NYC?"}]},
			{"role":"model","parts":[
				{"functionCall":{"name":"get_weather","args":{"city":"NYC"}}}
			]},
			{"role":"user","parts":[
				{"functionResponse":{"name":"get_weather","response":{"result":"72F sunny"}}}
			]}
		]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/v1beta/models/gemini-pro:generateContent")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.ToolCallSegments) != 1 {
		t.Fatalf("ToolCallSegments len=%d want 1", len(nc.ToolCallSegments))
	}
	if !strings.Contains(nc.ToolCallSegments[0], `"get_weather"`) {
		t.Errorf("ToolCallSegments[0]=%q missing function name", nc.ToolCallSegments[0])
	}
	// functionResponse text remains on Segments.
	found := false
	for _, s := range nc.Segments {
		if s == "72F sunny" {
			found = true
		}
	}
	if !found {
		t.Errorf("functionResponse result missing from Segments: %v", nc.Segments)
	}
}

// TestExtractRequest_ToolDefinitionsInMetadata pins that the top-level
// `tools` array (function declarations) lands in Metadata so audit
// captures the model's authorised function scope.
func TestExtractRequest_ToolDefinitionsInMetadata(t *testing.T) {
	body := []byte(`{
		"contents": [{"role":"user","parts":[{"text":"hi"}]}],
		"tools": [
			{"functionDeclarations":[{"name":"get_weather","description":"…"}]}
		]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/v1beta/models/gemini-pro:generateContent")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if !strings.Contains(nc.Metadata["tools"], `"get_weather"`) {
		t.Errorf("Metadata[tools]=%q missing decl", nc.Metadata["tools"])
	}
	if len(nc.ToolCallSegments) != 0 {
		t.Errorf("definitions must not leak to ToolCallSegments: %v", nc.ToolCallSegments)
	}
}

// TestExtractRequest_ThinkingPart pins Gemini 2.5+ extended thinking:
// a part with thought=true contains reasoning text that must land on
// ReasoningSegments, not Segments.
func TestExtractRequest_ThinkingPart(t *testing.T) {
	body := []byte(`{
		"contents": [
			{"role":"model","parts":[
				{"thought":true,"text":"Internal reasoning trace …"},
				{"text":"The answer is 42."}
			]}
		]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/v1beta/models/gemini-pro:generateContent")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.ReasoningSegments) != 1 || nc.ReasoningSegments[0] != "Internal reasoning trace …" {
		t.Errorf("ReasoningSegments=%v", nc.ReasoningSegments)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "The answer is 42." {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

// TestExtractRequest_Extra covers the safety-net: novel top-level
// fields land in Extra.
func TestExtractRequest_Extra(t *testing.T) {
	body := []byte(`{
		"contents":[{"role":"user","parts":[{"text":"hi"}]}],
		"x_future_field":{"sensitive":"data"}
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/v1beta/models/gemini-pro:generateContent")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	x, ok := nc.Extra["x_future_field"]
	if !ok || !strings.Contains(x, "sensitive") {
		t.Errorf("Extra=%v missing x_future_field", nc.Extra)
	}
}

// TestExtractResponse_FunctionCall covers the assistant emitting a
// functionCall part — invocation lands on ToolCallSegments.
func TestExtractResponse_FunctionCall(t *testing.T) {
	body := []byte(`{
		"candidates": [{
			"content": {"role":"model","parts":[
				{"text":"Looking up the weather …"},
				{"functionCall":{"name":"get_weather","args":{"city":"NYC"}}}
			]},
			"finishReason":"STOP"
		}],
		"modelVersion":"gemini-3-pro-001",
		"responseId":"resp_123"
	}`)
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), body, "/v1beta/models/gemini-pro:generateContent")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "Looking up the weather …" {
		t.Errorf("Segments=%v", nc.Segments)
	}
	if len(nc.ToolCallSegments) != 1 || !strings.Contains(nc.ToolCallSegments[0], `"get_weather"`) {
		t.Errorf("ToolCallSegments=%v", nc.ToolCallSegments)
	}
	if nc.Metadata["finishReason"] != "STOP" {
		t.Errorf("finishReason=%q", nc.Metadata["finishReason"])
	}
	if nc.Metadata["modelVersion"] != "gemini-3-pro-001" {
		t.Errorf("modelVersion=%q", nc.Metadata["modelVersion"])
	}
	if nc.Metadata["responseId"] != "resp_123" {
		t.Errorf("responseId=%q", nc.Metadata["responseId"])
	}
}

// TestExtractResponse_MultiCandidateFinishReasons pins that with n>1
// candidates the finishReasons are comma-joined in Metadata.
func TestExtractResponse_MultiCandidateFinishReasons(t *testing.T) {
	body := []byte(`{
		"candidates":[
			{"content":{"parts":[{"text":"a"}]},"finishReason":"STOP"},
			{"content":{"parts":[{"text":"b"}]},"finishReason":"MAX_TOKENS"}
		]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), body, "/v1beta/models/gemini-pro:generateContent")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if nc.Metadata["finishReason"] != "STOP,MAX_TOKENS" {
		t.Errorf("finishReason=%q want STOP,MAX_TOKENS", nc.Metadata["finishReason"])
	}
}

// TestExtractResponse_ThinkingPart pins Gemini 2.5+ thought=true output.
func TestExtractResponse_ThinkingPart(t *testing.T) {
	body := []byte(`{
		"candidates":[{
			"content":{"parts":[
				{"thought":true,"text":"Step 1: recall the formula …"},
				{"text":"Final answer: 7."}
			]}
		}]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), body, "/v1beta/models/gemini-pro:generateContent")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "Final answer: 7." {
		t.Errorf("Segments=%v", nc.Segments)
	}
	if len(nc.ReasoningSegments) != 1 || nc.ReasoningSegments[0] != "Step 1: recall the formula …" {
		t.Errorf("ReasoningSegments=%v", nc.ReasoningSegments)
	}
}

// TestExtractResponse_Extra covers response-side defence-in-depth.
func TestExtractResponse_Extra(t *testing.T) {
	body := []byte(`{
		"candidates":[{"content":{"parts":[{"text":"hi"}]}}],
		"x_future_response_field":{"new":"audit"}
	}`)
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), body, "/v1beta/models/gemini-pro:generateContent")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if _, ok := nc.Extra["x_future_response_field"]; !ok {
		t.Errorf("Extra missing x_future_response_field: %v", nc.Extra)
	}
}

// TestExtractStreamChunk_FunctionCall pins per-chunk functionCall delta.
func TestExtractStreamChunk_FunctionCall(t *testing.T) {
	chunk := []byte(`{
		"candidates":[{
			"content":{"parts":[
				{"functionCall":{"name":"send_email","args":{"to":"a@b.com"}}}
			]},
			"finishReason":"STOP"
		}]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/v1beta/models/gemini-pro:streamGenerateContent")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.ToolCallSegments) != 1 || !strings.Contains(nc.ToolCallSegments[0], `"send_email"`) {
		t.Errorf("ToolCallSegments=%v", nc.ToolCallSegments)
	}
	if nc.Metadata["finishReason"] != "STOP" {
		t.Errorf("finishReason=%q", nc.Metadata["finishReason"])
	}
}

// TestExtractStreamChunk_ThoughtPart pins streaming thought=true parts
// landing on ReasoningSegments.
func TestExtractStreamChunk_ThoughtPart(t *testing.T) {
	chunk := []byte(`{"candidates":[{"content":{"parts":[{"thought":true,"text":"Considering options …"}]}}]}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/v1beta/models/gemini-pro:streamGenerateContent")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.ReasoningSegments) != 1 || nc.ReasoningSegments[0] != "Considering options …" {
		t.Errorf("ReasoningSegments=%v", nc.ReasoningSegments)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("Segments must not absorb thought text: %v", nc.Segments)
	}
}

// Coverage gap: gemini.go line 88-90 (parts not array → skip), 132-134
// (cachedContent metadata), and Normalize.

// A content entry whose `parts` isn't an array must be skipped without
// erroring so the iterator continues to subsequent valid contents. This
// happens when a client sends a malformed turn mid-conversation.
func TestExtractRequest_PartsNotArray_Skipped(t *testing.T) {
	body := []byte(`{
		"contents":[
			{"role":"user","parts":"oops not an array"},
			{"role":"user","parts":[{"text":"valid"}]}
		]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/v1beta/models/gemini-pro:generateContent")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "valid" {
		t.Errorf("Segments=%v want [valid]", nc.Segments)
	}
}

// `cachedContent` (context-cache reference) must land in Metadata so
// cost analytics can attribute the cache hit.
func TestExtractRequest_CachedContentMetadata(t *testing.T) {
	body := []byte(`{
		"contents":[{"role":"user","parts":[{"text":"hi"}]}],
		"cachedContent":"cachedContents/abc123"
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/v1beta/models/gemini-pro:generateContent")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if nc.Metadata["cachedContent"] != "cachedContents/abc123" {
		t.Errorf("cachedContent=%q", nc.Metadata["cachedContent"])
	}
}

// snake_case `system_instruction` parts must be picked up alongside the
// camelCase variant — older clients still send the snake form.
func TestExtractRequest_SnakeCaseSystemInstruction(t *testing.T) {
	body := []byte(`{
		"system_instruction":{"parts":[{"text":"old format sys"}]},
		"contents":[{"role":"user","parts":[{"text":"hi"}]}]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/v1beta/models/gemini-pro:generateContent")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 2 || nc.Segments[0] != "old format sys" {
		t.Errorf("Segments=%v want [old format sys, hi]", nc.Segments)
	}
}

// `model` top-level field must land in Metadata (most Gemini clients put
// it in the URL but some explicitly add it to the body).
func TestExtractRequest_ModelMetadata(t *testing.T) {
	body := []byte(`{
		"model":"gemini-1.5-pro",
		"contents":[{"role":"user","parts":[{"text":"hi"}]}]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/v1beta/models/gemini-pro:generateContent")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if nc.Metadata["model"] != "gemini-1.5-pro" {
		t.Errorf("model=%q", nc.Metadata["model"])
	}
}

// Normalize — Tier-1 dispatch via the unified extract helper.

// TestExtractStreamChunk_NoMetadataAllocOnEmpty pins the optimisation
// that empty Metadata stays nil on the streaming hot path.
func TestExtractStreamChunk_NoMetadataAllocOnEmpty(t *testing.T) {
	chunk := []byte(`{"candidates":[{"content":{"parts":[{"text":"hi"}]}}]}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/v1beta/models/gemini-pro:streamGenerateContent")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if nc.Metadata != nil {
		t.Errorf("Metadata must be nil for empty-meta delta; got %v", nc.Metadata)
	}
}

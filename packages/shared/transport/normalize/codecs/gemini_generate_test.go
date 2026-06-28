package codecs

import (
	"context"
	"errors"
	"github.com/goccy/go-json"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

func TestGeminiGenerate_Request_TextOnly(t *testing.T) {
	body := `{
	  "contents": [
	    {"role": "user", "parts": [{"text": "hello world"}]}
	  ],
	  "generationConfig": {"temperature": 0.7, "maxOutputTokens": 100}
	}`
	n := NewGeminiGenerateNormalizer()
	got, err := n.Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionRequest, Model: "gemini-1.5-pro"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.Kind != core.KindAIChat {
		t.Fatalf("Kind: got %q want %q", got.Kind, core.KindAIChat)
	}
	if got.Protocol != "gemini-generate" {
		t.Fatalf("Protocol: got %q", got.Protocol)
	}
	if got.Model != "gemini-1.5-pro" {
		t.Fatalf("Model: got %q", got.Model)
	}
	if len(got.Messages) != 1 || got.Messages[0].Role != core.RoleUser {
		t.Fatalf("Messages: %+v", got.Messages)
	}
	if got.Messages[0].Content[0].Text != "hello world" {
		t.Fatalf("Text: %q", got.Messages[0].Content[0].Text)
	}
	if got.Params == nil || got.Params.Temperature == nil || *got.Params.Temperature != 0.7 {
		t.Fatalf("Params: %+v", got.Params)
	}
	if got.Params.MaxTokens == nil || *got.Params.MaxTokens != 100 {
		t.Fatalf("MaxTokens: %+v", got.Params.MaxTokens)
	}
}

func TestGeminiGenerate_Request_SystemInstruction(t *testing.T) {
	body := `{
	  "systemInstruction": {"parts": [{"text": "You are helpful."}]},
	  "contents": [
	    {"role": "user", "parts": [{"text": "hi"}]},
	    {"role": "model", "parts": [{"text": "hello"}]}
	  ]
	}`
	got, err := NewGeminiGenerateNormalizer().Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionRequest})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(got.Messages) != 3 {
		t.Fatalf("expected 3 messages (system + user + assistant), got %d: %+v", len(got.Messages), got.Messages)
	}
	if got.Messages[0].Role != core.RoleSystem {
		t.Fatalf("first message should be system, got %q", got.Messages[0].Role)
	}
	if got.Messages[0].Content[0].Text != "You are helpful." {
		t.Fatalf("system text: %q", got.Messages[0].Content[0].Text)
	}
	if got.Messages[2].Role != core.RoleAssistant {
		t.Fatalf("third message should be assistant (mapped from 'model'), got %q", got.Messages[2].Role)
	}
}

func TestGeminiGenerate_Request_Tools(t *testing.T) {
	body := `{
	  "contents": [{"role": "user", "parts": [{"text": "weather?"}]}],
	  "tools": [{"functionDeclarations": [
	    {"name": "get_weather", "description": "Look up weather", "parameters": {"type": "object", "properties": {"city": {"type": "string"}}}}
	  ]}]
	}`
	got, err := NewGeminiGenerateNormalizer().Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionRequest})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(got.Tools) != 1 || got.Tools[0].Name != "get_weather" {
		t.Fatalf("Tools: %+v", got.Tools)
	}
	if got.Tools[0].ParametersJSONSchema == nil {
		t.Fatalf("ParametersJSONSchema lost")
	}
}

func TestGeminiGenerate_Request_FunctionCallAndResponse(t *testing.T) {
	body := `{
	  "contents": [
	    {"role": "model", "parts": [
	      {"functionCall": {"id": "c1", "name": "get_weather", "args": {"city": "Boston"}}}
	    ]},
	    {"role": "user", "parts": [
	      {"functionResponse": {"id": "c1", "name": "get_weather", "response": {"temp": 72}}}
	    ]}
	  ]
	}`
	got, err := NewGeminiGenerateNormalizer().Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionRequest})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(got.Messages) != 2 {
		t.Fatalf("got %d messages", len(got.Messages))
	}
	// First message: assistant with tool_use
	if got.Messages[0].Role != core.RoleAssistant {
		t.Fatalf("msg0 role: %q", got.Messages[0].Role)
	}
	if len(got.Messages[0].Content) != 1 || got.Messages[0].Content[0].Type != core.ContentToolUse {
		t.Fatalf("msg0 content: %+v", got.Messages[0].Content)
	}
	if got.Messages[0].Content[0].ToolUse.Name != "get_weather" {
		t.Fatalf("tool name: %q", got.Messages[0].Content[0].ToolUse.Name)
	}
	if got.Messages[0].Content[0].ToolUse.Input["city"] != "Boston" {
		t.Fatalf("tool input: %+v", got.Messages[0].Content[0].ToolUse.Input)
	}
	// Second message: user with tool_result
	if got.Messages[1].Content[0].Type != core.ContentToolResult {
		t.Fatalf("msg1 content type: %v", got.Messages[1].Content[0].Type)
	}
	if !strings.Contains(got.Messages[1].Content[0].ToolResult.Output, "72") {
		t.Fatalf("tool result output missing payload: %q", got.Messages[1].Content[0].ToolResult.Output)
	}
}

func TestGeminiGenerate_Request_Thought(t *testing.T) {
	body := `{
	  "contents": [
	    {"role": "model", "parts": [
	      {"text": "let me think...", "thought": true},
	      {"text": "the answer is 42"}
	    ]}
	  ]
	}`
	got, err := NewGeminiGenerateNormalizer().Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionRequest})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(got.Messages[0].Content) != 2 {
		t.Fatalf("expected 2 content blocks, got %d", len(got.Messages[0].Content))
	}
	if got.Messages[0].Content[0].Type != core.ContentReasoning {
		t.Fatalf("expected first block to be reasoning, got %q", got.Messages[0].Content[0].Type)
	}
	if got.Messages[0].Content[1].Type != core.ContentText {
		t.Fatalf("expected second block to be text, got %q", got.Messages[0].Content[1].Type)
	}
}

func TestGeminiGenerate_Response_NonStream(t *testing.T) {
	body := `{
	  "modelVersion": "gemini-1.5-pro-002",
	  "candidates": [
	    {"content": {"role": "model", "parts": [{"text": "Paris is the capital."}]},
	     "finishReason": "STOP", "index": 0}
	  ],
	  "usageMetadata": {
	    "promptTokenCount": 10,
	    "candidatesTokenCount": 5,
	    "totalTokenCount": 15,
	    "cachedContentTokenCount": 3
	  }
	}`
	got, err := NewGeminiGenerateNormalizer().Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionResponse})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.Model != "gemini-1.5-pro-002" {
		t.Fatalf("Model: %q", got.Model)
	}
	if len(got.Messages) != 1 || got.Messages[0].Role != core.RoleAssistant {
		t.Fatalf("Messages: %+v", got.Messages)
	}
	if got.Messages[0].Content[0].Text != "Paris is the capital." {
		t.Fatalf("Text: %q", got.Messages[0].Content[0].Text)
	}
	if got.FinishReason != "STOP" {
		t.Fatalf("FinishReason: %q", got.FinishReason)
	}
	if got.Usage == nil {
		t.Fatalf("Usage is nil")
	}
	if got.Usage.PromptTokens == nil || *got.Usage.PromptTokens != 10 {
		t.Fatalf("PromptTokens: %+v", got.Usage.PromptTokens)
	}
	if got.Usage.CompletionTokens == nil || *got.Usage.CompletionTokens != 5 {
		t.Fatalf("CompletionTokens: %+v", got.Usage.CompletionTokens)
	}
	if got.Usage.CacheReadTokens == nil || *got.Usage.CacheReadTokens != 3 {
		t.Fatalf("CacheReadTokens: %+v", got.Usage.CacheReadTokens)
	}
}

func TestGeminiGenerate_Response_NoCandidates(t *testing.T) {
	body := `{"candidates": []}`
	_, err := NewGeminiGenerateNormalizer().Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionResponse})
	if !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("expected core.ErrUnsupported, got %v", err)
	}
}

func TestGeminiGenerate_Response_NonGeminiCandidatesDeclined(t *testing.T) {
	// A body that unmarshals but where NO candidate carries content and
	// usageMetadata is absent is not a Gemini response — it's some other
	// API's "candidates" list. The codec must decline with ErrUnsupported
	// so the registry walk continues to a structural projection, instead
	// of claiming an empty ai-chat payload.
	cases := map[string]string{
		"object candidates no gemini shape": `{"candidates":[{"name":"alice"},{"name":"bob"}]}`,
		"finish-reason-less empty objects":  `{"candidates":[{}],"total":1}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := NewGeminiGenerateNormalizer().Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionResponse})
			if !errors.Is(err, core.ErrUnsupported) {
				t.Fatalf("expected core.ErrUnsupported, got %v", err)
			}
		})
	}
}

func TestGeminiGenerate_Response_ContentlessWithUsageStillClaims(t *testing.T) {
	// A genuine empty Gemini response (safety block) carries finishReason
	// + usageMetadata and no content — that must still normalize, with
	// the finish reason and usage preserved for the audit row.
	body := `{"candidates":[{"finishReason":"SAFETY","index":0}],"usageMetadata":{"promptTokenCount":9,"totalTokenCount":9}}`
	got, err := NewGeminiGenerateNormalizer().Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionResponse})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.FinishReason != "SAFETY" {
		t.Fatalf("FinishReason = %q, want SAFETY", got.FinishReason)
	}
	if got.Usage == nil || got.Usage.PromptTokens == nil || *got.Usage.PromptTokens != 9 {
		t.Fatalf("Usage not preserved: %+v", got.Usage)
	}
}

func TestGeminiGenerate_Stream_SSE(t *testing.T) {
	raw := strings.Join([]string{
		`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"Hello"}]},"index":0}]}`,
		``,
		`data: {"candidates":[{"content":{"parts":[{"text":" world"}]},"index":0}]}`,
		``,
		`data: {"candidates":[{"content":{"parts":[{"text":"!"}]},"finishReason":"STOP","index":0}],"usageMetadata":{"promptTokenCount":4,"candidatesTokenCount":3,"totalTokenCount":7}}`,
		``,
	}, "\n")
	got, err := NewGeminiGenerateNormalizer().Normalize(context.Background(), []byte(raw), core.Meta{Direction: core.DirectionResponse, Stream: true})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !got.Stream {
		t.Fatalf("Stream flag lost")
	}
	if len(got.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(got.Messages))
	}
	if got.Messages[0].Role != core.RoleAssistant {
		t.Fatalf("role: %q", got.Messages[0].Role)
	}
	if got.Messages[0].Content[0].Text != "Hello world!" {
		t.Fatalf("stitched text: %q", got.Messages[0].Content[0].Text)
	}
	if got.FinishReason != "STOP" {
		t.Fatalf("FinishReason: %q", got.FinishReason)
	}
	if got.Usage == nil || got.Usage.PromptTokens == nil || *got.Usage.PromptTokens != 4 {
		t.Fatalf("Usage: %+v", got.Usage)
	}
}

// Stream confidence is frame coverage: a frame that parses as JSON but
// carries neither candidates nor usageMetadata counts toward the total
// only, so the operator sees an honest fraction of the stream the
// decoder understood instead of a field-shape score off the first frame.
func TestGeminiGenerate_StreamConfidenceIsFrameCoverage(t *testing.T) {
	raw := strings.Join([]string{
		`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"Hi"}]},"index":0}]}`,
		``,
		`data: {"ping":"keepalive"}`, // parses, no generateContent structure: total only
		``,
		`data: {"candidates":[{"content":{"parts":[{"text":"!"}]},"finishReason":"STOP","index":0}],"usageMetadata":{"promptTokenCount":4,"candidatesTokenCount":3,"totalTokenCount":7}}`,
		``,
	}, "\n")
	got, err := NewGeminiGenerateNormalizer().Normalize(context.Background(), []byte(raw), core.Meta{Direction: core.DirectionResponse, Stream: true})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	want := 2.0 / 3.0
	if got.Confidence != want {
		t.Errorf("confidence = %v, want %v (2 of 3 frames recognized)", got.Confidence, want)
	}
	if got.Messages[0].Content[0].Text != "Hi!" {
		t.Errorf("recognized frames not folded: %q", got.Messages[0].Content[0].Text)
	}
}

// TestGeminiGenerate_Stream_FinishOnlyStopFrame guards against the
// real prod regression where a Gemini streamGenerateContent terminator
// frame carrying ONLY {finishReason:"STOP", parts:[]} (no content
// delta) caused the normalizer to bail with core.ErrUnsupported, dropping
// the row to Tier-3 generic-http fallback. The 45% NULL detectedSpec
// rate on prod compliance-proxy + 32% on local ai-gateway was
// dominated by exactly this shape; the Tier-1 normalizer now
// recognises the STOP frame as a meaningful parse signal.
func TestGeminiGenerate_Stream_FinishOnlyStopFrame(t *testing.T) {
	raw := "data: {\"candidates\":[{\"content\":{\"parts\":[],\"role\":\"model\"},\"finishReason\":\"STOP\",\"index\":0}]}\n\n"
	got, err := NewGeminiGenerateNormalizer().Normalize(context.Background(), []byte(raw), core.Meta{Direction: core.DirectionResponse, Stream: true})
	if err != nil {
		t.Fatalf("unexpected err on STOP-only frame: %v", err)
	}
	if got.DetectedSpec != "gemini-generate" {
		t.Errorf("DetectedSpec = %q, want gemini-generate (frame should claim Tier-1, not fall through)", got.DetectedSpec)
	}
	if got.FinishReason != "STOP" {
		t.Errorf("FinishReason = %q, want STOP", got.FinishReason)
	}
	if got.Confidence < 0.40 {
		t.Errorf("Confidence = %.4f, want >= 0.40 floor", got.Confidence)
	}
	// Empty-content STOP frame legitimately produces an assistant message
	// with no content blocks — downstream consumers see the finishReason
	// and know the stream ended cleanly even without a visible delta.
	if len(got.Messages) == 0 {
		t.Errorf("want 1 assistant message even on STOP-only frame; got 0")
	}
}

// TestGeminiGenerate_Stream_UsageOnlyTailFrame guards against another
// Tier-3 fallthrough variant: a final flush frame that carries ONLY
// usageMetadata and no candidates[] at all. usage-only frames must
// claim the row so token counts are preserved in normalised storage
// instead of being lost to generic-http.
func TestGeminiGenerate_Stream_UsageOnlyTailFrame(t *testing.T) {
	raw := "data: {\"usageMetadata\":{\"promptTokenCount\":10,\"candidatesTokenCount\":5,\"totalTokenCount\":15}}\n\n"
	got, err := NewGeminiGenerateNormalizer().Normalize(context.Background(), []byte(raw), core.Meta{Direction: core.DirectionResponse, Stream: true})
	if err != nil {
		t.Fatalf("unexpected err on usage-only frame: %v", err)
	}
	if got.Usage == nil || got.Usage.PromptTokens == nil || *got.Usage.PromptTokens != 10 {
		t.Errorf("Usage tokens not preserved: %+v", got.Usage)
	}
}

func TestGeminiGenerate_Stream_SingleObjectFallback(t *testing.T) {
	// Vertex sometimes ships a JSON array or single object on
	// streamGenerateContent instead of SSE. Verify the fallback decoder.
	body := `{
	  "candidates": [
	    {"content": {"role": "model", "parts": [{"text": "ok"}]},
	     "finishReason": "STOP", "index": 0}
	  ]
	}`
	got, err := NewGeminiGenerateNormalizer().Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionResponse, Stream: true})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.Stream {
		t.Fatalf("Stream flag should be cleared by fallback path")
	}
	if len(got.Messages) != 1 || got.Messages[0].Content[0].Text != "ok" {
		t.Fatalf("Messages: %+v", got.Messages)
	}
}

func TestGeminiGenerate_Empty(t *testing.T) {
	for _, dir := range []core.Direction{core.DirectionRequest, core.DirectionResponse} {
		_, err := NewGeminiGenerateNormalizer().Normalize(context.Background(), nil, core.Meta{Direction: dir})
		if !errors.Is(err, core.ErrUnsupported) {
			t.Fatalf("dir=%s: expected core.ErrUnsupported, got %v", dir, err)
		}
	}
}

func TestGeminiGenerate_ID(t *testing.T) {
	if id := NewGeminiGenerateNormalizer().ID(); id != "gemini-generate" {
		t.Fatalf("ID: %q", id)
	}
}

func TestGeminiGenerate_JSON_RoundTrip(t *testing.T) {
	body := `{
	  "contents": [{"role": "user", "parts": [{"text": "hi"}]}]
	}`
	got, err := NewGeminiGenerateNormalizer().Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionRequest, Model: "gemini-1.5-flash"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back core.NormalizedPayload
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Protocol != "gemini-generate" {
		t.Fatalf("Protocol lost: %q", back.Protocol)
	}
}

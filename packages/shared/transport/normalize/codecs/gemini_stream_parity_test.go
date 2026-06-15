package codecs

import (
	"context"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// TestGeminiStreamParity_CorpusSSEText folds the captured gemini-sse-text
// stream (two cumulative-usage frames, second carrying finishReason) and
// asserts exact text concatenation plus last-frame-wins usage.
func TestGeminiStreamParity_CorpusSSEText(t *testing.T) {
	raw := readCorpusWire(t, "gemini-sse-text")
	got, err := NewGeminiGenerateNormalizer().Normalize(context.Background(), raw,
		core.Meta{Direction: core.DirectionResponse, Stream: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Kind != core.KindAIChat {
		t.Errorf("Kind = %q, want %q", got.Kind, core.KindAIChat)
	}
	if !got.Stream {
		t.Error("Stream = false, want true")
	}
	if got.Model != "gemini-2.5-flash-lite" {
		t.Errorf("Model = %q, want gemini-2.5-flash-lite", got.Model)
	}
	if got.DetectedSpec != "gemini-generate" {
		t.Errorf("DetectedSpec = %q, want gemini-generate", got.DetectedSpec)
	}
	if got.Confidence < 0.70 {
		t.Errorf("Confidence = %v, want >= 0.70 (Tier-1 acceptance threshold)", got.Confidence)
	}
	if got.FinishReason != "STOP" {
		t.Errorf("FinishReason = %q, want STOP", got.FinishReason)
	}

	if len(got.Messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(got.Messages))
	}
	msg := got.Messages[0]
	if msg.Role != core.RoleAssistant {
		t.Errorf("role = %q, want assistant", msg.Role)
	}
	if len(msg.Content) != 1 {
		t.Fatalf("content blocks = %d, want 1 (text only)", len(msg.Content))
	}
	const wantText = "A gateway is a point of entry or access to another system or network."
	if msg.Content[0].Type != core.ContentText || msg.Content[0].Text != wantText {
		t.Errorf("text block = %+v\nwant exact concatenation %q", msg.Content[0], wantText)
	}

	if got.Usage == nil {
		t.Fatal("Usage = nil, want populated from usageMetadata")
	}
	wantIntPtr(t, "PromptTokens", got.Usage.PromptTokens, 11)
	// candidatesTokenCount=15 + thoughtsTokenCount=0.
	wantIntPtr(t, "CompletionTokens", got.Usage.CompletionTokens, 15)
	wantIntPtr(t, "TotalTokens", got.Usage.TotalTokens, 26)
	if got.Usage.ReasoningTokens != nil {
		t.Errorf("ReasoningTokens = %d, want absent (no thoughtsTokenCount on wire)", *got.Usage.ReasoningTokens)
	}
	if got.Usage.CacheReadTokens != nil {
		t.Errorf("CacheReadTokens = %d, want absent (no cachedContentTokenCount on wire)", *got.Usage.CacheReadTokens)
	}
}

// TestGeminiStreamParity_FunctionCallPart pins the streaming functionCall
// shape: candidates[0].content.parts[0].functionCall arrives whole in one
// frame (Gemini does not fragment arguments the way OpenAI does) and must
// surface as a tool_use block with the decoded args map.
func TestGeminiStreamParity_FunctionCallPart(t *testing.T) {
	raw := strings.Join([]string{
		`data: {"candidates":[{"content":{"parts":[{"functionCall":{"name":"get_weather","args":{"location":"Paris","unit":"celsius"}}}],"role":"model"},"index":0}],"modelVersion":"gemini-2.5-flash"}`,
		``,
		`data: {"candidates":[{"content":{"parts":[],"role":"model"},"finishReason":"STOP","index":0}],"usageMetadata":{"promptTokenCount":21,"candidatesTokenCount":9,"totalTokenCount":30},"modelVersion":"gemini-2.5-flash"}`,
		``,
	}, "\n")

	got, err := NewGeminiGenerateNormalizer().Normalize(context.Background(), []byte(raw),
		core.Meta{Direction: core.DirectionResponse, Stream: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Model != "gemini-2.5-flash" {
		t.Errorf("Model = %q, want gemini-2.5-flash", got.Model)
	}
	if got.FinishReason != "STOP" {
		t.Errorf("FinishReason = %q, want STOP", got.FinishReason)
	}
	if len(got.Messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(got.Messages))
	}
	msg := got.Messages[0]
	if msg.Role != core.RoleAssistant {
		t.Errorf("role = %q, want assistant", msg.Role)
	}
	if len(msg.Content) != 1 {
		t.Fatalf("content blocks = %d, want 1 (tool_use only)", len(msg.Content))
	}
	blk := msg.Content[0]
	if blk.Type != core.ContentToolUse || blk.ToolUse == nil {
		t.Fatalf("block = %+v, want tool_use", blk)
	}
	if blk.ToolUse.Name != "get_weather" {
		t.Errorf("tool name = %q, want get_weather", blk.ToolUse.Name)
	}
	if loc, ok := blk.ToolUse.Input["location"].(string); !ok || loc != "Paris" {
		t.Errorf(`Input["location"] = %v, want "Paris"`, blk.ToolUse.Input["location"])
	}
	if unit, ok := blk.ToolUse.Input["unit"].(string); !ok || unit != "celsius" {
		t.Errorf(`Input["unit"] = %v, want "celsius"`, blk.ToolUse.Input["unit"])
	}

	if got.Usage == nil {
		t.Fatal("Usage = nil, want populated from usageMetadata")
	}
	wantIntPtr(t, "PromptTokens", got.Usage.PromptTokens, 21)
	wantIntPtr(t, "CompletionTokens", got.Usage.CompletionTokens, 9)
	wantIntPtr(t, "TotalTokens", got.Usage.TotalTokens, 30)
}

// TestGeminiStreamParity_ThoughtPartsFoldToReasoningBlock pins the Gemini
// extended-thinking stream shape: parts with thought:true accumulate into
// a ContentReasoning block placed before the visible text, and
// thoughtsTokenCount feeds both ReasoningTokens and the CompletionTokens
// sum (candidatesTokenCount + thoughtsTokenCount).
func TestGeminiStreamParity_ThoughtPartsFoldToReasoningBlock(t *testing.T) {
	raw := strings.Join([]string{
		`data: {"candidates":[{"content":{"parts":[{"text":"Comparing ","thought":true}],"role":"model"},"index":0}],"modelVersion":"gemini-2.5-pro"}`,
		``,
		`data: {"candidates":[{"content":{"parts":[{"text":"routes.","thought":true}],"role":"model"},"index":0}],"modelVersion":"gemini-2.5-pro"}`,
		``,
		`data: {"candidates":[{"content":{"parts":[{"text":"Take the train."}],"role":"model"},"finishReason":"STOP","index":0}],"usageMetadata":{"promptTokenCount":12,"candidatesTokenCount":5,"thoughtsTokenCount":18,"totalTokenCount":35},"modelVersion":"gemini-2.5-pro"}`,
		``,
	}, "\n")

	got, err := NewGeminiGenerateNormalizer().Normalize(context.Background(), []byte(raw),
		core.Meta{Direction: core.DirectionResponse, Stream: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.FinishReason != "STOP" {
		t.Errorf("FinishReason = %q, want STOP", got.FinishReason)
	}
	if len(got.Messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(got.Messages))
	}
	msg := got.Messages[0]
	if msg.Role != core.RoleAssistant {
		t.Errorf("role = %q, want assistant", msg.Role)
	}
	if len(msg.Content) != 2 {
		t.Fatalf("content blocks = %d, want 2 (reasoning + text)", len(msg.Content))
	}
	if msg.Content[0].Type != core.ContentReasoning || msg.Content[0].Text != "Comparing routes." {
		t.Errorf("block[0] = %+v, want reasoning %q", msg.Content[0], "Comparing routes.")
	}
	if msg.Content[1].Type != core.ContentText || msg.Content[1].Text != "Take the train." {
		t.Errorf("block[1] = %+v, want text %q", msg.Content[1], "Take the train.")
	}

	if got.Usage == nil {
		t.Fatal("Usage = nil, want populated from usageMetadata")
	}
	wantIntPtr(t, "PromptTokens", got.Usage.PromptTokens, 12)
	// candidatesTokenCount=5 + thoughtsTokenCount=18.
	wantIntPtr(t, "CompletionTokens", got.Usage.CompletionTokens, 23)
	wantIntPtr(t, "TotalTokens", got.Usage.TotalTokens, 35)
	wantIntPtr(t, "ReasoningTokens", got.Usage.ReasoningTokens, 18)
}

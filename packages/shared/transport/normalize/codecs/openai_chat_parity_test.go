package codecs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// readCorpusWire loads the captured wire bytes for a conformance corpus
// case. The corpus is the shared source of truth between the golden
// conformance runner and these codec-level parity tests: both must
// decode the same bytes to the same business values.
func readCorpusWire(t *testing.T, caseName string) []byte {
	t.Helper()
	p := filepath.Join("..", "conformance", "corpus", caseName, "wire")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read corpus wire %s: %v", p, err)
	}
	return b
}

// wantIntPtr asserts a *int usage field carries exactly want. A nil
// pointer is reported as "absent" so token-accounting regressions show
// the precise field that went missing.
func wantIntPtr(t *testing.T, field string, got *int, want int) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s = absent, want %d", field, want)
	}
	if *got != want {
		t.Errorf("%s = %d, want %d", field, *got, want)
	}
}

// TestOpenAIChatParity_CorpusSSEToolCalls folds the captured
// openai-sse-toolcalls stream (7 tool-call delta frames + finish +
// usage tail) and asserts the assembled business outcome: one assistant
// message whose tool_use block carries the fully stitched call.
func TestOpenAIChatParity_CorpusSSEToolCalls(t *testing.T) {
	raw := readCorpusWire(t, "openai-sse-toolcalls")
	got, err := NewOpenAIChatNormalizer().Normalize(context.Background(), raw,
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
	if got.Model != "gpt-4o-mini-2024-07-18" {
		t.Errorf("Model = %q, want gpt-4o-mini-2024-07-18", got.Model)
	}
	if got.DetectedSpec != "openai-chat" {
		t.Errorf("DetectedSpec = %q, want openai-chat", got.DetectedSpec)
	}
	if got.Confidence < 0.70 {
		t.Errorf("Confidence = %v, want >= 0.70 (Tier-1 acceptance threshold)", got.Confidence)
	}
	if got.FinishReason != "tool_calls" {
		t.Errorf("FinishReason = %q, want tool_calls", got.FinishReason)
	}

	if len(got.Messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(got.Messages))
	}
	msg := got.Messages[0]
	if msg.Role != core.RoleAssistant {
		t.Errorf("role = %q, want assistant", msg.Role)
	}
	if msg.FinishReason != "tool_calls" {
		t.Errorf("message finishReason = %q, want tool_calls", msg.FinishReason)
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
	if blk.ToolUse.CallID != "call_N0btkRj1vx1nTfnr9O88DTlZ" {
		t.Errorf("callID = %q, want call_N0btkRj1vx1nTfnr9O88DTlZ", blk.ToolUse.CallID)
	}
	// The arguments arrive as 5 separate fragments ({" / location / ":" /
	// Paris / "}); the fold must reassemble valid JSON.
	if loc, ok := blk.ToolUse.Input["location"].(string); !ok || loc != "Paris" {
		t.Errorf(`Input["location"] = %v, want "Paris"`, blk.ToolUse.Input["location"])
	}

	if got.Usage == nil {
		t.Fatal("Usage = nil, want populated from the usage tail frame")
	}
	wantIntPtr(t, "PromptTokens", got.Usage.PromptTokens, 59)
	wantIntPtr(t, "CompletionTokens", got.Usage.CompletionTokens, 14)
	wantIntPtr(t, "TotalTokens", got.Usage.TotalTokens, 73)
	if got.Usage.CacheReadTokens != nil {
		t.Errorf("CacheReadTokens = %d, want absent (wire cached_tokens=0)", *got.Usage.CacheReadTokens)
	}
	if got.Usage.ReasoningTokens != nil {
		t.Errorf("ReasoningTokens = %d, want absent (wire reasoning_tokens=0)", *got.Usage.ReasoningTokens)
	}
}

// TestOpenAIChatParity_CorpusSSEText folds the captured openai-sse-text
// stream and asserts exact delta concatenation plus the usage tail.
func TestOpenAIChatParity_CorpusSSEText(t *testing.T) {
	raw := readCorpusWire(t, "openai-sse-text")
	got, err := NewOpenAIChatNormalizer().Normalize(context.Background(), raw,
		core.Meta{Direction: core.DirectionResponse, Stream: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Model != "gpt-4o-mini-2024-07-18" {
		t.Errorf("Model = %q, want gpt-4o-mini-2024-07-18", got.Model)
	}
	if got.FinishReason != "stop" {
		t.Errorf("FinishReason = %q, want stop", got.FinishReason)
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
	const wantText = "A gateway is a network node that serves as an access point to another network," +
		" often facilitating communication between different protocols or systems."
	if msg.Content[0].Type != core.ContentText || msg.Content[0].Text != wantText {
		t.Errorf("text block = %+v\nwant exact concatenation %q", msg.Content[0], wantText)
	}

	if got.Usage == nil {
		t.Fatal("Usage = nil, want populated from the usage tail frame")
	}
	wantIntPtr(t, "PromptTokens", got.Usage.PromptTokens, 17)
	wantIntPtr(t, "CompletionTokens", got.Usage.CompletionTokens, 25)
	wantIntPtr(t, "TotalTokens", got.Usage.TotalTokens, 42)
}

// TestOpenAIChatParity_ReasoningContentDeltasFold pins the DeepSeek /
// Moonshot streaming reasoning shape: choices[0].delta.reasoning_content
// fragments fold into a single ContentReasoning block placed before the
// visible text, and the chars/3.5 derivation supplies ReasoningTokens
// when the wire usage omits the count.
func TestOpenAIChatParity_ReasoningContentDeltasFold(t *testing.T) {
	raw := strings.Join([]string{
		`data: {"model":"deepseek-reasoner","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"First "},"finish_reason":null}]}`,
		``,
		`data: {"model":"deepseek-reasoner","choices":[{"index":0,"delta":{"reasoning_content":"weigh both options."},"finish_reason":null}]}`,
		``,
		`data: {"model":"deepseek-reasoner","choices":[{"index":0,"delta":{"content":"Take "},"finish_reason":null}]}`,
		``,
		`data: {"model":"deepseek-reasoner","choices":[{"index":0,"delta":{"content":"the train."},"finish_reason":null}]}`,
		``,
		`data: {"model":"deepseek-reasoner","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	got, err := NewOpenAIChatNormalizer().Normalize(context.Background(), []byte(raw),
		core.Meta{Direction: core.DirectionResponse, Stream: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Model != "deepseek-reasoner" {
		t.Errorf("Model = %q, want deepseek-reasoner", got.Model)
	}
	if got.FinishReason != "stop" {
		t.Errorf("FinishReason = %q, want stop", got.FinishReason)
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
	if msg.Content[0].Type != core.ContentReasoning || msg.Content[0].Text != "First weigh both options." {
		t.Errorf("block[0] = %+v, want reasoning %q", msg.Content[0], "First weigh both options.")
	}
	if msg.Content[1].Type != core.ContentText || msg.Content[1].Text != "Take the train." {
		t.Errorf("block[1] = %+v, want text %q", msg.Content[1], "Take the train.")
	}
	// No wire usage frame: the codec derives ReasoningTokens from the
	// accumulated reasoning text — 25 chars * 2 / 7 = 7.
	if got.Usage == nil {
		t.Fatal("Usage = nil, want derived ReasoningTokens")
	}
	wantIntPtr(t, "ReasoningTokens", got.Usage.ReasoningTokens, 7)
}

// TestOpenAIChatParity_WireReasoningTokensWinOverDerivation pins the
// precedence rule: when the usage tail reports
// completion_tokens_details.reasoning_tokens, that wire value is kept and
// the chars/3.5 derivation must not overwrite it.
func TestOpenAIChatParity_WireReasoningTokensWinOverDerivation(t *testing.T) {
	raw := strings.Join([]string{
		`data: {"model":"deepseek-reasoner","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"hidden chain"},"finish_reason":null}]}`,
		``,
		`data: {"model":"deepseek-reasoner","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}`,
		``,
		`data: {"model":"deepseek-reasoner","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":40,"total_tokens":45,"completion_tokens_details":{"reasoning_tokens":33}}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	got, err := NewOpenAIChatNormalizer().Normalize(context.Background(), []byte(raw),
		core.Meta{Direction: core.DirectionResponse, Stream: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Usage == nil {
		t.Fatal("Usage = nil, want wire usage")
	}
	wantIntPtr(t, "PromptTokens", got.Usage.PromptTokens, 5)
	wantIntPtr(t, "CompletionTokens", got.Usage.CompletionTokens, 40)
	wantIntPtr(t, "TotalTokens", got.Usage.TotalTokens, 45)
	wantIntPtr(t, "ReasoningTokens", got.Usage.ReasoningTokens, 33)
}

// The alternate `reasoning` wire name (xAI / OpenRouter) must fold into
// the same ContentReasoning block as `reasoning_content`.
func TestOpenAIChatParity_ReasoningAliasDeltasFold(t *testing.T) {
	raw := []byte("data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"grok-3\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"reasoning\":\"thinking about \"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"grok-3\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning\":\"the answer\"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"grok-3\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"42\"},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n")
	n := &OpenAIChatNormalizer{}
	got, err := n.Normalize(context.Background(), raw, core.Meta{AdapterType: "openai", Direction: core.DirectionResponse, Stream: true})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	var reasoning, text string
	for _, m := range got.Messages {
		for _, b := range m.Content {
			switch b.Type {
			case core.ContentReasoning:
				reasoning = b.Text
			case core.ContentText:
				text = b.Text
			}
		}
	}
	if reasoning != "thinking about the answer" {
		t.Fatalf("reasoning alias not folded: %q", reasoning)
	}
	if text != "42" {
		t.Fatalf("text = %q, want 42", text)
	}
}

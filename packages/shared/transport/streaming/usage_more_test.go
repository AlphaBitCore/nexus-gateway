package streaming

import (
	"context"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

// TestNewUsageAccumulator_BedrockAnthropicModel — Bedrock + anthropic.*
// prefix yields the anthropic accumulator (covers the prefix branch).
func TestNewUsageAccumulator_BedrockAnthropicModel(t *testing.T) {
	acc := NewUsageAccumulator("bedrock", "anthropic.claude-3-haiku")
	if _, ok := acc.(*anthropicAccumulator); !ok {
		t.Fatalf("got %T, want *anthropicAccumulator", acc)
	}
}

// TestNewUsageAccumulator_BedrockNonAnthropic_BufferingFallback — Bedrock
// with non-anthropic model returns the generic bufferingAccumulator.
func TestNewUsageAccumulator_BedrockNonAnthropic_BufferingFallback(t *testing.T) {
	acc := NewUsageAccumulator("bedrock", "amazon.titan-text")
	if _, ok := acc.(*bufferingAccumulator); !ok {
		t.Fatalf("got %T, want *bufferingAccumulator", acc)
	}
}

// TestNewUsageAccumulator_VertexUnknownPublisher_Nil — vertex with an
// unknown publisher prefix returns nil so callers know to skip wiring.
func TestNewUsageAccumulator_VertexUnknownPublisher_Nil(t *testing.T) {
	if acc := NewUsageAccumulator("vertex", "meta/llama-3-70b"); acc != nil {
		t.Errorf("vertex meta/* should return nil, got %T", acc)
	}
}

// TestBufferingAccumulator_Feed_IgnoresNoise — nil event, Done event, and
// empty-Data event are all no-ops.
func TestBufferingAccumulator_Feed_IgnoresNoise(t *testing.T) {
	a := &bufferingAccumulator{tokenizer: heuristicGPT{}}
	a.Feed(nil)
	a.Feed(&SSEEvent{Done: true})
	a.Feed(&SSEEvent{Data: ""})
	if a.textBuf.Len() != 0 {
		t.Errorf("textBuf accumulated noise: %q", a.textBuf.String())
	}
	// One real frame should be captured.
	a.Feed(&SSEEvent{Data: "hello"})
	if a.textBuf.String() != "hello" {
		t.Errorf("textBuf = %q, want hello", a.textBuf.String())
	}
}

// TestBufferingAccumulator_Finalize_EstimatedFromBuffer — Finalize on the
// generic accumulator runs the tokenizer and reports streaming_estimated.
func TestBufferingAccumulator_Finalize_EstimatedFromBuffer(t *testing.T) {
	a := &bufferingAccumulator{tokenizer: heuristicGPT{}}
	a.Feed(&SSEEvent{Data: "some captured response text"})
	um := a.Finalize(context.Background())
	if um.Status != traffic.UsageStatusStreamingEstimated {
		t.Errorf("status = %q, want streaming_estimated", um.Status)
	}
	if um.CompletionTokens == nil || *um.CompletionTokens < 1 {
		t.Errorf("completion estimate missing/zero: %v", um.CompletionTokens)
	}
}

// TestBufferingAccumulator_Finalize_NilTokenizer_Unavailable — Finalize
// with a nil tokenizer returns streaming_unavailable.
func TestBufferingAccumulator_Finalize_NilTokenizer_Unavailable(t *testing.T) {
	a := &bufferingAccumulator{tokenizer: nil}
	a.Feed(&SSEEvent{Data: "anything"})
	um := a.Finalize(context.Background())
	if um.Status != traffic.UsageStatusStreamingUnavailable {
		t.Errorf("status = %q, want streaming_unavailable", um.Status)
	}
}

// TestOpenAIAccumulator_Feed_IgnoresInvalidJSON — non-JSON Data fields
// must not crash and must not corrupt the textBuf.
func TestOpenAIAccumulator_Feed_IgnoresInvalidJSON(t *testing.T) {
	a := &openaiAccumulator{tokenizer: heuristicGPT{}}
	a.Feed(&SSEEvent{Data: "not valid json"})
	if a.textBuf.Len() != 0 || a.prompt != nil || a.completion != nil {
		t.Errorf("invalid JSON should be ignored; textBuf=%q prompt=%v completion=%v",
			a.textBuf.String(), a.prompt, a.completion)
	}
}

// TestOpenAIAccumulator_Feed_OnlyPromptTokens — `usage.prompt_tokens` alone
// (no completion_tokens) yields reported status with PromptTokens set and
// CompletionTokens nil.
func TestOpenAIAccumulator_Feed_OnlyPromptTokens(t *testing.T) {
	a := &openaiAccumulator{tokenizer: heuristicGPT{}}
	a.Feed(&SSEEvent{Data: `{"usage":{"prompt_tokens":5}}`})
	um := a.Finalize(context.Background())
	if um.Status != traffic.UsageStatusStreamingReported {
		t.Errorf("status = %q, want streaming_reported", um.Status)
	}
	if um.PromptTokens == nil || *um.PromptTokens != 5 {
		t.Errorf("prompt = %v, want 5", um.PromptTokens)
	}
	if um.CompletionTokens != nil {
		t.Errorf("completion = %v, want nil", um.CompletionTokens)
	}
}

// TestOpenAIAccumulator_Feed_OnlyCompletionTokens — same as above but
// completion-only.
func TestOpenAIAccumulator_Feed_OnlyCompletionTokens(t *testing.T) {
	a := &openaiAccumulator{tokenizer: heuristicGPT{}}
	a.Feed(&SSEEvent{Data: `{"usage":{"completion_tokens":9}}`})
	um := a.Finalize(context.Background())
	if um.Status != traffic.UsageStatusStreamingReported {
		t.Errorf("status = %q, want streaming_reported", um.Status)
	}
	if um.CompletionTokens == nil || *um.CompletionTokens != 9 {
		t.Errorf("completion = %v, want 9", um.CompletionTokens)
	}
	if um.PromptTokens != nil {
		t.Errorf("prompt = %v, want nil", um.PromptTokens)
	}
}

// TestAnthropicAccumulator_Feed_IgnoresUnknownEvent — events that aren't
// message_start / message_delta / content_block_delta are ignored.
func TestAnthropicAccumulator_Feed_IgnoresUnknownEvent(t *testing.T) {
	a := &anthropicAccumulator{tokenizer: heuristicAnthropic{}}
	a.Feed(&SSEEvent{Event: "ping", Data: `{"type":"ping"}`})
	if a.prompt != nil || a.completion != nil || a.textBuf.Len() != 0 {
		t.Error("unknown event mutated state")
	}
}

// TestAnthropicAccumulator_Finalize_EstimatedFromContentBlockDelta —
// content_block_delta frames are buffered; without usage frames, Finalize
// falls back to tokenizer estimation.
func TestAnthropicAccumulator_Finalize_EstimatedFromContentBlockDelta(t *testing.T) {
	a := &anthropicAccumulator{tokenizer: heuristicAnthropic{}}
	a.Feed(&SSEEvent{Event: "content_block_delta", Data: `{"delta":{"text":"hello"}}`})
	a.Feed(&SSEEvent{Event: "content_block_delta", Data: `{"delta":{"text":" world"}}`})
	um := a.Finalize(context.Background())
	if um.Status != traffic.UsageStatusStreamingEstimated {
		t.Errorf("status = %q, want streaming_estimated", um.Status)
	}
	if um.CompletionTokens == nil || *um.CompletionTokens < 1 {
		t.Errorf("completion = %v", um.CompletionTokens)
	}
}

// TestAnthropicAccumulator_Feed_IgnoresInvalidJSON — invalid JSON is
// silently dropped.
func TestAnthropicAccumulator_Feed_IgnoresInvalidJSON(t *testing.T) {
	a := &anthropicAccumulator{tokenizer: heuristicAnthropic{}}
	a.Feed(&SSEEvent{Event: "message_start", Data: "bogus{"})
	if a.prompt != nil {
		t.Error("invalid JSON should not set prompt")
	}
}

// TestAnthropicAccumulator_Feed_IgnoresNil_DoneAndEmpty — nil event, done
// event, empty Data all no-op.
func TestAnthropicAccumulator_Feed_IgnoresNil_DoneAndEmpty(t *testing.T) {
	a := &anthropicAccumulator{tokenizer: heuristicAnthropic{}}
	a.Feed(nil)
	a.Feed(&SSEEvent{Done: true})
	a.Feed(&SSEEvent{Data: ""})
	if a.textBuf.Len() != 0 || a.prompt != nil {
		t.Error("noise mutated accumulator state")
	}
}

// TestGeminiAccumulator_Feed_ThoughtsAddedToCompletion — thoughtsTokenCount
// must be summed into CompletionTokens alongside candidatesTokenCount so
// total_tokens = prompt_tokens + completion_tokens holds.
func TestGeminiAccumulator_Feed_ThoughtsAddedToCompletion(t *testing.T) {
	a := &geminiAccumulator{tokenizer: heuristicSentencePiece{}}
	a.Feed(&SSEEvent{Data: `{"usageMetadata":{"promptTokenCount":30,"candidatesTokenCount":12,"thoughtsTokenCount":5}}`})
	um := a.Finalize(context.Background())
	if um.PromptTokens == nil || *um.PromptTokens != 30 {
		t.Errorf("prompt = %v, want 30", um.PromptTokens)
	}
	if um.CompletionTokens == nil || *um.CompletionTokens != 17 {
		t.Errorf("completion = %v, want 17 (candidates+thoughts)", um.CompletionTokens)
	}
}

// TestGeminiAccumulator_Feed_IgnoresNil_DoneAndEmpty — guards.
func TestGeminiAccumulator_Feed_IgnoresNil_DoneAndEmpty(t *testing.T) {
	a := &geminiAccumulator{tokenizer: heuristicSentencePiece{}}
	a.Feed(nil)
	a.Feed(&SSEEvent{Done: true})
	a.Feed(&SSEEvent{Data: ""})
	if a.textBuf.Len() != 0 || a.prompt != nil {
		t.Error("noise mutated accumulator state")
	}
}

// TestGeminiAccumulator_Feed_IgnoresInvalidJSON — invalid JSON is dropped.
func TestGeminiAccumulator_Feed_IgnoresInvalidJSON(t *testing.T) {
	a := &geminiAccumulator{tokenizer: heuristicSentencePiece{}}
	a.Feed(&SSEEvent{Data: "not-json"})
	if a.prompt != nil || a.textBuf.Len() != 0 {
		t.Error("invalid JSON should be ignored")
	}
}

// TestGeminiAccumulator_Finalize_EstimatedFromTextBuffer — no usageMetadata
// frame; Finalize must fall back to tokenizer estimation over the buffered
// candidates[].content.parts[].text segments.
func TestGeminiAccumulator_Finalize_EstimatedFromTextBuffer(t *testing.T) {
	a := &geminiAccumulator{tokenizer: heuristicSentencePiece{}}
	a.Feed(&SSEEvent{Data: `{"candidates":[{"content":{"parts":[{"text":"hello"}]}}]}`})
	a.Feed(&SSEEvent{Data: `{"candidates":[{"content":{"parts":[{"text":" world"}]}}]}`})
	um := a.Finalize(context.Background())
	if um.Status != traffic.UsageStatusStreamingEstimated {
		t.Errorf("status = %q, want streaming_estimated", um.Status)
	}
	if um.CompletionTokens == nil || *um.CompletionTokens < 1 {
		t.Errorf("completion = %v, want >=1", um.CompletionTokens)
	}
}

// TestGeminiAccumulator_Feed_OnlyPromptToken — promptTokenCount alone
// yields reported with completion = 0 (no candidates/thoughts seen).
func TestGeminiAccumulator_Feed_OnlyPromptToken(t *testing.T) {
	a := &geminiAccumulator{tokenizer: heuristicSentencePiece{}}
	a.Feed(&SSEEvent{Data: `{"usageMetadata":{"promptTokenCount":7}}`})
	um := a.Finalize(context.Background())
	if um.Status != traffic.UsageStatusStreamingReported {
		t.Errorf("status = %q, want streaming_reported", um.Status)
	}
	if um.PromptTokens == nil || *um.PromptTokens != 7 {
		t.Errorf("prompt = %v, want 7", um.PromptTokens)
	}
	if um.CompletionTokens == nil || *um.CompletionTokens != 0 {
		t.Errorf("completion = %v, want 0", um.CompletionTokens)
	}
}

// TestSetPromptText_EmptyString_NoOp — empty input does nothing on each
// known accumulator type.
func TestSetPromptText_EmptyString_NoOp(t *testing.T) {
	a := &openaiAccumulator{}
	SetPromptText(a, "")
	if a.promptText != "" {
		t.Errorf("openai promptText = %q, want empty", a.promptText)
	}
}

// TestSetPromptText_AllSupportedTypes — exercises each switch arm.
func TestSetPromptText_AllSupportedTypes(t *testing.T) {
	oa := &openaiAccumulator{}
	SetPromptText(oa, "prompt-openai")
	if oa.promptText != "prompt-openai" {
		t.Errorf("openai = %q", oa.promptText)
	}

	an := &anthropicAccumulator{}
	SetPromptText(an, "prompt-anthropic")
	if an.promptText != "prompt-anthropic" {
		t.Errorf("anthropic = %q", an.promptText)
	}

	ge := &geminiAccumulator{}
	SetPromptText(ge, "prompt-gemini")
	if ge.promptText != "prompt-gemini" {
		t.Errorf("gemini = %q", ge.promptText)
	}
}

// TestSetPromptText_UnsupportedType_NoPanic — calling on a type that
// isn't a known accumulator (the bufferingAccumulator default arm) is a
// no-op rather than a panic.
func TestSetPromptText_UnsupportedType_NoPanic(t *testing.T) {
	buf := &bufferingAccumulator{}
	SetPromptText(buf, "some-prompt")
	// bufferingAccumulator has no promptText field; calling must not panic.
}

// TestOpenAIAccumulator_Finalize_Estimated_NoPromptText — no usage frame,
// no prompt text → tokenizer runs on completion only; status estimated,
// PromptTokens stays nil (because prompt string is empty).
func TestOpenAIAccumulator_Finalize_Estimated_NoPromptText(t *testing.T) {
	a := &openaiAccumulator{tokenizer: heuristicGPT{}}
	a.Feed(&SSEEvent{Data: `{"choices":[{"delta":{"content":"abcdefghij"}}]}`})
	um := a.Finalize(context.Background())
	if um.Status != traffic.UsageStatusStreamingEstimated {
		t.Errorf("status = %q, want streaming_estimated", um.Status)
	}
	if um.PromptTokens != nil {
		t.Errorf("PromptTokens = %v, want nil when no prompt text set", um.PromptTokens)
	}
	if um.CompletionTokens == nil {
		t.Error("CompletionTokens nil, want estimate")
	}
}

// TestEstimateWithTokenizer_OnlyPromptError_StillEstimated — when ONE of
// the two tokenizer calls errors but the other succeeds, status is still
// estimated (only both-failing flips to unavailable).
func TestEstimateWithTokenizer_OnlyPromptError_StillEstimated(t *testing.T) {
	tok := &errOnTextTokenizer{failOn: "fail-this", delegate: heuristicGPT{}}
	um := estimateWithTokenizer(context.Background(), tok, "fail-this", "abcdef")
	if um.Status != traffic.UsageStatusStreamingEstimated {
		t.Errorf("status = %q, want streaming_estimated (only one branch errored)", um.Status)
	}
	if um.PromptTokens != nil {
		t.Errorf("PromptTokens = %v, want nil (prompt branch errored)", um.PromptTokens)
	}
	if um.CompletionTokens == nil {
		t.Error("CompletionTokens nil, want estimate")
	}
}

// errOnTextTokenizer fails on a specific input string and delegates
// everything else to the inner tokenizer.
type errOnTextTokenizer struct {
	failOn   string
	delegate Tokenizer
}

func (e *errOnTextTokenizer) Count(ctx context.Context, text string) (int, error) {
	if text == e.failOn {
		return 0, errInjected
	}
	return e.delegate.Count(ctx, text)
}

var errInjected = errInjectedErr{}

type errInjectedErr struct{}

func (errInjectedErr) Error() string { return "injected" }

// TestHeuristicAnthropicCount_EmptyAndMinimum — empty returns 0; tiny
// inputs floor at 1.
func TestHeuristicAnthropicCount_EmptyAndMinimum(t *testing.T) {
	tok := heuristicAnthropic{}
	if n, err := tok.Count(context.Background(), ""); err != nil || n != 0 {
		t.Errorf("empty -> (%d, %v), want (0, nil)", n, err)
	}
	if n, _ := tok.Count(context.Background(), "a"); n != 1 {
		t.Errorf("'a' -> %d, want 1", n)
	}
}

// TestHeuristicSentencePieceCount_AllArms — empty=0, minimum floor=1, exact-4
// = 1, exact-5 = 2 (covers all branches of Count).
func TestHeuristicSentencePieceCount_AllArms(t *testing.T) {
	tok := heuristicSentencePiece{}
	cases := map[string]int{
		"":      0,
		"a":     1,
		"abcd":  1,
		"abcde": 2,
	}
	for text, want := range cases {
		got, err := tok.Count(context.Background(), text)
		if err != nil {
			t.Errorf("Count(%q) err = %v", text, err)
		}
		if got != want {
			t.Errorf("Count(%q) = %d, want %d", text, got, want)
		}
	}
}

// TestHeuristicGPTCount_LargeCJK — multi-byte input is counted by runes
// not bytes.
func TestHeuristicGPTCount_LargeCJK(t *testing.T) {
	text := strings.Repeat("你", 16) // 16 runes, 48 bytes
	got, err := heuristicGPT{}.Count(context.Background(), text)
	if err != nil {
		t.Errorf("err = %v", err)
	}
	// 16/4 = 4 tokens, not 48/4 = 12.
	if got != 4 {
		t.Errorf("Count = %d, want 4 (rune-based)", got)
	}
}

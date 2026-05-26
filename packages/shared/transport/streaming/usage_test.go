package streaming

import (
	"context"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

// feedSSE parses the given SSE fixture and feeds every event to acc.
func feedSSE(t *testing.T, acc UsageAccumulator, fixture string) {
	t.Helper()
	parser := NewSSEParser(strings.NewReader(fixture))
	for {
		evt, err := parser.Next()
		if err != nil {
			break
		}
		acc.Feed(evt)
		if evt.Done {
			break
		}
	}
}

func TestOpenAIAccumulatorWithUsage(t *testing.T) {
	fixture := `data: {"id":"c1","choices":[{"delta":{"content":"Hello"}}]}

data: {"id":"c1","choices":[{"delta":{"content":" world"}}]}

data: {"id":"c1","choices":[],"usage":{"prompt_tokens":12,"completion_tokens":7}}

data: [DONE]

`
	acc := NewUsageAccumulator("openai", "gpt-4o")
	if acc == nil {
		t.Fatal("openai accumulator nil")
	}
	feedSSE(t, acc, fixture)
	um := acc.Finalize(context.Background())
	if um.Status != traffic.UsageStatusStreamingReported {
		t.Fatalf("status = %q, want streaming_reported", um.Status)
	}
	if um.PromptTokens == nil || *um.PromptTokens != 12 {
		t.Errorf("prompt = %v, want 12", um.PromptTokens)
	}
	if um.CompletionTokens == nil || *um.CompletionTokens != 7 {
		t.Errorf("completion = %v, want 7", um.CompletionTokens)
	}
}

func TestOpenAIAccumulatorWithoutUsageFallback(t *testing.T) {
	// No `usage` frame — accumulator must fall back to tokenizer estimation
	// and report streaming_estimated.
	fixture := `data: {"choices":[{"delta":{"content":"Hello"}}]}

data: {"choices":[{"delta":{"content":" world"}}]}

data: [DONE]

`
	acc := NewUsageAccumulator("openai", "gpt-4o")
	SetPromptText(acc, "prompt text for estimation")
	feedSSE(t, acc, fixture)
	um := acc.Finalize(context.Background())
	if um.Status != traffic.UsageStatusStreamingEstimated {
		t.Fatalf("status = %q, want streaming_estimated", um.Status)
	}
	if um.CompletionTokens == nil || *um.CompletionTokens < 1 {
		t.Errorf("completion estimate missing/zero: %v", um.CompletionTokens)
	}
	if um.PromptTokens == nil || *um.PromptTokens < 1 {
		t.Errorf("prompt estimate missing/zero: %v", um.PromptTokens)
	}
}

func TestAnthropicAccumulatorMessageDelta(t *testing.T) {
	// message_start sets input_tokens; message_delta cumulatively updates
	// output_tokens; the final value is what we report.
	fixture := `event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":42,"output_tokens":1}}}

event: content_block_delta
data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"Hi"}}

event: message_delta
data: {"type":"message_delta","usage":{"output_tokens":5}}

event: message_delta
data: {"type":"message_delta","usage":{"output_tokens":18}}

event: message_stop
data: {"type":"message_stop"}

`
	acc := NewUsageAccumulator("anthropic", "claude-3-5-sonnet-20241022")
	feedSSE(t, acc, fixture)
	um := acc.Finalize(context.Background())
	if um.Status != traffic.UsageStatusStreamingReported {
		t.Fatalf("status = %q, want streaming_reported", um.Status)
	}
	if um.PromptTokens == nil || *um.PromptTokens != 42 {
		t.Errorf("prompt = %v, want 42", um.PromptTokens)
	}
	if um.CompletionTokens == nil || *um.CompletionTokens != 18 {
		t.Errorf("completion = %v, want 18 (last message_delta wins)", um.CompletionTokens)
	}
}

func TestGeminiAccumulatorUsageMetadata(t *testing.T) {
	fixture := `data: {"candidates":[{"content":{"parts":[{"text":"Hello"}]}}]}

data: {"candidates":[{"content":{"parts":[{"text":" world"}]}}],"usageMetadata":{"promptTokenCount":30,"candidatesTokenCount":9}}

`
	acc := NewUsageAccumulator("gemini", "gemini-1.5-pro")
	feedSSE(t, acc, fixture)
	um := acc.Finalize(context.Background())
	if um.Status != traffic.UsageStatusStreamingReported {
		t.Fatalf("status = %q, want streaming_reported", um.Status)
	}
	if um.PromptTokens == nil || *um.PromptTokens != 30 {
		t.Errorf("prompt = %v, want 30", um.PromptTokens)
	}
	if um.CompletionTokens == nil || *um.CompletionTokens != 9 {
		t.Errorf("completion = %v, want 9", um.CompletionTokens)
	}
}

func TestBedrockAnthropicDispatch(t *testing.T) {
	// Bedrock + anthropic.* model → anthropic accumulator.
	acc := NewUsageAccumulator("bedrock", "anthropic.claude-3-5-sonnet-20241022-v2:0")
	if _, ok := acc.(*anthropicAccumulator); !ok {
		t.Fatalf("expected anthropicAccumulator for anthropic bedrock model, got %T", acc)
	}
}

func TestVertexPublisherDispatch(t *testing.T) {
	if acc := NewUsageAccumulator("vertex", "anthropic/claude-3-5-sonnet@20240620"); acc == nil {
		t.Error("vertex anthropic accumulator nil")
	} else if _, ok := acc.(*anthropicAccumulator); !ok {
		t.Errorf("vertex anthropic → %T, want *anthropicAccumulator", acc)
	}
	if acc := NewUsageAccumulator("vertex", "google/gemini-1.5-pro"); acc == nil {
		t.Error("vertex gemini accumulator nil")
	} else if _, ok := acc.(*geminiAccumulator); !ok {
		t.Errorf("vertex gemini → %T, want *geminiAccumulator", acc)
	}
	if acc := NewUsageAccumulator("vertex", "mistral/mistral-large"); acc != nil {
		t.Errorf("vertex mistral should be nil, got %T", acc)
	}
}

func TestUnknownProviderAccumulator(t *testing.T) {
	if acc := NewUsageAccumulator("unknown-provider", "some-model"); acc != nil {
		t.Errorf("unknown provider should return nil, got %T", acc)
	}
}

// slowTokenizer simulates a heavy tokenizer that blocks past the deadline.
type slowTokenizer struct{}

func (slowTokenizer) Count(ctx context.Context, text string) (int, error) {
	<-ctx.Done()
	return 0, ctx.Err()
}

func TestTokenizerDeadlineYieldsStreamingUnavailable(t *testing.T) {
	um := estimateWithTokenizer(context.Background(), slowTokenizer{}, "p", "c")
	if um.Status != traffic.UsageStatusStreamingUnavailable {
		t.Errorf("slow tokenizer should yield streaming_unavailable, got %q", um.Status)
	}
}

func TestTokenizerNilYieldsStreamingUnavailable(t *testing.T) {
	um := estimateWithTokenizer(context.Background(), nil, "p", "c")
	if um.Status != traffic.UsageStatusStreamingUnavailable {
		t.Errorf("nil tokenizer should yield streaming_unavailable, got %q", um.Status)
	}
}

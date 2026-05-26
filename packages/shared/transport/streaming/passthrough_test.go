package streaming

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

func TestPassthroughCopiesBytes(t *testing.T) {
	src := strings.Repeat("abcdef", 1000)
	var out bytes.Buffer
	if err := Passthrough(context.Background(), strings.NewReader(src), &out); err != nil {
		t.Fatalf("Passthrough: %v", err)
	}
	if out.String() != src {
		t.Fatalf("bytes not preserved (len %d vs %d)", out.Len(), len(src))
	}
}

func TestPassthroughWithAccumulatorNilDegrades(t *testing.T) {
	src := "data: hello\n\n"
	var out bytes.Buffer
	if err := PassthroughWithAccumulator(context.Background(), strings.NewReader(src), &out, nil); err != nil {
		t.Fatalf("PassthroughWithAccumulator: %v", err)
	}
	if out.String() != src {
		t.Fatalf("bytes not preserved: %q", out.String())
	}
}

func TestPassthroughWithAccumulatorOpenAIReported(t *testing.T) {
	// Canonical OpenAI chat completion stream with include_usage. The
	// accumulator must observe prompt_tokens=12, completion_tokens=7 while
	// the client receives the byte stream unchanged.
	fixture := "data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"content\":\" world\"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":7}}\n\n" +
		"data: [DONE]\n\n"

	acc := NewUsageAccumulator("openai", "gpt-4o")
	if acc == nil {
		t.Fatal("accumulator nil")
	}

	var out bytes.Buffer
	if err := PassthroughWithAccumulator(context.Background(), strings.NewReader(fixture), &out, acc); err != nil {
		t.Fatalf("PassthroughWithAccumulator: %v", err)
	}

	if out.String() != fixture {
		t.Fatalf("client bytes not preserved\n got: %q\nwant: %q", out.String(), fixture)
	}

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

func TestPassthroughWithAccumulatorAnthropicReported(t *testing.T) {
	// Minimal Anthropic messages stream — message_start gives input_tokens,
	// a trailing message_delta gives cumulative output_tokens.
	fixture := "event: message_start\n" +
		"data: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"usage\":{\"input_tokens\":25,\"output_tokens\":1}}}\n\n" +
		"event: content_block_delta\n" +
		"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"Hi\"}}\n\n" +
		"event: message_delta\n" +
		"data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":9}}\n\n" +
		"event: message_stop\n" +
		"data: {\"type\":\"message_stop\"}\n\n"

	acc := NewUsageAccumulator("anthropic", "claude-3-5-sonnet")
	if acc == nil {
		t.Fatal("accumulator nil")
	}

	var out bytes.Buffer
	if err := PassthroughWithAccumulator(context.Background(), strings.NewReader(fixture), &out, acc); err != nil {
		t.Fatalf("PassthroughWithAccumulator: %v", err)
	}
	if out.String() != fixture {
		t.Fatalf("client bytes not preserved")
	}

	um := acc.Finalize(context.Background())
	if um.Status != traffic.UsageStatusStreamingReported {
		t.Fatalf("status = %q, want streaming_reported", um.Status)
	}
	if um.PromptTokens == nil || *um.PromptTokens != 25 {
		t.Errorf("prompt = %v, want 25", um.PromptTokens)
	}
	if um.CompletionTokens == nil || *um.CompletionTokens != 9 {
		t.Errorf("completion = %v, want 9", um.CompletionTokens)
	}
}

// chunkedReader emits bytes in small fixed-size chunks to exercise the
// parser goroutine under realistic streaming-like I/O where SSE frames
// straddle Read() boundaries.
type chunkedReader struct {
	data []byte
	pos  int
	step int
}

func (r *chunkedReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	end := r.pos + r.step
	if end > len(r.data) {
		end = len(r.data)
	}
	if end-r.pos > len(p) {
		end = r.pos + len(p)
	}
	n := copy(p, r.data[r.pos:end])
	r.pos += n
	return n, nil
}

func TestPassthroughWithAccumulatorChunkedDelivery(t *testing.T) {
	// Parser goroutine must reassemble frames split across tiny reads.
	fixture := "data: {\"choices\":[{\"delta\":{\"content\":\"ab\"}}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":5}}\n\n" +
		"data: [DONE]\n\n"

	acc := NewUsageAccumulator("openai", "gpt-4o")
	var out bytes.Buffer
	r := &chunkedReader{data: []byte(fixture), step: 7}

	if err := PassthroughWithAccumulator(context.Background(), r, &out, acc); err != nil {
		t.Fatalf("PassthroughWithAccumulator: %v", err)
	}
	if out.String() != fixture {
		t.Fatalf("client bytes not preserved")
	}
	um := acc.Finalize(context.Background())
	if um.Status != traffic.UsageStatusStreamingReported {
		t.Fatalf("status = %q, want streaming_reported", um.Status)
	}
	if um.PromptTokens == nil || *um.PromptTokens != 3 {
		t.Errorf("prompt = %v, want 3", um.PromptTokens)
	}
	if um.CompletionTokens == nil || *um.CompletionTokens != 5 {
		t.Errorf("completion = %v, want 5", um.CompletionTokens)
	}
}

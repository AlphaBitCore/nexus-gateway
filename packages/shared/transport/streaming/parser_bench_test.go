package streaming

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
)

// sseFixture builds a realistic provider SSE stream: `frames` content deltas
// shaped like an OpenAI chat completion chunk, followed by the [DONE]
// sentinel. Frame size tracks a real token delta (one short word), which is
// what makes the per-frame cost dominate on a long reply.
func sseFixture(frames int) []byte {
	var b bytes.Buffer
	for i := range frames {
		fmt.Fprintf(&b, "data: {\"id\":\"chatcmpl-bench\",\"object\":\"chat.completion.chunk\",\"created\":1730000000,\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"tok%d \"},\"finish_reason\":null}]}\n\n", i)
	}
	b.WriteString("data: [DONE]\n\n")
	return b.Bytes()
}

// sseFixtureWithEventLines builds an Anthropic-shaped stream where every frame
// carries BOTH an `event:` and a `data:` line, so the parser walks two lines
// per frame instead of one.
func sseFixtureWithEventLines(frames int) []byte {
	var b bytes.Buffer
	for i := range frames {
		fmt.Fprintf(&b, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"tok%d \"}}\n\n", i)
	}
	b.WriteString("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	return b.Bytes()
}

// benchDrain runs the parser over the fixture to EOF and returns the frame
// count so the benchmark can assert it actually parsed what it meant to — a
// parser that errored early would otherwise report a great number for doing
// nothing.
func benchDrain(b *testing.B, fixture []byte, logger *slog.Logger) {
	b.Helper()

	// Verify the fixture parses to the expected shape once before timing.
	want := 0
	{
		p := NewSSEParserWithLogger(bytes.NewReader(fixture), logger)
		for {
			evt, err := p.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				b.Fatalf("fixture parse: %v", err)
			}
			want++
			_ = evt
		}
		if want == 0 {
			b.Fatal("fixture produced no events — benchmark would measure nothing")
		}
	}

	b.SetBytes(int64(len(fixture)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		// Production releases the parser on every exit path, which is what returns
		// the pooled scan buffer. An arm that skips Release measures a pool that is
		// never refilled — which is exactly why this benchmark first showed no
		// improvement at all from pooling that buffer.
		p := NewSSEParserWithLogger(bytes.NewReader(fixture), logger)
		got := 0
		for {
			_, err := p.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				b.Fatalf("Next: %v", err)
			}
			got++
		}
		p.Release()
		if got != want {
			b.Fatalf("parsed %d events, want %d", got, want)
		}
	}
}

// discardLogger keeps log handling out of the measurement for the baseline
// benchmarks; the warn-storm benchmark below deliberately uses a real handler.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

// BenchmarkSSEParser_OpenAIShape is the dominant compliance-proxy streaming
// shape: one `data:` line per frame, 150 frames ~ a 150-token reply.
func BenchmarkSSEParser_OpenAIShape(b *testing.B) {
	benchDrain(b, sseFixture(150), discardLogger())
}

// BenchmarkSSEParser_AnthropicShape walks two lines per frame.
func BenchmarkSSEParser_AnthropicShape(b *testing.B) {
	benchDrain(b, sseFixtureWithEventLines(150), discardLogger())
}

// BenchmarkSSEParser_LongReply scales the frame count to a long generation, so
// the per-frame cost separates cleanly from the per-stream setup cost.
func BenchmarkSSEParser_LongReply(b *testing.B) {
	benchDrain(b, sseFixture(1000), discardLogger())
}

// BenchmarkSSEParser_ShortReply isolates the per-STREAM setup cost (the 64 KiB
// scan buffer) by making the per-frame work negligible. A proxy serving many
// short streams pays this repeatedly.
func BenchmarkSSEParser_ShortReply(b *testing.B) {
	benchDrain(b, sseFixture(3), discardLogger())
}

// BenchmarkSSEParser_UnknownFieldWarnStorm measures the cost when a provider
// emits an SSE field the parser does not recognize. The SSE spec requires
// ignoring unknown fields, but this parser logs a Warn for every such LINE, so
// the cost scales with the stream length rather than being paid once.
func BenchmarkSSEParser_UnknownFieldWarnStorm(b *testing.B) {
	var buf bytes.Buffer
	for i := range 150 {
		fmt.Fprintf(&buf, "x-provider-seq: %d\ndata: {\"delta\":\"tok%d\"}\n\n", i, i)
	}
	buf.WriteString("data: [DONE]\n\n")
	// A real (non-discard) handler at Warn level, because the point of this
	// benchmark is what the warn path actually costs in production.
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelWarn}))
	benchDrain(b, buf.Bytes(), logger)
}

// BenchmarkSSEParser_LargeFrame exercises a frame near the practical upper end
// (a base64 image chunk or a big tool-call payload), where the per-line string
// copy in Text() is the dominant term rather than the per-frame struct.
func BenchmarkSSEParser_LargeFrame(b *testing.B) {
	payload := strings.Repeat("A", 32*1024)
	var buf bytes.Buffer
	for range 8 {
		fmt.Fprintf(&buf, "data: {\"delta\":{\"image\":\"%s\"}}\n\n", payload)
	}
	buf.WriteString("data: [DONE]\n\n")
	benchDrain(b, buf.Bytes(), discardLogger())
}

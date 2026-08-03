package streaming

import (
	"bytes"
	"context"
	"log/slog"
	"strconv"
	"strings"
	"testing"
)

// tokStr renders a *int token count for a failure message. Printing the pointer with %v
// yields an address, which tells whoever reads the failure nothing — the first version of
// this file did exactly that.
func tokStr(p *int) string {
	if p == nil {
		return "<nil>"
	}
	return strconv.Itoa(*p)
}

// Finding R-10: no end-to-end test drove LivePipeline.Process with a usage accumulator
// AND a usage trailer frame. WithUsageAccumulator was called only from the benchmarks, so
// the one path C-31's validity gate and R-8's behaviour-delta concern both touch — a
// trailer frame that carries token counts and NO delta content, flowing through the parser,
// the accumulator, extractDeltaText and the checkpoint gate together — was never exercised.
//
// It matters because a usage trailer is where provider-truth token counts come from. Every
// tier-1 accumulator reads them from a frame at the END of the stream, and the audit row's
// usage status depends on whether that frame landed. A regression there does not fail
// anything visibly: the stream still delivers, the row still writes, and the tokens quietly
// become an estimate.

const usageTrailerFrame = `data: {"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}` + "\n\n"

// TestLivePipeline_UsageTrailer_EndToEnd drives content frames, then the trailer, then
// [DONE], and asserts all three properties the trailer is responsible for.
func TestLivePipeline_UsageTrailer_EndToEnd(t *testing.T) {
	acc := NewUsageAccumulator("openai", "gpt-4o")
	lp := NewLivePipeline(LiveConfig{CheckpointChars: 5}, nil, slog.Default()).
		WithUsageAccumulator(acc)

	stream := "data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\" world\"}}]}\n\n" +
		usageTrailerFrame +
		"data: [DONE]\n\n"

	var out bytes.Buffer
	if _, err := lp.Process(context.Background(), strings.NewReader(stream), &out, nil); err != nil {
		t.Fatalf("Process: %v", err)
	}

	// 1. The trailer reached the client verbatim. A relay that swallowed it would break
	//    clients that read usage off the wire themselves.
	if !strings.Contains(out.String(), `"total_tokens":18`) {
		t.Fatalf("client output did not carry the usage trailer:\n%s", out.String())
	}

	// 2. The accumulator picked the counts up. This is the assertion that was missing:
	//    without it, a change that stopped feeding the trailer would silently downgrade
	//    every stream's audit row from provider-truth tokens to a tokenizer estimate.
	usage := acc.Finalize(context.Background())
	if usage.PromptTokens == nil || *usage.PromptTokens != 11 {
		t.Fatalf("PromptTokens = %s, want 11 — the usage trailer did not reach the accumulator", tokStr(usage.PromptTokens))
	}
	if usage.CompletionTokens == nil || *usage.CompletionTokens != 7 {
		t.Fatalf("CompletionTokens = %s, want 7", tokStr(usage.CompletionTokens))
	}

	// 3. The trailer contributed NO text to the transcript. It has no delta, so
	//    extractDeltaText must return "" for it; if it instead returned the raw frame
	//    (which is what the C-31 validity gate does for a frame it declines to decode),
	//    the JSON would land in the scanned transcript — inflating checkpoint content and
	//    showing hooks bytes that are not assistant output. Asserted on the client-visible
	//    transcript rather than on the internal counter so it stays true through refactors.
	if got := extractDeltaText(&SSEEvent{Data: strings.TrimSuffix(strings.TrimPrefix(usageTrailerFrame, "data: "), "\n\n")}); got != "" {
		t.Fatalf("extractDeltaText(usage trailer) = %q, want \"\": a trailer carries no assistant text, "+
			"and returning the raw frame would put JSON into the scanned transcript", got)
	}
}

// TestLivePipeline_UsageTrailer_BackslashBearing is the R-8 half. C-31's gate only engages
// on frames containing a backslash, so a trailer that carries one takes the narrow path
// through the validity check. A VALID such frame must still decode normally — the gate must
// not turn a well-formed frame into raw-text passthrough, which would both lose the usage
// and inject JSON into the transcript.
func TestLivePipeline_UsageTrailer_BackslashBearing(t *testing.T) {
	acc := NewUsageAccumulator("openai", "gpt-4o")
	lp := NewLivePipeline(LiveConfig{CheckpointChars: 5}, nil, slog.Default()).
		WithUsageAccumulator(acc)

	// A backslash inside a string value, valid JSON, alongside real usage counts.
	trailer := `data: {"choices":[{"delta":{"content":"path C:\\tmp"}}],` +
		`"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}` + "\n\n"
	stream := trailer + "data: [DONE]\n\n"

	var out bytes.Buffer
	if _, err := lp.Process(context.Background(), strings.NewReader(stream), &out, nil); err != nil {
		t.Fatalf("Process: %v", err)
	}

	usage := acc.Finalize(context.Background())
	// UsageMeta carries prompt/completion separately; there is no total field, and
	// asserting both halves is stricter than a total would have been anyway.
	if usage.PromptTokens == nil || *usage.PromptTokens != 3 ||
		usage.CompletionTokens == nil || *usage.CompletionTokens != 4 {
		t.Fatalf("usage = prompt %s / completion %s, want 3 / 4 — a valid backslash-bearing "+
			"frame must still be decoded; the validity gate is narrowed to backslash-bearing "+
			"frames, so this is the exact shape where a too-eager gate would drop usage silently",
			tokStr(usage.PromptTokens), tokStr(usage.CompletionTokens))
	}
	// The delta text must be the UNESCAPED content, proving the frame went through the
	// decoder rather than the gate's raw-data fallback.
	got := extractDeltaText(&SSEEvent{Data: strings.TrimSuffix(strings.TrimPrefix(trailer, "data: "), "\n\n")})
	if got != `path C:\tmp` {
		t.Fatalf("extractDeltaText = %q, want %q — a valid frame must be decoded, not passed "+
			"through as raw JSON", got, `path C:\tmp`)
	}
}

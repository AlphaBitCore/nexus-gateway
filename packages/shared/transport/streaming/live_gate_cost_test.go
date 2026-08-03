package streaming

import (
	"strings"
	"testing"
)

// The cost model for extractDeltaText's memory-safety gate, and the premise that
// model rests on.
//
// The gate is `strings.IndexByte(data, '\\') >= 0 && !stdjson.Valid(...)`. Its
// expensive half — the validity scan — runs ONLY on a frame that carries a
// backslash. On every other frame the whole gate is one byte search.
//
// This matters because the end-to-end benchmark arms cannot resolve the difference:
// the residual cost is ~0.5% of a 150-frame stream, and on this host an
// after-vs-after null control drifts ~19%. Rather than chase a statistical answer
// the machine cannot give, the cost is established deterministically — by proving
// which half of the gate executes — and the arithmetic is anchored by the
// microbenchmark below. Full derivation in
// docs/handoffs/perf-compliance-agent-program.md under R-9.

// TestGateCostModelPremise pins the load-bearing premise: none of the benchmark
// fixtures contains a backslash, so the validity scan provably never runs on them
// and the recorded cost model applies. If a fixture gains a backslash the model is
// silently invalidated — which is exactly the class of error that produced several
// retracted numbers in this program — so it fails here instead.
func TestGateCostModelPremise(t *testing.T) {
	fixtures := map[string]string{
		"sseFixture(150)":    string(sseFixture(150)),
		"sseFixture(1000)":   string(sseFixture(1000)),
		"makeAnthropicSSE":   makeAnthropicSSE(bench150Deltas()...),
		"frameOpenAIContent": frameOpenAIContent,
		"frameAnthropic":     frameAnthropic,
	}
	for name, f := range fixtures {
		if n := strings.Count(f, `\`); n > 0 {
			t.Errorf("%s contains %d backslash(es), so the validity scan DOES execute on it and the recorded cost model no longer applies — re-derive R-9 before quoting it", name, n)
		}
	}
}

// BenchmarkGatePrecondition is the cost anchor: what a frame pays when it does not
// carry a backslash, which is the whole gate for such a frame. Measured at
// ~7 ns / 0 allocs on a 170-byte frame, i.e. ~1 µs across a 150-frame stream.
//
// Run it one process per invocation, several rounds, and compare with benchstat —
// `go test -bench -count=N` does not interleave, so a single multi-count run inside
// one process is not a controlled comparison.
func BenchmarkGatePrecondition(b *testing.B) {
	d := frameOpenAIContent
	if strings.IndexByte(d, '\\') >= 0 {
		b.Fatal("fixture carries a backslash — this arm no longer measures the cheap half of the gate")
	}
	b.SetBytes(int64(len(d)))
	b.ReportAllocs()
	for range b.N {
		if strings.IndexByte(d, '\\') >= 0 {
			b.Fatal("unreachable")
		}
	}
}

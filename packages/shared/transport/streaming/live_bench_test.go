package streaming

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
)

// benchNoopPipeline is a zero-allocation PipelineExecutor. mockPipeline
// records every call, which would put test bookkeeping inside the measurement.
type benchNoopPipeline struct{ approve core.CompliancePipelineResult }

func (p *benchNoopPipeline) Execute(context.Context, *core.HookInput) *core.CompliancePipelineResult {
	return &p.approve
}

// benchNoopPreHook is installed because production (tlsbump) always installs
// one, and its presence changes the shape of the measured path: it turns on the
// raw-bytes tee (io.TeeReader into LockedByteBuffer) and makes every checkpoint
// take a full Snapshot copy. Benchmarking without it would measure a
// configuration that does not ship.
func benchNoopPreHook(_ []byte, _ *core.HookInput) {}

// benchLiveOpts selects which of the production wiring seams are installed.
// Both matter to the measured shape, and leaving either out measures a
// configuration that does not ship.
//
// preHook turns on the raw-bytes tee (io.TeeReader into LockedByteBuffer) and
// makes every checkpoint take a full Snapshot copy (C-22).
//
// accProvider attaches the usage accumulator the reader goroutine feeds on
// EVERY frame, which is where C-20's per-frame validity scan plus usage and
// choices path scans live. tlsbump installs one for all AI traffic (sse.go:
// `if acc != nil { livePipeline.WithUsageAccumulator(acc) }`), so an arm without
// it prices only half of the per-frame work.
type benchLiveOpts struct {
	preHook     bool
	accProvider string // "" = no accumulator; else a NewUsageAccumulator provider id
	accModel    string
}

// benchLiveStream drives one full stream through LivePipeline.Process — parse,
// deliver, accumulate, checkpoint — which is the end-to-end clean-path cost of
// an inspected SSE response on compliance-proxy and agent. This is the number
// C-16 (per-frame unmarshal), C-20 (the other per-frame JSON passes), C-21
// (per-frame serialization) and C-22 (snapshot copy amplification) all feed
// into, so it is the right yardstick for ranking them by end-to-end effect
// rather than per-function delta.
//
// Accumulator Finalize is deliberately NOT called: it runs once per stream and
// its cost is the tokenizer's, which would swamp the per-frame signal these
// arms exist to isolate. Feed — the per-frame half — is what the reader
// goroutine pays on every frame and is fully measured here.
func benchLiveStream(b *testing.B, fixture []byte, opts benchLiveOpts) {
	lg := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	base := &core.HookInput{Stage: "response"}

	build := func() *LivePipeline {
		lp := NewLivePipeline(LiveConfig{}, &benchNoopPipeline{
			approve: core.CompliancePipelineResult{Decision: core.Approve},
		}, lg)
		if opts.preHook {
			lp = lp.WithPreHook(benchNoopPreHook)
		}
		if opts.accProvider != "" {
			acc := NewUsageAccumulator(opts.accProvider, opts.accModel)
			if acc == nil {
				b.Fatalf("NewUsageAccumulator(%q, %q) = nil — arm would silently measure the no-accumulator path",
					opts.accProvider, opts.accModel)
			}
			lp = lp.WithUsageAccumulator(acc)
		}
		return lp
	}

	out := benchClientWriter(fixture)

	// Verify the fixture actually flows before timing it, and that the writer never
	// has to grow — a grow inside the timed loop would put harness allocation back
	// into the measurement.
	{
		if _, err := build().Process(context.Background(), bytes.NewReader(fixture), out, base); err != nil {
			b.Fatalf("fixture Process: %v", err)
		}
		if out.Len() == 0 {
			b.Fatal("fixture produced no output — benchmark would measure nothing")
		}
		if out.Len() > out.Cap() {
			b.Fatalf("relayed %d bytes into a %d-byte writer — it grew, so the measurement includes harness allocation", out.Len(), out.Cap())
		}
	}

	b.SetBytes(int64(len(fixture)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		out.Reset()
		if _, err := build().Process(context.Background(), bytes.NewReader(fixture), out, base); err != nil {
			b.Fatalf("Process: %v", err)
		}
	}
}

// benchClientWriter returns the stand-in for the client writer: one buffer, sized
// once and RESET per operation, so it contributes nothing to the measured
// allocation.
//
// This is a correctness property of the measurement, not a micro-optimization. An
// empty bytes.Buffer doubles its backing array as the relay writes into it, and a
// profile put bytes.growSlice at 37.7% of every byte the benchmark allocated.
// Production writes to an http.ResponseWriter whose buffer is already sized and
// reused, so that growth is harness noise inflating the heap-lock and madvise share
// of every CPU profile taken through it.
//
// Two things the first attempt at this got wrong, recorded so they are not repeated:
// only 78% of that growth was the client buffer — the other 22% is production, the
// raw-bytes tee into LockedByteBuffer, which no harness change can remove. And
// merely PRE-SIZING a fresh buffer per op cut total bytes 12.3%, not 37.7%, and left
// the buffer as the second-largest allocation site in the profile (56 KB/op, above
// LockedByteBuffer.Snapshot). Reusing one buffer with Reset() is what actually takes
// it to zero; the capacity only needs to cover the relayed stream, which is slightly
// larger than the source because WriteSSEEvent re-serializes each frame.
func benchClientWriter(fixture []byte) *bytes.Buffer {
	return bytes.NewBuffer(make([]byte, 0, len(fixture)+len(fixture)/4+4096))
}

// BenchmarkLivePipeline_OpenAI150 is the dominant inspected-stream shape for
// NON-AI traffic: a response pipeline is bound but no provider was detected, so
// tlsbump wires no usage accumulator.
func BenchmarkLivePipeline_OpenAI150(b *testing.B) {
	benchLiveStream(b, sseFixture(150), benchLiveOpts{preHook: true})
}

// BenchmarkLivePipeline_Anthropic150 is the binding-B9 shape: a wire the inline
// extractor does not model. Before the C-17 fix this ran ZERO checkpoints; it
// now runs the mandatory final one, so its cost is expected to be HIGHER than
// before — that is the correctness fix being paid for, not a regression.
func BenchmarkLivePipeline_Anthropic150(b *testing.B) {
	benchLiveStream(b, []byte(makeAnthropicSSE(bench150Deltas()...)), benchLiveOpts{preHook: true})
}

// BenchmarkLivePipeline_OpenAI150_NoPreHook isolates how much of the cost is
// the raw-bytes tee plus snapshot copying (C-22) versus the per-frame parsing
// (C-16 / C-20) — the delta between this and OpenAI150 is the tee's price.
func BenchmarkLivePipeline_OpenAI150_NoPreHook(b *testing.B) {
	benchLiveStream(b, sseFixture(150), benchLiveOpts{})
}

// BenchmarkLivePipeline_OpenAI1000 scales the frame count so per-frame cost
// separates from per-stream setup, and exercises the widening checkpoint
// cadence where the snapshot copy amplification (C-22) shows up.
func BenchmarkLivePipeline_OpenAI1000(b *testing.B) {
	benchLiveStream(b, sseFixture(1000), benchLiveOpts{preHook: true})
}

// BenchmarkLivePipeline_OpenAI1000_NoPreHook is the other half of the C-22
// measurement. The 150-frame pair showed the tee + snapshot costing +88.6 KiB for
// only +7 allocations — few allocations, enormous bytes, which is the signature of
// a whole-buffer copy rather than per-frame work. This arm establishes how that
// scales: each checkpoint copies the ENTIRE accumulated buffer, and the number of
// checkpoints grows with the stream, so the total is quadratic in stream length even
// with the widening cadence. Without this arm the 1000-frame number cannot be split
// into "per-frame parsing" and "snapshot amplification".
func BenchmarkLivePipeline_OpenAI1000_NoPreHook(b *testing.B) {
	benchLiveStream(b, sseFixture(1000), benchLiveOpts{})
}

// --- Usage-accumulator arms: the AI-traffic production shape ---
//
// These are the honest C-16 + C-20 yardstick. The arms above leave l.usage nil,
// so the reader goroutine's per-frame gjson.Valid + usage path scan + choices
// path scan never run and C-20's cost is simply absent from the number. Any
// before/after taken against a nil-usage arm would credit a change for work it
// never had to do.

// BenchmarkLivePipeline_OpenAI150_Usage is the dominant AI-traffic shape:
// response hooks bound, provider detected, pre-hook installed. Every frame is
// walked by BOTH the reader's openaiAccumulator.Feed and the delivery loop's
// extractDeltaText — the duplication C-16 + C-20 exist to collapse.
func BenchmarkLivePipeline_OpenAI150_Usage(b *testing.B) {
	benchLiveStream(b, sseFixture(150), benchLiveOpts{
		preHook: true, accProvider: "openai", accModel: "gpt-4o",
	})
}

// BenchmarkLivePipeline_Anthropic150_Usage is the same shape on a wire the
// inline extractor does not model: the accumulator extracts the text
// successfully via delta.text while extractDeltaText pays a full unmarshal to
// return "". The clearest statement of the duplicated work.
func BenchmarkLivePipeline_Anthropic150_Usage(b *testing.B) {
	benchLiveStream(b, []byte(makeAnthropicSSE(bench150Deltas()...)), benchLiveOpts{
		preHook: true, accProvider: "anthropic", accModel: "claude-sonnet-4-6",
	})
}

// BenchmarkLivePipeline_OpenAI1000_Usage scales the frame count so per-frame
// cost separates from per-stream setup with both passes wired.
func BenchmarkLivePipeline_OpenAI1000_Usage(b *testing.B) {
	benchLiveStream(b, sseFixture(1000), benchLiveOpts{
		preHook: true, accProvider: "openai", accModel: "gpt-4o",
	})
}

// BenchmarkLivePipeline_OpenAI150_Usage_Parallel models real proxy concurrency:
// many streams in flight at once, every P busy.
//
// It exists because the serial arms are scheduler-dominated in a way that is
// partly an artifact of the harness, not of the code. A CPU profile of the
// serial arm attributes ~44% to runtime.schedule / findRunnable — but with only
// two runnable goroutines against GOMAXPROCS=12, most of that is idle Ps
// spinning in runqsteal/osyield trying to steal work that does not exist. That
// spin cost does not exist on a loaded proxy where every P already has a stream.
// What DOES survive real concurrency is the per-frame channel handoff itself
// (one send + one receive, i.e. a park/unpark pair, per SSE frame), and this arm
// is the one that prices it honestly. Same lesson as C-15, whose Parallel arm
// was the arm that mattered.
func BenchmarkLivePipeline_OpenAI150_Usage_Parallel(b *testing.B) {
	benchLiveStreamParallel(b, sseFixture(150), benchLiveOpts{
		preHook: true, accProvider: "openai", accModel: "gpt-4o",
	})
}

// benchLiveStreamParallel is benchLiveStream driven through b.RunParallel so
// every P carries its own stream. Each goroutine builds its own pipeline and
// accumulator — they are single-use and not goroutine safe by contract.
func benchLiveStreamParallel(b *testing.B, fixture []byte, opts benchLiveOpts) {
	lg := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	base := &core.HookInput{Stage: "response"}

	build := func() *LivePipeline {
		lp := NewLivePipeline(LiveConfig{}, &benchNoopPipeline{
			approve: core.CompliancePipelineResult{Decision: core.Approve},
		}, lg)
		if opts.preHook {
			lp = lp.WithPreHook(benchNoopPreHook)
		}
		if opts.accProvider != "" {
			acc := NewUsageAccumulator(opts.accProvider, opts.accModel)
			if acc == nil {
				b.Fatalf("NewUsageAccumulator(%q, %q) = nil", opts.accProvider, opts.accModel)
			}
			lp = lp.WithUsageAccumulator(acc)
		}
		return lp
	}

	b.SetBytes(int64(len(fixture)))
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		// One writer per parallel goroutine, reset per op — bytes.Buffer is not
		// goroutine safe, so it cannot be hoisted out of RunParallel.
		out := benchClientWriter(fixture)
		for pb.Next() {
			out.Reset()
			if _, err := build().Process(context.Background(), bytes.NewReader(fixture), out, base); err != nil {
				b.Errorf("Process: %v", err)
				return
			}
			if out.Len() == 0 {
				b.Error("Process produced no output — the arm would measure nothing")
				return
			}
		}
	})
}

func bench150Deltas() []string {
	out := make([]string, 150)
	for i := range out {
		out[i] = "tok "
	}
	return out
}

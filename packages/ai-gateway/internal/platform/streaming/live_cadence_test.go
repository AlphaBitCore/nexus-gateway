package streaming

import (
	"context"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	goHooks "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
)

// countingHook records how many times the checkpoint hook ran (one call per
// runCheckpoint), and the content length seen at the last call.
func countingHook(calls *atomic.Int64, lastLen *atomic.Int64) StreamHookRunner {
	return func(_ context.Context, in *goHooks.HookInput) *goHooks.CompliancePipelineResult {
		calls.Add(1)
		if in != nil && in.Normalized != nil {
			n := 0
			for _, s := range in.Normalized.TextProjection() {
				n += len(s)
			}
			lastLen.Store(int64(n))
		}
		return &goHooks.CompliancePipelineResult{Decision: goHooks.Approve}
	}
}

// manyChunkStream builds an SSE stream of n content chunks each `each` chars.
func manyChunkStream(n, each int) string {
	var b strings.Builder
	seg := strings.Repeat("x", each)
	for range n {
		b.WriteString(`data: {"choices":[{"delta":{"content":"` + seg + `"}}]}` + "\n\n")
	}
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

// TestLiveCadence_LongStreamCheckpointsSublinear is the O(n²) fix guard: on a
// long stream the number of intermediate checkpoints must grow far slower than
// the transcript length. With the old fixed ReinspectStepChars cadence the
// count was contentLen/step; the widening cadence must cut it well below that.
func TestLiveCadence_LongStreamCheckpointsSublinear(t *testing.T) {
	const chunks, each, step = 400, 40, 128 // ~16000 content chars
	var calls, lastLen atomic.Int64
	lp := NewLivePipeline(LiveConfig{
		FirstInspectChars:  400,
		ReinspectStepChars: step,
		EmitOpenAIDone:     true,
	}, countingHook(&calls, &lastLen), nil, slog.Default())

	rec := httptest.NewRecorder()
	blocked := lp.Process(context.Background(), strings.NewReader(manyChunkStream(chunks, each)),
		rec, &StreamHookContext{IngressType: "AI_GATEWAY", Path: "/v1/chat/completions"})
	if blocked {
		t.Fatal("clean stream must not be blocked")
	}

	contentLen := chunks * each // 16000
	fixedCadenceCount := int64(contentLen / step)
	got := calls.Load()
	// The widening cadence must be well under half the fixed-cadence count on a
	// stream this long; otherwise the quadratic re-normalization is back.
	if got >= fixedCadenceCount/2 {
		t.Fatalf("checkpoint count %d not sublinear vs fixed-cadence %d (content %d chars)", got, fixedCadenceCount, contentLen)
	}
	// The final checkpoint must have scanned the FULL transcript.
	if lastLen.Load() < int64(contentLen) {
		t.Fatalf("final checkpoint scanned %d chars, want the full %d", lastLen.Load(), contentLen)
	}
}

// TestLiveCadence_ShortStreamUnchanged pins that short streams keep the
// original fine-grained cadence — the proportional term must not kick in
// below ~8× the step, so behavior for normal-length responses is unchanged.
func TestLiveCadence_ShortStreamUnchanged(t *testing.T) {
	const step = 128
	// ~900 content chars: below 8*step=1024, so cadence stays fixed at `step`.
	var calls, lastLen atomic.Int64
	lp := NewLivePipeline(LiveConfig{
		FirstInspectChars:  400,
		ReinspectStepChars: step,
		EmitOpenAIDone:     true,
	}, countingHook(&calls, &lastLen), nil, slog.Default())

	rec := httptest.NewRecorder()
	_ = lp.Process(context.Background(), strings.NewReader(manyChunkStream(30, 30)), // 900 chars
		rec, &StreamHookContext{IngressType: "AI_GATEWAY", Path: "/v1/chat/completions"})

	// First at 400, then +128 each: 400,528,656,784,(900 end) → 4 intermediate
	// + 1 mandatory EOF = 5. Assert we are in the fine-grained regime (>=4),
	// proving the widening did not coarsen a short stream.
	if got := calls.Load(); got < 4 {
		t.Fatalf("short stream got only %d checkpoints; fine-grained cadence regressed", got)
	}
}

// TestLiveCadence_DetectionStillFiresLateInStream ensures the widening cadence
// never drops the EOF scan: a match that only appears in the final chunk of a
// long stream must still be seen by a checkpoint (the mandatory EOF one).
func TestLiveCadence_DetectionStillFiresLateInStream(t *testing.T) {
	var sawMarker atomic.Bool
	hook := func(_ context.Context, in *goHooks.HookInput) *goHooks.CompliancePipelineResult {
		if in != nil && in.Normalized != nil {
			for _, s := range in.Normalized.TextProjection() {
				if strings.Contains(s, "SECRET_MARKER") {
					sawMarker.Store(true)
				}
			}
		}
		return &goHooks.CompliancePipelineResult{Decision: goHooks.Approve}
	}
	lp := NewLivePipeline(LiveConfig{
		FirstInspectChars: 400, ReinspectStepChars: 128, EmitOpenAIDone: true,
	}, hook, nil, slog.Default())

	stream := manyChunkStream(300, 40) // long benign prefix
	stream = strings.Replace(stream, "data: [DONE]",
		`data: {"choices":[{"delta":{"content":"SECRET_MARKER"}}]}`+"\n\ndata: [DONE]", 1)

	rec := httptest.NewRecorder()
	_ = lp.Process(context.Background(), strings.NewReader(stream),
		rec, &StreamHookContext{IngressType: "AI_GATEWAY", Path: "/v1/chat/completions"})

	if !sawMarker.Load() {
		t.Fatal("a marker in the final chunk must be seen by the mandatory EOF checkpoint")
	}
}

// TestLiveCadence_OverflowStillAudits pins MINOR-1: when a stream exceeds
// MaxBufferSize the mandatory EOF checkpoint is skipped, so the overflow path
// must run one checkpoint over the content accumulated so far — otherwise the
// widened cadence leaves the tail since the last intermediate checkpoint
// unaudited on the audit-only path.
func TestLiveCadence_OverflowStillAudits(t *testing.T) {
	var calls, lastLen atomic.Int64
	lp := NewLivePipeline(LiveConfig{
		FirstInspectChars:  400,
		ReinspectStepChars: 128,
		MaxBufferSize:      131072, // large enough that the step at overflow (~accLen/8) is a material tail
		EmitOpenAIDone:     true,
	}, countingHook(&calls, &lastLen), nil, slog.Default())

	rec := httptest.NewRecorder()
	// ~16000 raw bytes → crosses the 4096 buffer cap mid-stream.
	blocked := lp.Process(context.Background(), strings.NewReader(manyChunkStream(4000, 40)),
		rec, &StreamHookContext{IngressType: "AI_GATEWAY", Path: "/v1/chat/completions"})
	if !blocked {
		t.Fatal("stream exceeding MaxBufferSize must set blocked")
	}
	// Pin the fix's business behavior (not just "a checkpoint ran"): the
	// LAST checkpoint on the overflow path must have scanned ALL delivered
	// content. Without the overflow-path runCheckpoint, the last checkpoint
	// is the previous INTERMEDIATE one, which trails the delivered tail by up
	// to accLen/8 — so lastLen would be strictly less than what was delivered.
	delivered := strings.Count(rec.Body.String(), "x")
	if delivered == 0 {
		t.Fatal("precondition: some content must have been delivered before overflow")
	}
	// The overflow checkpoint must cover essentially all delivered content —
	// the unaudited tail must be far smaller than the widened step (accLen/8,
	// ~260 chars here). Without the overflow-path runCheckpoint the last scan
	// is the previous intermediate checkpoint, leaving a whole step's worth of
	// delivered tail unaudited (gap >> one base step).
	if gap := delivered - int(lastLen.Load()); gap >= 128 {
		t.Fatalf("overflow left %d delivered chars unaudited (scanned %d of %d) — the tail since the last intermediate checkpoint went unaudited", gap, lastLen.Load(), delivered)
	}
}

package streaming

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
)

// Findings R-6, R-8 (remainder) and R-11 all asked the same question: does the checkpoint-cadence
// inflation introduced by C-31's validity gate need a hard bound?
//
// The register carried them as "blocked on production data — the backslash-bearing frame fraction
// f is the independent variable and cannot be synthesized". That was the wrong frame. The effect is
// MONOTONIC in f: a frame only takes the raw-verbatim path when it is backslash-bearing AND
// invalid, and every such frame pushes more bytes into accumulatedAll than the decoded delta would.
// A monotonic effect's worst case is its terminal answer, and the worst case is f = 1 — every frame
// on the fallback path — which is trivially synthesizable. No production capture is required to
// decide whether a bound is needed; it is required only to predict where between the endpoints a
// given deployment sits, which is a different question and not what these findings asked.
//
// The answer these tests establish: the bound ALREADY EXISTS. The checkpoint step is
// max(CheckpointChars, accumulatedAll.Len()/8), so the cadence widens as the transcript grows and
// the execution count stays sublinear in the stream length even at f = 1. That is what keeps the
// per-checkpoint re-normalization from becoming quadratic on a long stream.

// f0Frame decodes normally: the delta contributes a few chars to the transcript.
func f0Frame(i int) string {
	return fmt.Sprintf("data: {\"choices\":[{\"delta\":{\"content\":\"tok%d\"}}]}\n\n", i)
}

// f1Frame is backslash-bearing AND invalid JSON (`\q` is not a legal escape), so extractDeltaText
// returns the WHOLE envelope verbatim — the worst case for cadence inflation, since accumulatedAll
// then grows by the frame length instead of by the delta length.
func f1Frame(i int) string {
	return fmt.Sprintf("data: {\"choices\":[{\"delta\":{\"content\":\"tok%d\\q\"}}]}\n\n", i)
}

func countCheckpoints(t *testing.T, frames int, mk func(int) string) int {
	t.Helper()
	var sb strings.Builder
	for i := range frames {
		sb.WriteString(mk(i))
	}
	sb.WriteString("data: [DONE]\n\n")

	mp := &mockPipeline{}
	lp := NewLivePipeline(LiveConfig{}, mp, slog.New(slog.DiscardHandler))
	var out bytes.Buffer
	// A non-nil baseInput is REQUIRED: with nil, Process runs no checkpoints at all and every
	// measurement below reads zero — the sixth harness fault in this program, caught by this
	// test's own vacuity guard rather than by inspection.
	if _, err := lp.Process(context.Background(), strings.NewReader(sb.String()), &out, &core.HookInput{
		Stage:       "response",
		IngressType: "COMPLIANCE_PROXY",
	}); err != nil {
		t.Fatalf("Process(%d frames): %v", frames, err)
	}
	return len(mp.calls)
}

// TestCheckpointCadence_StaysSublinearOnTheFallbackPath is the terminal answer to R-6 / R-8
// remainder / R-11. It measures both endpoints of f and asserts the property that makes a hard
// bound unnecessary: the execution count grows far slower than the stream length, so the total
// re-normalization work stays sub-quadratic even when EVERY frame takes the raw-verbatim path.
func TestCheckpointCadence_StaysSublinearOnTheFallbackPath(t *testing.T) {
	sizes := []int{50, 150, 500, 1500}
	f0 := make([]int, len(sizes))
	f1 := make([]int, len(sizes))
	for i, n := range sizes {
		f0[i] = countCheckpoints(t, n, f0Frame)
		f1[i] = countCheckpoints(t, n, f1Frame)
		t.Logf("frames=%-5d  f=0 executions=%-4d  f=1 executions=%-4d  inflation=%.1fx",
			n, f0[i], f1[i], float64(f1[i])/float64(max(f0[i], 1)))
	}

	// 1. The inflation is real and is what R-8 reported — recorded here rather than asserted as a
	//    fixed number, since it is a consequence of frame/delta length ratios, not a guarantee.
	if f1[1] <= f0[1] {
		t.Fatalf("at 150 frames f=1 produced %d executions and f=0 produced %d: the fallback path "+
			"is supposed to inflate the cadence, so this test is no longer measuring what it "+
			"exists to measure", f1[1], f0[1])
	}

	// 2. THE BOUND. Tripling the stream must not triple the executions — the step widens with the
	//    transcript, so the count is sublinear. Without this the per-checkpoint re-normalization of
	//    the cumulative body would be quadratic in the response length, which is the failure mode
	//    the growth term exists to prevent and the reason no additional bound is needed.
	for i := 1; i < len(sizes); i++ {
		frameRatio := float64(sizes[i]) / float64(sizes[i-1])
		execRatio := float64(f1[i]) / float64(max(f1[i-1], 1))
		if execRatio >= frameRatio {
			t.Fatalf("frames %d -> %d (%.1fx) drove executions %d -> %d (%.1fx) at f=1. "+
				"The checkpoint count must grow SLOWER than the stream length: the step is "+
				"max(CheckpointChars, accumulatedAll.Len()/8), and if that widening stops working "+
				"the cumulative-body re-normalization becomes quadratic in the response length.",
				sizes[i-1], sizes[i], frameRatio, f1[i-1], f1[i], execRatio)
		}
	}

	// 3. And the absolute worst case stays small enough that a hard cap would buy nothing: at
	//    f = 1 over 1500 frames the pipeline runs a double-digit number of times, not hundreds.
	if f1[len(f1)-1] > 60 {
		t.Fatalf("f=1 over %d frames produced %d executions. The register's conclusion that no "+
			"additional bound is needed rests on this staying small; re-open R-6 if it does not.",
			sizes[len(sizes)-1], f1[len(f1)-1])
	}
}

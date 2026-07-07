package streaming

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	core "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
)

// longSSE builds an SSE stream of n content chunks of `each` chars each.
func longSSE(n, each int) string {
	var b strings.Builder
	seg := strings.Repeat("x", each)
	for range n {
		b.WriteString(`data: {"choices":[{"delta":{"content":"` + seg + `"}}]}` + "\n\n")
	}
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

// TestLiveCadence_LongStreamSublinear is the O(n²) fix guard for the shared
// live pipeline: on a long stream the checkpoint count must grow far slower
// than transcript/CheckpointChars, and the last checkpoint must still see the
// full transcript.
func TestLiveCadence_LongStreamSublinear(t *testing.T) {
	const chunks, each, cp = 400, 40, 100 // 16000 content chars
	mp := &mockPipeline{}
	lp := NewLivePipeline(LiveConfig{CheckpointChars: cp}, mp, slog.Default())

	var out bytes.Buffer
	_, err := lp.Process(context.Background(), strings.NewReader(longSSE(chunks, each)), &out,
		&core.HookInput{Stage: "response", IngressType: "COMPLIANCE_PROXY"})
	if err != nil {
		t.Fatalf("process: %v", err)
	}

	contentLen := chunks * each
	fixed := contentLen / cp // old fixed-cadence checkpoint count
	got := len(mp.calls)
	if got >= fixed/2 {
		t.Fatalf("checkpoint count %d not sublinear vs fixed %d (%d chars)", got, fixed, contentLen)
	}
	// Last checkpoint must have scanned the whole transcript.
	last := mp.calls[len(mp.calls)-1]
	total := 0
	for _, s := range last {
		total += len(s)
	}
	if total < contentLen {
		t.Fatalf("final checkpoint scanned %d chars, want %d", total, contentLen)
	}
}

// TestLiveCadence_ShortStreamUnchanged pins that a short stream (below 8×
// CheckpointChars) keeps the fixed cadence — the widening term must not
// coarsen normal-length responses.
func TestLiveCadence_ShortStreamUnchanged(t *testing.T) {
	const cp = 100 // widening kicks in only past 8*cp=800 accumulated chars
	mp := &mockPipeline{}
	lp := NewLivePipeline(LiveConfig{CheckpointChars: cp}, mp, slog.Default())

	var out bytes.Buffer
	// ~600 content chars: 20 chunks × 30 → below 800, cadence stays at cp.
	_, err := lp.Process(context.Background(), strings.NewReader(longSSE(20, 30)), &out,
		&core.HookInput{Stage: "response", IngressType: "COMPLIANCE_PROXY"})
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	// 600 chars / 100 cadence → ~5-6 intermediate checkpoints; assert the
	// fine-grained regime survived (>=4).
	if got := len(mp.calls); got < 4 {
		t.Fatalf("short stream got only %d checkpoints; fine cadence regressed", got)
	}
}

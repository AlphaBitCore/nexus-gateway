package stream

// Internal test (package stream) so it can reach the unexported
// mapStopReasonToFinish. The stream package INLINES this mapping to avoid a
// production import cycle with anthropic/codec; this test-only import is the
// only thing that keeps the inlined copy in lockstep with the source of
// truth (codec.MapStopReason). If the codec mapping changes and the inlined
// copy is not updated (or vice versa), this fails — closing the silent-drift
// hole the "must stay in lockstep" comment otherwise relies on.

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/anthropic/codec"
)

func TestMapStopReasonToFinish_ParityWithCodec(t *testing.T) {
	// Cover every documented stop_reason plus empty + an unknown passthrough.
	for _, r := range []string{
		"end_turn", "stop_sequence", "max_tokens", "tool_use",
		"", "refusal", "pause_turn", "future_unknown_reason",
	} {
		got := mapStopReasonToFinish(r)
		want := codec.MapStopReason(r)
		if got != want {
			t.Errorf("drift: mapStopReasonToFinish(%q)=%q but codec.MapStopReason(%q)=%q", r, got, r, want)
		}
	}
}

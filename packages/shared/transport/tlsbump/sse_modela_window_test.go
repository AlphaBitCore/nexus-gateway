package tlsbump

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming/modela"
)

// The tail window is not a constant an operator picks — it is derived from the
// rule set, because the engine's guarantee is stated in terms of the longest
// contiguous pattern it has to match:
//
//	sound  ⟺  window > maxPattern + prescanBatch
//
// The gateway substrate derives it. This one did not: it passed the package
// default to both the warning and the engine, which for the shipped rule pack
// (maxPattern 7362, so the condition needs > 8386) leaves an 8192-byte window
// running OUTSIDE the guarantee it advertises — on the agent and
// compliance-proxy leg, which is the one in the host's outbound path.
//
// Two substrates, one condition, and only one of them was fixed. This gate is
// stated over the sizing function so a third substrate cannot repeat it.
func TestTLSBumpWindowSatisfiesTheEngineCondition(t *testing.T) {
	// The bound the shipped rule pack derives, as measured on the seeded pack.
	// A larger pack only makes the fixed default worse, never better.
	const shippedMaxPattern = 7362

	window := modela.TailWindowFor(shippedMaxPattern)
	if modela.StreamingCoverageGap(shippedMaxPattern, window) {
		t.Fatalf("TailWindowFor(%d) = %d does not satisfy the engine's own soundness condition",
			shippedMaxPattern, window)
	}

	// The value this substrate would run with if it kept using the constant.
	if !modela.StreamingCoverageGap(shippedMaxPattern, modela.DefaultTailWindowBytes) {
		t.Fatalf("the package default (%d) is expected to be unsound for the shipped pattern "+
			"bound (%d) — if that is no longer true this gate has lost its subject",
			modela.DefaultTailWindowBytes, shippedMaxPattern)
	}

	if got := tlsBumpTailWindow(shippedMaxPattern); got != window {
		t.Errorf("tlsBumpTailWindow(%d) = %d, want the derived %d. Running the fixed default here "+
			"means a contiguous pattern can complete after the window has already DELIVERED its "+
			"opening bytes — the disclosed leak, on the leg that sits in the host's packet path.",
			shippedMaxPattern, got, window)
	}
}

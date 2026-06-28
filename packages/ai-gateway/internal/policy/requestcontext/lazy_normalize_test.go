package requestcontext

import (
	"testing"

	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// TestLazyNormalize_ComputesOnceAndMemoizes locks the lazy seam: the compute fn
// runs exactly once on the first Normalized() call, the result is memoized, and
// NormalizedIfComputed reports it only after a pull triggered the compute.
func TestLazyNormalize_ComputesOnceAndMemoizes(t *testing.T) {
	calls := 0
	payload := &normcore.NormalizedPayload{Protocol: "openai", Kind: normcore.KindAIChat}
	rc := NewBuilder().WithLazyNormalize(func() *normcore.NormalizedPayload {
		calls++
		return payload
	}).Build()

	// Before any pull: not computed.
	if p, ok := rc.NormalizedIfComputed(); ok || p != nil {
		t.Fatalf("NormalizedIfComputed before pull = (%v,%v), want (nil,false)", p, ok)
	}

	// First pull computes.
	if got := rc.Normalized(); got != payload {
		t.Fatalf("Normalized() = %v, want %v", got, payload)
	}
	// Second pull memoized (no recompute).
	if got := rc.Normalized(); got != payload {
		t.Fatalf("second Normalized() = %v, want %v", got, payload)
	}
	if calls != 1 {
		t.Fatalf("compute fn ran %d times, want 1 (memoized)", calls)
	}

	// After pull: reported as computed.
	if p, ok := rc.NormalizedIfComputed(); !ok || p != payload {
		t.Fatalf("NormalizedIfComputed after pull = (%v,%v), want (%v,true)", p, ok, payload)
	}
}

// TestLazyNormalize_NilResultMemoized covers the compute-returns-nil branch:
// a failed normalize memoizes nil, Normalized stays nil, and NormalizedIfComputed
// reports not-available (computed but nil ⇒ ok=false, nothing to reuse).
func TestLazyNormalize_NilResultMemoized(t *testing.T) {
	calls := 0
	rc := NewBuilder().WithLazyNormalize(func() *normcore.NormalizedPayload {
		calls++
		return nil
	}).Build()

	if got := rc.Normalized(); got != nil {
		t.Fatalf("Normalized() = %v, want nil", got)
	}
	rc.Normalized() // second pull must not recompute
	if calls != 1 {
		t.Fatalf("compute fn ran %d times, want 1", calls)
	}
	if p, ok := rc.NormalizedIfComputed(); ok || p != nil {
		t.Fatalf("NormalizedIfComputed for nil canonical = (%v,%v), want (nil,false)", p, ok)
	}
}

// TestWithNormalized_PinsComputed locks the eager path: WithNormalized marks the
// context computed so Normalized returns the pinned payload without any seam, and
// NormalizedIfComputed reports it immediately (no pull needed).
func TestWithNormalized_PinsComputed(t *testing.T) {
	payload := &normcore.NormalizedPayload{Protocol: "anthropic", Kind: normcore.KindAIChat}
	rc := NewBuilder().WithNormalized(payload).Build()

	if p, ok := rc.NormalizedIfComputed(); !ok || p != payload {
		t.Fatalf("NormalizedIfComputed (eager) = (%v,%v), want (%v,true)", p, ok, payload)
	}
	if got := rc.Normalized(); got != payload {
		t.Fatalf("Normalized() (eager) = %v, want %v", got, payload)
	}
}

// TestLazyNormalize_NilReceiver covers the nil-receiver guards on both getters.
func TestLazyNormalize_NilReceiver(t *testing.T) {
	var rc *RequestContext
	if got := rc.Normalized(); got != nil {
		t.Fatalf("nil.Normalized() = %v, want nil", got)
	}
	if p, ok := rc.NormalizedIfComputed(); ok || p != nil {
		t.Fatalf("nil.NormalizedIfComputed() = (%v,%v), want (nil,false)", p, ok)
	}
}

// TestNormalized_NoSeamNoPayload covers a context with neither a pinned payload
// nor a lazy seam: Normalized computes (no fn) and returns nil, marking computed.
func TestNormalized_NoSeamNoPayload(t *testing.T) {
	rc := NewBuilder().WithRawBody([]byte(`{}`)).Build()
	if got := rc.Normalized(); got != nil {
		t.Fatalf("Normalized() with no seam = %v, want nil", got)
	}
	if _, ok := rc.NormalizedIfComputed(); ok {
		t.Fatalf("NormalizedIfComputed with no payload = ok true, want false")
	}
}

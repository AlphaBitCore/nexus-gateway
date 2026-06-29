package requestcontext

import (
	"testing"

	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// TestLazyNormalize_ComputesOnceAndMemoizes locks the lazy seam: the compute fn
// runs exactly once on the first Normalized() call and the result is memoized.
func TestLazyNormalize_ComputesOnceAndMemoizes(t *testing.T) {
	calls := 0
	payload := &normcore.NormalizedPayload{Protocol: "openai", Kind: normcore.KindAIChat}
	rc := NewBuilder().WithLazyNormalize(func() *normcore.NormalizedPayload {
		calls++
		return payload
	}).Build()

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
}

// TestLazyNormalize_NilResultMemoized covers the compute-returns-nil branch:
// a failed normalize memoizes nil and Normalized stays nil without recomputing.
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
}

// TestWithNormalized_PinsComputed locks the eager path: WithNormalized marks the
// context computed so Normalized returns the pinned payload without any seam.
func TestWithNormalized_PinsComputed(t *testing.T) {
	payload := &normcore.NormalizedPayload{Protocol: "anthropic", Kind: normcore.KindAIChat}
	rc := NewBuilder().WithNormalized(payload).Build()

	if got := rc.Normalized(); got != payload {
		t.Fatalf("Normalized() (eager) = %v, want %v", got, payload)
	}
}

// TestLazyNormalize_NilReceiver covers the nil-receiver guard on Normalized.
func TestLazyNormalize_NilReceiver(t *testing.T) {
	var rc *RequestContext
	if got := rc.Normalized(); got != nil {
		t.Fatalf("nil.Normalized() = %v, want nil", got)
	}
}

// TestNormalized_NoSeamNoPayload covers a context with neither a pinned payload
// nor a lazy seam: Normalized computes (no fn) and returns nil, marking computed.
func TestNormalized_NoSeamNoPayload(t *testing.T) {
	rc := NewBuilder().WithRawBody([]byte(`{}`)).Build()
	if got := rc.Normalized(); got != nil {
		t.Fatalf("Normalized() with no seam = %v, want nil", got)
	}
}

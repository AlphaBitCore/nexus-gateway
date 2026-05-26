package traffic

import "testing"

func TestLatencyBreakdown_GetSet(t *testing.T) {
	lb := LatencyBreakdown{}
	if _, ok := lb.Get(PhaseAuth); ok {
		t.Errorf("Get on empty must return ok=false")
	}
	lb.Set(PhaseAuth, 42)
	if v, ok := lb.Get(PhaseAuth); !ok || v != 42 {
		t.Errorf("Get after Set: got (%d, %v), want (42, true)", v, ok)
	}
}

func TestLatencyBreakdown_SetZeroDeletes(t *testing.T) {
	lb := LatencyBreakdown{"auth_ms": 42}
	lb.Set(PhaseAuth, 0)
	if _, ok := lb.Get(PhaseAuth); ok {
		t.Errorf("Set 0 must delete the key")
	}
}

func TestLatencyBreakdown_MarkStreamAborted(t *testing.T) {
	lb := LatencyBreakdown{}
	lb.MarkStreamAborted()
	if v := lb["stream_aborted"]; v != 1 {
		t.Errorf("stream_aborted: got %d, want 1", v)
	}
}

func TestLatencyBreakdown_NilSafe(t *testing.T) {
	var lb LatencyBreakdown
	if _, ok := lb.Get(PhaseAuth); ok {
		t.Errorf("nil Get must return ok=false")
	}
	lb.Set(PhaseAuth, 5) // no panic, no-op
	lb.MarkStreamAborted()
}

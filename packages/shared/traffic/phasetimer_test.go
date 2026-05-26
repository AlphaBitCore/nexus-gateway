package traffic

import (
	"testing"
	"time"
)

func TestPhaseTimer_MarkRecordsElapsed(t *testing.T) {
	pt := NewPhaseTimer()
	time.Sleep(5 * time.Millisecond)
	got := pt.Mark(PhaseAuth)
	if got < 4*time.Millisecond {
		t.Fatalf("auth mark too small: %v", got)
	}
	time.Sleep(10 * time.Millisecond)
	got2 := pt.Mark(PhaseQuota)
	if got2 < 9*time.Millisecond {
		t.Fatalf("quota mark too small: %v", got2)
	}
	snap := pt.Snapshot()
	if snap["auth_ms"] < 4 {
		t.Errorf("snapshot auth_ms too small: %d", snap["auth_ms"])
	}
	if snap["quota_ms"] < 9 {
		t.Errorf("snapshot quota_ms too small: %d", snap["quota_ms"])
	}
	// auth+quota together should be roughly the elapsed total.
	if snap["auth_ms"]+snap["quota_ms"] > int(pt.Elapsed()/time.Millisecond)+5 {
		t.Errorf("phase sum exceeds elapsed: %d > %d", snap["auth_ms"]+snap["quota_ms"], pt.Elapsed()/time.Millisecond)
	}
}

func TestPhaseTimer_MarkBetweenDoesNotAdvanceCursor(t *testing.T) {
	pt := NewPhaseTimer()
	time.Sleep(5 * time.Millisecond)
	pt.MarkBetween(PhaseRouting, 42*time.Millisecond)
	time.Sleep(5 * time.Millisecond)
	got := pt.Mark(PhaseAuth)
	if got < 9*time.Millisecond {
		t.Fatalf("auth mark should span the full 10ms (MarkBetween must not advance cursor), got %v", got)
	}
	snap := pt.Snapshot()
	if snap["routing_ms"] != 42 {
		t.Errorf("routing_ms: got %d, want 42", snap["routing_ms"])
	}
}

func TestPhaseTimer_SnapshotOmitsZero(t *testing.T) {
	pt := NewPhaseTimer()
	pt.SetMs(PhaseAuth, 50)
	pt.SetMs(PhaseQuota, 0)
	snap := pt.Snapshot()
	if _, ok := snap["quota_ms"]; ok {
		t.Errorf("zero-valued phase must be omitted: %v", snap)
	}
	if snap["auth_ms"] != 50 {
		t.Errorf("auth_ms: got %d, want 50", snap["auth_ms"])
	}
}

func TestPhaseTimer_EmptyName(t *testing.T) {
	pt := NewPhaseTimer()
	if d := pt.Mark(""); d != 0 {
		t.Errorf("Mark with empty name should be a no-op, returned %v", d)
	}
	pt.MarkBetween("", 10*time.Millisecond)
	pt.SetMs("", 99)
	if s := pt.Snapshot(); s != nil {
		t.Errorf("empty-name calls must not produce phases, got %v", s)
	}
}

func TestPhaseTimer_NilSafe(t *testing.T) {
	var pt *PhaseTimer
	pt.Mark(PhaseAuth)
	pt.MarkBetween(PhaseRouting, 5*time.Millisecond)
	pt.SetMs(PhaseQuota, 3)
	if s := pt.Snapshot(); s != nil {
		t.Errorf("nil timer Snapshot must return nil")
	}
	if e := pt.Elapsed(); e != 0 {
		t.Errorf("nil timer Elapsed must return 0")
	}
}

func TestPhaseTimer_NegativeClamp(t *testing.T) {
	pt := NewPhaseTimer()
	pt.MarkBetween(PhaseAuth, -1*time.Second)
	pt.SetMs(PhaseQuota, -42)
	if s := pt.Snapshot(); s != nil && (s["auth_ms"] != 0 && s["quota_ms"] != 0) {
		// Acceptable forms: empty map or both keys missing.
		t.Errorf("negative durations must clamp to zero (and thus be omitted): %v", s)
	}
}

func TestPhaseTimer_SnapshotDetail_FloorsSubMs(t *testing.T) {
	pt := NewPhaseTimer()
	// 100 µs — under 1 ms, but non-zero. Default Snapshot drops it; detail
	// mode floors it to 1.
	pt.MarkBetween(PhaseAuth, 100*time.Microsecond)
	pt.MarkBetween(PhaseQuota, 2*time.Millisecond)

	plain := pt.Snapshot()
	if _, ok := plain["auth_ms"]; ok {
		t.Errorf("plain Snapshot must drop sub-ms phases: %v", plain)
	}
	if plain["quota_ms"] != 2 {
		t.Errorf("plain quota_ms = %d, want 2", plain["quota_ms"])
	}

	detail := pt.SnapshotDetail(true)
	if detail["auth_ms"] != 1 {
		t.Errorf("detail auth_ms (sub-ms) must floor to 1, got %d", detail["auth_ms"])
	}
	if detail["quota_ms"] != 2 {
		t.Errorf("detail quota_ms = %d, want 2", detail["quota_ms"])
	}
}

func TestPhaseTimer_SnapshotDetail_ZeroStillDropped(t *testing.T) {
	pt := NewPhaseTimer()
	pt.MarkBetween(PhaseAuth, 0) // exactly zero — drop even in detail mode
	pt.MarkBetween(PhaseQuota, 50*time.Microsecond)

	detail := pt.SnapshotDetail(true)
	if _, ok := detail["auth_ms"]; ok {
		t.Errorf("exact-zero phase must drop even in detail mode: %v", detail)
	}
	if detail["quota_ms"] != 1 {
		t.Errorf("detail quota_ms (50µs) must floor to 1, got %d", detail["quota_ms"])
	}
}

package store

import (
	"testing"
	"time"
)

func TestGetLatencyP95_UnknownProvider(t *testing.T) {
	ht := newTestTracker(t)
	p95, total := ht.GetLatencyP95("nope")
	if p95 != -1 || total != 0 {
		t.Fatalf("unknown provider: got (%d,%d), want (-1,0)", p95, total)
	}
}

func TestGetLatencyP95_NearestRank(t *testing.T) {
	ht := newTestTracker(t)
	// 20 successful samples with latencies 1..20ms. Nearest-rank p95 =
	// sorted[ceil(0.95*20)-1] = sorted[18] = 19ms.
	for i := 1; i <= 20; i++ {
		ht.RecordSuccess("p", "prov", i)
	}
	ht.flush()
	p95, total := ht.GetLatencyP95("p")
	if p95 != 19 {
		t.Fatalf("p95: got %d, want 19", p95)
	}
	if total != 20 {
		t.Fatalf("total: got %d, want 20", total)
	}
}

func TestGetLatencyP95_SingleSample(t *testing.T) {
	ht := newTestTracker(t)
	ht.RecordSuccess("p", "prov", 42)
	ht.flush()
	p95, total := ht.GetLatencyP95("p")
	if p95 != 42 || total != 1 {
		t.Fatalf("single sample: got (%d,%d), want (42,1)", p95, total)
	}
}

func TestGetLatencyP95_SuccessOnly(t *testing.T) {
	ht := newTestTracker(t)
	// 10 fast successes + 5 slow failures. p95 must ignore the failures (else it
	// would be dominated by the 5000ms timeouts); total counts every attempt.
	for range 10 {
		ht.RecordSuccess("p", "prov", 100)
	}
	for range 5 {
		ht.RecordFailure("p", "prov", 5000)
	}
	ht.flush()
	p95, total := ht.GetLatencyP95("p")
	if p95 != 100 {
		t.Fatalf("p95 should be success-only 100, got %d", p95)
	}
	if total != 15 {
		t.Fatalf("total should count success+failure = 15, got %d", total)
	}
}

func TestGetLatencyP95_AllFailure(t *testing.T) {
	ht := newTestTracker(t)
	for range 8 {
		ht.RecordFailure("p", "prov", 900)
	}
	ht.flush()
	p95, total := ht.GetLatencyP95("p")
	if p95 != -1 {
		t.Fatalf("all-failure p95 should be -1 sentinel, got %d", p95)
	}
	if total != 8 {
		t.Fatalf("all-failure total should be 8, got %d", total)
	}
}

func TestGetLatencyP95_WindowCutoffPrune(t *testing.T) {
	ht := newTestTracker(t)
	// One aged-out success (outside the 5-min window) + two in-window successes.
	ht.recordAt("p", "prov", true, 9999, time.Now().Add(-2*healthWindowDuration))
	ht.RecordSuccess("p", "prov", 30)
	ht.RecordSuccess("p", "prov", 40)
	ht.flush()
	p95, total := ht.GetLatencyP95("p")
	if total != 2 {
		t.Fatalf("aged-out sample should be pruned: total got %d, want 2", total)
	}
	if p95 != 40 {
		t.Fatalf("p95 over in-window {30,40} should be 40, got %d", p95)
	}
}

func TestGetLatencyP95_AllAgedOut(t *testing.T) {
	ht := newTestTracker(t)
	ht.recordAt("p", "prov", true, 100, time.Now().Add(-2*healthWindowDuration))
	ht.flush()
	p95, total := ht.GetLatencyP95("p")
	if p95 != -1 || total != 0 {
		t.Fatalf("all-aged-out: got (%d,%d), want (-1,0)", p95, total)
	}
}

package store

import (
	"sync"
	"testing"
	"time"
)

// newTestTracker builds a tracker and registers Stop for cleanup so the writer
// goroutine never leaks across tests.
func newTestTracker(t *testing.T) *HealthTracker {
	t.Helper()
	ht := NewHealthTracker()
	t.Cleanup(ht.Stop)
	return ht
}

func TestHealthTracker_HealthyByDefault(t *testing.T) {
	ht := newTestTracker(t)
	state := ht.GetHealth("unknown-provider")
	if state.Status != HealthStatusHealthy {
		t.Errorf("expected healthy, got %s", state.Status)
	}
}

func TestHealthTracker_RecordSuccess(t *testing.T) {
	ht := newTestTracker(t)
	ht.RecordSuccess("p1", "provider1", 100)
	ht.RecordSuccess("p1", "provider1", 200)
	ht.flush()

	state := ht.GetHealth("p1")
	if state.Status != HealthStatusHealthy {
		t.Errorf("expected healthy, got %s", state.Status)
	}
	if state.SampleCount != 2 {
		t.Errorf("expected 2 samples, got %d", state.SampleCount)
	}
	if state.AvgLatencyMs != 150 {
		t.Errorf("expected avg 150ms, got %d", state.AvgLatencyMs)
	}
	if state.ErrorRate != 0 {
		t.Errorf("expected 0 error rate, got %f", state.ErrorRate)
	}
}

func TestHealthTracker_Degraded(t *testing.T) {
	ht := newTestTracker(t)
	// 10% error rate → degraded (threshold is 5%).
	for range 9 {
		ht.RecordSuccess("p1", "provider1", 50)
	}
	ht.RecordFailure("p1", "provider1", 500)
	ht.flush()

	state := ht.GetHealth("p1")
	if state.Status != HealthStatusDegraded {
		t.Errorf("expected degraded, got %s (errorRate=%f)", state.Status, state.ErrorRate)
	}
}

func TestHealthTracker_Unavailable(t *testing.T) {
	ht := newTestTracker(t)
	// 50% error rate → unavailable (threshold is 25%).
	for range 5 {
		ht.RecordSuccess("p1", "provider1", 50)
	}
	for range 5 {
		ht.RecordFailure("p1", "provider1", 500)
	}
	ht.flush()

	state := ht.GetHealth("p1")
	if state.Status != HealthStatusUnavailable {
		t.Errorf("expected unavailable, got %s (errorRate=%f)", state.Status, state.ErrorRate)
	}
}

// TestHealthTracker_ThresholdBoundaries pins the exact strict-`>` threshold
// semantics: 5% is NOT degraded (must exceed), just over 5% is; 25% is NOT
// unavailable, just over is. This is the byte-identity crux vs the prior
// implementation.
func TestHealthTracker_ThresholdBoundaries(t *testing.T) {
	// Exactly 5% (1 failure / 20) → healthy (5% is not > 5%).
	ht := newTestTracker(t)
	for range 19 {
		ht.RecordSuccess("p", "prov", 10)
	}
	ht.RecordFailure("p", "prov", 10)
	ht.flush()
	if s := ht.GetHealth("p"); s.Status != HealthStatusHealthy {
		t.Errorf("errorRate 0.05 must stay healthy (strict >), got %s", s.Status)
	}

	// Exactly 25% (5 failure / 20) → degraded (25% is not > 25%, but is > 5%).
	ht2 := newTestTracker(t)
	for range 15 {
		ht2.RecordSuccess("p", "prov", 10)
	}
	for range 5 {
		ht2.RecordFailure("p", "prov", 10)
	}
	ht2.flush()
	if s := ht2.GetHealth("p"); s.Status != HealthStatusDegraded {
		t.Errorf("errorRate 0.25 must be degraded not unavailable (strict >), got %s", s.Status)
	}
}

// TestHealthTracker_SampleCap verifies the window keeps only the most recent
// maxHealthSamples, exercised through the public GetHealth (no internals).
func TestHealthTracker_SampleCap(t *testing.T) {
	ht := newTestTracker(t)
	for range maxHealthSamples + 50 {
		ht.RecordSuccess("p1", "provider1", 10)
	}
	ht.flush()

	if c := ht.GetHealth("p1").SampleCount; c != maxHealthSamples {
		t.Errorf("samples should be capped at %d, got %d", maxHealthSamples, c)
	}
}

// TestHealthTracker_IdleRecoveryReadTimePrune injects one aged sample; the
// read-time 5-min cutoff must drop it so the provider reads healthy with zero
// in-window samples WITHOUT a new sample arriving (idle recovery). This is the
// F-R2-1 correctness trap.
func TestHealthTracker_IdleRecoveryReadTimePrune(t *testing.T) {
	ht := newTestTracker(t)
	// A single failure that already fell outside the window.
	ht.recordAt("p1", "provider1", false, 100, time.Now().Add(-2*healthWindowDuration))
	ht.flush()

	state := ht.GetHealth("p1")
	if state.Status != HealthStatusHealthy || state.SampleCount != 0 {
		t.Errorf("aged-out samples must prune to healthy zero at read time; got %+v", state)
	}
}

// TestHealthTracker_MixedInWindowAndExpired confirms only in-window samples
// count: an expired failure is ignored while a recent success keeps the
// provider healthy.
func TestHealthTracker_MixedInWindowAndExpired(t *testing.T) {
	ht := newTestTracker(t)
	ht.recordAt("p1", "provider1", false, 100, time.Now().Add(-2*healthWindowDuration)) // expired failure
	ht.RecordSuccess("p1", "provider1", 20)                                             // recent success
	ht.flush()

	state := ht.GetHealth("p1")
	if state.SampleCount != 1 {
		t.Fatalf("only the in-window sample should count, got %d", state.SampleCount)
	}
	if state.ErrorRate != 0 || state.Status != HealthStatusHealthy {
		t.Errorf("expired failure must not affect error rate; got %+v", state)
	}
}

func TestHealthTracker_IndependentProviders(t *testing.T) {
	ht := newTestTracker(t)
	ht.RecordSuccess("p1", "provider1", 100)
	ht.RecordFailure("p2", "provider2", 500)
	ht.flush()

	s1 := ht.GetHealth("p1")
	s2 := ht.GetHealth("p2")
	if s1.ErrorRate != 0 {
		t.Error("p1 should have 0 error rate")
	}
	if s2.ErrorRate == 0 {
		t.Error("p2 should have non-zero error rate")
	}
}

// TestHealthTracker_TickPublish exercises the periodic (ticker-driven) snapshot
// publish path — the production default cadence — by recording WITHOUT calling
// flush and polling until the tick makes the sample visible.
func TestHealthTracker_TickPublish(t *testing.T) {
	ht := newTestTracker(t)
	ht.RecordFailure("p1", "provider1", 100)

	deadline := time.Now().Add(2 * time.Second)
	var state HealthState
	for {
		state = ht.GetHealth("p1")
		if state.SampleCount > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if state.SampleCount != 1 {
		t.Fatalf("tick publish should make the sample visible, got %+v", state)
	}
	if state.Status != HealthStatusUnavailable {
		t.Errorf("single failure should read unavailable, got %s", state.Status)
	}
}

// TestHealthTracker_StopIdempotent verifies Stop can be called multiple times
// (and concurrently) without panicking.
func TestHealthTracker_StopIdempotent(t *testing.T) {
	ht := NewHealthTracker()
	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() { defer wg.Done(); ht.Stop() }()
	}
	wg.Wait()
	// A flush after Stop must not block or panic.
	ht.flush()
}

// TestHealthTracker_DropOnFull verifies the hot path drops (never blocks) when
// the channel is saturated: after Stop the writer is gone, so the buffer fills
// and further records are dropped and counted.
func TestHealthTracker_DropOnFull(t *testing.T) {
	ht := NewHealthTracker()
	ht.Stop() // writer gone; nothing drains ht.ch
	for range healthSampleChanCap + 100 {
		ht.RecordSuccess("p1", "provider1", 10)
	}
	if ht.droppedSamples() == 0 {
		t.Error("expected drops once the channel saturated with no writer draining")
	}
}

// TestHealthTracker_ConcurrentRecordAndRead hammers record + GetHealth + Stop
// concurrently; run under -race it proves the lock-free path is race-clean.
func TestHealthTracker_ConcurrentRecordAndRead(t *testing.T) {
	ht := NewHealthTracker()
	defer ht.Stop()

	var wg sync.WaitGroup
	for w := range 8 {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for range 500 {
				ht.RecordSuccess("p1", "provider1", id)
				ht.RecordFailure("p2", "provider2", id)
			}
		}(w)
	}
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 500 {
				_ = ht.GetHealth("p1")
				_ = ht.GetHealth("p2")
			}
		}()
	}
	wg.Wait()

	ht.flush()
	// After all records, p1 is all-success (healthy) and p2 all-failure.
	if s := ht.GetHealth("p1"); s.Status != HealthStatusHealthy {
		t.Errorf("p1 all-success should be healthy, got %s", s.Status)
	}
	if s := ht.GetHealth("p2"); s.Status != HealthStatusUnavailable {
		t.Errorf("p2 all-failure should be unavailable, got %s", s.Status)
	}
}

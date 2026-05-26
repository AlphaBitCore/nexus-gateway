package store

import (
	"testing"
	"time"
)

func TestHealthTracker_HealthyByDefault(t *testing.T) {
	ht := NewHealthTracker()
	state := ht.GetHealth("unknown-provider")
	if state.Status != HealthStatusHealthy {
		t.Errorf("expected healthy, got %s", state.Status)
	}
}

func TestHealthTracker_RecordSuccess(t *testing.T) {
	ht := NewHealthTracker()
	ht.RecordSuccess("p1", "provider1", 100)
	ht.RecordSuccess("p1", "provider1", 200)

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
	ht := NewHealthTracker()
	// 10% error rate → degraded (threshold is 5%).
	for range 9 {
		ht.RecordSuccess("p1", "provider1", 50)
	}
	ht.RecordFailure("p1", "provider1", 500)

	state := ht.GetHealth("p1")
	if state.Status != HealthStatusDegraded {
		t.Errorf("expected degraded, got %s (errorRate=%f)", state.Status, state.ErrorRate)
	}
}

func TestHealthTracker_Unavailable(t *testing.T) {
	ht := NewHealthTracker()
	// 50% error rate → unavailable (threshold is 25%).
	for range 5 {
		ht.RecordSuccess("p1", "provider1", 50)
	}
	for range 5 {
		ht.RecordFailure("p1", "provider1", 500)
	}

	state := ht.GetHealth("p1")
	if state.Status != HealthStatusUnavailable {
		t.Errorf("expected unavailable, got %s (errorRate=%f)", state.Status, state.ErrorRate)
	}
}

func TestHealthTracker_SampleCap(t *testing.T) {
	ht := NewHealthTracker()
	for range maxHealthSamples + 50 {
		ht.RecordSuccess("p1", "provider1", 10)
	}

	ht.mu.Lock()
	count := len(ht.windows["p1"].samples)
	ht.mu.Unlock()

	if count > maxHealthSamples {
		t.Errorf("samples should be capped at %d, got %d", maxHealthSamples, count)
	}
}

// TestHealthTracker_PruneAllSamplesExpired forces every sample to fall
// outside the health window so prune() drops them all, returning to a
// healthy zero-sample state. Covers the `i > 0` branch in prune.
func TestHealthTracker_PruneAllSamplesExpired(t *testing.T) {
	ht := NewHealthTracker()
	// Record one sample, then back-date it past the window.
	ht.RecordSuccess("p1", "provider1", 100)
	ht.mu.Lock()
	for i := range ht.windows["p1"].samples {
		ht.windows["p1"].samples[i].timestamp = time.Now().Add(-2 * healthWindowDuration)
	}
	ht.mu.Unlock()

	state := ht.GetHealth("p1")
	if state.Status != HealthStatusHealthy || state.SampleCount != 0 {
		t.Errorf("expired samples should prune to healthy zero; got %+v", state)
	}
}

func TestHealthTracker_IndependentProviders(t *testing.T) {
	ht := NewHealthTracker()
	ht.RecordSuccess("p1", "provider1", 100)
	ht.RecordFailure("p2", "provider2", 500)

	s1 := ht.GetHealth("p1")
	s2 := ht.GetHealth("p2")
	if s1.ErrorRate != 0 {
		t.Error("p1 should have 0 error rate")
	}
	if s2.ErrorRate == 0 {
		t.Error("p2 should have non-zero error rate")
	}
}

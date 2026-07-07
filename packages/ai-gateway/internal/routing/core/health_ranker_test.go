package core

import (
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
)

// awaitStatus polls until a provider reaches the expected health status. The
// HealthTracker applies records asynchronously (single writer), so a read
// immediately after recording must wait for the samples to be published.
func awaitStatus(t *testing.T, tracker *store.HealthTracker, providerID string, want store.HealthStatus) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if tracker.GetHealth(providerID).Status == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("provider %s did not reach %s in time; got %+v", providerID, want, tracker.GetHealth(providerID))
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestHealthRanker_Reorder(t *testing.T) {
	tracker := store.NewHealthTracker()
	t.Cleanup(tracker.Stop)
	// Record failures for provider B to make it unhealthy
	for range 20 {
		tracker.RecordFailure("provB", "provider-b", 100)
	}
	// Provider A is healthy (no records = healthy)
	awaitStatus(t, tracker, "provB", store.HealthStatusUnavailable)

	ranker := NewHealthRanker(tracker)
	targets := []RoutingTarget{
		{ProviderID: "provB", ProviderName: "provider-b"},
		{ProviderID: "provA", ProviderName: "provider-a"},
	}

	reordered := ranker.Reorder(targets)

	// provA (healthy) should come first
	if reordered[0].ProviderID != "provA" {
		t.Errorf("expected provA first, got %s", reordered[0].ProviderID)
	}
}

func TestHealthRanker_NilTracker(t *testing.T) {
	ranker := NewHealthRanker(nil)
	targets := []RoutingTarget{{ProviderID: "a"}, {ProviderID: "b"}}
	result := ranker.Reorder(targets)
	if len(result) != 2 || result[0].ProviderID != "a" {
		t.Error("nil tracker should return targets unchanged")
	}
}

func TestHealthRanker_SingleTarget(t *testing.T) {
	tracker := store.NewHealthTracker()
	t.Cleanup(tracker.Stop)
	ranker := NewHealthRanker(tracker)
	targets := []RoutingTarget{{ProviderID: "a"}}
	result := ranker.Reorder(targets)
	if len(result) != 1 {
		t.Error("single target should pass through")
	}
}

// TestHealthRanker_DegradedProvider verifies that a degraded provider (error rate
// > 5% but ≤ 25%) is ranked after healthy but before unavailable.
func TestHealthRanker_DegradedProvider(t *testing.T) {
	tracker := store.NewHealthTracker()
	t.Cleanup(tracker.Stop)
	// 10 failures + 90 successes = 10% error rate → degraded (>5%, ≤25%)
	for range 10 {
		tracker.RecordFailure("provDegraded", "provider-degraded", 100)
	}
	for range 90 {
		tracker.RecordSuccess("provDegraded", "provider-degraded", 50)
	}
	// provHealthy: no records → healthy
	// provUnavailable: all failures → unavailable
	for range 20 {
		tracker.RecordFailure("provUnavailable", "provider-unavailable", 100)
	}
	awaitStatus(t, tracker, "provDegraded", store.HealthStatusDegraded)
	awaitStatus(t, tracker, "provUnavailable", store.HealthStatusUnavailable)

	ranker := NewHealthRanker(tracker)
	targets := []RoutingTarget{
		{ProviderID: "provUnavailable", ProviderName: "provider-unavailable"},
		{ProviderID: "provDegraded", ProviderName: "provider-degraded"},
		{ProviderID: "provHealthy", ProviderName: "provider-healthy"},
	}

	result := ranker.Reorder(targets)

	// Expected order: healthy → degraded → unavailable
	if result[0].ProviderID != "provHealthy" {
		t.Errorf("expected provHealthy first, got %s", result[0].ProviderID)
	}
	if result[1].ProviderID != "provDegraded" {
		t.Errorf("expected provDegraded second, got %s", result[1].ProviderID)
	}
	if result[2].ProviderID != "provUnavailable" {
		t.Errorf("expected provUnavailable third, got %s", result[2].ProviderID)
	}
}

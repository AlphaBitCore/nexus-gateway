package core

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
)

func TestHealthRanker_Reorder(t *testing.T) {
	tracker := store.NewHealthTracker()
	// Record failures for provider B to make it unhealthy
	for range 20 {
		tracker.RecordFailure("provB", "provider-b", 100)
	}
	// Provider A is healthy (no records = healthy)

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
	ranker := NewHealthRanker(store.NewHealthTracker())
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

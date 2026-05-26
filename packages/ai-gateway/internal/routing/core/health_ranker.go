package core

import (
	"sort"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
)

// HealthRanker reorders targets so healthy providers are tried first.
// Unhealthy targets are moved to the end, not removed (they may have recovered).
type HealthRanker struct {
	tracker *store.HealthTracker
}

// NewHealthRanker creates a HealthRanker backed by the given tracker.
func NewHealthRanker(tracker *store.HealthTracker) *HealthRanker {
	return &HealthRanker{tracker: tracker}
}

// Reorder sorts targets: healthy first, degraded next, unavailable last.
// Within each health group, original relative order is preserved (stable sort).
func (h *HealthRanker) Reorder(targets []RoutingTarget) []RoutingTarget {
	if h.tracker == nil || len(targets) <= 1 {
		return targets
	}

	type ranked struct {
		target RoutingTarget
		rank   int // 0=healthy, 1=degraded, 2=unavailable
		index  int // original position
	}

	items := make([]ranked, len(targets))
	for i, t := range targets {
		state := h.tracker.GetHealth(t.ProviderID)
		r := 0
		switch state.Status {
		case store.HealthStatusDegraded:
			r = 1
		case store.HealthStatusUnavailable:
			r = 2
		}
		items[i] = ranked{target: t, rank: r, index: i}
	}

	sort.SliceStable(items, func(i, j int) bool {
		if items[i].rank != items[j].rank {
			return items[i].rank < items[j].rank
		}
		return items[i].index < items[j].index
	})

	result := make([]RoutingTarget, len(items))
	for i, item := range items {
		result[i] = item.target
	}
	return result
}

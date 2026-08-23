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

// Reorder arranges the TAIL by health — healthy first, degraded next,
// unavailable last — preserving relative order within each band.
//
// Position zero is left where the strategy put it. That is the strategy's
// answer, reached with information health does not have: an LLM router's read
// of the request, a conditional's branch, a weighted draw an admin configured.
// Health knows which providers have been failing lately. Both are worth acting
// on, and they answer different questions; letting the second overrule the
// first serves the request from whichever model happened to be up rather than
// from the one chosen to serve it.
//
// Measured, before the rule existed — a model:auto request carrying a markdown
// document traced "selected cohere/command-r7b-12-2024" and came back "nexus:
// Moonshot chat has no inline document part", because reordering had promoted a
// target picked for its context window over the one picked for the request. A
// sort key patched that specific pairing and went away with the flag it keyed
// on; the general rule replaces it.
//
// One exception, and the line falls where the question changes. A DEGRADED
// provider is still answering, so preferring a healthier target over the
// strategy's pick would be a preference overruling a preference — that is the
// case this rule exists to refuse. An UNAVAILABLE one is not answering at all,
// so dispatching to it spends an attempt and a timeout to learn what is already
// known; that is not a preference, it is a fact about whether the call can
// happen.
//
// Which is why the cleaner answer is to DROP an unavailable target rather than
// sort it, and the pipeline is where that belongs. Until it does, demoting the
// head when it is unavailable keeps the fact from being ignored without
// granting health a vote on the ordinary case.
func (h *HealthRanker) Reorder(targets []RoutingTarget) []RoutingTarget {
	if h.tracker == nil || len(targets) <= 1 {
		return targets
	}
	if h.tracker.GetHealth(targets[0].ProviderID).Status == store.HealthStatusUnavailable {
		return h.reorderTail(targets)
	}
	if len(targets) <= 2 {
		return targets
	}
	head, tail := targets[0], h.reorderTail(targets[1:])
	out := make([]RoutingTarget, 0, len(targets))
	return append(append(out, head), tail...)
}

func (h *HealthRanker) reorderTail(targets []RoutingTarget) []RoutingTarget {
	if len(targets) <= 1 {
		return targets
	}

	type ranked struct {
		target  RoutingTarget
		upgrade bool // armed for context overflow; must stay behind real targets
		rank    int  // 0=healthy, 1=degraded, 2=unavailable
		index   int  // original position
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
		items[i] = ranked{target: t, upgrade: t.ContextUpgradeOnly, rank: r, index: i}
	}

	sort.SliceStable(items, func(i, j int) bool {
		if items[i].upgrade != items[j].upgrade {
			return !items[i].upgrade
		}
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

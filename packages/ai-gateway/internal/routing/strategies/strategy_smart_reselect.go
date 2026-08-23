// strategy_smart_reselect.go — the pool members that ride along behind the
// router's pick, so a failure has somewhere to go inside the same rule.
package strategies

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
)

// smartReselectDepth is how many candidates accompany the router's choice.
//
// The pool the router chose from is already filtered for this request — the
// key's allowlist, the required capabilities, a window that holds the prompt —
// so its other members are the right thing to try when the choice fails.
// Discarding them left a `model:auto` request with one target: a single 5xx and
// the walk had to leave the rule entirely, for a rule whose whole job was to
// pick among many.
//
// Fixed rather than configurable. The number an admin would set is not the
// interesting one — the interesting one is the SPEND, and that is already what
// `maxUpstreamCalls` says. It also cannot be "the whole pool": the call budget
// is derived from the plan's length, so a thirty-model pool would multiply the
// ceiling by fifteen for a request that will almost always succeed on the
// first call.
const smartReselectDepth = 2

// appendReselectionPool returns the router's pick followed by up to
// smartReselectDepth other members of the pool it was chosen from, in the
// pool's own price order.
//
// Ascending input price, undeclared last — the same rule the modality path
// uses, through the same predicate. The order is not this function's to invent:
// the pick is promoted to position zero and the rest follow, so the tail is
// simply the sorted pool minus what was promoted.
//
// An earlier version ordered by "a provider not already in the plan first,
// then the cheaper model". That put a second answer to a question the recovery
// engine already answers: `selectNext` prefers a different provider after a
// 429 or a 5xx, which is where the choice belongs — it is the one place that
// knows WHICH failure happened, and a quota is only per-provider for the
// failures that are. Deciding it here applied the preference to failures it
// does not fit, and did so with a price rule that disagreed with the one
// beside it on any model declaring an output price but no input price.
func (s *SmartStrategy) appendReselectionPool(ctx context.Context, candidates []core.SmartModelRow,
	selected *core.SmartModelRow, primary *core.RoutingTarget, trace *[]core.TraceEntry, start time.Time,
) []core.RoutingTarget {
	targets := []core.RoutingTarget{*primary}

	ordered := append([]core.SmartModelRow(nil), candidates...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return inputPriceOrMax(ordered[i]) < inputPriceOrMax(ordered[j])
	})

	// Bounded by depth, and separately by a run of lookup failures. Without the
	// second bound a catalogue that resolves nothing rescans the whole pool once
	// per companion; with a pool of a couple of hundred rows that is a
	// millisecond of scanning to produce nothing.
	failures := 0
	for _, c := range ordered {
		if len(targets) == smartReselectDepth+1 || failures > smartReselectDepth {
			break
		}
		if c.ModelID == selected.ModelID {
			continue
		}
		t, err := s.deps.Lookup(ctx, c.ProviderID, c.ModelID)
		if err != nil {
			// Skipped, not fatal: the router's own choice resolved, and a
			// companion that cannot be resolved is one fewer place to fail
			// over to rather than a reason to fail the request.
			s.deps.Logger.Warn("smart: reselection target lookup failed",
				"modelId", c.ModelID, "providerId", c.ProviderID, "error", err)
			failures++
			continue
		}
		targets = append(targets, *t)
		*trace = append(*trace, core.TraceEntry{
			StrategyType: "smart",
			Decision: fmt.Sprintf("re-selection pool: %s [%s/%s] carried behind the router's choice",
				core.FormatTargetFriendly(t), c.ProviderID, c.ModelID),
			DurationMs: int(time.Since(start).Milliseconds()),
		})
	}
	return targets
}

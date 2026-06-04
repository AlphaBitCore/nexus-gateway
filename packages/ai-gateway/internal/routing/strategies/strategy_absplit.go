package strategies

import (
	"context"

	"fmt"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"math/rand/v2"
)

// ABSplitStrategy selects a target via weighted random from inline A/B targets.
type ABSplitStrategy struct {
	lookup core.TargetLookup
}

func (s *ABSplitStrategy) Type() string { return "ab_split" }

func (s *ABSplitStrategy) Evaluate(ctx context.Context, node core.StrategyNode, _ *core.RoutingContext, trace *[]core.TraceEntry, _ int, _ RecurseFunc) ([]core.RoutingTarget, error) {
	if len(node.ABTargets) == 0 {
		*trace = append(*trace, core.TraceEntry{
			StrategyType: "ab_split",
			Decision:     "no abTargets configured — returning no targets",
		})
		return nil, nil
	}

	totalWeight := 0
	for _, t := range node.ABTargets {
		totalWeight += t.Weight
	}
	if totalWeight <= 0 {
		*trace = append(*trace, core.TraceEntry{
			StrategyType: "ab_split",
			Decision:     fmt.Sprintf("total weight is %d — returning no targets", totalWeight),
		})
		return nil, nil
	}

	r := rand.IntN(totalWeight)
	cumulative := 0
	for _, t := range node.ABTargets {
		cumulative += t.Weight
		if r < cumulative {
			target, err := s.lookup(ctx, t.ProviderID, t.ModelID)
			if err != nil {
				*trace = append(*trace, core.TraceEntry{
					StrategyType: "ab_split",
					Decision:     fmt.Sprintf("lookup failed: %s/%s: %v", t.ProviderID, t.ModelID, err),
				})
				return nil, nil
			}
			*trace = append(*trace, core.TraceEntry{
				StrategyType: "ab_split",
				Decision:     fmt.Sprintf("selected %s [%s/%s] (weight %d/%d)", core.FormatTargetFriendly(target), t.ProviderID, t.ModelID, t.Weight, totalWeight),
			})
			return []core.RoutingTarget{*target}, nil
		}
	}

	// Unreachable under correct math. Kept defensively.
	*trace = append(*trace, core.TraceEntry{
		StrategyType: "ab_split",
		Decision:     "weighted selection fell through — returning no targets",
	})
	return nil, nil
}

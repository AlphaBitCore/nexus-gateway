package strategies

import (
	"context"

	"fmt"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"math/rand/v2"
)

// LoadbalanceStrategy selects a child node via weighted random selection.
type LoadbalanceStrategy struct{}

func (s *LoadbalanceStrategy) Type() string { return "loadbalance" }

func (s *LoadbalanceStrategy) Evaluate(ctx context.Context, node core.StrategyNode, rctx *core.RoutingContext, trace *[]core.TraceEntry, depth int, recurse RecurseFunc) ([]core.RoutingTarget, error) {
	if len(node.Weighted) == 0 {
		*trace = append(*trace, core.TraceEntry{
			StrategyType: "loadbalance",
			Decision:     "no weightedTargets configured — returning no targets",
		})
		return nil, nil
	}

	totalWeight := 0
	for _, w := range node.Weighted {
		totalWeight += w.Weight
	}
	if totalWeight <= 0 {
		*trace = append(*trace, core.TraceEntry{
			StrategyType: "loadbalance",
			Decision:     fmt.Sprintf("total weight is %d — returning no targets", totalWeight),
		})
		return nil, nil
	}

	r := rand.IntN(totalWeight)
	cumulative := 0
	for _, w := range node.Weighted {
		cumulative += w.Weight
		if r < cumulative {
			targets, err := recurse(ctx, w.Node, rctx, trace, depth)
			if err != nil {
				return nil, err
			}
			*trace = append(*trace, core.TraceEntry{
				StrategyType: "loadbalance",
				Decision:     fmt.Sprintf("weighted random selected weight=%d of total=%d", w.Weight, totalWeight),
			})
			return targets, nil
		}
	}

	// Unreachable under correct math: rand.IntN(totalWeight) is always < totalWeight.
	// Kept as a defensive last resort so an arithmetic regression cannot silently
	// drop the request on the floor.
	*trace = append(*trace, core.TraceEntry{
		StrategyType: "loadbalance",
		Decision:     "weighted selection fell through — using first bucket",
	})
	return recurse(ctx, node.Weighted[0].Node, rctx, trace, depth)
}

package strategies

import (
	"context"

	"fmt"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
)

// FallbackStrategy concatenates targets from all child nodes in order.
type FallbackStrategy struct{ lookup core.TargetLookup }

func (s *FallbackStrategy) Type() string { return "fallback" }

func (s *FallbackStrategy) Evaluate(ctx context.Context, node core.StrategyNode, _ *core.RoutingContext, trace *[]core.TraceEntry) ([]core.RoutingTarget, error) {
	var all []core.RoutingTarget
	for _, child := range node.Targets {
		target, err := resolveLeaf(ctx, child, s.lookup, "fallback", trace)
		if err != nil {
			return nil, err
		}
		if target != nil {
			all = append(all, *target)
		}
	}
	*trace = append(*trace, core.TraceEntry{
		StrategyType: "fallback",
		Decision:     fmt.Sprintf("concatenated %d targets from %d children", len(all), len(node.Targets)),
	})
	return all, nil
}

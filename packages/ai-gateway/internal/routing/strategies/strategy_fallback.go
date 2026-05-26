package strategies

import (
	"context"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"fmt"
)

// FallbackStrategy concatenates targets from all child nodes in order.
type FallbackStrategy struct{}

func (s *FallbackStrategy) Type() string { return "fallback" }

func (s *FallbackStrategy) Evaluate(ctx context.Context, node core.StrategyNode, rctx *core.RoutingContext, trace *[]core.TraceEntry, depth int, recurse RecurseFunc) ([]core.RoutingTarget, error) {
	var all []core.RoutingTarget
	for _, child := range node.Targets {
		targets, err := recurse(ctx, child, rctx, trace, depth)
		if err != nil {
			return nil, err
		}
		all = append(all, targets...)
	}
	*trace = append(*trace, core.TraceEntry{
		StrategyType: "fallback",
		Decision:     fmt.Sprintf("concatenated %d targets from %d children", len(all), len(node.Targets)),
	})
	return all, nil
}

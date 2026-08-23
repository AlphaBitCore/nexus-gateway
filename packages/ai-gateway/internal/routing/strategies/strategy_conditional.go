package strategies

import (
	"context"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/matcher"

	"fmt"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
)

// ConditionalStrategy evaluates branches in order and recurses into
// the first matching branch, or the default if none match.
type ConditionalStrategy struct{ lookup core.TargetLookup }

func (s *ConditionalStrategy) Type() string { return "conditional" }

func (s *ConditionalStrategy) Evaluate(ctx context.Context, node core.StrategyNode, rctx *core.RoutingContext, trace *[]core.TraceEntry) ([]core.RoutingTarget, error) {
	for i, branch := range node.Conditions {
		if matcher.EvaluateExpression(branch.When, rctx) {
			*trace = append(*trace, core.TraceEntry{
				StrategyType: "conditional",
				Decision:     fmt.Sprintf("branch %d matched", i),
			})
			target, err := resolveLeaf(ctx, branch.Then, s.lookup, "conditional", trace)
			if err != nil || target == nil {
				return nil, err
			}
			return []core.RoutingTarget{*target}, nil
		}
	}
	// No branch matched — evaluate default.
	if node.Default != nil {
		*trace = append(*trace, core.TraceEntry{
			StrategyType: "conditional",
			Decision:     "no branch matched, using default",
		})
		target, err := resolveLeaf(ctx, *node.Default, s.lookup, "conditional default", trace)
		if err != nil || target == nil {
			return nil, err
		}
		return []core.RoutingTarget{*target}, nil
	}
	*trace = append(*trace, core.TraceEntry{
		StrategyType: "conditional",
		Decision:     "no branch matched, no default",
	})
	return nil, nil
}

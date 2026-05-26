package strategies

import (
	"context"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/matcher"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"fmt"
)

// ConditionalStrategy evaluates branches in order and recurses into
// the first matching branch, or the default if none match.
type ConditionalStrategy struct{}

func (s *ConditionalStrategy) Type() string { return "conditional" }

func (s *ConditionalStrategy) Evaluate(ctx context.Context, node core.StrategyNode, rctx *core.RoutingContext, trace *[]core.TraceEntry, depth int, recurse RecurseFunc) ([]core.RoutingTarget, error) {
	for i, branch := range node.Conditions {
		if matcher.EvaluateExpression(branch.When, rctx) {
			*trace = append(*trace, core.TraceEntry{
				StrategyType: "conditional",
				Decision:     fmt.Sprintf("branch %d matched", i),
			})
			return recurse(ctx, branch.Then, rctx, trace, depth)
		}
	}
	// No branch matched — evaluate default.
	if node.Default != nil {
		*trace = append(*trace, core.TraceEntry{
			StrategyType: "conditional",
			Decision:     "no branch matched, using default",
		})
		return recurse(ctx, *node.Default, rctx, trace, depth)
	}
	*trace = append(*trace, core.TraceEntry{
		StrategyType: "conditional",
		Decision:     "no branch matched, no default",
	})
	return nil, nil
}

package strategies

import (
	"context"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
)

// PolicyStrategy is a no-op strategy for stage-0 policy narrowing.
// Policy nodes are merged into narrowing state by the resolver,
// not evaluated for targets.
type PolicyStrategy struct{}

func (s *PolicyStrategy) Type() string { return "policy" }

func (s *PolicyStrategy) Evaluate(_ context.Context, _ core.StrategyNode, _ *core.RoutingContext, trace *[]core.TraceEntry, _ int, _ RecurseFunc) ([]core.RoutingTarget, error) {
	*trace = append(*trace, core.TraceEntry{
		StrategyType: "policy",
		Decision:     "policy applied to narrowing (no targets)",
	})
	return nil, nil
}

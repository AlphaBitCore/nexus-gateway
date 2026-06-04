package strategies

import (
	"context"

	"fmt"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
)

// SingleStrategy resolves a single provider+model pair into a routing target.
type SingleStrategy struct {
	lookup core.TargetLookup
}

func (s *SingleStrategy) Type() string { return "single" }

func (s *SingleStrategy) Evaluate(ctx context.Context, node core.StrategyNode, _ *core.RoutingContext, trace *[]core.TraceEntry, _ int, _ RecurseFunc) ([]core.RoutingTarget, error) {
	target, err := s.lookup(ctx, node.ProviderID, node.ModelID)
	if err != nil {
		*trace = append(*trace, core.TraceEntry{
			StrategyType: "single",
			Decision:     fmt.Sprintf("lookup failed: %s/%s: %v", node.ProviderID, node.ModelID, err),
		})
		return nil, nil // soft failure — no targets
	}
	*trace = append(*trace, core.TraceEntry{
		StrategyType: "single",
		Decision:     fmt.Sprintf("resolved %s [%s/%s]", core.FormatTargetFriendly(target), node.ProviderID, node.ModelID),
	})
	return []core.RoutingTarget{*target}, nil
}

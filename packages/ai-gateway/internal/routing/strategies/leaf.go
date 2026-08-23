package strategies

import (
	"context"
	"fmt"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
)

// resolveLeaf turns a nested strategy node into the one target it names.
//
// Every multi-target strategy in this package points at leaves: a weighted
// entry, a conditional branch, a fallback step. The admin surface only ever
// produces `{"type":"single", providerId, modelId}` there, and only ever reads
// that shape back — a deeper nesting could not be displayed in the rule editor,
// let alone edited.
//
// So the generic recursive evaluator behind those pointers served no user path
// while charging for one: a depth budget threaded through every strategy
// signature, an error class for exceeding it, and the possibility of a rule
// whose behaviour nobody could see. Reading the leaf directly costs one type
// check.
//
// The persisted shape is untouched. A stored `{weight, node:{type:"single"…}}`
// still validates, still round-trips, and still means what it meant; what is
// gone is the evaluator that would have followed it anywhere.
//
// A node that is not a leaf yields no target and says so in the trace, rather
// than erroring: the walk should reach the rules below a misconfigured one, and
// refusing the shape belongs at the write boundary where the admin can be told.
func resolveLeaf(ctx context.Context, node core.StrategyNode, lookup core.TargetLookup, where string, trace *[]core.TraceEntry) (*core.RoutingTarget, error) {
	if node.Type != "" && node.Type != "single" {
		*trace = append(*trace, core.TraceEntry{
			StrategyType: where,
			Decision: fmt.Sprintf("%s points at a %q node; only a single provider+model leaf is "+
				"supported here — returning no target", where, node.Type),
		})
		return nil, nil
	}
	if node.ProviderID == "" || node.ModelID == "" {
		*trace = append(*trace, core.TraceEntry{
			StrategyType: where,
			Decision:     where + " leaf names no provider+model — returning no target",
		})
		return nil, nil
	}
	if lookup == nil {
		return nil, fmt.Errorf("%s: no target lookup wired", where)
	}
	return lookup(ctx, node.ProviderID, node.ModelID)
}

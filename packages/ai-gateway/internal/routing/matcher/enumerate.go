package matcher

import (
	"context"

	"fmt"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
)

// enumerateDepthLimit guards against malformed cyclic configs when walking
// the strategy tree. The live Evaluate path uses core.MaxRoutingDepth; mirror it.
const enumerateDepthLimit = core.MaxRoutingDepth

// EnumerateTerminalTargets walks a strategy node tree and returns every
// terminal target reachable, each with the cumulative selection probability
// that the live stochastic router would use.
//
// This is the deterministic counterpart to StrategyRegistry.Evaluate: where
// Evaluate rolls a weighted die and returns one branch, EnumerateTerminalTargets
// returns all branches with their probability weights intact. It is intended
// for simulate / explain flows so operators can see the full distribution of
// a loadbalance or ab_split rule, not just the single branch one simulate
// call happened to hit.
//
// Behavior per strategy:
//   - single: one target, probability 1.0
//   - fallback: every listed target, probability 1.0 (all get a chance on retry)
//   - loadbalance: every weighted child, probability weight/sum (recurses)
//   - conditional: every then-branch plus default; Matched reflects whether
//     the predicate evaluates true against rctx. Only Matched branches plus
//     the default are assigned a non-zero probability.
//   - ab_split: every ab target, probability weight/sum
//   - policy: no terminal targets (stage-0 only); empty slice
//   - smart: not enumerable without a full live deps path; returns empty.
//     Callers should disclose this limitation in the UI.
//
// Lookup failures (disabled provider/model, missing records) do not abort
// enumeration; the affected entries are returned with an explanatory Note
// and no Target.ProviderName, so the UI can still tell operators "branch X
// would fire Y% of the time but the target is currently unresolvable".
func EnumerateTerminalTargets(ctx context.Context, node core.StrategyNode, rctx *core.RoutingContext, lookup core.TargetLookup) []core.BranchedTarget {
	if lookup == nil {
		return nil
	}
	return enumerate(ctx, node, rctx, lookup, "", 1.0, 0)
}

func enumerate(ctx context.Context, node core.StrategyNode, rctx *core.RoutingContext, lookup core.TargetLookup, path string, prob float64, depth int) []core.BranchedTarget {
	if depth > enumerateDepthLimit {
		return nil
	}

	switch node.Type {
	case "single":
		// Inlined (instead of calling singleBranch) so the path can use the
		// resolved target's friendly identifier — the surrounding branch
		// JSON already surfaces providerId / modelId, so duplicating UUIDs
		// in the path is just noise. On lookup failure we fall back to
		// UUIDs so the failing branch remains unambiguously identifiable.
		target, err := lookup(ctx, node.ProviderID, node.ModelID)
		if err != nil || target == nil {
			return []core.BranchedTarget{{
				Target: core.RoutingTarget{
					ProviderID: node.ProviderID,
					ModelID:    node.ModelID,
				},
				Probability: prob,
				Path:        joinPath(path, fmt.Sprintf("single(%s/%s)", node.ProviderID, node.ModelID)),
				Matched:     true,
				Note:        explainLookupErr(err),
			}}
		}
		return []core.BranchedTarget{{
			Target:      *target,
			Probability: prob,
			Path:        joinPath(path, fmt.Sprintf("single(%s)", core.FormatTargetPath(target))),
			Matched:     true,
		}}

	case "fallback":
		var out []core.BranchedTarget
		for i, child := range node.Targets {
			// Each fallback target gets a full chance on retry; probability
			// stays at the parent's (no division among siblings).
			childPath := joinPath(path, fmt.Sprintf("fallback[%d]", i))
			out = append(out, enumerate(ctx, child, rctx, lookup, childPath, prob, depth+1)...)
		}
		return out

	case "loadbalance":
		return enumerateWeighted(ctx, node.Weighted, rctx, lookup, path, prob, depth)

	case "conditional":
		return enumerateConditional(ctx, node, rctx, lookup, path, prob, depth)

	case "ab_split":
		return enumerateABSplit(ctx, node.ABTargets, lookup, path, prob)

	case "policy":
		// Stage-0 only; contributes narrowing, not terminal targets.
		return nil

	case "smart":
		// Cannot be enumerated without a live smart deps + message corpus;
		// simulate already surfaces its own trace for smart flows.
		return nil

	default:
		return nil
	}
}

func enumerateWeighted(ctx context.Context, weighted []core.WeightedTarget, rctx *core.RoutingContext, lookup core.TargetLookup, path string, prob float64, depth int) []core.BranchedTarget {
	if len(weighted) == 0 {
		return nil
	}
	totalWeight := 0
	for _, w := range weighted {
		totalWeight += w.Weight
	}
	if totalWeight <= 0 {
		return nil
	}

	var out []core.BranchedTarget
	for i, w := range weighted {
		branchProb := prob * (float64(w.Weight) / float64(totalWeight))
		childPath := joinPath(path, fmt.Sprintf("loadbalance[%d,w=%d/%d]", i, w.Weight, totalWeight))
		out = append(out, enumerate(ctx, w.Node, rctx, lookup, childPath, branchProb, depth+1)...)
	}
	return out
}

func enumerateConditional(ctx context.Context, node core.StrategyNode, rctx *core.RoutingContext, lookup core.TargetLookup, path string, prob float64, depth int) []core.BranchedTarget {
	var out []core.BranchedTarget
	matchedAny := false
	for i, br := range node.Conditions {
		matched := rctx != nil && EvaluateExpression(br.When, rctx)
		if matched {
			matchedAny = true
		}
		childPath := joinPath(path, fmt.Sprintf("conditional[%d,matched=%t]", i, matched))
		branchProb := 0.0
		if matched {
			branchProb = prob
		}
		for _, b := range enumerate(ctx, br.Then, rctx, lookup, childPath, branchProb, depth+1) {
			b.Matched = matched
			out = append(out, b)
		}
	}
	if node.Default != nil {
		// The default fires iff no branch matched. That means the default
		// alone carries the full probability whenever matchedAny == false.
		defaultProb := 0.0
		if !matchedAny {
			defaultProb = prob
		}
		childPath := joinPath(path, fmt.Sprintf("conditional[default,applied=%t]", !matchedAny))
		for _, b := range enumerate(ctx, *node.Default, rctx, lookup, childPath, defaultProb, depth+1) {
			b.Matched = !matchedAny
			out = append(out, b)
		}
	}
	return out
}

func enumerateABSplit(ctx context.Context, targets []core.ABTarget, lookup core.TargetLookup, path string, prob float64) []core.BranchedTarget {
	if len(targets) == 0 {
		return nil
	}
	totalWeight := 0
	for _, t := range targets {
		totalWeight += t.Weight
	}
	if totalWeight <= 0 {
		return nil
	}

	out := make([]core.BranchedTarget, 0, len(targets))
	for i, t := range targets {
		branchProb := prob * (float64(t.Weight) / float64(totalWeight))
		p := joinPath(path, fmt.Sprintf("ab_split[%d,w=%d/%d]", i, t.Weight, totalWeight))
		out = append(out, singleBranch(ctx, t.ProviderID, t.ModelID, lookup, p, branchProb, true))
	}
	return out
}

func singleBranch(ctx context.Context, providerID, modelID string, lookup core.TargetLookup, path string, prob float64, matched bool) core.BranchedTarget {
	target, err := lookup(ctx, providerID, modelID)
	if err != nil || target == nil {
		return core.BranchedTarget{
			Target: core.RoutingTarget{
				ProviderID: providerID,
				ModelID:    modelID,
			},
			Probability: prob,
			Path:        path,
			Matched:     matched,
			Note:        explainLookupErr(err),
		}
	}
	return core.BranchedTarget{
		Target:      *target,
		Probability: prob,
		Path:        path,
		Matched:     matched,
	}
}

func explainLookupErr(err error) string {
	if err == nil {
		return "lookup returned nil target"
	}
	return "lookup failed: " + err.Error()
}

func joinPath(parent, child string) string {
	if parent == "" {
		return child
	}
	return parent + " > " + child
}

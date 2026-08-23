package matcher

import (
	"context"

	"fmt"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
)

// EnumerateTerminalTargets returns every target a rule can reach, each with the
// selection probability the live router would use.
//
// It walks exactly as far as the evaluator does: one level. An entry inside a
// strategy names a provider and a model, and a stored entry that names another
// strategy instead is reported as UNREACHABLE rather than descended into.
// Descending was what this walker did while the evaluator still recursed;
// keeping it afterwards made simulate publish a distribution over branches a
// live request can never take — which is worse than publishing nothing, because
// simulate is what an admin uses to check a rule before trusting it.
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
//   - loadbalance: every weighted entry, probability weight/sum
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
	// A child that is not a leaf is enumerated as UNREACHABLE, not walked.
	//
	// This walker used to descend into any node shape, because the evaluator
	// did too. It no longer does: children are provider+model leaves, and a
	// nested strategy resolves to nothing. Descending anyway made simulate
	// report a distribution over branches the live request can never take —
	// worse than showing nothing, because simulate is what an admin uses to
	// check a rule before trusting it.
	//
	// The write boundary now refuses new nesting, so what reaches here is a row
	// stored before that. Naming it is the point: the operator sees the branch
	// they wrote AND that it is inert.
	if depth > 0 && node.Type != "" && node.Type != "single" {
		return []core.BranchedTarget{{
			Target:      core.RoutingTarget{ProviderID: node.ProviderID, ModelID: node.ModelID},
			Probability: prob,
			Path:        joinPath(path, fmt.Sprintf("%s(nested)", node.Type)),
			Matched:     false,
			Note: "a nested " + node.Type + " is not evaluated: the gateway resolves an " +
				"entry inside a strategy as a provider+model leaf, so this branch routes " +
				"nothing. Re-author it as a leaf, or split it into its own rule.",
		}}
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

	case "latency":
		return enumerateLatency(ctx, node.LatencyTargets, lookup, path, prob)

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

// enumerateLatency lists every latency target as a reachable terminal. Unlike
// the weighted strategies the live selection order is driven by runtime p95 (not
// a static weight), so a meaningful per-branch probability cannot be computed
// offline; each target is reported with the even share prob/n and a Note that
// the live order is latency-driven with bounded exploration.
func enumerateLatency(ctx context.Context, targets []core.LatencyTarget, lookup core.TargetLookup, path string, prob float64) []core.BranchedTarget {
	if len(targets) == 0 {
		return nil
	}
	share := prob / float64(len(targets))
	out := make([]core.BranchedTarget, 0, len(targets))
	for i, t := range targets {
		p := joinPath(path, fmt.Sprintf("latency[%d]", i))
		b := singleBranch(ctx, t.ProviderID, t.ModelID, lookup, p, share, true)
		if b.Note == "" {
			b.Note = "order is runtime p95-ranked with bounded exploration"
		}
		out = append(out, b)
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

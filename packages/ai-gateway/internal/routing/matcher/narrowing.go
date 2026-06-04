package matcher

import (
	"context"

	"encoding/json"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"sort"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
)

// NarrowingState tracks cumulative allow/deny sets from stage-0 policy rules.
type NarrowingState struct {
	ModelAllow    map[string]bool // nil = no restriction; non-nil = only these allowed
	ModelDeny     map[string]bool
	ProviderAllow map[string]bool // nil = no restriction
	ProviderDeny  map[string]bool
}

// EmptyNarrowingState returns a state with no restrictions.
func EmptyNarrowingState() NarrowingState {
	return NarrowingState{
		ModelAllow:    nil,
		ModelDeny:     make(map[string]bool),
		ProviderAllow: nil,
		ProviderDeny:  make(map[string]bool),
	}
}

// MergePolicyIntoState applies a policy strategy's allow/deny lists to the
// cumulative narrowing state.
// Allow-lists: intersect (AND — becomes more restrictive).
// Deny-lists: union (OR — becomes more restrictive).
func MergePolicyIntoState(state NarrowingState, node core.StrategyNode) NarrowingState {
	if len(node.AllowModelIDs) > 0 {
		state.ModelAllow = intersect(state.ModelAllow, toSet(node.AllowModelIDs))
	}
	if len(node.DenyModelIDs) > 0 {
		for _, id := range node.DenyModelIDs {
			state.ModelDeny[id] = true
		}
	}
	if len(node.AllowProviderIDs) > 0 {
		state.ProviderAllow = intersect(state.ProviderAllow, toSet(node.AllowProviderIDs))
	}
	if len(node.DenyProviderIDs) > 0 {
		for _, id := range node.DenyProviderIDs {
			state.ProviderDeny[id] = true
		}
	}
	return state
}

// IsTargetAllowed checks whether a model+provider pair passes narrowing.
func IsTargetAllowed(state NarrowingState, modelID, providerID string) bool {
	if state.ModelDeny[modelID] || state.ProviderDeny[providerID] {
		return false
	}
	if state.ModelAllow != nil && !state.ModelAllow[modelID] {
		return false
	}
	if state.ProviderAllow != nil && !state.ProviderAllow[providerID] {
		return false
	}
	return true
}

// IsNarrowingEmpty returns true if no restrictions are in effect.
func IsNarrowingEmpty(state NarrowingState) bool {
	return state.ModelAllow == nil && len(state.ModelDeny) == 0 &&
		state.ProviderAllow == nil && len(state.ProviderDeny) == 0
}

// ToSummary converts the state to a serializable summary.
func ToSummary(state NarrowingState) core.NarrowingSummary {
	return core.NarrowingSummary{
		AllowModelIDs:    sortedKeys(state.ModelAllow),
		DenyModelIDs:     sortedKeys(state.ModelDeny),
		AllowProviderIDs: sortedKeys(state.ProviderAllow),
		DenyProviderIDs:  sortedKeys(state.ProviderDeny),
	}
}

// NarrowingEngine applies stage-0 policy narrowing to target lists.
type NarrowingEngine struct{}

// Apply evaluates all stage-0 policy rules and returns accumulated narrowing state
// plus trace entries for the pipeline log.
func (n *NarrowingEngine) Apply(ctx context.Context, rules []store.RoutingRule, rctx *core.RoutingContext, logger interface {
	Warn(msg string, args ...any)
}, matchFn func(store.RoutingRule, string, *core.RoutingContext) bool) (NarrowingState, []core.PipelineTraceEntry) {
	state := EmptyNarrowingState()
	var trace []core.PipelineTraceEntry

	for _, rule := range rules {
		if rule.PipelineStage != 0 {
			continue
		}
		if !matchFn(rule, rctx.RequestedModel.ID, rctx) {
			continue
		}

		var node core.StrategyNode
		if err := json.Unmarshal(rule.Config, &node); err != nil {
			logger.Warn("invalid strategy config", "ruleId", rule.ID, "error", err)
			continue
		}
		if node.Type == "policy" {
			state = MergePolicyIntoState(state, node)
		}

		trace = append(trace, core.PipelineTraceEntry{
			Stage:    0,
			Decision: "policy rule applied",
		})
	}

	return state, trace
}

// Filter removes targets that violate narrowing constraints and VK allowed models.
func (n *NarrowingEngine) Filter(targets []core.RoutingTarget, state NarrowingState, rctx *core.RoutingContext) []core.RoutingTarget {
	var filtered []core.RoutingTarget
	for _, t := range targets {
		if !IsTargetAllowed(state, t.ModelID, t.ProviderID) {
			continue
		}
		if rctx.VirtualKey != nil && len(rctx.VirtualKey.AllowedModels) > 0 {
			if !core.ModelMatchesAllowedRefs(t.ModelID, t.ProviderModelID, t.ProviderID, rctx.VirtualKey.AllowedModels) {
				continue
			}
		}
		filtered = append(filtered, t)
	}
	return filtered
}

func toSet(ids []string) map[string]bool {
	m := make(map[string]bool, len(ids))
	for _, id := range ids {
		m[id] = true
	}
	return m
}

func intersect(existing, incoming map[string]bool) map[string]bool {
	if existing == nil {
		// First policy sets the allow list.
		return incoming
	}
	// Subsequent policies intersect.
	result := make(map[string]bool)
	for id := range existing {
		if incoming[id] {
			result[id] = true
		}
	}
	return result
}

func sortedKeys(m map[string]bool) []string {
	if m == nil {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

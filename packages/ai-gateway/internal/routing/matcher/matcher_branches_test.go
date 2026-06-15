// Package matcher — matcher_gap_test.go covers branches not reached by the
// existing test files.
//
// Named failure modes:
//   - NarrowingEngine.Apply: stage-0 policy rules applied, non-stage-0 skipped,
//     non-matching rule skipped, invalid JSON config (warn + continue),
//     non-policy node type skipped
//   - NarrowingEngine.Filter: VKContext.AllowedModels filter
//   - RuleMatchesContext: conds.Projects with matching/non-matching VK projectID;
//     conds.VirtualKeys glob match and no-match
//   - singleBranch: target == nil with nil error → uses "lookup returned nil target" note
//   - enumerateWeighted: empty + zero total weight
//   - enumerateABSplit: empty + zero total weight
//   - evalOperators: $not with non-matching sub-expression
package matcher

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
)

// logWarnCapture captures Warn calls for the Apply logger interface.
type logWarnCapture struct {
	warns []string
}

func (l *logWarnCapture) Warn(msg string, args ...any) {
	l.warns = append(l.warns, msg)
}

func matchFnAlwaysTrue(_ store.RoutingRule, _ string, _ *core.RoutingContext) bool  { return true }
func matchFnAlwaysFalse(_ store.RoutingRule, _ string, _ *core.RoutingContext) bool { return false }

func TestNarrowingEngine_Apply_noRules_emptyState(t *testing.T) {
	eng := &NarrowingEngine{}
	logger := &logWarnCapture{}
	rctx := &core.RoutingContext{}
	state, trace := eng.Apply(context.Background(), nil, rctx, logger, matchFnAlwaysTrue)
	if !IsNarrowingEmpty(state) {
		t.Error("expected empty narrowing state for no rules")
	}
	if len(trace) != 0 {
		t.Errorf("expected 0 trace entries, got %d", len(trace))
	}
}

func TestNarrowingEngine_Apply_nonStage0Rules_skipped(t *testing.T) {
	eng := &NarrowingEngine{}
	logger := &logWarnCapture{}
	// Rule at stage 1 — should be skipped by Apply (which only processes stage 0).
	rules := []store.RoutingRule{
		{
			ID:            "r1",
			PipelineStage: 1,
			Config:        json.RawMessage(`{"type":"policy","denyModelIds":["gpt-4"]}`),
		},
	}
	rctx := &core.RoutingContext{}
	state, trace := eng.Apply(context.Background(), rules, rctx, logger, matchFnAlwaysTrue)
	if !IsNarrowingEmpty(state) {
		t.Error("stage-1 rule should not affect narrowing state")
	}
	if len(trace) != 0 {
		t.Errorf("expected 0 trace entries for stage-1 rules, got %d", len(trace))
	}
}

func TestNarrowingEngine_Apply_nonMatchingRule_skipped(t *testing.T) {
	eng := &NarrowingEngine{}
	logger := &logWarnCapture{}
	rules := []store.RoutingRule{
		{
			ID:            "r1",
			PipelineStage: 0,
			Config:        json.RawMessage(`{"type":"policy","denyModelIds":["gpt-4"]}`),
		},
	}
	rctx := &core.RoutingContext{}
	// matchFn always returns false → rule is skipped
	state, trace := eng.Apply(context.Background(), rules, rctx, logger, matchFnAlwaysFalse)
	if !IsNarrowingEmpty(state) {
		t.Error("non-matching rule should not affect narrowing state")
	}
	if len(trace) != 0 {
		t.Errorf("expected 0 trace entries, got %d", len(trace))
	}
}

func TestNarrowingEngine_Apply_invalidJSON_warnsAndContinues(t *testing.T) {
	eng := &NarrowingEngine{}
	logger := &logWarnCapture{}
	rules := []store.RoutingRule{
		{
			ID:            "r-bad",
			PipelineStage: 0,
			Config:        json.RawMessage(`{not valid json`),
		},
	}
	rctx := &core.RoutingContext{}
	state, trace := eng.Apply(context.Background(), rules, rctx, logger, matchFnAlwaysTrue)
	// State should remain empty (the bad rule was skipped).
	if !IsNarrowingEmpty(state) {
		t.Error("invalid config should be skipped, state should stay empty")
	}
	if len(trace) != 0 {
		t.Errorf("expected 0 trace entries for bad config, got %d", len(trace))
	}
	if len(logger.warns) == 0 {
		t.Error("expected at least one Warn call for invalid config")
	}
}

func TestNarrowingEngine_Apply_policyRule_appliesNarrowing(t *testing.T) {
	eng := &NarrowingEngine{}
	logger := &logWarnCapture{}
	rules := []store.RoutingRule{
		{
			ID:            "r-policy",
			PipelineStage: 0,
			Config:        json.RawMessage(`{"type":"policy","denyModelIds":["gpt-3.5-turbo"]}`),
		},
	}
	rctx := &core.RoutingContext{}
	state, trace := eng.Apply(context.Background(), rules, rctx, logger, matchFnAlwaysTrue)
	if IsNarrowingEmpty(state) {
		t.Error("expected non-empty state after policy rule")
	}
	if !IsTargetAllowed(state, "gpt-4", "openai") {
		t.Error("gpt-4 should be allowed")
	}
	if IsTargetAllowed(state, "gpt-3.5-turbo", "openai") {
		t.Error("gpt-3.5-turbo should be denied")
	}
	if len(trace) != 1 {
		t.Errorf("expected 1 trace entry, got %d", len(trace))
	}
}

func TestNarrowingEngine_Apply_nonPolicyNode_skipsNarrowing(t *testing.T) {
	eng := &NarrowingEngine{}
	logger := &logWarnCapture{}
	// Stage 0 but non-policy type — should not affect state.
	rules := []store.RoutingRule{
		{
			ID:            "r-single",
			PipelineStage: 0,
			Config:        json.RawMessage(`{"type":"single","providerId":"openai","modelId":"gpt-4"}`),
		},
	}
	rctx := &core.RoutingContext{}
	state, trace := eng.Apply(context.Background(), rules, rctx, logger, matchFnAlwaysTrue)
	// Non-policy node: trace appended but state unchanged.
	if !IsNarrowingEmpty(state) {
		t.Error("non-policy node should not affect narrowing state")
	}
	// Trace entry is still appended (any matching stage-0 rule adds a trace).
	if len(trace) != 1 {
		t.Errorf("expected 1 trace entry for matching non-policy node, got %d", len(trace))
	}
}

// NarrowingEngine.Filter: VirtualKey.AllowedModels

func TestNarrowingEngine_Filter_virtualKeyAllowedModels(t *testing.T) {
	eng := &NarrowingEngine{}
	state := EmptyNarrowingState()
	targets := []core.RoutingTarget{
		{ModelID: "m-allowed", ProviderModelID: "gpt-4", ProviderID: "openai"},
		{ModelID: "m-denied", ProviderModelID: "claude-3", ProviderID: "anthropic"},
	}
	rctx := &core.RoutingContext{
		VirtualKey: &core.VKContext{
			// AllowedModelRef must specify the ProviderID for the match to work
			// (ModelMatchesAllowedRefs skips refs whose ProviderID doesn't match).
			AllowedModels: []store.AllowedModelRef{
				{ModelID: "m-allowed", ProviderID: "openai"},
			},
		},
	}
	filtered := eng.Filter(targets, state, rctx)
	if len(filtered) != 1 || filtered[0].ModelID != "m-allowed" {
		t.Errorf("expected 1 allowed target, got %v", filtered)
	}
}

func TestNarrowingEngine_Filter_noVirtualKey_allowsAll(t *testing.T) {
	eng := &NarrowingEngine{}
	state := EmptyNarrowingState()
	targets := []core.RoutingTarget{
		{ModelID: "m1", ProviderModelID: "gpt-4", ProviderID: "openai"},
		{ModelID: "m2", ProviderModelID: "claude-3", ProviderID: "anthropic"},
	}
	rctx := &core.RoutingContext{VirtualKey: nil}
	filtered := eng.Filter(targets, state, rctx)
	if len(filtered) != 2 {
		t.Errorf("expected 2 targets without VK filter, got %d", len(filtered))
	}
}

// RuleMatchesContext: VirtualKeys glob

func TestRuleMatchesContext_virtualKeys_globMatch(t *testing.T) {
	conds := &core.MatchConditions{
		VirtualKeys: []string{"vk-prod-*"},
	}
	rctx := &core.RoutingContext{
		VirtualKey: &core.VKContext{Name: "vk-prod-eu"},
	}
	if !RuleMatchesContext(conds, "", rctx) {
		t.Error("glob vk-prod-* should match vk-prod-eu")
	}
}

func TestRuleMatchesContext_virtualKeys_noMatch(t *testing.T) {
	conds := &core.MatchConditions{
		VirtualKeys: []string{"vk-prod-*"},
	}
	rctx := &core.RoutingContext{
		VirtualKey: &core.VKContext{Name: "vk-dev-eu"},
	}
	if RuleMatchesContext(conds, "", rctx) {
		t.Error("glob vk-prod-* should not match vk-dev-eu")
	}
}

func TestRuleMatchesContext_virtualKeys_nilVirtualKey_noMatch(t *testing.T) {
	conds := &core.MatchConditions{
		VirtualKeys: []string{"vk-prod-*"},
	}
	rctx := &core.RoutingContext{VirtualKey: nil}
	// No VK → empty name → glob must not match non-"" pattern
	if RuleMatchesContext(conds, "", rctx) {
		t.Error("no VK name should not match non-empty pattern")
	}
}

func TestRuleMatchesContext_projects_matchingVK(t *testing.T) {
	conds := &core.MatchConditions{
		Projects: []string{"proj-eu"},
	}
	rctx := &core.RoutingContext{
		VirtualKey: &core.VKContext{ProjectID: "proj-eu"},
	}
	if !RuleMatchesContext(conds, "", rctx) {
		t.Error("projectID proj-eu should match condition proj-eu")
	}
}

func TestRuleMatchesContext_projects_nonMatchingVK(t *testing.T) {
	conds := &core.MatchConditions{
		Projects: []string{"proj-eu"},
	}
	rctx := &core.RoutingContext{
		VirtualKey: &core.VKContext{ProjectID: "proj-us"},
	}
	if RuleMatchesContext(conds, "", rctx) {
		t.Error("projectID proj-us should not match condition proj-eu")
	}
}

// singleBranch: nil target with nil error

func TestSingleBranch_nilTargetNilError_reportsNote(t *testing.T) {
	// A lookup that returns nil target AND nil error should produce a BranchedTarget
	// with a "lookup returned nil target" note.
	nilTargetLookup := func(_ context.Context, providerID, modelID string) (*core.RoutingTarget, error) {
		return nil, nil // nil target, no error
	}
	result := singleBranch(context.Background(), "p", "m", nilTargetLookup, "path", 1.0, true)
	if result.Note != "lookup returned nil target" {
		t.Errorf("note: got %q, want 'lookup returned nil target'", result.Note)
	}
	if result.Target.ProviderID != "p" || result.Target.ModelID != "m" {
		t.Errorf("target IDs should be set from inputs: %+v", result.Target)
	}
}

// enumerateWeighted: empty and zero total weight edges

func TestEnumerateWeighted_empty_returnsNil(t *testing.T) {
	result := enumerateWeighted(context.Background(), nil, &core.RoutingContext{}, enumLookup, "", 1.0, 0)
	if result != nil {
		t.Errorf("expected nil for empty weighted list, got %v", result)
	}
}

func TestEnumerateWeighted_zeroTotalWeight_returnsNil(t *testing.T) {
	weighted := []core.WeightedTarget{
		{Weight: 0, Node: core.StrategyNode{Type: "single", ProviderID: "p", ModelID: "m"}},
	}
	result := enumerateWeighted(context.Background(), weighted, &core.RoutingContext{}, enumLookup, "", 1.0, 0)
	if result != nil {
		t.Errorf("expected nil for zero total weight, got %v", result)
	}
}

// enumerateABSplit: empty and zero total weight edges

func TestEnumerateABSplit_empty_returnsNil(t *testing.T) {
	result := enumerateABSplit(context.Background(), nil, enumLookup, "", 1.0)
	if result != nil {
		t.Errorf("expected nil for empty ab targets, got %v", result)
	}
}

func TestEnumerateABSplit_zeroTotalWeight_returnsNil(t *testing.T) {
	targets := []core.ABTarget{
		{ProviderID: "p", ModelID: "m", Weight: 0},
	}
	result := enumerateABSplit(context.Background(), targets, enumLookup, "", 1.0)
	if result != nil {
		t.Errorf("expected nil for zero total weight, got %v", result)
	}
}

// evalOperators: $not with non-matching sub-expression

func TestEvalOperators_not_operator_nonMatching(t *testing.T) {
	// $not:{$eq:"chat"} on a "chat" value → inner matches → outer returns false
	rctx := &core.RoutingContext{
		RequestedModel: core.RequestedModel{Type: "chat"},
	}
	expr := map[string]any{
		"requestedModel.type": map[string]any{
			"$not": map[string]any{"$eq": "chat"},
		},
	}
	if EvaluateExpression(expr, rctx) {
		t.Error("$not:$eq:chat should return false when value is 'chat'")
	}
}

func TestEvalOperators_not_operator_matching(t *testing.T) {
	// $not:{$eq:"embedding"} on a "chat" value → inner doesn't match → outer returns true
	rctx := &core.RoutingContext{
		RequestedModel: core.RequestedModel{Type: "chat"},
	}
	expr := map[string]any{
		"requestedModel.type": map[string]any{
			"$not": map[string]any{"$eq": "embedding"},
		},
	}
	if !EvaluateExpression(expr, rctx) {
		t.Error("$not:$eq:embedding should return true when value is 'chat'")
	}
}

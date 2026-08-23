// Package matcher — matcher_gap_test.go covers branches not reached by the
// existing test files.
//
// Named failure modes:
//
//	  non-matching rule skipped, invalid JSON config (warn + continue),
//	  non-policy node type skipped
//	- RuleMatchesContext: conds.Projects with matching/non-matching VK projectID;
//	  conds.VirtualKeys glob match and no-match
//	- singleBranch: target == nil with nil error → uses "lookup returned nil target" note
//	- enumerateWeighted: empty + zero total weight
//	- enumerateABSplit: empty + zero total weight
//	- evalOperators: $not with non-matching sub-expression
package matcher

import (
	"context"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
)

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

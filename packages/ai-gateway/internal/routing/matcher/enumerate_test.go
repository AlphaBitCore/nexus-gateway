package matcher

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
)

var errStub = errors.New("stub lookup failure")

// enumLookup returns a target whose ProviderName exposes both IDs so tests
// can assert lookup was invoked per branch.
func enumLookup(_ context.Context, providerID, modelID string) (*core.RoutingTarget, error) {
	return &core.RoutingTarget{
		ProviderID:      providerID,
		ProviderName:    providerID + "-name",
		ModelID:         modelID,
		ModelName:       modelID + "-name",
		ProviderModelID: modelID,
	}, nil
}

func sumProb(branches []core.BranchedTarget) float64 {
	total := 0.0
	for _, b := range branches {
		total += b.Probability
	}
	return total
}

func findBranch(branches []core.BranchedTarget, providerID, modelID string) *core.BranchedTarget {
	for i := range branches {
		if branches[i].Target.ProviderID == providerID && branches[i].Target.ModelID == modelID {
			return &branches[i]
		}
	}
	return nil
}

func TestEnumerate_Single_OneBranchProb1(t *testing.T) {
	node := core.StrategyNode{Type: "single", ProviderID: "openai", ModelID: "gpt-4"}
	branches := EnumerateTerminalTargets(context.Background(), node, &core.RoutingContext{}, enumLookup)
	if len(branches) != 1 {
		t.Fatalf("expected 1 branch, got %d", len(branches))
	}
	if branches[0].Probability != 1.0 {
		t.Errorf("single should have prob=1.0, got %v", branches[0].Probability)
	}
	if branches[0].Target.ProviderName != "openai-name" {
		t.Errorf("lookup not invoked; got %+v", branches[0].Target)
	}
}

// TestEnumerate_Loadbalance_WeightsBecomeProbabilities is the core fix for
// the simulate UX gap: stochastic loadbalance now surfaces every branch
// deterministically, with weight-derived probabilities that sum to 1.0.
func TestEnumerate_Loadbalance_WeightsBecomeProbabilities(t *testing.T) {
	node := core.StrategyNode{
		Type:      "loadbalance",
		Algorithm: "weighted",
		Weighted: []core.WeightedTarget{
			{Weight: 70, Node: core.StrategyNode{Type: "single", ProviderID: "openai", ModelID: "gpt-4"}},
			{Weight: 30, Node: core.StrategyNode{Type: "single", ProviderID: "google", ModelID: "gemini"}},
		},
	}
	branches := EnumerateTerminalTargets(context.Background(), node, &core.RoutingContext{}, enumLookup)
	if len(branches) != 2 {
		t.Fatalf("expected 2 branches, got %d", len(branches))
	}
	openai := findBranch(branches, "openai", "gpt-4")
	google := findBranch(branches, "google", "gemini")
	if openai == nil || google == nil {
		t.Fatalf("branches missing: %+v", branches)
	}
	if math.Abs(openai.Probability-0.7) > 1e-9 {
		t.Errorf("openai prob = %v, want 0.7", openai.Probability)
	}
	if math.Abs(google.Probability-0.3) > 1e-9 {
		t.Errorf("google prob = %v, want 0.3", google.Probability)
	}
	if math.Abs(sumProb(branches)-1.0) > 1e-9 {
		t.Errorf("probabilities should sum to 1.0, got %v", sumProb(branches))
	}
}

func TestEnumerate_NestedLoadbalance_ProbabilitiesMultiply(t *testing.T) {
	// outer weights 50/50 into inner nodes that each split 80/20.
	inner := func(a, b string) core.StrategyNode {
		return core.StrategyNode{
			Type: "loadbalance",
			Weighted: []core.WeightedTarget{
				{Weight: 80, Node: core.StrategyNode{Type: "single", ProviderID: a, ModelID: "m1"}},
				{Weight: 20, Node: core.StrategyNode{Type: "single", ProviderID: b, ModelID: "m2"}},
			},
		}
	}
	outer := core.StrategyNode{
		Type: "loadbalance",
		Weighted: []core.WeightedTarget{
			{Weight: 1, Node: inner("a1", "a2")},
			{Weight: 1, Node: inner("b1", "b2")},
		},
	}
	branches := EnumerateTerminalTargets(context.Background(), outer, &core.RoutingContext{}, enumLookup)
	if len(branches) != 4 {
		t.Fatalf("expected 4 leaves, got %d (%+v)", len(branches), branches)
	}
	// Each outer branch has 0.5; inner 0.8/0.2.
	a1 := findBranch(branches, "a1", "m1")
	a2 := findBranch(branches, "a2", "m2")
	if math.Abs(a1.Probability-0.4) > 1e-9 || math.Abs(a2.Probability-0.1) > 1e-9 {
		t.Errorf("a-branch probs wrong: %v / %v (want 0.4 / 0.1)", a1.Probability, a2.Probability)
	}
	if math.Abs(sumProb(branches)-1.0) > 1e-9 {
		t.Errorf("nested probs should sum to 1.0, got %v", sumProb(branches))
	}
}

func TestEnumerate_ABSplit_WeightsBecomeProbabilities(t *testing.T) {
	node := core.StrategyNode{
		Type: "ab_split",
		ABTargets: []core.ABTarget{
			{ProviderID: "openai", ModelID: "gpt-4", Weight: 1},
			{ProviderID: "anthropic", ModelID: "claude-3", Weight: 3},
		},
	}
	branches := EnumerateTerminalTargets(context.Background(), node, &core.RoutingContext{}, enumLookup)
	if len(branches) != 2 {
		t.Fatalf("expected 2 branches, got %d", len(branches))
	}
	openai := findBranch(branches, "openai", "gpt-4")
	anthropic := findBranch(branches, "anthropic", "claude-3")
	if math.Abs(openai.Probability-0.25) > 1e-9 || math.Abs(anthropic.Probability-0.75) > 1e-9 {
		t.Errorf("ab_split probs wrong: openai=%v anthropic=%v", openai.Probability, anthropic.Probability)
	}
}

func TestEnumerate_Fallback_AllTargetsReachable(t *testing.T) {
	node := core.StrategyNode{
		Type: "fallback",
		Targets: []core.StrategyNode{
			{Type: "single", ProviderID: "openai", ModelID: "gpt-4"},
			{Type: "single", ProviderID: "anthropic", ModelID: "claude-3"},
		},
	}
	branches := EnumerateTerminalTargets(context.Background(), node, &core.RoutingContext{}, enumLookup)
	if len(branches) != 2 {
		t.Fatalf("expected 2 fallback branches, got %d", len(branches))
	}
	// Fallback: each target independently has a chance on retry; we preserve
	// that by not dividing probability. That is intentional — a 1.0 probability
	// per target means "on retry this will be tried in order". Tests should
	// not treat fallback probabilities as a distribution that sums to 1.0.
	for _, b := range branches {
		if b.Probability != 1.0 {
			t.Errorf("fallback branches should each carry prob=1.0, got %v for %s/%s",
				b.Probability, b.Target.ProviderID, b.Target.ModelID)
		}
	}
}

func TestEnumerate_Conditional_MatchedAndDefault(t *testing.T) {
	node := core.StrategyNode{
		Type: "conditional",
		Conditions: []core.ConditionalBranch{
			{
				When: map[string]any{"requestedModel.type": "embedding"},
				Then: core.StrategyNode{Type: "single", ProviderID: "openai", ModelID: "text-embed"},
			},
			{
				When: map[string]any{"requestedModel.type": "chat"},
				Then: core.StrategyNode{Type: "single", ProviderID: "anthropic", ModelID: "claude-3"},
			},
		},
		Default: &core.StrategyNode{Type: "single", ProviderID: "openai", ModelID: "gpt-4"},
	}
	rctx := &core.RoutingContext{RequestedModel: core.RequestedModel{Type: "chat"}}
	branches := EnumerateTerminalTargets(context.Background(), node, rctx, enumLookup)
	if len(branches) != 3 {
		t.Fatalf("expected 3 enumerated branches (2 conditions + default), got %d (%+v)", len(branches), branches)
	}
	// Only the chat branch should be Matched.
	embed := findBranch(branches, "openai", "text-embed")
	chat := findBranch(branches, "anthropic", "claude-3")
	def := findBranch(branches, "openai", "gpt-4")
	if embed.Matched || !chat.Matched || def.Matched {
		t.Errorf("match flags wrong: embed=%v chat=%v default=%v", embed.Matched, chat.Matched, def.Matched)
	}
	// Probabilities: matched chat = 1.0, unmatched embed = 0, default (fires
	// iff no match) = 0 since chat matched.
	if chat.Probability != 1.0 {
		t.Errorf("matched chat prob = %v, want 1.0", chat.Probability)
	}
	if embed.Probability != 0 || def.Probability != 0 {
		t.Errorf("non-firing probs should be 0; embed=%v default=%v", embed.Probability, def.Probability)
	}
}

// TestEnumerate_Conditional_NoMatch_DefaultFires covers the branch where no
// condition matches — the default carries the full probability.
func TestEnumerate_Conditional_NoMatch_DefaultFires(t *testing.T) {
	node := core.StrategyNode{
		Type: "conditional",
		Conditions: []core.ConditionalBranch{
			{
				When: map[string]any{"requestedModel.type": "image"},
				Then: core.StrategyNode{Type: "single", ProviderID: "openai", ModelID: "dall-e"},
			},
		},
		Default: &core.StrategyNode{Type: "single", ProviderID: "openai", ModelID: "gpt-4"},
	}
	rctx := &core.RoutingContext{RequestedModel: core.RequestedModel{Type: "chat"}}
	branches := EnumerateTerminalTargets(context.Background(), node, rctx, enumLookup)
	def := findBranch(branches, "openai", "gpt-4")
	if !def.Matched || def.Probability != 1.0 {
		t.Errorf("default should be matched=true prob=1.0, got matched=%v prob=%v", def.Matched, def.Probability)
	}
}

// TestEnumerate_LoadbalanceZeroTotalWeight_ReturnsNil pins that a
// misconfigured loadbalance (all zero weights) yields no branches, matching
// the Evaluate soft-fail contract.
func TestEnumerate_LoadbalanceZeroTotalWeight_ReturnsNil(t *testing.T) {
	node := core.StrategyNode{
		Type: "loadbalance",
		Weighted: []core.WeightedTarget{
			{Weight: 0, Node: core.StrategyNode{Type: "single", ProviderID: "x", ModelID: "y"}},
		},
	}
	branches := EnumerateTerminalTargets(context.Background(), node, &core.RoutingContext{}, enumLookup)
	if branches != nil {
		t.Errorf("expected nil branches on zero total weight, got %+v", branches)
	}
}

// TestEnumerate_LookupFailure_ReportsNote pins that when the lookup fails
// (disabled provider, missing model), the branch is still surfaced with a
// Note so the UI can explain the gap.
func TestEnumerate_LookupFailure_ReportsNote(t *testing.T) {
	failing := func(_ context.Context, providerID, modelID string) (*core.RoutingTarget, error) {
		return nil, errStub
	}
	node := core.StrategyNode{Type: "single", ProviderID: "google", ModelID: "gemini"}
	branches := EnumerateTerminalTargets(context.Background(), node, &core.RoutingContext{}, failing)
	if len(branches) != 1 {
		t.Fatalf("expected 1 branch, got %d", len(branches))
	}
	b := branches[0]
	if b.Target.ProviderID != "google" || b.Target.ModelID != "gemini" {
		t.Errorf("branch should preserve ids on lookup failure: %+v", b.Target)
	}
	if !strings.Contains(b.Note, "lookup failed") {
		t.Errorf("note should explain failure, got %q", b.Note)
	}
}

// TestEnumerate_PolicyAndSmart_ProduceNothing pins that policy (stage-0 only)
// and smart (needs full deps + messages) contribute no enumerated terminals.
func TestEnumerate_PolicyAndSmart_ProduceNothing(t *testing.T) {
	policy := core.StrategyNode{Type: "policy", AllowModelIDs: []string{"gpt-4"}}
	if branches := EnumerateTerminalTargets(context.Background(), policy, &core.RoutingContext{}, enumLookup); branches != nil {
		t.Errorf("policy should yield no branches, got %+v", branches)
	}
	smart := core.StrategyNode{Type: "smart"}
	if branches := EnumerateTerminalTargets(context.Background(), smart, &core.RoutingContext{}, enumLookup); branches != nil {
		t.Errorf("smart should yield no branches, got %+v", branches)
	}
}

func TestEnumerate_NilLookup_ReturnsNil(t *testing.T) {
	node := core.StrategyNode{Type: "single", ProviderID: "x", ModelID: "y"}
	if branches := EnumerateTerminalTargets(context.Background(), node, &core.RoutingContext{}, nil); branches != nil {
		t.Errorf("nil lookup must short-circuit to nil, got %+v", branches)
	}
}

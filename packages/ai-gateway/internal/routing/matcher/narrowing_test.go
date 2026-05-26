package matcher

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
)

func TestNarrowing_Empty(t *testing.T) {
	state := EmptyNarrowingState()
	if !IsNarrowingEmpty(state) {
		t.Error("empty state should be empty")
	}
	if !IsTargetAllowed(state, "gpt-4", "openai") {
		t.Error("empty state should allow everything")
	}
}

func TestNarrowing_DenyModel(t *testing.T) {
	state := EmptyNarrowingState()
	state = MergePolicyIntoState(state, core.StrategyNode{
		Type:         "policy",
		DenyModelIDs: []string{"gpt-3.5-turbo"},
	})

	if IsTargetAllowed(state, "gpt-3.5-turbo", "openai") {
		t.Error("denied model should not be allowed")
	}
	if !IsTargetAllowed(state, "gpt-4", "openai") {
		t.Error("other model should be allowed")
	}
}

func TestNarrowing_AllowModelIntersect(t *testing.T) {
	state := EmptyNarrowingState()

	// First policy: allow gpt-4 and claude-3.
	state = MergePolicyIntoState(state, core.StrategyNode{
		Type:          "policy",
		AllowModelIDs: []string{"gpt-4", "claude-3"},
	})
	if !IsTargetAllowed(state, "gpt-4", "openai") {
		t.Error("gpt-4 should be allowed after first policy")
	}
	if !IsTargetAllowed(state, "claude-3", "anthropic") {
		t.Error("claude-3 should be allowed after first policy")
	}
	if IsTargetAllowed(state, "llama-3", "meta") {
		t.Error("llama-3 should not be allowed (not in allow list)")
	}

	// Second policy: allow only gpt-4. Intersection = {gpt-4}.
	state = MergePolicyIntoState(state, core.StrategyNode{
		Type:          "policy",
		AllowModelIDs: []string{"gpt-4"},
	})
	if !IsTargetAllowed(state, "gpt-4", "openai") {
		t.Error("gpt-4 should survive intersection")
	}
	if IsTargetAllowed(state, "claude-3", "anthropic") {
		t.Error("claude-3 should be removed by intersection")
	}
}

func TestNarrowing_DenyProvider(t *testing.T) {
	state := EmptyNarrowingState()
	state = MergePolicyIntoState(state, core.StrategyNode{
		Type:            "policy",
		DenyProviderIDs: []string{"meta"},
	})
	if IsTargetAllowed(state, "llama-3", "meta") {
		t.Error("denied provider should not be allowed")
	}
	if !IsTargetAllowed(state, "gpt-4", "openai") {
		t.Error("other provider should be allowed")
	}
}

func TestNarrowing_AllowProviderIntersect(t *testing.T) {
	state := EmptyNarrowingState()
	state = MergePolicyIntoState(state, core.StrategyNode{
		Type:             "policy",
		AllowProviderIDs: []string{"openai", "anthropic"},
	})
	state = MergePolicyIntoState(state, core.StrategyNode{
		Type:             "policy",
		AllowProviderIDs: []string{"openai"},
	})
	if !IsTargetAllowed(state, "gpt-4", "openai") {
		t.Error("openai should survive intersection")
	}
	if IsTargetAllowed(state, "claude-3", "anthropic") {
		t.Error("anthropic should be removed by intersection")
	}
}

func TestNarrowing_CombinedAllowAndDeny(t *testing.T) {
	state := EmptyNarrowingState()
	state = MergePolicyIntoState(state, core.StrategyNode{
		Type:          "policy",
		AllowModelIDs: []string{"gpt-4", "gpt-3.5-turbo"},
		DenyModelIDs:  []string{"gpt-3.5-turbo"},
	})
	// gpt-3.5-turbo is in both allow and deny → deny wins.
	if IsTargetAllowed(state, "gpt-3.5-turbo", "openai") {
		t.Error("deny should override allow")
	}
	if !IsTargetAllowed(state, "gpt-4", "openai") {
		t.Error("gpt-4 should be allowed")
	}
}

func TestToSummary(t *testing.T) {
	state := EmptyNarrowingState()
	state = MergePolicyIntoState(state, core.StrategyNode{
		Type:          "policy",
		AllowModelIDs: []string{"gpt-4"},
		DenyModelIDs:  []string{"gpt-3.5"},
	})
	summary := ToSummary(state)
	if len(summary.AllowModelIDs) != 1 || summary.AllowModelIDs[0] != "gpt-4" {
		t.Errorf("AllowModelIDs = %v", summary.AllowModelIDs)
	}
	if len(summary.DenyModelIDs) != 1 || summary.DenyModelIDs[0] != "gpt-3.5" {
		t.Errorf("DenyModelIDs = %v", summary.DenyModelIDs)
	}
	if summary.AllowProviderIDs != nil {
		t.Error("nil allow should serialize as nil")
	}
}

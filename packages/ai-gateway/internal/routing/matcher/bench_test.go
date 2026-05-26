package matcher

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
)

func BenchmarkEvaluateExpression_Simple(b *testing.B) {
	ctx := &core.RoutingContext{
		RequestedModel: core.RequestedModel{ID: "gpt-4", Type: "chat", ProviderID: "openai"},
		EndpointType:   "chat",
		VirtualKey:     &core.VKContext{Name: "engineering-openai", ProjectID: "proj-1"},
	}
	expr := map[string]any{
		"requestedModel.type": "chat",
		"endpointType":        "chat",
	}
	for b.Loop() {
		EvaluateExpression(expr, ctx)
	}
}

func BenchmarkEvaluateExpression_Complex(b *testing.B) {
	ctx := &core.RoutingContext{
		RequestedModel: core.RequestedModel{ID: "gpt-4", Type: "chat", ProviderID: "openai"},
		EndpointType:   "chat",
		VirtualKey:     &core.VKContext{Name: "engineering-openai", ProjectID: "proj-1"},
	}
	expr := map[string]any{
		"$and": []any{
			map[string]any{"requestedModel.type": map[string]any{"$in": []any{"chat", "embedding"}}},
			map[string]any{"virtualKey.name": map[string]any{"$regex": "^engineering-.*"}},
			map[string]any{
				"$or": []any{
					map[string]any{"endpointType": "chat"},
					map[string]any{"endpointType": "embeddings"},
				},
			},
		},
	}
	for b.Loop() {
		EvaluateExpression(expr, ctx)
	}
}

func BenchmarkMatchGlob(b *testing.B) {
	for b.Loop() {
		MatchGlob("gpt-*", "gpt-4-turbo-2024")
	}
}

func BenchmarkNarrowing_MergePolicy(b *testing.B) {
	policy := core.StrategyNode{
		Type:             "policy",
		AllowModelIDs:    []string{"gpt-4", "claude-3", "gemini-pro"},
		DenyModelIDs:     []string{"gpt-3.5-turbo"},
		AllowProviderIDs: []string{"openai", "anthropic"},
	}
	for b.Loop() {
		state := EmptyNarrowingState()
		MergePolicyIntoState(state, policy)
	}
}

func BenchmarkRuleMatchesContext(b *testing.B) {
	conds := &core.MatchConditions{
		Models:      []string{"gpt-4", "claude-3"},
		ModelTypes:  []string{"chat"},
		VirtualKeys: []string{"engineering-*"},
	}
	ctx := &core.RoutingContext{
		RequestedModel: core.RequestedModel{ID: "gpt-4", Type: "chat", ProviderID: "openai"},
		VirtualKey:     &core.VKContext{Name: "engineering-openai"},
	}
	for b.Loop() {
		RuleMatchesContext(conds, "gpt-4", ctx)
	}
}

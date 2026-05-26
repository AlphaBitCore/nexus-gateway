package strategies

import (
	"context"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
)

func BenchmarkStrategyEvaluate_Single(b *testing.B) {
	reg := NewStrategyRegistry()
	lookup := func(_ context.Context, providerID, modelID string) (*core.RoutingTarget, error) {
		return &core.RoutingTarget{ProviderID: providerID, ModelID: modelID, ProviderName: providerID}, nil
	}
	RegisterAllStrategies(reg, lookup, nil)

	node := core.StrategyNode{Type: "single", ProviderID: "openai", ModelID: "gpt-4"}
	ctx := &core.RoutingContext{}

	for b.Loop() {
		var trace []core.TraceEntry
		_, _ = reg.Evaluate(context.Background(), node, ctx, &trace, 0)
	}
}

func BenchmarkStrategyEvaluate_Fallback(b *testing.B) {
	reg := NewStrategyRegistry()
	lookup := func(_ context.Context, providerID, modelID string) (*core.RoutingTarget, error) {
		return &core.RoutingTarget{ProviderID: providerID, ModelID: modelID, ProviderName: providerID}, nil
	}
	RegisterAllStrategies(reg, lookup, nil)

	node := core.StrategyNode{
		Type: "fallback",
		Targets: []core.StrategyNode{
			{Type: "single", ProviderID: "openai", ModelID: "gpt-4"},
			{Type: "single", ProviderID: "anthropic", ModelID: "claude-3"},
			{Type: "single", ProviderID: "google", ModelID: "gemini-pro"},
		},
	}
	ctx := &core.RoutingContext{}

	for b.Loop() {
		var trace []core.TraceEntry
		_, _ = reg.Evaluate(context.Background(), node, ctx, &trace, 0)
	}
}

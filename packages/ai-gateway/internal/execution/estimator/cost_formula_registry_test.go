// Package estimator_test — cost_formula_registry_test.go covers the
// per-endpoint cost formula dispatch table keyed by canonical
// typology.EndpointKind strings.
//
// Named failure modes tested:
//   - "chat" → chatCostFormula → uses both prompt + completion tokens
//   - "embeddings" → embeddingsCostFormula → prompt-only, completion ignored
//   - unknown endpoint → safe default (chatCostFormula)
//   - RegisterFormula  → custom formula replaces built-in for that endpoint
//   - BillableUnits zero → cost zero
//   - EmbeddingsCostFormula: 1000 tokens × $0.02/M = $0.00002
package estimator_test

import (
	"math"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/estimator"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/metrics"
)

func ptr64(f float64) *float64 { return &f }

// TestLookup_knownEndpoints verifies that built-in endpoints resolve to the
// correct formula and produce expected cost totals.
func TestLookup_knownEndpoints(t *testing.T) {
	chatPrices := metrics.ModelPrices{
		InputUsdPerM:  ptr64(2.5),  // $2.50 / 1M input
		OutputUsdPerM: ptr64(10.0), // $10.00 / 1M output
	}
	embPrices := metrics.ModelPrices{
		InputUsdPerM:  ptr64(0.02), // $0.02 / 1M input
		OutputUsdPerM: ptr64(0.0),  // embeddings have no output price
	}

	cases := []struct {
		endpoint         string
		units            estimator.BillableUnits
		prices           metrics.ModelPrices
		wantTotalApprox  float64
		desc             string
	}{
		{
			endpoint: "chat",
			units:    estimator.BillableUnits{PromptTokens: 1000, CompletionTokens: 500},
			prices:   chatPrices,
			// 1000*2.5/1e6 + 500*10/1e6 = 0.0025 + 0.005 = 0.0075
			wantTotalApprox: 0.0075,
			desc:            "chat: prompt + completion",
		},
		{
			endpoint: "embeddings",
			units:    estimator.BillableUnits{PromptTokens: 1000, CompletionTokens: 99}, // completion must be ignored
			prices:   embPrices,
			// 1000*0.02/1e6 = 0.00002
			wantTotalApprox: 0.00002,
			desc:            "embeddings: prompt-only; completion tokens ignored",
		},
		{
			endpoint: "embeddings",
			units:    estimator.BillableUnits{PromptTokens: 0},
			prices:   embPrices,
			wantTotalApprox: 0.0,
			desc:            "embeddings zero tokens → zero cost",
		},
		{
			endpoint: "unknown-future-endpoint",
			units:    estimator.BillableUnits{PromptTokens: 1000, CompletionTokens: 500},
			prices:   chatPrices,
			// Safe default: falls back to chat formula
			wantTotalApprox: 0.0075,
			desc:            "unknown endpoint falls back to chat formula",
		},
	}

	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			formula := estimator.Lookup(tc.endpoint)
			if formula == nil {
				t.Fatal("Lookup returned nil formula")
			}
			cost := formula(tc.units, tc.prices)
			if math.Abs(cost.Total-tc.wantTotalApprox) > 1e-10 {
				t.Errorf("cost.Total = %.10f, want %.10f", cost.Total, tc.wantTotalApprox)
			}
		})
	}
}

// TestEmbeddingsCostFormula_completionIgnored verifies that completion tokens
// do not contribute to the embeddings cost formula (SDD T3.5: embeddings
// populate only PromptTokens; completion tokens must be zero from codec).
func TestEmbeddingsCostFormula_completionIgnored(t *testing.T) {
	prices := metrics.ModelPrices{
		InputUsdPerM:  ptr64(0.13), // text-embedding-3-small pricing
		OutputUsdPerM: ptr64(100.0), // very high — must not appear in result
	}
	units := estimator.BillableUnits{PromptTokens: 1000, CompletionTokens: 9999}
	cost := estimator.Lookup("embeddings")(units, prices)
	// Should only be 1000 * 0.13 / 1e6 = 0.00013
	want := 0.00013
	if math.Abs(cost.Total-want) > 1e-10 {
		t.Errorf("embeddings cost = %.10f, want %.10f (completion tokens leaked into formula)", cost.Total, want)
	}
}

// TestRegisterFormula_overridesBuiltin verifies that RegisterFormula lets
// future epics inject a custom formula without modifying the dispatcher.
func TestRegisterFormula_overridesBuiltin(t *testing.T) {
	const testEndpoint = "_test_custom_endpoint_e62"
	called := false
	custom := func(u estimator.BillableUnits, p metrics.ModelPrices) metrics.Cost {
		called = true
		return metrics.Cost{Total: 42.0}
	}
	estimator.RegisterFormula(testEndpoint, custom)
	// No cleanup needed: test endpoint key is private to this test
	// and will not conflict with other tests.

	formula := estimator.Lookup(testEndpoint)
	cost := formula(estimator.BillableUnits{}, metrics.ModelPrices{})
	if !called {
		t.Error("registered custom formula was not called")
	}
	if cost.Total != 42.0 {
		t.Errorf("custom formula result = %.1f, want 42.0", cost.Total)
	}
}

// TestCostForUnits_cacheAware verifies the full-accounting costForUnits
// path that threads CachedTokens through metrics.CalculateCost.
// This function is the hook point for future cache-cost extensions that need
// cache-aware cost calculation from BillableUnits.
func TestCostForUnits_cacheAware(t *testing.T) {
	inPM := 2.5   // $2.50 / 1M input
	outPM := 10.0 // $10.00 / 1M output
	cacheReadPM := 1.25 // 50% discount
	prices := metrics.ModelPrices{
		InputUsdPerM:           &inPM,
		OutputUsdPerM:          &outPM,
		CachedInputReadUsdPerM: &cacheReadPM,
	}
	units := estimator.BillableUnits{
		PromptTokens:     1000, // total prompt
		CachedTokens:     200,  // subset served from cache
		CompletionTokens: 100,
	}
	cost := estimator.CostForUnitsExported(units, prices)
	// uncached input = 1000 - 200 = 800 tokens at $2.50/M = 0.002
	// cache read     = 200 tokens at $1.25/M = 0.00025
	// completion     = 100 tokens at $10.00/M = 0.001
	// total = 0.00325
	want := 0.00325
	if math.Abs(cost.Total-want) > 1e-9 {
		t.Errorf("costForUnits total = %.10f, want %.10f", cost.Total, want)
	}
}

// TestLookup_chatCostMatchesEstimatedCostUSD validates that the chat
// formula produces the same result as the previous estimatedCostUSD
// helper so no regression in existing chat cost stamping.
func TestLookup_chatCostMatchesEstimatedCostUSD(t *testing.T) {
	inPM := 2.5
	outPM := 10.0
	promptTok := int64(1300)
	completionTok := int64(700)

	// Old formula: float64(p)*inPM/1e6 + float64(c)*outPM/1e6
	want := float64(promptTok)*inPM/1_000_000 + float64(completionTok)*outPM/1_000_000

	units := estimator.BillableUnits{
		PromptTokens:     int(promptTok),
		CompletionTokens: int(completionTok),
	}
	cost := estimator.Lookup("chat")(units, metrics.ModelPrices{
		InputUsdPerM:  &inPM,
		OutputUsdPerM: &outPM,
	})
	if math.Abs(cost.Total-want) > 1e-10 {
		t.Errorf("chat formula = %.10f, want %.10f (regression vs estimatedCostUSD)", cost.Total, want)
	}
}

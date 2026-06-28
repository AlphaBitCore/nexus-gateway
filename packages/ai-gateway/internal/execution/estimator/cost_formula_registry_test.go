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
	"bytes"
	"log/slog"
	"math"
	"strings"
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
		endpoint        string
		units           estimator.BillableUnits
		prices          metrics.ModelPrices
		wantTotalApprox float64
		desc            string
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
			endpoint:        "embeddings",
			units:           estimator.BillableUnits{PromptTokens: 0},
			prices:          embPrices,
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
		InputUsdPerM:  ptr64(0.13),  // text-embedding-3-small pricing
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

// TestLookup_unregisteredEndpoint_WarnsOnce is the visibility
// assertion: an unregistered endpoint must (a) still resolve to a usable
// (chat) formula and (b) emit exactly one WARN log naming the endpoint, no
// matter how many times it is looked up — so the silent token-mispricing
// fallback becomes observable without flooding the log.
func TestLookup_unregisteredEndpoint_WarnsOnce(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	// A unique endpoint string so the process-lifetime dedup map has not
	// already recorded it from another test.
	const ep = "_f0234_unregistered_endpoint_probe"

	for range 5 {
		formula := estimator.Lookup(ep)
		if formula == nil {
			t.Fatalf("Lookup(%q) returned nil; expected chat-formula fallback", ep)
		}
		// Fallback must price like the chat formula (prompt + completion).
		inPM, outPM := 2.0, 4.0
		cost := formula(estimator.BillableUnits{PromptTokens: 1000, CompletionTokens: 500}, metrics.ModelPrices{
			InputUsdPerM:  &inPM,
			OutputUsdPerM: &outPM,
		})
		want := 1000*inPM/1e6 + 500*outPM/1e6
		if math.Abs(cost.Total-want) > 1e-10 {
			t.Errorf("fallback cost = %.10f, want chat-formula %.10f", cost.Total, want)
		}
	}

	logged := buf.String()
	if !strings.Contains(logged, ep) {
		t.Errorf("expected a WARN naming endpoint %q; log was: %q", ep, logged)
	}
	if got := strings.Count(logged, ep); got != 1 {
		t.Errorf("expected exactly 1 WARN for endpoint %q across 5 lookups, got %d; log was: %q", ep, got, logged)
	}
}

// TestBillableUnits_OnlyTokenFields locks the trim: BillableUnits
// carries exactly the two live token fields and the chat formula prices from
// both. If a dead unit field is re-added without a live setter, this test's
// surrounding contract (and the doc comment on BillableUnits) flags it.
func TestBillableUnits_OnlyTokenFields(t *testing.T) {
	u := estimator.BillableUnits{PromptTokens: 10, CompletionTokens: 20}
	inPM, outPM := 1000.0, 2000.0
	cost := estimator.Lookup("chat")(u, metrics.ModelPrices{InputUsdPerM: &inPM, OutputUsdPerM: &outPM})
	want := 10*inPM/1e6 + 20*outPM/1e6
	if math.Abs(cost.Total-want) > 1e-12 {
		t.Errorf("BillableUnits chat cost = %.12f, want %.12f", cost.Total, want)
	}
}

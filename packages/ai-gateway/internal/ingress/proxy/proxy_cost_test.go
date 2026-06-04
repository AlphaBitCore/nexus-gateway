package proxy

import (
	"math"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
)

// TestEstimatedCostUSD pins the per-request USD cost calculation. The Model
// row stores prices as "USD per million tokens" (Model.inputPricePerMillion
// / outputPricePerMillion); the helper just unscales by 1e6 and sums.
//
// Regression guard: rec.EstimatedCostUsd was previously never populated
// (NULL on every traffic_event row) even though the prices and tokens were
// both available — analytics surfaces showed Cost as `-` everywhere.
func TestEstimatedCostUSD(t *testing.T) {
	cases := []struct {
		name      string
		promptTok int64
		complTok  int64
		inPM      float64
		outPM     float64
		want      float64
	}{
		{
			name:      "gpt-4o pricing",
			promptTok: 1000, complTok: 500,
			inPM: 2.5, outPM: 10.0,
			// 1000 * 2.5 / 1e6 + 500 * 10 / 1e6 = 0.0025 + 0.005 = 0.0075
			want: 0.0075,
		},
		{
			name:      "claude-opus-4-7 pricing",
			promptTok: 13, complTok: 10,
			inPM: 15.0, outPM: 75.0,
			// 13 * 15 / 1e6 + 10 * 75 / 1e6 = 0.000195 + 0.00075 = 0.000945
			want: 0.000945,
		},
		{
			name:      "zero tokens",
			promptTok: 0, complTok: 0,
			inPM: 2.5, outPM: 10.0,
			want: 0,
		},
		{
			name:      "zero prices yield zero (model has no pricing yet)",
			promptTok: 1000, complTok: 500,
			inPM: 0, outPM: 0,
			want: 0,
		},
		{
			name:      "only completion priced (e.g. embeddings only have input)",
			promptTok: 1000, complTok: 0,
			inPM: 0.13, outPM: 0,
			want: 0.00013,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := estimatedCostUSD(tc.promptTok, tc.complTok, tc.inPM, tc.outPM)
			if math.Abs(got-tc.want) > 1e-12 {
				t.Errorf("estimatedCostUSD(%d, %d, %v, %v) = %v, want %v",
					tc.promptTok, tc.complTok, tc.inPM, tc.outPM, got, tc.want)
			}
		})
	}
}

// staticCachePricing is a test double for CachePricingLookup that returns a
// fixed ProviderPricing regardless of adapter/provider/model.
type staticCachePricing struct{ p *store.ProviderPricing }

func (s *staticCachePricing) LookupCachePricing(_, _, _ string) *store.ProviderPricing {
	return s.p
}

// TestComputeCacheCosts verifies that EstimatedCostUsd is computed from scratch
// using provider_pricing consistently, so a price mismatch between the models
// table (quotaInPrice) and provider_pricing (p.InputUSDPerM) can never produce
// a negative cost.
//
// The normalizer sums uncached + cache_read + cache_creation into
// PromptTokens at codec time (see anthropic_messages.go), so PromptTokens
// is consistently "total input including cached" across every adapter.
// Test mocks reflect that; the function no longer branches on AdapterType.
// The regression test below pins the claude-opus-4-1 case that surfaced
// the original double-count bug.
func TestComputeCacheCosts(t *testing.T) {
	pricing := &store.ProviderPricing{
		InputUSDPerM:      0.25,
		OutputUSDPerM:     1.25,
		CacheReadUSDPerM:  0.03,
		CacheWriteUSDPerM: 0.30,
	}

	cases := []struct {
		name                string
		adapterType         string
		promptTokens        int64
		completionTokens    int64
		cacheReadTokens     int64
		cacheCreationTokens int64
		// Pre-set EstimatedCostUsd (as computed by estimatedCostUSD with a
		// potentially different quotaInPrice — this should be fully replaced).
		startCost       float64
		wantCost        float64
		wantReadSavings float64
		wantWriteCost   float64
		wantNetSavings  float64
		// Optional per-case pricing override. nil = use the shared
		// `pricing` defined above. Set this on cases that need to
		// reproduce production pricing (e.g., claude-opus-4 regression).
		pricingOverride *store.ProviderPricing
	}{
		{
			name:         "openai: prompt total = uncached + cache_read",
			adapterType:  "openai",
			promptTokens: 4476, completionTokens: 64, cacheReadTokens: 4352,
			// uncached = 4476 - 4352 = 124
			startCost:       estimatedCostUSD(4476, 64, 0.15, 1.25),
			wantCost:        (124*0.25 + 4352*0.03 + 64*1.25) / 1e6,
			wantReadSavings: 4352 * (0.25 - 0.03) / 1e6,
			wantWriteCost:   0,
			wantNetSavings:  4352 * (0.25 - 0.03) / 1e6,
		},
		{
			name:         "anthropic: prompt total = uncached + cache_read (normalizer sums)",
			adapterType:  "anthropic",
			promptTokens: 4476, completionTokens: 64, cacheReadTokens: 4352,
			// uncached = 4476 - 4352 = 124 (same math as openai now that
			// AdapterType branch is gone — single formula).
			startCost:       estimatedCostUSD(4476, 64, 0.25, 1.25),
			wantCost:        (124*0.25 + 4352*0.03 + 64*1.25) / 1e6,
			wantReadSavings: 4352 * (0.25 - 0.03) / 1e6,
			wantWriteCost:   0,
			wantNetSavings:  4352 * (0.25 - 0.03) / 1e6,
		},
		{
			name:         "anthropic: cache write cost (prompt = uncached + cache_creation)",
			adapterType:  "anthropic",
			promptTokens: 2500, completionTokens: 50, cacheReadTokens: 0, cacheCreationTokens: 2000,
			// uncached = 2500 - 0 - 2000 = 500
			startCost:       estimatedCostUSD(2500, 50, 0.25, 1.25),
			wantCost:        (500*0.25 + 2000*0.30 + 50*1.25) / 1e6,
			wantReadSavings: 0,
			wantWriteCost:   2000 * 0.30 / 1e6,
			wantNetSavings:  -(2000 * 0.30 / 1e6),
		},
		{
			name:         "openai: both read and write",
			adapterType:  "openai",
			promptTokens: 5000, completionTokens: 100, cacheReadTokens: 3000, cacheCreationTokens: 0,
			startCost:       estimatedCostUSD(5000, 100, 0.25, 1.25),
			wantCost:        (2000*0.25 + 3000*0.03 + 0*0.30 + 100*1.25) / 1e6,
			wantReadSavings: 3000 * (0.25 - 0.03) / 1e6,
			wantWriteCost:   0,
			wantNetSavings:  3000 * (0.25 - 0.03) / 1e6,
		},
		{
			// Regression for the Anthropic double-count bug:
			// claude-opus-4-1 provider_pricing (input=$15, output=$75,
			// cache_read=$1.5, cache_write=$18.75). Tokens:
			// prompt=10082 (= uncached 908 + cache_read 8823 + cache_write 351),
			// completion=1024. Pre-fix cost was $0.247846 (2.25× over).
			// Expected: 908×$15 + 8823×$1.5 + 351×$18.75 + 1024×$75 = $0.110235/M.
			name:                "anthropic: claude-opus-4 regression (no double-count)",
			adapterType:         "anthropic",
			promptTokens:        10082,
			completionTokens:    1024,
			cacheReadTokens:     8823,
			cacheCreationTokens: 351,
			startCost:           0.247846,
			wantCost:            (908*15.0 + 8823*1.5 + 351*18.75 + 1024*75.0) / 1e6,
			wantReadSavings:     8823 * (15.0 - 1.5) / 1e6,
			wantWriteCost:       351 * 18.75 / 1e6,
			wantNetSavings:      8823*(15.0-1.5)/1e6 - 351*18.75/1e6,
			pricingOverride: &store.ProviderPricing{
				InputUSDPerM:      15.0,
				OutputUSDPerM:     75.0,
				CacheReadUSDPerM:  1.5,
				CacheWriteUSDPerM: 18.75,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			effectivePricing := pricing
			if tc.pricingOverride != nil {
				effectivePricing = tc.pricingOverride
			}
			h := &Handler{
				deps: &Deps{
					CachePricing: &staticCachePricing{p: effectivePricing},
				},
			}
			rec := &audit.Record{
				PromptTokens:        tc.promptTokens,
				CompletionTokens:    tc.completionTokens,
				CacheReadTokens:     tc.cacheReadTokens,
				CacheCreationTokens: tc.cacheCreationTokens,
				EstimatedCostUsd:    tc.startCost,
			}
			target := routingcore.RoutingTarget{AdapterType: tc.adapterType}
			h.computeCacheCosts(rec, target)

			const eps = 1e-10
			if math.Abs(rec.EstimatedCostUsd-tc.wantCost) > eps {
				t.Errorf("EstimatedCostUsd = %v, want %v", rec.EstimatedCostUsd, tc.wantCost)
			}
			if rec.EstimatedCostUsd < 0 {
				t.Errorf("EstimatedCostUsd must not be negative, got %v", rec.EstimatedCostUsd)
			}
			if math.Abs(rec.CacheReadSavingsUsd-tc.wantReadSavings) > eps {
				t.Errorf("CacheReadSavingsUsd = %v, want %v", rec.CacheReadSavingsUsd, tc.wantReadSavings)
			}
			if math.Abs(rec.CacheWriteCostUsd-tc.wantWriteCost) > eps {
				t.Errorf("CacheWriteCostUsd = %v, want %v", rec.CacheWriteCostUsd, tc.wantWriteCost)
			}
			if math.Abs(rec.CacheNetSavingsUsd-tc.wantNetSavings) > eps {
				t.Errorf("CacheNetSavingsUsd = %v, want %v", rec.CacheNetSavingsUsd, tc.wantNetSavings)
			}
		})
	}
}

package costing

import (
	"math"
	"testing"
)

func ptr(v float64) *float64 { return &v }

// TestRatesFromModel_NullColumnSemantics pins the single interpretation of a
// NULL price column that the customer request path and the internal-operations
// callers now share. Two different answers here would price the same model two
// ways depending on who called it.
func TestRatesFromModel_NullColumnSemantics(t *testing.T) {
	tests := []struct {
		name                            string
		input, output, cRead, cWrite    *float64
		wantPriced                      bool
		wantIn, wantOut, wantCR, wantCW float64
		why                             string
	}{
		{
			name:  "all four set — passed through verbatim",
			input: ptr(2.50), output: ptr(10.00), cRead: ptr(0.625), cWrite: ptr(3.125),
			wantPriced: true,
			wantIn:     2.50, wantOut: 10.00, wantCR: 0.625, wantCW: 3.125,
			why: "a fully-priced row must not be reinterpreted",
		},
		{
			name:  "NULL cache columns fall back to the input rate",
			input: ptr(2.50), output: ptr(10.00),
			wantPriced: true,
			wantIn:     2.50, wantOut: 10.00, wantCR: 2.50, wantCW: 2.50,
			why: "NULL cache price means no discount and no surcharge, not free",
		},
		{
			name:  "NULL output is zero, not the input rate",
			input: ptr(2.50), cRead: ptr(0.625), cWrite: ptr(3.125),
			wantPriced: true,
			wantIn:     2.50, wantOut: 0, wantCR: 0.625, wantCW: 3.125,
			why: "output has no meaningful fallback; billing it at the input rate would invent a charge",
		},
		{
			name:       "NULL input is unpriced, not free",
			output:     ptr(10.00),
			wantPriced: false,
			why:        "an unpriced model must be distinguishable from one priced at zero",
		},
		{
			name:  "explicit zero input is priced and genuinely free",
			input: ptr(0), output: ptr(0),
			wantPriced: true,
			why:        "a zero-priced model is a statement, not an absence",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, priced := RatesFromModel(tt.input, tt.output, tt.cRead, tt.cWrite)
			if priced != tt.wantPriced {
				t.Fatalf("priced = %v, want %v — %s", priced, tt.wantPriced, tt.why)
			}
			if !tt.wantPriced {
				if got != (Rates{}) {
					t.Errorf("rates = %+v, want the zero value on an unpriced model", got)
				}
				return
			}
			want := Rates{
				InputUSDPerM: tt.wantIn, OutputUSDPerM: tt.wantOut,
				CacheReadUSDPerM: tt.wantCR, CacheWriteUSDPerM: tt.wantCW,
			}
			if got != want {
				t.Errorf("rates = %+v, want %+v — %s", got, want, tt.why)
			}
		})
	}
}

// TestRates_Priced separates "no price configured" from "priced at zero". The
// callers use it to decide whether a zero cost is a claim or an absence, so
// collapsing the two would make a free model look like a catalog gap and
// trigger the unpriced-model warning on every call.
func TestRates_Priced(t *testing.T) {
	if (Rates{}).Priced() {
		t.Error("zero rates must report unpriced")
	}
	if !(Rates{InputUSDPerM: 2.50}).Priced() {
		t.Error("an input rate alone must report priced")
	}
	if !(Rates{OutputUSDPerM: 10.00}).Priced() {
		t.Error("an output-only rate must report priced — output-billed models exist")
	}
	if !(Rates{CacheReadUSDPerM: 0.625}).Priced() {
		t.Error("a cache-read rate alone must report priced")
	}
}

// TestEstimateUSD_CachedShareBilledAtCacheRate is the core regression: prompt
// tokens are the TOTAL input, so the cached share must be subtracted from the
// uncached remainder and billed at its own rate. Billing the full prompt at the
// input rate — what the router and the AI Guard classifier used to do — charges
// the cached tokens twice over at the wrong price.
func TestEstimateUSD_CachedShareBilledAtCacheRate(t *testing.T) {
	r := Rates{InputUSDPerM: 2.50, OutputUSDPerM: 10.00, CacheReadUSDPerM: 0.625, CacheWriteUSDPerM: 3.125}
	got := r.EstimateUSD(Tokens{Prompt: 10_000, Completion: 100, CacheRead: 8_000, CacheCreation: 1_000})

	// 1000 uncached × 2.50 + 8000 read × 0.625 + 1000 write × 3.125 + 100 out × 10.
	want := (1_000*2.50 + 8_000*0.625 + 1_000*3.125 + 100*10.00) / 1_000_000.0
	if math.Abs(got-want) > 1e-12 {
		t.Fatalf("EstimateUSD = %v, want %v", got, want)
	}

	flat := r.EstimateUSD(Tokens{Prompt: 10_000, Completion: 100})
	if got >= flat {
		t.Errorf("a cache-bearing call (%v) must cost less than the same tokens uncached (%v)", got, flat)
	}
}

// TestEstimateUSD_NoCacheTokensMatchesFlatRate pins that the four-tier formula
// is a strict generalisation: with no cache buckets it must produce exactly the
// two-tier result, so moving every caller onto it changes no uncached bill.
func TestEstimateUSD_NoCacheTokensMatchesFlatRate(t *testing.T) {
	r := Rates{InputUSDPerM: 2.50, OutputUSDPerM: 10.00, CacheReadUSDPerM: 0.625, CacheWriteUSDPerM: 3.125}
	got := r.EstimateUSD(Tokens{Prompt: 2_000, Completion: 50})
	want := (2_000*2.50 + 50*10.00) / 1_000_000.0
	if math.Abs(got-want) > 1e-12 {
		t.Errorf("EstimateUSD = %v, want the flat-rate %v", got, want)
	}
}

// TestEstimateUSD_CacheBucketsExceedingPromptDoNotGoNegative pins the named
// failure mode "provider reports cache counts larger than its own prompt
// count". The remainder floors at zero: a negative uncached count would credit
// the account, which is how negative cost values reached the traffic rows when
// two price sources disagreed historically.
func TestEstimateUSD_CacheBucketsExceedingPromptDoNotGoNegative(t *testing.T) {
	r := Rates{InputUSDPerM: 2.50, OutputUSDPerM: 10.00, CacheReadUSDPerM: 0.625, CacheWriteUSDPerM: 3.125}
	got := r.EstimateUSD(Tokens{Prompt: 100, Completion: 10, CacheRead: 500, CacheCreation: 200})

	// The cache buckets are still billed; only the uncached remainder floors.
	want := (500*0.625 + 200*3.125 + 10*10.00) / 1_000_000.0
	if math.Abs(got-want) > 1e-12 {
		t.Fatalf("EstimateUSD = %v, want %v", got, want)
	}
	if got < 0 {
		t.Errorf("EstimateUSD = %v, must never be negative", got)
	}
}

// TestEstimateUSD_ZeroTokensCostNothing pins the cache-hit / early-failure
// shape: a call that consumed no tokens is billed zero, not left to inherit a
// stale amount.
func TestEstimateUSD_ZeroTokensCostNothing(t *testing.T) {
	r := Rates{InputUSDPerM: 2.50, OutputUSDPerM: 10.00, CacheReadUSDPerM: 0.625, CacheWriteUSDPerM: 3.125}
	if got := r.EstimateUSD(Tokens{}); got != 0 {
		t.Errorf("EstimateUSD = %v, want 0 for a call that consumed nothing", got)
	}
}

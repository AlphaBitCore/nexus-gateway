package cachelayer

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
)

// The provider_pricing regex-index seam was retired in favor of reading
// prices directly from the Models snapshot. The old tests against
// pricingIndex/loadProviderPricing/regex-precedence are gone; these
// tests pin the new contract.

func newLayerWithModels(models map[string]store.Model) *Layer {
	l := &Layer{}
	l.modelsByCode.Store(&models)
	return l
}

// TestLookupCachePricing_NilSnapshot returns nil before any models load.
func TestLookupCachePricing_NilSnapshot(t *testing.T) {
	l := &Layer{}
	if got := l.LookupCachePricing("gpt-4o"); got != nil {
		t.Errorf("nil snapshot must return nil; got %+v", got)
	}
}

// TestLookupCachePricing_ModelMissing returns nil for unknown code.
func TestLookupCachePricing_ModelMissing(t *testing.T) {
	l := newLayerWithModels(map[string]store.Model{
		"gpt-4o": {Code: "gpt-4o", InputPricePM: f64ptr(2.5)},
	})
	if got := l.LookupCachePricing("claude-opus"); got != nil {
		t.Errorf("missing code must return nil; got %+v", got)
	}
}

// TestLookupCachePricing_InputPriceMissing returns nil when the model
// has no price configured — caller treats cache cost as zero.
func TestLookupCachePricing_InputPriceMissing(t *testing.T) {
	l := newLayerWithModels(map[string]store.Model{
		"x": {Code: "x"},
	})
	if got := l.LookupCachePricing("x"); got != nil {
		t.Errorf("nil InputPricePM must return nil; got %+v", got)
	}
}

// TestLookupCachePricing_AllPricesPresent populates every field.
func TestLookupCachePricing_AllPricesPresent(t *testing.T) {
	l := newLayerWithModels(map[string]store.Model{
		"claude-opus-4-1": {
			Code:                    "claude-opus-4-1",
			InputPricePM:            f64ptr(15.0),
			OutputPricePM:           f64ptr(75.0),
			CachedInputReadPricePM:  f64ptr(1.5),
			CachedInputWritePricePM: f64ptr(18.75),
		},
	})
	got := l.LookupCachePricing("claude-opus-4-1")
	if got == nil {
		t.Fatal("want non-nil for fully-priced model")
	}
	if got.InputUSDPerM != 15.0 || got.OutputUSDPerM != 75.0 ||
		got.CacheReadUSDPerM != 1.5 || got.CacheWriteUSDPerM != 18.75 {
		t.Errorf("price translation wrong: %+v", got)
	}
}

// TestLookupCachePricing_NullCacheFallsBackToInput pins the contract
// that NULL cache prices on the Model row fall back to InputPricePM so
// the cost formula degrades to "flat input rate, no caching effect"
// instead of zero-billing a model the operator hasn't fully configured.
func TestLookupCachePricing_NullCacheFallsBackToInput(t *testing.T) {
	l := newLayerWithModels(map[string]store.Model{
		"moonshot-v1-8k": {
			Code:          "moonshot-v1-8k",
			InputPricePM:  f64ptr(0.12),
			OutputPricePM: f64ptr(0.12),
			// CachedInputReadPricePM / CachedInputWritePricePM: nil
		},
	})
	got := l.LookupCachePricing("moonshot-v1-8k")
	if got == nil {
		t.Fatal("expected non-nil even with NULL cache prices")
	}
	if got.CacheReadUSDPerM != 0.12 || got.CacheWriteUSDPerM != 0.12 {
		t.Errorf("NULL cache should fall back to input price; got read=%v write=%v",
			got.CacheReadUSDPerM, got.CacheWriteUSDPerM)
	}
}

func f64ptr(v float64) *float64 { return &v }

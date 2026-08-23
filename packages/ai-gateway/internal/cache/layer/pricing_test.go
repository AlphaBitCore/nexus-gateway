package cachelayer

import (
	"context"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/configcache"
)

// The provider_pricing regex-index seam was retired in favor of reading
// prices directly from the Models snapshot. The old tests against
// pricingIndex/loadProviderPricing/regex-precedence are gone; these
// tests pin the new contract.

// newLayerWithModels seeds the by-UUID Model snapshot, the index pricing reads.
// Keys are Model.id, not code — pricing must resolve a model the code index has
// dropped because it stopped being servable.
func newLayerWithModels(t *testing.T, models map[string]store.Model) *Layer {
	t.Helper()
	l := &Layer{}
	l.models = configcache.NewSnapshotCache(func(context.Context) (map[string]store.Model, error) {
		return models, nil
	})
	if err := l.models.Load(context.Background()); err != nil {
		t.Fatalf("seed model snapshot: %v", err)
	}
	return l
}

// TestLookupCachePricing_EmptySnapshot returns nil before any models load.
func TestLookupCachePricing_EmptySnapshot(t *testing.T) {
	l := newLayerWithModels(t, nil)
	if got := l.LookupCachePricing("m-gpt-4o"); got != nil {
		t.Errorf("empty snapshot must return nil; got %+v", got)
	}
}

// TestLookupCachePricing_UnservableModelStillPriced is the contract that keeps
// cost stamping correct across an operator action: a response that finishes
// after its provider was disabled must still get its cache decomposition, so
// pricing reads the by-UUID snapshot rather than the servable code index.
func TestLookupCachePricing_UnservableModelStillPriced(t *testing.T) {
	l := newLayerWithModels(t, map[string]store.Model{
		"m-dead": {
			ID: "m-dead", Code: "embed-english-v3.0",
			Enabled: true, ProviderEnabled: false, Status: "active",
			InputPricePM: f64ptr(1.0), CachedInputReadPricePM: f64ptr(0.1),
		},
	})
	got := l.LookupCachePricing("m-dead")
	if got == nil {
		t.Fatal("a model whose provider was disabled mid-flight must still price")
	}
	if got.CacheReadUSDPerM != 0.1 {
		t.Errorf("cache read rate = %v, want 0.1", got.CacheReadUSDPerM)
	}
}

// TestLookupCachePricing_ModelMissing returns nil for an unknown model id.
func TestLookupCachePricing_ModelMissing(t *testing.T) {
	l := newLayerWithModels(t, map[string]store.Model{
		"m-gpt-4o": {ID: "m-gpt-4o", Code: "gpt-4o", InputPricePM: f64ptr(2.5)},
	})
	if got := l.LookupCachePricing("m-claude-opus"); got != nil {
		t.Errorf("missing code must return nil; got %+v", got)
	}
}

// TestLookupCachePricing_InputPriceMissing returns nil when the model
// has no price configured — caller treats cache cost as zero.
func TestLookupCachePricing_InputPriceMissing(t *testing.T) {
	l := newLayerWithModels(t, map[string]store.Model{
		"m-x": {ID: "m-x", Code: "x"},
	})
	if got := l.LookupCachePricing("m-x"); got != nil {
		t.Errorf("nil InputPricePM must return nil; got %+v", got)
	}
}

// TestLookupCachePricing_AllPricesPresent populates every field.
func TestLookupCachePricing_AllPricesPresent(t *testing.T) {
	l := newLayerWithModels(t, map[string]store.Model{
		"m-opus": {
			ID:                      "m-opus",
			Code:                    "claude-opus-4-1",
			InputPricePM:            f64ptr(15.0),
			OutputPricePM:           f64ptr(75.0),
			CachedInputReadPricePM:  f64ptr(1.5),
			CachedInputWritePricePM: f64ptr(18.75),
		},
	})
	got := l.LookupCachePricing("m-opus")
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
	l := newLayerWithModels(t, map[string]store.Model{
		"m-moonshot": {
			ID:            "m-moonshot",
			Code:          "moonshot-v1-8k",
			InputPricePM:  f64ptr(0.12),
			OutputPricePM: f64ptr(0.12),
			// CachedInputReadPricePM / CachedInputWritePricePM: nil
		},
	})
	got := l.LookupCachePricing("m-moonshot")
	if got == nil {
		t.Fatal("expected non-nil even with NULL cache prices")
	}
	if got.CacheReadUSDPerM != 0.12 || got.CacheWriteUSDPerM != 0.12 {
		t.Errorf("NULL cache should fall back to input price; got read=%v write=%v",
			got.CacheReadUSDPerM, got.CacheWriteUSDPerM)
	}
}

func f64ptr(v float64) *float64 { return &v }

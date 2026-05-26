package cachelayer

import (
	"context"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
)

// Model row is the single source of truth for ALL four prices
// (input, output, cached-read, cached-write). When admin edits a Model
// row's prices in the CP UI, the change takes effect on the next snapshot
// reload. Cache decomposition rows in the Traffic Event drawer match
// exactly because UI + gateway read the same 4 numbers.

// LookupCachePricing returns a *store.ProviderPricing-shaped struct
// assembled from the in-memory Models snapshot for the model identified
// by modelCode. Returns nil when the model is not in the snapshot OR
// when its InputPricePM is nil (no price configured → caller treats
// cache costs as zero).
//
// `adapterType` and `providerID` arguments are accepted for backwards
// compatibility with the historical signature but are no longer used:
// the Model row carries everything we need. They MAY be used by future
// per-provider overrides if the need arises.
func (l *Layer) LookupCachePricing(adapterType, providerID, modelCode string) *store.ProviderPricing {
	_ = adapterType
	_ = providerID
	idx := l.modelsByCode.Load()
	if idx == nil {
		return nil
	}
	m, ok := (*idx)[modelCode]
	if !ok || m.InputPricePM == nil {
		return nil
	}
	p := &store.ProviderPricing{
		InputUSDPerM:  *m.InputPricePM,
		OutputUSDPerM: derefOrZero(m.OutputPricePM),
		// NULL cache prices mean "no discount / no surcharge configured"
		// — fall back to InputPricePM (flat rate, no caching effect).
		CacheReadUSDPerM:  derefOrFallback(m.CachedInputReadPricePM, m.InputPricePM),
		CacheWriteUSDPerM: derefOrFallback(m.CachedInputWritePricePM, m.InputPricePM),
	}
	return p
}

// ReloadProviderPricing is preserved as a no-op for callers that still
// invoke it from config reload paths. The real reload happens via
// ReloadModels now.
func (l *Layer) ReloadProviderPricing(_ context.Context) error {
	return nil
}

func derefOrZero(f *float64) float64 {
	if f == nil {
		return 0
	}
	return *f
}

func derefOrFallback(primary, fallback *float64) float64 {
	if primary != nil {
		return *primary
	}
	if fallback != nil {
		return *fallback
	}
	return 0
}

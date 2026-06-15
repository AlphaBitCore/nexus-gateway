package cachelayer

import (
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
)

// Model row is the single source of truth for ALL four prices
// (input, output, cached-read, cached-write). When admin edits a Model
// row's prices in the CP UI, the change takes effect on the next snapshot
// reload. Cache decomposition rows in the Traffic Event drawer match
// exactly because UI + gateway read the same 4 numbers.

// LookupCachePricing returns the four per-million-token prices for the model
// identified by modelCode, assembled from the in-memory Models snapshot.
// Returns nil when the model is not in the snapshot OR when its InputPricePM
// is nil (no price configured → caller treats cache costs as zero).
func (l *Layer) LookupCachePricing(modelCode string) *store.CachePricing {
	idx := l.modelsByCode.Load()
	if idx == nil {
		return nil
	}
	m, ok := (*idx)[modelCode]
	if !ok || m.InputPricePM == nil {
		return nil
	}
	return &store.CachePricing{
		InputUSDPerM:  *m.InputPricePM,
		OutputUSDPerM: derefOrZero(m.OutputPricePM),
		// NULL cache prices mean "no discount / no surcharge configured"
		// — fall back to InputPricePM (flat rate, no caching effect).
		CacheReadUSDPerM:  derefOrFallback(m.CachedInputReadPricePM, m.InputPricePM),
		CacheWriteUSDPerM: derefOrFallback(m.CachedInputWritePricePM, m.InputPricePM),
	}
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

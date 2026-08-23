package cachelayer

import (
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/costing"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
)

// Model row is the single source of truth for ALL four prices
// (input, output, cached-read, cached-write). When admin edits a Model
// row's prices in the CP UI, the change takes effect on the next snapshot
// reload. Cache decomposition rows in the Traffic Event drawer match
// exactly because UI + gateway read the same 4 numbers.

// LookupCachePricing returns the four per-million-token prices for the model
// identified by its UUID, assembled from the in-memory Models snapshot.
// Returns nil when the model is not in the snapshot OR when its InputPricePM
// is nil (no price configured → caller treats cache costs as zero).
//
// The NULL-column interpretation — including the cache-price fallback to the
// input price — lives in costing.RatesFromModel, which the internal-operations
// price lookup also calls. Two copies of that rule would let the customer path
// and the router/AI-Guard path price the same model differently.
//
// Keyed by UUID, like every other accounting lookup, because pricing must
// survive the model becoming unservable: a request already streaming when an
// operator disables its provider still has to be costed, and the code-keyed
// index deliberately drops unservable rows. Resolving prices through that
// index would zero the cache decomposition on exactly those responses and
// leave cached tokens billed at the full input rate.
func (l *Layer) LookupCachePricing(modelID string) *store.CachePricing {
	m, ok := l.models.Get(modelID)
	if !ok {
		return nil
	}
	rates, priced := costing.RatesFromModel(
		m.InputPricePM, m.OutputPricePM, m.CachedInputReadPricePM, m.CachedInputWritePricePM)
	if !priced {
		return nil
	}
	return &rates
}

package store

import "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/costing"

// CachePricing carries the four per-million-token prices the gateway needs to
// recompute cache cost/savings for a single model. It is assembled from the
// in-memory Models snapshot (the Model row is the single source of truth for
// all pricing — the provider_pricing table was retired). Returned by
// Layer.LookupCachePricing; consumed by the proxy cache-cost recompute path.
//
// An alias, not a distinct type: the internal-operations callers (smart-router
// decider, AI Guard backend) price their own calls through costing.Rates and
// must not pull the store package's database driver into the policy layer,
// while the proxy path keeps naming the type CachePricing. Both spellings
// therefore have to be the same type, so the two paths cannot drift into
// separate pricing regimes for the same model.
type CachePricing = costing.Rates

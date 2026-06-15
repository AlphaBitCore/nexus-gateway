package store

// CachePricing carries the four per-million-token prices the gateway needs to
// recompute cache cost/savings for a single model. It is assembled from the
// in-memory Models snapshot (the Model row is the single source of truth for
// all pricing — the provider_pricing table was retired). Returned by
// Layer.LookupCachePricing; consumed by the proxy cache-cost recompute path.
type CachePricing struct {
	InputUSDPerM      float64
	OutputUSDPerM     float64
	CacheWriteUSDPerM float64
	CacheReadUSDPerM  float64
}

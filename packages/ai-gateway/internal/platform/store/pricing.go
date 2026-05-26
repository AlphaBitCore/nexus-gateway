package store

// ProviderPricing is a row from the provider_pricing table. It maps
// an (adapter_type, model_pattern) pair to per-million-token prices.
// ProviderID is nil for global defaults seeded at migration time; a
// non-nil ProviderID scopes the row to operator-configured overrides
// for a specific Provider instance and takes precedence over globals
// at lookup time.
type ProviderPricing struct {
	ID                string
	ProviderID        *string
	ModelPattern      string
	AdapterType       string
	InputUSDPerM      float64
	OutputUSDPerM     float64
	CacheWriteUSDPerM float64
	CacheReadUSDPerM  float64
	Priority          int
}

package core

// Analytics wire structs: cost, cache ROI, routing fallbacks, latency phases,
// by-provider usage, compliance KPIs, and the per-provider SLO detail.

// CostReport is the {data:[...],total} envelope from GET /api/admin/analytics/cost.
type CostReport struct {
	Data  []CostRow `json:"data"`
	Total int       `json:"total"`
}

// CostRow is one grouped cost bucket.
type CostRow struct {
	Group         string  `json:"group"`
	GroupLabel    string  `json:"groupLabel"`
	RequestCount  int     `json:"requestCount"`
	TotalTokens   int     `json:"totalTokens"`
	TotalCostUSD  float64 `json:"totalCostUsd"`
	CacheHitCount int     `json:"cacheHitCount"`
}

// CacheROIResult is GET /api/admin/analytics/cache-roi — the cache savings
// rollup powering the Cost view's "cache $ saved" line.
type CacheROIResult struct {
	PeriodDays               int     `json:"periodDays"`
	TotalEstimatedCostUSD    float64 `json:"totalEstimatedCostUsd"`
	TotalCacheNetSavingsUSD  float64 `json:"totalCacheNetSavingsUsd"`
	TotalCacheReadSavingsUSD float64 `json:"totalCacheReadSavingsUsd"`
	TotalCacheWriteCostUSD   float64 `json:"totalCacheWriteCostUsd"`
	GatewayCacheHitCount     int     `json:"gatewayCacheHitCount"`
	RequestsWithCacheHit     int     `json:"requestsWithCacheHit"`
}

// FallbacksResult is GET /api/admin/analytics/routing/fallbacks ({data:[...]}).
type FallbacksResult struct {
	Data []FallbackRow `json:"data"`
}

// FallbackRow is one routing-fallback bucket (group is a rule id or a synthetic
// label like "passthrough-fallback"; groupLabel is the resolved human name).
type FallbackRow struct {
	Group        string `json:"group"`
	GroupLabel   string `json:"groupLabel"`
	RequestCount int    `json:"requestCount"`
}

// LatencyPhasesResult is GET /api/admin/analytics/latency-phases
// ({window,rows:[...]}); it powers the Performance/SLO per-provider table.
type LatencyPhasesResult struct {
	Window LatencyWindow     `json:"window"`
	Rows   []LatencyPhaseRow `json:"rows"`
}

// LatencyWindow is the time range the rows were computed over.
type LatencyWindow struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

// LatencyPhaseRow is the latency-percentile breakdown for one group (provider /
// model / virtual key). Percentiles are whole milliseconds.
type LatencyPhaseRow struct {
	GroupKey           string `json:"groupKey"`
	GroupLabel         string `json:"groupLabel"`
	RequestCount       int    `json:"requestCount"`
	TotalP50Ms         int    `json:"totalP50Ms"`
	TotalP95Ms         int    `json:"totalP95Ms"`
	TotalP99Ms         int    `json:"totalP99Ms"`
	UpstreamTTFBP50Ms  int    `json:"upstreamTtfbP50Ms"`
	UpstreamTTFBP95Ms  int    `json:"upstreamTtfbP95Ms"`
	UpstreamTotalP50Ms int    `json:"upstreamTotalP50Ms"`
	UpstreamTotalP95Ms int    `json:"upstreamTotalP95Ms"`
	RequestHooksP95Ms  int    `json:"requestHooksP95Ms"`
	ResponseHooksP95Ms int    `json:"responseHooksP95Ms"`
}

// ByProviderResult is GET /api/admin/analytics/by-provider ({data:[...]}).
type ByProviderResult struct {
	Data []ProviderUsageRow `json:"data"`
}

// ProviderUsageRow is one provider's usage rollup (top-talkers + cost view).
type ProviderUsageRow struct {
	Provider        string  `json:"provider"`
	ProviderLabel   string  `json:"providerLabel"`
	RequestCount    int     `json:"requestCount"`
	AvgLatencyMs    float64 `json:"avgLatencyMs"`
	TotalTokens     int     `json:"totalTokens"`
	TotalEstCostUSD float64 `json:"totalEstimatedCostUsd"`
}

// ComplianceOverview is GET /api/admin/compliance/overview — compliance KPIs.
type ComplianceOverview struct {
	KPIs ComplianceKPIs `json:"kpis"`
}

// ComplianceKPIs are the headline compliance numbers.
type ComplianceKPIs struct {
	TotalRequests    int     `json:"totalRequests"`
	TotalBlocked     int     `json:"totalBlocked"`
	OverallBlockRate float64 `json:"overallBlockRate"`
	TLSCoveragePct   float64 `json:"tlsCoveragePercent"`
	HookErrorRate    float64 `json:"hookErrorRate"`
}

// ProviderDetail is GET /api/admin/analytics/provider/:id — a single provider's
// SLO detail. Only the summary (the headline) is modeled.
type ProviderDetail struct {
	Summary ProviderDetailSummary `json:"summary"`
}

// ProviderDetailSummary is the per-provider headline (availability + latency).
type ProviderDetailSummary struct {
	TotalRequests     int     `json:"totalRequests"`
	ErrorCount        int     `json:"errorCount"`
	ErrorRate         float64 `json:"errorRate"`
	CacheHitRate      float64 `json:"cacheHitRate"`
	AvgLatencyMs      float64 `json:"avgLatencyMs"`
	AvgUpstreamTTFBMs float64 `json:"avgUpstreamTtfbMs"`
	TotalEstCostUSD   float64 `json:"totalEstimatedCostUsd"`
}

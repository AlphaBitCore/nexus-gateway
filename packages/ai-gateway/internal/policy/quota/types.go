// Package quota implements cost-based quota enforcement for the AI gateway
// using a hierarchical policy/override system with Redis-backed usage tracking.
package quota

// CostEstimate is the pre-call cost prediction used by Engine.Check().
type CostEstimate struct {
	EstimatedInputTokens int64
	MaxOutputTokens      int64
	InputPricePM         float64 // price per million tokens
	OutputPricePM        float64
}

// EstimatedCost returns the predicted cost in USD.
func (e CostEstimate) EstimatedCost() float64 {
	return (float64(e.EstimatedInputTokens)*e.InputPricePM +
		float64(e.MaxOutputTokens)*e.OutputPricePM) / 1_000_000
}

// ActualUsage is the post-call cost for reconciliation.
//
// CostUSD is the single canonical, cache-aware per-request cost the AI Gateway
// already computed once from the Model table (the `rec.EstimatedCostUsd` value
// produced by the cost pipeline, including prompt-cache read/write token
// decomposition). It is the SAME number persisted to
// traffic_event.estimated_cost_usd, summed into billed_cost_usd by the Hub
// rollup, and re-seeded into the live counter by the boot Backfill. The
// Reconcile charges exactly this value rather than recomputing tokens × price
// here — a second, divergent computation that omitted cache-token
// decomposition and could disagree with the rollup across a reboot boundary.
// One price source (Model table), one cost value, every path.
type ActualUsage struct {
	CostUSD float64
}

// ActualCost returns the real cost in USD.
//
// This is already cache-aware. The CostUSD it returns is
// rec.EstimatedCostUsd as recomputed by computeCacheCosts in the proxy: that
// function bills cache-read tokens at CacheReadUSDPerM and cache-write tokens
// at CacheWriteUSDPerM, charging only the uncached remainder at the full input
// price. There is therefore NO per-token recomputation here that could charge
// cache-read tokens at full InputPricePM — the reconcile counter tracks the
// same discounted cost the caller is billed, so cache-heavy traffic is not
// over-throttled. (Re-introducing a tokens × price formula in this package is
// exactly the divergence that was removed.)
func (u ActualUsage) ActualCost() float64 {
	return u.CostUSD
}

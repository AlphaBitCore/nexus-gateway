// rollup_5m_vendor_spend.go carries the vendor-spend fan-out of the 5-minute
// fleet aggregator. It lives beside rollup_5m.go rather than inside it because
// its emission shape is the one exception to that file's rule: every other
// metric is emitted once per row over the dimension set buildEventDims
// produced, while vendor spend is emitted once per COST COMPONENT, each under
// the dimension of the provider that was actually charged for it.

package rollup

import (
	metrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/instruments"
)

// vendorSpendDimension is the dimension name every vendor-spend row is keyed
// under. It reuses `routed_provider` — the aggregator's only provider
// dimension — so a reconciliation query can read vendor spend and customer
// traffic for one provider off the same dimension key.
const vendorSpendDimension = "routed_provider"

// vendorSpendComponent is one cost figure paired with the provider that was
// actually charged for it. internal marks the internal-ops components
// (smart-router, L2 embedding, ai-guard classifier), which are additionally
// summed onto vendor_spend_internal_usd.
type vendorSpendComponent struct {
	providerID string
	amountUSD  float64
	internal   bool
}

// rowVendorSpendComponents splits one traffic_event row into the vendor-spend
// components it owes, each paired with the provider that billed for it.
//
// Inclusion is decided PER COMPONENT, never per row:
//
//   - Customer traffic (estimated_cost_usd, charged by routedProviderID) is
//     gated on G1 — tokens were produced and the response did not come from the
//     gateway cache. It is deliberately status-agnostic: a client abort or a
//     post-generation block still generated tokens the vendor charges for.
//     cacheHit here is the SELECT's derived gateway_cache_status check, not the
//     legacy cache_status column that conflated gateway hits with provider
//     prompt-cache discounts (once a ~118x over-count).
//   - The internal components need no gate at all — a non-zero value already
//     means the vendor call happened. A row-level !cacheHit gate would be
//     WRONG for them: the embedding cost is stamped on every L1 miss that
//     triggered an embedding call, including a row that then scored an L2 HIT
//     where cacheHit is true. That embedding was real money, and dropping it is
//     exactly the case the L2 cache exists to create.
//
// The ai-guard classifier's own call is served by the provider stamped on that
// row's routedProviderID (the classifier row IS the request), which is why
// ai-guard shares the customer component's provider id while the router and
// embedding calls carry their own.
func rowVendorSpendComponents(
	cacheHit *bool,
	totalTokens *int,
	estimatedCost *float64,
	routedProviderID *string,
	routerCostUsd *float64,
	routerProviderID *string,
	embeddingCostUsd *float64,
	embeddingProviderID *string,
	aiGuardCostUsd *float64,
) []vendorSpendComponent {
	var customerSpend float64
	if !derefBool5m(cacheHit) && derefInt5m(totalTokens) > 0 {
		customerSpend = derefFloat5m(estimatedCost)
	}
	return []vendorSpendComponent{
		{providerID: deref5m(routedProviderID), amountUSD: customerSpend},
		{providerID: deref5m(routerProviderID), amountUSD: derefFloat5m(routerCostUsd), internal: true},
		{providerID: deref5m(embeddingProviderID), amountUSD: derefFloat5m(embeddingCostUsd), internal: true},
		{providerID: deref5m(routedProviderID), amountUSD: derefFloat5m(aiGuardCostUsd), internal: true},
	}
}

// emitVendorSpend accumulates vendor_spend_usd — and, for internal-ops
// components, vendor_spend_internal_usd — under each component's OWN provider
// dimension. Unlike every other metric in this aggregator, one traffic_event
// row can contribute to SEVERAL routed_provider dimensions: a request served
// by one vendor is routinely routed by a model hosted on another (on
// 2026-07-30 production made 2,020 smart-router calls against 1,258
// OpenAI-served requests), so a single dimension set cannot express what the
// row owes.
//
// Components with no provider id are DROPPED. An unattributable cost cannot be
// reconciled against any vendor's bill, and folding it into the routed
// provider — the shape of pre-cutover history, where ai-guard and embedding
// costs were stamped without attribution — is precisely the bug this series
// exists to eliminate.
//
// Only accValues is written. Vendor spend carries no timestamp metadata: a
// metadata row sharing a value row's (metric, dimension, sub-dimension) would
// overwrite it in deduplicateRows5m, silently zeroing the amount. Timestamp
// metadata therefore stays on its own dedicated metric names (first_seen,
// traffic.error_class.seen), as it already does elsewhere in this package.
func emitVendorSpend(
	accValues map[accKey5m]float64,
	subDim string,
	components []vendorSpendComponent,
) {
	for _, c := range components {
		if c.providerID == "" || c.amountUSD == 0 {
			continue
		}
		dimKey := metrics.BuildDimensionKey(vendorSpendDimension, c.providerID)
		accValues[accKey5m{metrics.MetricVendorSpendUSD, dimKey, subDim}] += c.amountUSD
		if c.internal {
			accValues[accKey5m{metrics.MetricVendorSpendInternalUSD, dimKey, subDim}] += c.amountUSD
		}
	}
}

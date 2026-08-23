// rollup_vendor_spend_test.go pins the vendor-spend series: every dollar the
// gateway caused a vendor to charge, attributed to the provider ACTUALLY
// charged. The failure mode it exists to prevent is an internal call's cost
// being summed under the provider that served the REQUEST — an OpenAI embedding
// or router call reported as Anthropic spend, which makes the reconciliation
// report disagree with both vendors' bills at once.

package rollup

import (
	"context"
	"math"
	"slices"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"

	metrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/instruments"
)

// trafficEventFixture describes one traffic_event row in business terms. Only
// the fields the vendor-spend rules read are modelled; everything else keeps
// the neutral defaults of oneTrafficEventRow (source ai-gateway → sub-dimension
// source=vk, status 200, no entity/org/model).
type trafficEventFixture struct {
	// RoutedProviderID is the provider that served the REQUEST. It also bills
	// the ai-guard classifier call, because on an ai-guard row the classifier
	// call IS the request.
	RoutedProviderID string
	EstimatedCostUsd float64
	TotalTokens      int
	// StatusCode defaults to 200 when left zero.
	StatusCode int
	// GatewayCacheStatus is the raw column value; the fixture applies the same
	// IN ('hit','hit_inflight') derivation the aggregator's SELECT does.
	GatewayCacheStatus string
	// RouterCostUsd / RouterProviderID: the smart-router call, billed by the
	// provider hosting the router model — routinely NOT RoutedProviderID.
	RouterCostUsd    float64
	RouterProviderID string
	// EmbeddingCostUsd / EmbeddingProviderID: the L2 semantic-cache lookup
	// embedding, billed by the embedding model's provider.
	EmbeddingCostUsd    float64
	EmbeddingProviderID string
	AIGuardCostUsd      float64
	InternalPurpose     string
}

// gatewayCacheStatusIsHit mirrors the aggregator's SELECT-side derivation
// `gateway_cache_status IN ('hit','hit_inflight')`. Fixtures are written in
// terms of that column and never the legacy cache_status column, which
// conflated gateway-cache hits with provider prompt-cache discounts and once
// produced a ~118x over-count.
func gatewayCacheStatusIsHit(status string) bool {
	return status == "hit" || status == "hit_inflight"
}

func fixturePtr[T any](v T) *T { return &v }

// vendorSpendFixtureRow renders a fixture as one pgxmock row in
// trafficEventCols order. Values are placed by COLUMN NAME rather than by
// position: the aggregator's scan list is long and positional, and a hand-built
// literal that drifts by one silently files a cost under the wrong column.
func vendorSpendFixtureRow(ts time.Time, f trafficEventFixture) []any {
	row := oneTrafficEventRow(ts)
	set := func(col string, v any) {
		idx := slices.Index(trafficEventCols, col)
		if idx < 0 {
			panic("unknown traffic_event column in fixture: " + col)
		}
		row[idx] = v
	}

	if f.StatusCode != 0 {
		set("status_code", fixturePtr(f.StatusCode))
	}
	if f.RoutedProviderID != "" {
		set("routed_provider_id", fixturePtr(f.RoutedProviderID))
	}
	if f.EstimatedCostUsd != 0 {
		set("estimated_cost_usd", fixturePtr(f.EstimatedCostUsd))
	}
	if f.TotalTokens != 0 {
		set("total_tokens", fixturePtr(f.TotalTokens))
	}
	if f.GatewayCacheStatus != "" {
		set("cache_hit", fixturePtr(gatewayCacheStatusIsHit(f.GatewayCacheStatus)))
	}
	if f.RouterCostUsd != 0 {
		set("router_cost_usd", fixturePtr(f.RouterCostUsd))
	}
	if f.RouterProviderID != "" {
		set("router_provider_id", fixturePtr(f.RouterProviderID))
	}
	if f.EmbeddingCostUsd != 0 {
		set("embedding_cost_usd", fixturePtr(f.EmbeddingCostUsd))
	}
	if f.EmbeddingProviderID != "" {
		set("embedding_provider_id", fixturePtr(f.EmbeddingProviderID))
	}
	if f.AIGuardCostUsd != 0 {
		set("ai_guard_cost_usd", fixturePtr(f.AIGuardCostUsd))
	}
	if f.InternalPurpose != "" {
		set("internal_purpose", fixturePtr(f.InternalPurpose))
	}
	return row
}

// rollupFixtureRows drives one sealed bucket through the real aggregator with
// the given traffic_event rows and returns the rollup rows it produced, so the
// assertions below cover the SELECT list, the positional scan, and the emission
// together rather than the emission helper alone.
func rollupFixtureRows(t *testing.T, fixtures ...trafficEventFixture) []metrics.RollupRow {
	t.Helper()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	bucket := time.Now().UTC().Truncate(bucketDuration5m).Add(-10 * time.Minute)
	ts := bucket.Add(time.Minute)

	pgRows := pgxmock.NewRows(trafficEventCols)
	for _, f := range fixtures {
		pgRows = pgRows.AddRow(vendorSpendFixtureRow(ts, f)...)
	}
	mock.ExpectBegin()
	mock.ExpectQuery(`FROM traffic_event`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgRows)

	tx, err := mock.Begin(context.Background())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	rows, err := NewRollup5m(nil, time.Minute, testLogger(), false).
		aggregateTrafficEvents(context.Background(), tx, bucket, bucket.Add(bucketDuration5m))
	if err != nil {
		t.Fatalf("aggregateTrafficEvents: %v", err)
	}
	return rows
}

// vendorSpendByDim reduces the produced rollup rows to dimensionKey → summed
// value for one metric.
func vendorSpendByDim(t *testing.T, rows []metrics.RollupRow, metricName string) map[string]float64 {
	t.Helper()
	out := map[string]float64{}
	for _, r := range rows {
		if r.MetricName == metricName {
			out[r.DimensionKey] += r.Value
		}
	}
	return out
}

func routedProviderDim(providerID string) string {
	return metrics.BuildDimensionKey("routed_provider", providerID)
}

func TestRollup5m_VendorSpend_AttributesEachComponentToItsOwnProvider(t *testing.T) {
	// One request: served by Anthropic, routed by a gpt-4o router on OpenAI,
	// embedded by an OpenAI embedding model. Three providers, one row.
	rows := rollupFixtureRows(t, trafficEventFixture{
		RoutedProviderID:    "prov-anthropic",
		EstimatedCostUsd:    0.40,
		TotalTokens:         1200,
		GatewayCacheStatus:  "miss",
		RouterCostUsd:       0.0055,
		RouterProviderID:    "prov-openai",
		EmbeddingCostUsd:    0.000004,
		EmbeddingProviderID: "prov-openai-embed",
	})

	got := vendorSpendByDim(t, rows, metrics.MetricVendorSpendUSD)

	want := map[string]float64{
		routedProviderDim("prov-anthropic"):    0.40,
		routedProviderDim("prov-openai"):       0.0055,
		routedProviderDim("prov-openai-embed"): 0.000004,
	}
	for dim, wantVal := range want {
		if math.Abs(got[dim]-wantVal) > 1e-12 {
			t.Errorf("vendor_spend_usd[%s] = %v, want %v", dim, got[dim], wantVal)
		}
	}
	if len(got) != len(want) {
		t.Errorf("emitted %d provider dimensions, want %d: %v", len(got), len(want), got)
	}

	internal := vendorSpendByDim(t, rows, metrics.MetricVendorSpendInternalUSD)
	if math.Abs(internal[routedProviderDim("prov-anthropic")]) > 1e-12 {
		t.Error("customer traffic cost must not appear in vendor_spend_internal_usd")
	}
	if math.Abs(internal[routedProviderDim("prov-openai")]-0.0055) > 1e-12 {
		t.Errorf("internal series missing the router cost: %v", internal)
	}
	if math.Abs(internal[routedProviderDim("prov-openai-embed")]-0.000004) > 1e-12 {
		t.Errorf("internal series missing the embedding cost: %v", internal)
	}
}

func TestRollup5m_VendorSpend_L2HitStillOwesForItsEmbedding(t *testing.T) {
	// An L2 HIT row still paid for the embedding that produced the lookup
	// vector. A row-level !cache_hit gate would silently drop exactly the
	// case L2 exists to create.
	rows := rollupFixtureRows(t, trafficEventFixture{
		RoutedProviderID:    "prov-anthropic",
		EstimatedCostUsd:    0.40,
		TotalTokens:         1200,
		GatewayCacheStatus:  "hit",
		EmbeddingCostUsd:    0.000004,
		EmbeddingProviderID: "prov-openai-embed",
	})

	got := vendorSpendByDim(t, rows, metrics.MetricVendorSpendUSD)

	if math.Abs(got[routedProviderDim("prov-openai-embed")]-0.000004) > 1e-12 {
		t.Errorf("embedding cost dropped on an L2 hit: %v", got)
	}
	if _, ok := got[routedProviderDim("prov-anthropic")]; ok {
		t.Error("a cache hit made no upstream call; its estimated cost must not be vendor spend")
	}
}

func TestRollup5m_VendorSpend_FailedRequestWithTokensCountsButIsNotBilled(t *testing.T) {
	rows := rollupFixtureRows(t, trafficEventFixture{
		RoutedProviderID:   "prov-openai",
		EstimatedCostUsd:   0.12,
		TotalTokens:        800,
		StatusCode:         500,
		GatewayCacheStatus: "miss",
	})

	dim := routedProviderDim("prov-openai")
	spend := vendorSpendByDim(t, rows, metrics.MetricVendorSpendUSD)
	billed := vendorSpendByDim(t, rows, metrics.MetricBilledCostUSD)

	if math.Abs(spend[dim]-0.12) > 1e-12 {
		t.Errorf("vendor_spend_usd = %v, want 0.12 — the vendor charged for the tokens", spend[dim])
	}
	if billed[dim] != 0 {
		t.Errorf("billed_cost_usd = %v, want 0 — a 5xx is not customer-billable", billed[dim])
	}
}

// A row that produced no tokens cost the vendor nothing — an early rejection or
// a connection that never reached generation. G1's token half keeps such a row
// out of vendor spend even if a cost estimate was stamped on it.
func TestRollup5m_VendorSpend_ZeroTokenRowOwesNothing(t *testing.T) {
	rows := rollupFixtureRows(t, trafficEventFixture{
		RoutedProviderID:   "prov-openai",
		EstimatedCostUsd:   0.12,
		TotalTokens:        0,
		StatusCode:         403,
		GatewayCacheStatus: "miss",
	})

	spend := vendorSpendByDim(t, rows, metrics.MetricVendorSpendUSD)
	if len(spend) != 0 {
		t.Errorf("vendor_spend_usd = %v, want no rows — no tokens were generated, so no vendor charged", spend)
	}
}

// An ai-guard row is the classifier's OWN request: it carries token counts, a
// cache MISS and the classifier's cost, but no customer cost. Today the zero
// amount is what keeps its customer component out of vendor spend; this pins
// the outcome so that if estimated_cost_usd ever starts being stamped on
// ai-guard rows, the double count fails here instead of on an invoice.
func TestRollup5m_VendorSpend_AIGuardRowOwesOnlyTheClassifierCost(t *testing.T) {
	rows := rollupFixtureRows(t, trafficEventFixture{
		RoutedProviderID:   "prov-openai",
		TotalTokens:        350,
		StatusCode:         200,
		GatewayCacheStatus: "miss",
		AIGuardCostUsd:     0.0002,
		InternalPurpose:    "ai-guard",
	})

	dim := routedProviderDim("prov-openai")
	spend := vendorSpendByDim(t, rows, metrics.MetricVendorSpendUSD)
	internal := vendorSpendByDim(t, rows, metrics.MetricVendorSpendInternalUSD)

	if math.Abs(spend[dim]-0.0002) > 1e-12 {
		t.Errorf("vendor_spend_usd[%s] = %v, want the classifier cost 0.0002", dim, spend[dim])
	}
	if len(spend) != 1 {
		t.Errorf("emitted %d provider dimensions, want 1: %v", len(spend), spend)
	}
	// Every dollar this row owes is internal overhead. If the two series ever
	// diverge here, customer spend has been attributed to a row that never
	// served a customer request.
	if math.Abs(spend[dim]-internal[dim]) > 1e-12 {
		t.Errorf("ai-guard row: vendor_spend_usd %v != vendor_spend_internal_usd %v — customer spend was counted on a classifier row",
			spend[dim], internal[dim])
	}
}

// Rows written before the attribution columns existed carry an internal-ops
// cost with no provider id. Folding those into routed_provider_id is precisely
// the bug this series exists to eliminate, so an unattributable component is
// DROPPED — a cost no vendor's bill can be matched against is worse than a
// visible gap.
func TestRollup5m_VendorSpend_UnattributedComponentIsDroppedNotFolded(t *testing.T) {
	t.Run("unattributed embedding is not folded into the routed provider", func(t *testing.T) {
		rows := rollupFixtureRows(t, trafficEventFixture{
			RoutedProviderID:   "prov-anthropic",
			EstimatedCostUsd:   0.40,
			TotalTokens:        1200,
			GatewayCacheStatus: "miss",
			EmbeddingCostUsd:   0.000004, // pre-cutover row: no embedding_provider_id
		})

		dim := routedProviderDim("prov-anthropic")
		spend := vendorSpendByDim(t, rows, metrics.MetricVendorSpendUSD)
		internal := vendorSpendByDim(t, rows, metrics.MetricVendorSpendInternalUSD)

		if got := spend[dim]; got != 0.40 {
			t.Errorf("vendor_spend_usd[%s] = %v, want exactly 0.40 — the unattributed embedding was folded into the serving provider", dim, got)
		}
		if len(spend) != 1 {
			t.Errorf("emitted %d provider dimensions, want 1: %v", len(spend), spend)
		}
		if len(internal) != 0 {
			t.Errorf("vendor_spend_internal_usd = %v, want no rows — the embedding has no provider to reconcile against", internal)
		}
	})

	t.Run("a fully unattributed row emits no vendor-spend series at all", func(t *testing.T) {
		rows := rollupFixtureRows(t, trafficEventFixture{
			TotalTokens:        900,
			GatewayCacheStatus: "miss",
			EmbeddingCostUsd:   0.000004,
			AIGuardCostUsd:     0.0002,
			RouterCostUsd:      0.0055,
		})

		spend := vendorSpendByDim(t, rows, metrics.MetricVendorSpendUSD)
		internal := vendorSpendByDim(t, rows, metrics.MetricVendorSpendInternalUSD)
		if len(spend) != 0 || len(internal) != 0 {
			t.Errorf("vendor spend emitted with no provider attribution: spend=%v internal=%v", spend, internal)
		}
	})
}

// Vendor spend is the only metric in this aggregator emitted outside the
// buildEventDims fan-out. It must stay on routed_provider dimensions only: a
// global-dimension row would double the reconciliation total the moment a
// report sums the per-provider rows.
func TestRollup5m_VendorSpend_EmittedOnProviderDimensionsOnly(t *testing.T) {
	rows := rollupFixtureRows(t, trafficEventFixture{
		RoutedProviderID:    "prov-anthropic",
		EstimatedCostUsd:    0.40,
		TotalTokens:         1200,
		GatewayCacheStatus:  "miss",
		RouterCostUsd:       0.0055,
		RouterProviderID:    "prov-openai",
		EmbeddingCostUsd:    0.000004,
		EmbeddingProviderID: "prov-openai",
	})

	for _, r := range rows {
		if r.MetricName != metrics.MetricVendorSpendUSD && r.MetricName != metrics.MetricVendorSpendInternalUSD {
			continue
		}
		if r.DimensionKey != routedProviderDim("prov-anthropic") && r.DimensionKey != routedProviderDim("prov-openai") {
			t.Errorf("%s emitted on dimension %q — vendor spend belongs on routed_provider dimensions only", r.MetricName, r.DimensionKey)
		}
		if r.SubDimension != metrics.BuildSubDimension("vk", "") {
			t.Errorf("%s sub-dimension = %q, want source=vk", r.MetricName, r.SubDimension)
		}
	}

	// The two internal components share one provider here, so they must SUM on
	// that provider's dimension rather than one overwriting the other.
	internal := vendorSpendByDim(t, rows, metrics.MetricVendorSpendInternalUSD)
	if want := 0.0055 + 0.000004; math.Abs(internal[routedProviderDim("prov-openai")]-want) > 1e-12 {
		t.Errorf("vendor_spend_internal_usd[prov-openai] = %v, want %v — components sharing a provider must accumulate",
			internal[routedProviderDim("prov-openai")], want)
	}
}

// Several rows in one bucket accumulate per provider, and each row's own
// attribution is preserved: the second row's router ran on a third provider.
func TestRollup5m_VendorSpend_AccumulatesAcrossRowsInABucket(t *testing.T) {
	rows := rollupFixtureRows(t,
		trafficEventFixture{
			RoutedProviderID:   "prov-anthropic",
			EstimatedCostUsd:   0.40,
			TotalTokens:        1200,
			GatewayCacheStatus: "miss",
			RouterCostUsd:      0.0055,
			RouterProviderID:   "prov-openai",
		},
		trafficEventFixture{
			RoutedProviderID:   "prov-anthropic",
			EstimatedCostUsd:   0.10,
			TotalTokens:        300,
			GatewayCacheStatus: "miss",
			RouterCostUsd:      0.0011,
			RouterProviderID:   "prov-groq",
		},
	)

	spend := vendorSpendByDim(t, rows, metrics.MetricVendorSpendUSD)
	want := map[string]float64{
		routedProviderDim("prov-anthropic"): 0.50,
		routedProviderDim("prov-openai"):    0.0055,
		routedProviderDim("prov-groq"):      0.0011,
	}
	for dim, wantVal := range want {
		if math.Abs(spend[dim]-wantVal) > 1e-12 {
			t.Errorf("vendor_spend_usd[%s] = %v, want %v", dim, spend[dim], wantVal)
		}
	}
	if len(spend) != len(want) {
		t.Errorf("emitted %d provider dimensions, want %d: %v", len(spend), len(want), spend)
	}
}

// The merge cascade folds by the per-metric aggregation kind, defaulting to
// Sum, and excludes only the point-in-time gauge snapshots. Both vendor-spend
// series are additive money, so they must be on the Sum default and absent
// from the gauge denylist — otherwise 5m → 1h → 1d → 1mo propagation either
// stops or over-counts.
func TestVendorSpend_MergesAsAdditiveSumAcrossTiers(t *testing.T) {
	for _, name := range []string{metrics.MetricVendorSpendUSD, metrics.MetricVendorSpendInternalUSD} {
		if kind := metrics.AggregationKindFor(name); kind != metrics.AggregationSum {
			t.Errorf("%s aggregation kind = %v, want Sum", name, kind)
		}
		if _, denied := gaugeMergeMetrics[name]; denied {
			t.Errorf("%s is in gaugeMergeMetrics — it is an additive sum, not a gauge snapshot, and would never reach the coarser tiers", name)
		}
	}
}

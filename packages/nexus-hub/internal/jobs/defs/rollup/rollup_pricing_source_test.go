// rollup_pricing_source_test.go pins the rollup side of the single-canonical-
// price-source invariant.
//
// The AI Gateway computes a request's cost exactly once, from the Model table,
// cache-aware, and stamps it onto traffic_event.estimated_cost_usd. The rollup
// must treat that column as the authoritative cost — billed_cost_usd is a
// PASSTHROUGH of estimated_cost_usd for success + non-cache rows, never a
// re-priced value. Combined with the gateway-side test that the live quota
// counter is incremented by the same estimated_cost_usd
// (TestServeProxy_Reconcile_ChargesCanonicalEstimatedCost_F0163 in ai-gateway),
// this proves enforcement, reconcile, rollup, and the boot Backfill all price a
// given model identically and cannot diverge across a reboot.
package rollup

import (
	"context"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"

	metrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/instruments"
)

// successNonCacheCostRow returns one ai-gateway traffic_event row with HTTP 200,
// no cache hit, no internal-ops costs, and a known estimated_cost_usd. The
// rollup must emit billed_cost_usd == estimated_cost_usd == cost for it.
func successNonCacheCostRow(ts time.Time, cost float64) []any {
	src := "ai-gateway"
	sc := 200
	entity := "user-canon"
	etype := "user"
	org := "org-canon"
	c := cost
	return []any{
		&src,            // source
		(*string)(nil),  // provider_id
		(*string)(nil),  // model_id
		&entity,         // entity_id
		&etype,          // entity_type
		&org,            // org_id
		(*string)(nil),  // routed_provider_id
		(*string)(nil),  // routing_rule_id
		(*string)(nil),  // target_host
		(*string)(nil),  // source_ip
		&sc,             // status_code
		(*int)(nil),     // latency_ms
		(*bool)(nil),    // cache_hit (nil → not a cache hit → billed counts it)
		(*int)(nil),     // prompt_tokens
		(*int)(nil),     // completion_tokens
		(*int)(nil),     // total_tokens
		&c,              // estimated_cost_usd  ← the single canonical cost
		(*float64)(nil), // gateway_cache_savings_usd
		(*string)(nil),  // request_hook_decision
		(*string)(nil),  // response_hook_decision
		(*string)(nil),  // bump_status
		(*string)(nil),  // routed_model_id
		(*string)(nil),  // original_model_id
		(*bool)(nil),    // has_quality_signals
		(*string)(nil),  // virtual_key_id
		(*string)(nil),  // project_id
		(*string)(nil),  // error_code  (empty → success)
		ts,              // timestamp
		(*float64)(nil), // cache_write_cost_usd
		(*float64)(nil), // cache_read_savings_usd
		(*float64)(nil), // cache_net_savings_usd
		(*int64)(nil),   // cache_creation_tokens
		(*int64)(nil),   // cache_read_tokens
		(*bool)(nil),    // l4_cache_hit
		(*int64)(nil),   // normalized_strip_count
		(*int64)(nil),   // normalized_strip_bytes
		(*int64)(nil),   // cache_marker_injected
		(*int)(nil),     // upstream_ttfb_ms
		(*int)(nil),     // upstream_total_ms
		(*int)(nil),     // request_hooks_ms
		(*int)(nil),     // response_hooks_ms
		(*float64)(nil), // embedding_cost_usd  (no internal ops)
		(*float64)(nil), // ai_guard_cost_usd   (no internal ops)
	}
}

// TestRollup5m_BilledCostIsEstimatedCostPassthrough_F0163 asserts that, for a
// success + non-cache row with no internal-ops costs, the rollup emits
// billed_cost_usd equal to the gateway-stamped estimated_cost_usd at every
// dimension cell — i.e. the rollup reads the unified Model-table-derived cost
// and does not re-price. If the rollup ever recomputed billed from a second
// price source (the old provider_pricing divergence), these would differ.
func TestRollup5m_BilledCostIsEstimatedCostPassthrough_F0163(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	bucket := time.Now().UTC().Truncate(5 * time.Minute).Add(-10 * time.Minute)
	ts := bucket.Add(time.Minute)
	const cost = 6.30 // the canonical cache-aware cost the gateway stamped

	mock.ExpectBegin()
	rows := pgxmock.NewRows(trafficEventCols).AddRow(successNonCacheCostRow(ts, cost)...)
	mock.ExpectQuery(`FROM traffic_event`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(rows)

	tx, err := mock.Begin(context.Background())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	// excludeInternalOpsFromBilled=false (the default): billed = estimated here
	// because there are no internal-ops costs on the row.
	j := NewRollup5m(nil, time.Minute, testLogger(), false)
	rollupRows, err := j.aggregateTrafficEvents(context.Background(), tx, bucket, bucket.Add(bucketDuration5m))
	if err != nil {
		t.Fatalf("aggregateTrafficEvents: %v", err)
	}

	type cell struct{ dim, sub string }
	estimated := map[cell]float64{}
	billed := map[cell]float64{}
	for _, r := range rollupRows {
		switch r.MetricName {
		case metrics.MetricEstimatedCostUSD:
			estimated[cell{r.DimensionKey, r.SubDimension}] = r.Value
		case metrics.MetricBilledCostUSD:
			billed[cell{r.DimensionKey, r.SubDimension}] = r.Value
		}
	}

	if len(estimated) == 0 || len(billed) == 0 {
		t.Fatalf("expected both estimated and billed cost rows; estimated=%d billed=%d", len(estimated), len(billed))
	}
	// Every billed cell must passthrough the estimated cost (== the gateway-
	// stamped canonical cost), with no re-pricing drift.
	for c, b := range billed {
		e, ok := estimated[c]
		if !ok {
			t.Errorf("billed cell %+v has no matching estimated cell", c)
			continue
		}
		if b != e {
			t.Errorf("cell %+v: billed=%.6f != estimated=%.6f (rollup must not re-price; F-0163)", c, b, e)
		}
		if b != cost {
			t.Errorf("cell %+v: billed=%.6f != stamped cost %.6f (rollup must passthrough estimated_cost_usd)", c, b, cost)
		}
	}
}

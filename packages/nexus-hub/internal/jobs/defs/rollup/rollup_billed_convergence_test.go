package rollup

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"

	metrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/instruments"
)

// rowWithInternalOps is successNonCacheCostRow plus the two internal-ops costs
// the existing rollup fixtures always leave at zero — which is exactly why the
// divergence below went unnoticed: no test ever put a value in these columns.
func rowWithInternalOps(ts time.Time, cost, embedding, aiGuard float64) []any {
	row := successNonCacheCostRow(ts, cost)
	row[slices.Index(trafficEventCols, "embedding_cost_usd")] = &embedding
	row[slices.Index(trafficEventCols, "ai_guard_cost_usd")] = &aiGuard
	return row
}

// billed_cost_usd is the number the AI Gateway's quota Backfill re-seeds the
// live counter from on boot (usage_cache_backfill.go). The live counter itself
// only ever charges rec.EstimatedCostUsd — it has no idea internal-ops costs
// exist, and cannot: the flag that used to control this lived in the HUB's
// config and was never pushed to the gateway.
//
// So any internal-ops cost the rollup folds into billed makes the quota counter
// JUMP across a reboot: charged one number while live, re-seeded from a bigger
// one. That only stays invisible while the semantic cache and AI-Guard are both
// switched off, which zeroes those columns on every row — which is exactly the
// blind spot the fixtures around this one share.
//
// The invariant, now structural rather than configurable: billed passes through
// estimated. Internal-ops costs stay visible on their own dedicated series.
func TestRollup5m_BilledPassesThroughEstimated_EvenWithInternalOps(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	ts := time.Now().UTC().Truncate(time.Minute)
	bucket := ts.Truncate(bucketDuration5m)
	const (
		cost      = 1.0
		embedding = 0.25
		aiGuard   = 0.10
	)

	mock.ExpectBegin()
	mock.ExpectQuery(`FROM traffic_event`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(trafficEventCols).AddRow(rowWithInternalOps(ts, cost, embedding, aiGuard)...))

	tx, err := mock.Begin(context.Background())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	// false is the flag's shipped default ("include internal ops"). The point of
	// this test is that the value no longer changes the answer: billed passes
	// through estimated either way, because the gateway's live counter has no
	// way to know what was chosen here.
	j := NewRollup5m(nil, time.Minute, testLogger(), false)
	rows, err := j.aggregateTrafficEvents(context.Background(), tx, bucket, bucket.Add(bucketDuration5m))
	if err != nil {
		t.Fatalf("aggregateTrafficEvents: %v", err)
	}

	var billed, estimated, embMetric, guardMetric float64
	var sawBilled bool
	for _, r := range rows {
		switch r.MetricName {
		case metrics.MetricBilledCostUSD:
			billed += r.Value
			sawBilled = true
		case metrics.MetricEstimatedCostUSD:
			estimated += r.Value
		case metrics.MetricEmbeddingCostUSD:
			embMetric += r.Value
		case metrics.MetricAIGuardCostUSD:
			guardMetric += r.Value
		}
	}

	if !sawBilled {
		t.Fatal("no billed_cost_usd row produced at all")
	}
	if billed != estimated {
		t.Fatalf("billed=%v != estimated=%v — the gateway's live quota counter charges only estimated, so a billed that includes internal ops makes the counter jump on reboot when the Backfill re-seeds from it",
			billed, estimated)
	}
	// One row fans out across several dimensions (entity / org / provider / …),
	// so both sums are that same row counted once per dimension. The invariant
	// is that they track each other, not their absolute value — and the sums
	// must be a whole multiple of the row's cost, never cost+embedding+aiGuard.
	if billed == 0 || float64(int(billed/cost))*cost != billed {
		t.Fatalf("billed=%v is not a multiple of the row's estimated_cost_usd %v — internal ops (%v + %v) leaked in",
			billed, cost, embedding, aiGuard)
	}

	// Excluding them from billed must not HIDE them: an operator still has to be
	// able to see what the L2 cache and the classifier cost. Same dimension
	// fan-out as above, so compare the ratio against billed rather than the raw
	// per-row value.
	fanout := billed / cost
	if embMetric != embedding*fanout {
		t.Errorf("embedding cost is not intact on its own series: got %v want %v — excluded from billed must not mean dropped", embMetric, embedding*fanout)
	}
	if guardMetric != aiGuard*fanout {
		t.Errorf("ai-guard cost is not intact on its own series: got %v want %v", guardMetric, aiGuard*fanout)
	}
}

// The knob is vestigial, and this pins that: both values must produce the same
// billed total. It cannot be honoured — it lives in the Hub's config while the
// gateway that charges the live quota counter is never told it exists, so any
// value that changed the answer here would put the two sides out of step.
func TestRollup5m_BilledIgnoresTheVestigialToggle(t *testing.T) {
	ts := time.Now().UTC().Truncate(time.Minute)
	bucket := ts.Truncate(bucketDuration5m)
	const cost, embedding, aiGuard = 1.0, 0.25, 0.10

	totals := map[bool]float64{}
	for _, flag := range []bool{false, true} {
		mock, err := pgxmock.NewPool()
		if err != nil {
			t.Fatalf("pgxmock: %v", err)
		}
		mock.ExpectBegin()
		mock.ExpectQuery(`FROM traffic_event`).
			WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
			WillReturnRows(pgxmock.NewRows(trafficEventCols).AddRow(rowWithInternalOps(ts, cost, embedding, aiGuard)...))
		tx, err := mock.Begin(context.Background())
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		rows, err := NewRollup5m(nil, time.Minute, testLogger(), flag).
			aggregateTrafficEvents(context.Background(), tx, bucket, bucket.Add(bucketDuration5m))
		if err != nil {
			t.Fatalf("aggregateTrafficEvents(flag=%v): %v", flag, err)
		}
		for _, r := range rows {
			if r.MetricName == metrics.MetricBilledCostUSD {
				totals[flag] += r.Value
			}
		}
		mock.Close()
	}

	if totals[false] != totals[true] {
		t.Fatalf("the toggle still moves billed: false=%v true=%v — it cannot, the gateway never sees this flag and would charge a different number",
			totals[false], totals[true])
	}
}

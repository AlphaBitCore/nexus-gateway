package analytics

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"

	metrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/instruments"
)

// expectWatermarksMissing queues the 3 GetWatermark probes
// (merge-1mo / merge-1d / merge-1h) with ErrNoRows so the cascade
// boundaries collapse to q.StartTime → only the trailing 5m segment
// actually runs a real query.
func expectWatermarksMissing(mock pgxmock.PgxPoolIface) {
	for range 3 {
		mock.ExpectQuery(`FROM "rollup_watermark"`).
			WithArgs(pgxmock.AnyArg()).
			WillReturnError(pgx.ErrNoRows)
	}
}

// expectCascade5mEmpty queues the watermark probes + an empty 5m SELECT
// matching any positional args. The 5m SELECT carries [from, to, ...metrics,
// optional dim, optional sub] — variable count — so we use a permissive
// catch-all arg matcher.
func expectCascade5mEmpty(mock pgxmock.PgxPoolIface) {
	expectWatermarksMissing(mock)
	mock.ExpectQuery(`FROM "metric_rollup_5m"`).
		WithArgs(anyArgsUpTo(12)...).
		WillReturnRows(pgxmock.NewRows(rollupCols))
}

// expectCascade5mRows queues the watermark probes + a 5m SELECT returning
// the supplied rows.
func expectCascade5mRows(mock pgxmock.PgxPoolIface, rows *pgxmock.Rows, nargs int) {
	expectWatermarksMissing(mock)
	mock.ExpectQuery(`FROM "metric_rollup_5m"`).
		WithArgs(anyArgsUpTo(nargs)...).
		WillReturnRows(rows)
}

// anyArgsUpTo returns n pgxmock.AnyArg() values.
func anyArgsUpTo(n int) []any {
	out := make([]any, n)
	for i := range out {
		out[i] = pgxmock.AnyArg()
	}
	return out
}

// Aliases that infer the typical arg count for each helper based on the
// caller surface. costSummaryMetrics has 5 entries (embedding + ai-guard included) so:
//   - rollupCostSummaryTotals: 2(time) + 5(metrics) = 7 args
//   - rollupCostSummaryByDim:  2(time) + 5(metrics) + 1(dim) = 8 args
//
// For other cascades use expectCascade5mRows(mock, rows, n) directly.
func expectCascadeAllEmpty(mock pgxmock.PgxPoolIface) { expectCascade5mEmpty(mock) }
func expectCascadeWithRows(mock pgxmock.PgxPoolIface, rows *pgxmock.Rows) {
	expectCascade5mRows(mock, rows, 7)
}
func expectCascadeWithRowsDim(mock pgxmock.PgxPoolIface, rows *pgxmock.Rows) {
	expectCascade5mRows(mock, rows, 8)
}

// expectCascade5mEmpty5 is an arg-count-aware empty cascade for callers
// where the metric set size differs from rollupCostSummary defaults. n is
// the total positional arg count (time + metrics + optional dim/sub).
func expectCascade5mEmptyN(mock pgxmock.PgxPoolIface, n int) {
	expectWatermarksMissing(mock)
	mock.ExpectQuery(`FROM "metric_rollup_5m"`).
		WithArgs(anyArgsUpTo(n)...).
		WillReturnRows(pgxmock.NewRows(rollupCols))
}

// expectCascade5mErrorN makes the cascade FAIL rather than come back empty.
// Telling the two apart is the whole point of the tests below: both fall back to
// the direct scan, but only one of them is a fault worth logging.
func expectCascade5mErrorN(mock pgxmock.PgxPoolIface, n int) {
	expectWatermarksMissing(mock)
	mock.ExpectQuery(`FROM "metric_rollup_5m"`).
		WithArgs(anyArgsUpTo(n)...).
		WillReturnError(errors.New("rollup table unavailable"))
}

func TestRollupCostSummaryTotals_NoData(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	expectCascade5mEmptyN(mock, 7)
	_, ok := h.rollupCostSummaryTotals(context.Background(),
		time.Now().Add(-time.Hour), time.Now())
	if ok {
		t.Errorf("expected ok=false when rollup empty")
	}
}

func TestRollupCostSummaryTotals_WithRows(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	bucket := time.Now().Add(-30 * time.Minute)
	rows := pgxmock.NewRows(rollupCols).
		AddRow("id1", bucket, metrics.MetricEstimatedCostUSD, "", "", float64(42.5), []byte(nil), bucket).
		AddRow("id2", bucket, metrics.MetricGatewayCacheSavingsUSD, "", "", float64(5.0), []byte(nil), bucket).
		AddRow("id3", bucket, metrics.MetricCacheNetSavingsUSD, "", "", float64(7.5), []byte(nil), bucket)
	expectCascadeWithRows(mock, rows)
	tot, ok := h.rollupCostSummaryTotals(context.Background(),
		time.Now().Add(-time.Hour), time.Now())
	if !ok || tot.estimated != 42.5 || tot.gatewaySavings != 5.0 || tot.providerPromptNet != 7.5 {
		t.Errorf("got %#v ok=%v", tot, ok)
	}
}

func TestRollupCostSummaryTotals_QueryError(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	expectWatermarksMissing(mock)
	mock.ExpectQuery(`FROM "metric_rollup_5m"`).
		WithArgs(anyArgsUpTo(7)...).
		WillReturnError(errors.New("conn lost"))
	_, ok := h.rollupCostSummaryTotals(context.Background(),
		time.Now().Add(-time.Hour), time.Now())
	if ok {
		t.Errorf("expected ok=false on query error")
	}
}

func TestRollupCostSummaryByDim_NoData(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	expectCascade5mEmptyN(mock, 8)
	groups := h.rollupCostSummaryByDim(context.Background(),
		time.Now().Add(-time.Hour), time.Now(), "organization")
	if len(groups) != 0 {
		t.Errorf("want empty groups, got %d", len(groups))
	}
}

func TestRollupCostSummaryByDim_WithRows(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	bucket := time.Now().Add(-30 * time.Minute)
	rows := pgxmock.NewRows(rollupCols).
		AddRow("id1", bucket, metrics.MetricEstimatedCostUSD, "organization=org-1", "",
			float64(100.0), []byte(nil), bucket)
	expectCascadeWithRowsDim(mock, rows)
	groups := h.rollupCostSummaryByDim(context.Background(),
		time.Now().Add(-time.Hour), time.Now(), "organization")
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Logf("expectations: %v", err)
	}
	if len(groups) != 1 {
		t.Fatalf("want 1 group, got %d", len(groups))
	}
}

func TestRollupCostSummaryByDim_QueryError(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	expectWatermarksMissing(mock)
	mock.ExpectQuery(`FROM "metric_rollup_5m"`).
		WithArgs(anyArgsUpTo(8)...).
		WillReturnError(errors.New("boom"))
	groups := h.rollupCostSummaryByDim(context.Background(),
		time.Now().Add(-time.Hour), time.Now(), "organization")
	if groups != nil {
		t.Errorf("want nil on query error, got %v", groups)
	}
}

func TestAnalyticsCostSummary_NilDB(t *testing.T) {
	t.Parallel()
	h := &Handler{} // db nil
	c, rec := echoCtx("GET", "/api/admin/analytics/cost-summary")
	if err := h.AnalyticsCostSummary(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	assertStatus(t, rec, http.StatusInternalServerError)
}

// TestAnalyticsCostSummary_RollupHit exercises the rollup path: cascade has
// data → totals computed from it, then byOrg + byProvider are also pulled
// from rollup. The reasoning_cost direct fallback still fires.
func TestAnalyticsCostSummary_RollupHit(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	bucket := time.Now().Add(-30 * time.Minute)

	// totals cascade: 3 metrics + 2 time = 5 args (no dim).
	totalRows := pgxmock.NewRows(rollupCols).
		AddRow("a", bucket, metrics.MetricEstimatedCostUSD, "", "", float64(99.0), []byte(nil), bucket).
		AddRow("b", bucket, metrics.MetricGatewayCacheSavingsUSD, "", "", float64(11.0), []byte(nil), bucket).
		AddRow("c", bucket, metrics.MetricCacheNetSavingsUSD, "", "", float64(3.0), []byte(nil), bucket)
	expectCascade5mRows(mock, totalRows, 7)

	// reasoning_cost direct query
	mock.ExpectQuery(`SELECT COALESCE\(SUM\(reasoning_cost_usd\)`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"sum"}).AddRow(float64(2.0)))

	// byOrg cascade: 3 metrics + 2 time + 1 dim = 6 args
	orgRows := pgxmock.NewRows(rollupCols).
		AddRow("o1", bucket, metrics.MetricEstimatedCostUSD, "organization=org-A", "",
			float64(60.0), []byte(nil), bucket).
		AddRow("o2", bucket, metrics.MetricEstimatedCostUSD, "organization=", "",
			float64(5.0), []byte(nil), bucket)
	expectCascade5mRows(mock, orgRows, 8)

	// org label resolution — h.resolveDimensionLabels fetch by Organization
	mock.ExpectQuery(`FROM "Organization"`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id", "name"}).AddRow("org-A", "Acme"))

	// byProvider cascade (routed_provider): 3 metrics + 2 time + 1 dim = 6
	provRows := pgxmock.NewRows(rollupCols).
		AddRow("p1", bucket, metrics.MetricEstimatedCostUSD, "routed_provider=prov-1", "",
			float64(40.0), []byte(nil), bucket)
	expectCascade5mRows(mock, provRows, 8)

	c, rec := echoCtx("GET", "/api/admin/analytics/cost-summary")
	if err := h.AnalyticsCostSummary(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	assertStatus(t, rec, http.StatusOK)
	body := jsonBody(t, rec)
	if body["totalCostUsd"] != float64(99.0) {
		t.Errorf("totalCostUsd=%v want 99", body["totalCostUsd"])
	}
	if body["totalGatewayCacheSavingsUsd"] != float64(11.0) {
		t.Errorf("gatewayCacheSavings=%v", body["totalGatewayCacheSavingsUsd"])
	}
	if body["totalProviderPromptCacheNetSavingsUsd"] != float64(3.0) {
		t.Errorf("providerPromptCacheNetSavings=%v", body["totalProviderPromptCacheNetSavingsUsd"])
	}
	if body["totalReasoningCostUsd"] != float64(2.0) {
		t.Errorf("reasoningCost=%v", body["totalReasoningCostUsd"])
	}
	byOrg, _ := body["byOrg"].([]any)
	if len(byOrg) != 2 {
		t.Errorf("byOrg len=%d", len(byOrg))
	}
	byProv, _ := body["byProvider"].([]any)
	if len(byProv) != 1 {
		t.Errorf("byProvider len=%d", len(byProv))
	}
}

// TestAnalyticsCostSummary_DirectFallback exercises the path where rollup
// returns nothing → handler queries traffic_event directly for all three
// sections (totals, byOrg, byProvider).
func TestAnalyticsCostSummary_DirectFallback(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)

	// totals cascade empty (7 args: time + 5 metrics)
	expectCascade5mEmptyN(mock, 7)

	// direct totals query (6-column scan: cost, gws, cns, rc, ec, ag)
	mock.ExpectQuery(`SELECT[\s\S]*FROM traffic_event`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"cost", "gws", "cns", "rc", "ec", "ag"}).
			AddRow(float64(123.0), float64(7.0), float64(2.0), float64(0.5), float64(0.01), float64(0.02)))

	// byOrg cascade empty (8 args: time + 5 metrics + dim)
	expectCascade5mEmptyN(mock, 8)

	// direct byOrg query
	mock.ExpectQuery(`GROUP BY org_id`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"org_id", "cost"}).
			AddRow("org-A", float64(100.0)))

	// byProvider cascade empty (8 args)
	expectCascade5mEmptyN(mock, 8)

	// direct byProvider query
	mock.ExpectQuery(`GROUP BY 1`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"provider_id", "cost"}).
			AddRow("prov-A", float64(50.0)))

	c, rec := echoCtx("GET", "/api/admin/analytics/cost-summary")
	if err := h.AnalyticsCostSummary(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	assertStatus(t, rec, http.StatusOK)
	body := jsonBody(t, rec)
	if body["totalCostUsd"] != float64(123.0) {
		t.Errorf("totalCostUsd=%v", body["totalCostUsd"])
	}
	if body["periodDays"] != float64(30) {
		t.Errorf("periodDays=%v", body["periodDays"])
	}
	byOrg, _ := body["byOrg"].([]any)
	if len(byOrg) != 1 {
		t.Errorf("byOrg len=%d", len(byOrg))
	}
	byProv, _ := body["byProvider"].([]any)
	if len(byProv) != 1 {
		t.Errorf("byProvider len=%d", len(byProv))
	}
}

func TestAnalyticsCostSummary_DirectTotalsError(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	expectCascade5mEmptyN(mock, 7)
	mock.ExpectQuery(`SELECT[\s\S]*FROM traffic_event`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnError(errors.New("boom"))

	c, rec := echoCtx("GET", "/api/admin/analytics/cost-summary")
	if err := h.AnalyticsCostSummary(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	assertStatus(t, rec, http.StatusInternalServerError)
}

func TestAnalyticsCostSummary_DirectOrgError(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	expectCascade5mEmptyN(mock, 7)
	mock.ExpectQuery(`SELECT[\s\S]*FROM traffic_event`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"a", "b", "c", "d", "e", "f"}).
			AddRow(float64(0), float64(0), float64(0), float64(0), float64(0), float64(0)))
	// byOrg cascade empty
	expectCascade5mEmptyN(mock, 8)
	mock.ExpectQuery(`GROUP BY org_id`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnError(errors.New("boom"))

	c, rec := echoCtx("GET", "/api/admin/analytics/cost-summary")
	if err := h.AnalyticsCostSummary(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	assertStatus(t, rec, http.StatusInternalServerError)
}

func TestAnalyticsCostSummary_DirectProviderError(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	expectCascade5mEmptyN(mock, 7)
	mock.ExpectQuery(`SELECT[\s\S]*FROM traffic_event`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"a", "b", "c", "d", "e", "f"}).
			AddRow(float64(0), float64(0), float64(0), float64(0), float64(0), float64(0)))
	expectCascade5mEmptyN(mock, 8)
	mock.ExpectQuery(`GROUP BY org_id`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"org_id", "cost"}))
	expectCascade5mEmptyN(mock, 8)
	mock.ExpectQuery(`GROUP BY 1`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnError(errors.New("boom"))

	c, rec := echoCtx("GET", "/api/admin/analytics/cost-summary")
	if err := h.AnalyticsCostSummary(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	assertStatus(t, rec, http.StatusInternalServerError)
}

// TestAnalyticsCostSummary_DirectScanError covers the rows.Scan error
// continuation path: a row with the wrong column count for both direct
// breakdowns is skipped without breaking the response.
func TestAnalyticsCostSummary_DirectScanError(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	expectCascade5mEmptyN(mock, 7)
	mock.ExpectQuery(`SELECT[\s\S]*FROM traffic_event`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"a", "b", "c", "d", "e", "f"}).
			AddRow(float64(0), float64(0), float64(0), float64(0), float64(0), float64(0)))

	// byOrg cascade empty → fallback. One row with mismatched scan target
	// (string instead of float) → loop continues; one good row.
	expectCascade5mEmptyN(mock, 8)
	mock.ExpectQuery(`GROUP BY org_id`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"org_id", "cost"}).
			AddRow("o1", "notafloat").
			AddRow("o2", float64(5.0)))

	expectCascade5mEmptyN(mock, 8)
	mock.ExpectQuery(`GROUP BY 1`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"provider_id", "cost"}).
			AddRow("p1", "notafloat").
			AddRow("p2", float64(7.0)))

	c, rec := echoCtx("GET", "/api/admin/analytics/cost-summary")
	if err := h.AnalyticsCostSummary(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	assertStatus(t, rec, http.StatusOK)
	body := jsonBody(t, rec)
	byOrg, _ := body["byOrg"].([]any)
	if len(byOrg) != 1 {
		t.Errorf("byOrg expected 1 (other skipped), got %d", len(byOrg))
	}
}

// Cost summary is deliberately NOT in TestAnalyticsEndpoints_ReadFailureIsAlways5xx:
// unlike those endpoints it has a real direct-scan fallback, so a rollup read
// failure must degrade to the slow-but-correct path rather than 5xx. Pinning
// that here because the two contracts now sit side by side in one package and
// the wrong one is easy to copy.
func TestAnalyticsCostSummary_RollupReadFailureFallsBackNot5xx(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)

	expectCascade5mErrorN(mock, 7) // totals cascade FAILS
	mock.ExpectQuery(`COALESCE\(SUM\(estimated_cost_usd\)`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"c1", "c2", "c3", "c4", "c5", "c6"}).
			AddRow(float64(42), float64(1), float64(1), float64(1), float64(1), float64(1)))

	expectCascade5mErrorN(mock, 8) // org breakdown cascade FAILS
	mock.ExpectQuery(`GROUP BY org_id`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"org", "cost"}).AddRow("acme", float64(42)))

	expectCascade5mErrorN(mock, 8) // provider breakdown cascade FAILS
	mock.ExpectQuery(`COALESCE\(routed_provider_id`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"prov", "cost"}).AddRow("openai", float64(42)))

	c, rec := echoCtx("GET", "/api/admin/analytics/cost-summary")
	if err := h.AnalyticsCostSummary(c); err != nil {
		t.Fatalf("handler returned a transport error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: a rollup read failure has a correct answer "+
			"available on the direct path and must not be surfaced as an outage; body=%s",
			rec.Code, rec.Body.String())
	}
	if got := jsonBody(t, rec)["totalCostUsd"]; got != float64(42) {
		t.Errorf("totalCostUsd = %v, want 42 (the direct scan's answer); 0 means the "+
			"failed rollup was published as if it were a quiet month", got)
	}
}

// The FallsBackNot5xx test above deliberately does not discriminate this
// change: conflating a read error with an empty window ALSO fell back, so it
// stays green either way. What the split actually buys is the signal — a
// broken rollup leg used to send every request to a full traffic_event scan
// with nothing anywhere to say so. That is the thing worth pinning, and it is
// only observable through the log.
//
// A pair again: log on failure, SILENCE on an empty window. Logging both would
// make a fresh install (and every quiet month) emit an error per request,
// which is how a signal stops being one.
func TestRollupCostSummary_LogsTheFailureButNotTheQuietWindow(t *testing.T) {
	t.Parallel()

	run := func(t *testing.T, prime func(pgxmock.PgxPoolIface)) string {
		t.Helper()
		var buf bytes.Buffer
		mock, h := newMockHandler(t)
		h.logger = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
		prime(mock)
		if _, ok := h.rollupCostSummaryTotals(context.Background(),
			time.Now().Add(-time.Hour), time.Now()); ok {
			t.Fatal("both arms must report ok=false so the caller falls back")
		}
		return buf.String()
	}

	t.Run("read failure is logged", func(t *testing.T) {
		t.Parallel()
		out := run(t, func(m pgxmock.PgxPoolIface) { expectCascade5mErrorN(m, 7) })
		if !strings.Contains(out, "rollup totals read failed") {
			t.Errorf("a failed rollup read left no trace; the fallback is a full "+
				"traffic_event scan and nobody would know why. log=%q", out)
		}
	})

	t.Run("empty window is silent", func(t *testing.T) {
		t.Parallel()
		out := run(t, func(m pgxmock.PgxPoolIface) { expectCascade5mEmptyN(m, 7) })
		if strings.Contains(out, "rollup totals read failed") {
			t.Errorf("a quiet window is not a fault; logging it would emit an error per "+
				"request on a fresh install. log=%q", out)
		}
	})
}

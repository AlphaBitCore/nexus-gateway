package analytics

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"

	metrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/instruments"
)

// rollupCacheROITotals: 14 metrics + 2 time = 16 args

func TestRollupCacheROITotals_NoData(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	expectCascade5mEmptyN(mock, 16)
	_, ok := h.rollupCacheROITotals(context.Background(),
		time.Now().Add(-time.Hour), time.Now())
	if ok {
		t.Errorf("want false")
	}
}

func TestRollupCacheROITotals_QueryError(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	expectWatermarksMissing(mock)
	mock.ExpectQuery(`FROM "metric_rollup_5m"`).
		WithArgs(anyArgsUpTo(16)...).
		WillReturnError(errors.New("boom"))
	_, ok := h.rollupCacheROITotals(context.Background(),
		time.Now().Add(-time.Hour), time.Now())
	if ok {
		t.Errorf("want false on err")
	}
}

func TestRollupCacheROITotals_Happy(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	bucket := time.Now()
	rows := pgxmock.NewRows(rollupCols).
		AddRow("a", bucket, metrics.MetricEstimatedCostUSD, "", "", float64(50), []byte(nil), bucket).
		AddRow("b", bucket, metrics.MetricGatewayCacheSavingsUSD, "", "", float64(5), []byte(nil), bucket).
		AddRow("c", bucket, metrics.MetricCacheHitCount, "", "", float64(3), []byte(nil), bucket).
		AddRow("d", bucket, metrics.MetricCacheReadTokens, "", "", float64(200), []byte(nil), bucket)
	expectCascade5mRows(mock, rows, 16)
	got, ok := h.rollupCacheROITotals(context.Background(),
		time.Now().Add(-time.Hour), time.Now())
	if !ok {
		t.Fatal("want ok=true")
	}
	if got.TotalEstimatedCostUSD != 50 || got.TotalGatewayCacheSavingsUSD != 5 ||
		got.GatewayCacheHitCount != 3 || got.TotalCacheReadTokens != 200 {
		t.Errorf("got %+v", got)
	}
}

// rollupCacheROIDaily: 14 metrics + 2 = 16 (no dim)

func TestRollupCacheROIDaily_NoData(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	expectCascade5mEmptyN(mock, 16)
	got := h.rollupCacheROIDaily(context.Background(),
		time.Now().Add(-time.Hour), time.Now())
	if got != nil {
		t.Errorf("want nil, got %v", got)
	}
}

func TestRollupCacheROIDaily_QueryError(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	expectWatermarksMissing(mock)
	mock.ExpectQuery(`FROM "metric_rollup_5m"`).
		WithArgs(anyArgsUpTo(16)...).
		WillReturnError(errors.New("boom"))
	got := h.rollupCacheROIDaily(context.Background(),
		time.Now().Add(-time.Hour), time.Now())
	if got != nil {
		t.Errorf("want nil, got %v", got)
	}
}

func TestRollupCacheROIDaily_Happy(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	bucket := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	bucket2 := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	rows := pgxmock.NewRows(rollupCols).
		AddRow("a", bucket, metrics.MetricGatewayCacheSavingsUSD, "", "", float64(5), []byte(nil), bucket).
		AddRow("b", bucket, metrics.MetricCacheWriteCostUSD, "", "", float64(1), []byte(nil), bucket).
		AddRow("c", bucket, metrics.MetricCacheReadSavingsUSD, "", "", float64(3), []byte(nil), bucket).
		AddRow("d", bucket, metrics.MetricCacheNetSavingsUSD, "", "", float64(2), []byte(nil), bucket).
		AddRow("e", bucket, metrics.MetricCacheCreationTokens, "", "", float64(100), []byte(nil), bucket).
		AddRow("f", bucket, metrics.MetricCacheReadTokens, "", "", float64(50), []byte(nil), bucket).
		AddRow("g", bucket2, metrics.MetricCacheReadSavingsUSD, "", "", float64(2), []byte(nil), bucket2).
		// One non-allowed metric — skipped
		AddRow("h", bucket2, "some_other_metric", "", "", float64(99), []byte(nil), bucket2)
	expectCascade5mRows(mock, rows, 16)
	got := h.rollupCacheROIDaily(context.Background(),
		time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 5, 3, 0, 0, 0, 0, time.UTC))
	if len(got) != 2 {
		t.Fatalf("want 2 day rows, got %d", len(got))
	}
	if got[0].GatewayCacheSavingsUSD != 5 || got[0].CacheReadTokens != 50 {
		t.Errorf("day 1: %+v", got[0])
	}
}

// AnalyticsCacheROI — full handler integration

func TestAnalyticsCacheROI_NilDB(t *testing.T) {
	t.Parallel()
	h := &Handler{}
	c, rec := echoCtx("GET", "/api/admin/analytics/cache-roi")
	if err := h.AnalyticsCacheROI(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	assertStatus(t, rec, http.StatusInternalServerError)
}

// TestAnalyticsCacheROI_RollupHit exercises the full success path through
// rollup for totals, by-adapter, and daily.
func TestAnalyticsCacheROI_RollupHit(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	bucket := time.Now()

	// rollupCacheROITotals — 16 args
	totalRows := pgxmock.NewRows(rollupCols).
		AddRow("a", bucket, metrics.MetricEstimatedCostUSD, "", "", float64(100), []byte(nil), bucket).
		AddRow("b", bucket, metrics.MetricGatewayCacheSavingsUSD, "", "", float64(7), []byte(nil), bucket)
	expectCascade5mRows(mock, totalRows, 16)

	// adapter cascade — 14 metrics + 2 time + 1 dim = 17 args
	adapterRows := pgxmock.NewRows(rollupCols).
		AddRow("c", bucket, metrics.MetricEstimatedCostUSD, "routed_provider=prov-1", "",
			float64(60), []byte(nil), bucket).
		AddRow("d", bucket, metrics.MetricGatewayCacheSavingsUSD, "routed_provider=prov-1", "",
			float64(3), []byte(nil), bucket).
		AddRow("e", bucket, metrics.MetricEstimatedCostUSD, "routed_provider=", "",
			float64(10), []byte(nil), bucket) // empty provider id → skipped
	expectCascade5mRows(mock, adapterRows, 17)

	// fetchProviderAdapterTypes
	mock.ExpectQuery(`FROM "Provider"`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id", "adapter_type"}).
			AddRow("prov-1", "openai"))

	// rollupCacheROIDaily — 16 args
	dailyRows := pgxmock.NewRows(rollupCols).
		AddRow("dd", bucket, metrics.MetricGatewayCacheSavingsUSD, "", "", float64(3), []byte(nil), bucket)
	expectCascade5mRows(mock, dailyRows, 16)

	c, rec := echoCtx("GET", "/api/admin/analytics/cache-roi")
	if err := h.AnalyticsCacheROI(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	assertStatus(t, rec, http.StatusOK)
	body := jsonBody(t, rec)
	if body["dataSource"] != "rollup" {
		t.Errorf("dataSource=%v", body["dataSource"])
	}
	byA, _ := body["byAdapter"].([]any)
	// One row aggregates into "openai", and the empty-provider-id row
	// aggregates into "unknown" (the adapterByProv lookup returns "").
	if len(byA) != 2 {
		t.Errorf("byAdapter=%v (want 2)", byA)
	}
}

// TestAnalyticsCacheROI_DirectFallback exercises the path where rollup
// returns no rows → handler queries traffic_event directly.
func TestAnalyticsCacheROI_DirectFallback(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)

	// totals cascade empty
	expectCascade5mEmptyN(mock, 16)

	// direct totals query (14-column scan)
	mock.ExpectQuery(`FROM traffic_event\s+WHERE`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{
			"cost", "gws", "hits", "wc", "rs", "ns",
			"pt", "ct", "cc", "cr", "ns2", "nb", "mi", "rch",
		}).AddRow(
			float64(10), float64(2), int64(3), float64(0.5), float64(1.0), float64(0.5),
			int64(100), int64(50), int64(20), int64(10), int64(0), int64(0), int64(0), int64(2)))

	// adapter cascade empty
	expectCascade5mEmptyN(mock, 17)

	// direct adapter breakdown
	mock.ExpectQuery(`FROM traffic_event te\s+LEFT JOIN "Provider"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{
			"at", "cost", "gws", "hits", "wc", "rs", "ns", "pt", "ct", "cc", "cr", "rch",
		}).AddRow(
			"openai", float64(8), float64(1.5), int64(2), float64(0), float64(0.5), float64(0.5),
			int64(60), int64(30), int64(10), int64(5), int64(1)))

	// daily cascade empty
	expectCascade5mEmptyN(mock, 16)

	// direct daily query
	day := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`FROM traffic_event\s+WHERE timestamp >= \$1 AND timestamp < \$2.*GROUP BY 1`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{
			"day", "gws", "wc", "rs", "ns", "cc", "cr",
		}).AddRow(day, float64(2), float64(0), float64(0.5), float64(0.5), int64(10), int64(5)))

	c, rec := echoCtx("GET", "/api/admin/analytics/cache-roi")
	if err := h.AnalyticsCacheROI(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	assertStatus(t, rec, http.StatusOK)
	body := jsonBody(t, rec)
	if body["dataSource"] != "direct" {
		t.Errorf("dataSource=%v", body["dataSource"])
	}
	byA, _ := body["byAdapter"].([]any)
	if len(byA) != 1 {
		t.Errorf("byAdapter len=%d", len(byA))
	}
	daily, _ := body["daily"].([]any)
	if len(daily) != 1 {
		t.Errorf("daily len=%d", len(daily))
	}
}

func TestAnalyticsCacheROI_DirectTotalsError(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	expectCascade5mEmptyN(mock, 16)
	mock.ExpectQuery(`FROM traffic_event\s+WHERE`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(errors.New("boom"))
	c, rec := echoCtx("GET", "/api/admin/analytics/cache-roi")
	if err := h.AnalyticsCacheROI(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	assertStatus(t, rec, http.StatusInternalServerError)
}

func TestAnalyticsCacheROI_DirectAdapterError(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	expectCascade5mEmptyN(mock, 16)
	mock.ExpectQuery(`FROM traffic_event\s+WHERE`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{
			"cost", "gws", "hits", "wc", "rs", "ns",
			"pt", "ct", "cc", "cr", "ns2", "nb", "mi", "rch",
		}).AddRow(
			float64(0), float64(0), int64(0), float64(0), float64(0), float64(0),
			int64(0), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0)))
	expectCascade5mEmptyN(mock, 17)
	mock.ExpectQuery(`FROM traffic_event te\s+LEFT JOIN "Provider"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(errors.New("boom"))
	c, rec := echoCtx("GET", "/api/admin/analytics/cache-roi")
	if err := h.AnalyticsCacheROI(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	assertStatus(t, rec, http.StatusInternalServerError)
}

func TestAnalyticsCacheROI_DirectDailyError(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	expectCascade5mEmptyN(mock, 16)
	mock.ExpectQuery(`FROM traffic_event\s+WHERE`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{
			"cost", "gws", "hits", "wc", "rs", "ns",
			"pt", "ct", "cc", "cr", "ns2", "nb", "mi", "rch",
		}).AddRow(
			float64(0), float64(0), int64(0), float64(0), float64(0), float64(0),
			int64(0), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0)))
	expectCascade5mEmptyN(mock, 17)
	mock.ExpectQuery(`FROM traffic_event te\s+LEFT JOIN "Provider"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{
			"at", "cost", "gws", "hits", "wc", "rs", "ns", "pt", "ct", "cc", "cr", "rch",
		}))
	expectCascade5mEmptyN(mock, 16)
	mock.ExpectQuery(`FROM traffic_event\s+WHERE timestamp >= \$1 AND timestamp < \$2.*GROUP BY 1`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(errors.New("boom"))
	c, rec := echoCtx("GET", "/api/admin/analytics/cache-roi")
	if err := h.AnalyticsCacheROI(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	assertStatus(t, rec, http.StatusInternalServerError)
}

// TestAnalyticsCacheROI_DirectAdapterScanError covers the scan-error
// continuation in the direct adapter loop.
func TestAnalyticsCacheROI_DirectAdapterScanError(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	expectCascade5mEmptyN(mock, 16)
	mock.ExpectQuery(`FROM traffic_event\s+WHERE`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{
			"cost", "gws", "hits", "wc", "rs", "ns",
			"pt", "ct", "cc", "cr", "ns2", "nb", "mi", "rch",
		}).AddRow(
			float64(0), float64(0), int64(0), float64(0), float64(0), float64(0),
			int64(0), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0)))
	expectCascade5mEmptyN(mock, 17)
	// Row with wrong scan target type — loop continues.
	mock.ExpectQuery(`FROM traffic_event te\s+LEFT JOIN "Provider"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"only_one"}).AddRow("oops"))

	expectCascade5mEmptyN(mock, 16)
	mock.ExpectQuery(`FROM traffic_event\s+WHERE timestamp >= \$1 AND timestamp < \$2.*GROUP BY 1`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"day", "gws", "wc", "rs", "ns", "cc", "cr"}))

	c, rec := echoCtx("GET", "/api/admin/analytics/cache-roi")
	if err := h.AnalyticsCacheROI(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	assertStatus(t, rec, http.StatusOK)
}

// TestAnalyticsCacheROI_TotalsAreFleetWideMultiProvider is the F-0196 intent
// assertion: the top-level totals are a single FLEET-WIDE combined figure (one
// summed row, no provider dimension) while the byAdapter breakdown carries the
// per-provider split. A multi-provider tenant (OpenAI + Gemini) therefore sees
// one combined total AND two adapter rows — the two answer different questions
// and the totals are deliberately NOT per-provider.
func TestAnalyticsCacheROI_TotalsAreFleetWideMultiProvider(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)

	// totals cascade empty → direct totals path.
	expectCascade5mEmptyN(mock, 16)
	// Direct totals: one combined row summed across ALL providers by SQL
	// (estimated cost 30 = openai 18 + gemini 12). No provider dimension.
	mock.ExpectQuery(`FROM traffic_event\s+WHERE`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{
			"cost", "gws", "hits", "wc", "rs", "ns",
			"pt", "ct", "cc", "cr", "ns2", "nb", "mi", "rch",
		}).AddRow(
			float64(30), float64(5), int64(4), float64(2), float64(3), float64(1),
			int64(300), int64(150), int64(40), int64(20), int64(0), int64(0), int64(0), int64(4)))

	// adapter cascade empty → direct per-provider breakdown (two adapters).
	expectCascade5mEmptyN(mock, 17)
	mock.ExpectQuery(`FROM traffic_event te\s+LEFT JOIN "Provider"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{
			"at", "cost", "gws", "hits", "wc", "rs", "ns", "pt", "ct", "cc", "cr", "rch",
		}).
			AddRow("openai", float64(18), float64(3), int64(2), float64(1), float64(2), float64(1),
				int64(180), int64(90), int64(25), int64(12), int64(2)).
			AddRow("gemini", float64(12), float64(2), int64(2), float64(1), float64(1), float64(0),
				int64(120), int64(60), int64(15), int64(8), int64(2)))

	// daily cascade empty → direct daily (empty result is fine).
	expectCascade5mEmptyN(mock, 16)
	mock.ExpectQuery(`FROM traffic_event\s+WHERE timestamp >= \$1 AND timestamp < \$2.*GROUP BY 1`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"day", "gws", "wc", "rs", "ns", "cc", "cr"}))

	c, rec := echoCtx("GET", "/api/admin/analytics/cache-roi")
	if err := h.AnalyticsCacheROI(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	assertStatus(t, rec, http.StatusOK)
	body := jsonBody(t, rec)

	// Fleet-wide total: the single combined figure, not split by provider.
	if body["totalEstimatedCostUsd"] != float64(30) {
		t.Errorf("totalEstimatedCostUsd=%v want 30 (fleet-wide combined across providers)", body["totalEstimatedCostUsd"])
	}
	// Per-provider attribution lives in byAdapter, and the two adapter rows must
	// sum back to the fleet-wide total — proving totals == Σ(per-provider).
	byA, _ := body["byAdapter"].([]any)
	if len(byA) != 2 {
		t.Fatalf("byAdapter=%v want 2 adapter rows (openai + gemini)", byA)
	}
	var adapterCostSum float64
	for _, r := range byA {
		m, _ := r.(map[string]any)
		v, _ := m["estimatedCostUsd"].(float64)
		adapterCostSum += v
	}
	if adapterCostSum != body["totalEstimatedCostUsd"] {
		t.Errorf("Σ(byAdapter cost)=%v != fleet-wide total %v — aggregation inconsistent", adapterCostSum, body["totalEstimatedCostUsd"])
	}
}

// TestAnalyticsCacheROI_DirectFallback_NullProviderReconciliation is the
// F-0196 regression test: when traffic_event rows have NULL provider_id
// (compliance-proxy / agent / errored-before-routing traffic), the LEFT JOIN
// must route them to the "unknown" bucket instead of dropping them.
// Invariant: Σ(byAdapter cost) == fleet-wide totalEstimatedCostUsd.
func TestAnalyticsCacheROI_DirectFallback_NullProviderReconciliation(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)

	// totals cascade empty → direct path.
	expectCascade5mEmptyN(mock, 16)
	// Fleet-wide total = 25 (10 openai + 15 null-provider).
	mock.ExpectQuery(`FROM traffic_event\s+WHERE`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{
			"cost", "gws", "hits", "wc", "rs", "ns",
			"pt", "ct", "cc", "cr", "ns2", "nb", "mi", "rch",
		}).AddRow(
			float64(25), float64(3), int64(1), float64(0), float64(0), float64(0),
			int64(0), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0)))

	// adapter cascade empty → direct adapter breakdown via LEFT JOIN.
	expectCascade5mEmptyN(mock, 17)
	// Two rows: one real adapter row + one "unknown" row for NULL provider_id.
	mock.ExpectQuery(`FROM traffic_event te\s+LEFT JOIN "Provider"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{
			"at", "cost", "gws", "hits", "wc", "rs", "ns", "pt", "ct", "cc", "cr", "rch",
		}).
			AddRow("openai", float64(10), float64(2), int64(1), float64(0), float64(0), float64(0),
				int64(0), int64(0), int64(0), int64(0), int64(0)).
			AddRow("unknown", float64(15), float64(1), int64(0), float64(0), float64(0), float64(0),
				int64(0), int64(0), int64(0), int64(0), int64(0)))

	// daily cascade empty + direct daily (empty result is fine).
	expectCascade5mEmptyN(mock, 16)
	mock.ExpectQuery(`FROM traffic_event\s+WHERE timestamp >= \$1 AND timestamp < \$2.*GROUP BY 1`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"day", "gws", "wc", "rs", "ns", "cc", "cr"}))

	c, rec := echoCtx("GET", "/api/admin/analytics/cache-roi")
	if err := h.AnalyticsCacheROI(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	assertStatus(t, rec, http.StatusOK)
	body := jsonBody(t, rec)

	fleetTotal, _ := body["totalEstimatedCostUsd"].(float64)
	if fleetTotal != 25 {
		t.Errorf("totalEstimatedCostUsd=%v want 25", fleetTotal)
	}

	byA, _ := body["byAdapter"].([]any)
	// Must have two rows: "openai" and "unknown".
	if len(byA) != 2 {
		t.Fatalf("byAdapter len=%d want 2 (openai + unknown)", len(byA))
	}

	// Reconciliation: Σ(byAdapter cost) must equal the fleet-wide total.
	var adapterCostSum float64
	for _, r := range byA {
		m, _ := r.(map[string]any)
		v, _ := m["estimatedCostUsd"].(float64)
		adapterCostSum += v
	}
	if adapterCostSum != fleetTotal {
		t.Errorf("F-0196 reconciliation FAIL: Σ(byAdapter cost)=%v != fleetTotal=%v — null-provider rows dropped by INNER JOIN", adapterCostSum, fleetTotal)
	}

	// Confirm the "unknown" bucket exists and holds the NULL-provider traffic.
	var unknownCost float64
	for _, r := range byA {
		m, _ := r.(map[string]any)
		if m["adapter"] == "unknown" {
			unknownCost, _ = m["estimatedCostUsd"].(float64)
		}
	}
	if unknownCost != 15 {
		t.Errorf("unknown bucket cost=%v want 15", unknownCost)
	}
}

// TestAnalyticsCacheROI_WithExplicitRange exercises the start/end window
// override path so the periodDays calc + Since/Until stamping run.
func TestAnalyticsCacheROI_WithExplicitRange(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	expectCascade5mEmptyN(mock, 16)
	mock.ExpectQuery(`FROM traffic_event\s+WHERE`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{
			"cost", "gws", "hits", "wc", "rs", "ns",
			"pt", "ct", "cc", "cr", "ns2", "nb", "mi", "rch",
		}).AddRow(
			float64(0), float64(0), int64(0), float64(0), float64(0), float64(0),
			int64(0), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0)))
	expectCascade5mEmptyN(mock, 17)
	mock.ExpectQuery(`FROM traffic_event te\s+LEFT JOIN "Provider"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{
			"at", "cost", "gws", "hits", "wc", "rs", "ns", "pt", "ct", "cc", "cr", "rch",
		}))
	expectCascade5mEmptyN(mock, 16)
	mock.ExpectQuery(`FROM traffic_event\s+WHERE timestamp >= \$1 AND timestamp < \$2.*GROUP BY 1`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"day", "gws", "wc", "rs", "ns", "cc", "cr"}))

	c, rec := echoCtx("GET",
		"/api/admin/analytics/cache-roi?start=2026-05-01T00:00:00Z&end=2026-05-05T00:00:00Z")
	if err := h.AnalyticsCacheROI(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	assertStatus(t, rec, http.StatusOK)
	body := jsonBody(t, rec)
	if body["periodDays"] != float64(5) {
		t.Errorf("periodDays=%v", body["periodDays"])
	}
}

package analytics

import (
	"context"
	"github.com/goccy/go-json"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"
	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/store"
	metrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/instruments"
)

// Pure helpers: rollupDimensionForGroupKey, applyTopN, formatFloat,
// buildProviderDim, rollupDefaultTimeRange, sourceSubDimension,
// sourceFilterParam, rollupSubDimensionForGroupKey

func TestRollupDimensionForGroupKey_Map(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"provider":       "routed_provider",
		"modelUsed":      "model",
		"projectId":      "project",
		"organizationId": "organization",
		"userId":         "user",
		"virtualKeyId":   "virtual_key",
		"routedProvider": "routed_provider",
		"routingRuleId":  "routing_rule",
		"targetHost":     "target_host",
		"host":           "target_host",
		"deviceId":       "device",
	}
	for k, want := range cases {
		got := rollupDimensionForGroupKey[k]
		if got != want {
			t.Errorf("rollupDimensionForGroupKey[%q] = %q, want %q", k, got, want)
		}
	}
}

func TestApplyTopN_NoTrim(t *testing.T) {
	t.Parallel()
	in := []store.GroupByResult{{Group: "a", RequestCount: 1}, {Group: "b", RequestCount: 2}}
	got := applyTopN(in, 5)
	if len(got) != 2 {
		t.Errorf("len=%d, want 2", len(got))
	}
}

func TestApplyTopN_Trims(t *testing.T) {
	t.Parallel()
	in := []store.GroupByResult{
		{Group: "a", RequestCount: 1, TotalTokens: 10, TotalPromptTokens: 5, TotalCompletionTokens: 5, TotalCostUsd: 1, TotalEstimatedCostUsd: 1},
		{Group: "b", RequestCount: 9, TotalTokens: 90},
		{Group: "c", RequestCount: 4, TotalTokens: 40},
		{Group: "d", RequestCount: 7, TotalTokens: 70},
	}
	got := applyTopN(in, 2)
	if len(got) != 3 {
		t.Fatalf("len=%d, want 3 (top 2 + Other)", len(got))
	}
	if got[2].Group != "Other" {
		t.Errorf("last group=%v, want Other", got[2].Group)
	}
	if got[2].RequestCount != 5 { // 1 + 4
		t.Errorf("Other.RequestCount=%d, want 5", got[2].RequestCount)
	}
}

func TestFormatFloat(t *testing.T) {
	t.Parallel()
	if formatFloat(42.5) != "42.5" {
		t.Errorf("formatFloat=%q", formatFloat(42.5))
	}
	if formatFloat(0) != "0" {
		t.Errorf("formatFloat(0)=%q", formatFloat(0))
	}
}

func TestBuildProviderDim(t *testing.T) {
	t.Parallel()
	out := buildProviderDim("p1", "OpenAI")
	var m map[string]string
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if m["provider"] != "p1" || m["providerLabel"] != "OpenAI" {
		t.Errorf("got %v", m)
	}
}

func TestRollupDefaultTimeRange(t *testing.T) {
	t.Parallel()
	s := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	e := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	gotS, gotE := rollupDefaultTimeRange(&s, &e, time.UTC)
	if !gotS.Equal(s) || !gotE.Equal(e) {
		t.Errorf("provided range mangled")
	}
	// Both nil → roughly last 365 days.
	gotS, gotE = rollupDefaultTimeRange(nil, nil, time.UTC)
	if gotE.Sub(gotS) < 360*24*time.Hour {
		t.Errorf("default span too small: %v", gotE.Sub(gotS))
	}
}

func TestSourceSubDimension_All(t *testing.T) {
	t.Parallel()
	tests := []struct {
		src  string
		want string
	}{
		{"vk", "source=vk"},
		{"proxy", "source=proxy"},
		{"agent", "source=agent"},
		{"", ""},
		{"junk", ""},
	}
	for _, tc := range tests {
		c, _ := echoCtx("GET", "/?source="+tc.src)
		got := sourceSubDimension(c)
		if got != tc.want {
			t.Errorf("source=%q got=%q want=%q", tc.src, got, tc.want)
		}
	}
}

func TestSourceFilterParam_All(t *testing.T) {
	t.Parallel()
	tests := []struct {
		src  string
		want string
	}{
		{"vk", "ai-gateway"},
		{"ai-gateway", "ai-gateway"},
		{"proxy", "compliance-proxy"},
		{"compliance-proxy", "compliance-proxy"},
		{"agent", "agent"},
		{"", "all"},
		{"other", "all"},
	}
	for _, tc := range tests {
		c, _ := echoCtx("GET", "/?source="+tc.src)
		if got := sourceFilterParam(c); got != tc.want {
			t.Errorf("source=%q got=%q want=%q", tc.src, got, tc.want)
		}
	}
}

func TestRollupSubDimensionForGroupKey(t *testing.T) {
	t.Parallel()
	c, _ := echoCtx("GET", "/?source=vk")
	if got := rollupSubDimensionForGroupKey(c, "modelUsed"); got != "source=vk" {
		t.Errorf("got %q", got)
	}
}

// rollupGroupsToGroupByResults: covers all three sumFields branches and the
// ParseDimensionKey path

func TestRollupGroupsToGroupByResults_Tokens(t *testing.T) {
	t.Parallel()
	_, h := newMockHandler(t)
	groups := []metrics.MetricsGroup{
		{
			DimensionKey: "model=", // empty value — skipped from label fetch
			Values: map[string]float64{
				metrics.MetricRequestCount:     7,
				metrics.MetricPromptTokens:     20,
				metrics.MetricCompletionTokens: 10,
				metrics.MetricTotalTokens:      30,
			},
		},
	}
	out := h.rollupGroupsToGroupByResults(context.Background(), "model", groups, "tokens")
	if len(out) != 1 || out[0].RequestCount != 7 || out[0].TotalTokens != 30 {
		t.Errorf("got %+v", out)
	}
}

func TestRollupGroupsToGroupByResults_Cost(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	// Provide a label-resolution row so the JOIN to Model returns a label.
	mock.ExpectQuery(`FROM "Model"`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id", "name"}).AddRow("m1", "gpt-4"))

	groups := []metrics.MetricsGroup{
		{
			DimensionKey: "model=m1",
			Values: map[string]float64{
				metrics.MetricRequestCount:      2,
				metrics.MetricEstimatedCostUSD:  4.0,
				metrics.MetricTotalTokens:       40,
				metrics.MetricCacheHitCount:     1,
				metrics.MetricCacheSavedCostUSD: 0.5,
			},
		},
	}
	out := h.rollupGroupsToGroupByResults(context.Background(), "model", groups, "cost")
	if len(out) != 1 {
		t.Fatalf("want 1, got %d", len(out))
	}
	if out[0].TotalCostUsd != 4.0 || out[0].CacheHitCount != 1 || out[0].GatewayCacheSavingsUsd != 0.5 {
		t.Errorf("cost mapping wrong: %+v", out[0])
	}
	if out[0].GroupLabel != "gpt-4" {
		t.Errorf("label not stamped: %+v", out[0])
	}
}

func TestRollupGroupsToGroupByResults_Provider(t *testing.T) {
	t.Parallel()
	_, h := newMockHandler(t)
	groups := []metrics.MetricsGroup{
		{
			DimensionKey: "routed_provider=", // empty val skipped
			Values: map[string]float64{
				metrics.MetricLatencySum:       100,
				metrics.MetricLatencyCount:     5,
				metrics.MetricRequestCount:     3,
				metrics.MetricTotalTokens:      300,
				metrics.MetricEstimatedCostUSD: 6.0,
			},
		},
	}
	out := h.rollupGroupsToGroupByResults(context.Background(), "routed_provider", groups, "provider")
	if out[0].AvgLatencyMs != 20.0 || out[0].TotalEstimatedCostUsd != 6.0 {
		t.Errorf("provider mapping wrong: %+v", out[0])
	}
}

// queryAnalyticsPhasePercentiles, queryByProviderPhasePercentiles

func TestQueryAnalyticsPhasePercentiles_Happy(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	a, b, c := 10, 20, 30
	mock.ExpectQuery(`FROM   traffic_event`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"us", "ttfb", "total"}).AddRow(&a, &b, &c))
	us, ttfb, total, ok := h.queryAnalyticsPhasePercentiles(context.Background(),
		time.Now().Add(-time.Hour), time.Now(), "all")
	if !ok || us == nil || *us != 10 || ttfb == nil || *ttfb != 20 || total == nil || *total != 30 {
		t.Errorf("got us=%v ttfb=%v total=%v ok=%v", us, ttfb, total, ok)
	}
}

func TestQueryAnalyticsPhasePercentiles_WithSourceFilter(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	a, b, c := 1, 2, 3
	mock.ExpectQuery(`FROM   traffic_event`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), "ai-gateway").
		WillReturnRows(pgxmock.NewRows([]string{"us", "ttfb", "total"}).AddRow(&a, &b, &c))
	_, _, _, ok := h.queryAnalyticsPhasePercentiles(context.Background(),
		time.Now().Add(-time.Hour), time.Now(), "ai-gateway")
	if !ok {
		t.Error("want ok=true")
	}
}

func TestQueryAnalyticsPhasePercentiles_ErrFails(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	mock.ExpectQuery(`FROM   traffic_event`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(pgx.ErrNoRows)
	_, _, _, ok := h.queryAnalyticsPhasePercentiles(context.Background(),
		time.Now().Add(-time.Hour), time.Now(), "all")
	if ok {
		t.Error("want ok=false on error")
	}
}

func TestQueryByProviderPhasePercentiles_Happy(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	a, b, c := 10, 20, 30
	mock.ExpectQuery(`FROM   traffic_event`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"provider", "us", "ttfb", "total"}).
			AddRow("OpenAI", &a, &b, &c))
	got := h.queryByProviderPhasePercentiles(context.Background(),
		time.Now().Add(-time.Hour), time.Now(), "all")
	if got["OpenAI"].UsP95 == nil || *got["OpenAI"].UsP95 != 10 {
		t.Errorf("got %+v", got)
	}
}

func TestQueryByProviderPhasePercentiles_ErrorReturnsNil(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	mock.ExpectQuery(`FROM   traffic_event`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(pgx.ErrNoRows)
	got := h.queryByProviderPhasePercentiles(context.Background(),
		time.Now().Add(-time.Hour), time.Now(), "all")
	if got != nil {
		t.Errorf("want nil, got %v", got)
	}
}

func TestQueryByProviderPhasePercentiles_SourceFilter(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	mock.ExpectQuery(`FROM   traffic_event`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), "agent").
		WillReturnRows(pgxmock.NewRows([]string{"provider", "us", "ttfb", "total"}))
	got := h.queryByProviderPhasePercentiles(context.Background(),
		time.Now().Add(-time.Hour), time.Now(), "agent")
	if got == nil || len(got) != 0 {
		t.Errorf("want empty non-nil map, got %v", got)
	}
}

func TestQueryByProviderPhasePercentiles_ScanError(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	// Wrong column count → scan fails; loop continues.
	mock.ExpectQuery(`FROM   traffic_event`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"only"}).AddRow("oops"))
	got := h.queryByProviderPhasePercentiles(context.Background(),
		time.Now().Add(-time.Hour), time.Now(), "all")
	if len(got) != 0 {
		t.Errorf("want empty, got %v", got)
	}
}

func TestQueryMetricsOrFallback_TimeSeriesPath(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	// QueryRollupAware: at 1d granularity it calls GetWatermark("merge-1d"),
	// then querys metric_rollup_1d (coarse). With ErrNoRows watermark it
	// falls back to coarse-only. The coarse-only query has 3 args:
	// from, to, MetricRequestCount.
	// 7-day span → 1h table.
	mock.ExpectQuery(`FROM "rollup_watermark"`).
		WithArgs("merge-1h").
		WillReturnError(pgx.ErrNoRows)
	bucket := time.Now()
	mock.ExpectQuery(`FROM "metric_rollup_1h"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(rollupCols).
			AddRow("id1", bucket, metrics.MetricRequestCount, "", "", float64(5), []byte(nil), bucket))

	q := metrics.MetricsQuery{
		Metrics:    []string{metrics.MetricRequestCount},
		StartTime:  time.Now().AddDate(0, 0, -7),
		EndTime:    time.Now(),
		TimeSeries: true,
	}
	res, err := h.queryMetricsOrFallback(context.Background(), q)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res == nil {
		t.Fatalf("want non-nil result")
	}
}

func TestQueryMetricsOrFallback_NoData(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	expectCascadeAllEmpty(mock)
	q := metrics.MetricsQuery{Metrics: []string{metrics.MetricRequestCount},
		StartTime: time.Now().Add(-time.Hour), EndTime: time.Now()}
	res, err := h.queryMetricsOrFallback(context.Background(), q)
	if err != nil || res != nil {
		t.Errorf("want nil/nil, got %v / %v", res, err)
	}
}

func TestTryRollupSummary_NoData(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	expectCascadeAllEmpty(mock)
	c, _ := echoCtx("GET", "/?start=2026-01-01T00:00:00Z&end=2026-01-02T00:00:00Z")
	if h.tryRollupSummary(c) {
		t.Error("want false on empty rollup")
	}
}

func TestTryRollupSummary_HappyAndPhases(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	bucket := time.Now()
	// summary cascade: only the 5m segment matters since no watermarks.
	// tryRollupSummary issues 11 metrics + 2 time = 13 positional args.
	expectCascade5mRows(mock, pgxmock.NewRows(rollupCols).
		AddRow("a", bucket, metrics.MetricRequestCount, "", "", float64(100), []byte(nil), bucket).
		AddRow("b", bucket, metrics.MetricStatus4xxCount, "", "", float64(2), []byte(nil), bucket).
		AddRow("c", bucket, metrics.MetricStatus5xxCount, "", "", float64(1), []byte(nil), bucket).
		AddRow("d", bucket, metrics.MetricCacheHitCount, "", "", float64(10), []byte(nil), bucket).
		AddRow("e", bucket, metrics.MetricEstimatedCostUSD, "", "", float64(5), []byte(nil), bucket).
		AddRow("f", bucket, metrics.MetricLatencySum, "", "", float64(200), []byte(nil), bucket).
		AddRow("g", bucket, metrics.MetricLatencyCount, "", "", float64(20), []byte(nil), bucket).
		AddRow("h", bucket, metrics.MetricTotalTokens, "", "", float64(1000), []byte(nil), bucket), 13)

	// Phase percentiles query (queryAnalyticsPhasePercentiles)
	a, b, cVal := 12, 24, 36
	mock.ExpectQuery(`FROM   traffic_event`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"us", "ttfb", "total"}).AddRow(&a, &b, &cVal))

	c, rec := echoCtx("GET", "/?start=2026-01-01T00:00:00Z&end=2026-01-02T00:00:00Z")
	if !h.tryRollupSummary(c) {
		t.Fatal("want true")
	}
	body := jsonBody(t, rec)
	if body["totalRequests"] != float64(100) {
		t.Errorf("totalRequests=%v", body["totalRequests"])
	}
	if body["errorCount"] != float64(3) {
		t.Errorf("errorCount=%v want 3", body["errorCount"])
	}
}

// tryRollupGroupBy + tryRollupByProvider + tryRollupRouting +
// tryRollupRoutingFallbacks + tryRollupCostReport + tryRollupQuality

func TestTryRollupGroupBy_UnknownKey(t *testing.T) {
	t.Parallel()
	_, h := newMockHandler(t)
	c, _ := echoCtx("GET", "/")
	_, ok := h.tryRollupGroupBy(c, "nonsense", "tokens")
	if ok {
		t.Error("want false for unknown groupKey")
	}
}

func TestTryRollupGroupBy_NoData(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	// tokens: 4 metrics + 2 time + 1 dim = 7
	expectCascade5mEmptyN(mock, 7)
	c, _ := echoCtx("GET", "/")
	_, ok := h.tryRollupGroupBy(c, "modelUsed", "tokens")
	if ok {
		t.Error("want false when rollup empty")
	}
}

func TestTryRollupGroupBy_HappyTokens(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	bucket := time.Now()
	rows := pgxmock.NewRows(rollupCols).
		AddRow("a", bucket, metrics.MetricRequestCount, "model=m1", "", float64(5),
			[]byte(nil), bucket)
	expectCascade5mRows(mock, rows, 7)
	// label resolve for model
	mock.ExpectQuery(`FROM "Model"`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id", "name"}).AddRow("m1", "gpt-4"))
	c, _ := echoCtx("GET", "/")
	got, ok := h.tryRollupGroupBy(c, "modelUsed", "tokens")
	if !ok || len(got) != 1 {
		t.Fatalf("ok=%v got=%v", ok, got)
	}
}

func TestTryRollupGroupBy_DefaultMetrics(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	// sumFields="other" → only MetricRequestCount in the metric set.
	// 1 metric + 2 time + 1 dim = 4
	bucket := time.Now()
	rows := pgxmock.NewRows(rollupCols).
		AddRow("a", bucket, metrics.MetricRequestCount, "model=m1", "", float64(2),
			[]byte(nil), bucket)
	expectCascade5mRows(mock, rows, 4)
	mock.ExpectQuery(`FROM "Model"`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id", "name"}).AddRow("m1", "gpt-4"))
	c, _ := echoCtx("GET", "/")
	_, ok := h.tryRollupGroupBy(c, "modelUsed", "other-default")
	if !ok {
		t.Error("want true")
	}
}

func TestTryRollupByProvider_NoData(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	// 5 metrics + 2 time + 1 dim = 8
	expectCascade5mEmptyN(mock, 8)
	c, _ := echoCtx("GET", "/")
	if h.tryRollupByProvider(c) {
		t.Error("want false when rollup empty")
	}
}

func TestTryRollupByProvider_Happy(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	bucket := time.Now()
	rows := pgxmock.NewRows(rollupCols).
		AddRow("a", bucket, metrics.MetricRequestCount, "routed_provider=p1", "", float64(5),
			[]byte(nil), bucket)
	expectCascade5mRows(mock, rows, 8)
	// rollupGroupsToGroupByResults will look up label for the provider dim.
	mock.ExpectQuery(`FROM "Provider"`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id", "name"}).AddRow("p1", "OpenAI"))
	// queryByProviderPhasePercentiles fires; return one row matching by label.
	x, y, z := 10, 20, 30
	mock.ExpectQuery(`FROM   traffic_event`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"provider", "us", "ttfb", "total"}).
			AddRow("OpenAI", &x, &y, &z))

	c, rec := echoCtx("GET", "/")
	if !h.tryRollupByProvider(c) {
		t.Fatal("want true")
	}
	body := jsonBody(t, rec)
	data, _ := body["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("data len=%d", len(data))
	}
	row := data[0].(map[string]any)
	if row["usOverheadP95Ms"] != float64(10) {
		t.Errorf("usOverheadP95Ms=%v", row["usOverheadP95Ms"])
	}
}

func TestTryRollupRouting_NoData(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	// 1 metric + 2 time + 1 dim = 4
	expectCascade5mEmptyN(mock, 4)
	c, _ := echoCtx("GET", "/")
	got := h.tryRollupRouting(c)
	if got != nil {
		t.Errorf("want nil, got %v", got)
	}
}

func TestTryRollupRouting_Happy(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	bucket := time.Now()
	rows := pgxmock.NewRows(rollupCols).
		AddRow("a", bucket, metrics.MetricRequestCount, "routed_provider=p1", "", float64(5),
			[]byte(nil), bucket)
	expectCascade5mRows(mock, rows, 4)
	c, _ := echoCtx("GET", "/")
	got := h.tryRollupRouting(c)
	if len(got) != 1 || got[0].RequestCount != 5 {
		t.Errorf("got %+v", got)
	}
}

func TestTryRollupRoutingFallbacks_NoData(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	// 1 metric + 2 time + 1 dim = 4
	expectCascade5mEmptyN(mock, 4)
	c, _ := echoCtx("GET", "/")
	if got := h.tryRollupRoutingFallbacks(c); got != nil {
		t.Errorf("want nil, got %v", got)
	}
}

func TestTryRollupRoutingFallbacks_Happy(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	bucket := time.Now()
	rows := pgxmock.NewRows(rollupCols).
		AddRow("a", bucket, metrics.MetricRoutingRuleHit, "routing_rule=r1", "", float64(7),
			[]byte(nil), bucket)
	expectCascade5mRows(mock, rows, 4)
	c, _ := echoCtx("GET", "/")
	got := h.tryRollupRoutingFallbacks(c)
	if len(got) != 1 || got[0].RequestCount != 7 {
		t.Errorf("got %+v", got)
	}
}

func TestTryRollupCostReport_NoData(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	// 3 metrics + 2 time + 1 dim = 6
	expectCascade5mEmptyN(mock, 6)
	c, _ := echoCtx("GET", "/")
	if got := h.tryRollupCostReport(c); got != nil {
		t.Errorf("want nil, got %v", got)
	}
}

func TestTryRollupCostReport_Happy(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	bucket := time.Now()
	rows := pgxmock.NewRows(rollupCols).
		AddRow("a", bucket, metrics.MetricEstimatedCostUSD, "organization=o1", "", float64(15),
			[]byte(nil), bucket)
	expectCascade5mRows(mock, rows, 6)
	mock.ExpectQuery(`FROM "Organization"`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id", "name"}).AddRow("o1", "Acme"))
	c, _ := echoCtx("GET", "/")
	got := h.tryRollupCostReport(c)
	if len(got) != 1 || got[0].TotalCostUsd != 15 {
		t.Errorf("got %+v", got)
	}
}

func TestTryRollupQuality_NoData(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	// 2 metrics + 2 time = 4
	expectCascade5mEmptyN(mock, 4)
	c, _ := echoCtx("GET", "/")
	if h.tryRollupQuality(c) {
		t.Error("want false when empty")
	}
}

func TestTryRollupQuality_Happy(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	bucket := time.Now()
	expectCascade5mRows(mock, pgxmock.NewRows(rollupCols).
		AddRow("a", bucket, metrics.MetricStatus2xxCount, "", "", float64(100), []byte(nil), bucket).
		AddRow("b", bucket, metrics.MetricQualityAnomalyCount, "", "", float64(2), []byte(nil), bucket), 4)

	c, rec := echoCtx("GET", "/")
	if !h.tryRollupQuality(c) {
		t.Fatal("want true")
	}
	body := jsonBody(t, rec)
	if body["totalResponses"] != float64(100) || body["anomalyCount"] != float64(2) {
		t.Errorf("got %v", body)
	}
	if body["anomalyRate"] != 0.02 {
		t.Errorf("anomalyRate=%v", body["anomalyRate"])
	}
}

func TestTryRollupMetricsAggregates_NoData(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	// QueryRollup auto-selects granularity from the range. With no specific
	// range a 365d default lookback gets selected — that's metric_rollup_1d.
	// 16 metrics + 2 time = 18 args (no dim).
	mock.ExpectQuery(`FROM "metric_rollup_1d"`).
		WithArgs(matchManyArgs(18)...).
		WillReturnRows(pgxmock.NewRows(rollupCols))
	c, _ := echoCtx("GET", "/")
	if h.tryRollupMetricsAggregates(c) {
		t.Error("want false on empty")
	}
}

func TestTryRollupMetricsAggregates_Happy(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	bucket := time.Now()
	// First QueryRollup — global summary on 1d table.
	// 16 metrics + 2 time = 18 args.
	mock.ExpectQuery(`FROM "metric_rollup_1d"`).
		WithArgs(matchManyArgs(18)...).
		WillReturnRows(pgxmock.NewRows(rollupCols).
			AddRow("a", bucket, metrics.MetricRequestCount, "", "", float64(50), []byte(nil), bucket).
			AddRow("b", bucket, metrics.MetricStatus4xxCount, "", "", float64(3), []byte(nil), bucket).
			AddRow("c", bucket, metrics.MetricTotalTokens, "", "", float64(2000), []byte(nil), bucket).
			AddRow("d", bucket, metrics.MetricEstimatedCostUSD, "", "", float64(7.5), []byte(nil), bucket))
	// Second QueryRollup — per-provider 1d table.
	// 4 metrics + 2 time + 1 dim = 7 args.
	mock.ExpectQuery(`FROM "metric_rollup_1d"`).
		WithArgs(matchManyArgs(7)...).
		WillReturnRows(pgxmock.NewRows(rollupCols).
			AddRow("p1r", bucket, metrics.MetricRequestCount, "routed_provider=prov-1", "", float64(20),
				[]byte(nil), bucket).
			AddRow("p1t", bucket, metrics.MetricTotalTokens, "routed_provider=prov-1", "", float64(800),
				[]byte(nil), bucket).
			AddRow("p1c", bucket, metrics.MetricEstimatedCostUSD, "routed_provider=prov-1", "", float64(2.5),
				[]byte(nil), bucket))
	// resolveDimensionLabels for provider
	mock.ExpectQuery(`FROM "Provider"`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id", "name"}).AddRow("prov-1", "OpenAI"))

	c, rec := echoCtx("GET", "/")
	if !h.tryRollupMetricsAggregates(c) {
		t.Fatal("want true")
	}
	body := jsonBody(t, rec)
	data, _ := body["data"].([]any)
	if len(data) == 0 {
		t.Errorf("expected data rows, got body=%s", rec.Body.String())
	}
}

// matchManyArgs returns n pgxmock.AnyArg() values for ExpectQuery.WithArgs
// when the underlying SQL has many positional params we don't care about.
func matchManyArgs(n int) []any {
	out := make([]any, n)
	for i := range out {
		out[i] = pgxmock.AnyArg()
	}
	return out
}

// RegisterRoutes assertion — calling RegisterRoutes should not panic, and
// the group should accept the canonical mount names.
func TestRegisterRoutes_MountsAll(t *testing.T) {
	t.Parallel()
	_, h := newMockHandler(t)
	e := echo.New()
	g := e.Group("/api/admin")
	h.RegisterAnalyticsRoutes(g, iamMWNoop)
	h.RegisterMetricsRoutes(g, iamMWNoop)
	// If we got here, no panic.
	_ = http.StatusOK
}

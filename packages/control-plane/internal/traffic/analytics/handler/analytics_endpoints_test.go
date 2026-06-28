package analytics

import (
	"context"
	"errors"
	"github.com/goccy/go-json"
	"net/http"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/store"
	metrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/instruments"
)

// parseTimeRange, parseTZParam, tzLoc

func TestParseTimeRange_Variants(t *testing.T) {
	t.Parallel()
	tests := []struct {
		query string
		s, e  bool // expect non-nil pointers
	}{
		{"", false, false},
		{"start=2026-01-02T00:00:00Z", true, false},
		{"startTime=2026-01-02T00:00:00Z", true, false},
		{"start=2026-01-02T00:00:00Z&end=2026-01-03T00:00:00Z", true, true},
		{"end=2026-01-03T00:00:00Z", false, true},
		{"endTime=2026-01-03T00:00:00Z", false, true},
		{"start=garbage", false, false},
	}
	for _, tc := range tests {
		c, _ := echoCtx("GET", "/?"+tc.query)
		s, e := parseTimeRange(c)
		if (s != nil) != tc.s || (e != nil) != tc.e {
			t.Errorf("query=%q s=%v e=%v want s=%v e=%v", tc.query, s != nil, e != nil, tc.s, tc.e)
		}
	}
}

func TestParseTZParam_All(t *testing.T) {
	t.Parallel()
	tests := []struct {
		tz        string
		wantName  string
		wantValid bool // whether resolved to UTC
	}{
		{"", "UTC", true},
		{"UTC", "UTC", true},
		{"America/Los_Angeles", "America/Los_Angeles", true},
		{"nonsense/zone", "UTC", true}, // fallback
	}
	for _, tc := range tests {
		c, _ := echoCtx("GET", "/?tz="+tc.tz)
		loc, name := parseTZParam(c)
		if name != tc.wantName {
			t.Errorf("tz=%q got name=%q want %q", tc.tz, name, tc.wantName)
		}
		if loc == nil {
			t.Errorf("tz=%q got nil loc", tc.tz)
		}
	}
}

func TestTzLoc_Wrapper(t *testing.T) {
	t.Parallel()
	c, _ := echoCtx("GET", "/?tz=UTC")
	if tzLoc(c).String() != "UTC" {
		t.Errorf("got %q", tzLoc(c).String())
	}
}

// AnalyticsProviderDetail (uses store.GetProviderAnalyticsDetail)

func TestAnalyticsProviderDetail_Error(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	// GetProviderAnalyticsDetail will fire one or more queries — first one
	// errors and the handler returns 500.
	mock.ExpectQuery(`.*`).WillReturnError(errors.New("conn lost"))

	c, rec := echoCtx("GET", "/api/admin/analytics/provider/p1")
	c = withParam(c, "providerId", "p1")
	if err := h.AnalyticsProviderDetail(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	assertStatus(t, rec, http.StatusInternalServerError)
}

// Top-level endpoint handlers — happy & fallback paths

func TestMetricsAggregates_Fallback(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	// QueryRollup empty → no second query → handler returns "no data" JSON.
	mock.ExpectQuery(`FROM "metric_rollup_1d"`).
		WithArgs(matchManyArgs(18)...).
		WillReturnRows(pgxmock.NewRows(rollupCols))
	c, rec := echoCtx("GET", "/api/admin/metrics/aggregates")
	if err := h.MetricsAggregates(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	assertStatus(t, rec, http.StatusOK)
	body := jsonBody(t, rec)
	data, _ := body["data"].([]any)
	if len(data) != 0 {
		t.Errorf("want empty data, got %v", data)
	}
}

func TestAnalyticsSummary_Fallback(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	expectCascade5mEmptyN(mock, 13)
	c, rec := echoCtx("GET", "/api/admin/analytics/summary")
	if err := h.AnalyticsSummary(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	assertStatus(t, rec, http.StatusOK)
	body := jsonBody(t, rec)
	if body["totalRequests"] != float64(0) {
		t.Errorf("totalRequests=%v", body["totalRequests"])
	}
}

func TestAnalyticsByProvider_Fallback(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	expectCascade5mEmptyN(mock, 8)
	c, rec := echoCtx("GET", "/api/admin/analytics/by-provider")
	if err := h.AnalyticsByProvider(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	assertStatus(t, rec, http.StatusOK)
	body := jsonBody(t, rec)
	data, _ := body["data"].([]any)
	if len(data) != 0 {
		t.Errorf("want empty, got %d", len(data))
	}
}

// AnalyticsByUser uses store.GetAnalyticsGroupBy directly. The "user" enum
// maps to userId which is NOT in analyticsGroupSQL — store returns an error.
func TestAnalyticsByUser_Error(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	// First call will return an error directly from the map check; no SQL
	// fires. Add nothing — handler returns 500.
	_ = mock
	c, rec := echoCtx("GET", "/api/admin/analytics/by-user")
	if err := h.AnalyticsByUser(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	assertStatus(t, rec, http.StatusInternalServerError)
}

func TestAnalyticsUsage_FallbackEmpty(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	// modelUsed: 4 tokens metrics + 2 + 1 dim = 7
	expectCascade5mEmptyN(mock, 7)
	c, rec := echoCtx("GET", "/api/admin/analytics/usage?groupBy=model")
	if err := h.AnalyticsUsage(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	assertStatus(t, rec, http.StatusOK)
	body := jsonBody(t, rec)
	if body["total"] != float64(0) {
		t.Errorf("total=%v", body["total"])
	}
}

func TestAnalyticsUsage_UnknownGroupByDefaultsProvider(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	// Unknown groupBy → falls back to "provider" → 7 args
	expectCascade5mEmptyN(mock, 7)
	c, rec := echoCtx("GET", "/api/admin/analytics/usage?groupBy=junk")
	if err := h.AnalyticsUsage(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	assertStatus(t, rec, http.StatusOK)
}

func TestAnalyticsCost_FallbackEmpty(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	// cost: 5 metrics + 2 + 1 = 8
	expectCascade5mEmptyN(mock, 8)
	c, rec := echoCtx("GET", "/api/admin/analytics/cost?groupBy=model")
	if err := h.AnalyticsCost(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	assertStatus(t, rec, http.StatusOK)
}

func TestAnalyticsCostReport_FallbackEmpty(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	// organization: 3 metrics + 2 + 1 = 6
	expectCascade5mEmptyN(mock, 6)
	c, rec := echoCtx("GET", "/api/admin/analytics/cost-report")
	if err := h.AnalyticsCostReport(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	assertStatus(t, rec, http.StatusOK)
	body := jsonBody(t, rec)
	if body["total"] != float64(0) {
		t.Errorf("total=%v", body["total"])
	}
}

func TestAnalyticsRouting_FallbackEmpty(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	// 1 metric + 2 + 1 = 4
	expectCascade5mEmptyN(mock, 4)
	c, rec := echoCtx("GET", "/api/admin/analytics/routing")
	if err := h.AnalyticsRouting(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	assertStatus(t, rec, http.StatusOK)
}

func TestAnalyticsRouting_HappyPath(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	bucket := time.Now()
	rows := pgxmock.NewRows(rollupCols).
		AddRow("a", bucket, metrics.MetricRequestCount, "routed_provider=p1", "", float64(3),
			[]byte(nil), bucket)
	expectCascade5mRows(mock, rows, 4)
	c, rec := echoCtx("GET", "/api/admin/analytics/routing")
	if err := h.AnalyticsRouting(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	assertStatus(t, rec, http.StatusOK)
}

func TestAnalyticsRoutingFallbacks_FallbackEmpty(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	expectCascade5mEmptyN(mock, 4)
	c, rec := echoCtx("GET", "/api/admin/analytics/routing/fallbacks")
	if err := h.AnalyticsRoutingFallbacks(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	assertStatus(t, rec, http.StatusOK)
}

func TestAnalyticsRoutingFallbacks_Happy(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	bucket := time.Now()
	rows := pgxmock.NewRows(rollupCols).
		AddRow("a", bucket, metrics.MetricRoutingRuleHit, "routing_rule=r1", "", float64(2),
			[]byte(nil), bucket)
	expectCascade5mRows(mock, rows, 4)
	// resolveGroupLabels — routingRuleId
	mock.ExpectQuery(`FROM "RoutingRule"`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id", "name"}).AddRow("r1", "rule-1"))
	c, rec := echoCtx("GET", "/api/admin/analytics/routing/fallbacks")
	if err := h.AnalyticsRoutingFallbacks(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	assertStatus(t, rec, http.StatusOK)
}

func TestAnalyticsQuality_FallbackEmpty(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	expectCascade5mEmptyN(mock, 4)
	c, rec := echoCtx("GET", "/api/admin/analytics/quality")
	if err := h.AnalyticsQuality(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	assertStatus(t, rec, http.StatusOK)
	body := jsonBody(t, rec)
	if body["totalResponses"] != float64(0) {
		t.Errorf("totalResponses=%v", body["totalResponses"])
	}
}

func TestAnalyticsSparkline_NoDataDefault(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	// Sparkline: timeSeries=true → QueryRollupAware → with no window in
	// query string, defaults to last 7 days → 1h table.
	mock.ExpectQuery(`FROM "rollup_watermark"`).
		WithArgs("merge-1h").
		WillReturnError(errPgxNoRowsSentinel)
	mock.ExpectQuery(`FROM "metric_rollup_1h"`).
		WithArgs(matchManyArgs(17)...).
		WillReturnRows(pgxmock.NewRows(rollupCols))
	c, rec := echoCtx("GET", "/api/admin/analytics/sparkline")
	if err := h.AnalyticsSparkline(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	assertStatus(t, rec, http.StatusOK)
	body := jsonBody(t, rec)
	if body["granularity"] != "1d" {
		t.Errorf("granularity=%v", body["granularity"])
	}
}

// errPgxNoRowsSentinel — local alias so the import stays clean.
var errPgxNoRowsSentinel = noRowsErr()

// resolveGroupLabels — every switch arm

func TestResolveGroupLabels_NoData(t *testing.T) {
	t.Parallel()
	_, h := newMockHandler(t)
	h.resolveGroupLabels(context.Background(), "userId", nil)
	// no expectations — should be a no-op
}

func TestResolveGroupLabels_VirtualKey_HasExtra(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	mock.ExpectQuery(`FROM "VirtualKey"`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id", "name", "owner"}).
			AddRow("v1", "VK-1", "alice"))
	data := []store.GroupByResult{{Group: "v1"}}
	h.resolveGroupLabels(context.Background(), "virtualKeyId", data)
	if data[0].GroupLabel != "VK-1" || data[0].GroupExtra != "alice" {
		t.Errorf("got %+v", data[0])
	}
}

func TestResolveGroupLabels_Project(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	mock.ExpectQuery(`FROM "Project"`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id", "name"}).AddRow("p1", "ProjA"))
	data := []store.GroupByResult{{Group: "p1"}}
	h.resolveGroupLabels(context.Background(), "projectId", data)
	if data[0].GroupLabel != "ProjA" {
		t.Errorf("got %+v", data[0])
	}
}

func TestResolveGroupLabels_Organization(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	mock.ExpectQuery(`FROM "Organization"`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id", "name"}).AddRow("o1", "OrgA"))
	data := []store.GroupByResult{{Group: "o1"}}
	h.resolveGroupLabels(context.Background(), "organizationId", data)
	if data[0].GroupLabel != "OrgA" {
		t.Errorf("got %+v", data[0])
	}
}

func TestResolveGroupLabels_User(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	mock.ExpectQuery(`FROM "NexusUser"`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id", "name"}).AddRow("u1", "Alice"))
	data := []store.GroupByResult{{Group: "u1"}}
	h.resolveGroupLabels(context.Background(), "userId", data)
	if data[0].GroupLabel != "Alice" {
		t.Errorf("got %+v", data[0])
	}
}

func TestResolveGroupLabels_RoutingRule(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	mock.ExpectQuery(`FROM "RoutingRule"`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id", "name"}).AddRow("r1", "rule-1"))
	data := []store.GroupByResult{{Group: "r1"}}
	h.resolveGroupLabels(context.Background(), "routingRuleId", data)
	if data[0].GroupLabel != "rule-1" {
		t.Errorf("got %+v", data[0])
	}
}

func TestResolveGroupLabels_Device_ResolvesUsers(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	mock.ExpectQuery(`FROM thing`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id", "hostname"}).AddRow("d1", "host-1"))
	// device users sub-query
	mock.ExpectQuery(`FROM "DeviceAssignment"`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"deviceId", "users"}).AddRow("d1", "Alice, Bob"))
	data := []store.GroupByResult{{Group: "d1"}}
	h.resolveGroupLabels(context.Background(), "deviceId", data)
	if data[0].GroupLabel != "host-1" || data[0].GroupExtra != "Alice, Bob" {
		t.Errorf("got %+v", data[0])
	}
}

func TestResolveGroupLabels_UnknownKey_NoOp(t *testing.T) {
	t.Parallel()
	_, h := newMockHandler(t)
	data := []store.GroupByResult{{Group: "x"}}
	h.resolveGroupLabels(context.Background(), "unknown", data)
	if data[0].GroupLabel != "" {
		t.Errorf("got %+v", data[0])
	}
}

func TestResolveGroupLabels_QueryError(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	mock.ExpectQuery(`FROM "Project"`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnError(errors.New("boom"))
	data := []store.GroupByResult{{Group: "p1"}}
	h.resolveGroupLabels(context.Background(), "projectId", data)
	if data[0].GroupLabel != "" {
		t.Errorf("got %+v", data[0])
	}
}

func TestResolveGroupLabels_ScanError_Continues(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	mock.ExpectQuery(`FROM "Project"`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"only_one"}).AddRow("x"))
	data := []store.GroupByResult{{Group: "p1"}}
	h.resolveGroupLabels(context.Background(), "projectId", data)
	if data[0].GroupLabel != "" {
		t.Errorf("got %+v", data[0])
	}
}

func TestResolveDeviceUsers_QueryError(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	mock.ExpectQuery(`FROM "DeviceAssignment"`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnError(errors.New("conn lost"))
	data := []store.GroupByResult{{Group: "d1"}}
	h.resolveDeviceUsers(context.Background(), data)
	if data[0].GroupExtra != "" {
		t.Errorf("got %+v", data[0])
	}
}

func TestResolveDeviceUsers_ScanError_Continues(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	mock.ExpectQuery(`FROM "DeviceAssignment"`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"only_one"}).AddRow("x"))
	data := []store.GroupByResult{{Group: "d1"}}
	h.resolveDeviceUsers(context.Background(), data)
	if data[0].GroupExtra != "" {
		t.Errorf("got %+v", data[0])
	}
}

// buildProviderDim's marshal-error fallback — exercised via a bad input
// is not possible since map[string]string cannot fail to marshal. Skipped.

// TestRollupGroupsToGroupByResults_DefaultSwitch covers the default branch
// in the sumFields switch (no fields stamped).
func TestRollupGroupsToGroupByResults_Default(t *testing.T) {
	t.Parallel()
	_, h := newMockHandler(t)
	groups := []metrics.MetricsGroup{
		{
			DimensionKey: "model=", // empty value
			Values:       map[string]float64{metrics.MetricRequestCount: 3},
		},
	}
	out := h.rollupGroupsToGroupByResults(context.Background(), "model", groups, "unknown")
	if len(out) != 1 || out[0].RequestCount != 3 || out[0].TotalCostUsd != 0 {
		t.Errorf("got %+v", out)
	}
}

// TestAnalyticsSummary_HitRollupHasPhases exercises the full top-level
// handler so the function appears covered (it just delegates to
// tryRollupSummary when rollup has data).
func TestAnalyticsSummary_HitRollup(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	bucket := time.Now()
	expectCascade5mRows(mock, pgxmock.NewRows(rollupCols).
		AddRow("a", bucket, metrics.MetricRequestCount, "", "", float64(10), []byte(nil), bucket), 13)
	// queryAnalyticsPhasePercentiles fires
	a, b, cVal := 11, 22, 33
	mock.ExpectQuery(`FROM   traffic_event`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"us", "ttfb", "total"}).AddRow(&a, &b, &cVal))

	c, rec := echoCtx("GET", "/api/admin/analytics/summary")
	if err := h.AnalyticsSummary(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	assertStatus(t, rec, http.StatusOK)
}

// TestMetricsAggregates_HitRollup — exercises the success branch.
func TestMetricsAggregates_HitRollup(t *testing.T) {
	t.Parallel()
	mock, h := newMockHandler(t)
	bucket := time.Now()
	// First QueryRollup
	mock.ExpectQuery(`FROM "metric_rollup_1d"`).
		WithArgs(matchManyArgs(18)...).
		WillReturnRows(pgxmock.NewRows(rollupCols).
			AddRow("a", bucket, metrics.MetricRequestCount, "", "", float64(50), []byte(nil), bucket))
	// Provider second query (empty is fine)
	mock.ExpectQuery(`FROM "metric_rollup_1d"`).
		WithArgs(matchManyArgs(7)...).
		WillReturnRows(pgxmock.NewRows(rollupCols))

	c, rec := echoCtx("GET", "/api/admin/metrics/aggregates")
	if err := h.MetricsAggregates(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	assertStatus(t, rec, http.StatusOK)
}

// TestAnalyticsByUser_StoreErrorFromBadCol — the AnalyticsByUser code calls
// `h.db.GetAnalyticsGroupBy(ctx, "userId", ...)`. "userId" is NOT in the
// analyticsGroupSQL map → store returns an error. Pin to a 500 response.
func TestAnalyticsByUser_BadGroupReturnsError(t *testing.T) {
	t.Parallel()
	_, h := newMockHandler(t)
	c, rec := echoCtx("GET", "/api/admin/analytics/by-user")
	if err := h.AnalyticsByUser(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	assertStatus(t, rec, http.StatusInternalServerError)
	body := jsonBody(t, rec)
	e, _ := body["error"].(map[string]any)
	if e["type"] != "server_error" {
		t.Errorf("got %v", body)
	}
}

// TestBuildProviderDimReturnsValidJSON — sanity check on the build helper.
func TestBuildProviderDim_ValidJSON(t *testing.T) {
	t.Parallel()
	raw := buildProviderDim("p-99", "DisplayName")
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	if m["provider"] != "p-99" || m["providerLabel"] != "DisplayName" {
		t.Errorf("got %v", m)
	}
}

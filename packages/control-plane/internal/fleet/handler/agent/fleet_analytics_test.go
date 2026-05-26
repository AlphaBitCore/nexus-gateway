package agent

// fleet_analytics_test.go — coverage for FleetAnalyticsSummary,
// FleetAnalyticsTrends, FleetAnalyticsTopDest, and queryMetricsOrFallback.
//
// All three handlers drive concrete *agentstore.Store / *metricsstore.Store
// methods, so the test seam is pgxmock expectations on the shared pool.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/pashagolub/pgxmock/v4"

	metricspkg "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/instruments"
)

func TestRegisterFleetAnalyticsRoutes_Mounts(t *testing.T) {
	h := newHandlerForTest(nil, &fakeHub{}, nil)
	e := echo.New()
	g := e.Group("/api/admin")
	h.RegisterFleetAnalyticsRoutes(g, noopIAM())
	want := []string{
		"GET /api/admin/fleet-analytics/summary",
		"GET /api/admin/fleet-analytics/trends",
		"GET /api/admin/fleet-analytics/top-destinations",
	}
	seen := map[string]bool{}
	for _, r := range e.Routes() {
		seen[r.Method+" "+r.Path] = true
	}
	for _, k := range want {
		if !seen[k] {
			t.Errorf("missing fleet-analytics route: %s", k)
		}
	}
}

func TestFleetAnalyticsSummary_Happy(t *testing.T) {
	mock := newMockPool(t)
	h := newHandlerForTest(mock, &fakeHub{}, nil)
	// GetAgentFleetHealth runs a QueryRow against `thing` with 2 time args.
	mock.ExpectQuery(`FROM thing\s+WHERE type = 'agent'`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{
			"total", "active", "stale", "critical", "revoked",
		}).AddRow(10, 8, 1, 0, 1))

	e := echo.New()
	e.GET("/fleet-analytics/summary", h.FleetAnalyticsSummary)
	req := httptest.NewRequest(http.MethodGet, "/fleet-analytics/summary", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	// Observable: Total should round-trip as numeric 10.
	if total, _ := body["total"].(float64); total != 10 {
		t.Errorf("total=%v, want 10", body["total"])
	}
}

func TestFleetAnalyticsSummary_DBError(t *testing.T) {
	mock := newMockPool(t)
	h := newHandlerForTest(mock, &fakeHub{}, nil)
	mock.ExpectQuery(`FROM thing\s+WHERE type = 'agent'`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(errors.New("db boom"))

	e := echo.New()
	e.GET("/fleet-analytics/summary", h.FleetAnalyticsSummary)
	req := httptest.NewRequest(http.MethodGet, "/fleet-analytics/summary", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	// Named failure mode: store error → 500 Internal Server Error.
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestFleetAnalyticsTrends_HappyDefaultMetric(t *testing.T) {
	mock := newMockPool(t)
	h := newHandlerForTest(mock, &fakeHub{}, nil)
	// ListMetricRollupBuckets queries metric_rollup_1h with metric name + limit.
	mock.ExpectQuery(`FROM metric_rollup_1h`).
		WithArgs("device_fleet_status", 168).
		WillReturnRows(pgxmock.NewRows([]string{"bucketStart", "dimensionKey", "value"}).
			AddRow(time.Now(), "k=v", float64(42)))

	e := echo.New()
	e.GET("/fleet-analytics/trends", h.FleetAnalyticsTrends)
	req := httptest.NewRequest(http.MethodGet, "/fleet-analytics/trends", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Observable: default metric name echoed back.
	if body["metric"] != "device_fleet_status" {
		t.Errorf("metric=%v, want device_fleet_status", body["metric"])
	}
}

func TestFleetAnalyticsTrends_ExplicitMetric(t *testing.T) {
	mock := newMockPool(t)
	h := newHandlerForTest(mock, &fakeHub{}, nil)
	mock.ExpectQuery(`FROM metric_rollup_1h`).
		WithArgs("request_count", 168).
		WillReturnRows(pgxmock.NewRows([]string{"bucketStart", "dimensionKey", "value"}))

	e := echo.New()
	e.GET("/fleet-analytics/trends", h.FleetAnalyticsTrends)
	req := httptest.NewRequest(http.MethodGet, "/fleet-analytics/trends?metric=request_count", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Named failure mode: explicit metric param respected.
	if body["metric"] != "request_count" {
		t.Errorf("metric=%v, want request_count", body["metric"])
	}
}

func TestFleetAnalyticsTrends_DBError(t *testing.T) {
	mock := newMockPool(t)
	h := newHandlerForTest(mock, &fakeHub{}, nil)
	mock.ExpectQuery(`FROM metric_rollup_1h`).
		WithArgs("device_fleet_status", 168).
		WillReturnError(errors.New("db boom"))

	e := echo.New()
	e.GET("/fleet-analytics/trends", h.FleetAnalyticsTrends)
	req := httptest.NewRequest(http.MethodGet, "/fleet-analytics/trends", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	// Named failure mode: store error → 500.
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
}

// TestFleetAnalyticsTopDest_EmptyFallback exercises the nil-result fallback path
// (queryMetricsOrFallback returns nil when cascade returns no rows).
func TestFleetAnalyticsTopDest_EmptyFallback(t *testing.T) {
	mock := newMockPool(t)
	h := newHandlerForTest(mock, &fakeHub{}, nil)
	// QueryRollupCascade path: no rows returned → result is nil.
	mock.ExpectQuery(`metric_rollup`).
		WillReturnRows(pgxmock.NewRows([]string{"metricName", "dimensionKey", "value", "bucketStart", "granularity"}))

	e := echo.New()
	e.GET("/fleet-analytics/top-destinations", h.FleetAnalyticsTopDest)
	req := httptest.NewRequest(http.MethodGet, "/fleet-analytics/top-destinations", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Named failure mode: no data → fallback returns empty data slice.
	data, ok := body["data"]
	if !ok {
		t.Fatalf("data key missing: %v", body)
	}
	if data == nil {
		t.Errorf("data should not be null")
	}
}

// TestFleetAnalyticsTopDest_WithResult exercises the happy path where
// queryMetricsOrFallback returns groups, which are mapped to TopDestination.
func TestFleetAnalyticsTopDest_WithResult(t *testing.T) {
	mock := newMockPool(t)
	h := newHandlerForTest(mock, &fakeHub{}, nil)
	// QueryRollupCascade will be called; return a row with two metrics.
	// The handler calls QueryRollupCascade (TimeSeries=false).
	// The cascade tries multiple tables — give it a match on the first table.
	mock.ExpectQuery(`metric_rollup`).
		WillReturnRows(pgxmock.NewRows([]string{"metricName", "dimensionKey", "value", "bucketStart", "granularity"}).
			AddRow(metricspkg.MetricRequestCount, "target_host=api.openai.com", float64(100), time.Now(), "1h").
			AddRow(metricspkg.MetricActiveEntities, "target_host=api.openai.com", float64(5), time.Now(), "1h"))

	e := echo.New()
	e.GET("/fleet-analytics/top-destinations", h.FleetAnalyticsTopDest)
	req := httptest.NewRequest(http.MethodGet, "/fleet-analytics/top-destinations", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
}

// TestQueryMetricsOrFallback_TimeSeriesPath exercises the h.TimeSeries=true branch
// which calls QueryRollupAware instead of QueryRollupCascade.
func TestQueryMetricsOrFallback_TimeSeriesPath(t *testing.T) {
	mock := newMockPool(t)
	h := newHandlerForTest(mock, &fakeHub{}, nil)
	// QueryRollupAware is wired when TimeSeries=true; it will hit the
	// watermark query then the rollup table. Return empty rows so the
	// function returns nil result cleanly.
	mock.ExpectQuery(`metric_rollup`).
		WillReturnRows(pgxmock.NewRows([]string{"metricName", "dimensionKey", "value", "bucketStart", "granularity"}))

	now := time.Now()
	q := metricspkg.MetricsQuery{
		Metrics:    []string{metricspkg.MetricRequestCount},
		StartTime:  now.Add(-1 * time.Hour),
		EndTime:    now,
		TimeSeries: true,
	}
	result, err := h.queryMetricsOrFallback(context.Background(), q)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Observable: empty rows → nil result (not error).
	if result != nil {
		t.Errorf("expected nil result for empty rows, got %+v", result)
	}
}

// TestRunUpdateDeviceTags_PoolPath exercises the pool.Exec path of
// runUpdateDeviceTags (when updateDeviceTagsFn is nil). This covers
// the 50% uncovered branch in device_tags.go:21-24.
func TestRunUpdateDeviceTags_PoolPath(t *testing.T) {
	mock := newMockPool(t)
	h := newHandlerForTest(mock, &fakeHub{}, nil)
	// updateDeviceTagsFn is nil → pool.Exec runs.
	mock.ExpectExec(`UPDATE thing SET tags`).
		WithArgs("device-x", []string{"a", "b"}).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	if err := h.runUpdateDeviceTags(context.Background(), "device-x", []string{"a", "b"}); err != nil {
		t.Fatalf("runUpdateDeviceTags pool path: %v", err)
	}
}

func TestRunUpdateDeviceTags_PoolError(t *testing.T) {
	mock := newMockPool(t)
	h := newHandlerForTest(mock, &fakeHub{}, nil)
	mock.ExpectExec(`UPDATE thing SET tags`).
		WithArgs("device-x", []string{"a"}).
		WillReturnError(errors.New("exec boom"))

	// Named failure mode: pool.Exec failure propagated.
	if err := h.runUpdateDeviceTags(context.Background(), "device-x", []string{"a"}); err == nil {
		t.Fatal("expected error, got nil")
	}
}

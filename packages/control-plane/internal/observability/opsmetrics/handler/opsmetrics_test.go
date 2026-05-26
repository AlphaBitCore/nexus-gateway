// Package opsmetrics — handler unit tests.
// Drive every handler method through httptest + Echo with a pgxmock-backed
// opsstore.Store. Covers: param validation, DB-nil guard (503), DB error
// propagation (500), nil-slice normalisation, granularity auto-selection,
// and the parseFromTo / parseOpsTimeseriesParams helpers.
package opsmetrics

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/observability/opsmetrics/opsstore"
)

// Test helpers

func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newHandlerWithMock creates a Handler backed by a pgxmock pool so SQL
// expectations can be set without a real Postgres instance.
func newHandlerWithMock(t *testing.T) (pgxmock.PgxPoolIface, *Handler) {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mock.Close)
	return mock, New(Deps{Pool: mock, Logger: silentLogger()})
}

// newHandlerNilOps creates a Handler with h.ops set to nil directly
// (white-box) so the DB-nil guard (h.ops == nil → 503) is exercised.
// New() always sets h.ops to a non-nil *opsstore.Store, so reaching the guard
// requires white-box injection.
func newHandlerNilOps() *Handler {
	h := New(Deps{Pool: nil, Logger: silentLogger()})
	h.ops = nil
	return h
}

// echoCtx returns an Echo context bound to the given request + recorder.
func echoCtx(req *http.Request, rec *httptest.ResponseRecorder) echo.Context {
	e := echo.New()
	return e.NewContext(req, rec)
}

func decodeJSON(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode body: %v; raw=%s", err, rec.Body.String())
	}
	return m
}

func errType(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	m := decodeJSON(t, rec)
	errObj, _ := m["error"].(map[string]any)
	if errObj == nil {
		t.Fatalf("no error envelope: %s", rec.Body.String())
	}
	s, _ := errObj["type"].(string)
	return s
}

// validFromTo returns a pair of RFC3339 strings spanning 24 hours from now.
func validFromTo() (string, string) {
	now := time.Now().UTC()
	return now.Add(-24 * time.Hour).Format(time.RFC3339), now.Format(time.RFC3339)
}

// internalServerError helper

func TestInternalServerError_Returns500(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	c := echoCtx(req, rec)
	_ = internalServerError(c, "something went wrong")
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d; want 500", rec.Code)
	}
	body := decodeJSON(t, rec)
	errObj, _ := body["error"].(map[string]any)
	if errObj == nil || errObj["type"] != "server_error" {
		t.Errorf("unexpected body: %s", rec.Body.String())
	}
}

// New / parseRFC3339Flexible

func TestNew_WiresLogger(t *testing.T) {
	lg := silentLogger()
	h := New(Deps{Pool: nil, Logger: lg})
	if h.logger != lg {
		t.Error("logger not wired")
	}
}

func TestNew_NilLogger_UsesDefault(t *testing.T) {
	h := New(Deps{Pool: nil, Logger: nil})
	if h.logger == nil {
		t.Error("logger must not be nil")
	}
}

func TestParseRFC3339Flexible_RFC3339Nano(t *testing.T) {
	s := "2026-01-15T10:30:00.000000001Z"
	_, ok := parseRFC3339Flexible(s)
	if !ok {
		t.Errorf("parseRFC3339Flexible(%q) = false; want true", s)
	}
}

func TestParseRFC3339Flexible_RFC3339(t *testing.T) {
	s := "2026-01-15T10:30:00Z"
	_, ok := parseRFC3339Flexible(s)
	if !ok {
		t.Errorf("parseRFC3339Flexible(%q) = false; want true", s)
	}
}

func TestParseRFC3339Flexible_Invalid(t *testing.T) {
	_, ok := parseRFC3339Flexible("not-a-date")
	if ok {
		t.Errorf("parseRFC3339Flexible invalid string should return false")
	}
}

// errJSON / badReq / parseFromTo helpers

func TestErrJSON_Shape(t *testing.T) {
	got := errJSON("msg", "validation_error", "CODE")
	errObj, _ := got["error"].(map[string]any)
	if errObj == nil || errObj["message"] != "msg" || errObj["type"] != "validation_error" || errObj["code"] != "CODE" {
		t.Errorf("unexpected envelope: %v", got)
	}
}

func TestParseFromTo_MissingBoth_400(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	c := echoCtx(req, httptest.NewRecorder())
	_, _, herr := parseFromTo(c)
	if herr == nil {
		t.Fatal("expected herr; got nil")
	}
	if herr.status != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", herr.status)
	}
}

func TestParseFromTo_InvalidFrom_400(t *testing.T) {
	_, to := validFromTo()
	req := httptest.NewRequest(http.MethodGet, "/?from=NOTADATE&to="+to, nil)
	c := echoCtx(req, httptest.NewRecorder())
	_, _, herr := parseFromTo(c)
	if herr == nil || herr.status != http.StatusBadRequest {
		t.Fatal("expected 400 for invalid from")
	}
}

func TestParseFromTo_InvalidTo_400(t *testing.T) {
	from, _ := validFromTo()
	req := httptest.NewRequest(http.MethodGet, "/?from="+from+"&to=NOTADATE", nil)
	c := echoCtx(req, httptest.NewRecorder())
	_, _, herr := parseFromTo(c)
	if herr == nil || herr.status != http.StatusBadRequest {
		t.Fatal("expected 400 for invalid to")
	}
}

func TestParseFromTo_FromNotBeforeTo_400(t *testing.T) {
	now := time.Now().UTC().Format(time.RFC3339)
	past := time.Now().UTC().Add(-1 * time.Hour).Format(time.RFC3339)
	// from = now, to = past — from is not before to
	req := httptest.NewRequest(http.MethodGet, "/?from="+now+"&to="+past, nil)
	c := echoCtx(req, httptest.NewRecorder())
	_, _, herr := parseFromTo(c)
	if herr == nil || herr.status != http.StatusBadRequest {
		t.Fatal("expected 400 when from >= to")
	}
}

func TestParseFromTo_Happy(t *testing.T) {
	from, to := validFromTo()
	req := httptest.NewRequest(http.MethodGet, "/?from="+from+"&to="+to, nil)
	c := echoCtx(req, httptest.NewRecorder())
	f, tt, herr := parseFromTo(c)
	if herr != nil {
		t.Fatalf("unexpected herr: %v", herr)
	}
	if f.IsZero() || tt.IsZero() {
		t.Errorf("times should be non-zero; from=%v to=%v", f, tt)
	}
}

func TestOpsMetricsCurrent_NilOps_503(t *testing.T) {
	h := newHandlerNilOps()
	req := httptest.NewRequest(http.MethodGet, "/ops-metrics/current", nil)
	rec := httptest.NewRecorder()
	if err := h.OpsMetricsCurrent(echoCtx(req, rec)); err != nil {
		t.Fatalf("OpsMetricsCurrent: %v", err)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("code = %d; want 503", rec.Code)
	}
}

func TestOpsMetricsCurrent_InvalidNodeType_400(t *testing.T) {
	_, h := newHandlerWithMock(t)
	req := httptest.NewRequest(http.MethodGet, "/ops-metrics/current?nodeType=bad-type", nil)
	rec := httptest.NewRecorder()
	if err := h.OpsMetricsCurrent(echoCtx(req, rec)); err != nil {
		t.Fatalf("OpsMetricsCurrent: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d; want 400", rec.Code)
	}
	if errType(t, rec) != "validation_error" {
		t.Errorf("type = %q; want validation_error", errType(t, rec))
	}
}

func TestOpsMetricsCurrent_ValidNodeTypes_AcceptedByValidation(t *testing.T) {
	// Valid nodeType values must pass the guard (not return 400).
	for _, nt := range []string{"service", "agent", "control-plane", "ai-gateway", "compliance-proxy", "nexus-hub"} {
		mock, h := newHandlerWithMock(t)
		// GetOpsMetricsCurrent passes 2 args: thingType, thingID (both may be nil *string).
		mock.ExpectQuery(`WITH ranked`).
			WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
			WillReturnRows(pgxmock.NewRows([]string{
				"sampled_at", "thing_id", "thing_type", "metric_name", "metric_kind", "dimension_key", "value", "metadata",
			}))
		req := httptest.NewRequest(http.MethodGet, "/ops-metrics/current?nodeType="+nt, nil)
		rec := httptest.NewRecorder()
		if err := h.OpsMetricsCurrent(echoCtx(req, rec)); err != nil {
			t.Fatalf("nodeType=%q: %v", nt, err)
		}
		if rec.Code == http.StatusBadRequest {
			t.Errorf("nodeType=%q was rejected; want OK", nt)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("nodeType=%q unmet: %v", nt, err)
		}
	}
}

func TestOpsMetricsCurrent_DBError_500(t *testing.T) {
	mock, h := newHandlerWithMock(t)
	mock.ExpectQuery(`WITH ranked`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(errDBError())
	req := httptest.NewRequest(http.MethodGet, "/ops-metrics/current", nil)
	rec := httptest.NewRecorder()
	if err := h.OpsMetricsCurrent(echoCtx(req, rec)); err != nil {
		t.Fatalf("OpsMetricsCurrent: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d; want 500", rec.Code)
	}
}

func TestOpsMetricsCurrent_NoRows_ReturnsEmptySlice(t *testing.T) {
	mock, h := newHandlerWithMock(t)
	mock.ExpectQuery(`WITH ranked`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{
			"sampled_at", "thing_id", "thing_type", "metric_name", "metric_kind", "dimension_key", "value", "metadata",
		}))
	req := httptest.NewRequest(http.MethodGet, "/ops-metrics/current", nil)
	rec := httptest.NewRecorder()
	if err := h.OpsMetricsCurrent(echoCtx(req, rec)); err != nil {
		t.Fatalf("OpsMetricsCurrent: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d; want 200", rec.Code)
	}
	body := decodeJSON(t, rec)
	data, _ := body["data"].([]any)
	if data == nil {
		t.Errorf("data must not be nil (must serialize as []); body=%s", rec.Body.String())
	}
}

func TestOpsMetricsCurrent_EmptyNodeType_AcceptsAll(t *testing.T) {
	// Empty nodeType must not be rejected — means "all types".
	mock, h := newHandlerWithMock(t)
	mock.ExpectQuery(`WITH ranked`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{
			"sampled_at", "thing_id", "thing_type", "metric_name", "metric_kind", "dimension_key", "value", "metadata",
		}))
	req := httptest.NewRequest(http.MethodGet, "/ops-metrics/current", nil)
	rec := httptest.NewRecorder()
	if err := h.OpsMetricsCurrent(echoCtx(req, rec)); err != nil {
		t.Fatalf("OpsMetricsCurrent: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d; want 200 (no nodeType filter)", rec.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// errDBError returns a generic DB error for failure-path tests.
func errDBError() error {
	return &dbError{msg: "planner error"}
}

type dbError struct{ msg string }

func (e *dbError) Error() string { return e.msg }

func TestOpsMetricsTimeseries_NilOps_503(t *testing.T) {
	h := newHandlerNilOps()
	from, to := validFromTo()
	req := httptest.NewRequest(http.MethodGet,
		"/ops-metrics/timeseries?nodeId=abc&metric=cpu&from="+from+"&to="+to, nil)
	rec := httptest.NewRecorder()
	if err := h.OpsMetricsTimeseries(echoCtx(req, rec)); err != nil {
		t.Fatalf("OpsMetricsTimeseries: %v", err)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("code = %d; want 503", rec.Code)
	}
}

func TestOpsMetricsTimeseries_MissingNodeID_400(t *testing.T) {
	_, h := newHandlerWithMock(t)
	from, to := validFromTo()
	req := httptest.NewRequest(http.MethodGet,
		"/ops-metrics/timeseries?metric=cpu&from="+from+"&to="+to, nil)
	rec := httptest.NewRecorder()
	if err := h.OpsMetricsTimeseries(echoCtx(req, rec)); err != nil {
		t.Fatalf("OpsMetricsTimeseries: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d; want 400", rec.Code)
	}
}

func TestOpsMetricsTimeseries_MissingMetric_400(t *testing.T) {
	_, h := newHandlerWithMock(t)
	from, to := validFromTo()
	req := httptest.NewRequest(http.MethodGet,
		"/ops-metrics/timeseries?nodeId=abc&from="+from+"&to="+to, nil)
	rec := httptest.NewRecorder()
	if err := h.OpsMetricsTimeseries(echoCtx(req, rec)); err != nil {
		t.Fatalf("OpsMetricsTimeseries: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d; want 400", rec.Code)
	}
}

func TestOpsMetricsTimeseries_MissingFromTo_400(t *testing.T) {
	_, h := newHandlerWithMock(t)
	req := httptest.NewRequest(http.MethodGet,
		"/ops-metrics/timeseries?nodeId=abc&metric=cpu", nil)
	rec := httptest.NewRecorder()
	if err := h.OpsMetricsTimeseries(echoCtx(req, rec)); err != nil {
		t.Fatalf("OpsMetricsTimeseries: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d; want 400", rec.Code)
	}
}

func TestOpsMetricsTimeseries_InvalidGranularity_400(t *testing.T) {
	_, h := newHandlerWithMock(t)
	from, to := validFromTo()
	req := httptest.NewRequest(http.MethodGet,
		"/ops-metrics/timeseries?nodeId=abc&metric=cpu&from="+from+"&to="+to+"&granularity=bad", nil)
	rec := httptest.NewRecorder()
	if err := h.OpsMetricsTimeseries(echoCtx(req, rec)); err != nil {
		t.Fatalf("OpsMetricsTimeseries: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d; want 400", rec.Code)
	}
}

func TestOpsMetricsTimeseries_DBError_500(t *testing.T) {
	mock, h := newHandlerWithMock(t)
	// queryOpsRaw uses 5 args: ThingID, MetricName, dim, From, To
	mock.ExpectQuery(`FROM metric_ops_raw`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(errDBError())
	from, to := validFromTo()
	req := httptest.NewRequest(http.MethodGet,
		"/ops-metrics/timeseries?nodeId=abc&metric=cpu&from="+from+"&to="+to+"&granularity=raw", nil)
	rec := httptest.NewRecorder()
	if err := h.OpsMetricsTimeseries(echoCtx(req, rec)); err != nil {
		t.Fatalf("OpsMetricsTimeseries: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d; want 500", rec.Code)
	}
}

func TestOpsMetricsTimeseries_NoRows_ReturnsEmptySlice(t *testing.T) {
	mock, h := newHandlerWithMock(t)
	// queryOpsRaw uses 5 args: ThingID, MetricName, dim, From, To
	mock.ExpectQuery(`FROM metric_ops_raw`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{
			"sampled_at", "thing_id", "thing_type", "metric_name", "metric_kind", "dimension_key", "value", "metadata",
		}))
	from, to := validFromTo()
	req := httptest.NewRequest(http.MethodGet,
		"/ops-metrics/timeseries?nodeId=abc&metric=cpu&from="+from+"&to="+to+"&granularity=raw", nil)
	rec := httptest.NewRecorder()
	if err := h.OpsMetricsTimeseries(echoCtx(req, rec)); err != nil {
		t.Fatalf("OpsMetricsTimeseries: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d; body=%s", rec.Code, rec.Body.String())
	}
	body := decodeJSON(t, rec)
	data, _ := body["data"].([]any)
	if data == nil {
		t.Errorf("data must not be nil; body=%s", rec.Body.String())
	}
	if body["granularity"] != "raw" {
		t.Errorf("granularity = %v; want raw", body["granularity"])
	}
}

func TestOpsMetricsTimeseries_AutoGranularity_Short(t *testing.T) {
	// A 2h window should auto-select "raw".
	mock, h := newHandlerWithMock(t)
	mock.ExpectQuery(`FROM metric_ops_raw`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{
			"sampled_at", "thing_id", "thing_type", "metric_name", "metric_kind", "dimension_key", "value", "metadata",
		}))
	now := time.Now().UTC()
	from := now.Add(-2 * time.Hour).Format(time.RFC3339)
	to := now.Format(time.RFC3339)
	req := httptest.NewRequest(http.MethodGet,
		"/ops-metrics/timeseries?nodeId=abc&metric=cpu&from="+from+"&to="+to+"&granularity=auto", nil)
	rec := httptest.NewRecorder()
	_ = h.OpsMetricsTimeseries(echoCtx(req, rec))
	body := decodeJSON(t, rec)
	if body["granularity"] != "raw" {
		t.Errorf("granularity = %v; want raw for 2h window", body["granularity"])
	}
}

func TestOpsMetricsTimeseries_WithDimParam(t *testing.T) {
	// Verify that the ?dim= query param is accepted and passed to the store
	// (exercises the dim *string branch in parseOpsTimeseriesParams).
	mock, h := newHandlerWithMock(t)
	mock.ExpectQuery(`FROM metric_ops_raw`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{
			"sampled_at", "thing_id", "thing_type", "metric_name", "metric_kind", "dimension_key", "value", "metadata",
		}))
	from, to := validFromTo()
	req := httptest.NewRequest(http.MethodGet,
		"/ops-metrics/timeseries?nodeId=abc&metric=cpu&from="+from+"&to="+to+"&granularity=raw&dim=provider=openai", nil)
	rec := httptest.NewRecorder()
	_ = h.OpsMetricsTimeseries(echoCtx(req, rec))
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d; want 200", rec.Code)
	}
}

func TestOpsMetricsFleet_NilOps_503(t *testing.T) {
	h := newHandlerNilOps()
	from, to := validFromTo()
	req := httptest.NewRequest(http.MethodGet,
		"/ops-metrics/fleet?nodeType=agent&metric=cpu&from="+from+"&to="+to, nil)
	rec := httptest.NewRecorder()
	if err := h.OpsMetricsFleet(echoCtx(req, rec)); err != nil {
		t.Fatalf("OpsMetricsFleet: %v", err)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("code = %d; want 503", rec.Code)
	}
}

func TestOpsMetricsFleet_MissingNodeType_400(t *testing.T) {
	_, h := newHandlerWithMock(t)
	from, to := validFromTo()
	req := httptest.NewRequest(http.MethodGet,
		"/ops-metrics/fleet?metric=cpu&from="+from+"&to="+to, nil)
	rec := httptest.NewRecorder()
	if err := h.OpsMetricsFleet(echoCtx(req, rec)); err != nil {
		t.Fatalf("OpsMetricsFleet: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d; want 400", rec.Code)
	}
}

func TestOpsMetricsFleet_MissingMetric_400(t *testing.T) {
	_, h := newHandlerWithMock(t)
	from, to := validFromTo()
	req := httptest.NewRequest(http.MethodGet,
		"/ops-metrics/fleet?nodeType=agent&from="+from+"&to="+to, nil)
	rec := httptest.NewRecorder()
	if err := h.OpsMetricsFleet(echoCtx(req, rec)); err != nil {
		t.Fatalf("OpsMetricsFleet: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d; want 400", rec.Code)
	}
}

func TestOpsMetricsFleet_InvalidGranularity_400(t *testing.T) {
	_, h := newHandlerWithMock(t)
	from, to := validFromTo()
	req := httptest.NewRequest(http.MethodGet,
		"/ops-metrics/fleet?nodeType=agent&metric=cpu&from="+from+"&to="+to+"&granularity=raw", nil)
	rec := httptest.NewRecorder()
	if err := h.OpsMetricsFleet(echoCtx(req, rec)); err != nil {
		t.Fatalf("OpsMetricsFleet: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d; want 400 (raw is invalid for fleet)", rec.Code)
	}
}

func TestOpsMetricsFleet_InvalidCustomGranularity_400(t *testing.T) {
	_, h := newHandlerWithMock(t)
	from, to := validFromTo()
	req := httptest.NewRequest(http.MethodGet,
		"/ops-metrics/fleet?nodeType=agent&metric=cpu&from="+from+"&to="+to+"&granularity=5m", nil)
	rec := httptest.NewRecorder()
	if err := h.OpsMetricsFleet(echoCtx(req, rec)); err != nil {
		t.Fatalf("OpsMetricsFleet: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d; want 400 (invalid granularity 5m)", rec.Code)
	}
}

func TestOpsMetricsFleet_DBError_500(t *testing.T) {
	mock, h := newHandlerWithMock(t)
	// Fleet queries metric_ops_rollup_1h with 5 args: ThingType, MetricName, dim, From, To
	mock.ExpectQuery(`FROM metric_ops_rollup_1h`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(errDBError())
	from, to := validFromTo()
	req := httptest.NewRequest(http.MethodGet,
		"/ops-metrics/fleet?nodeType=agent&metric=cpu&from="+from+"&to="+to+"&granularity=1h", nil)
	rec := httptest.NewRecorder()
	if err := h.OpsMetricsFleet(echoCtx(req, rec)); err != nil {
		t.Fatalf("OpsMetricsFleet: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d; want 500", rec.Code)
	}
}

func TestOpsMetricsFleet_NoRows_ReturnsEmptySlice(t *testing.T) {
	mock, h := newHandlerWithMock(t)
	mock.ExpectQuery(`FROM metric_ops_rollup_1h`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{
			"bucket_start", "thing_id", "thing_type", "metric_name", "metric_kind",
			"dimension_key", "value_avg", "value_sum", "value_min", "value_max", "sample_count", "metadata",
		}))
	from, to := validFromTo()
	req := httptest.NewRequest(http.MethodGet,
		"/ops-metrics/fleet?nodeType=agent&metric=cpu&from="+from+"&to="+to+"&granularity=1h", nil)
	rec := httptest.NewRecorder()
	if err := h.OpsMetricsFleet(echoCtx(req, rec)); err != nil {
		t.Fatalf("OpsMetricsFleet: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d; body=%s", rec.Code, rec.Body.String())
	}
	body := decodeJSON(t, rec)
	data, _ := body["data"].([]any)
	if data == nil {
		t.Errorf("data must not be nil; body=%s", rec.Body.String())
	}
}

func TestOpsMetricsFleet_AutoGranularity_ShortWindow_BumpsTo1h(t *testing.T) {
	// A 2h window → opsstore.SelectGranularity returns "raw" → fleet handler
	// bumps it to "1h" and issues the 1h query.
	mock, h := newHandlerWithMock(t)
	mock.ExpectQuery(`FROM metric_ops_rollup_1h`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{
			"bucket_start", "thing_id", "thing_type", "metric_name", "metric_kind",
			"dimension_key", "value_avg", "value_sum", "value_min", "value_max", "sample_count", "metadata",
		}))
	now := time.Now().UTC()
	from := now.Add(-2 * time.Hour).Format(time.RFC3339)
	to := now.Format(time.RFC3339)
	req := httptest.NewRequest(http.MethodGet,
		"/ops-metrics/fleet?nodeType=agent&metric=cpu&from="+from+"&to="+to+"&granularity=auto", nil)
	rec := httptest.NewRecorder()
	_ = h.OpsMetricsFleet(echoCtx(req, rec))
	body := decodeJSON(t, rec)
	if body["granularity"] != "1h" {
		t.Errorf("granularity = %v; want 1h (raw bumped for fleet)", body["granularity"])
	}
}

func TestOpsMetricsFleet_WithDimParam(t *testing.T) {
	// Exercise the optional ?dim= branch for fleet queries.
	mock, h := newHandlerWithMock(t)
	mock.ExpectQuery(`FROM metric_ops_rollup_1h`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{
			"bucket_start", "thing_id", "thing_type", "metric_name", "metric_kind",
			"dimension_key", "value_avg", "value_sum", "value_min", "value_max", "sample_count", "metadata",
		}))
	from, to := validFromTo()
	req := httptest.NewRequest(http.MethodGet,
		"/ops-metrics/fleet?nodeType=agent&metric=cpu&from="+from+"&to="+to+"&granularity=1h&dim=model=gpt-4", nil)
	rec := httptest.NewRecorder()
	_ = h.OpsMetricsFleet(echoCtx(req, rec))
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d; want 200 with dim param", rec.Code)
	}
}

// SelectGranularity (opsstore helper — tested via handler)

func TestSelectGranularity_Boundaries(t *testing.T) {
	base := time.Now()
	cases := []struct {
		name string
		span time.Duration
		want string
	}{
		{"≤6h → raw", 5 * time.Hour, "raw"},
		{"6h boundary → raw", 6 * time.Hour, "raw"},
		{"6h+1min → 1h", 6*time.Hour + time.Minute, "1h"},
		{"7d → 1h", 7 * 24 * time.Hour, "1h"},
		{"7d+1h → 1d", 7*24*time.Hour + time.Hour, "1d"},
		{"90d → 1d", 90 * 24 * time.Hour, "1d"},
		{"91d → 1mo", 91 * 24 * time.Hour, "1mo"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			from := base
			to := base.Add(tc.span)
			got := opsstore.SelectGranularity(from, to)
			if got != tc.want {
				t.Errorf("span=%v: got %q; want %q", tc.span, got, tc.want)
			}
		})
	}
}

func TestRegisterOpsMetricsRoutes_RegistersThreeRoutes(t *testing.T) {
	_, h := newHandlerWithMock(t)
	e := echo.New()
	g := e.Group("/api/admin")
	iamMW := func(_ string) echo.MiddlewareFunc {
		return func(next echo.HandlerFunc) echo.HandlerFunc { return next }
	}
	h.RegisterOpsMetricsRoutes(g, iamMW)

	want := map[string]bool{
		"GET /api/admin/ops-metrics/current":    false,
		"GET /api/admin/ops-metrics/timeseries": false,
		"GET /api/admin/ops-metrics/fleet":      false,
	}
	for _, r := range e.Routes() {
		key := r.Method + " " + r.Path
		if _, ok := want[key]; ok {
			want[key] = true
		}
	}
	for k, present := range want {
		if !present {
			t.Errorf("route not registered: %s", k)
		}
	}
}

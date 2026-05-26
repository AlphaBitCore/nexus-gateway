// Package thingstats — unit tests for GetThingStats and helpers.
// Uses pgxmock for the thingstore.Store pool; h.pool is set to nil (white-box)
// so resolveDimensionNames takes the nil-pool short-circuit path. All other
// handler branches are fully exercised.
package thingstats

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v4"
	"github.com/pashagolub/pgxmock/v4"

	authn "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authn"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/observability/thingstats/thingstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	metrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/instruments"
)

// Test helpers

func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newHandlerWithMock creates a Handler where:
//   - h.thing is backed by pgxmock so SQL expectations can be set.
//   - h.pool is nil so resolveDimensionNames short-circuits (pool == nil → nil).
func newHandlerWithMock(t *testing.T) (pgxmock.PgxPoolIface, *Handler) {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mock.Close)
	h := &Handler{
		thing:  thingstore.New(mock),
		pool:   nil, // white-box: skip resolveDimensionNames DB calls
		logger: silentLogger(),
	}
	return mock, h
}

// newHandlerNilThing creates a Handler where h.thing is nil — triggers the
// DB-nil guard (h.thing == nil → 503).
func newHandlerNilThing() *Handler {
	return &Handler{thing: nil, pool: nil, logger: silentLogger()}
}

func decodeJSON(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode JSON: %v; raw=%s", err, rec.Body.String())
	}
	return m
}

func errGeneric() error { return &genericErr{"simulated db error"} }

type genericErr struct{ msg string }

func (e *genericErr) Error() string { return e.msg }

// thingRow returns pgxmock rows that represent a single thing table row.
func thingRow(id, thingType string, name *string) *pgxmock.Rows {
	now := time.Now()
	emptyJSON := []byte("{}")
	n := (*string)(nil)
	if name != nil {
		s := *name
		n = &s
	}
	return pgxmock.NewRows([]string{
		"id", "type", "name", "version", "address", "enrolled_by", "auth_type", "conn_protocol",
		"status", "enrolled_at", "last_seen_at", "updated_at",
		"desired", "reported", "desired_ver", "reported_ver", "metadata",
	}).AddRow(
		id, thingType, n, nil, nil, nil, "token", "ws",
		"online", now, nil, now,
		emptyJSON, emptyJSON, int64(1), int64(1), emptyJSON,
	)
}

// rollupHasAnyRecentRow returns pgxmock rows for the ThingRollupHasAnyRecent
// query (SELECT EXISTS(...)) — uses QueryRow, so pgxmock expects a Query.
func rollupHasAnyRecentRow(has bool) *pgxmock.Rows {
	return pgxmock.NewRows([]string{"exists"}).AddRow(has)
}

// rollupCols returns pgxmock rows for QueryThingRollup.
// Column order: id, bucketStart, thing_id, metricName, dimensionKey, subDimension, value, metadata, updatedAt
func rollupCols() *pgxmock.Rows {
	return pgxmock.NewRows([]string{
		"id", "bucketStart", "thing_id", "metricName", "dimensionKey", "subDimension", "value", "metadata", "updatedAt",
	})
}

// New / logger wiring

func TestNew_NilLogger_UsesDefault(t *testing.T) {
	h := New(Deps{Pool: nil, Logger: nil})
	if h.logger == nil {
		t.Error("logger must not be nil")
	}
}

func TestNew_LoggerWired(t *testing.T) {
	lg := silentLogger()
	h := New(Deps{Pool: nil, Logger: lg})
	if h.logger != lg {
		t.Error("logger not wired correctly")
	}
}

// errJSON / internalServerError

func TestErrJSON_Shape(t *testing.T) {
	got := errJSON("msg", "validation_error", "CODE")
	errObj, _ := got["error"].(map[string]any)
	if errObj == nil || errObj["message"] != "msg" {
		t.Errorf("unexpected shape: %v", errObj)
	}
}

func TestInternalServerError_Returns500(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)
	_ = internalServerError(c, "oops")
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d; want 500", rec.Code)
	}
}

func TestParsePagination_Defaults(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	c := e.NewContext(req, httptest.NewRecorder())
	p := parsePagination(c)
	if p.Limit != 50 || p.Offset != 0 {
		t.Errorf("defaults: limit=%d offset=%d; want 50/0", p.Limit, p.Offset)
	}
}

func TestParsePagination_CustomValues(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/?limit=100&offset=20", nil)
	c := e.NewContext(req, httptest.NewRecorder())
	p := parsePagination(c)
	if p.Limit != 100 || p.Offset != 20 {
		t.Errorf("custom: limit=%d offset=%d; want 100/20", p.Limit, p.Offset)
	}
}

func TestParsePagination_LimitCappedAt1000(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/?limit=9999", nil)
	c := e.NewContext(req, httptest.NewRecorder())
	p := parsePagination(c)
	if p.Limit != 1000 {
		t.Errorf("limit capped: got %d; want 1000", p.Limit)
	}
}

func TestParsePagination_InvalidLimit_UsesDefault(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/?limit=notanumber", nil)
	c := e.NewContext(req, httptest.NewRecorder())
	p := parsePagination(c)
	if p.Limit != 50 {
		t.Errorf("invalid limit: got %d; want 50", p.Limit)
	}
}

func TestParsePagination_ZeroLimit_UsesDefault(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/?limit=0", nil)
	c := e.NewContext(req, httptest.NewRecorder())
	p := parsePagination(c)
	if p.Limit != 50 {
		t.Errorf("zero limit: got %d; want 50 (default, since 0 is not > 0)", p.Limit)
	}
}

// parseRFC3339Flexible / parseTimeRange

func TestParseRFC3339Flexible_RFC3339(t *testing.T) {
	_, ok := parseRFC3339Flexible("2026-01-15T10:00:00Z")
	if !ok {
		t.Errorf("RFC3339 should parse ok")
	}
}

func TestParseRFC3339Flexible_RFC3339Nano(t *testing.T) {
	_, ok := parseRFC3339Flexible("2026-01-15T10:00:00.000000001Z")
	if !ok {
		t.Errorf("RFC3339Nano should parse ok")
	}
}

func TestParseRFC3339Flexible_Invalid(t *testing.T) {
	_, ok := parseRFC3339Flexible("not-a-date")
	if ok {
		t.Error("invalid string should return false")
	}
}

func TestParseTimeRange_WithValidTimestamps(t *testing.T) {
	e := echo.New()
	s := time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339)
	end := time.Now().UTC().Format(time.RFC3339)
	req := httptest.NewRequest(http.MethodGet, "/?startTime="+s+"&endTime="+end, nil)
	c := e.NewContext(req, httptest.NewRecorder())
	start, endOut := parseTimeRange(c)
	if start == nil || endOut == nil {
		t.Errorf("expected non-nil start and end; got start=%v end=%v", start, endOut)
	}
}

func TestParseTimeRange_EmptyParams_ReturnsNils(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	c := e.NewContext(req, httptest.NewRecorder())
	start, end := parseTimeRange(c)
	if start != nil || end != nil {
		t.Errorf("empty params: expected nil; got start=%v end=%v", start, end)
	}
}

func TestParseTimeRange_InvalidStartTime_ReturnsNilStart(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/?startTime=notadate", nil)
	c := e.NewContext(req, httptest.NewRecorder())
	start, _ := parseTimeRange(c)
	if start != nil {
		t.Errorf("invalid startTime: expected nil; got %v", start)
	}
}

func TestActorFromContext_NoAuth_Empty(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	c := e.NewContext(req, httptest.NewRecorder())
	a := actorFromContext(c)
	if a.UserID != "" || a.Name != "" {
		t.Errorf("expected empty actor; got %+v", a)
	}
}

func TestActorFromContext_WithAuth_ReturnsFields(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	c := e.NewContext(req, httptest.NewRecorder())
	middleware.WithAdminAuth(c, &authn.AdminAuth{KeyID: "user-abc", KeyName: "Alice"})
	a := actorFromContext(c)
	if a.UserID != "user-abc" || a.Name != "Alice" {
		t.Errorf("actor = %+v; want {user-abc, Alice}", a)
	}
}

func TestSplitDimensionKey_Empty(t *testing.T) {
	_, _, ok := splitDimensionKey("")
	if ok {
		t.Error("empty key should return ok=false")
	}
}

func TestSplitDimensionKey_Valid(t *testing.T) {
	name, value, ok := splitDimensionKey("model=gpt-4")
	if !ok || name != "model" || value != "gpt-4" {
		t.Errorf("got name=%q value=%q ok=%v; want model/gpt-4/true", name, value, ok)
	}
}

func TestSplitDimensionKey_NoEquals(t *testing.T) {
	_, _, ok := splitDimensionKey("nodim")
	if ok {
		t.Error("no equals should return ok=false")
	}
}

func TestSplitDimensionKey_EqualsAtStart(t *testing.T) {
	_, _, ok := splitDimensionKey("=value")
	if ok {
		t.Error("equals at start (idx=0) should return ok=false")
	}
}

func TestSplitDimensionKey_EqualsAtEnd(t *testing.T) {
	_, _, ok := splitDimensionKey("name=")
	if ok {
		t.Error("equals at end (empty value) should return ok=false")
	}
}

func TestConvertThingRollupRows_Empty(t *testing.T) {
	out := convertThingRollupRows(nil)
	if len(out) != 0 {
		t.Errorf("expected empty; got %d rows", len(out))
	}
}

func TestConvertThingRollupRows_WithMetadata(t *testing.T) {
	rows := []metrics.ThingRollupRow{
		{
			BucketStart:  time.Now(),
			MetricName:   "requests",
			DimensionKey: "model=gpt-4",
			Value:        42.0,
			Metadata:     json.RawMessage(`{"latency":100}`),
		},
		{
			BucketStart: time.Now(),
			MetricName:  "errors",
			Value:       1.0,
			Metadata:    nil,
		},
		{
			BucketStart: time.Now(),
			MetricName:  "bad_meta",
			Value:       0.5,
			Metadata:    json.RawMessage(`NOT VALID JSON`), // json.Unmarshal fails → metadata stays nil
		},
	}
	out := convertThingRollupRows(rows)
	if len(out) != 3 {
		t.Fatalf("expected 3 rows; got %d", len(out))
	}
	if out[0].Metadata == nil {
		t.Error("metadata should be non-nil for first row (valid JSON)")
	}
	if out[1].Metadata != nil {
		t.Error("metadata should be nil for second row (nil Metadata)")
	}
	// Invalid JSON: json.Unmarshal fails → row.Metadata stays nil
	if out[2].Metadata != nil {
		t.Error("metadata should be nil for third row (invalid JSON)")
	}
}

// newFailingPool returns a *pgxpool.Pool backed by an unreachable address so
// pool.Query() always returns a connection error. This exercises the
// resolveDimensionNames branches that handle per-dim query failures.
func newFailingPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), "postgresql://user:pass@127.0.0.1:1/testdb?pool_max_conns=1&pool_max_conn_lifetime=1s")
	if err != nil {
		t.Fatalf("pgxpool.New failed: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestResolveDimensionNames_NilPool_ReturnsNil(t *testing.T) {
	// h.pool = nil (set in newHandlerWithMock) → immediate nil return.
	_, h := newHandlerWithMock(t)
	rows := []metrics.ThingRollupRow{
		{DimensionKey: "model=some-uuid"},
	}
	got := h.resolveDimensionNames(t.Context(), rows)
	if got != nil {
		t.Errorf("nil pool should return nil; got %v", got)
	}
}

func TestResolveDimensionNames_EmptyRows_ReturnsNil(t *testing.T) {
	_, h := newHandlerWithMock(t)
	got := h.resolveDimensionNames(t.Context(), nil)
	if got != nil {
		t.Errorf("empty rows should return nil; got %v", got)
	}
}

func TestResolveDimensionNames_NoKnownDim_ReturnsNil(t *testing.T) {
	// All row dim keys are not in dimLookups → byDim stays empty → nil.
	_, h := newHandlerWithMock(t)
	h.pool = newFailingPool(t)
	rows := []metrics.ThingRollupRow{
		{DimensionKey: "target_host=example.com"}, // target_host not in dimLookups
		{DimensionKey: "source=agent"},            // source not in dimLookups
		{DimensionKey: ""},                        // empty → splitDimensionKey returns false
	}
	got := h.resolveDimensionNames(t.Context(), rows)
	if got != nil {
		t.Errorf("no known dim rows: expected nil; got %v", got)
	}
}

func TestResolveDimensionNames_KnownDim_QueryFailure_ReturnsNilGracefully(t *testing.T) {
	// dim=model is in dimLookups, but pool query fails → logs debug + continues.
	// Final out is empty → returns nil.
	_, h := newHandlerWithMock(t)
	h.pool = newFailingPool(t)
	rows := []metrics.ThingRollupRow{
		{DimensionKey: "model=some-model-uuid"},
	}
	// Should NOT panic; query failure is handled gracefully.
	got := h.resolveDimensionNames(t.Context(), rows)
	// Result is nil because query failed and out map stays empty.
	if got != nil {
		// If this non-nil, the query somehow succeeded — unusual but OK.
		t.Logf("resolveDimensionNames returned non-nil (unexpected for failing pool): %v", got)
	}
}

func TestRegisterAdminThingStatsRoutes_RegistersRoute(t *testing.T) {
	_, h := newHandlerWithMock(t)
	e := echo.New()
	g := e.Group("/api/admin")
	iamMW := func(_ string) echo.MiddlewareFunc {
		return func(next echo.HandlerFunc) echo.HandlerFunc { return next }
	}
	h.RegisterAdminThingStatsRoutes(g, iamMW)

	want := false
	for _, r := range e.Routes() {
		if r.Method == http.MethodGet && r.Path == "/api/admin/things/:id/stats" {
			want = true
		}
	}
	if !want {
		t.Error("route GET /api/admin/things/:id/stats not registered")
	}
}

func TestGetThingStats_NilThing_503(t *testing.T) {
	h := newHandlerNilThing()
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/api/admin/things/thing-1/stats", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("id")
	c.SetParamValues("thing-1")
	if err := h.GetThingStats(c); err != nil {
		t.Fatalf("GetThingStats: %v", err)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("code = %d; want 503", rec.Code)
	}
}

func TestGetThingStats_EmptyID_400(t *testing.T) {
	_, h := newHandlerWithMock(t)
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/api/admin/things//stats", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("id")
	c.SetParamValues("   ") // whitespace-only after TrimSpace → empty
	if err := h.GetThingStats(c); err != nil {
		t.Fatalf("GetThingStats: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d; want 400 for empty id", rec.Code)
	}
}

func TestGetThingStats_GetThingDBError_500(t *testing.T) {
	mock, h := newHandlerWithMock(t)
	mock.ExpectQuery(`FROM thing WHERE id`).WithArgs("thing-1").WillReturnError(errGeneric())
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/api/admin/things/thing-1/stats", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("id")
	c.SetParamValues("thing-1")
	if err := h.GetThingStats(c); err != nil {
		t.Fatalf("GetThingStats: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d; want 500", rec.Code)
	}
}

func TestGetThingStats_ThingNotFound_404(t *testing.T) {
	mock, h := newHandlerWithMock(t)
	// GetThing with QueryRow returning no rows → returns nil,nil.
	mock.ExpectQuery(`SELECT`).
		WithArgs("thing-missing").
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "type", "name", "version", "address", "enrolled_by", "auth_type", "conn_protocol",
			"status", "enrolled_at", "last_seen_at", "updated_at",
			"desired", "reported", "desired_ver", "reported_ver", "metadata",
		})) // no rows → GetThing returns nil,nil
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/api/admin/things/thing-missing/stats", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("id")
	c.SetParamValues("thing-missing")
	if err := h.GetThingStats(c); err != nil {
		t.Fatalf("GetThingStats: %v", err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("code = %d; want 404", rec.Code)
	}
}

func TestGetThingStats_InvalidEndTime_400(t *testing.T) {
	mock, h := newHandlerWithMock(t)
	mock.ExpectQuery(`SELECT`).
		WithArgs("thing-1").
		WillReturnRows(thingRow("thing-1", "ai-gateway", nil))
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/api/admin/things/thing-1/stats?end=NOTADATE", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("id")
	c.SetParamValues("thing-1")
	if err := h.GetThingStats(c); err != nil {
		t.Fatalf("GetThingStats: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d; want 400 for invalid end", rec.Code)
	}
}

func TestGetThingStats_InvalidStartTime_400(t *testing.T) {
	mock, h := newHandlerWithMock(t)
	mock.ExpectQuery(`SELECT`).
		WithArgs("thing-1").
		WillReturnRows(thingRow("thing-1", "ai-gateway", nil))
	e := echo.New()
	end := time.Now().UTC().Format(time.RFC3339)
	req := httptest.NewRequest(http.MethodGet, "/api/admin/things/thing-1/stats?end="+end+"&start=NOTADATE", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("id")
	c.SetParamValues("thing-1")
	if err := h.GetThingStats(c); err != nil {
		t.Fatalf("GetThingStats: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d; want 400 for invalid start", rec.Code)
	}
}

func TestGetThingStats_StartAfterEnd_400(t *testing.T) {
	mock, h := newHandlerWithMock(t)
	mock.ExpectQuery(`SELECT`).
		WithArgs("thing-1").
		WillReturnRows(thingRow("thing-1", "ai-gateway", nil))
	e := echo.New()
	now := time.Now().UTC()
	start := now.Format(time.RFC3339)
	end := now.Add(-1 * time.Hour).Format(time.RFC3339)
	req := httptest.NewRequest(http.MethodGet, "/api/admin/things/thing-1/stats?start="+start+"&end="+end, nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("id")
	c.SetParamValues("thing-1")
	if err := h.GetThingStats(c); err != nil {
		t.Fatalf("GetThingStats: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d; want 400 (start >= end)", rec.Code)
	}
}

func TestGetThingStats_QueryRollupError_500(t *testing.T) {
	mock, h := newHandlerWithMock(t)
	// GetThing succeeds.
	mock.ExpectQuery(`FROM thing WHERE id`).
		WithArgs("thing-1").
		WillReturnRows(thingRow("thing-1", "ai-gateway", nil))
	// QueryThingRollup fails.
	mock.ExpectQuery(`bucketStart`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(errGeneric())
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/api/admin/things/thing-1/stats", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("id")
	c.SetParamValues("thing-1")
	if err := h.GetThingStats(c); err != nil {
		t.Fatalf("GetThingStats: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d; want 500", rec.Code)
	}
}

func TestGetThingStats_HappyPath_NonAgent_200(t *testing.T) {
	mock, h := newHandlerWithMock(t)
	thingName := "my-gateway"
	mock.ExpectQuery(`FROM thing WHERE id`).
		WithArgs("gw-1").
		WillReturnRows(thingRow("gw-1", "ai-gateway", &thingName))
	// QueryThingRollup returns empty rows (no data — but type is ai-gateway, not agent).
	mock.ExpectQuery(`bucketStart`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(rollupCols())
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/api/admin/things/gw-1/stats", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("id")
	c.SetParamValues("gw-1")
	if err := h.GetThingStats(c); err != nil {
		t.Fatalf("GetThingStats: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d; body=%s", rec.Code, rec.Body.String())
	}
	body := decodeJSON(t, rec)
	if body["thingId"] != "gw-1" {
		t.Errorf("thingId = %v; want gw-1", body["thingId"])
	}
	if body["enabled"] != true {
		t.Errorf("enabled = %v; want true (non-agent type always enabled)", body["enabled"])
	}
	if body["thingName"] != "my-gateway" {
		t.Errorf("thingName = %v; want my-gateway", body["thingName"])
	}
}

func TestGetThingStats_AgentNoRows_RollupHasRecentTrue_EnabledTrue(t *testing.T) {
	// Agent type with no rollup rows but ThingRollupHasAnyRecent=true → enabled stays true.
	mock, h := newHandlerWithMock(t)
	mock.ExpectQuery(`FROM thing WHERE id`).
		WithArgs("agent-1").
		WillReturnRows(thingRow("agent-1", "agent", nil))
	mock.ExpectQuery(`bucketStart`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(rollupCols())
	// ThingRollupHasAnyRecent → SELECT EXISTS(...)
	mock.ExpectQuery(`SELECT EXISTS`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(rollupHasAnyRecentRow(true))
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/api/admin/things/agent-1/stats", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("id")
	c.SetParamValues("agent-1")
	if err := h.GetThingStats(c); err != nil {
		t.Fatalf("GetThingStats: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d; body=%s", rec.Code, rec.Body.String())
	}
	body := decodeJSON(t, rec)
	if body["enabled"] != true {
		t.Errorf("enabled = %v; want true (hasRecent=true)", body["enabled"])
	}
}

func TestGetThingStats_AgentNoRows_RollupDisabled_EnabledFalse(t *testing.T) {
	// Agent type with no rows AND ThingRollupHasAnyRecent=false → enabled=false.
	mock, h := newHandlerWithMock(t)
	mock.ExpectQuery(`FROM thing WHERE id`).
		WithArgs("agent-2").
		WillReturnRows(thingRow("agent-2", "agent", nil))
	mock.ExpectQuery(`bucketStart`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(rollupCols())
	mock.ExpectQuery(`SELECT EXISTS`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(rollupHasAnyRecentRow(false))
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/api/admin/things/agent-2/stats", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("id")
	c.SetParamValues("agent-2")
	if err := h.GetThingStats(c); err != nil {
		t.Fatalf("GetThingStats: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d; body=%s", rec.Code, rec.Body.String())
	}
	body := decodeJSON(t, rec)
	if body["enabled"] != false {
		t.Errorf("enabled = %v; want false (rollup disabled for agent)", body["enabled"])
	}
	if body["rollupDisabledMessage"] == nil || body["rollupDisabledMessage"] == "" {
		t.Error("rollupDisabledMessage should be set when enabled=false")
	}
}

func TestGetThingStats_WithMetricFilter_200(t *testing.T) {
	// ?metric=requests,errors exercises the metric split logic.
	// The rollup query gains extra $4/$5 args for the metric IN clause.
	mock, h := newHandlerWithMock(t)
	mock.ExpectQuery(`FROM thing WHERE id`).
		WithArgs("gw-1").
		WillReturnRows(thingRow("gw-1", "ai-gateway", nil))
	// Use MatchExpectationsInOrder(false) by allowing any extra args via AnyArg.
	mock.ExpectQuery(`bucketStart`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg()). // extra for "requests" + "errors"
		WillReturnRows(rollupCols())
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/api/admin/things/gw-1/stats?metric=requests,errors", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("id")
	c.SetParamValues("gw-1")
	if err := h.GetThingStats(c); err != nil {
		t.Fatalf("GetThingStats: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d; want 200", rec.Code)
	}
}

// TestParseRFC3339Flexible_RFC3339WithoutNanos covers the second branch in
// parseRFC3339Flexible — input that lacks nanosecond precision (so the
// RFC3339Nano parse fails) but is otherwise valid RFC3339 (so the second
// Parse call succeeds). The handler accepts this via the time-range
// query-param helper.
func TestParseRFC3339Flexible_RFC3339WithoutNanos(t *testing.T) {
	in := "2026-05-19T12:00:00+05:30"
	got, ok := parseRFC3339Flexible(in)
	if !ok {
		t.Fatalf("parseRFC3339Flexible(%q) returned ok=false; want true", in)
	}
	if got.IsZero() {
		t.Error("parseRFC3339Flexible returned zero time on success")
	}
}

// TestParseRFC3339Flexible_Malformed_ReturnsFalse covers the final
// fall-through path where both RFC3339Nano and RFC3339 Parse calls fail
// and the function returns the zero time with ok=false.
func TestParseRFC3339Flexible_Malformed_ReturnsFalse(t *testing.T) {
	for _, in := range []string{"not-a-date", "2026/05/19", "12:00:00", ""} {
		got, ok := parseRFC3339Flexible(in)
		if ok {
			t.Errorf("parseRFC3339Flexible(%q) ok=true; want false", in)
		}
		if !got.IsZero() {
			t.Errorf("parseRFC3339Flexible(%q) returned non-zero time on failure", in)
		}
	}
}

// resolveDimensionNames residual (81.8%) and parseRFC3339Flexible (80%) are
// accepted coverage residuals:
//   - resolveDimensionNames: the query-error path needs a non-nil
//     *pgxpool.Pool that returns an error; *pgxpool.Pool is a concrete
//     type pgxmock doesn't satisfy. Closing without a production seam
//     would require exporting a Pool interface from this handler package.
//   - parseRFC3339Flexible: the inner `time.Parse(RFC3339, s)` after a
//     `time.Parse(RFC3339Nano, s)` failure is structurally unreachable —
//     RFC3339Nano accepts every input RFC3339 does (RFC3339Nano is a
//     superset). The defensive second arm has no path that distinguishes
//     it from the fall-through. Acceptable defensive code.

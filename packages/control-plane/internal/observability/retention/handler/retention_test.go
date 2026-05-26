// Package observability — retention handler unit tests.
// Drive GetRetention and PutRetention through httptest + Echo with a
// pgxmock-backed opsstore.Store. Covers: nil-ops guards (503), DB errors
// (500), validation failures (400), and happy-path 200 responses.
package observability

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/pashagolub/pgxmock/v4"

	authn "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authn"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/observability/opsmetrics/opsstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
)

// Test helpers

func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newHandlerWithMock creates a Handler backed by a pgxmock pool. Includes a
// no-op audit.Writer (nil mq.Producer → Writer.Log returns nil) so the
// happy-path test can reach the audit.LogObserved call without panicking.
func newHandlerWithMock(t *testing.T) (pgxmock.PgxPoolIface, *Handler) {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mock.Close)
	noopAudit := audit.NewWriter(nil, "", silentLogger())
	return mock, New(Deps{Pool: mock, Logger: silentLogger(), Audit: noopAudit})
}

// newHandlerNilOps sets h.ops = nil directly (white-box) to reach the
// DB-nil guard without calling opsstore.New() with a real nil pool.
func newHandlerNilOps(t *testing.T) *Handler {
	t.Helper()
	h := New(Deps{Pool: nil, Logger: silentLogger()})
	h.ops = nil
	return h
}

func echoGET(path string) (echo.Context, *httptest.ResponseRecorder) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	return e.NewContext(req, rec), rec
}

func echoPUT(path string, body any) (echo.Context, *httptest.ResponseRecorder) {
	e := echo.New()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPut, path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	return e.NewContext(req, rec), rec
}

func decodeJSON(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode JSON: %v; raw=%s", err, rec.Body.String())
	}
	return m
}

func retentionCols() *pgxmock.Rows {
	return pgxmock.NewRows([]string{"layer", "retention_days", "updated_at"})
}

func errGeneric() error { return &genericErr{"simulated db error"} }

type genericErr struct{ msg string }

func (e *genericErr) Error() string { return e.msg }

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

// errJSON shape

func TestErrJSON_Shape(t *testing.T) {
	got := errJSON("test message", "validation_error", "CODE")
	errObj, _ := got["error"].(map[string]any)
	if errObj == nil {
		t.Fatal("no error envelope")
	}
	if errObj["message"] != "test message" || errObj["type"] != "validation_error" || errObj["code"] != "CODE" {
		t.Errorf("unexpected shape: %v", errObj)
	}
}

// actorFromContext — nil middleware.AdminAuth

func TestActorFromContext_NoAuth_ReturnsEmpty(t *testing.T) {
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
	middleware.WithAdminAuth(c, &authn.AdminAuth{KeyID: "user-123", KeyName: "Alice"})
	a := actorFromContext(c)
	if a.UserID != "user-123" || a.Name != "Alice" {
		t.Errorf("actor = %+v; want {user-123, Alice}", a)
	}
}

func TestSourceIP_ReturnsValue(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.1:5000"
	c := e.NewContext(req, httptest.NewRecorder())
	ip := sourceIP(c)
	if ip == "" {
		t.Error("sourceIP returned empty string")
	}
}

// internalServerError helper

func TestInternalServerError_Returns500(t *testing.T) {
	c, rec := echoGET("/")
	_ = internalServerError(c, "something failed")
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d; want 500", rec.Code)
	}
}

func TestRetentionRowsToMap_Empty(t *testing.T) {
	m := retentionRowsToMap(nil)
	if len(m) != 0 {
		t.Errorf("expected empty map; got %v", m)
	}
}

func TestRetentionRowsToMap_WithRows(t *testing.T) {
	rows := []opsstore.RetentionEntry{
		{Layer: "runtime_raw", RetentionDays: 7},
		{Layer: "business_raw", RetentionDays: 14},
	}
	m := retentionRowsToMap(rows)
	if m["runtime_raw"] != 7 || m["business_raw"] != 14 {
		t.Errorf("unexpected map: %v", m)
	}
}

func TestValidateRetentionUpdates_UnknownLayer_Error(t *testing.T) {
	err := validateRetentionUpdates(map[string]int{"nonexistent_layer": 7})
	if err == nil {
		t.Fatal("expected error for unknown layer")
	}
}

func TestValidateRetentionUpdates_BelowMin_Error(t *testing.T) {
	// runtime_raw has min=1, so 0 is out of range.
	err := validateRetentionUpdates(map[string]int{"runtime_raw": 0})
	if err == nil {
		t.Fatal("expected error for value below min")
	}
}

func TestValidateRetentionUpdates_AboveMax_Error(t *testing.T) {
	// runtime_raw has max=30, so 31 is out of range.
	err := validateRetentionUpdates(map[string]int{"runtime_raw": 31})
	if err == nil {
		t.Fatal("expected error for value above max")
	}
}

func TestValidateRetentionUpdates_ValidLayer_NoError(t *testing.T) {
	err := validateRetentionUpdates(map[string]int{"runtime_raw": 7})
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestValidateRetentionUpdates_AllLayers_HappyPath(t *testing.T) {
	// Each layer gets a value within its declared range.
	valid := map[string]int{
		"runtime_raw":  7,
		"business_raw": 14,
		"runtime_1h":   90,
		"business_1h":  180,
		"runtime_1d":   365,
		"business_1d":  730,
		"runtime_1mo":  730,
		"business_1mo": 1000,
		"diag_warn":    30,
		"diag_error":   60,
		"diag_fatal":   365,
	}
	if err := validateRetentionUpdates(valid); err != nil {
		t.Errorf("unexpected error for all-valid layers: %v", err)
	}
}

func TestRegisterObservabilityRetentionRoutes_RegistersRoutes(t *testing.T) {
	_, h := newHandlerWithMock(t)
	e := echo.New()
	g := e.Group("/api/admin")
	iamMW := func(_ string) echo.MiddlewareFunc {
		return func(next echo.HandlerFunc) echo.HandlerFunc { return next }
	}
	h.RegisterObservabilityRetentionRoutes(g, iamMW)

	want := map[string]bool{
		"GET /api/admin/observability/retention": false,
		"PUT /api/admin/observability/retention": false,
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

func TestGetRetention_NilOps_503(t *testing.T) {
	h := newHandlerNilOps(t)
	c, rec := echoGET("/api/admin/observability/retention")
	if err := h.GetRetention(c); err != nil {
		t.Fatalf("GetRetention: %v", err)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("code = %d; want 503", rec.Code)
	}
}

func TestGetRetention_DBError_500(t *testing.T) {
	mock, h := newHandlerWithMock(t)
	mock.ExpectQuery(`SELECT layer`).WillReturnError(errGeneric())
	c, rec := echoGET("/api/admin/observability/retention")
	if err := h.GetRetention(c); err != nil {
		t.Fatalf("GetRetention: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d; want 500", rec.Code)
	}
}

func TestGetRetention_NoRows_ReturnsEmptyRetentionMap(t *testing.T) {
	mock, h := newHandlerWithMock(t)
	mock.ExpectQuery(`SELECT layer`).WillReturnRows(retentionCols())
	c, rec := echoGET("/api/admin/observability/retention")
	if err := h.GetRetention(c); err != nil {
		t.Fatalf("GetRetention: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d; body=%s", rec.Code, rec.Body.String())
	}
	body := decodeJSON(t, rec)
	if body["retention"] == nil {
		t.Errorf("retention key missing; body=%s", rec.Body.String())
	}
}

func TestGetRetention_KnownLayers_RangeAnnotated(t *testing.T) {
	// Returned row with a known layer key gets its min/max populated.
	mock, h := newHandlerWithMock(t)
	now := time.Now()
	mock.ExpectQuery(`SELECT layer`).WillReturnRows(
		retentionCols().AddRow("runtime_raw", 7, now),
	)
	c, rec := echoGET("/api/admin/observability/retention")
	if err := h.GetRetention(c); err != nil {
		t.Fatalf("GetRetention: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d; body=%s", rec.Code, rec.Body.String())
	}
	body := decodeJSON(t, rec)
	retention, _ := body["retention"].(map[string]any)
	rr, _ := retention["runtime_raw"].(map[string]any)
	if rr == nil {
		t.Fatalf("runtime_raw key missing; retention=%v", retention)
	}
	// min=1, max=30 from retentionLayerRanges
	minVal, _ := rr["min"].(float64)
	maxVal, _ := rr["max"].(float64)
	if minVal != 1 || maxVal != 30 {
		t.Errorf("runtime_raw min/max = %v/%v; want 1/30", minVal, maxVal)
	}
}

func TestGetRetention_UnknownLayerInDB_ForwardCompat(t *testing.T) {
	// A layer not in retentionLayerRanges is surfaced with zero-valued bounds
	// rather than dropped (forward-compat path).
	mock, h := newHandlerWithMock(t)
	now := time.Now()
	mock.ExpectQuery(`SELECT layer`).WillReturnRows(
		retentionCols().AddRow("future_layer_x", 100, now),
	)
	c, rec := echoGET("/api/admin/observability/retention")
	if err := h.GetRetention(c); err != nil {
		t.Fatalf("GetRetention: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d; body=%s", rec.Code, rec.Body.String())
	}
	body := decodeJSON(t, rec)
	retention, _ := body["retention"].(map[string]any)
	if retention["future_layer_x"] == nil {
		t.Errorf("expected forward-compat layer in response; got %v", retention)
	}
}

func TestPutRetention_NilOps_503(t *testing.T) {
	h := newHandlerNilOps(t)
	c, rec := echoPUT("/api/admin/observability/retention", map[string]int{"runtime_raw": 7})
	if err := h.PutRetention(c); err != nil {
		t.Fatalf("PutRetention: %v", err)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("code = %d; want 503", rec.Code)
	}
}

func TestPutRetention_InvalidJSON_400(t *testing.T) {
	_, h := newHandlerWithMock(t)
	e := echo.New()
	req := httptest.NewRequest(http.MethodPut, "/api/admin/observability/retention", bytes.NewBufferString("not-json"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if err := h.PutRetention(c); err != nil {
		t.Fatalf("PutRetention: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d; want 400 for invalid JSON", rec.Code)
	}
}

func TestPutRetention_EmptyBody_400(t *testing.T) {
	_, h := newHandlerWithMock(t)
	c, rec := echoPUT("/api/admin/observability/retention", map[string]int{})
	if err := h.PutRetention(c); err != nil {
		t.Fatalf("PutRetention: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d; want 400 for empty body", rec.Code)
	}
}

func TestPutRetention_UnknownLayer_400(t *testing.T) {
	_, h := newHandlerWithMock(t)
	c, rec := echoPUT("/api/admin/observability/retention", map[string]int{"garbage_layer": 7})
	if err := h.PutRetention(c); err != nil {
		t.Fatalf("PutRetention: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d; want 400 for unknown layer", rec.Code)
	}
}

func TestPutRetention_OutOfRange_400(t *testing.T) {
	_, h := newHandlerWithMock(t)
	// runtime_raw max=30; 100 is out of range.
	c, rec := echoPUT("/api/admin/observability/retention", map[string]int{"runtime_raw": 100})
	if err := h.PutRetention(c); err != nil {
		t.Fatalf("PutRetention: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d; want 400 for out-of-range value", rec.Code)
	}
}

func TestPutRetention_ListBeforeUpdateDBError_500(t *testing.T) {
	// UpdateRetentionConfig fails (after ListRetentionConfig succeeds) → 500.
	// Exercises the error-arm of the update path.
	mock, h := newHandlerWithMock(t)
	mock.ExpectQuery(`SELECT layer`).WillReturnRows(retentionCols())
	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE metric_ops_retention_config`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(errGeneric())
	mock.ExpectRollback()

	c, rec := echoPUT("/api/admin/observability/retention", map[string]int{"runtime_raw": 7})
	if err := h.PutRetention(c); err != nil {
		t.Fatalf("PutRetention: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d; want 500", rec.Code)
	}
}

// TestPutRetention_Success_200 exercises the full happy-path including the
// audit.EntryFor call. The catalog declares [VerbRead, VerbWrite] for the
// observability resource (iam.VerbWrite), so the audit code is reached
// without panic.
func TestPutRetention_Success_200(t *testing.T) {
	mock, h := newHandlerWithMock(t)
	// ListRetentionConfig (before-snapshot) returns one existing row so the
	// audit BeforeState payload is non-empty.
	mock.ExpectQuery(`SELECT layer`).WillReturnRows(retentionCols().
		AddRow("runtime_raw", 14, time.Now()))
	// UpdateRetentionConfig: Begin + Exec succeed + Commit.
	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE metric_ops_retention_config`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectCommit()

	c, rec := echoPUT("/api/admin/observability/retention", map[string]int{"runtime_raw": 7})
	if err := h.PutRetention(c); err != nil {
		t.Fatalf("PutRetention: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d; want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	var body map[string]any
	_ = json.NewDecoder(rec.Body).Decode(&body)
	if body["ok"] != true {
		t.Errorf("expected ok=true, got %v", body["ok"])
	}
	if got, want := body["updated"], float64(1); got != want {
		t.Errorf("expected updated=%v, got %v", want, got)
	}
}

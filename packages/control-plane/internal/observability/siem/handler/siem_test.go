// Package siem — SIEM handler unit tests.
// Exercises GetSIEMConfig, UpdateSIEMConfig, TestSIEMConfig, and
// ListSIEMEventTypes through httptest + Echo with an in-memory
// pgxmock-backed systemmetastore. Covers: DB errors, nil-config default,
// header masking, format/URL validation, audit write, and route registration.
package siem

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/store/systemmetastore"
)

// Test helpers

func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// noopAuditWriter returns an audit.Writer that silently drops all entries
// (producer=nil path in writer.go — no panic, no MQ enqueue).
func noopAuditWriter() *audit.Writer {
	return audit.NewWriter(nil, "test-queue", silentLogger())
}

// newHandlerWithMock constructs a Handler backed by pgxmock so SQL
// expectations can be set without a real Postgres instance.
func newHandlerWithMock(t *testing.T) (pgxmock.PgxPoolIface, *Handler) {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mock.Close)
	db := systemmetastore.NewFromPool(mock)
	h := New(Deps{
		Meta:   db,
		Hub:    nil,
		Audit:  noopAuditWriter(),
		Logger: silentLogger(),
	})
	return mock, h
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

func echoPOST(path string, body any) (echo.Context, *httptest.ResponseRecorder) {
	e := echo.New()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
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

func errGeneric() error { return &genericErr{"simulated db error"} }

type genericErr struct{ msg string }

func (e *genericErr) Error() string { return e.msg }

// New / logger wiring

func TestNew_NilLogger_UsesDefault(t *testing.T) {
	h := New(Deps{Logger: nil})
	if h.logger == nil {
		t.Error("logger must not be nil")
	}
}

func TestNew_LoggerWired(t *testing.T) {
	lg := silentLogger()
	h := New(Deps{Logger: lg})
	if h.logger != lg {
		t.Error("logger not wired correctly")
	}
}

// errJSON / internalServerError / actorFromContext / sourceIP helpers

func TestErrJSON_Shape(t *testing.T) {
	got := errJSON("msg", "validation_error", "CODE")
	errObj, _ := got["error"].(map[string]any)
	if errObj == nil || errObj["message"] != "msg" || errObj["type"] != "validation_error" {
		t.Errorf("unexpected shape: %v", errObj)
	}
}

func TestInternalServerError_Returns500(t *testing.T) {
	c, rec := echoGET("/")
	_ = internalServerError(c, "oops")
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d; want 500", rec.Code)
	}
}

func TestActorFromContext_NoAuth_ReturnsEmpty(t *testing.T) {
	c, _ := echoGET("/")
	a := actorFromContext(c)
	if a.UserID != "" || a.Name != "" {
		t.Errorf("expected empty actor; got %+v", a)
	}
}

func TestSourceIP_ReturnsValue(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "1.2.3.4:9000"
	c := e.NewContext(req, httptest.NewRecorder())
	ip := sourceIP(c)
	if ip == "" {
		t.Error("sourceIP returned empty string")
	}
}

func TestRegisterSIEMRoutes_RegistersRoutes(t *testing.T) {
	_, h := newHandlerWithMock(t)
	e := echo.New()
	g := e.Group("/api/admin")
	iamMW := func(_ string) echo.MiddlewareFunc {
		return func(next echo.HandlerFunc) echo.HandlerFunc { return next }
	}
	h.RegisterSIEMRoutes(g, iamMW)

	want := map[string]bool{
		"GET /api/admin/settings/siem":             false,
		"PUT /api/admin/settings/siem":             false,
		"POST /api/admin/settings/siem/test":       false,
		"GET /api/admin/settings/siem/event-types": false,
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

func TestGetSIEMConfig_DBError_500(t *testing.T) {
	mock, h := newHandlerWithMock(t)
	mock.ExpectQuery(`SELECT value`).
		WithArgs("siem.config").
		WillReturnError(errGeneric())
	c, rec := echoGET("/api/admin/settings/siem")
	if err := h.GetSIEMConfig(c); err != nil {
		t.Fatalf("GetSIEMConfig: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d; want 500", rec.Code)
	}
}

func TestGetSIEMConfig_NoRow_ReturnsDefaultJSON(t *testing.T) {
	mock, h := newHandlerWithMock(t)
	// pgx.ErrNoRows is returned when key doesn't exist; GetSystemMetadata returns nil,nil.
	mock.ExpectQuery(`SELECT value`).
		WithArgs("siem.config").
		WillReturnRows(pgxmock.NewRows([]string{"value"})) // no rows → ErrNoRows
	c, rec := echoGET("/api/admin/settings/siem")
	if err := h.GetSIEMConfig(c); err != nil {
		t.Fatalf("GetSIEMConfig: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d; body=%s", rec.Code, rec.Body.String())
	}
	body := decodeJSON(t, rec)
	if body["format"] != "json" {
		t.Errorf("default format = %v; want json", body["format"])
	}
}

func TestGetSIEMConfig_WithConfig_ReturnsMaskedHeaders(t *testing.T) {
	// A config with auth headers must have them masked.
	cfg := SIEMConfig{
		Enabled: true,
		URL:     "https://siem.example.com",
		Format:  "cef",
		Headers: map[string]string{
			"Authorization": "Bearer supersecrettoken1234",
			"x-api-key":     "key", // ≤ 8 chars → masked as "****"
			"Content-Type":  "application/json",
		},
	}
	raw, _ := json.Marshal(cfg)

	mock, h := newHandlerWithMock(t)
	mock.ExpectQuery(`SELECT value`).
		WithArgs("siem.config").
		WillReturnRows(pgxmock.NewRows([]string{"value"}).AddRow(raw))
	c, rec := echoGET("/api/admin/settings/siem")
	if err := h.GetSIEMConfig(c); err != nil {
		t.Fatalf("GetSIEMConfig: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d; body=%s", rec.Code, rec.Body.String())
	}

	var out SIEMConfig
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Authorization header should be masked (len > 8)
	authMasked := out.Headers["Authorization"]
	if authMasked == "Bearer supersecrettoken1234" {
		t.Errorf("Authorization header was not masked")
	}
	// x-api-key is short (< 8 chars) → "****"
	if out.Headers["x-api-key"] != "****" {
		t.Errorf("x-api-key = %q; want ****", out.Headers["x-api-key"])
	}
	// Content-Type should pass through unchanged.
	if out.Headers["Content-Type"] != "application/json" {
		t.Errorf("Content-Type = %q; want application/json", out.Headers["Content-Type"])
	}
}

func TestGetSIEMConfig_InvalidJSON_ReturnsDefault(t *testing.T) {
	// Corrupt JSON in DB falls back to default format=json response.
	mock, h := newHandlerWithMock(t)
	mock.ExpectQuery(`SELECT value`).
		WithArgs("siem.config").
		WillReturnRows(pgxmock.NewRows([]string{"value"}).AddRow([]byte("not-json")))
	c, rec := echoGET("/api/admin/settings/siem")
	if err := h.GetSIEMConfig(c); err != nil {
		t.Fatalf("GetSIEMConfig: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d; want 200 (fallback)", rec.Code)
	}
}

func TestUpdateSIEMConfig_InvalidJSON_400(t *testing.T) {
	_, h := newHandlerWithMock(t)
	e := echo.New()
	req := httptest.NewRequest(http.MethodPut, "/api/admin/settings/siem",
		bytes.NewBufferString("not-json"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if err := h.UpdateSIEMConfig(c); err != nil {
		t.Fatalf("UpdateSIEMConfig: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d; want 400", rec.Code)
	}
}

func TestUpdateSIEMConfig_InvalidFormat_400(t *testing.T) {
	_, h := newHandlerWithMock(t)
	c, rec := echoPUT("/api/admin/settings/siem", SIEMConfig{Format: "xml"})
	if err := h.UpdateSIEMConfig(c); err != nil {
		t.Fatalf("UpdateSIEMConfig: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d; want 400 for invalid format", rec.Code)
	}
}

func TestUpdateSIEMConfig_InvalidURL_400(t *testing.T) {
	_, h := newHandlerWithMock(t)
	c, rec := echoPUT("/api/admin/settings/siem", SIEMConfig{URL: "ftp://example.com"})
	if err := h.UpdateSIEMConfig(c); err != nil {
		t.Fatalf("UpdateSIEMConfig: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d; want 400 for invalid URL scheme", rec.Code)
	}
}

func TestUpdateSIEMConfig_DBError_500(t *testing.T) {
	mock, h := newHandlerWithMock(t)
	mock.ExpectExec(`INSERT INTO system_metadata`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(errGeneric())
	c, rec := echoPUT("/api/admin/settings/siem", SIEMConfig{Format: "json", URL: "https://example.com"})
	if err := h.UpdateSIEMConfig(c); err != nil {
		t.Fatalf("UpdateSIEMConfig: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d; want 500", rec.Code)
	}
}

func TestUpdateSIEMConfig_HappyPath_200(t *testing.T) {
	// ResourceSettings has VerbUpdate in the IAM catalog — no panic.
	mock, h := newHandlerWithMock(t)
	mock.ExpectExec(`INSERT INTO system_metadata`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	c, rec := echoPUT("/api/admin/settings/siem", SIEMConfig{
		Format: "json",
		URL:    "https://example.com/siem",
	})
	if err := h.UpdateSIEMConfig(c); err != nil {
		t.Fatalf("UpdateSIEMConfig: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d; body=%s", rec.Code, rec.Body.String())
	}
	body := decodeJSON(t, rec)
	if body["ok"] != true {
		t.Errorf("ok = %v; want true", body["ok"])
	}
}

func TestUpdateSIEMConfig_NoURL_NoFormat_200(t *testing.T) {
	// Empty URL and format (both optional) passes validation.
	mock, h := newHandlerWithMock(t)
	mock.ExpectExec(`INSERT INTO system_metadata`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	c, rec := echoPUT("/api/admin/settings/siem", SIEMConfig{Enabled: true})
	if err := h.UpdateSIEMConfig(c); err != nil {
		t.Fatalf("UpdateSIEMConfig: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d; want 200", rec.Code)
	}
}

func TestTestSIEMConfig_NoConfig_400(t *testing.T) {
	// DB returns no row → "SIEM is not configured" 400.
	mock, h := newHandlerWithMock(t)
	mock.ExpectQuery(`SELECT value`).
		WithArgs("siem.config").
		WillReturnRows(pgxmock.NewRows([]string{"value"}))
	c, rec := echoPOST("/api/admin/settings/siem/test", nil)
	if err := h.TestSIEMConfig(c); err != nil {
		t.Fatalf("TestSIEMConfig: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d; want 400 (not configured)", rec.Code)
	}
}

func TestTestSIEMConfig_DBError_400(t *testing.T) {
	// DB error also falls into the "not configured" 400 branch.
	mock, h := newHandlerWithMock(t)
	mock.ExpectQuery(`SELECT value`).
		WithArgs("siem.config").
		WillReturnError(errGeneric())
	c, rec := echoPOST("/api/admin/settings/siem/test", nil)
	if err := h.TestSIEMConfig(c); err != nil {
		t.Fatalf("TestSIEMConfig: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d; want 400", rec.Code)
	}
}

func TestTestSIEMConfig_InvalidJSONInDB_400(t *testing.T) {
	// Config row present but corrupt → 400 "SIEM URL is not configured".
	mock, h := newHandlerWithMock(t)
	mock.ExpectQuery(`SELECT value`).
		WithArgs("siem.config").
		WillReturnRows(pgxmock.NewRows([]string{"value"}).AddRow([]byte("not-json")))
	c, rec := echoPOST("/api/admin/settings/siem/test", nil)
	if err := h.TestSIEMConfig(c); err != nil {
		t.Fatalf("TestSIEMConfig: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d; want 400", rec.Code)
	}
}

func TestTestSIEMConfig_NoURL_400(t *testing.T) {
	// Config with no URL → 400.
	cfg := SIEMConfig{Enabled: true, Format: "json"}
	raw, _ := json.Marshal(cfg)
	mock, h := newHandlerWithMock(t)
	mock.ExpectQuery(`SELECT value`).
		WithArgs("siem.config").
		WillReturnRows(pgxmock.NewRows([]string{"value"}).AddRow(raw))
	c, rec := echoPOST("/api/admin/settings/siem/test", nil)
	if err := h.TestSIEMConfig(c); err != nil {
		t.Fatalf("TestSIEMConfig: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d; want 400 (no URL)", rec.Code)
	}
}

func TestTestSIEMConfig_ConnectError_ReturnsOKFalse(t *testing.T) {
	// Config has a URL that cannot be dialed → 200 with ok=false.
	// Using a test HTTP server that closes immediately to trigger a
	// connection error is the preferred approach, but pointing to a
	// guaranteed-unreachable address works for the error-path branch.
	cfg := SIEMConfig{Enabled: true, URL: "http://127.0.0.1:1", Format: "json"}
	raw, _ := json.Marshal(cfg)
	mock, h := newHandlerWithMock(t)
	mock.ExpectQuery(`SELECT value`).
		WithArgs("siem.config").
		WillReturnRows(pgxmock.NewRows([]string{"value"}).AddRow(raw))
	c, rec := echoPOST("/api/admin/settings/siem/test", nil)
	if err := h.TestSIEMConfig(c); err != nil {
		t.Fatalf("TestSIEMConfig: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d; want 200 (ok=false for unreachable)", rec.Code)
	}
	body := decodeJSON(t, rec)
	if body["ok"] != false {
		t.Errorf("ok = %v; want false (connection refused)", body["ok"])
	}
}

func TestTestSIEMConfig_ServerError_ReturnsOKFalse(t *testing.T) {
	// Config points to a real test server that returns 500 → ok=false.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	cfg := SIEMConfig{Enabled: true, URL: ts.URL + "/ingest", Format: "json"}
	raw, _ := json.Marshal(cfg)
	mock, h := newHandlerWithMock(t)
	mock.ExpectQuery(`SELECT value`).
		WithArgs("siem.config").
		WillReturnRows(pgxmock.NewRows([]string{"value"}).AddRow(raw))
	c, rec := echoPOST("/api/admin/settings/siem/test", nil)
	if err := h.TestSIEMConfig(c); err != nil {
		t.Fatalf("TestSIEMConfig: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d; want 200 envelope", rec.Code)
	}
	body := decodeJSON(t, rec)
	if body["ok"] != false {
		t.Errorf("ok = %v; want false (HTTP 500 from SIEM)", body["ok"])
	}
}

func TestTestSIEMConfig_ServerOK_ReturnsOKTrue(t *testing.T) {
	// Config points to a test server returning 200 → ok=true.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	cfg := SIEMConfig{
		Enabled: true,
		URL:     ts.URL + "/ingest",
		Format:  "json",
		Headers: map[string]string{"x-custom": "val"},
	}
	raw, _ := json.Marshal(cfg)
	mock, h := newHandlerWithMock(t)
	mock.ExpectQuery(`SELECT value`).
		WithArgs("siem.config").
		WillReturnRows(pgxmock.NewRows([]string{"value"}).AddRow(raw))
	c, rec := echoPOST("/api/admin/settings/siem/test", nil)
	if err := h.TestSIEMConfig(c); err != nil {
		t.Fatalf("TestSIEMConfig: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d; want 200", rec.Code)
	}
	body := decodeJSON(t, rec)
	if body["ok"] != true {
		t.Errorf("ok = %v; want true", body["ok"])
	}
}

func TestListSIEMEventTypes_ReturnsNonEmpty(t *testing.T) {
	_, h := newHandlerWithMock(t)
	c, rec := echoGET("/api/admin/settings/siem/event-types")
	if err := h.ListSIEMEventTypes(c); err != nil {
		t.Fatalf("ListSIEMEventTypes: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d; want 200", rec.Code)
	}
	body := decodeJSON(t, rec)
	types, _ := body["eventTypes"].([]any)
	if len(types) == 0 {
		t.Errorf("expected non-empty eventTypes list; got %v", types)
	}
	// Verify each entry has type, resource, service fields.
	for i, et := range types {
		row, _ := et.(map[string]any)
		if row == nil {
			t.Errorf("[%d] not an object", i)
			continue
		}
		if row["type"] == nil || row["resource"] == nil || row["service"] == nil {
			t.Errorf("[%d] missing field; got %v", i, row)
		}
	}
}

func TestListSIEMEventTypes_IncludesTrafficTypes(t *testing.T) {
	// The 3 hard-coded traffic.* types must always be present.
	_, h := newHandlerWithMock(t)
	c, rec := echoGET("/api/admin/settings/siem/event-types")
	_ = h.ListSIEMEventTypes(c)

	body := decodeJSON(t, rec)
	types, _ := body["eventTypes"].([]any)
	trafficTypes := map[string]bool{
		"traffic.rate_limited":    false,
		"traffic.budget_exceeded": false,
		"traffic.request_blocked": false,
	}
	for _, et := range types {
		row, _ := et.(map[string]any)
		if t2, ok := row["type"].(string); ok {
			if _, want := trafficTypes[t2]; want {
				trafficTypes[t2] = true
			}
		}
	}
	for k, found := range trafficTypes {
		if !found {
			t.Errorf("traffic type %q not found in event types", k)
		}
	}
}

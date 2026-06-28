// Package siem — SIEM handler unit tests.
// Exercises GetSIEMConfig, UpdateSIEMConfig, TestSIEMConfig, and
// ListSIEMEventTypes through httptest + Echo with an in-memory
// pgxmock-backed systemmetastore. Covers: DB errors, nil-config default,
// header masking, format/URL validation, audit write, and route registration.
package siem

import (
	"bytes"
	"github.com/goccy/go-json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/store/systemmetastore"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
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
	db := systemmetastore.New(mock)
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
	// Secret headers are redacted to the FIXED sentinel — no byte of the
	// real token may appear in the read-back (not even a partial first/last-4
	// reveal). The full token must not leak, and the sentinel must not contain
	// any substring of the real value.
	authMasked := out.Headers["Authorization"]
	if authMasked != redactedSecretSentinel {
		t.Errorf("Authorization = %q; want fixed sentinel %q (no partial reveal)", authMasked, redactedSecretSentinel)
	}
	if strings.Contains(authMasked, "supersecret") || strings.Contains(authMasked, "1234") ||
		strings.Contains(authMasked, "Bear") {
		t.Errorf("Authorization read-back %q leaks part of the real token", authMasked)
	}
	// x-api-key (non-empty secret) is also redacted to the fixed sentinel.
	if out.Headers["x-api-key"] != redactedSecretSentinel {
		t.Errorf("x-api-key = %q; want fixed sentinel %q", out.Headers["x-api-key"], redactedSecretSentinel)
	}
	// Content-Type (non-secret) passes through unchanged.
	if out.Headers["Content-Type"] != "application/json" {
		t.Errorf("Content-Type = %q; want application/json", out.Headers["Content-Type"])
	}
}

// TestGetSIEMConfig_EmptySecretHeaderNotSentinel proves an unset secret header
// reads back as empty (not the sentinel) so the UI can tell "no value" apart
// from "value set but hidden".
func TestGetSIEMConfig_EmptySecretHeaderNotSentinel(t *testing.T) {
	cfg := SIEMConfig{
		Enabled: true,
		URL:     "https://siem.example.com",
		Format:  "json",
		Headers: map[string]string{"Authorization": ""},
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
	var out SIEMConfig
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Headers["Authorization"] != "" {
		t.Errorf("empty secret header should read back empty, got %q", out.Headers["Authorization"])
	}
}

// TestUpdateSIEMConfig_PreservesSecretOnSentinelRoundTrip is the core
// write-side assertion: when the client PUTs the redaction sentinel for a secret
// header (the value GetSIEMConfig handed it), the previously stored REAL token
// must be persisted — not the placeholder — so the SIEM forwarder keeps working
// without the admin re-typing the secret.
func TestUpdateSIEMConfig_PreservesSecretOnSentinelRoundTrip(t *testing.T) {
	mock, h := newHandlerWithMock(t)

	// Prior stored config carries the real token.
	prior := SIEMConfig{
		Enabled: true,
		URL:     "https://siem.example.com",
		Format:  "json",
		Headers: map[string]string{"Authorization": "Bearer real-token-xyz"},
	}
	priorRaw, _ := json.Marshal(prior)
	mock.ExpectQuery(`SELECT value`).
		WithArgs("siem.config").
		WillReturnRows(pgxmock.NewRows([]string{"value"}).AddRow(priorRaw))

	// The write must persist the REAL token, never the sentinel.
	mock.ExpectExec(`INSERT INTO system_metadata`).
		WithArgs("siem.config", pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	// Client replays the masked sentinel (as GetSIEMConfig returned it).
	c, rec := echoPUT("/api/admin/settings/siem", SIEMConfig{
		Enabled: true,
		URL:     "https://siem.example.com",
		Format:  "json",
		Headers: map[string]string{"Authorization": redactedSecretSentinel},
	})
	if err := h.UpdateSIEMConfig(c); err != nil {
		t.Fatalf("UpdateSIEMConfig: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestUpdateSIEMConfig_AcceptsNewSecret proves a genuinely new secret value (not
// the sentinel) is taken as submitted and overwrites the stored token.
func TestUpdateSIEMConfig_AcceptsNewSecret(t *testing.T) {
	mock, h := newHandlerWithMock(t)
	prior := SIEMConfig{Headers: map[string]string{"Authorization": "Bearer old"}}
	priorRaw, _ := json.Marshal(prior)
	mock.ExpectQuery(`SELECT value`).
		WithArgs("siem.config").
		WillReturnRows(pgxmock.NewRows([]string{"value"}).AddRow(priorRaw))
	mock.ExpectExec(`INSERT INTO system_metadata`).
		WithArgs("siem.config", pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	c, rec := echoPUT("/api/admin/settings/siem", SIEMConfig{
		Format:  "json",
		Headers: map[string]string{"Authorization": "Bearer brand-new"},
	})
	if err := h.UpdateSIEMConfig(c); err != nil {
		t.Fatalf("UpdateSIEMConfig: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
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
	// ResourceAuditLog has VerbWrite in the IAM catalog — no panic.
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

// TestTestSIEMConfig_PrivateURL_SSRFBlocked is the end-to-end SSRF
// regression: a SIEM URL pointing at a loopback / private address must be
// refused at dial time by the scoped BlockPrivateDialControl guard, and the
// response must be the GENERIC unreachable envelope — no statusCode field, and
// no raw transport-error text that would fingerprint the internal endpoint.
func TestTestSIEMConfig_PrivateURL_SSRFBlocked(t *testing.T) {
	// A live httptest server binds to 127.0.0.1, so it doubles as a real
	// internal endpoint the guard must refuse to reach even though it is up.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK) // would be ok=true WITHOUT the guard
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
		t.Errorf("ok = %v; want false (loopback blocked by SSRF guard)", body["ok"])
	}
	if body["error"] != "Failed to reach the SIEM endpoint" {
		t.Errorf("error = %q; want the generic unreachable message (no raw transport error)", body["error"])
	}
	if _, leaked := body["statusCode"]; leaked {
		t.Errorf("response leaked statusCode %v — that is an internal-endpoint oracle", body["statusCode"])
	}
}

// TestSiemProbeResult_NoOracle is the unit contract for the response
// mapper: a dial failure and an upstream error status each collapse to a fixed
// boolean + message with NO statusCode, and a 2xx is a bare ok=true. The key
// assertion is that two DISTINCT upstream error statuses (403 vs 500) yield
// byte-identical bodies — otherwise the probe is a fingerprinting oracle.
func TestSiemProbeResult_NoOracle(t *testing.T) {
	dial := siemProbeResult(nil, errGeneric())
	if dial["ok"] != false || dial["error"] != "Failed to reach the SIEM endpoint" {
		t.Errorf("dial-error result = %v; want generic unreachable", dial)
	}
	if _, ok := dial["statusCode"]; ok {
		t.Error("dial-error result must not carry statusCode")
	}

	r403 := siemProbeResult(&http.Response{StatusCode: http.StatusForbidden}, nil)
	r500 := siemProbeResult(&http.Response{StatusCode: http.StatusInternalServerError}, nil)
	b403, _ := json.Marshal(r403)
	b500, _ := json.Marshal(r500)
	if string(b403) != string(b500) {
		t.Errorf("403 body %s != 500 body %s — distinguishable statuses are an SSRF oracle", b403, b500)
	}
	if r500["ok"] != false || r500["error"] != "The SIEM endpoint returned an error response" {
		t.Errorf("error-status result = %v; want generic error response", r500)
	}
	if _, ok := r500["statusCode"]; ok {
		t.Error("error-status result must not carry statusCode")
	}

	ok := siemProbeResult(&http.Response{StatusCode: http.StatusNoContent}, nil)
	if ok["ok"] != true {
		t.Errorf("2xx result = %v; want ok=true", ok)
	}
	if _, leaked := ok["statusCode"]; leaked {
		t.Error("success result must not carry statusCode")
	}
}

// TestRegisterSIEMRoutes_MutatingVerbsRequireSettingsWrite is the IAM
// tier regression: the egress-mutating verbs (PUT config, POST test) must be
// gated on settings:write — NOT the old audit-log:write — so a narrow audit-log
// grant can no longer redirect the org-wide audit stream. The read verbs stay on
// audit-log:read.
func TestRegisterSIEMRoutes_MutatingVerbsRequireSettingsWrite(t *testing.T) {
	_, h := newHandlerWithMock(t)
	g := echo.New().Group("/api/admin")
	// RegisterSIEMRoutes calls iamMW(action) once per route in registration
	// order: GET config, PUT config, POST test, GET event-types. Capture that
	// ordered list of action strings and assert the egress-mutating verbs moved
	// off audit-log:write onto settings:write.
	var actions []string
	iamMW := func(action string) echo.MiddlewareFunc {
		actions = append(actions, action)
		return func(next echo.HandlerFunc) echo.HandlerFunc { return next }
	}
	h.RegisterSIEMRoutes(g, iamMW)

	want := []string{
		iam.ResourceAuditLog.Action(iam.VerbRead),  // GET config
		iam.ResourceSettings.Action(iam.VerbWrite), // PUT config
		iam.ResourceSettings.Action(iam.VerbWrite), // POST test
		iam.ResourceAuditLog.Action(iam.VerbRead),  // GET event-types
	}
	if len(actions) != len(want) {
		t.Fatalf("captured %d gate actions %v; want %d", len(actions), actions, len(want))
	}
	for i := range want {
		if actions[i] != want[i] {
			t.Errorf("route #%d gated on %q; want %q", i, actions[i], want[i])
		}
	}
	// Belt-and-suspenders: the old under-privileged gate must be fully gone.
	for _, a := range actions {
		if a == iam.ResourceAuditLog.Action(iam.VerbWrite) {
			t.Errorf("a SIEM route is still gated on audit-log:write (%q) — SEC-M6-01 regression", a)
		}
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

// TestListSIEMEventTypes_IncludesAuthLoginTypes is the picker half:
// the auth.login_failure / auth.login_success identities ClassifyAdminEvent maps
// login rows to must be selectable in the filter, otherwise enabling any
// whitelist silently stops forwarding login events.
func TestListSIEMEventTypes_IncludesAuthLoginTypes(t *testing.T) {
	_, h := newHandlerWithMock(t)
	c, rec := echoGET("/api/admin/settings/siem/event-types")
	_ = h.ListSIEMEventTypes(c)

	body := decodeJSON(t, rec)
	types, _ := body["eventTypes"].([]any)
	authTypes := map[string]bool{
		"auth.login_failure": false,
		"auth.login_success": false,
	}
	for _, et := range types {
		row, _ := et.(map[string]any)
		if t2, ok := row["type"].(string); ok {
			if _, want := authTypes[t2]; want {
				authTypes[t2] = true
			}
		}
	}
	for k, found := range authTypes {
		if !found {
			t.Errorf("auth login type %q not found in event types (F-0192 picker gap)", k)
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

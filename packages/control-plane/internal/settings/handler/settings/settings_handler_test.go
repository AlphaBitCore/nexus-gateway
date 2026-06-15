// Settings handler unit tests. Drive every method through httptest +
// Echo with a pgxmock-backed store.DB for system_metadata SQL and an
// injected listIdPsFn for device-auth IdP enumeration. Verifies HTTP
// shape, request validation, DB error wrapping, audit emission, and
// the agent.config.version side effect for UpdateAgentSettings.
package settings

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/labstack/echo/v4"
	"github.com/pashagolub/pgxmock/v4"

	auth "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authn"
	authserver_store "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/store/systemmetastore"
)

// Test plumbing

// silentLogger discards output so error-path tests don't spam stderr.
func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// auditSpy implements mq.Producer (Publish/Enqueue/Close) so the
// audit.Writer publishes into an in-memory buffer instead of NATS.
type auditSpy struct {
	mu    sync.Mutex
	calls [][]byte
}

func (a *auditSpy) Publish(context.Context, string, []byte) error { return nil }
func (a *auditSpy) Enqueue(_ context.Context, _ string, data []byte) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	cp := make([]byte, len(data))
	copy(cp, data)
	a.calls = append(a.calls, cp)
	return nil
}
func (a *auditSpy) Close() error { return nil }

func (a *auditSpy) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.calls)
}

// newHandlerWithMock wires a settings.Handler with:
//   - a pgxmock-backed *systemmetastore.Store (for system_metadata Get/Set);
//   - an in-memory audit spy;
//   - a silent logger;
//   - listIdPsFn left nil so the caller can override per-test.
func newHandlerWithMock(t *testing.T) (pgxmock.PgxPoolIface, *Handler, *auditSpy) {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mock.Close)
	aud := &auditSpy{}
	h := New(Deps{
		Meta:   systemmetastore.New(mock),
		Audit:  audit.NewWriter(aud, "nexus.event.admin-audit", silentLogger()),
		Logger: silentLogger(),
	})
	return mock, h, aud
}

// adminCtx builds an Echo context with WithAdminAuth populated so the
// handler stamps updatedBy onto SetSystemMetadata.
func adminCtx(req *http.Request, rec *httptest.ResponseRecorder, keyID, keyName string) echo.Context {
	e := echo.New()
	c := e.NewContext(req, rec)
	middleware.WithAdminAuth(c, &auth.AdminAuth{
		KeyID:             keyID,
		KeyName:           keyName,
		AuthPrincipalType: "admin_user",
	})
	return c
}

// anonCtx builds an Echo context with NO AdminAuth attached so the
// updatedBy branch in UpdateSettings / UpdateAgentSettings falls
// through to the empty-string default.
func anonCtx(req *http.Request, rec *httptest.ResponseRecorder) echo.Context {
	e := echo.New()
	return e.NewContext(req, rec)
}

// jsonReq returns a JSON-typed request so Echo's binder picks the
// JSON decoder instead of falling back to form binding (which would
// silently accept malformed JSON as an empty struct).
func jsonReq(method, path string, body string) *http.Request {
	r := httptest.NewRequest(method, path, bytes.NewReader([]byte(body)))
	r.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	return r
}

// decodeJSON unmarshals a recorder body into a map for assertions.
func decodeJSON(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode body: %v; raw=%s", err, rec.Body.String())
	}
	return m
}

// errEnvelopeType pulls error.type out of a 4xx/5xx envelope.
func errEnvelopeType(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	m := decodeJSON(t, rec)
	errObj, _ := m["error"].(map[string]any)
	if errObj == nil {
		t.Fatalf("no error envelope; body=%s", rec.Body.String())
	}
	s, _ := errObj["type"].(string)
	return s
}

// errJSON / internalServerError / incrementConfigVersion helpers

func TestErrJSON_Shape(t *testing.T) {
	got := errJSON("msg", "validation_error", "BAD")
	errObj, _ := got["error"].(map[string]any)
	if errObj == nil {
		t.Fatalf("missing error key; got %v", got)
	}
	if errObj["message"] != "msg" || errObj["type"] != "validation_error" || errObj["code"] != "BAD" {
		t.Errorf("unexpected envelope: %v", errObj)
	}
}

func TestInternalServerError_Writes500(t *testing.T) {
	req := jsonReq(http.MethodGet, "/x", "")
	rec := httptest.NewRecorder()
	c := anonCtx(req, rec)
	if err := internalServerError(c, "boom"); err != nil {
		t.Fatalf("internalServerError: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d; want 500", rec.Code)
	}
	if errEnvelopeType(t, rec) != "server_error" {
		t.Errorf("type = %q; want server_error", errEnvelopeType(t, rec))
	}
}

// incrementConfigVersion swallows DB errors by design. Cover both the
// "no prior version" path (Get returns ErrNoRows → version starts at 0)
// and the "prior version present" path.
func TestIncrementConfigVersion_NoPrior_SetsOne(t *testing.T) {
	mock, h, _ := newHandlerWithMock(t)
	mock.ExpectQuery(`SELECT value FROM system_metadata`).
		WithArgs("agent.config.version").
		WillReturnError(pgx.ErrNoRows)
	mock.ExpectExec(`INSERT INTO system_metadata`).
		WithArgs("agent.config.version", []byte(`1`), "system").
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	h.incrementConfigVersion(context.Background())
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestIncrementConfigVersion_WithPrior_Increments(t *testing.T) {
	mock, h, _ := newHandlerWithMock(t)
	mock.ExpectQuery(`SELECT value FROM system_metadata`).
		WithArgs("agent.config.version").
		WillReturnRows(pgxmock.NewRows([]string{"value"}).AddRow([]byte(`7`)))
	mock.ExpectExec(`INSERT INTO system_metadata`).
		WithArgs("agent.config.version", []byte(`8`), "system").
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	h.incrementConfigVersion(context.Background())
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestIncrementConfigVersion_SetError_LoggedNotPropagated(t *testing.T) {
	mock, h, _ := newHandlerWithMock(t)
	mock.ExpectQuery(`SELECT value FROM system_metadata`).
		WithArgs("agent.config.version").
		WillReturnRows(pgxmock.NewRows([]string{"value"}).AddRow([]byte(`3`)))
	mock.ExpectExec(`INSERT INTO system_metadata`).
		WithArgs("agent.config.version", []byte(`4`), "system").
		WillReturnError(errors.New("planner err"))
	// Must not panic and must not propagate — incrementConfigVersion
	// is fire-and-forget by contract.
	h.incrementConfigVersion(context.Background())
}

func TestIncrementConfigVersion_GarbageValue_RestartsFromOne(t *testing.T) {
	mock, h, _ := newHandlerWithMock(t)
	// Non-integer payload — handler falls back to version=0, increments to 1.
	mock.ExpectQuery(`SELECT value FROM system_metadata`).
		WithArgs("agent.config.version").
		WillReturnRows(pgxmock.NewRows([]string{"value"}).AddRow([]byte(`"not-a-number"`)))
	mock.ExpectExec(`INSERT INTO system_metadata`).
		WithArgs("agent.config.version", []byte(`1`), "system").
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	h.incrementConfigVersion(context.Background())
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// New() + RegisterRoutes

func TestNew_FieldsWired(t *testing.T) {
	meta := systemmetastore.New(nil)
	aud := &auditSpy{}
	w := audit.NewWriter(aud, "q", silentLogger())
	lg := silentLogger()
	h := New(Deps{Meta: meta, Audit: w, Logger: lg})
	if h.meta != meta || h.audit != w || h.logger != lg {
		t.Errorf("New did not wire deps: %+v", h)
	}
	if h.listIdPsFn != nil {
		t.Errorf("listIdPsFn must default to nil; production path uses concrete pool")
	}
}

func TestRegisterRoutes_RegistersAllSixEndpoints(t *testing.T) {
	_, h, _ := newHandlerWithMock(t)
	e := echo.New()
	g := e.Group("/api/admin")
	// iamMW returns a no-op middleware — RegisterRoutes only cares about
	// being invoked, not about IAM behaviour.
	iamMW := func(action string) echo.MiddlewareFunc {
		return func(next echo.HandlerFunc) echo.HandlerFunc { return next }
	}
	h.RegisterRoutes(g, iamMW)
	routes := e.Routes()
	want := map[string]bool{
		"GET /api/admin/settings":                 false,
		"PUT /api/admin/settings":                 false,
		"GET /api/admin/settings/device-auth":     false,
		"PUT /api/admin/settings/device-auth":     false,
		"GET /api/admin/settings/device-defaults": false,
		"PUT /api/admin/settings/device-defaults": false,
	}
	for _, r := range routes {
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

// GetSettings / UpdateSettings (settings.go)

func TestGetSettings_DefaultsWhenNoMetadata(t *testing.T) {
	mock, h, _ := newHandlerWithMock(t)
	mock.ExpectQuery(`SELECT value FROM system_metadata`).
		WithArgs("gateway.settings").
		WillReturnError(pgx.ErrNoRows)

	req := jsonReq(http.MethodGet, "/api/admin/settings", "")
	rec := httptest.NewRecorder()
	if err := h.GetSettings(adminCtx(req, rec, "k1", "Admin")); err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d; want 200", rec.Code)
	}
	body := decodeJSON(t, rec)
	for _, k := range []string{"maintenanceMode", "logLevel", "defaultHookTimeout", "defaultFailBehavior", "uptime", "version", "goVersion"} {
		if _, ok := body[k]; !ok {
			t.Errorf("response missing %s; body=%v", k, body)
		}
	}
	if body["goVersion"] != runtime.Version() {
		t.Errorf("goVersion = %v; want %v", body["goVersion"], runtime.Version())
	}
	if body["maintenanceMode"] != false || body["logLevel"] != "info" {
		t.Errorf("defaults not applied: %v", body)
	}
}

func TestGetSettings_VersionFromEnv(t *testing.T) {
	t.Setenv("APP_VERSION", "v1.2.3")
	mock, h, _ := newHandlerWithMock(t)
	mock.ExpectQuery(`SELECT value FROM system_metadata`).
		WithArgs("gateway.settings").WillReturnError(pgx.ErrNoRows)

	req := jsonReq(http.MethodGet, "/api/admin/settings", "")
	rec := httptest.NewRecorder()
	if err := h.GetSettings(adminCtx(req, rec, "k1", "Admin")); err != nil {
		t.Fatal(err)
	}
	if got := decodeJSON(t, rec)["version"]; got != "v1.2.3" {
		t.Errorf("version = %v; want v1.2.3", got)
	}
}

func TestGetSettings_VersionFallsBackToDevWhenEnvUnset(t *testing.T) {
	_ = os.Unsetenv("APP_VERSION")
	mock, h, _ := newHandlerWithMock(t)
	mock.ExpectQuery(`SELECT value FROM system_metadata`).
		WithArgs("gateway.settings").WillReturnError(pgx.ErrNoRows)

	req := jsonReq(http.MethodGet, "/api/admin/settings", "")
	rec := httptest.NewRecorder()
	_ = h.GetSettings(adminCtx(req, rec, "k1", "Admin"))
	if got := decodeJSON(t, rec)["version"]; got != "dev" {
		t.Errorf("version = %v; want dev", got)
	}
}

func TestGetSettings_StoredOverridesDefaults(t *testing.T) {
	mock, h, _ := newHandlerWithMock(t)
	mock.ExpectQuery(`SELECT value FROM system_metadata`).
		WithArgs("gateway.settings").
		WillReturnRows(pgxmock.NewRows([]string{"value"}).
			AddRow([]byte(`{"maintenanceMode":true,"logLevel":"debug"}`)))

	req := jsonReq(http.MethodGet, "/api/admin/settings", "")
	rec := httptest.NewRecorder()
	_ = h.GetSettings(adminCtx(req, rec, "k1", "Admin"))
	body := decodeJSON(t, rec)
	if body["maintenanceMode"] != true {
		t.Errorf("maintenanceMode = %v; want true", body["maintenanceMode"])
	}
	if body["logLevel"] != "debug" {
		t.Errorf("logLevel = %v; want debug", body["logLevel"])
	}
	// Defaults still applied for keys the stored blob does not override.
	if body["defaultHookTimeout"] != float64(5000) {
		t.Errorf("defaultHookTimeout = %v; want 5000", body["defaultHookTimeout"])
	}
}

func TestGetSettings_GarbageStoredBlobFallsBackToDefaults(t *testing.T) {
	mock, h, _ := newHandlerWithMock(t)
	mock.ExpectQuery(`SELECT value FROM system_metadata`).
		WithArgs("gateway.settings").
		WillReturnRows(pgxmock.NewRows([]string{"value"}).
			AddRow([]byte(`"not-an-object"`)))

	req := jsonReq(http.MethodGet, "/api/admin/settings", "")
	rec := httptest.NewRecorder()
	_ = h.GetSettings(adminCtx(req, rec, "k1", "Admin"))
	body := decodeJSON(t, rec)
	if body["logLevel"] != "info" {
		t.Errorf("logLevel = %v; want info (default)", body["logLevel"])
	}
}

func TestUpdateSettings_InvalidJSON_400(t *testing.T) {
	_, h, _ := newHandlerWithMock(t)
	req := jsonReq(http.MethodPut, "/api/admin/settings", `{not json`)
	rec := httptest.NewRecorder()
	if err := h.UpdateSettings(adminCtx(req, rec, "k1", "Admin")); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d; want 400", rec.Code)
	}
	if errEnvelopeType(t, rec) != "validation_error" {
		t.Errorf("type = %q", errEnvelopeType(t, rec))
	}
}

func TestUpdateSettings_HappyPath_PersistsAndAudits(t *testing.T) {
	mock, h, aud := newHandlerWithMock(t)
	// loadSettings (before-state) — return empty.
	mock.ExpectQuery(`SELECT value FROM system_metadata`).
		WithArgs("gateway.settings").WillReturnError(pgx.ErrNoRows)
	// loadSettings (current snapshot before mutation).
	mock.ExpectQuery(`SELECT value FROM system_metadata`).
		WithArgs("gateway.settings").WillReturnError(pgx.ErrNoRows)
	// Upsert with merged values; updatedBy = "k1".
	mock.ExpectExec(`INSERT INTO system_metadata`).
		WithArgs("gateway.settings", pgxmock.AnyArg(), "k1").
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	// GetSettings(after-update) loads the value again.
	mock.ExpectQuery(`SELECT value FROM system_metadata`).
		WithArgs("gateway.settings").WillReturnError(pgx.ErrNoRows)

	body := `{
		"maintenanceMode":true,
		"logLevel":"warn",
		"defaultHookTimeout":1500,
		"defaultFailBehavior":"closed"
	}`
	req := jsonReq(http.MethodPut, "/api/admin/settings", body)
	rec := httptest.NewRecorder()
	if err := h.UpdateSettings(adminCtx(req, rec, "k1", "Admin")); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d; body=%s", rec.Code, rec.Body.String())
	}
	if aud.count() != 1 {
		t.Errorf("audit calls = %d; want 1", aud.count())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestUpdateSettings_IgnoresUnknownLogLevel_AndOutOfRangeTimeout(t *testing.T) {
	mock, h, _ := newHandlerWithMock(t)
	mock.ExpectQuery(`SELECT value FROM system_metadata`).WithArgs("gateway.settings").WillReturnError(pgx.ErrNoRows)
	mock.ExpectQuery(`SELECT value FROM system_metadata`).WithArgs("gateway.settings").WillReturnError(pgx.ErrNoRows)
	mock.ExpectExec(`INSERT INTO system_metadata`).
		WithArgs("gateway.settings", pgxmock.AnyArg(), "k1").
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	mock.ExpectQuery(`SELECT value FROM system_metadata`).WithArgs("gateway.settings").WillReturnError(pgx.ErrNoRows)

	// All four fields invalid → handler keeps defaults silently.
	body := `{
		"logLevel":"yelling",
		"defaultHookTimeout":0,
		"defaultFailBehavior":"sometimes"
	}`
	req := jsonReq(http.MethodPut, "/api/admin/settings", body)
	rec := httptest.NewRecorder()
	if err := h.UpdateSettings(adminCtx(req, rec, "k1", "Admin")); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d; want 200 (handler silently drops bad values)", rec.Code)
	}
	got := decodeJSON(t, rec)
	if got["logLevel"] != "info" {
		t.Errorf("logLevel = %v; want info (rejected bad value)", got["logLevel"])
	}
	if got["defaultFailBehavior"] != "open" {
		t.Errorf("defaultFailBehavior = %v; want open", got["defaultFailBehavior"])
	}
}

func TestUpdateSettings_TimeoutOverCeiling_Rejected(t *testing.T) {
	mock, h, _ := newHandlerWithMock(t)
	mock.ExpectQuery(`SELECT value FROM system_metadata`).WithArgs("gateway.settings").WillReturnError(pgx.ErrNoRows)
	mock.ExpectQuery(`SELECT value FROM system_metadata`).WithArgs("gateway.settings").WillReturnError(pgx.ErrNoRows)
	mock.ExpectExec(`INSERT INTO system_metadata`).
		WithArgs("gateway.settings", pgxmock.AnyArg(), "k1").
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	mock.ExpectQuery(`SELECT value FROM system_metadata`).WithArgs("gateway.settings").WillReturnError(pgx.ErrNoRows)

	body := `{"defaultHookTimeout":99999}`
	req := jsonReq(http.MethodPut, "/api/admin/settings", body)
	rec := httptest.NewRecorder()
	_ = h.UpdateSettings(adminCtx(req, rec, "k1", "Admin"))
	got := decodeJSON(t, rec)
	if got["defaultHookTimeout"] != float64(5000) {
		t.Errorf("defaultHookTimeout = %v; want 5000 (rejected over-ceiling)", got["defaultHookTimeout"])
	}
}

func TestUpdateSettings_SetMetadataFailure_500(t *testing.T) {
	mock, h, _ := newHandlerWithMock(t)
	mock.ExpectQuery(`SELECT value FROM system_metadata`).WithArgs("gateway.settings").WillReturnError(pgx.ErrNoRows)
	mock.ExpectQuery(`SELECT value FROM system_metadata`).WithArgs("gateway.settings").WillReturnError(pgx.ErrNoRows)
	mock.ExpectExec(`INSERT INTO system_metadata`).
		WithArgs("gateway.settings", pgxmock.AnyArg(), "k1").
		WillReturnError(errors.New("disk full"))

	req := jsonReq(http.MethodPut, "/api/admin/settings", `{"logLevel":"debug"}`)
	rec := httptest.NewRecorder()
	if err := h.UpdateSettings(adminCtx(req, rec, "k1", "Admin")); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d; want 500", rec.Code)
	}
	if errEnvelopeType(t, rec) != "server_error" {
		t.Errorf("type = %q", errEnvelopeType(t, rec))
	}
}

func TestUpdateSettings_AnonContext_UpdatedByIsEmpty(t *testing.T) {
	mock, h, _ := newHandlerWithMock(t)
	mock.ExpectQuery(`SELECT value FROM system_metadata`).WithArgs("gateway.settings").WillReturnError(pgx.ErrNoRows)
	mock.ExpectQuery(`SELECT value FROM system_metadata`).WithArgs("gateway.settings").WillReturnError(pgx.ErrNoRows)
	mock.ExpectExec(`INSERT INTO system_metadata`).
		WithArgs("gateway.settings", pgxmock.AnyArg(), ""). // empty updatedBy
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	mock.ExpectQuery(`SELECT value FROM system_metadata`).WithArgs("gateway.settings").WillReturnError(pgx.ErrNoRows)

	req := jsonReq(http.MethodPut, "/api/admin/settings", `{"logLevel":"debug"}`)
	rec := httptest.NewRecorder()
	if err := h.UpdateSettings(anonCtx(req, rec)); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// GetAgentSettings / UpdateAgentSettings (agent_settings.go)

func TestMapBool_Helpers(t *testing.T) {
	m := map[string]any{"k": true, "wrongType": "x"}
	if !mapBool(m, "k", false) {
		t.Errorf("mapBool present should be true")
	}
	if mapBool(m, "missing", false) {
		t.Errorf("mapBool missing default false")
	}
	if !mapBool(m, "wrongType", true) {
		t.Errorf("mapBool wrong-type returns fallback")
	}
}

func TestMapString_Helpers(t *testing.T) {
	if mapString(map[string]any{"k": "v"}, "k") != "v" {
		t.Errorf("mapString happy")
	}
	if mapString(map[string]any{"k": 42}, "k") != "" {
		t.Errorf("mapString wrong type → empty")
	}
}

func TestMapStringSlice_Helpers(t *testing.T) {
	got := mapStringSlice(map[string]any{"k": []any{"a", "", "b", 42, "c"}}, "k")
	want := []string{"a", "b", "c"} // drops empties + non-strings
	if len(got) != len(want) {
		t.Fatalf("len = %d; want %d (got=%v)", len(got), len(want), got)
	}
	for i, v := range want {
		if got[i] != v {
			t.Errorf("got[%d] = %q; want %q", i, got[i], v)
		}
	}
	// missing key → empty (not nil — must marshal as []).
	empty := mapStringSlice(map[string]any{}, "missing")
	if empty == nil {
		t.Errorf("mapStringSlice missing must not return nil")
	}
	if len(empty) != 0 {
		t.Errorf("len = %d; want 0", len(empty))
	}
	// wrong type → empty.
	if got := mapStringSlice(map[string]any{"k": "not-a-slice"}, "k"); len(got) != 0 || got == nil {
		t.Errorf("wrong type → empty slice; got %v", got)
	}
}

func TestGetAgentSettings_NoStoredBlob_SurfacesDefaults(t *testing.T) {
	mock, h, _ := newHandlerWithMock(t)
	mock.ExpectQuery(`SELECT value FROM system_metadata`).
		WithArgs("agent.settings").WillReturnError(pgx.ErrNoRows)

	req := jsonReq(http.MethodGet, "/api/admin/settings/device-defaults", "")
	rec := httptest.NewRecorder()
	if err := h.GetAgentSettings(adminCtx(req, rec, "k1", "Admin")); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	got := decodeJSON(t, rec)
	for _, k := range []string{
		"quitAllowed", "shutdownWarningEnabled",
		"autoUpdateEnabled", "autoUpdateChannel",
		"trafficUploadLevel", "themeId",
		"forceQUICFallbackBundles",
		"attestationEnabled",
	} {
		if _, ok := got[k]; !ok {
			t.Errorf("response missing %s", k)
		}
	}
	// attestationEnabled defaults false when never set — the toggle must
	// stay off until an admin opts in (perf optimization, not security gate).
	if got["attestationEnabled"] != false {
		t.Errorf("attestationEnabled default = %v; want false", got["attestationEnabled"])
	}
	// These 5 dead fields must not appear in the GET response: the agent
	// runtime ignores them and they are not stored.
	for _, dead := range []string{
		"logLevel", "heartbeatIntervalSec", "auditDrainIntervalSec",
		"configSyncIntervalSec", "auditBatchSize",
	} {
		if _, present := got[dead]; present {
			t.Errorf("response should not surface dead field %q; got %v", dead, got[dead])
		}
	}
	// forceQUICFallbackBundles must serialise as [] not null.
	if arr, ok := got["forceQUICFallbackBundles"].([]any); !ok || arr == nil {
		t.Errorf("forceQUICFallbackBundles = %v; want []", got["forceQUICFallbackBundles"])
	}
	if _, present := got["shutdownWarning"]; present {
		t.Errorf("shutdownWarning should be absent when not in stored blob; got %v", got["shutdownWarning"])
	}
}

func TestGetAgentSettings_StoredValuesSurfaceThrough(t *testing.T) {
	mock, h, _ := newHandlerWithMock(t)
	// The blob includes legacy dead keys to lock GET behaviour:
	// even when an older row carries them, GET must NOT surface them.
	blob := `{
		"quitAllowed":true,
		"shutdownWarningEnabled":true,
		"heartbeatIntervalSec":60,
		"auditDrainIntervalSec":30,
		"configSyncIntervalSec":120,
		"auditBatchSize":500,
		"autoUpdateEnabled":true,
		"autoUpdateChannel":"beta",
		"logLevel":"debug",
		"trafficUploadLevel":"all",
		"themeId":"corp-dark",
		"forceQUICFallbackBundles":["com.apple.Safari","com.openai.chatgpt"],
		"shutdownWarning":{"en":"Stop","zh":"停止"}
	}`
	mock.ExpectQuery(`SELECT value FROM system_metadata`).
		WithArgs("agent.settings").
		WillReturnRows(pgxmock.NewRows([]string{"value"}).AddRow([]byte(blob)))

	req := jsonReq(http.MethodGet, "/api/admin/settings/device-defaults", "")
	rec := httptest.NewRecorder()
	_ = h.GetAgentSettings(adminCtx(req, rec, "k1", "Admin"))
	got := decodeJSON(t, rec)
	if got["quitAllowed"] != true {
		t.Errorf("quitAllowed = %v", got["quitAllowed"])
	}
	if got["autoUpdateChannel"] != "beta" {
		t.Errorf("autoUpdateChannel = %v; want beta", got["autoUpdateChannel"])
	}
	bundles, _ := got["forceQUICFallbackBundles"].([]any)
	if len(bundles) != 2 {
		t.Errorf("forceQUICFallbackBundles len = %d; want 2", len(bundles))
	}
	sw, ok := got["shutdownWarning"].(map[string]any)
	if !ok {
		t.Fatalf("shutdownWarning missing; got %v", got["shutdownWarning"])
	}
	if sw["en"] != "Stop" || sw["zh"] != "停止" {
		t.Errorf("shutdownWarning = %v", sw)
	}
	// Dead keys must NOT round-trip through GET, even when present in the
	// stored blob.
	for _, dead := range []string{
		"logLevel", "heartbeatIntervalSec", "auditDrainIntervalSec",
		"configSyncIntervalSec", "auditBatchSize",
	} {
		if _, present := got[dead]; present {
			t.Errorf("dead field %q must not surface through GET; got %v", dead, got[dead])
		}
	}
}

func TestGetAgentSettings_DropsNonStringShutdownWarning(t *testing.T) {
	mock, h, _ := newHandlerWithMock(t)
	// The "en" key is fine, but "fr" is a number which must be skipped
	// silently (the agent renders strings only).
	blob := `{"shutdownWarning":{"en":"Stop","fr":42}}`
	mock.ExpectQuery(`SELECT value FROM system_metadata`).
		WithArgs("agent.settings").
		WillReturnRows(pgxmock.NewRows([]string{"value"}).AddRow([]byte(blob)))
	req := jsonReq(http.MethodGet, "/api/admin/settings/device-defaults", "")
	rec := httptest.NewRecorder()
	_ = h.GetAgentSettings(adminCtx(req, rec, "k1", "Admin"))
	sw, _ := decodeJSON(t, rec)["shutdownWarning"].(map[string]any)
	if _, present := sw["fr"]; present {
		t.Errorf("fr non-string should have been dropped; got %v", sw)
	}
}

func TestGetAgentSettings_DBError_500(t *testing.T) {
	mock, h, _ := newHandlerWithMock(t)
	mock.ExpectQuery(`SELECT value FROM system_metadata`).
		WithArgs("agent.settings").
		WillReturnError(errors.New("planner err"))

	req := jsonReq(http.MethodGet, "/api/admin/settings/device-defaults", "")
	rec := httptest.NewRecorder()
	if err := h.GetAgentSettings(adminCtx(req, rec, "k1", "Admin")); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d; want 500", rec.Code)
	}
}

func TestUpdateAgentSettings_InvalidJSON_400(t *testing.T) {
	_, h, _ := newHandlerWithMock(t)
	req := jsonReq(http.MethodPut, "/api/admin/settings/device-defaults", `{not json`)
	rec := httptest.NewRecorder()
	if err := h.UpdateAgentSettings(adminCtx(req, rec, "k1", "Admin")); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d; want 400", rec.Code)
	}
	if errEnvelopeType(t, rec) != "validation_error" {
		t.Errorf("type = %q", errEnvelopeType(t, rec))
	}
}

func TestUpdateAgentSettings_RejectsBadAutoUpdateChannel(t *testing.T) {
	_, h, _ := newHandlerWithMock(t)
	req := jsonReq(http.MethodPut, "/api/admin/settings/device-defaults",
		`{"autoUpdateChannel":"alpha"}`)
	rec := httptest.NewRecorder()
	_ = h.UpdateAgentSettings(adminCtx(req, rec, "k1", "Admin"))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d; want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "autoUpdateChannel") {
		t.Errorf("body should mention autoUpdateChannel: %s", rec.Body.String())
	}
}

func TestUpdateAgentSettings_RejectsBadTrafficUploadLevel(t *testing.T) {
	_, h, _ := newHandlerWithMock(t)
	req := jsonReq(http.MethodPut, "/api/admin/settings/device-defaults",
		`{"trafficUploadLevel":"sometimes"}`)
	rec := httptest.NewRecorder()
	_ = h.UpdateAgentSettings(adminCtx(req, rec, "k1", "Admin"))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d; want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "trafficUploadLevel") {
		t.Errorf("body should mention trafficUploadLevel: %s", rec.Body.String())
	}
}

func TestUpdateAgentSettings_RejectsThemeIDTooLong(t *testing.T) {
	_, h, _ := newHandlerWithMock(t)
	longID := strings.Repeat("x", 65)
	req := jsonReq(http.MethodPut, "/api/admin/settings/device-defaults",
		`{"themeId":"`+longID+`"}`)
	rec := httptest.NewRecorder()
	_ = h.UpdateAgentSettings(adminCtx(req, rec, "k1", "Admin"))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d; want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "64") {
		t.Errorf("body should mention 64-char limit: %s", rec.Body.String())
	}
}

func TestUpdateAgentSettings_RejectsThemeIDNonASCII(t *testing.T) {
	_, h, _ := newHandlerWithMock(t)
	req := jsonReq(http.MethodPut, "/api/admin/settings/device-defaults",
		`{"themeId":"corp-é"}`) // é is non-ASCII
	rec := httptest.NewRecorder()
	_ = h.UpdateAgentSettings(adminCtx(req, rec, "k1", "Admin"))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d; want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "printable ASCII") {
		t.Errorf("body should mention printable ASCII: %s", rec.Body.String())
	}
}

func TestUpdateAgentSettings_AttestationEnabled_RoundTrips(t *testing.T) {
	// PATCH with attestationEnabled=true must persist into the merged blob
	// and surface in the response. Other fields omitted from the body must
	// NOT be zeroed (pointer-typed bind contract).
	mock, h, _ := newHandlerWithMock(t)
	existing := `{"quitAllowed":true,"autoUpdateEnabled":true,"autoUpdateChannel":"stable"}`
	mock.ExpectQuery(`SELECT value FROM system_metadata`).
		WithArgs("agent.settings").
		WillReturnRows(pgxmock.NewRows([]string{"value"}).AddRow([]byte(existing)))
	mock.ExpectExec(`INSERT INTO system_metadata`).
		WithArgs("agent.settings", pgxmock.AnyArg(), "k1").
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	mock.ExpectQuery(`SELECT value FROM system_metadata`).
		WithArgs("agent.config.version").WillReturnError(pgx.ErrNoRows)
	mock.ExpectExec(`INSERT INTO system_metadata`).
		WithArgs("agent.config.version", []byte(`1`), "system").
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))

	req := jsonReq(http.MethodPut, "/api/admin/settings/device-defaults",
		`{"attestationEnabled":true}`)
	rec := httptest.NewRecorder()
	if err := h.UpdateAgentSettings(adminCtx(req, rec, "k1", "Admin")); err != nil {
		t.Fatalf("UpdateAgentSettings: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d; body=%s", rec.Code, rec.Body.String())
	}
	got := decodeJSON(t, rec)
	if got["attestationEnabled"] != true {
		t.Errorf("attestationEnabled = %v; want true", got["attestationEnabled"])
	}
	// Pointer-typed bind contract: omitted fields must keep their stored
	// values (regression guard for accidental zero-out).
	if got["quitAllowed"] != true {
		t.Errorf("quitAllowed lost on partial PATCH: %v", got["quitAllowed"])
	}
	if got["autoUpdateChannel"] != "stable" {
		t.Errorf("autoUpdateChannel lost on partial PATCH: %v", got["autoUpdateChannel"])
	}
}

func TestUpdateAgentSettings_AttestationEnabled_OmittedKeepsStored(t *testing.T) {
	// Regression guard: when attestationEnabled is absent from the PATCH
	// body the stored value (true) must survive — pointer-typed bind
	// distinguishes "absent" from "false".
	mock, h, _ := newHandlerWithMock(t)
	existing := `{"attestationEnabled":true,"quitAllowed":false}`
	mock.ExpectQuery(`SELECT value FROM system_metadata`).
		WithArgs("agent.settings").
		WillReturnRows(pgxmock.NewRows([]string{"value"}).AddRow([]byte(existing)))
	mock.ExpectExec(`INSERT INTO system_metadata`).
		WithArgs("agent.settings", pgxmock.AnyArg(), "k1").
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	mock.ExpectQuery(`SELECT value FROM system_metadata`).
		WithArgs("agent.config.version").WillReturnError(pgx.ErrNoRows)
	mock.ExpectExec(`INSERT INTO system_metadata`).
		WithArgs("agent.config.version", []byte(`1`), "system").
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))

	req := jsonReq(http.MethodPut, "/api/admin/settings/device-defaults",
		`{"quitAllowed":true}`)
	rec := httptest.NewRecorder()
	if err := h.UpdateAgentSettings(adminCtx(req, rec, "k1", "Admin")); err != nil {
		t.Fatalf("UpdateAgentSettings: %v", err)
	}
	got := decodeJSON(t, rec)
	if got["attestationEnabled"] != true {
		t.Errorf("attestationEnabled lost when omitted from body: %v", got["attestationEnabled"])
	}
}

func TestUpdateAgentSettings_RejectsForceQUICEntryTooLong(t *testing.T) {
	_, h, _ := newHandlerWithMock(t)
	long := strings.Repeat("a", 201)
	body := `{"forceQUICFallbackBundles":["` + long + `"]}`
	req := jsonReq(http.MethodPut, "/api/admin/settings/device-defaults", body)
	rec := httptest.NewRecorder()
	_ = h.UpdateAgentSettings(adminCtx(req, rec, "k1", "Admin"))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d; want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "200 chars") {
		t.Errorf("body should mention 200 chars: %s", rec.Body.String())
	}
}

func TestUpdateAgentSettings_RejectsForceQUICEntryWithWhitespace(t *testing.T) {
	_, h, _ := newHandlerWithMock(t)
	body := `{"forceQUICFallbackBundles":["bundle with space"]}`
	req := jsonReq(http.MethodPut, "/api/admin/settings/device-defaults", body)
	rec := httptest.NewRecorder()
	_ = h.UpdateAgentSettings(adminCtx(req, rec, "k1", "Admin"))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d; want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "printable ASCII") {
		t.Errorf("body should mention printable ASCII: %s", rec.Body.String())
	}
}

func TestUpdateAgentSettings_RejectsTooManyForceQUICEntries(t *testing.T) {
	_, h, _ := newHandlerWithMock(t)
	entries := make([]string, 65)
	for i := range entries {
		entries[i] = `"b` + strings.Repeat("x", 3) + string(rune('a'+i%26)) + string(rune('0'+i%10)) + `"`
	}
	body := `{"forceQUICFallbackBundles":[` + strings.Join(entries, ",") + `]}`
	req := jsonReq(http.MethodPut, "/api/admin/settings/device-defaults", body)
	rec := httptest.NewRecorder()
	_ = h.UpdateAgentSettings(adminCtx(req, rec, "k1", "Admin"))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d; want 400 (over 64-entry cap)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "64 entries") {
		t.Errorf("body should mention 64 entries: %s", rec.Body.String())
	}
}

// TestUpdateAgentSettings_RejectsForceQUICSystemBundle closes SEC-M8-01 at the
// CP write path: naming a system networking daemon directly must be rejected
// before it can reach the agent_settings shadow, so a low-priv admin cannot
// close that daemon's UDP fleet-wide.
func TestUpdateAgentSettings_RejectsForceQUICSystemBundle(t *testing.T) {
	_, h, _ := newHandlerWithMock(t)
	body := `{"forceQUICFallbackBundles":["com.apple.apsd"]}`
	req := jsonReq(http.MethodPut, "/api/admin/settings/device-defaults", body)
	rec := httptest.NewRecorder()
	_ = h.UpdateAgentSettings(adminCtx(req, rec, "k1", "Admin"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d; want 400 (system daemon must be rejected): %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "protected system service") || !strings.Contains(rec.Body.String(), "com.apple.apsd") {
		t.Errorf("body should name the protected service: %s", rec.Body.String())
	}
}

// TestUpdateAgentSettings_RejectsForceQUICOverBroadPrefix is the headline
// attack: a bare "com.apple" entry would prefix-match every com.apple.* daemon
// in the NE kill check, taking down DNS/DHCP/Push/Continuity. It must be
// rejected even though no exact system bundle is named.
func TestUpdateAgentSettings_RejectsForceQUICOverBroadPrefix(t *testing.T) {
	_, h, _ := newHandlerWithMock(t)
	body := `{"forceQUICFallbackBundles":["com.apple"]}`
	req := jsonReq(http.MethodPut, "/api/admin/settings/device-defaults", body)
	rec := httptest.NewRecorder()
	_ = h.UpdateAgentSettings(adminCtx(req, rec, "k1", "Admin"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d; want 400 (over-broad prefix must be rejected): %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "protected system service") {
		t.Errorf("body should flag the protected-service violation: %s", rec.Body.String())
	}
}

func TestUpdateAgentSettings_RejectsBypassEntryTooLong(t *testing.T) {
	_, h, _ := newHandlerWithMock(t)
	body := `{"bypassBundles":["` + strings.Repeat("a", 201) + `"]}`
	req := jsonReq(http.MethodPut, "/api/admin/settings/device-defaults", body)
	rec := httptest.NewRecorder()
	_ = h.UpdateAgentSettings(adminCtx(req, rec, "k1", "Admin"))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d; want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "200 chars") {
		t.Errorf("body should mention 200 chars: %s", rec.Body.String())
	}
}

func TestUpdateAgentSettings_RejectsBypassEntryWithWhitespace(t *testing.T) {
	_, h, _ := newHandlerWithMock(t)
	body := `{"bypassBundles":["bundle with space"]}`
	req := jsonReq(http.MethodPut, "/api/admin/settings/device-defaults", body)
	rec := httptest.NewRecorder()
	_ = h.UpdateAgentSettings(adminCtx(req, rec, "k1", "Admin"))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d; want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "printable ASCII") {
		t.Errorf("body should mention printable ASCII: %s", rec.Body.String())
	}
}

func TestUpdateAgentSettings_RejectsTooManyBypassEntries(t *testing.T) {
	_, h, _ := newHandlerWithMock(t)
	entries := make([]string, 65)
	for i := range entries {
		entries[i] = `"b` + strings.Repeat("x", 3) + string(rune('a'+i%26)) + string(rune('0'+i%10)) + `"`
	}
	body := `{"bypassBundles":[` + strings.Join(entries, ",") + `]}`
	req := jsonReq(http.MethodPut, "/api/admin/settings/device-defaults", body)
	rec := httptest.NewRecorder()
	_ = h.UpdateAgentSettings(adminCtx(req, rec, "k1", "Admin"))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d; want 400 (over 64-entry cap)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "64 entries") {
		t.Errorf("body should mention 64 entries: %s", rec.Body.String())
	}
}

// TestUpdateAgentSettings_BypassAllowsSystemBundle pins the deliberate
// difference from forceQUICFallbackBundles: a system daemon entry is a
// HARMLESS no-op for the bypass list (exempting it only removes it from
// inspection — it cannot close any UDP), so the handler must ACCEPT it
// rather than reject it the way the QUIC-kill path does.
func TestUpdateAgentSettings_BypassAllowsSystemBundle(t *testing.T) {
	mock, h, _ := newHandlerWithMock(t)
	mock.ExpectQuery(`SELECT value FROM system_metadata`).
		WithArgs("agent.settings").WillReturnError(pgx.ErrNoRows)
	mock.ExpectExec(`INSERT INTO system_metadata`).
		WithArgs("agent.settings", pgxmock.AnyArg(), "k1").
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	mock.ExpectQuery(`SELECT value FROM system_metadata`).
		WithArgs("agent.config.version").WillReturnError(pgx.ErrNoRows)
	mock.ExpectExec(`INSERT INTO system_metadata`).
		WithArgs("agent.config.version", []byte(`1`), "system").
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))

	body := `{"bypassBundles":["com.apple.apsd","com.anthropic.claude-code"]}`
	req := jsonReq(http.MethodPut, "/api/admin/settings/device-defaults", body)
	rec := httptest.NewRecorder()
	if err := h.UpdateAgentSettings(adminCtx(req, rec, "k1", "Admin")); err != nil {
		t.Fatalf("UpdateAgentSettings: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d; want 200 (system bundle is a valid bypass target): %s", rec.Code, rec.Body.String())
	}
	got := decodeJSON(t, rec)
	arr, _ := got["bypassBundles"].([]any)
	if len(arr) != 2 {
		t.Errorf("bypassBundles = %v; want both entries persisted", got["bypassBundles"])
	}
}

// TestUpdateAgentSettings_HappyPath_StripsDeadFieldsAndIncrementsConfigVersion
// drives the full mutation pipeline. Pins:
//   - load existing blob
//   - SetSystemMetadata("agent.settings", ...)
//   - incrementConfigVersion (GET + SET on agent.config.version)
//   - audit emission
//   - trimmed themeId stored
//   - dedupe + empty-drop in forceQUICFallbackBundles
//   - legacy dead fields posted in the body get silently stripped
//     and do NOT round-trip into the response.
func TestUpdateAgentSettings_HappyPath_StripsDeadFieldsAndIncrementsConfigVersion(t *testing.T) {
	mock, h, aud := newHandlerWithMock(t)

	// 1. Load existing agent.settings blob (empty).
	mock.ExpectQuery(`SELECT value FROM system_metadata`).
		WithArgs("agent.settings").WillReturnError(pgx.ErrNoRows)
	// 2. Persist merged blob.
	mock.ExpectExec(`INSERT INTO system_metadata`).
		WithArgs("agent.settings", pgxmock.AnyArg(), "k1").
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	// 3. incrementConfigVersion: Get + Set agent.config.version.
	mock.ExpectQuery(`SELECT value FROM system_metadata`).
		WithArgs("agent.config.version").WillReturnError(pgx.ErrNoRows)
	mock.ExpectExec(`INSERT INTO system_metadata`).
		WithArgs("agent.config.version", []byte(`1`), "system").
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))

	body := `{
		"quitAllowed":true,
		"shutdownWarningEnabled":true,
		"shutdownWarning":{"en":"Stop"},
		"heartbeatIntervalSec":5,
		"auditDrainIntervalSec":999999,
		"configSyncIntervalSec":0,
		"auditBatchSize":99999,
		"autoUpdateEnabled":true,
		"autoUpdateChannel":"stable",
		"logLevel":"info",
		"trafficUploadLevel":"processed",
		"themeId":"  corp-dark  ",
		"forceQUICFallbackBundles":["com.apple.Safari","","com.apple.Safari","com.openai.chatgpt"]
	}`
	req := jsonReq(http.MethodPut, "/api/admin/settings/device-defaults", body)
	rec := httptest.NewRecorder()
	if err := h.UpdateAgentSettings(adminCtx(req, rec, "k1", "Admin")); err != nil {
		t.Fatalf("UpdateAgentSettings: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d; body=%s", rec.Code, rec.Body.String())
	}
	got := decodeJSON(t, rec)
	// themeId trimmed.
	if got["themeId"] != "corp-dark" {
		t.Errorf("themeId = %v; want corp-dark (trimmed)", got["themeId"])
	}
	// Bundles: deduped + empties dropped → [Safari, chatgpt].
	bundles, _ := got["forceQUICFallbackBundles"].([]any)
	if len(bundles) != 2 {
		t.Errorf("forceQUICFallbackBundles len = %d; want 2 (deduped); got %v", len(bundles), bundles)
	}
	// Dead fields posted in body get silently stripped — neither
	// the agent runtime acknowledges them, so they must
	// not round-trip into the persisted blob or the response.
	for _, dead := range []string{
		"logLevel", "heartbeatIntervalSec", "auditDrainIntervalSec",
		"configSyncIntervalSec", "auditBatchSize",
	} {
		if _, present := got[dead]; present {
			t.Errorf("dead field %q must be stripped; got %v", dead, got[dead])
		}
	}
	if aud.count() != 1 {
		t.Errorf("audit calls = %d; want 1", aud.count())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestUpdateAgentSettings_PreservesExistingFieldsWhenOmitted(t *testing.T) {
	mock, h, _ := newHandlerWithMock(t)
	// Existing blob has quitAllowed=true and an unrelated key "extra".
	existing := `{"quitAllowed":true,"extra":"keep-me"}`
	mock.ExpectQuery(`SELECT value FROM system_metadata`).
		WithArgs("agent.settings").
		WillReturnRows(pgxmock.NewRows([]string{"value"}).AddRow([]byte(existing)))
	mock.ExpectExec(`INSERT INTO system_metadata`).
		WithArgs("agent.settings", pgxmock.AnyArg(), "k1").
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	mock.ExpectQuery(`SELECT value FROM system_metadata`).
		WithArgs("agent.config.version").WillReturnError(pgx.ErrNoRows)
	mock.ExpectExec(`INSERT INTO system_metadata`).
		WithArgs("agent.config.version", pgxmock.AnyArg(), "system").
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))

	// Only autoUpdateChannel sent; other fields must be preserved.
	body := `{"autoUpdateChannel":"beta"}`
	req := jsonReq(http.MethodPut, "/api/admin/settings/device-defaults", body)
	rec := httptest.NewRecorder()
	_ = h.UpdateAgentSettings(adminCtx(req, rec, "k1", "Admin"))
	got := decodeJSON(t, rec)
	if got["quitAllowed"] != true {
		t.Errorf("quitAllowed = %v; want true (preserved)", got["quitAllowed"])
	}
	if got["extra"] != "keep-me" {
		t.Errorf("unrelated key dropped: %v", got)
	}
	if got["autoUpdateChannel"] != "beta" {
		t.Errorf("autoUpdateChannel = %v; want beta", got["autoUpdateChannel"])
	}
}

func TestUpdateAgentSettings_SetMetadataFailure_500(t *testing.T) {
	mock, h, _ := newHandlerWithMock(t)
	mock.ExpectQuery(`SELECT value FROM system_metadata`).
		WithArgs("agent.settings").WillReturnError(pgx.ErrNoRows)
	mock.ExpectExec(`INSERT INTO system_metadata`).
		WithArgs("agent.settings", pgxmock.AnyArg(), "k1").
		WillReturnError(errors.New("disk full"))

	req := jsonReq(http.MethodPut, "/api/admin/settings/device-defaults",
		`{"quitAllowed":true}`)
	rec := httptest.NewRecorder()
	_ = h.UpdateAgentSettings(adminCtx(req, rec, "k1", "Admin"))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d; want 500", rec.Code)
	}
}

func TestUpdateAgentSettings_AnonContext_UpdatedByIsEmpty(t *testing.T) {
	mock, h, _ := newHandlerWithMock(t)
	mock.ExpectQuery(`SELECT value FROM system_metadata`).
		WithArgs("agent.settings").WillReturnError(pgx.ErrNoRows)
	mock.ExpectExec(`INSERT INTO system_metadata`).
		WithArgs("agent.settings", pgxmock.AnyArg(), ""). // empty updatedBy
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	mock.ExpectQuery(`SELECT value FROM system_metadata`).
		WithArgs("agent.config.version").WillReturnError(pgx.ErrNoRows)
	mock.ExpectExec(`INSERT INTO system_metadata`).
		WithArgs("agent.config.version", pgxmock.AnyArg(), "system").
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))

	req := jsonReq(http.MethodPut, "/api/admin/settings/device-defaults",
		`{"logLevel":"info"}`)
	rec := httptest.NewRecorder()
	_ = h.UpdateAgentSettings(anonCtx(req, rec))
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestUpdateAgentSettings_GarbageStoredBlob_TreatedAsEmpty(t *testing.T) {
	mock, h, _ := newHandlerWithMock(t)
	mock.ExpectQuery(`SELECT value FROM system_metadata`).
		WithArgs("agent.settings").
		WillReturnRows(pgxmock.NewRows([]string{"value"}).AddRow([]byte(`"not-an-object"`)))
	mock.ExpectExec(`INSERT INTO system_metadata`).
		WithArgs("agent.settings", pgxmock.AnyArg(), "k1").
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	mock.ExpectQuery(`SELECT value FROM system_metadata`).
		WithArgs("agent.config.version").WillReturnError(pgx.ErrNoRows)
	mock.ExpectExec(`INSERT INTO system_metadata`).
		WithArgs("agent.config.version", pgxmock.AnyArg(), "system").
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))

	req := jsonReq(http.MethodPut, "/api/admin/settings/device-defaults",
		`{"logLevel":"info"}`)
	rec := httptest.NewRecorder()
	if err := h.UpdateAgentSettings(adminCtx(req, rec, "k1", "Admin")); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d; want 200", rec.Code)
	}
}

func TestUpdateAgentSettings_AcceptsEmptyStringEnums(t *testing.T) {
	// Empty-string enums are valid for the optional fields — they mean
	// "fall back to agent default" rather than "reject as invalid".
	mock, h, _ := newHandlerWithMock(t)
	mock.ExpectQuery(`SELECT value FROM system_metadata`).
		WithArgs("agent.settings").WillReturnError(pgx.ErrNoRows)
	mock.ExpectExec(`INSERT INTO system_metadata`).
		WithArgs("agent.settings", pgxmock.AnyArg(), "k1").
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	mock.ExpectQuery(`SELECT value FROM system_metadata`).
		WithArgs("agent.config.version").WillReturnError(pgx.ErrNoRows)
	mock.ExpectExec(`INSERT INTO system_metadata`).
		WithArgs("agent.config.version", pgxmock.AnyArg(), "system").
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))

	body := `{"autoUpdateChannel":"","logLevel":"","trafficUploadLevel":""}`
	req := jsonReq(http.MethodPut, "/api/admin/settings/device-defaults", body)
	rec := httptest.NewRecorder()
	_ = h.UpdateAgentSettings(adminCtx(req, rec, "k1", "Admin"))
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d; want 200 for empty-string enums", rec.Code)
	}
}

// GetDeviceAuthSettings / UpdateDeviceAuthSettings (device_auth.go)

func TestGetDeviceAuthSettings_DefaultModeWhenNoMetadata(t *testing.T) {
	mock, h, _ := newHandlerWithMock(t)
	// Inject empty IdP list so the helper short-circuits the *pgxpool.Pool path.
	h.listIdPsFn = func(context.Context) ([]authserver_store.IdentityProvider, error) {
		return nil, nil
	}
	mock.ExpectQuery(`SELECT value FROM system_metadata`).
		WithArgs("device.auth.mode").WillReturnError(pgx.ErrNoRows)

	req := jsonReq(http.MethodGet, "/api/admin/settings/device-auth", "")
	rec := httptest.NewRecorder()
	if err := h.GetDeviceAuthSettings(adminCtx(req, rec, "k1", "Admin")); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	got := decodeJSON(t, rec)
	if got["mode"] != "mtls-only" {
		t.Errorf("mode = %v; want mtls-only (default)", got["mode"])
	}
	if got["ssoConfigured"] != false {
		t.Errorf("ssoConfigured = %v; want false", got["ssoConfigured"])
	}
	if got["localLoginAvailable"] != false {
		t.Errorf("localLoginAvailable = %v; want false", got["localLoginAvailable"])
	}
}

func TestGetDeviceAuthSettings_StoredMode(t *testing.T) {
	mock, h, _ := newHandlerWithMock(t)
	h.listIdPsFn = func(context.Context) ([]authserver_store.IdentityProvider, error) {
		return []authserver_store.IdentityProvider{
			{ID: "okta", Type: "oidc", Name: "Okta"},
		}, nil
	}
	mock.ExpectQuery(`SELECT value FROM system_metadata`).
		WithArgs("device.auth.mode").
		WillReturnRows(pgxmock.NewRows([]string{"value"}).AddRow([]byte(`"enterprise-login"`)))

	req := jsonReq(http.MethodGet, "/api/admin/settings/device-auth", "")
	rec := httptest.NewRecorder()
	_ = h.GetDeviceAuthSettings(adminCtx(req, rec, "k1", "Admin"))
	got := decodeJSON(t, rec)
	if got["mode"] != "enterprise-login" {
		t.Errorf("mode = %v; want enterprise-login", got["mode"])
	}
	if got["ssoConfigured"] != true {
		t.Errorf("ssoConfigured = %v; want true", got["ssoConfigured"])
	}
	providers, _ := got["ssoProviders"].([]any)
	if len(providers) != 1 {
		t.Errorf("ssoProviders len = %d; want 1", len(providers))
	}
}

func TestGetDeviceAuthSettings_GarbageStoredValue_FallsBackToDefault(t *testing.T) {
	mock, h, _ := newHandlerWithMock(t)
	h.listIdPsFn = func(context.Context) ([]authserver_store.IdentityProvider, error) { return nil, nil }
	// Non-string JSON cannot be unmarshalled into the string var → handler
	// keeps the default mode without surfacing an error.
	mock.ExpectQuery(`SELECT value FROM system_metadata`).
		WithArgs("device.auth.mode").
		WillReturnRows(pgxmock.NewRows([]string{"value"}).AddRow([]byte(`{}`)))

	req := jsonReq(http.MethodGet, "/api/admin/settings/device-auth", "")
	rec := httptest.NewRecorder()
	_ = h.GetDeviceAuthSettings(adminCtx(req, rec, "k1", "Admin"))
	if got := decodeJSON(t, rec)["mode"]; got != "mtls-only" {
		t.Errorf("mode = %v; want mtls-only (default on parse failure)", got)
	}
}

func TestGetDeviceAuthSettings_StoredEmptyStringMode_FallsBackToDefault(t *testing.T) {
	mock, h, _ := newHandlerWithMock(t)
	h.listIdPsFn = func(context.Context) ([]authserver_store.IdentityProvider, error) { return nil, nil }
	// Stored empty string — handler must keep the default rather than
	// surface "" as the mode.
	mock.ExpectQuery(`SELECT value FROM system_metadata`).
		WithArgs("device.auth.mode").
		WillReturnRows(pgxmock.NewRows([]string{"value"}).AddRow([]byte(`""`)))
	req := jsonReq(http.MethodGet, "/api/admin/settings/device-auth", "")
	rec := httptest.NewRecorder()
	_ = h.GetDeviceAuthSettings(adminCtx(req, rec, "k1", "Admin"))
	if got := decodeJSON(t, rec)["mode"]; got != "mtls-only" {
		t.Errorf("mode = %v; want mtls-only", got)
	}
}

func TestGetDeviceAuthSettings_IdPListError_DegradesGracefully(t *testing.T) {
	mock, h, _ := newHandlerWithMock(t)
	h.listIdPsFn = func(context.Context) ([]authserver_store.IdentityProvider, error) {
		return nil, errors.New("idp store boom")
	}
	mock.ExpectQuery(`SELECT value FROM system_metadata`).
		WithArgs("device.auth.mode").WillReturnError(pgx.ErrNoRows)

	req := jsonReq(http.MethodGet, "/api/admin/settings/device-auth", "")
	rec := httptest.NewRecorder()
	if err := h.GetDeviceAuthSettings(adminCtx(req, rec, "k1", "Admin")); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d; want 200 (degrade not fail)", rec.Code)
	}
	got := decodeJSON(t, rec)
	if got["ssoConfigured"] != false || got["localLoginAvailable"] != false {
		t.Errorf("must zero-out on idp error: %v", got)
	}
}

func TestUpdateDeviceAuthSettings_InvalidJSON_400(t *testing.T) {
	_, h, _ := newHandlerWithMock(t)
	req := jsonReq(http.MethodPut, "/api/admin/settings/device-auth", `{not json`)
	rec := httptest.NewRecorder()
	_ = h.UpdateDeviceAuthSettings(adminCtx(req, rec, "k1", "Admin"))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d; want 400", rec.Code)
	}
	if errEnvelopeType(t, rec) != "validation_error" {
		t.Errorf("type = %q", errEnvelopeType(t, rec))
	}
}

func TestUpdateDeviceAuthSettings_RejectsUnknownMode(t *testing.T) {
	_, h, _ := newHandlerWithMock(t)
	req := jsonReq(http.MethodPut, "/api/admin/settings/device-auth",
		`{"mode":"hopeful-glance"}`)
	rec := httptest.NewRecorder()
	_ = h.UpdateDeviceAuthSettings(adminCtx(req, rec, "k1", "Admin"))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d; want 400", rec.Code)
	}
	if errEnvelopeType(t, rec) != "validation_error" {
		t.Errorf("type = %q", errEnvelopeType(t, rec))
	}
}

func TestUpdateDeviceAuthSettings_EnterpriseWithoutSso_400(t *testing.T) {
	mock, h, _ := newHandlerWithMock(t)
	h.listIdPsFn = func(context.Context) ([]authserver_store.IdentityProvider, error) {
		// Only local — no SSO available.
		return []authserver_store.IdentityProvider{{ID: "lcl", Type: "local", Name: "Local"}}, nil
	}
	// The handler should refuse before touching system_metadata.
	_ = mock // unused expectations are fine; no calls expected

	req := jsonReq(http.MethodPut, "/api/admin/settings/device-auth",
		`{"mode":"enterprise-login"}`)
	rec := httptest.NewRecorder()
	_ = h.UpdateDeviceAuthSettings(adminCtx(req, rec, "k1", "Admin"))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d; want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "no_sso_provider") {
		t.Errorf("body should mention no_sso_provider: %s", rec.Body.String())
	}
}

func TestUpdateDeviceAuthSettings_LocalLoginWithoutLocalIdP_400(t *testing.T) {
	_, h, _ := newHandlerWithMock(t)
	h.listIdPsFn = func(context.Context) ([]authserver_store.IdentityProvider, error) {
		return []authserver_store.IdentityProvider{{ID: "okta", Type: "oidc", Name: "Okta"}}, nil
	}
	req := jsonReq(http.MethodPut, "/api/admin/settings/device-auth",
		`{"mode":"local-login"}`)
	rec := httptest.NewRecorder()
	_ = h.UpdateDeviceAuthSettings(adminCtx(req, rec, "k1", "Admin"))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d; want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "local_idp_unavailable") {
		t.Errorf("body should mention local_idp_unavailable: %s", rec.Body.String())
	}
}

func TestUpdateDeviceAuthSettings_HappyPath_MtlsOnly(t *testing.T) {
	mock, h, aud := newHandlerWithMock(t)
	h.listIdPsFn = func(context.Context) ([]authserver_store.IdentityProvider, error) { return nil, nil }
	// mtls-only is special: handler skips the IdP pre-conditions entirely,
	// goes straight to upsert + GET.
	mock.ExpectExec(`INSERT INTO system_metadata`).
		WithArgs("device.auth.mode", pgxmock.AnyArg(), "k1").
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	// GetDeviceAuthSettings tail call: load mode + list IdPs.
	mock.ExpectQuery(`SELECT value FROM system_metadata`).
		WithArgs("device.auth.mode").
		WillReturnRows(pgxmock.NewRows([]string{"value"}).AddRow([]byte(`"mtls-only"`)))

	req := jsonReq(http.MethodPut, "/api/admin/settings/device-auth",
		`{"mode":"mtls-only"}`)
	rec := httptest.NewRecorder()
	if err := h.UpdateDeviceAuthSettings(adminCtx(req, rec, "k1", "Admin")); err != nil {
		t.Fatalf("UpdateDeviceAuthSettings: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d; body=%s", rec.Code, rec.Body.String())
	}
	if aud.count() != 1 {
		t.Errorf("audit calls = %d; want 1", aud.count())
	}
	if got := decodeJSON(t, rec)["mode"]; got != "mtls-only" {
		t.Errorf("mode = %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestUpdateDeviceAuthSettings_HappyPath_EnterpriseLogin(t *testing.T) {
	mock, h, aud := newHandlerWithMock(t)
	h.listIdPsFn = func(context.Context) ([]authserver_store.IdentityProvider, error) {
		return []authserver_store.IdentityProvider{
			{ID: "okta", Type: "oidc", Name: "Okta"},
		}, nil
	}
	mock.ExpectExec(`INSERT INTO system_metadata`).
		WithArgs("device.auth.mode", pgxmock.AnyArg(), "k1").
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	mock.ExpectQuery(`SELECT value FROM system_metadata`).
		WithArgs("device.auth.mode").
		WillReturnRows(pgxmock.NewRows([]string{"value"}).AddRow([]byte(`"enterprise-login"`)))

	req := jsonReq(http.MethodPut, "/api/admin/settings/device-auth",
		`{"mode":"enterprise-login","oidc":{"foo":"bar"}}`) // oidc body is silently ignored
	rec := httptest.NewRecorder()
	_ = h.UpdateDeviceAuthSettings(adminCtx(req, rec, "k1", "Admin"))
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d; want 200", rec.Code)
	}
	if aud.count() != 1 {
		t.Errorf("audit calls = %d; want 1", aud.count())
	}
}

func TestUpdateDeviceAuthSettings_SetMetadataFailure_500(t *testing.T) {
	mock, h, _ := newHandlerWithMock(t)
	h.listIdPsFn = func(context.Context) ([]authserver_store.IdentityProvider, error) { return nil, nil }
	mock.ExpectExec(`INSERT INTO system_metadata`).
		WithArgs("device.auth.mode", pgxmock.AnyArg(), "k1").
		WillReturnError(errors.New("disk full"))

	req := jsonReq(http.MethodPut, "/api/admin/settings/device-auth",
		`{"mode":"mtls-only"}`)
	rec := httptest.NewRecorder()
	_ = h.UpdateDeviceAuthSettings(adminCtx(req, rec, "k1", "Admin"))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d; want 500", rec.Code)
	}
	if errEnvelopeType(t, rec) != "server_error" {
		t.Errorf("type = %q", errEnvelopeType(t, rec))
	}
}

func TestUpdateDeviceAuthSettings_AnonContext_UpdatedByIsEmpty(t *testing.T) {
	mock, h, _ := newHandlerWithMock(t)
	h.listIdPsFn = func(context.Context) ([]authserver_store.IdentityProvider, error) { return nil, nil }
	mock.ExpectExec(`INSERT INTO system_metadata`).
		WithArgs("device.auth.mode", pgxmock.AnyArg(), ""). // empty updatedBy
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	mock.ExpectQuery(`SELECT value FROM system_metadata`).
		WithArgs("device.auth.mode").WillReturnError(pgx.ErrNoRows)

	req := jsonReq(http.MethodPut, "/api/admin/settings/device-auth", `{"mode":"mtls-only"}`)
	rec := httptest.NewRecorder()
	_ = h.UpdateDeviceAuthSettings(anonCtx(req, rec))
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// listIdPs: when neither the seam nor h.db is set, we get the
// safe-default (nil, nil) so the calling endpoint still renders.
func TestListIdPs_NilDBNoSeam_ReturnsNilNil(t *testing.T) {
	h := &Handler{logger: silentLogger()}
	idps, err := h.listIdPs(context.Background())
	if idps != nil || err != nil {
		t.Errorf("listIdPs(nil db, no seam) = (%v, %v); want (nil, nil)", idps, err)
	}
}

// collectDeviceAuthSettings: when listIdPs surfaces an error, the helper
// returns a zero-value response (degrades — does not propagate).
func TestCollectDeviceAuthSettings_ListError_ReturnsZero(t *testing.T) {
	h := &Handler{logger: silentLogger()}
	h.listIdPsFn = func(context.Context) ([]authserver_store.IdentityProvider, error) {
		return nil, errors.New("idp boom")
	}
	got := h.collectDeviceAuthSettings(context.Background())
	if got.SsoConfigured || got.LocalLoginAvailable {
		t.Errorf("want zero-value on error; got %+v", got)
	}
	if got.SsoProviders == nil {
		t.Errorf("SsoProviders must be empty slice, not nil; got nil")
	}
}

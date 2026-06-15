package infra

// Tests for the infra admin-handler branches: readiness DB/hub health checks,
// instance listing, hub-proxy forwarding (jobs / config-sync / enrollment / node
// resync) on non-2xx Hub responses, diag-mode + diag-silence listings, and CA-cert
// setup. Each test names the specific failure mode it exercises, per the
// unit-test-coverage-95 policy.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/hub"
)

// parseRFC3339Flexible — plain RFC3339 (non-Nano) fallback branch. RFC3339Nano
// parsing fails on tz-offset strings like "+05:30", so the fallback to plain
// RFC3339 fires.

func TestParseRFC3339Flexible_PlainRFC3339TZOffset(t *testing.T) {
	// "2026-05-17T10:00:00+05:30" — this is valid RFC3339 but NOT RFC3339Nano
	// (nano requires fractional seconds or Z), so the code takes the second branch.
	s := "2026-05-17T10:00:00+05:30"
	got, ok := parseRFC3339Flexible(s)
	if !ok {
		t.Fatalf("named failure mode: plain RFC3339 with tz offset should parse; got !ok")
	}
	if got.IsZero() {
		t.Error("parsed time must be non-zero")
	}
}

// servicePoolFor — h.db.Pool return branch.
// Fires when servicePoolOverride is nil AND h.db.Pool is non-nil.
// The test constructs a Handler without the override and with a real-looking
// non-nil db (db.Pool stays nil in NewWithPgxPool, so we assert the nil
// return from servicePoolFor when db.Pool == nil).

func TestServicePoolFor_NilPoolReturnsNil(t *testing.T) {
	// Named failure mode: db is non-nil but db.Pool (concrete *pgxpool.Pool)
	// is nil (the test-only NewWithPgxPool constructor) → servicePoolFor
	// returns nil.
	_, db := newMockDB(t)
	h := newHandler(t, db, &fakeHub{}, nil)
	// servicePoolOverride is nil (not set by newHandler), db.Pool is nil.
	got := h.servicePoolFor()
	// db.Pool is nil because NewWithPgxPool doesn't set Pool.
	if got != nil {
		t.Errorf("expected nil servicePoolFor when db.Pool is nil; got %T", got)
	}
}

// ReadinessCheck — DB ping seam tests.
// The dbPingFn seam lets us drive ReadinessCheck without a live *pgxpool.Pool.
// Hub health is exercised via an httptest.Server.

func TestReadinessCheck_DBUnhealthy_HubNotConfigured(t *testing.T) {
	// Named failure mode: DB ping fails → checks[database]=unhealthy; hub
	// not configured → checks[hub]=not_configured; overall status=not_ready.
	h := newHandler(t, nil, &fakeHub{}, nil)
	h.dbPingFn = func(_ context.Context) error { return errors.New("connection refused") }

	c, rec := echoCtx(http.MethodGet, "/ready", "", false)
	if err := h.ReadinessCheck(c); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d body=%s; want 503", rec.Code, rec.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["status"] != "not_ready" {
		t.Errorf("status=%v, want not_ready", out["status"])
	}
	checks, _ := out["checks"].(map[string]any)
	if checks["database"] != "unhealthy" {
		t.Errorf("checks.database=%v, want unhealthy", checks["database"])
	}
	if checks["hub"] != "not_configured" {
		t.Errorf("checks.hub=%v, want not_configured", checks["hub"])
	}
}

func TestReadinessCheck_DBHealthy_HubNotConfigured(t *testing.T) {
	// Named failure mode: DB ping succeeds; hub BaseURL empty → not_configured.
	// Overall status should be ready (hub not_configured is not a failure).
	h := newHandler(t, nil, &fakeHub{}, nil)
	h.dbPingFn = func(_ context.Context) error { return nil }

	c, rec := echoCtx(http.MethodGet, "/ready", "", false)
	if err := h.ReadinessCheck(c); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s; want 200 (hub not_configured is not a failure)", rec.Code, rec.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["status"] != "ready" {
		t.Errorf("status=%v, want ready", out["status"])
	}
}

func TestReadinessCheck_HubHealthy(t *testing.T) {
	// Named failure mode: DB ping succeeds; hub /healthz returns 200 → ready.
	hubSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	}))
	defer hubSrv.Close()

	h := newHandler(t, nil, &fakeHub{baseURL: hubSrv.URL}, nil)
	h.dbPingFn = func(_ context.Context) error { return nil }

	c, rec := echoCtx(http.MethodGet, "/ready", "", false)
	if err := h.ReadinessCheck(c); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s; want 200", rec.Code, rec.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	checks, _ := out["checks"].(map[string]any)
	if checks["hub"] != "ok" {
		t.Errorf("checks.hub=%v, want ok", checks["hub"])
	}
}

func TestReadinessCheck_HubUnhealthy(t *testing.T) {
	// Named failure mode: hub /healthz returns non-200 → checks[hub]=unhealthy.
	hubSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer hubSrv.Close()

	h := newHandler(t, nil, &fakeHub{baseURL: hubSrv.URL}, nil)
	h.dbPingFn = func(_ context.Context) error { return nil }

	c, rec := echoCtx(http.MethodGet, "/ready", "", false)
	if err := h.ReadinessCheck(c); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d body=%s; want 503 (hub unhealthy)", rec.Code, rec.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	checks, _ := out["checks"].(map[string]any)
	if checks["hub"] != "unhealthy" {
		t.Errorf("checks.hub=%v, want unhealthy", checks["hub"])
	}
}

func TestReadinessCheck_HubUnreachable(t *testing.T) {
	// Named failure mode: hub HTTP request fails (unreachable) → hub=unreachable.
	h := newHandler(t, nil, &fakeHub{baseURL: "http://127.0.0.1:1"}, nil)
	h.dbPingFn = func(_ context.Context) error { return nil }
	h.hubProxyClientRef = &http.Client{Timeout: 50 * time.Millisecond}

	c, rec := echoCtx(http.MethodGet, "/ready", "", false)
	if err := h.ReadinessCheck(c); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d body=%s; want 503 (hub unreachable)", rec.Code, rec.Body.String())
	}
}

// RegisterReadinessRoutes — smoke.
// Verifies the route is mounted without panicking and that an unauthenticated
// request at least reaches the handler (returns 200, not 404).

func TestRegisterReadinessRoutes_MountsInstancesRoute(t *testing.T) {
	mock, db := newMockDB(t)
	h := newHandler(t, db, &fakeHub{}, nil)

	// Wire up ListThingServices + GetServiceSummaries mock expectations
	// so the /instances handler completes successfully.
	mock.ExpectQuery(`FROM thing t`).
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "type", "version", "address", "status",
			"enrolled_at", "last_seen_at", "reported",
			"role", "metrics_url",
		}))
	mock.ExpectQuery(`FROM thing`).
		WillReturnRows(pgxmock.NewRows([]string{
			"type", "total", "online", "offline", "drift",
		}))

	e := echo.New()
	g := e.Group("/api/admin")
	noopIAMFn := func(_ string) echo.MiddlewareFunc {
		return func(next echo.HandlerFunc) echo.HandlerFunc {
			return func(c echo.Context) error { return next(c) }
		}
	}
	h.RegisterReadinessRoutes(g, noopIAMFn)

	req := httptest.NewRequest(http.MethodGet, "/api/admin/instances", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("RegisterReadinessRoutes: instances route code=%d body=%s", rec.Code, rec.Body.String())
	}
}

// ListInstances — happy path.

func TestListInstances_HappyEmpty(t *testing.T) {
	mock, db := newMockDB(t)
	h := newHandler(t, db, &fakeHub{}, nil)

	// ListThingServices returns 0 rows.
	mock.ExpectQuery(`FROM thing t`).
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "type", "version", "address", "status",
			"enrolled_at", "last_seen_at", "reported",
			"role", "metrics_url",
		}))
	// GetServiceSummaries → GetThingTypeSummaries returns 0 rows.
	mock.ExpectQuery(`FROM thing`).
		WillReturnRows(pgxmock.NewRows([]string{
			"type", "total", "online", "offline", "drift",
		}))

	c, rec := echoCtx(http.MethodGet, "/instances", "", true)
	if err := h.ListInstances(c); err != nil {
		t.Fatalf("ListInstances error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := out["instances"]; !ok {
		t.Error("response missing 'instances' key")
	}
	if _, ok := out["count"]; !ok {
		t.Error("response missing 'count' key")
	}
}

func TestListInstances_WithOneInstance(t *testing.T) {
	mock, db := newMockDB(t)
	h := newHandler(t, db, &fakeHub{}, nil)

	now := time.Now().UTC()
	reported := []byte(`{"uptime":42,"checks":null,"status":"online"}`)

	mock.ExpectQuery(`FROM thing t`).
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "type", "version", "address", "status",
			"enrolled_at", "last_seen_at", "reported",
			"role", "metrics_url",
		}).AddRow(
			"inst-1", "ai-gateway", "1.0.0", strPtr("http://gw:3050"), "online",
			now, &now, reported,
			"api", strPtr("http://gw:9090"),
		))
	mock.ExpectQuery(`FROM thing`).
		WillReturnRows(pgxmock.NewRows([]string{
			"type", "total", "online", "offline", "drift",
		}).AddRow("ai-gateway", 1, 1, 0, 0))

	c, rec := echoCtx(http.MethodGet, "/instances", "", true)
	if err := h.ListInstances(c); err != nil {
		t.Fatalf("ListInstances error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	// count should reflect the single ai-gateway instance.
	count, _ := out["count"].(float64)
	if count != 1 {
		t.Errorf("count=%v, want 1", count)
	}
}

// TestListInstances_ListThingServicesError covers the first error branch:
// ListThingServices fails → 500.
func TestListInstances_ListThingServicesError(t *testing.T) {
	mock, db := newMockDB(t)
	h := newHandler(t, db, &fakeHub{}, nil)

	mock.ExpectQuery(`FROM thing t`).
		WillReturnError(errors.New("db down"))

	c, rec := echoCtx(http.MethodGet, "/instances", "", true)
	if err := h.ListInstances(c); err != nil {
		t.Fatalf("unexpected handler error: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("named failure mode: ListThingServices error → 500; got %d body=%s",
			rec.Code, rec.Body.String())
	}
}

// TestListInstances_GetServiceSummariesError covers the second error branch:
// GetServiceSummaries fails → 500.
func TestListInstances_GetServiceSummariesError(t *testing.T) {
	mock, db := newMockDB(t)
	h := newHandler(t, db, &fakeHub{}, nil)

	mock.ExpectQuery(`FROM thing t`).
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "type", "version", "address", "status",
			"enrolled_at", "last_seen_at", "reported",
			"role", "metrics_url",
		}))
	// GetThingTypeSummaries errors.
	mock.ExpectQuery(`FROM thing`).
		WillReturnError(errors.New("summaries boom"))

	c, rec := echoCtx(http.MethodGet, "/instances", "", true)
	if err := h.ListInstances(c); err != nil {
		t.Fatalf("unexpected handler error: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("named failure mode: GetServiceSummaries error → 500; got %d body=%s",
			rec.Code, rec.Body.String())
	}
}

// BulkEnableDiagMode — len(ids) > maxBulkDiagModeThings.
// ResolveBulkAgents uses LIMIT maxThings+1, so if it returns 501 rows the
// handler rejects with 400 TOO_MANY_NODES.

func TestBulkEnableDiagMode_TooManyNodes(t *testing.T) {
	mock, db := newMockDB(t)
	h := newHandler(t, db, &fakeHub{}, nil)

	// Return 501 rows to exceed maxBulkDiagModeThings(500).
	// The attribute-filter ResolveBulkAgents path uses LIMIT maxThings+1 = 501
	// and 3 bind parameters: agentVer, osName, maxThings+1.
	rows := pgxmock.NewRows([]string{"id"})
	for i := range 501 {
		rows.AddRow(fmt.Sprintf("agent-%d", i))
	}
	mock.ExpectQuery(`FROM thing`).WithArgs(anyArgs(3)...).WillReturnRows(rows)

	until := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	body := `{"until":"` + until + `"}`
	c, rec := echoCtx(http.MethodPost, "/", body, true)
	if err := h.BulkEnableDiagMode(c); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("named failure mode: >500 nodes → 400; got %d body=%s",
			rec.Code, rec.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	errObj, _ := out["error"].(map[string]any)
	if errObj["code"] != "TOO_MANY_NODES" {
		t.Errorf("expected TOO_MANY_NODES code; got %v", errObj["code"])
	}
}

// DiagSilencesList — happy path with one data row
// Exercises the row scan loop branch.

func TestDiagSilencesList_HappyWithData(t *testing.T) {
	mock, db := newMockDB(t)
	h := newHandler(t, db, &fakeHub{}, nil)

	expiresAt := time.Now().Add(24 * time.Hour)
	mock.ExpectQuery(`FROM diag_silence`).
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "message_hash", "level", "silenced_by", "silenced_at", "expires_at", "reason",
		}).AddRow("s-1", "abc123", "error", "admin", time.Now(), &expiresAt, (*string)(nil)))

	c, rec := echoCtx(http.MethodGet, "/", "", true)
	if err := h.DiagSilencesList(c); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	data, _ := out["data"].([]any)
	if len(data) != 1 {
		t.Errorf("expected 1 silence in data; got %d", len(data))
	}
}

// DiagEventsCrashCohorts — happy path with one data row
// Exercises the scan loop branch.

func TestDiagEventsCrashCohorts_HappyWithData(t *testing.T) {
	mock, db := newMockDB(t)
	h := newHandler(t, db, &fakeHub{}, nil)

	mock.ExpectQuery(`event_type = 'crash'`).WithArgs(anyArgs(2)...).
		WillReturnRows(pgxmock.NewRows([]string{
			"agent_version", "os", "os_version",
			"crash_count", "affected_things", "last_seen",
		}).AddRow("1.2.3", "darwin", "15.0", 3, 2, time.Now()))

	c, rec := echoCtx(http.MethodGet,
		"/?from=2026-05-17T00:00:00Z&to=2026-05-18T00:00:00Z", "", true)
	if err := h.DiagEventsCrashCohorts(c); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	data, _ := out["data"].([]any)
	if len(data) != 1 {
		t.Errorf("expected 1 cohort; got %d", len(data))
	}
}

// ListDiagMode — happy path with one data row
// Exercises the scan loop branch.

func TestListDiagMode_HappyWithData(t *testing.T) {
	mock, db := newMockDB(t)
	h := newHandler(t, db, &fakeHub{}, nil)

	mock.ExpectQuery(`FROM thing_diag_mode_window`).
		WillReturnRows(pgxmock.NewRows(strings.Split(diagModeWindowCols, ",")).AddRow(
			"w-1", "t-1", "agent",
			time.Now(), time.Now().Add(time.Hour),
			(*string)(nil), (*string)(nil), time.Now(),
		))

	c, rec := echoCtx(http.MethodGet, "/", "", true)
	if err := h.ListDiagMode(c); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	data, _ := out["data"].([]any)
	if len(data) != 1 {
		t.Errorf("expected 1 window; got %d", len(data))
	}
}

// hub_proxy.go — hubForward(c,...) err != nil paths
// These paths fire when hubForward returns a non-nil error (i.e. the response
// has already been written by hubForward itself — Echo returns the error from
// hubForward; but hubForward always writes a response before returning nil).
// In practice the only time hubForward itself can return a non-nil error is if
// c.JSON / c.JSONBlob fails, which requires an unwritable ResponseWriter —
// structurally unreachable in tests.
//
// What IS newly covered: the NodesResync happy-then-hubForward path where
// hubForward returns nil but the response status is non-2xx (no audit is emitted).
// The existing TestNodesResync_HubErrorNoAudit already hits the 502 branch.

// TestJobsUpdate_ReadBodyError covers the io.ReadAll error path in JobsUpdate,
// forced by replacing the body with an io.Reader that errors on read.
func TestJobsUpdate_ReadBodyError(t *testing.T) {
	h := newHandler(t, nil, &fakeHub{baseURL: "http://hub"}, nil)
	e := echo.New()
	e.PUT("/jobs/:id", h.JobsUpdate)

	req := httptest.NewRequest(http.MethodPut, "/jobs/j-1", &errorReader{})
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("named failure mode: body read error → 400; got %d body=%s",
			rec.Code, rec.Body.String())
	}
}

// errorReader is an io.Reader that always errors.
type errorReader struct{}

func (errorReader) Read(_ []byte) (int, error) { return 0, errors.New("read error") }

// ConfigSyncUpdate — hubForward error path.
// ConfigSyncUpdate calls json.Marshal on a map which can't fail in practice,
// so the real error path is hubForward failing. The existing
// TestConfigSyncUpdate_HubError drives this via an unreachable hub.
// Additional coverage: hubForward path after marshal succeeds but hub returns
// non-2xx (no audit emitted — ConfigSyncUpdate doesn't audit on non-2xx).

// TestConfigSyncUpdate_NoStateOrAction covers the hub-payload build path when
// neither optional state nor action is present.
func TestConfigSyncUpdate_NoStateOrAction(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	h := newHandler(t, nil, &fakeHub{baseURL: srv.URL, token: "tok"}, nil)
	h.hubProxyClientRef = srv.Client()

	c, rec := echoCtx(http.MethodPost, "/config-sync/update",
		`{"nodeType":"agent","configKey":"routing"}`, true)
	if err := h.ConfigSyncUpdate(c); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
}

// EnrollmentCreateToken — hubForward non-2xx path.
// When hubForward succeeds but Hub returns non-2xx, audit is NOT emitted.
// The existing TestEnrollmentCreateToken_HubError covers hub-unreachable (502).
// Here we cover the case where Hub returns 401 (no audit emitted).

func TestEnrollmentCreateToken_HubNon2xx_NoAudit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
	}))
	defer srv.Close()

	spy := &auditSpy{}
	h := newHandler(t, nil, &fakeHub{baseURL: srv.URL, token: "tok"}, spy)
	h.hubProxyClientRef = srv.Client()

	c, rec := echoCtx(http.MethodPost, "/enrollment/token", "", true)
	if err := h.EnrollmentCreateToken(c); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 from Hub; got %d", rec.Code)
	}
	// Named failure mode: non-2xx Hub response → no audit entry emitted.
	if spy.count() != 0 {
		t.Errorf("expected no audit on non-2xx Hub; got %d", spy.count())
	}
}

// SetupGetCACert — the io.ReadAll error path is structurally unreachable in normal
// tests because the httptest.Server always supplies a readable body (it would fire
// only on a broken pipe after headers are sent — not safely reproducible in unit
// tests). The non-200 proxy-response path IS covered below via a mock proxy server
// returning 503.

func TestSetupGetCACert_ProxyNon200(t *testing.T) {
	// Proxy management endpoint returns non-200.
	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/management/ca-cert") {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`error`))
			return
		}
		http.NotFound(w, r)
	}))
	defer proxySrv.Close()

	// Wire hub GetThingServiceMeta to return the proxy's base URL.
	fh := &fakeHub{
		serviceMeta: &hub.ThingServiceMeta{ManagementURL: proxySrv.URL},
	}
	h := newHandler(t, nil, fh, nil)
	h.complianceProxyClient = proxySrv.Client()

	c, rec := echoCtxParam(http.MethodGet, "/setup/proxy/:thingId/ca-cert", "", true,
		[]string{"thingId"}, []string{"cp-1"})
	if err := h.SetupGetCACert(c); err != nil {
		t.Fatalf("unexpected handler error: %v", err)
	}
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("named failure mode: proxy returns non-200 → 502; got %d body=%s",
			rec.Code, rec.Body.String())
	}
}

// JobsTrigger — hubForward non-2xx path.
// When Hub returns 4xx the audit block is skipped.

func TestJobsTrigger_HubNon2xx_NoAudit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not found"}`))
	}))
	defer srv.Close()

	spy := &auditSpy{}
	h := newHandler(t, nil, &fakeHub{baseURL: srv.URL, token: "tok"}, spy)
	h.hubProxyClientRef = srv.Client()

	c, rec := echoCtxParam(http.MethodPost, "/jobs/:id/trigger", "", true,
		[]string{"id"}, []string{"j-missing"})
	if err := h.JobsTrigger(c); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 from Hub; got %d", rec.Code)
	}
	// Named failure mode: non-2xx Hub response → no audit entry emitted.
	if spy.count() != 0 {
		t.Errorf("expected no audit on non-2xx; got %d", spy.count())
	}
}

// JobsUpdate — hubForward non-2xx path.
// When Hub returns 4xx the audit block is skipped.

func TestJobsUpdate_HubNon2xx_NoAudit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"bad"}`))
	}))
	defer srv.Close()

	spy := &auditSpy{}
	h := newHandler(t, nil, &fakeHub{baseURL: srv.URL, token: "tok"}, spy)
	h.hubProxyClientRef = srv.Client()

	c, rec := echoCtxParam(http.MethodPut, "/jobs/:id", `{"enabled":false}`, true,
		[]string{"id"}, []string{"j-1"})
	if err := h.JobsUpdate(c); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 from Hub; got %d", rec.Code)
	}
	// Named failure mode: non-2xx Hub response → no audit entry emitted.
	if spy.count() != 0 {
		t.Errorf("expected no audit on non-2xx; got %d", spy.count())
	}
}

// NodesResync — non-2xx Hub response.
// Named failure mode: Hub returns non-2xx → no audit logged.

func TestNodesResync_HubNon2xx_NoAudit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusGatewayTimeout)
		_, _ = w.Write([]byte(`{"error":"timeout"}`))
	}))
	defer srv.Close()

	spy := &auditSpy{}
	h := newHandler(t, nil, &fakeHub{baseURL: srv.URL, token: "tok"}, spy)
	h.hubProxyClientRef = srv.Client()

	body := `{"configKey":"routing"}`
	c, rec := echoCtxParam(http.MethodPost, "/nodes/:id/resync", body, true,
		[]string{"id"}, []string{"n-1"})
	if err := h.AdminResyncNode(c); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// AdminResyncNode normalises any Hub non-2xx into a 502 BadGateway with
	// a sanitized error envelope so internal Hub status codes don't leak.
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 from Hub-non-2xx normalisation; got %d", rec.Code)
	}
	// Named failure mode: non-2xx Hub response → no audit entry emitted.
	if spy.count() != 0 {
		t.Errorf("expected no audit on non-2xx; got %d", spy.count())
	}
}

func strPtr(s string) *string { return &s }

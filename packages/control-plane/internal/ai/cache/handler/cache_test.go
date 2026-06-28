package cache

import (
	"bytes"
	"context"
	"errors"
	"github.com/goccy/go-json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/labstack/echo/v4"
	"github.com/pashagolub/pgxmock/v4"

	auth "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authn"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/iam"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/hub"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/cacheconfig"
)

// Helpers (mirrors handler/alerts package pattern; copied per R6 runbook §4.2)

// auditSpy captures MQ enqueues so tests can assert audit emission.
type auditSpy struct {
	mu    sync.Mutex
	calls [][]byte
}

func (a *auditSpy) Publish(context.Context, string, []byte) error { return nil }
func (a *auditSpy) Enqueue(_ context.Context, _ string, data []byte) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	copied := make([]byte, len(data))
	copy(copied, data)
	a.calls = append(a.calls, copied)
	return nil
}
func (a *auditSpy) Close() error { return nil }

func (a *auditSpy) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.calls)
}

func (a *auditSpy) last() map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.calls) == 0 {
		return nil
	}
	var m map[string]any
	_ = json.Unmarshal(a.calls[len(a.calls)-1], &m)
	return m
}

// echoContext builds an Echo context with the authenticated admin
// principal populated under the same key the real AdminAuth middleware
// uses.
func echoContext(req *http.Request, rec *httptest.ResponseRecorder, userName, userID string) echo.Context {
	e := echo.New()
	c := e.NewContext(req, rec)
	middleware.WithAdminAuth(c, &auth.AdminAuth{
		KeyID:             userID,
		KeyName:           userName,
		AuthPrincipalType: "admin_user",
	})
	return c
}

// fakeHub is a stub HubConfigChanger that captures the last request and
// returns a programmable error. Lets the test exercise both the success
// and propagation-failure branches of every mutating handler without
// standing up an HTTP server.
type fakeHub struct {
	mu      sync.Mutex
	hits    int
	lastReq hub.ConfigChangeRequest
	resp    *hub.ConfigChangeResponse
	err     error
}

func (f *fakeHub) NotifyConfigChange(_ context.Context, req hub.ConfigChangeRequest) (*hub.ConfigChangeResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hits++
	f.lastReq = req
	return f.resp, f.err
}

// newMockDB constructs a pgxmock pool for tests; the handler routes
// every SQL call through this mock.
func newMockDB(t *testing.T) (pgxmock.PgxPoolIface, pgxmock.PgxPoolIface) {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mock.Close)
	return mock, mock
}

// newHandler builds a *Handler with the supplied parts and a discard
// logger; nils default to fresh empties.
func newHandler(t *testing.T, pool pgxmock.PgxPoolIface, hub HubConfigChanger, spy *auditSpy) *Handler {
	t.Helper()
	if spy == nil {
		spy = &auditSpy{}
	}
	return newWithPool(pool, hub, audit.NewWriter(spy, "audit", slog.Default()), slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// expectAssembleBlobOK queues mock rows for the 3 SELECTs of
// AssembleCacheConfigBlob (global / adapters / providers — all empty
// except provider row if includeProvider provided).
func expectAssembleBlobOK(mock pgxmock.PgxPoolIface, includeProvider string) {
	mock.ExpectQuery(`FROM cache_global_config`).
		WillReturnRows(pgxmock.NewRows([]string{"config"}).AddRow([]byte(`{}`)))
	mock.ExpectQuery(`FROM cache_adapter_config`).
		WillReturnRows(pgxmock.NewRows([]string{"adapter_type", "config"}))
	rows := pgxmock.NewRows([]string{"provider_id", "config"})
	if includeProvider != "" {
		rows.AddRow(includeProvider, []byte(`{"cache_enabled":true}`))
	}
	mock.ExpectQuery(`FROM cache_provider_config`).WillReturnRows(rows)
}

// validateAdapterConfigKnobs — pure validators (no DB / no HTTP)

func TestValidateAdapterConfigKnobs(t *testing.T) {
	tru := true
	five := 5
	tests := []struct {
		name       string
		adapter    string
		cfg        cacheconfig.AdapterConfig
		wantErrSub string
	}{
		{"anthropic clean", "anthropic", cacheconfig.AdapterConfig{MarkerInjectEnabled: &tru}, ""},
		{"anthropic with gemini knob", "anthropic", cacheconfig.AdapterConfig{CacheEnabled: &tru}, "gemini knobs"},
		{"anthropic with min_system_chars", "anthropic", cacheconfig.AdapterConfig{MinSystemChars: &five}, "gemini knobs"},
		{"anthropic with ttl_seconds", "anthropic", cacheconfig.AdapterConfig{TTLSeconds: &five}, "gemini knobs"},
		{"anthropic with cb_threshold", "anthropic", cacheconfig.AdapterConfig{CircuitBreakerThreshold: &five}, "gemini knobs"},
		{"anthropic with cb_open_secs", "anthropic", cacheconfig.AdapterConfig{CircuitBreakerOpenSecs: &five}, "gemini knobs"},
		{"gemini clean", "gemini", cacheconfig.AdapterConfig{CacheEnabled: &tru}, ""},
		{"gemini with marker_inject", "gemini", cacheconfig.AdapterConfig{MarkerInjectEnabled: &tru}, "anthropic marker knobs"},
		{"gemini with marker_boundary3", "gemini", cacheconfig.AdapterConfig{MarkerBoundary3Enabled: &tru}, "anthropic marker knobs"},
		{"none rejects any cache knob", "openai", cacheconfig.AdapterConfig{CacheEnabled: &tru}, "no admin-tunable cache knobs"},
		{"none rejects marker_inject", "openai", cacheconfig.AdapterConfig{MarkerInjectEnabled: &tru}, "no admin-tunable cache knobs"},
		{"none rejects min_system_chars", "openai", cacheconfig.AdapterConfig{MinSystemChars: &five}, "no admin-tunable cache knobs"},
		{"none rejects ttl_seconds", "openai", cacheconfig.AdapterConfig{TTLSeconds: &five}, "no admin-tunable cache knobs"},
		{"none rejects cb_threshold", "openai", cacheconfig.AdapterConfig{CircuitBreakerThreshold: &five}, "no admin-tunable cache knobs"},
		{"none rejects cb_open_secs", "openai", cacheconfig.AdapterConfig{CircuitBreakerOpenSecs: &five}, "no admin-tunable cache knobs"},
		{"none rejects marker_boundary3", "openai", cacheconfig.AdapterConfig{MarkerBoundary3Enabled: &tru}, "no admin-tunable cache knobs"},
		{"none allows empty (just rules)", "openai", cacheconfig.AdapterConfig{Rules: map[string]cacheconfig.RuleOverride{"a": {}}}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateAdapterConfigKnobs(tc.adapter, tc.cfg)
			if tc.wantErrSub == "" && err != nil {
				t.Fatalf("expected nil; got %v", err)
			}
			if tc.wantErrSub != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErrSub)) {
				t.Fatalf("expected err substr %q; got %v", tc.wantErrSub, err)
			}
		})
	}
}

func TestValidateProviderConfigKnobs(t *testing.T) {
	tru := true
	five := 5
	tests := []struct {
		name       string
		adapter    string
		cfg        cacheconfig.ProviderConfig
		wantErrSub string
	}{
		{"anthropic clean", "anthropic", cacheconfig.ProviderConfig{MarkerInjectEnabled: &tru}, ""},
		{"anthropic with gemini knob", "anthropic", cacheconfig.ProviderConfig{CacheEnabled: &tru}, "gemini knobs"},
		{"anthropic with min_system_chars", "anthropic", cacheconfig.ProviderConfig{MinSystemChars: &five}, "gemini knobs"},
		{"anthropic with ttl", "anthropic", cacheconfig.ProviderConfig{TTLSeconds: &five}, "gemini knobs"},
		{"anthropic with cb_threshold", "anthropic", cacheconfig.ProviderConfig{CircuitBreakerThreshold: &five}, "gemini knobs"},
		{"anthropic with cb_open", "anthropic", cacheconfig.ProviderConfig{CircuitBreakerOpenSecs: &five}, "gemini knobs"},
		{"gemini clean", "gemini", cacheconfig.ProviderConfig{CacheEnabled: &tru}, ""},
		{"gemini with marker_inject", "gemini", cacheconfig.ProviderConfig{MarkerInjectEnabled: &tru}, "anthropic marker knobs"},
		{"gemini with marker_boundary3", "gemini", cacheconfig.ProviderConfig{MarkerBoundary3Enabled: &tru}, "anthropic marker knobs"},
		{"none rejects cache_enabled", "openai", cacheconfig.ProviderConfig{CacheEnabled: &tru}, "no admin-tunable"},
		{"none rejects marker_inject", "openai", cacheconfig.ProviderConfig{MarkerInjectEnabled: &tru}, "no admin-tunable"},
		{"none rejects marker_boundary3", "openai", cacheconfig.ProviderConfig{MarkerBoundary3Enabled: &tru}, "no admin-tunable"},
		{"none rejects min_system_chars", "openai", cacheconfig.ProviderConfig{MinSystemChars: &five}, "no admin-tunable"},
		{"none rejects ttl", "openai", cacheconfig.ProviderConfig{TTLSeconds: &five}, "no admin-tunable"},
		{"none rejects cb_threshold", "openai", cacheconfig.ProviderConfig{CircuitBreakerThreshold: &five}, "no admin-tunable"},
		{"none rejects cb_open", "openai", cacheconfig.ProviderConfig{CircuitBreakerOpenSecs: &five}, "no admin-tunable"},
		{"none accepts empty", "openai", cacheconfig.ProviderConfig{}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateProviderConfigKnobs(tc.adapter, tc.cfg)
			if tc.wantErrSub == "" && err != nil {
				t.Fatalf("expected nil; got %v", err)
			}
			if tc.wantErrSub != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErrSub)) {
				t.Fatalf("expected err substr %q; got %v", tc.wantErrSub, err)
			}
		})
	}
}

func TestIsProviderConfigEmpty(t *testing.T) {
	tru := true
	five := 5
	if !isProviderConfigEmpty(cacheconfig.ProviderConfig{}) {
		t.Error("zero value should be empty")
	}
	tests := []cacheconfig.ProviderConfig{
		{MarkerInjectEnabled: &tru},
		{MarkerBoundary3Enabled: &tru},
		{CacheEnabled: &tru},
		{MinSystemChars: &five},
		{TTLSeconds: &five},
		{CircuitBreakerThreshold: &five},
		{CircuitBreakerOpenSecs: &five},
	}
	for i, c := range tests {
		if isProviderConfigEmpty(c) {
			t.Errorf("case %d should be non-empty: %+v", i, c)
		}
	}
}

// Misc package-private helpers (errJSON / wrappedErr / internalServerError /
// actorFromContext). The propagation-failure envelope now lives in the shared
// hub package (hub.PropagationErrorJSON) and is tested there.

func TestErrJSONShape(t *testing.T) {
	out := errJSON("hi", "validation_error", "X")
	inner, ok := out["error"].(map[string]any)
	if !ok {
		t.Fatalf("missing envelope: %v", out)
	}
	if inner["message"] != "hi" || inner["type"] != "validation_error" || inner["code"] != "X" {
		t.Fatalf("bad fields: %v", inner)
	}
}

func TestWrappedErr(t *testing.T) {
	root := errors.New("boom")
	w := wrapErr("ctx-tag", root)
	if w.Error() != "ctx-tag: boom" {
		t.Errorf("Error() = %q", w.Error())
	}
	if !errors.Is(w, root) {
		t.Errorf("Unwrap path broken")
	}
}

func TestInternalServerError(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if err := internalServerError(c, "kaput"); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "kaput") {
		t.Errorf("body = %s", rec.Body.String())
	}
}

func TestActorFromContext(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "alice", "user-1")
	a := actorFromContext(c)
	if a.UserID != "user-1" || a.Name != "alice" {
		t.Errorf("got %+v", a)
	}

	// No AdminAuth set → empty actor.
	e := echo.New()
	c2 := e.NewContext(httptest.NewRequest(http.MethodGet, "/x", nil), httptest.NewRecorder())
	a2 := actorFromContext(c2)
	if a2.UserID != "" || a2.Name != "" {
		t.Errorf("missing-auth: got %+v", a2)
	}
}

// propagateCacheConfig — direct unit tests (separate from handler-flow tests
// because of three paths: nil hub / blob-assembly err / NotifyConfigChange err)

func TestPropagateCacheConfig_NilHub(t *testing.T) {
	h := newHandler(t, nil, nil, nil)
	if err := h.propagateCacheConfig(context.Background(), "u", "alice"); err != nil {
		t.Errorf("nil hub should be no-op; got %v", err)
	}
}

func TestPropagateCacheConfig_AssembleErrWraps(t *testing.T) {
	mock, db := newMockDB(t)
	hub := &fakeHub{}
	h := newHandler(t, db, hub, nil)
	mock.ExpectQuery(`FROM cache_global_config`).WillReturnError(errors.New("planner err"))
	err := h.propagateCacheConfig(context.Background(), "u", "alice")
	if err == nil || !strings.Contains(err.Error(), "assemble cache blob") {
		t.Fatalf("expected wrapped err; got %v", err)
	}
	if hub.hits != 0 {
		t.Errorf("hub should not be called on assemble err; hits=%d", hub.hits)
	}
}

func TestPropagateCacheConfig_HubErrPropagated(t *testing.T) {
	mock, db := newMockDB(t)
	hub := &fakeHub{err: errors.New("hub down")}
	h := newHandler(t, db, hub, nil)
	expectAssembleBlobOK(mock, "")
	err := h.propagateCacheConfig(context.Background(), "u-1", "alice")
	if err == nil || !strings.Contains(err.Error(), "hub down") {
		t.Fatalf("got %v", err)
	}
	if hub.lastReq.ThingType != "ai-gateway" || hub.lastReq.ConfigKey != "cache" {
		t.Errorf("hub req wiring: %+v", hub.lastReq)
	}
	if hub.lastReq.ActorID != "u-1" || hub.lastReq.ActorName != "alice" {
		t.Errorf("actor wiring: %+v", hub.lastReq)
	}
}

func TestPropagateCacheConfig_Success(t *testing.T) {
	mock, db := newMockDB(t)
	hub := &fakeHub{resp: &hub.ConfigChangeResponse{OK: true, Version: 7}}
	h := newHandler(t, db, hub, nil)
	expectAssembleBlobOK(mock, "")
	if err := h.propagateCacheConfig(context.Background(), "u-2", "bob"); err != nil {
		t.Fatalf("err: %v", err)
	}
	if hub.hits != 1 {
		t.Errorf("expected 1 hub hit; got %d", hub.hits)
	}
}

func TestCacheGetGlobal_Happy(t *testing.T) {
	mock, db := newMockDB(t)
	h := newHandler(t, db, nil, nil)
	mock.ExpectQuery(`FROM cache_global_config`).
		WillReturnRows(pgxmock.NewRows([]string{"config"}).
			AddRow([]byte(`{"normaliser_enabled":true,"cache_master_kill_switch":false}`)))

	req := httptest.NewRequest(http.MethodGet, "/api/admin/cache/global", nil)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "alice", "user-1")
	if err := h.CacheGetGlobal(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got cacheconfig.GlobalConfig
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !got.NormaliserEnabled {
		t.Errorf("body = %s", rec.Body.String())
	}
}

func TestCacheGetGlobal_DBErr500(t *testing.T) {
	mock, db := newMockDB(t)
	h := newHandler(t, db, nil, nil)
	mock.ExpectQuery(`FROM cache_global_config`).WillReturnError(errors.New("planner err"))

	req := httptest.NewRequest(http.MethodGet, "/api/admin/cache/global", nil)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "alice", "user-1")
	if err := h.CacheGetGlobal(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d", rec.Code)
	}
}

// CachePutGlobal — bind err / DB err / hub err / audit success

func TestCachePutGlobal_BindErr(t *testing.T) {
	_, db := newMockDB(t)
	h := newHandler(t, db, nil, nil)
	req := httptest.NewRequest(http.MethodPut, "/api/admin/cache/global", strings.NewReader("not-json"))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "alice", "user-1")
	if err := h.CachePutGlobal(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestCachePutGlobal_DBErr500(t *testing.T) {
	mock, db := newMockDB(t)
	h := newHandler(t, db, nil, nil)
	mock.ExpectExec(`INSERT INTO cache_global_config`).
		WithArgs(pgxmock.AnyArg(), "user-1").
		WillReturnError(errors.New("dup key"))

	body := bytes.NewReader([]byte(`{"normaliser_enabled":true}`))
	req := httptest.NewRequest(http.MethodPut, "/api/admin/cache/global", body)
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "alice", "user-1")
	if err := h.CachePutGlobal(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestCachePutGlobal_HubPropErr502(t *testing.T) {
	mock, db := newMockDB(t)
	spy := &auditSpy{}
	hub := &fakeHub{err: errors.New("hub down")}
	h := newHandler(t, db, hub, spy)

	mock.ExpectExec(`INSERT INTO cache_global_config`).
		WithArgs(pgxmock.AnyArg(), "user-1").
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	expectAssembleBlobOK(mock, "")

	req := httptest.NewRequest(http.MethodPut, "/api/admin/cache/global",
		bytes.NewReader([]byte(`{"normaliser_enabled":true}`)))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "alice", "user-1")
	if err := h.CachePutGlobal(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	if spy.count() != 0 {
		t.Errorf("audit must not fire on hub err; count=%d", spy.count())
	}
	if !strings.Contains(rec.Body.String(), "propagation_error") {
		t.Errorf("missing propagation_error; body = %s", rec.Body.String())
	}
}

func TestCachePutGlobal_HappyAuditFires(t *testing.T) {
	mock, db := newMockDB(t)
	spy := &auditSpy{}
	hub := &fakeHub{resp: &hub.ConfigChangeResponse{OK: true}}
	h := newHandler(t, db, hub, spy)

	mock.ExpectExec(`INSERT INTO cache_global_config`).
		WithArgs(pgxmock.AnyArg(), "user-1").
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	expectAssembleBlobOK(mock, "")

	req := httptest.NewRequest(http.MethodPut, "/api/admin/cache/global",
		bytes.NewReader([]byte(`{"normaliser_enabled":true,"cache_master_kill_switch":false}`)))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "alice", "user-1")
	if err := h.CachePutGlobal(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	if spy.count() != 1 {
		t.Fatalf("audit count = %d", spy.count())
	}
	entry := spy.last()
	if entry["entityId"] != "global" {
		t.Errorf("audit entityId = %v", entry["entityId"])
	}
	if entry["action"] != "update" {
		t.Errorf("audit action = %v", entry["action"])
	}
}

func TestCacheListAdapters_Happy(t *testing.T) {
	mock, db := newMockDB(t)
	h := newHandler(t, db, nil, nil)
	mock.ExpectQuery(`FROM cache_adapter_config`).
		WillReturnRows(pgxmock.NewRows([]string{"adapter_type", "config"}).
			AddRow("openai", []byte(`{}`)).
			AddRow("anthropic", []byte(`{}`)))

	req := httptest.NewRequest(http.MethodGet, "/api/admin/cache/adapters", nil)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "alice", "user-1")
	if err := h.CacheListAdapters(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["total"].(float64) != 2 {
		t.Errorf("total = %v", out["total"])
	}
}

func TestCacheListAdapters_DBErr500(t *testing.T) {
	mock, db := newMockDB(t)
	h := newHandler(t, db, nil, nil)
	mock.ExpectQuery(`FROM cache_adapter_config`).WillReturnError(errors.New("err"))

	req := httptest.NewRequest(http.MethodGet, "/api/admin/cache/adapters", nil)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "alice", "user-1")
	if err := h.CacheListAdapters(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d", rec.Code)
	}
}

// CacheGetAdapter — happy / not found / DB err

func TestCacheGetAdapter_Happy(t *testing.T) {
	mock, db := newMockDB(t)
	h := newHandler(t, db, nil, nil)
	mock.ExpectQuery(`FROM cache_adapter_config`).
		WithArgs("anthropic").
		WillReturnRows(pgxmock.NewRows([]string{"config"}).
			AddRow([]byte(`{"marker_inject_enabled":true}`)))

	req := httptest.NewRequest(http.MethodGet, "/api/admin/cache/adapter/anthropic", nil)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "alice", "user-1")
	c.SetParamNames("adapter_type")
	c.SetParamValues("anthropic")
	if err := h.CacheGetAdapter(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestCacheGetAdapter_NotFound404(t *testing.T) {
	mock, db := newMockDB(t)
	h := newHandler(t, db, nil, nil)
	mock.ExpectQuery(`FROM cache_adapter_config`).
		WithArgs("nope").WillReturnError(pgx.ErrNoRows)

	req := httptest.NewRequest(http.MethodGet, "/api/admin/cache/adapter/nope", nil)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "alice", "user-1")
	c.SetParamNames("adapter_type")
	c.SetParamValues("nope")
	if err := h.CacheGetAdapter(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestCacheGetAdapter_DBErr500(t *testing.T) {
	mock, db := newMockDB(t)
	h := newHandler(t, db, nil, nil)
	mock.ExpectQuery(`FROM cache_adapter_config`).
		WithArgs("x").WillReturnError(errors.New("boom"))

	req := httptest.NewRequest(http.MethodGet, "/api/admin/cache/adapter/x", nil)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "alice", "user-1")
	c.SetParamNames("adapter_type")
	c.SetParamValues("x")
	if err := h.CacheGetAdapter(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d", rec.Code)
	}
}

// CachePutAdapter — bind / validate / DB / hub / happy

func TestCachePutAdapter_BindErr(t *testing.T) {
	_, db := newMockDB(t)
	h := newHandler(t, db, nil, nil)
	req := httptest.NewRequest(http.MethodPut, "/x",
		strings.NewReader("not-json"))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "alice", "user-1")
	c.SetParamNames("adapter_type")
	c.SetParamValues("anthropic")
	if err := h.CachePutAdapter(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestCachePutAdapter_KnobMismatchErr400(t *testing.T) {
	_, db := newMockDB(t)
	h := newHandler(t, db, nil, nil)
	// Anthropic adapter receives a gemini-only knob → validator rejects.
	body := bytes.NewReader([]byte(`{"cache_enabled":true}`))
	req := httptest.NewRequest(http.MethodPut, "/x", body)
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "alice", "user-1")
	c.SetParamNames("adapter_type")
	c.SetParamValues("anthropic")
	if err := h.CachePutAdapter(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "adapter_mismatch") {
		t.Errorf("missing adapter_mismatch code; body = %s", rec.Body.String())
	}
}

func TestCachePutAdapter_DBErr500(t *testing.T) {
	mock, db := newMockDB(t)
	h := newHandler(t, db, nil, nil)
	mock.ExpectExec(`INSERT INTO cache_adapter_config`).
		WithArgs("anthropic", pgxmock.AnyArg(), "user-1").
		WillReturnError(errors.New("planner err"))

	body := bytes.NewReader([]byte(`{"marker_inject_enabled":true}`))
	req := httptest.NewRequest(http.MethodPut, "/x", body)
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "alice", "user-1")
	c.SetParamNames("adapter_type")
	c.SetParamValues("anthropic")
	if err := h.CachePutAdapter(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestCachePutAdapter_HubErr502(t *testing.T) {
	mock, db := newMockDB(t)
	spy := &auditSpy{}
	hub := &fakeHub{err: errors.New("hub down")}
	h := newHandler(t, db, hub, spy)

	mock.ExpectExec(`INSERT INTO cache_adapter_config`).
		WithArgs("anthropic", pgxmock.AnyArg(), "user-1").
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	expectAssembleBlobOK(mock, "")

	body := bytes.NewReader([]byte(`{"marker_inject_enabled":true}`))
	req := httptest.NewRequest(http.MethodPut, "/x", body)
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "alice", "user-1")
	c.SetParamNames("adapter_type")
	c.SetParamValues("anthropic")
	if err := h.CachePutAdapter(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusBadGateway {
		t.Errorf("code = %d", rec.Code)
	}
	if spy.count() != 0 {
		t.Errorf("audit fired despite hub err; count=%d", spy.count())
	}
}

func TestCachePutAdapter_HappyAuditFires(t *testing.T) {
	mock, db := newMockDB(t)
	spy := &auditSpy{}
	hub := &fakeHub{resp: &hub.ConfigChangeResponse{OK: true}}
	h := newHandler(t, db, hub, spy)

	mock.ExpectExec(`INSERT INTO cache_adapter_config`).
		WithArgs("anthropic", pgxmock.AnyArg(), "user-1").
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	expectAssembleBlobOK(mock, "")

	body := bytes.NewReader([]byte(`{"marker_inject_enabled":true}`))
	req := httptest.NewRequest(http.MethodPut, "/x", body)
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "alice", "user-1")
	c.SetParamNames("adapter_type")
	c.SetParamValues("anthropic")
	if err := h.CachePutAdapter(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	if spy.count() != 1 {
		t.Fatalf("audit count = %d", spy.count())
	}
	entry := spy.last()
	if entry["entityId"] != "adapter:anthropic" {
		t.Errorf("entityId = %v", entry["entityId"])
	}
}

// CacheGetProvider — happy / DB err

func TestCacheGetProvider_Happy(t *testing.T) {
	mock, db := newMockDB(t)
	h := newHandler(t, db, nil, nil)
	mock.ExpectQuery(`FROM cache_provider_config`).
		WithArgs("prov-1").
		WillReturnRows(pgxmock.NewRows([]string{"config"}).
			AddRow([]byte(`{"cache_enabled":true}`)))

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "alice", "user-1")
	c.SetParamNames("provider_id")
	c.SetParamValues("prov-1")
	if err := h.CacheGetProvider(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
}

func TestCacheGetProvider_NotFoundReturns200Zero(t *testing.T) {
	// The handler does not distinguish missing rows from empty rows —
	// non-existent provider returns zero-value ProviderConfig with 200.
	mock, db := newMockDB(t)
	h := newHandler(t, db, nil, nil)
	mock.ExpectQuery(`FROM cache_provider_config`).
		WithArgs("absent").WillReturnError(pgx.ErrNoRows)

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "alice", "user-1")
	c.SetParamNames("provider_id")
	c.SetParamValues("absent")
	if err := h.CacheGetProvider(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestCacheGetProvider_DBErr500(t *testing.T) {
	mock, db := newMockDB(t)
	h := newHandler(t, db, nil, nil)
	mock.ExpectQuery(`FROM cache_provider_config`).
		WithArgs("x").WillReturnError(errors.New("boom"))

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "alice", "user-1")
	c.SetParamNames("provider_id")
	c.SetParamValues("x")
	if err := h.CacheGetProvider(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d", rec.Code)
	}
}

// CachePutProvider — bind / lookup-err / not-found / validate-err / DB / hub /
// happy

func TestCachePutProvider_BindErr(t *testing.T) {
	_, db := newMockDB(t)
	h := newHandler(t, db, nil, nil)
	req := httptest.NewRequest(http.MethodPut, "/x", strings.NewReader("not-json"))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "alice", "user-1")
	c.SetParamNames("provider_id")
	c.SetParamValues("prov-1")
	if err := h.CachePutProvider(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestCachePutProvider_LookupErr500(t *testing.T) {
	mock, db := newMockDB(t)
	h := newHandler(t, db, nil, nil)
	mock.ExpectQuery(`FROM "Provider"`).
		WithArgs("prov-1").WillReturnError(errors.New("planner err"))

	body := bytes.NewReader([]byte(`{"cache_enabled":true}`))
	req := httptest.NewRequest(http.MethodPut, "/x", body)
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "alice", "user-1")
	c.SetParamNames("provider_id")
	c.SetParamValues("prov-1")
	if err := h.CachePutProvider(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestCachePutProvider_NotFound404(t *testing.T) {
	mock, db := newMockDB(t)
	h := newHandler(t, db, nil, nil)
	mock.ExpectQuery(`FROM "Provider"`).
		WithArgs("absent").WillReturnError(pgx.ErrNoRows)

	body := bytes.NewReader([]byte(`{"cache_enabled":true}`))
	req := httptest.NewRequest(http.MethodPut, "/x", body)
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "alice", "user-1")
	c.SetParamNames("provider_id")
	c.SetParamValues("absent")
	if err := h.CachePutProvider(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestCachePutProvider_KnobMismatch400(t *testing.T) {
	mock, db := newMockDB(t)
	h := newHandler(t, db, nil, nil)
	mock.ExpectQuery(`FROM "Provider"`).
		WithArgs("prov-1").
		WillReturnRows(pgxmock.NewRows([]string{"adapter_type"}).AddRow("anthropic"))

	// Anthropic provider with gemini knob.
	body := bytes.NewReader([]byte(`{"cache_enabled":true}`))
	req := httptest.NewRequest(http.MethodPut, "/x", body)
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "alice", "user-1")
	c.SetParamNames("provider_id")
	c.SetParamValues("prov-1")
	if err := h.CachePutProvider(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "adapter_mismatch") {
		t.Errorf("missing adapter_mismatch; body = %s", rec.Body.String())
	}
}

func TestCachePutProvider_DBErr500(t *testing.T) {
	mock, db := newMockDB(t)
	h := newHandler(t, db, nil, nil)
	mock.ExpectQuery(`FROM "Provider"`).
		WithArgs("prov-1").
		WillReturnRows(pgxmock.NewRows([]string{"adapter_type"}).AddRow("gemini"))
	mock.ExpectExec(`INSERT INTO cache_provider_config`).
		WithArgs("prov-1", pgxmock.AnyArg(), "user-1").
		WillReturnError(errors.New("planner err"))

	body := bytes.NewReader([]byte(`{"cache_enabled":true}`))
	req := httptest.NewRequest(http.MethodPut, "/x", body)
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "alice", "user-1")
	c.SetParamNames("provider_id")
	c.SetParamValues("prov-1")
	if err := h.CachePutProvider(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestCachePutProvider_HubErr502(t *testing.T) {
	mock, db := newMockDB(t)
	spy := &auditSpy{}
	hub := &fakeHub{err: errors.New("hub down")}
	h := newHandler(t, db, hub, spy)

	mock.ExpectQuery(`FROM "Provider"`).
		WithArgs("prov-1").
		WillReturnRows(pgxmock.NewRows([]string{"adapter_type"}).AddRow("gemini"))
	mock.ExpectExec(`INSERT INTO cache_provider_config`).
		WithArgs("prov-1", pgxmock.AnyArg(), "user-1").
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	expectAssembleBlobOK(mock, "")

	body := bytes.NewReader([]byte(`{"cache_enabled":true}`))
	req := httptest.NewRequest(http.MethodPut, "/x", body)
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "alice", "user-1")
	c.SetParamNames("provider_id")
	c.SetParamValues("prov-1")
	if err := h.CachePutProvider(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusBadGateway {
		t.Errorf("code = %d", rec.Code)
	}
	if spy.count() != 0 {
		t.Errorf("audit fired despite hub err; count=%d", spy.count())
	}
}

func TestCachePutProvider_HappyAuditFires(t *testing.T) {
	mock, db := newMockDB(t)
	spy := &auditSpy{}
	hub := &fakeHub{resp: &hub.ConfigChangeResponse{OK: true}}
	h := newHandler(t, db, hub, spy)

	mock.ExpectQuery(`FROM "Provider"`).
		WithArgs("prov-1").
		WillReturnRows(pgxmock.NewRows([]string{"adapter_type"}).AddRow("gemini"))
	mock.ExpectExec(`INSERT INTO cache_provider_config`).
		WithArgs("prov-1", pgxmock.AnyArg(), "user-1").
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	expectAssembleBlobOK(mock, "")

	body := bytes.NewReader([]byte(`{"cache_enabled":true}`))
	req := httptest.NewRequest(http.MethodPut, "/x", body)
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "alice", "user-1")
	c.SetParamNames("provider_id")
	c.SetParamValues("prov-1")
	if err := h.CachePutProvider(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	if spy.count() != 1 {
		t.Fatalf("audit count = %d", spy.count())
	}
	entry := spy.last()
	if entry["entityId"] != "provider:prov-1" {
		t.Errorf("entityId = %v", entry["entityId"])
	}
}

// CacheDeleteProvider — DB err / hub err / happy

func TestCacheDeleteProvider_DBErr500(t *testing.T) {
	mock, db := newMockDB(t)
	h := newHandler(t, db, nil, nil)
	mock.ExpectExec(`DELETE FROM cache_provider_config`).
		WithArgs("prov-1").
		WillReturnError(errors.New("planner err"))

	req := httptest.NewRequest(http.MethodDelete, "/x", nil)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "alice", "user-1")
	c.SetParamNames("provider_id")
	c.SetParamValues("prov-1")
	if err := h.CacheDeleteProvider(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestCacheDeleteProvider_HubErr502(t *testing.T) {
	mock, db := newMockDB(t)
	spy := &auditSpy{}
	hub := &fakeHub{err: errors.New("hub down")}
	h := newHandler(t, db, hub, spy)

	mock.ExpectExec(`DELETE FROM cache_provider_config`).
		WithArgs("prov-1").
		WillReturnResult(pgconn.NewCommandTag("DELETE 1"))
	expectAssembleBlobOK(mock, "")

	req := httptest.NewRequest(http.MethodDelete, "/x", nil)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "alice", "user-1")
	c.SetParamNames("provider_id")
	c.SetParamValues("prov-1")
	if err := h.CacheDeleteProvider(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusBadGateway {
		t.Errorf("code = %d", rec.Code)
	}
	if spy.count() != 0 {
		t.Errorf("audit fired despite hub err; count=%d", spy.count())
	}
}

func TestCacheDeleteProvider_Happy204AuditFires(t *testing.T) {
	mock, db := newMockDB(t)
	spy := &auditSpy{}
	hub := &fakeHub{resp: &hub.ConfigChangeResponse{OK: true}}
	h := newHandler(t, db, hub, spy)

	mock.ExpectExec(`DELETE FROM cache_provider_config`).
		WithArgs("prov-1").
		WillReturnResult(pgconn.NewCommandTag("DELETE 1"))
	expectAssembleBlobOK(mock, "")

	req := httptest.NewRequest(http.MethodDelete, "/x", nil)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "alice", "user-1")
	c.SetParamNames("provider_id")
	c.SetParamValues("prov-1")
	if err := h.CacheDeleteProvider(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusNoContent {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	if spy.count() != 1 {
		t.Fatalf("audit count = %d", spy.count())
	}
	entry := spy.last()
	if entry["entityId"] != "provider:prov-1" {
		t.Errorf("entityId = %v", entry["entityId"])
	}
	if entry["action"] != "update" {
		t.Errorf("action = %v", entry["action"])
	}
}

// CacheGetEffective — missing provider_id / lookup err / not-found /
// blob err / happy

func TestCacheGetEffective_MissingProviderID(t *testing.T) {
	_, db := newMockDB(t)
	h := newHandler(t, db, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/admin/cache/effective", nil)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "alice", "user-1")
	if err := h.CacheGetEffective(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestCacheGetEffective_LookupErr500(t *testing.T) {
	mock, db := newMockDB(t)
	h := newHandler(t, db, nil, nil)
	mock.ExpectQuery(`FROM "Provider"`).
		WithArgs("prov-1").WillReturnError(errors.New("boom"))

	req := httptest.NewRequest(http.MethodGet, "/api/admin/cache/effective?provider_id=prov-1", nil)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "alice", "user-1")
	if err := h.CacheGetEffective(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestCacheGetEffective_NotFound404(t *testing.T) {
	mock, db := newMockDB(t)
	h := newHandler(t, db, nil, nil)
	mock.ExpectQuery(`FROM "Provider"`).
		WithArgs("absent").WillReturnError(pgx.ErrNoRows)

	req := httptest.NewRequest(http.MethodGet, "/api/admin/cache/effective?provider_id=absent", nil)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "alice", "user-1")
	if err := h.CacheGetEffective(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestCacheGetEffective_BlobErr500(t *testing.T) {
	mock, db := newMockDB(t)
	h := newHandler(t, db, nil, nil)
	mock.ExpectQuery(`FROM "Provider"`).
		WithArgs("prov-1").
		WillReturnRows(pgxmock.NewRows([]string{"adapter_type"}).AddRow("gemini"))
	// GetProviderName runs even when blob assembly later fails — succeed it.
	mock.ExpectQuery(`SELECT name FROM "Provider"`).
		WithArgs("prov-1").
		WillReturnRows(pgxmock.NewRows([]string{"name"}).AddRow("Gemini Prov"))
	mock.ExpectQuery(`FROM cache_global_config`).WillReturnError(errors.New("boom"))

	req := httptest.NewRequest(http.MethodGet, "/api/admin/cache/effective?provider_id=prov-1", nil)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "alice", "user-1")
	if err := h.CacheGetEffective(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestCacheGetEffective_Happy(t *testing.T) {
	mock, db := newMockDB(t)
	h := newHandler(t, db, nil, nil)
	mock.ExpectQuery(`FROM "Provider"`).
		WithArgs("prov-1").
		WillReturnRows(pgxmock.NewRows([]string{"adapter_type"}).AddRow("gemini"))
	mock.ExpectQuery(`SELECT name FROM "Provider"`).
		WithArgs("prov-1").
		WillReturnRows(pgxmock.NewRows([]string{"name"}).AddRow("Gemini Prov"))
	expectAssembleBlobOK(mock, "prov-1")

	req := httptest.NewRequest(http.MethodGet, "/api/admin/cache/effective?provider_id=prov-1", nil)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "alice", "user-1")
	if err := h.CacheGetEffective(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got effectiveResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ProviderID != "prov-1" || got.AdapterType != "gemini" || got.ProviderName != "Gemini Prov" {
		t.Errorf("got = %+v", got)
	}
	if _, ok := got.Effective["cache_enabled"]; !ok {
		t.Errorf("missing cache_enabled in effective; got %+v", got.Effective)
	}
}

func TestCacheGetEffective_NameErrIsLoggedNotFatal(t *testing.T) {
	// GetProviderName returning a generic err only logs — must NOT short-circuit
	// (the handler proceeds with empty name and continues to AssembleBlob).
	mock, db := newMockDB(t)
	h := newHandler(t, db, nil, nil)
	mock.ExpectQuery(`FROM "Provider"`).
		WithArgs("prov-1").
		WillReturnRows(pgxmock.NewRows([]string{"adapter_type"}).AddRow("gemini"))
	mock.ExpectQuery(`SELECT name FROM "Provider"`).
		WithArgs("prov-1").WillReturnError(errors.New("planner err"))
	expectAssembleBlobOK(mock, "")

	req := httptest.NewRequest(http.MethodGet, "/api/admin/cache/effective?provider_id=prov-1", nil)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "alice", "user-1")
	if err := h.CacheGetEffective(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d", rec.Code)
	}
}

// CacheListOverrides — blob err / orphan / happy

func TestCacheListOverrides_BlobErr500(t *testing.T) {
	mock, db := newMockDB(t)
	h := newHandler(t, db, nil, nil)
	mock.ExpectQuery(`FROM cache_global_config`).WillReturnError(errors.New("boom"))

	req := httptest.NewRequest(http.MethodGet, "/api/admin/cache/overrides", nil)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "alice", "user-1")
	if err := h.CacheListOverrides(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestCacheListOverrides_EmptyFiltersOut(t *testing.T) {
	// Empty ProviderConfig rows should be filtered (isProviderConfigEmpty
	// short-circuit).
	mock, db := newMockDB(t)
	h := newHandler(t, db, nil, nil)
	mock.ExpectQuery(`FROM cache_global_config`).
		WillReturnRows(pgxmock.NewRows([]string{"config"}).AddRow([]byte(`{}`)))
	mock.ExpectQuery(`FROM cache_adapter_config`).
		WillReturnRows(pgxmock.NewRows([]string{"adapter_type", "config"}))
	mock.ExpectQuery(`FROM cache_provider_config`).
		WillReturnRows(pgxmock.NewRows([]string{"provider_id", "config"}).
			AddRow("prov-empty", []byte(`{}`)))

	req := httptest.NewRequest(http.MethodGet, "/api/admin/cache/overrides", nil)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "alice", "user-1")
	if err := h.CacheListOverrides(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["total"].(float64) != 0 {
		t.Errorf("total = %v; want 0 (empty filtered)", out["total"])
	}
}

func TestCacheListOverrides_OrphanSkipped(t *testing.T) {
	// Non-empty override + Provider row not found → silently skip.
	mock, db := newMockDB(t)
	h := newHandler(t, db, nil, nil)
	mock.ExpectQuery(`FROM cache_global_config`).
		WillReturnRows(pgxmock.NewRows([]string{"config"}).AddRow([]byte(`{}`)))
	mock.ExpectQuery(`FROM cache_adapter_config`).
		WillReturnRows(pgxmock.NewRows([]string{"adapter_type", "config"}))
	mock.ExpectQuery(`FROM cache_provider_config`).
		WillReturnRows(pgxmock.NewRows([]string{"provider_id", "config"}).
			AddRow("orphan", []byte(`{"cache_enabled":true}`)))
	mock.ExpectQuery(`FROM "Provider"`).
		WithArgs("orphan").WillReturnError(pgx.ErrNoRows)

	req := httptest.NewRequest(http.MethodGet, "/api/admin/cache/overrides", nil)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "alice", "user-1")
	if err := h.CacheListOverrides(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["total"].(float64) != 0 {
		t.Errorf("total = %v; want 0 (orphan skipped)", out["total"])
	}
}

func TestCacheListOverrides_OrphanGenericErrAlsoSkipped(t *testing.T) {
	// Non-empty override + Provider lookup err → silently skip.
	mock, db := newMockDB(t)
	h := newHandler(t, db, nil, nil)
	mock.ExpectQuery(`FROM cache_global_config`).
		WillReturnRows(pgxmock.NewRows([]string{"config"}).AddRow([]byte(`{}`)))
	mock.ExpectQuery(`FROM cache_adapter_config`).
		WillReturnRows(pgxmock.NewRows([]string{"adapter_type", "config"}))
	mock.ExpectQuery(`FROM cache_provider_config`).
		WillReturnRows(pgxmock.NewRows([]string{"provider_id", "config"}).
			AddRow("bad", []byte(`{"cache_enabled":true}`)))
	mock.ExpectQuery(`FROM "Provider"`).
		WithArgs("bad").WillReturnError(errors.New("planner err"))

	req := httptest.NewRequest(http.MethodGet, "/api/admin/cache/overrides", nil)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "alice", "user-1")
	if err := h.CacheListOverrides(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestCacheListOverrides_HappyMultiKnobDiff(t *testing.T) {
	// Override for prov-1 (gemini) sets cache_enabled + ttl + cb_threshold +
	// cb_open_secs + min_system_chars + marker knobs (cross-family for diff
	// coverage). Verifies every recordDiff branch fires.
	mock, db := newMockDB(t)
	h := newHandler(t, db, nil, nil)
	mock.ExpectQuery(`FROM cache_global_config`).
		WillReturnRows(pgxmock.NewRows([]string{"config"}).AddRow([]byte(`{}`)))
	mock.ExpectQuery(`FROM cache_adapter_config`).
		WillReturnRows(pgxmock.NewRows([]string{"adapter_type", "config"}))
	mock.ExpectQuery(`FROM cache_provider_config`).
		WillReturnRows(pgxmock.NewRows([]string{"provider_id", "config"}).
			AddRow("prov-1", []byte(`{
				"marker_inject_enabled":true,
				"marker_boundary3_enabled":false,
				"cache_enabled":true,
				"min_system_chars":1234,
				"ttl_seconds":42,
				"circuit_breaker_threshold":7,
				"circuit_breaker_open_secs":99
			}`)))
	mock.ExpectQuery(`FROM "Provider"`).
		WithArgs("prov-1").
		WillReturnRows(pgxmock.NewRows([]string{"adapter_type"}).AddRow("gemini"))
	mock.ExpectQuery(`SELECT name FROM "Provider"`).
		WithArgs("prov-1").
		WillReturnRows(pgxmock.NewRows([]string{"name"}).AddRow("Gem One"))

	req := httptest.NewRequest(http.MethodGet, "/api/admin/cache/overrides", nil)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "alice", "user-1")
	if err := h.CacheListOverrides(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Items []overrideRow `json:"items"`
		Total int           `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Total != 1 || len(out.Items) != 1 {
		t.Fatalf("got %+v", out)
	}
	row := out.Items[0]
	if row.ProviderName != "Gem One" || row.AdapterType != "gemini" {
		t.Errorf("row meta = %+v", row)
	}
	if len(row.OverriddenKeys) != 7 {
		t.Errorf("expected 7 overridden keys; got %v", row.OverriddenKeys)
	}
	// Sorted by recordDiff insertion + sort.Strings — verify by membership.
	want := map[string]bool{
		"marker_inject_enabled": true, "marker_boundary3_enabled": true,
		"cache_enabled": true, "min_system_chars": true, "ttl_seconds": true,
		"circuit_breaker_threshold": true, "circuit_breaker_open_secs": true,
	}
	for _, k := range row.OverriddenKeys {
		if !want[k] {
			t.Errorf("unexpected key %q in overridden_keys", k)
		}
	}
}

func TestCacheListOverrides_SortedByProviderName(t *testing.T) {
	// Two provider overrides with names "B" and "A" → final list sorted A then B.
	mock, db := newMockDB(t)
	h := newHandler(t, db, nil, nil)
	mock.ExpectQuery(`FROM cache_global_config`).
		WillReturnRows(pgxmock.NewRows([]string{"config"}).AddRow([]byte(`{}`)))
	mock.ExpectQuery(`FROM cache_adapter_config`).
		WillReturnRows(pgxmock.NewRows([]string{"adapter_type", "config"}))
	mock.ExpectQuery(`FROM cache_provider_config`).
		WillReturnRows(pgxmock.NewRows([]string{"provider_id", "config"}).
			AddRow("prov-b", []byte(`{"cache_enabled":true}`)).
			AddRow("prov-a", []byte(`{"cache_enabled":true}`)))
	// Order of Provider/Name lookups is map-iteration-order — accept any.
	mock.MatchExpectationsInOrder(false)
	mock.ExpectQuery(`FROM "Provider"`).
		WithArgs("prov-a").
		WillReturnRows(pgxmock.NewRows([]string{"adapter_type"}).AddRow("gemini"))
	mock.ExpectQuery(`SELECT name FROM "Provider"`).
		WithArgs("prov-a").
		WillReturnRows(pgxmock.NewRows([]string{"name"}).AddRow("Alpha"))
	mock.ExpectQuery(`FROM "Provider"`).
		WithArgs("prov-b").
		WillReturnRows(pgxmock.NewRows([]string{"adapter_type"}).AddRow("gemini"))
	mock.ExpectQuery(`SELECT name FROM "Provider"`).
		WithArgs("prov-b").
		WillReturnRows(pgxmock.NewRows([]string{"name"}).AddRow("Beta"))

	req := httptest.NewRequest(http.MethodGet, "/api/admin/cache/overrides", nil)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "alice", "user-1")
	if err := h.CacheListOverrides(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Items []overrideRow `json:"items"`
		Total int           `json:"total"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Total != 2 || out.Items[0].ProviderName != "Alpha" || out.Items[1].ProviderName != "Beta" {
		t.Errorf("not sorted; got %+v", out.Items)
	}
}

// RegisterRoutes — IAM wiring smoke test (mounts every route, fires GET without
// admin auth → 401/403). Confirms the route table is wired up at all.

func TestRegisterRoutes_IAMDeniesUnauthenticated(t *testing.T) {
	_, db := newMockDB(t)
	h := newHandler(t, db, nil, nil)
	e := echo.New()
	eng := iam.NewEngine(nil, slog.Default())
	iamMW := func(action string) echo.MiddlewareFunc {
		return middleware.RequireIAMPermission(eng, action, nil)
	}
	g := e.Group("/api/admin")
	h.RegisterRoutes(g, iamMW)

	// One probe per registered path / method combination.
	probes := []struct {
		method, path string
	}{
		{http.MethodGet, "/api/admin/cache/global"},
		{http.MethodPut, "/api/admin/cache/global"},
		{http.MethodGet, "/api/admin/cache/adapters"},
		{http.MethodGet, "/api/admin/cache/adapter/openai"},
		{http.MethodPut, "/api/admin/cache/adapter/openai"},
		{http.MethodGet, "/api/admin/cache/provider/prov-1"},
		{http.MethodPut, "/api/admin/cache/provider/prov-1"},
		{http.MethodDelete, "/api/admin/cache/provider/prov-1"},
		{http.MethodGet, "/api/admin/cache/effective?provider_id=prov-1"},
		{http.MethodGet, "/api/admin/cache/overrides"},
	}
	for _, p := range probes {
		req := httptest.NewRequest(p.method, p.path, nil)
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		// No admin auth on context → iam engine denies → 401 or 403.
		if rec.Code != http.StatusForbidden && rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: code = %d (want 401/403); body = %s",
				p.method, p.path, rec.Code, rec.Body.String())
		}
	}
}

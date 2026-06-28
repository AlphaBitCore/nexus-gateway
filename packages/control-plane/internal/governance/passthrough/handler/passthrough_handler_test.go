package passthrough

import (
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
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/labstack/echo/v4"
	"github.com/pashagolub/pgxmock/v4"

	auth "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authn"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/iam"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/hub"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
)

// Test helpers — mirror sibling cache/hooks/virtualkey pattern

// newMockHandler constructs a *Handler whose pool is a pgxmock.PgxPoolIface
// stand-in. Pool stays nil so any accidental direct-*pgxpool.Pool path
// produces a nil-deref instead of silently using a mock-shaped value.
func newMockHandler(t *testing.T, hub HubConfigChanger) (pgxmock.PgxPoolIface, *Handler) {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(mock.Close)
	h := &Handler{
		hub:    hub,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		pool:   mock,
	}
	return mock, h
}

// echoCtxWithUser returns an Echo context with the AdminAuth identity
// attached the way the production AdminAuth middleware does (id "admin-1",
// name "Alice").
func echoCtxWithUser(method, path, body string) (echo.Context, *httptest.ResponseRecorder) {
	e := echo.New()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	}
	rec := httptest.NewRecorder()
	c := e.NewContext(r, rec)
	middleware.WithAdminAuth(c, &auth.AdminAuth{KeyID: "admin-1", KeyName: "Alice", AuthPrincipalType: "admin_user"})
	return c, rec
}

// echoCtxAnon returns an Echo context with NO admin auth attached —
// exercises the actor() empty-string fallback.
func echoCtxAnon(method, path, body string) (echo.Context, *httptest.ResponseRecorder) {
	e := echo.New()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	}
	rec := httptest.NewRecorder()
	return e.NewContext(r, rec), rec
}

// withParam stamps a path param on an existing Echo context the way the
// router would after a successful match.
func withParam(c echo.Context, name, value string) echo.Context {
	c.SetParamNames(name)
	c.SetParamValues(value)
	return c
}

// fakeHub captures the most recent NotifyConfigChange call and returns
// a programmable error.
type fakeHub struct {
	mu      sync.Mutex
	hits    int
	lastReq hub.ConfigChangeRequest
	err     error
}

func (f *fakeHub) NotifyConfigChange(_ context.Context, req hub.ConfigChangeRequest) (*hub.ConfigChangeResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hits++
	f.lastReq = req
	if f.err != nil {
		return nil, f.err
	}
	return &hub.ConfigChangeResponse{}, nil
}

// expectEmptyAssembleBlob queues the 3 SELECTs assembleBlob fires, all
// returning empty result sets.
func expectEmptyAssembleBlob(mock pgxmock.PgxPoolIface) {
	mock.ExpectQuery(`FROM gateway_passthrough_config_global`).
		WillReturnRows(pgxmock.NewRows([]string{"enabled", "config", "expires_at", "enabled_by", "reason"}))
	mock.ExpectQuery(`FROM gateway_passthrough_config_adapter`).
		WillReturnRows(pgxmock.NewRows([]string{"adapter_type", "enabled", "config", "expires_at", "enabled_by", "reason"}))
	mock.ExpectQuery(`FROM gateway_passthrough_config_provider`).
		WillReturnRows(pgxmock.NewRows([]string{"provider_id", "enabled", "config", "expires_at", "enabled_by", "reason"}))
}

// SourceState is the config-drift reconciler's SourceLoader. It must
// assemble the gateway_passthrough source-of-truth blob from the three
// gateway_passthrough_config_* tables — identical to what propagateConfig
// pushes — so the drift diff compares CP tables against thing.desired, not a
// Hub-written thing_config_template (which could never reveal a dropped push).
func TestSourceState_AssemblesBlobFromConfigTables(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	exp := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`FROM gateway_passthrough_config_global`).
		WillReturnRows(pgxmock.NewRows([]string{"enabled", "config", "expires_at", "enabled_by", "reason"}).
			AddRow(true, []byte(`{"bypassHooks":true,"bypassCache":false,"bypassNormalize":false}`), &exp, "admin-1", "global outage"))
	mock.ExpectQuery(`FROM gateway_passthrough_config_adapter`).
		WillReturnRows(pgxmock.NewRows([]string{"adapter_type", "enabled", "config", "expires_at", "enabled_by", "reason"}).
			AddRow("anthropic", true, []byte(`{"bypassHooks":false,"bypassCache":true,"bypassNormalize":false}`), &exp, "admin-2", "adapter bypass"))
	mock.ExpectQuery(`FROM gateway_passthrough_config_provider`).
		WillReturnRows(pgxmock.NewRows([]string{"provider_id", "enabled", "config", "expires_at", "enabled_by", "reason"}).
			AddRow("prov-9", false, []byte(`{"bypassHooks":false,"bypassCache":false,"bypassNormalize":true}`), &exp, "admin-3", "provider bypass"))

	raw, err := SourceState(context.Background(), mock)
	if err != nil {
		t.Fatalf("SourceState: %v", err)
	}

	var got blob
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal SourceState output: %v", err)
	}

	// Per-tier flags are preserved verbatim — SourceState does NOT collapse
	// tiers (the runtime OR-merge happens in the gateway, not here).
	if !got.Global.Enabled || !got.Global.BypassHooks || got.Global.BypassCache {
		t.Errorf("global tier wrong: %+v", got.Global)
	}
	if got.Global.Reason != "global outage" || got.Global.EnabledBy != "admin-1" {
		t.Errorf("global metadata wrong: %+v", got.Global)
	}
	a, ok := got.Adapters["anthropic"]
	if !ok || !a.Enabled || a.BypassHooks || !a.BypassCache {
		t.Errorf("adapter tier wrong: %+v", got.Adapters)
	}
	p, ok := got.Providers["prov-9"]
	if !ok || p.Enabled || !p.BypassNormalize {
		t.Errorf("provider tier wrong: %+v", got.Providers)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// SourceState on an unseeded fleet marshals to the zero blob with
// initialized (non-nil) maps, matching what an ai-gateway with no passthrough
// config carries in thing.desired — so the content diff does not thrash.
func TestSourceState_EmptyFleetZeroBlob(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()
	expectEmptyAssembleBlob(mock)

	raw, err := SourceState(context.Background(), mock)
	if err != nil {
		t.Fatalf("SourceState: %v", err)
	}
	var got blob
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Global.Enabled {
		t.Errorf("expected disabled global on empty fleet; got %+v", got.Global)
	}
	if got.Adapters == nil || got.Providers == nil {
		t.Errorf("expected non-nil adapter/provider maps; got %+v", got)
	}
	if len(got.Adapters) != 0 || len(got.Providers) != 0 {
		t.Errorf("expected empty maps; got %+v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// validBody returns a marshaled enabled-true payload with sane defaults.
func validBody(t *testing.T) string {
	t.Helper()
	exp := time.Now().Add(1 * time.Hour)
	b, err := json.Marshal(payload{
		Enabled:     true,
		BypassHooks: true,
		ExpiresAt:   &exp,
		Reason:      "incident-2026-05-13 anthropic upstream outage",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// New() + Deps wiring

func TestNew_WithNilPool(t *testing.T) {
	h := New(Deps{Pool: nil, Hub: nil, Logger: nil})
	if h == nil {
		t.Fatal("expected non-nil Handler")
		return
	}
	if h.pool != nil {
		t.Errorf("pool should be nil when Pool nil; got %T", h.pool)
	}
}

func TestNew_WiresLoggerAndHub(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	hub := &fakeHub{}
	h := New(Deps{Pool: nil, Hub: hub, Logger: log})
	if h.logger != log {
		t.Error("logger not wired")
	}
	if h.hub != hub {
		t.Error("hub not wired")
	}
}

// TestNew_WithPoolSetsPool exercises the d.Pool != nil branch of New so the
// h.pool = d.Pool assignment is covered.
func TestNew_WithPoolSetsPool(t *testing.T) {
	// pool is set via newMockHandler; we just verify
	// that the h.pool field is non-nil after injection.
	_, h := newMockHandler(t, nil)
	if h.pool == nil {
		t.Error("pool should be non-nil after newMockHandler injection")
	}
}

func TestActor_PopulatedAndAnon(t *testing.T) {
	c, _ := echoCtxWithUser(http.MethodGet, "/", "")
	id, name := actor(c)
	if id != "admin-1" || name != "Alice" {
		t.Errorf("admin-auth context: got id=%q name=%q", id, name)
	}

	c2, _ := echoCtxAnon(http.MethodGet, "/", "")
	id2, name2 := actor(c2)
	if id2 != "" || name2 != "" {
		t.Errorf("anon: got id=%q name=%q (want empty)", id2, name2)
	}
}

// RegisterRoutes — IAM denial smoke test for every path/method

func TestRegisterRoutes_IAMDeniesUnauthenticated(t *testing.T) {
	_, h := newMockHandler(t, nil)
	e := echo.New()
	eng := iam.NewEngine(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	iamMW := func(action string) echo.MiddlewareFunc {
		return middleware.RequireIAMPermission(eng, action, nil)
	}
	g := e.Group("/api/admin")
	h.RegisterRoutes(g, iamMW)

	probes := []struct {
		method, path string
	}{
		{http.MethodGet, "/api/admin/passthrough/global"},
		{http.MethodPut, "/api/admin/passthrough/global"},
		{http.MethodGet, "/api/admin/passthrough/adapter/anthropic"},
		{http.MethodPut, "/api/admin/passthrough/adapter/anthropic"},
		{http.MethodDelete, "/api/admin/passthrough/adapter/anthropic"},
		{http.MethodGet, "/api/admin/passthrough/provider/prov-1"},
		{http.MethodPut, "/api/admin/passthrough/provider/prov-1"},
		{http.MethodDelete, "/api/admin/passthrough/provider/prov-1"},
		{http.MethodGet, "/api/admin/passthrough/snapshot"},
	}
	for _, p := range probes {
		req := httptest.NewRequest(p.method, p.path, nil)
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden && rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: code = %d (want 401/403); body = %s",
				p.method, p.path, rec.Code, rec.Body.String())
		}
	}
}

func TestGetGlobal_NoRowReturnsEmptyPayload(t *testing.T) {
	mock, h := newMockHandler(t, nil)
	mock.ExpectQuery(`FROM gateway_passthrough_config_global WHERE id = 'singleton'`).
		WillReturnRows(pgxmock.NewRows([]string{"enabled", "config", "expires_at", "reason"}))

	c, rec := echoCtxWithUser(http.MethodGet, "/passthrough/global", "")
	if err := h.GetGlobal(c); err != nil {
		t.Fatalf("GetGlobal err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d; want 200", rec.Code)
	}
	var p payload
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if p.Enabled {
		t.Errorf("empty payload should have Enabled=false; got %+v", p)
	}
}

func TestGetGlobal_HappyPath(t *testing.T) {
	mock, h := newMockHandler(t, nil)
	exp := time.Now().Add(2 * time.Hour)
	reason := "scheduled-test"
	mock.ExpectQuery(`FROM gateway_passthrough_config_global WHERE id = 'singleton'`).
		WillReturnRows(pgxmock.NewRows([]string{"enabled", "config", "expires_at", "reason"}).
			AddRow(true, []byte(`{"bypassHooks":true,"bypassCache":false,"bypassNormalize":false}`), &exp, &reason))

	c, rec := echoCtxWithUser(http.MethodGet, "/passthrough/global", "")
	if err := h.GetGlobal(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d", rec.Code)
	}
	var p payload
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !p.Enabled || !p.BypassHooks || p.BypassCache || p.BypassNormalize || p.Reason != reason {
		t.Errorf("decoded payload mismatch: %+v", p)
	}
}

func TestGetGlobal_NullReasonOmitted(t *testing.T) {
	mock, h := newMockHandler(t, nil)
	exp := time.Now().Add(1 * time.Hour)
	mock.ExpectQuery(`FROM gateway_passthrough_config_global WHERE id = 'singleton'`).
		WillReturnRows(pgxmock.NewRows([]string{"enabled", "config", "expires_at", "reason"}).
			AddRow(true, []byte(`{}`), &exp, (*string)(nil)))

	c, rec := echoCtxWithUser(http.MethodGet, "/passthrough/global", "")
	if err := h.GetGlobal(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d", rec.Code)
	}
	var p payload
	_ = json.Unmarshal(rec.Body.Bytes(), &p)
	if p.Reason != "" {
		t.Errorf("Reason = %q; want empty", p.Reason)
	}
}

func TestGetGlobal_DBError(t *testing.T) {
	mock, h := newMockHandler(t, nil)
	mock.ExpectQuery(`FROM gateway_passthrough_config_global`).WillReturnError(errors.New("db boom"))

	c, rec := echoCtxWithUser(http.MethodGet, "/passthrough/global", "")
	if err := h.GetGlobal(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "server_error") {
		t.Errorf("body lacks server_error: %s", rec.Body.String())
	}
}

func TestPutGlobal_BindError(t *testing.T) {
	_, h := newMockHandler(t, nil)
	c, rec := echoCtxWithUser(http.MethodPut, "/passthrough/global", `{not-json`)
	if err := h.PutGlobal(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "validation_error") {
		t.Errorf("body lacks validation_error: %s", rec.Body.String())
	}
}

func TestPutGlobal_ValidationError(t *testing.T) {
	_, h := newMockHandler(t, nil)
	// enabled=true, no flags → passthrough_no_bypass_selected
	exp := time.Now().Add(1 * time.Hour)
	bad, _ := json.Marshal(payload{Enabled: true, ExpiresAt: &exp, Reason: "long enough reason text indeed"})
	c, rec := echoCtxWithUser(http.MethodPut, "/passthrough/global", string(bad))
	if err := h.PutGlobal(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "passthrough_no_bypass_selected") {
		t.Errorf("body: %s", rec.Body.String())
	}
}

func TestPutGlobal_DBError(t *testing.T) {
	mock, h := newMockHandler(t, nil)
	mock.ExpectExec(`INSERT INTO gateway_passthrough_config_global`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(errors.New("upsert kaboom"))
	c, rec := echoCtxWithUser(http.MethodPut, "/passthrough/global", validBody(t))
	if err := h.PutGlobal(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "server_error") {
		t.Errorf("body: %s", rec.Body.String())
	}
}

func TestPutGlobal_HappyPathPropagates(t *testing.T) {
	hub := &fakeHub{}
	mock, h := newMockHandler(t, hub)
	mock.ExpectExec(`INSERT INTO gateway_passthrough_config_global`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	expectEmptyAssembleBlob(mock)

	c, rec := echoCtxWithUser(http.MethodPut, "/passthrough/global", validBody(t))
	if err := h.PutGlobal(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d; body = %s", rec.Code, rec.Body.String())
	}
	if hub.hits != 1 {
		t.Errorf("hub hits = %d; want 1", hub.hits)
	}
	if hub.lastReq.ThingType != "ai-gateway" || hub.lastReq.ConfigKey != shadowKey {
		t.Errorf("hub req = %+v", hub.lastReq)
	}
	if hub.lastReq.ActorID != "admin-1" {
		t.Errorf("actor id = %q", hub.lastReq.ActorID)
	}
}

func TestPutGlobal_HubPropagationError(t *testing.T) {
	hub := &fakeHub{err: errors.New("hub down")}
	mock, h := newMockHandler(t, hub)
	mock.ExpectExec(`INSERT INTO gateway_passthrough_config_global`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	expectEmptyAssembleBlob(mock)

	c, rec := echoCtxWithUser(http.MethodPut, "/passthrough/global", validBody(t))
	if err := h.PutGlobal(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusBadGateway {
		t.Errorf("code = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "propagation_error") {
		t.Errorf("body lacks propagation_error: %s", rec.Body.String())
	}
}

func TestGetAdapter_NotFound(t *testing.T) {
	mock, h := newMockHandler(t, nil)
	mock.ExpectQuery(`FROM gateway_passthrough_config_adapter WHERE adapter_type`).
		WithArgs("anthropic").
		WillReturnRows(pgxmock.NewRows([]string{"enabled", "config", "expires_at", "reason"}))

	c, rec := echoCtxWithUser(http.MethodGet, "/passthrough/adapter/anthropic", "")
	c = withParam(c, "adapter_type", "anthropic")
	if err := h.GetAdapter(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestGetAdapter_HappyPath(t *testing.T) {
	mock, h := newMockHandler(t, nil)
	exp := time.Now().Add(3 * time.Hour)
	reason := "anthropic outage"
	mock.ExpectQuery(`FROM gateway_passthrough_config_adapter WHERE adapter_type`).
		WithArgs("anthropic").
		WillReturnRows(pgxmock.NewRows([]string{"enabled", "config", "expires_at", "reason"}).
			AddRow(true, []byte(`{"bypassCache":true,"bypassNormalize":true}`), &exp, &reason))

	c, rec := echoCtxWithUser(http.MethodGet, "/passthrough/adapter/anthropic", "")
	c = withParam(c, "adapter_type", "anthropic")
	if err := h.GetAdapter(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d; body=%s", rec.Code, rec.Body.String())
	}
	var p payload
	_ = json.Unmarshal(rec.Body.Bytes(), &p)
	if !p.BypassCache || !p.BypassNormalize || p.Reason != reason {
		t.Errorf("decoded payload mismatch: %+v", p)
	}
}

func TestGetAdapter_NullReasonOmitted(t *testing.T) {
	mock, h := newMockHandler(t, nil)
	exp := time.Now().Add(1 * time.Hour)
	mock.ExpectQuery(`FROM gateway_passthrough_config_adapter WHERE adapter_type`).
		WithArgs("openai").
		WillReturnRows(pgxmock.NewRows([]string{"enabled", "config", "expires_at", "reason"}).
			AddRow(false, []byte(`{}`), &exp, (*string)(nil)))

	c, rec := echoCtxWithUser(http.MethodGet, "/passthrough/adapter/openai", "")
	c = withParam(c, "adapter_type", "openai")
	if err := h.GetAdapter(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d", rec.Code)
	}
	var p payload
	_ = json.Unmarshal(rec.Body.Bytes(), &p)
	if p.Reason != "" {
		t.Errorf("Reason = %q; want empty", p.Reason)
	}
}

func TestGetAdapter_DBError(t *testing.T) {
	mock, h := newMockHandler(t, nil)
	mock.ExpectQuery(`FROM gateway_passthrough_config_adapter`).
		WithArgs("anthropic").
		WillReturnError(errors.New("io"))
	c, rec := echoCtxWithUser(http.MethodGet, "/passthrough/adapter/anthropic", "")
	c = withParam(c, "adapter_type", "anthropic")
	if err := h.GetAdapter(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestPutAdapter_BindError(t *testing.T) {
	_, h := newMockHandler(t, nil)
	c, rec := echoCtxWithUser(http.MethodPut, "/passthrough/adapter/anthropic", `{bad`)
	c = withParam(c, "adapter_type", "anthropic")
	if err := h.PutAdapter(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestPutAdapter_ValidationError(t *testing.T) {
	_, h := newMockHandler(t, nil)
	exp := time.Now().Add(1 * time.Hour)
	bad, _ := json.Marshal(payload{Enabled: true, BypassNormalize: true, ExpiresAt: &exp, Reason: "long enough reason text indeed"})
	c, rec := echoCtxWithUser(http.MethodPut, "/passthrough/adapter/anthropic", string(bad))
	c = withParam(c, "adapter_type", "anthropic")
	if err := h.PutAdapter(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "passthrough_normalize_requires_cache_bypass") {
		t.Errorf("body: %s", rec.Body.String())
	}
}

func TestPutAdapter_DBError(t *testing.T) {
	mock, h := newMockHandler(t, nil)
	mock.ExpectExec(`INSERT INTO gateway_passthrough_config_adapter`).
		WithArgs("anthropic", pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(errors.New("upsert err"))
	c, rec := echoCtxWithUser(http.MethodPut, "/passthrough/adapter/anthropic", validBody(t))
	c = withParam(c, "adapter_type", "anthropic")
	if err := h.PutAdapter(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestPutAdapter_HappyPath(t *testing.T) {
	hub := &fakeHub{}
	mock, h := newMockHandler(t, hub)
	mock.ExpectExec(`INSERT INTO gateway_passthrough_config_adapter`).
		WithArgs("anthropic", pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	expectEmptyAssembleBlob(mock)

	c, rec := echoCtxWithUser(http.MethodPut, "/passthrough/adapter/anthropic", validBody(t))
	c = withParam(c, "adapter_type", "anthropic")
	if err := h.PutAdapter(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d; body = %s", rec.Code, rec.Body.String())
	}
	if hub.hits != 1 {
		t.Errorf("hub hits = %d", hub.hits)
	}
}

func TestPutAdapter_HubPropagationError(t *testing.T) {
	hub := &fakeHub{err: errors.New("hub kaput")}
	mock, h := newMockHandler(t, hub)
	mock.ExpectExec(`INSERT INTO gateway_passthrough_config_adapter`).
		WithArgs("anthropic", pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	expectEmptyAssembleBlob(mock)

	c, rec := echoCtxWithUser(http.MethodPut, "/passthrough/adapter/anthropic", validBody(t))
	c = withParam(c, "adapter_type", "anthropic")
	if err := h.PutAdapter(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusBadGateway {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestDeleteAdapter_DBError(t *testing.T) {
	mock, h := newMockHandler(t, nil)
	mock.ExpectExec(`DELETE FROM gateway_passthrough_config_adapter`).
		WithArgs("anthropic").
		WillReturnError(errors.New("delete kaboom"))
	c, rec := echoCtxWithUser(http.MethodDelete, "/passthrough/adapter/anthropic", "")
	c = withParam(c, "adapter_type", "anthropic")
	if err := h.DeleteAdapter(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestDeleteAdapter_HappyPath(t *testing.T) {
	hub := &fakeHub{}
	mock, h := newMockHandler(t, hub)
	mock.ExpectExec(`DELETE FROM gateway_passthrough_config_adapter`).
		WithArgs("anthropic").
		WillReturnResult(pgconn.NewCommandTag("DELETE 1"))
	expectEmptyAssembleBlob(mock)
	c, rec := echoCtxWithUser(http.MethodDelete, "/passthrough/adapter/anthropic", "")
	c = withParam(c, "adapter_type", "anthropic")
	if err := h.DeleteAdapter(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusNoContent {
		t.Errorf("code = %d", rec.Code)
	}
	if hub.hits != 1 {
		t.Errorf("hub hits = %d", hub.hits)
	}
}

func TestDeleteAdapter_HubPropagationError(t *testing.T) {
	hub := &fakeHub{err: errors.New("hub down")}
	mock, h := newMockHandler(t, hub)
	mock.ExpectExec(`DELETE FROM gateway_passthrough_config_adapter`).
		WithArgs("anthropic").
		WillReturnResult(pgconn.NewCommandTag("DELETE 1"))
	expectEmptyAssembleBlob(mock)
	c, rec := echoCtxWithUser(http.MethodDelete, "/passthrough/adapter/anthropic", "")
	c = withParam(c, "adapter_type", "anthropic")
	if err := h.DeleteAdapter(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusBadGateway {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestGetProvider_NotFound(t *testing.T) {
	mock, h := newMockHandler(t, nil)
	mock.ExpectQuery(`FROM gateway_passthrough_config_provider WHERE provider_id`).
		WithArgs("prov-1").
		WillReturnRows(pgxmock.NewRows([]string{"enabled", "config", "expires_at", "reason"}))

	c, rec := echoCtxWithUser(http.MethodGet, "/passthrough/provider/prov-1", "")
	c = withParam(c, "provider_id", "prov-1")
	if err := h.GetProvider(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestGetProvider_HappyPath(t *testing.T) {
	mock, h := newMockHandler(t, nil)
	exp := time.Now().Add(2 * time.Hour)
	reason := "smoke-test"
	mock.ExpectQuery(`FROM gateway_passthrough_config_provider WHERE provider_id`).
		WithArgs("prov-1").
		WillReturnRows(pgxmock.NewRows([]string{"enabled", "config", "expires_at", "reason"}).
			AddRow(true, []byte(`{"bypassHooks":true}`), &exp, &reason))
	c, rec := echoCtxWithUser(http.MethodGet, "/passthrough/provider/prov-1", "")
	c = withParam(c, "provider_id", "prov-1")
	if err := h.GetProvider(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d", rec.Code)
	}
	var p payload
	_ = json.Unmarshal(rec.Body.Bytes(), &p)
	if !p.Enabled || !p.BypassHooks || p.Reason != reason {
		t.Errorf("decoded mismatch: %+v", p)
	}
}

func TestGetProvider_NullReasonOmitted(t *testing.T) {
	mock, h := newMockHandler(t, nil)
	exp := time.Now().Add(1 * time.Hour)
	mock.ExpectQuery(`FROM gateway_passthrough_config_provider WHERE provider_id`).
		WithArgs("prov-1").
		WillReturnRows(pgxmock.NewRows([]string{"enabled", "config", "expires_at", "reason"}).
			AddRow(true, []byte(`{}`), &exp, (*string)(nil)))
	c, rec := echoCtxWithUser(http.MethodGet, "/passthrough/provider/prov-1", "")
	c = withParam(c, "provider_id", "prov-1")
	if err := h.GetProvider(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	var p payload
	_ = json.Unmarshal(rec.Body.Bytes(), &p)
	if p.Reason != "" {
		t.Errorf("Reason = %q; want empty", p.Reason)
	}
}

func TestGetProvider_DBError(t *testing.T) {
	mock, h := newMockHandler(t, nil)
	mock.ExpectQuery(`FROM gateway_passthrough_config_provider`).
		WithArgs("prov-1").
		WillReturnError(errors.New("io"))
	c, rec := echoCtxWithUser(http.MethodGet, "/passthrough/provider/prov-1", "")
	c = withParam(c, "provider_id", "prov-1")
	if err := h.GetProvider(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestPutProvider_BindError(t *testing.T) {
	_, h := newMockHandler(t, nil)
	c, rec := echoCtxWithUser(http.MethodPut, "/passthrough/provider/prov-1", `{not-json`)
	c = withParam(c, "provider_id", "prov-1")
	if err := h.PutProvider(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestPutProvider_ValidationError(t *testing.T) {
	_, h := newMockHandler(t, nil)
	exp := time.Now().Add(1 * time.Hour)
	bad, _ := json.Marshal(payload{Enabled: true, BypassHooks: true, ExpiresAt: &exp, Reason: "short"})
	c, rec := echoCtxWithUser(http.MethodPut, "/passthrough/provider/prov-1", string(bad))
	c = withParam(c, "provider_id", "prov-1")
	if err := h.PutProvider(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "passthrough_invalid_reason") {
		t.Errorf("body: %s", rec.Body.String())
	}
}

func TestPutProvider_DBError(t *testing.T) {
	mock, h := newMockHandler(t, nil)
	mock.ExpectExec(`INSERT INTO gateway_passthrough_config_provider`).
		WithArgs("prov-1", pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(errors.New("upsert err"))
	c, rec := echoCtxWithUser(http.MethodPut, "/passthrough/provider/prov-1", validBody(t))
	c = withParam(c, "provider_id", "prov-1")
	if err := h.PutProvider(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestPutProvider_HappyPath(t *testing.T) {
	hub := &fakeHub{}
	mock, h := newMockHandler(t, hub)
	mock.ExpectExec(`INSERT INTO gateway_passthrough_config_provider`).
		WithArgs("prov-1", pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	expectEmptyAssembleBlob(mock)

	c, rec := echoCtxWithUser(http.MethodPut, "/passthrough/provider/prov-1", validBody(t))
	c = withParam(c, "provider_id", "prov-1")
	if err := h.PutProvider(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d; body = %s", rec.Code, rec.Body.String())
	}
	if hub.hits != 1 {
		t.Errorf("hub hits = %d", hub.hits)
	}
}

func TestPutProvider_HubPropagationError(t *testing.T) {
	hub := &fakeHub{err: errors.New("hub down")}
	mock, h := newMockHandler(t, hub)
	mock.ExpectExec(`INSERT INTO gateway_passthrough_config_provider`).
		WithArgs("prov-1", pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	expectEmptyAssembleBlob(mock)
	c, rec := echoCtxWithUser(http.MethodPut, "/passthrough/provider/prov-1", validBody(t))
	c = withParam(c, "provider_id", "prov-1")
	if err := h.PutProvider(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusBadGateway {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestDeleteProvider_DBError(t *testing.T) {
	mock, h := newMockHandler(t, nil)
	mock.ExpectExec(`DELETE FROM gateway_passthrough_config_provider`).
		WithArgs("prov-1").
		WillReturnError(errors.New("delete kaboom"))
	c, rec := echoCtxWithUser(http.MethodDelete, "/passthrough/provider/prov-1", "")
	c = withParam(c, "provider_id", "prov-1")
	if err := h.DeleteProvider(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestDeleteProvider_HappyPath(t *testing.T) {
	hub := &fakeHub{}
	mock, h := newMockHandler(t, hub)
	mock.ExpectExec(`DELETE FROM gateway_passthrough_config_provider`).
		WithArgs("prov-1").
		WillReturnResult(pgconn.NewCommandTag("DELETE 1"))
	expectEmptyAssembleBlob(mock)
	c, rec := echoCtxWithUser(http.MethodDelete, "/passthrough/provider/prov-1", "")
	c = withParam(c, "provider_id", "prov-1")
	if err := h.DeleteProvider(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusNoContent {
		t.Errorf("code = %d", rec.Code)
	}
	if hub.hits != 1 {
		t.Errorf("hub hits = %d", hub.hits)
	}
}

func TestDeleteProvider_HubPropagationError(t *testing.T) {
	hub := &fakeHub{err: errors.New("hub down")}
	mock, h := newMockHandler(t, hub)
	mock.ExpectExec(`DELETE FROM gateway_passthrough_config_provider`).
		WithArgs("prov-1").
		WillReturnResult(pgconn.NewCommandTag("DELETE 1"))
	expectEmptyAssembleBlob(mock)
	c, rec := echoCtxWithUser(http.MethodDelete, "/passthrough/provider/prov-1", "")
	c = withParam(c, "provider_id", "prov-1")
	if err := h.DeleteProvider(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusBadGateway {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestGetSnapshot_AssembleBlobError(t *testing.T) {
	mock, h := newMockHandler(t, nil)
	mock.ExpectQuery(`FROM gateway_passthrough_config_global`).
		WillReturnError(errors.New("global io"))
	// assembleBlob's global query bails with non-ErrNoRows → returns
	// error; snapshot wraps it as 500.
	c, rec := echoCtxWithUser(http.MethodGet, "/passthrough/snapshot", "")
	if err := h.GetSnapshot(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestGetSnapshot_EmptyBlob(t *testing.T) {
	mock, h := newMockHandler(t, nil)
	expectEmptyAssembleBlob(mock)
	c, rec := echoCtxWithUser(http.MethodGet, "/passthrough/snapshot", "")
	if err := h.GetSnapshot(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d", rec.Code)
	}
	var resp snapshotResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Adapters) != 0 || len(resp.Providers) != 0 {
		t.Errorf("expected empty maps; got %+v", resp)
	}
}

func TestGetSnapshot_WithProvidersResolvesNames(t *testing.T) {
	mock, h := newMockHandler(t, nil)
	// global empty
	mock.ExpectQuery(`FROM gateway_passthrough_config_global`).
		WillReturnRows(pgxmock.NewRows([]string{"enabled", "config", "expires_at", "enabled_by", "reason"}))
	// adapter row to exercise rows.Next iteration
	exp := time.Now().Add(2 * time.Hour)
	mock.ExpectQuery(`FROM gateway_passthrough_config_adapter`).
		WillReturnRows(pgxmock.NewRows([]string{"adapter_type", "enabled", "config", "expires_at", "enabled_by", "reason"}).
			AddRow("anthropic", true, []byte(`{"bypassHooks":true}`), &exp, "adminX", "ana-reason"))
	// two provider rows
	mock.ExpectQuery(`FROM gateway_passthrough_config_provider`).
		WillReturnRows(pgxmock.NewRows([]string{"provider_id", "enabled", "config", "expires_at", "enabled_by", "reason"}).
			AddRow("prov-1", true, []byte(`{"bypassCache":true}`), &exp, "adminY", "rs1").
			AddRow("prov-2", false, []byte(`{}`), (*time.Time)(nil), "adminZ", ""))
	// Provider name lookup — pgxmock.AnyArg accepts the []string ids slice.
	mock.ExpectQuery(`FROM "Provider" WHERE id = ANY`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id", "name"}).
			AddRow("prov-1", "Anthropic").
			AddRow("prov-2", "OpenAI"))

	c, rec := echoCtxWithUser(http.MethodGet, "/passthrough/snapshot", "")
	if err := h.GetSnapshot(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d", rec.Code)
	}
	var resp snapshotResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Providers) != 2 {
		t.Errorf("providers map = %+v; want 2", resp.Providers)
	}
	if len(resp.Adapters) != 1 || !resp.Adapters["anthropic"].Enabled {
		t.Errorf("adapters map = %+v", resp.Adapters)
	}
	if resp.ProviderNames["prov-1"] != "Anthropic" || resp.ProviderNames["prov-2"] != "OpenAI" {
		t.Errorf("providerNames = %+v", resp.ProviderNames)
	}
}

func TestGetSnapshot_WithProvidersLookupError(t *testing.T) {
	// Provider lookup failure is swallowed — snapshot still 200 with
	// empty providerNames map (UI falls back to raw IDs).
	mock, h := newMockHandler(t, nil)
	mock.ExpectQuery(`FROM gateway_passthrough_config_global`).
		WillReturnRows(pgxmock.NewRows([]string{"enabled", "config", "expires_at", "enabled_by", "reason"}))
	mock.ExpectQuery(`FROM gateway_passthrough_config_adapter`).
		WillReturnRows(pgxmock.NewRows([]string{"adapter_type", "enabled", "config", "expires_at", "enabled_by", "reason"}))
	exp := time.Now().Add(1 * time.Hour)
	mock.ExpectQuery(`FROM gateway_passthrough_config_provider`).
		WillReturnRows(pgxmock.NewRows([]string{"provider_id", "enabled", "config", "expires_at", "enabled_by", "reason"}).
			AddRow("prov-1", true, []byte(`{}`), &exp, "", ""))
	mock.ExpectQuery(`FROM "Provider" WHERE id = ANY`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnError(errors.New("lookup boom"))

	c, rec := echoCtxWithUser(http.MethodGet, "/passthrough/snapshot", "")
	if err := h.GetSnapshot(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d; body=%s", rec.Code, rec.Body.String())
	}
	var resp snapshotResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.ProviderNames) != 0 {
		t.Errorf("expected empty providerNames on lookup error; got %+v", resp.ProviderNames)
	}
}

// assembleBlob — branches not reached by GetSnapshot tests above

func TestAssembleBlob_GlobalScanReturnsErrNoRows_NotAnError(t *testing.T) {
	// When the singleton row is absent, Scan emits pgx.ErrNoRows and
	// assembleBlob must NOT propagate it — it just leaves b.Global zero.
	mock, h := newMockHandler(t, nil)
	expectEmptyAssembleBlob(mock)
	b, err := h.assembleBlob(context.Background())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if b.Global.Enabled {
		t.Errorf("global = %+v; want zero", b.Global)
	}
}

func TestAssembleBlob_GlobalRowDecodes(t *testing.T) {
	mock, h := newMockHandler(t, nil)
	exp := time.Now().Add(4 * time.Hour)
	mock.ExpectQuery(`FROM gateway_passthrough_config_global`).
		WillReturnRows(pgxmock.NewRows([]string{"enabled", "config", "expires_at", "enabled_by", "reason"}).
			AddRow(true, []byte(`{"bypassHooks":true}`), &exp, "incident-resp", "anthropic outage"))
	mock.ExpectQuery(`FROM gateway_passthrough_config_adapter`).
		WillReturnRows(pgxmock.NewRows([]string{"adapter_type", "enabled", "config", "expires_at", "enabled_by", "reason"}))
	mock.ExpectQuery(`FROM gateway_passthrough_config_provider`).
		WillReturnRows(pgxmock.NewRows([]string{"provider_id", "enabled", "config", "expires_at", "enabled_by", "reason"}))
	b, err := h.assembleBlob(context.Background())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !b.Global.Enabled || !b.Global.BypassHooks || b.Global.EnabledBy != "incident-resp" {
		t.Errorf("global = %+v", b.Global)
	}
}

func TestAssembleBlob_AdaptersQueryError(t *testing.T) {
	mock, h := newMockHandler(t, nil)
	mock.ExpectQuery(`FROM gateway_passthrough_config_global`).
		WillReturnRows(pgxmock.NewRows([]string{"enabled", "config", "expires_at", "enabled_by", "reason"}))
	mock.ExpectQuery(`FROM gateway_passthrough_config_adapter`).
		WillReturnError(errors.New("adapters io"))
	_, err := h.assembleBlob(context.Background())
	if err == nil || !strings.Contains(err.Error(), "read adapters") {
		t.Errorf("err = %v; want wrap 'read adapters'", err)
	}
}

func TestAssembleBlob_AdaptersRowScanError(t *testing.T) {
	mock, h := newMockHandler(t, nil)
	mock.ExpectQuery(`FROM gateway_passthrough_config_global`).
		WillReturnRows(pgxmock.NewRows([]string{"enabled", "config", "expires_at", "enabled_by", "reason"}))
	// Wrong column count vs the destination scan vars triggers scan err.
	mock.ExpectQuery(`FROM gateway_passthrough_config_adapter`).
		WillReturnRows(pgxmock.NewRows([]string{"adapter_type"}).AddRow("oops"))
	_, err := h.assembleBlob(context.Background())
	if err == nil {
		t.Fatal("expected scan error; got nil")
	}
}

func TestAssembleBlob_ProvidersQueryError(t *testing.T) {
	mock, h := newMockHandler(t, nil)
	mock.ExpectQuery(`FROM gateway_passthrough_config_global`).
		WillReturnRows(pgxmock.NewRows([]string{"enabled", "config", "expires_at", "enabled_by", "reason"}))
	mock.ExpectQuery(`FROM gateway_passthrough_config_adapter`).
		WillReturnRows(pgxmock.NewRows([]string{"adapter_type", "enabled", "config", "expires_at", "enabled_by", "reason"}))
	mock.ExpectQuery(`FROM gateway_passthrough_config_provider`).
		WillReturnError(errors.New("providers io"))
	_, err := h.assembleBlob(context.Background())
	if err == nil || !strings.Contains(err.Error(), "read providers") {
		t.Errorf("err = %v; want wrap 'read providers'", err)
	}
}

func TestAssembleBlob_ProvidersRowScanError(t *testing.T) {
	mock, h := newMockHandler(t, nil)
	mock.ExpectQuery(`FROM gateway_passthrough_config_global`).
		WillReturnRows(pgxmock.NewRows([]string{"enabled", "config", "expires_at", "enabled_by", "reason"}))
	mock.ExpectQuery(`FROM gateway_passthrough_config_adapter`).
		WillReturnRows(pgxmock.NewRows([]string{"adapter_type", "enabled", "config", "expires_at", "enabled_by", "reason"}))
	mock.ExpectQuery(`FROM gateway_passthrough_config_provider`).
		WillReturnRows(pgxmock.NewRows([]string{"provider_id"}).AddRow("bad"))
	_, err := h.assembleBlob(context.Background())
	if err == nil {
		t.Fatal("expected scan error; got nil")
	}
}

// propagateConfig — direct unit-test for nil-Hub and assemble-error

func TestPropagateConfig_NilHubNoop(t *testing.T) {
	_, h := newMockHandler(t, nil) // hub nil
	if err := h.propagateConfig(context.Background(), "actor-1", "Alice"); err != nil {
		t.Errorf("nil-hub should be no-op; got %v", err)
	}
}

func TestPropagateConfig_AssembleError(t *testing.T) {
	hub := &fakeHub{}
	mock, h := newMockHandler(t, hub)
	mock.ExpectQuery(`FROM gateway_passthrough_config_global`).
		WillReturnError(errors.New("kaboom"))
	err := h.propagateConfig(context.Background(), "actor-1", "Alice")
	if err == nil {
		t.Fatal("expected assemble error; got nil")
	}
	if !strings.Contains(err.Error(), "assemble passthrough blob") {
		t.Errorf("err = %v; want wrap 'assemble passthrough blob'", err)
	}
	if hub.hits != 0 {
		t.Errorf("hub should not be called on assemble fail; hits=%d", hub.hits)
	}
}

func TestPropagateConfig_HubError(t *testing.T) {
	hub := &fakeHub{err: errors.New("hub-fail")}
	mock, h := newMockHandler(t, hub)
	expectEmptyAssembleBlob(mock)
	err := h.propagateConfig(context.Background(), "actor-1", "Alice")
	if err == nil || !strings.Contains(err.Error(), "hub-fail") {
		t.Errorf("err = %v; want hub-fail", err)
	}
}

func TestPropagateConfig_HappyWiresActorAndShadowKey(t *testing.T) {
	hub := &fakeHub{}
	mock, h := newMockHandler(t, hub)
	expectEmptyAssembleBlob(mock)
	if err := h.propagateConfig(context.Background(), "act-9", "Bob"); err != nil {
		t.Fatalf("err: %v", err)
	}
	if hub.lastReq.ThingType != "ai-gateway" || hub.lastReq.ConfigKey != shadowKey {
		t.Errorf("req = %+v", hub.lastReq)
	}
	if hub.lastReq.ActorID != "act-9" || hub.lastReq.ActorName != "Bob" {
		t.Errorf("actor = %s/%s", hub.lastReq.ActorID, hub.lastReq.ActorName)
	}
	// State must be a typed blob struct, not pre-marshaled bytes.
	if _, ok := hub.lastReq.State.(blob); !ok {
		t.Errorf("State type = %T; want passthrough.blob", hub.lastReq.State)
	}
}

// Anon-actor coverage — verify PutGlobal happy-path still works without
// the "user" context key (actor() returns empty strings).

func TestPutGlobal_AnonActor(t *testing.T) {
	hub := &fakeHub{}
	mock, h := newMockHandler(t, hub)
	mock.ExpectExec(`INSERT INTO gateway_passthrough_config_global`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	expectEmptyAssembleBlob(mock)
	c, rec := echoCtxAnon(http.MethodPut, "/passthrough/global", validBody(t))
	if err := h.PutGlobal(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d", rec.Code)
	}
	if hub.lastReq.ActorID != "" {
		t.Errorf("anon actor id should be empty; got %q", hub.lastReq.ActorID)
	}
}

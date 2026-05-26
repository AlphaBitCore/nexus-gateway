package hooks

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

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/labstack/echo/v4"
	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/governance/hooks/hookstore"
	auth "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authn"
	cpiam "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/iam"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/store/systemmetastore"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
)

// Test helpers

// hubSpy records every InvalidateConfig call. The hooks domain MUST invalidate
// all three Thing types — ai-gateway, compliance-proxy, agent — on every CUD
// path. Drift here would silently leave dataplane caches stale.
type hubSpy struct {
	mu    sync.Mutex
	calls []string // "thingType/configKey"
}

func (h *hubSpy) InvalidateConfig(_ context.Context, thingType, configKey string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, thingType+"/"+configKey)
}

func (h *hubSpy) seen() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, len(h.calls))
	copy(out, h.calls)
	return out
}

// auditSpy captures MQ enqueues for assertion.
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

// newMockStore returns a pgxmock pool and a systemmetastore.Store backed by it,
// ready for hook CRUD + system_metadata queries. Both hookstore and
// systemmetastore share the same mock pool so expectations interleave naturally.
func newMockStore(t *testing.T) (pgxmock.PgxPoolIface, *systemmetastore.Store) {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mock.Close)
	return mock, systemmetastore.NewFromPool(mock)
}

// hookConfigCols mirrors store/hook_config_test.go.
var hookConfigCols = []string{
	"id", "name", "type", "implementationId", "stage", "category", "endpoint", "script",
	"config", "priority", "timeoutMs", "failBehavior", "enabled", "applicableIngress",
	"createdAt", "updatedAt",
}

func makeHookConfigRow(now time.Time) []any {
	category := "compliance"
	endpoint := "http://localhost:9000/pii"
	return []any{
		"hc-1", "pii-redact", "webhook", "pii-detector", "request", &category,
		&endpoint, (*string)(nil), json.RawMessage(`{"k":"v"}`), 10, 5000,
		"fail-open", true, []string{"openai", "anthropic"}, now, now,
	}
}

// newHandler returns a Handler ready for tests with the given hub/audit/pool+meta.
// pool is used for hookstore queries; meta is used for system_metadata operations.
// Both are backed by the same pgxmock so expectations interleave naturally.
func newHandler(pool hookstore.PgxPool, meta *systemmetastore.Store, hub HubInvalidator, aw *audit.Writer) *Handler {
	return New(Deps{
		Pool:   pool,
		Meta:   meta,
		Hub:    hub,
		Audit:  aw,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

// echoCtx builds an Echo context with an authenticated admin so audit + IAM
// metadata extraction work end-to-end.
func echoCtx(req *http.Request, rec *httptest.ResponseRecorder, userID string) (echo.Context, *echo.Echo) {
	e := echo.New()
	c := e.NewContext(req, rec)
	middleware.WithAdminAuth(c, &auth.AdminAuth{
		KeyID:             userID,
		KeyName:           "admin-" + userID,
		AuthPrincipalType: "admin_user",
	})
	return c, e
}

// pagination helpers

func TestParsePagination(t *testing.T) {
	tests := []struct {
		name       string
		query      string
		wantLimit  int
		wantOffset int
	}{
		{"defaults", "", 50, 0},
		{"happy custom", "limit=10&offset=20", 10, 20},
		{"zero limit ignored → default", "limit=0", 50, 0},
		{"negative offset ignored → default", "offset=-3", 50, 0},
		{"non-int values ignored", "limit=abc&offset=xyz", 50, 0},
		{"limit clamp at 1000", "limit=5000", 1000, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := echo.New()
			req := httptest.NewRequest(http.MethodGet, "/?"+tc.query, nil)
			rec := httptest.NewRecorder()
			c := e.NewContext(req, rec)
			pg := parsePagination(c)
			if pg.Limit != tc.wantLimit || pg.Offset != tc.wantOffset {
				t.Errorf("limit=%d offset=%d; want %d/%d", pg.Limit, pg.Offset, tc.wantLimit, tc.wantOffset)
			}
		})
	}
}

func TestErrJSON_Shape(t *testing.T) {
	got := errJSON("oops", "validation_error", "field-x")
	env, ok := got["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected error envelope, got: %v", got)
	}
	if env["message"] != "oops" || env["type"] != "validation_error" || env["code"] != "field-x" {
		t.Errorf("bad envelope: %+v", env)
	}
}

func TestInternalServerError_StatusAndBody(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if err := internalServerError(c, "boom"); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"boom"`) || !strings.Contains(rec.Body.String(), `"server_error"`) {
		t.Errorf("body = %s", rec.Body.String())
	}
}

func TestDeref(t *testing.T) {
	if deref(nil) != "" {
		t.Errorf("nil should be empty")
	}
	s := "hi"
	if deref(&s) != "hi" {
		t.Errorf("ptr should deref")
	}
}

func TestInvalidateHookConfigEverywhere_NilHubTolerated(t *testing.T) {
	h := newHandler(nil, nil, nil, audit.NewWriter(nil, "", slog.Default()))
	// Must not panic with hub==nil.
	h.invalidateHookConfigEverywhere(context.Background())
}

func TestInvalidateHookConfigEverywhere_AllThreeThings(t *testing.T) {
	spy := &hubSpy{}
	h := newHandler(nil, nil, spy, audit.NewWriter(nil, "", slog.Default()))
	h.invalidateHookConfigEverywhere(context.Background())
	calls := spy.seen()
	want := []string{"ai-gateway/hooks", "compliance-proxy/hooks", "agent/hooks"}
	if len(calls) != len(want) {
		t.Fatalf("calls=%v; want %v", calls, want)
	}
	for i, c := range calls {
		if c != want[i] {
			t.Errorf("call[%d] = %s; want %s", i, c, want[i])
		}
	}
}

func TestIncrementConfigVersion_FreshKey_StartsAtOne(t *testing.T) {
	mock, meta := newMockStore(t)
	// GetSystemMetadata returns nil for fresh key (pgx.ErrNoRows treated as nil).
	mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key`).
		WithArgs("agent.config.version").
		WillReturnRows(pgxmock.NewRows([]string{"value"}))
	// SetSystemMetadata inserts version=1.
	mock.ExpectExec(`INSERT INTO system_metadata`).
		WithArgs("agent.config.version", []byte("1"), "system").
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	h := newHandler(mock, meta, nil, audit.NewWriter(nil, "", slog.Default()))
	h.incrementConfigVersion(context.Background())
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

func TestIncrementConfigVersion_ExistingValue_IncrementsByOne(t *testing.T) {
	mock, meta := newMockStore(t)
	mock.ExpectQuery(`SELECT value FROM system_metadata`).
		WithArgs("agent.config.version").
		WillReturnRows(pgxmock.NewRows([]string{"value"}).AddRow([]byte("7")))
	mock.ExpectExec(`INSERT INTO system_metadata`).
		WithArgs("agent.config.version", []byte("8"), "system").
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	h := newHandler(mock, meta, nil, audit.NewWriter(nil, "", slog.Default()))
	h.incrementConfigVersion(context.Background())
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

func TestIncrementConfigVersion_MalformedExistingValue_TreatedAsZero(t *testing.T) {
	mock, meta := newMockStore(t)
	mock.ExpectQuery(`SELECT value FROM system_metadata`).
		WithArgs("agent.config.version").
		WillReturnRows(pgxmock.NewRows([]string{"value"}).AddRow([]byte("not-a-number")))
	mock.ExpectExec(`INSERT INTO system_metadata`).
		WithArgs("agent.config.version", []byte("1"), "system").
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	h := newHandler(mock, meta, nil, audit.NewWriter(nil, "", slog.Default()))
	h.incrementConfigVersion(context.Background())
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

func TestIncrementConfigVersion_SetError_LoggedNotPropagated(t *testing.T) {
	mock, meta := newMockStore(t)
	mock.ExpectQuery(`SELECT value FROM system_metadata`).
		WithArgs("agent.config.version").
		WillReturnRows(pgxmock.NewRows([]string{"value"}))
	mock.ExpectExec(`INSERT INTO system_metadata`).
		WithArgs("agent.config.version", []byte("1"), "system").
		WillReturnError(errors.New("disk full"))

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	h := New(Deps{Pool: mock, Meta: meta, Audit: audit.NewWriter(nil, "", slog.Default()), Logger: logger})
	// Should not panic / propagate the error.
	h.incrementConfigVersion(context.Background())
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
	if !strings.Contains(buf.String(), "increment agent config version") {
		t.Errorf("expected error log; got: %s", buf.String())
	}
}

// classifyHook + enrichHook

func TestClassifyHook_KnownImpl_RegistryDriven(t *testing.T) {
	hc := &hookstore.HookConfig{
		ID:               "h1",
		ImplementationID: "pii-detector",
		Stage:            "request",
	}
	cls := classifyHook(hc)
	if cls.Category != "compliance" || cls.CategoryLabel == "" {
		t.Errorf("category: %+v", cls)
	}
	if cls.CategorySource != "registry" {
		t.Errorf("source = %q; want registry", cls.CategorySource)
	}
	if cls.Phase != "request" || cls.PhaseLabel != "Request" {
		t.Errorf("phase: %q/%q", cls.Phase, cls.PhaseLabel)
	}
	if !cls.DualPhaseCapable {
		t.Errorf("pii-detector should be dual-phase")
	}
	if cls.ImplementationID == nil || *cls.ImplementationID != "pii-detector" {
		t.Errorf("implID: %+v", cls.ImplementationID)
	}
	if cls.ImplementationLabel == nil {
		t.Errorf("implLabel should be set for known impl")
	}
}

func TestClassifyHook_DBOverride_TakesPrecedence(t *testing.T) {
	cat := "quality"
	hc := &hookstore.HookConfig{
		ID:               "h1",
		ImplementationID: "pii-detector",
		Stage:            "response",
		Category:         &cat,
	}
	cls := classifyHook(hc)
	if cls.Category != "quality" || cls.CategorySource != "database" {
		t.Errorf("override didn't win: %+v", cls)
	}
	if cls.PhaseLabel != "Response" {
		t.Errorf("phase label: %s", cls.PhaseLabel)
	}
}

func TestClassifyHook_UnknownImpl_FallsBackToCustom(t *testing.T) {
	hc := &hookstore.HookConfig{ID: "h1", ImplementationID: "unknown-impl", Stage: "request"}
	cls := classifyHook(hc)
	if cls.Category != "custom" || cls.CategoryLabel != "Custom / other" {
		t.Errorf("unknown should fall back to custom: %+v", cls)
	}
	if cls.ImplementationID == nil || cls.ImplementationLabel != nil {
		t.Errorf("unknown impl: id set, label nil; got id=%v label=%v", cls.ImplementationID, cls.ImplementationLabel)
	}
}

func TestClassifyHook_EmptyImpl_NoImplFields(t *testing.T) {
	hc := &hookstore.HookConfig{ID: "h1", Stage: "request"}
	cls := classifyHook(hc)
	if cls.ImplementationID != nil || cls.ImplementationLabel != nil {
		t.Errorf("empty impl: both nil; got %v / %v", cls.ImplementationID, cls.ImplementationLabel)
	}
	// stage list should default to [stage]
	if len(cls.SupportedStages) != 1 || cls.SupportedStages[0] != "request" {
		t.Errorf("stages: %v", cls.SupportedStages)
	}
}

func TestClassifyHook_EmptyCategoryString_UsesRegistryNotEmptyString(t *testing.T) {
	empty := ""
	hc := &hookstore.HookConfig{
		ID:               "h1",
		ImplementationID: "pii-detector",
		Stage:            "request",
		Category:         &empty, // explicit empty must NOT override
	}
	cls := classifyHook(hc)
	if cls.Category != "compliance" || cls.CategorySource != "registry" {
		t.Errorf("empty-string category should fall through to registry; got: %+v", cls)
	}
}

func TestClassifyHook_UnknownDBCategory_DefaultsLabel(t *testing.T) {
	cat := "totally-made-up"
	hc := &hookstore.HookConfig{ID: "h1", ImplementationID: "noop", Stage: "request", Category: &cat}
	cls := classifyHook(hc)
	if cls.Category != "totally-made-up" || cls.CategoryLabel != "Custom / other" {
		t.Errorf("unknown category should keep override but default label: %+v", cls)
	}
}

func TestEnrichHooks_PreservesOrderAndContent(t *testing.T) {
	hcs := []hookstore.HookConfig{
		{ID: "a", ImplementationID: "pii-detector", Stage: "request"},
		{ID: "b", ImplementationID: "rate-limiter", Stage: "request"},
	}
	out := enrichHooks(hcs)
	if len(out) != 2 || out[0].ID != "a" || out[1].ID != "b" {
		t.Errorf("order broke: %+v", out)
	}
	if out[0].Classification.Category != "compliance" || out[1].Classification.Category != "traffic_control" {
		t.Errorf("classification: %+v / %+v", out[0].Classification, out[1].Classification)
	}
}

func TestEnrichHook_NonRequestStage_PhaseLabelPassthrough(t *testing.T) {
	hc := &hookstore.HookConfig{ID: "h1", ImplementationID: "noop", Stage: "custom-stage"}
	out := enrichHook(hc)
	if out.Classification.PhaseLabel != "custom-stage" {
		t.Errorf("non-canonical stage should passthrough as PhaseLabel; got %q", out.Classification.PhaseLabel)
	}
}

func TestValidateHookEnums(t *testing.T) {
	tests := []struct {
		name   string
		stage  string
		fb     string
		typ    string
		impl   string
		wantOK bool
	}{
		{"all empty", "", "", "", "", true},
		{"good values", "request", "fail-open", "webhook", "pii-detector", true},
		{"bad stage", "weird", "", "", "", false},
		{"bad fail behavior", "", "weird", "", "", false},
		{"bad type", "", "", "weird", "", false},
		{"unknown impl", "", "", "", "nonexistent", false},
		{"good response stage + fail-closed", "response", "fail-closed", "script", "noop", true},
		{"good builtin", "", "", "builtin", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ValidateHookEnums(tc.stage, tc.fb, tc.typ, tc.impl)
			ok := got == ""
			if ok != tc.wantOK {
				t.Errorf("got=%q wantOK=%v", got, tc.wantOK)
			}
		})
	}
}

func TestRegisterRoutes_WiresAllSeven(t *testing.T) {
	h := newHandler(nil, nil, nil, audit.NewWriter(nil, "", slog.Default()))
	e := echo.New()
	g := e.Group("/api/admin")
	iamMW := func(_ string) echo.MiddlewareFunc {
		return func(next echo.HandlerFunc) echo.HandlerFunc { return next }
	}
	h.RegisterRoutes(g, iamMW)

	wanted := []struct {
		method, path string
	}{
		{http.MethodGet, "/api/admin/hooks"},
		{http.MethodPost, "/api/admin/hooks"},
		{http.MethodPost, "/api/admin/hooks/reorder"},
		{http.MethodPost, "/api/admin/hooks/refresh"},
		{http.MethodGet, "/api/admin/hooks/:id"},
		{http.MethodPut, "/api/admin/hooks/:id"},
		{http.MethodDelete, "/api/admin/hooks/:id"},
	}
	routes := e.Routes()
	for _, w := range wanted {
		found := false
		for _, r := range routes {
			if r.Method == w.method && r.Path == w.path {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing route: %s %s", w.method, w.path)
		}
	}
}

func TestListHookConfigs_Happy(t *testing.T) {
	mock, meta := newMockStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM "HookConfig"`).
		WithArgs("%pii%", true).
		WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
	mock.ExpectQuery(`SELECT .* FROM "HookConfig"`).
		WithArgs("%pii%", true, 25, 10).
		WillReturnRows(pgxmock.NewRows(hookConfigCols).AddRow(makeHookConfigRow(now)...))

	h := newHandler(mock, meta, nil, audit.NewWriter(nil, "", slog.Default()))
	req := httptest.NewRequest(http.MethodGet, "/?q=pii&enabled=true&pipeline=request&limit=25&offset=10", nil)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u1")

	if err := h.ListHookConfigs(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data  []map[string]any `json:"data"`
		Total int              `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("parse: %v body=%s", err, rec.Body.String())
	}
	if body.Total != 1 || len(body.Data) != 1 {
		t.Errorf("body: %+v", body)
	}
	if body.Data[0]["name"] != "pii-redact" {
		t.Errorf("name = %v", body.Data[0]["name"])
	}
	// classification should be present
	if _, ok := body.Data[0]["classification"]; !ok {
		t.Errorf("expected classification key; got %+v", body.Data[0])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

func TestListHookConfigs_EnabledFalseFilter(t *testing.T) {
	mock, meta := newMockStore(t)
	mock.ExpectQuery(`SELECT COUNT\(\*\)`).
		WithArgs(false).
		WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(0))
	mock.ExpectQuery(`SELECT .* FROM "HookConfig"`).
		WithArgs(false, 50, 0).
		WillReturnRows(pgxmock.NewRows(hookConfigCols))

	h := newHandler(mock, meta, nil, audit.NewWriter(nil, "", slog.Default()))
	req := httptest.NewRequest(http.MethodGet, "/?enabled=false", nil)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u1")
	if err := h.ListHookConfigs(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

func TestListHookConfigs_NoFilters(t *testing.T) {
	mock, meta := newMockStore(t)
	mock.ExpectQuery(`SELECT COUNT\(\*\)`).
		WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(0))
	mock.ExpectQuery(`SELECT .* FROM "HookConfig"`).
		WithArgs(50, 0).
		WillReturnRows(pgxmock.NewRows(hookConfigCols))

	h := newHandler(mock, meta, nil, audit.NewWriter(nil, "", slog.Default()))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u1")
	if err := h.ListHookConfigs(c); err != nil {
		t.Fatalf("err: %v", err)
	}
}

func TestListHookConfigs_StoreError_Returns500(t *testing.T) {
	mock, meta := newMockStore(t)
	mock.ExpectQuery(`SELECT COUNT\(\*\)`).WillReturnError(errors.New("db down"))

	h := newHandler(mock, meta, nil, audit.NewWriter(nil, "", slog.Default()))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u1")
	if err := h.ListHookConfigs(c); err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
}

func TestGetHookConfig_Happy(t *testing.T) {
	mock, meta := newMockStore(t)
	now := time.Now().UTC()
	mock.ExpectQuery(`SELECT .* FROM "HookConfig" WHERE id`).
		WithArgs("hc-1").
		WillReturnRows(pgxmock.NewRows(hookConfigCols).AddRow(makeHookConfigRow(now)...))

	h := newHandler(mock, meta, nil, audit.NewWriter(nil, "", slog.Default()))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u1")
	c.SetParamNames("id")
	c.SetParamValues("hc-1")
	if err := h.GetHookConfig(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["id"] != "hc-1" || body["name"] != "pii-redact" {
		t.Errorf("body: %+v", body)
	}
}

func TestGetHookConfig_NotFound_Returns404(t *testing.T) {
	mock, meta := newMockStore(t)
	mock.ExpectQuery(`SELECT .* FROM "HookConfig" WHERE id`).
		WithArgs("missing").
		WillReturnRows(pgxmock.NewRows(hookConfigCols))

	h := newHandler(mock, meta, nil, audit.NewWriter(nil, "", slog.Default()))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u1")
	c.SetParamNames("id")
	c.SetParamValues("missing")
	if err := h.GetHookConfig(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d; want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestGetHookConfig_StoreError_Returns500(t *testing.T) {
	mock, meta := newMockStore(t)
	mock.ExpectQuery(`SELECT .* FROM "HookConfig"`).
		WithArgs("x").
		WillReturnError(&pgconn.PgError{Message: "boom"})

	h := newHandler(mock, meta, nil, audit.NewWriter(nil, "", slog.Default()))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u1")
	c.SetParamNames("id")
	c.SetParamValues("x")
	_ = h.GetHookConfig(c)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
}

func TestCreateHookConfig_BadJSON_Returns400(t *testing.T) {
	h := newHandler(nil, nil, nil, audit.NewWriter(nil, "", slog.Default()))
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("not-json"))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u1")
	_ = h.CreateHookConfig(c)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rec.Code)
	}
}

func TestCreateHookConfig_MissingNameOrType_Returns400(t *testing.T) {
	h := newHandler(nil, nil, nil, audit.NewWriter(nil, "", slog.Default()))
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u1")
	_ = h.CreateHookConfig(c)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "name and type are required") {
		t.Errorf("body: %s", rec.Body.String())
	}
}

func TestCreateHookConfig_BadEnum_Returns400(t *testing.T) {
	h := newHandler(nil, nil, nil, audit.NewWriter(nil, "", slog.Default()))
	body := `{"name":"x","type":"webhook","stage":"banana"}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u1")
	_ = h.CreateHookConfig(c)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "stage must be") {
		t.Errorf("body: %s", rec.Body.String())
	}
}

func TestCreateHookConfig_EmptyApplicableIngress_Returns400(t *testing.T) {
	h := newHandler(nil, nil, nil, audit.NewWriter(nil, "", slog.Default()))
	body := `{"name":"x","type":"webhook","implementationId":"noop","applicableIngress":[]}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u1")
	_ = h.CreateHookConfig(c)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "applicableIngress must not be empty") {
		t.Errorf("body: %s", rec.Body.String())
	}
}

func TestCreateHookConfig_Happy_DefaultsApplied(t *testing.T) {
	mock, meta := newMockStore(t)
	now := time.Now().UTC()
	// CreateHookConfig INSERT — defaults: implID=noop, stage=request,
	// timeoutMs=5000, failBehavior=fail-open, enabled=true.
	mock.ExpectQuery(`INSERT INTO "HookConfig"`).
		WithArgs(
			"x", "webhook", "noop", "request",
			(*string)(nil), (*string)(nil), (*string)(nil),
			json.RawMessage(nil), 0, 5000, "fail-open", true,
			[]string(nil),
		).
		WillReturnRows(pgxmock.NewRows(hookConfigCols).AddRow(makeHookConfigRow(now)...))
	// invalidateHookConfigEverywhere - hub call counted via spy.
	// incrementConfigVersion does select + insert into system_metadata.
	mock.ExpectQuery(`SELECT value FROM system_metadata`).
		WithArgs("agent.config.version").
		WillReturnRows(pgxmock.NewRows([]string{"value"}))
	mock.ExpectExec(`INSERT INTO system_metadata`).
		WithArgs("agent.config.version", []byte("1"), "system").
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	hubS := &hubSpy{}
	aspy := &auditSpy{}
	h := newHandler(mock, meta, hubS, audit.NewWriter(aspy, "audit", slog.Default()))
	body := `{"name":"x","type":"webhook"}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "user-42")

	if err := h.CreateHookConfig(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := hubS.seen(); len(got) != 3 {
		t.Errorf("hub invalidations = %d; want 3 (%v)", len(got), got)
	}
	// Wait briefly for audit fire-and-forget enqueue path.
	deadline := time.Now().Add(500 * time.Millisecond)
	for aspy.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if aspy.count() != 1 {
		t.Errorf("audit count = %d; want 1", aspy.count())
	}
	entry := aspy.last()
	if entry["action"] != "create" || entry["entityType"] != "hook" || entry["entityId"] != "hc-1" {
		t.Errorf("audit entry: %+v", entry)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

func TestCreateHookConfig_ExplicitEnabledFalse_PropagatesAndStoreError500(t *testing.T) {
	mock, meta := newMockStore(t)
	mock.ExpectQuery(`INSERT INTO "HookConfig"`).
		WithArgs(
			"x", "webhook", "noop", "request",
			(*string)(nil), (*string)(nil), (*string)(nil),
			json.RawMessage(nil), 0, 5000, "fail-open", false,
			[]string(nil),
		).
		WillReturnError(errors.New("db boom"))

	hubS := &hubSpy{}
	h := newHandler(mock, meta, hubS, audit.NewWriter(nil, "", slog.Default()))
	body := `{"name":"x","type":"webhook","enabled":false}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u1")
	_ = h.CreateHookConfig(c)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
	if len(hubS.seen()) != 0 {
		t.Errorf("hub should NOT be called on store error; got %v", hubS.seen())
	}
}

func TestUpdateHookConfig_NotFound_Returns404(t *testing.T) {
	mock, meta := newMockStore(t)
	mock.ExpectQuery(`SELECT .* FROM "HookConfig" WHERE id`).
		WithArgs("missing").
		WillReturnRows(pgxmock.NewRows(hookConfigCols))

	h := newHandler(mock, meta, nil, audit.NewWriter(nil, "", slog.Default()))
	req := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(`{}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u1")
	c.SetParamNames("id")
	c.SetParamValues("missing")
	_ = h.UpdateHookConfig(c)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d", rec.Code)
	}
}

func TestUpdateHookConfig_GetError_Returns500(t *testing.T) {
	mock, meta := newMockStore(t)
	mock.ExpectQuery(`SELECT .* FROM "HookConfig" WHERE id`).
		WithArgs("x").
		WillReturnError(errors.New("boom"))

	h := newHandler(mock, meta, nil, audit.NewWriter(nil, "", slog.Default()))
	req := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(`{}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u1")
	c.SetParamNames("id")
	c.SetParamValues("x")
	_ = h.UpdateHookConfig(c)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d", rec.Code)
	}
}

func TestUpdateHookConfig_BadJSON_Returns400(t *testing.T) {
	mock, meta := newMockStore(t)
	now := time.Now().UTC()
	mock.ExpectQuery(`SELECT .* FROM "HookConfig" WHERE id`).
		WithArgs("hc-1").
		WillReturnRows(pgxmock.NewRows(hookConfigCols).AddRow(makeHookConfigRow(now)...))

	h := newHandler(mock, meta, nil, audit.NewWriter(nil, "", slog.Default()))
	req := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(`not-json`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u1")
	c.SetParamNames("id")
	c.SetParamValues("hc-1")
	_ = h.UpdateHookConfig(c)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d", rec.Code)
	}
}

func TestUpdateHookConfig_BadEnum_Returns400(t *testing.T) {
	mock, meta := newMockStore(t)
	now := time.Now().UTC()
	mock.ExpectQuery(`SELECT .* FROM "HookConfig" WHERE id`).
		WithArgs("hc-1").
		WillReturnRows(pgxmock.NewRows(hookConfigCols).AddRow(makeHookConfigRow(now)...))

	h := newHandler(mock, meta, nil, audit.NewWriter(nil, "", slog.Default()))
	req := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(`{"stage":"banana"}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u1")
	c.SetParamNames("id")
	c.SetParamValues("hc-1")
	_ = h.UpdateHookConfig(c)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestUpdateHookConfig_EmptyApplicableIngress_Returns400(t *testing.T) {
	mock, meta := newMockStore(t)
	now := time.Now().UTC()
	mock.ExpectQuery(`SELECT .* FROM "HookConfig" WHERE id`).
		WithArgs("hc-1").
		WillReturnRows(pgxmock.NewRows(hookConfigCols).AddRow(makeHookConfigRow(now)...))

	h := newHandler(mock, meta, nil, audit.NewWriter(nil, "", slog.Default()))
	req := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(`{"applicableIngress":[]}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u1")
	c.SetParamNames("id")
	c.SetParamValues("hc-1")
	_ = h.UpdateHookConfig(c)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "applicableIngress must not be empty") {
		t.Errorf("body: %s", rec.Body.String())
	}
}

func TestUpdateHookConfig_NothingProvided_ReturnsExistingNoMutation(t *testing.T) {
	mock, meta := newMockStore(t)
	now := time.Now().UTC()
	mock.ExpectQuery(`SELECT .* FROM "HookConfig" WHERE id`).
		WithArgs("hc-1").
		WillReturnRows(pgxmock.NewRows(hookConfigCols).AddRow(makeHookConfigRow(now)...))

	hubS := &hubSpy{}
	aspy := &auditSpy{}
	h := newHandler(mock, meta, hubS, audit.NewWriter(aspy, "audit", slog.Default()))
	req := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(`{}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u1")
	c.SetParamNames("id")
	c.SetParamValues("hc-1")
	if err := h.UpdateHookConfig(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
	if len(hubS.seen()) != 0 {
		t.Errorf("no-mutation update must NOT invalidate hub; got %v", hubS.seen())
	}
	if aspy.count() != 0 {
		t.Errorf("no-mutation update must NOT audit; got %d", aspy.count())
	}
}

// TestUpdateHookConfig_ConfigField_HappyPath exercises the body.Config != nil
// branch via a JSON object that round-trips through json.Marshal — the only
// reachable shape post-Bind. The error arm of json.Marshal(body.Config) is
// structurally unreachable: echo.Bind decodes JSON into `any` which is
// always re-marshalable (nil/bool/float64/string/[]any/map[string]any).
func TestUpdateHookConfig_ConfigField_HappyPath(t *testing.T) {
	mock, meta := newMockStore(t)
	now := time.Now().UTC()
	mock.ExpectQuery(`SELECT .* FROM "HookConfig" WHERE id`).
		WithArgs("hc-1").
		WillReturnRows(pgxmock.NewRows(hookConfigCols).AddRow(makeHookConfigRow(now)...))

	h := newHandler(mock, meta, nil, audit.NewWriter(nil, "", slog.Default()))
	body := `{"config":{"k":"v"}}`
	req := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u1")
	c.SetParamNames("id")
	c.SetParamValues("hc-1")

	// Update query: only config (param 9) carries a value; all other
	// optional params are nil. Order matches store.UpdateHookConfig.
	mock.ExpectQuery(`UPDATE "HookConfig"`).
		WithArgs(
			"hc-1",
			(*string)(nil), (*string)(nil), (*string)(nil), (*string)(nil),
			(*string)(nil), (*string)(nil), (*string)(nil),
			json.RawMessage(`{"k":"v"}`),
			(*int)(nil), (*int)(nil), (*string)(nil), (*bool)(nil),
			[]string(nil),
		).
		WillReturnRows(pgxmock.NewRows(hookConfigCols).AddRow(makeHookConfigRow(now)...))
	mock.ExpectQuery(`SELECT value FROM system_metadata`).
		WillReturnRows(pgxmock.NewRows([]string{"value"}))
	mock.ExpectExec(`INSERT INTO system_metadata`).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	if err := h.UpdateHookConfig(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestUpdateHookConfig_Happy_NameOnly(t *testing.T) {
	mock, meta := newMockStore(t)
	now := time.Now().UTC()
	mock.ExpectQuery(`SELECT .* FROM "HookConfig" WHERE id`).
		WithArgs("hc-1").
		WillReturnRows(pgxmock.NewRows(hookConfigCols).AddRow(makeHookConfigRow(now)...))

	newName := "renamed"
	mock.ExpectQuery(`UPDATE "HookConfig"`).
		WithArgs(
			"hc-1",
			&newName,
			(*string)(nil), (*string)(nil), (*string)(nil), (*string)(nil),
			(*string)(nil), (*string)(nil), json.RawMessage(nil),
			(*int)(nil), (*int)(nil), (*string)(nil), (*bool)(nil),
			[]string(nil),
		).
		WillReturnRows(pgxmock.NewRows(hookConfigCols).AddRow(makeHookConfigRow(now)...))
	mock.ExpectQuery(`SELECT value FROM system_metadata`).
		WillReturnRows(pgxmock.NewRows([]string{"value"}))
	mock.ExpectExec(`INSERT INTO system_metadata`).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	hubS := &hubSpy{}
	aspy := &auditSpy{}
	h := newHandler(mock, meta, hubS, audit.NewWriter(aspy, "audit", slog.Default()))
	body := `{"name":"renamed"}`
	req := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u1")
	c.SetParamNames("id")
	c.SetParamValues("hc-1")
	if err := h.UpdateHookConfig(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(hubS.seen()) != 3 {
		t.Errorf("hub invalidations = %d; want 3", len(hubS.seen()))
	}
	deadline := time.Now().Add(500 * time.Millisecond)
	for aspy.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if aspy.count() != 1 {
		t.Errorf("audit count = %d; want 1", aspy.count())
	}
}

func TestUpdateHookConfig_StoreUpdateError_Returns500(t *testing.T) {
	mock, meta := newMockStore(t)
	now := time.Now().UTC()
	mock.ExpectQuery(`SELECT .* FROM "HookConfig" WHERE id`).
		WithArgs("hc-1").
		WillReturnRows(pgxmock.NewRows(hookConfigCols).AddRow(makeHookConfigRow(now)...))

	newName := "renamed"
	mock.ExpectQuery(`UPDATE "HookConfig"`).
		WithArgs(
			"hc-1",
			&newName,
			(*string)(nil), (*string)(nil), (*string)(nil), (*string)(nil),
			(*string)(nil), (*string)(nil), json.RawMessage(nil),
			(*int)(nil), (*int)(nil), (*string)(nil), (*bool)(nil),
			[]string(nil),
		).
		WillReturnError(errors.New("db boom"))

	hubS := &hubSpy{}
	h := newHandler(mock, meta, hubS, audit.NewWriter(nil, "", slog.Default()))
	body := `{"name":"renamed"}`
	req := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u1")
	c.SetParamNames("id")
	c.SetParamValues("hc-1")
	_ = h.UpdateHookConfig(c)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d", rec.Code)
	}
	if len(hubS.seen()) != 0 {
		t.Errorf("must not invalidate on store error; got %v", hubS.seen())
	}
}

func TestDeleteHookConfig_GetError_Returns500(t *testing.T) {
	mock, meta := newMockStore(t)
	mock.ExpectQuery(`SELECT .* FROM "HookConfig" WHERE id`).
		WithArgs("x").
		WillReturnError(errors.New("boom"))

	h := newHandler(mock, meta, nil, audit.NewWriter(nil, "", slog.Default()))
	req := httptest.NewRequest(http.MethodDelete, "/", nil)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u1")
	c.SetParamNames("id")
	c.SetParamValues("x")
	_ = h.DeleteHookConfig(c)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d", rec.Code)
	}
}

func TestDeleteHookConfig_NotFound_Returns404(t *testing.T) {
	mock, meta := newMockStore(t)
	mock.ExpectQuery(`SELECT .* FROM "HookConfig" WHERE id`).
		WithArgs("missing").
		WillReturnRows(pgxmock.NewRows(hookConfigCols))

	h := newHandler(mock, meta, nil, audit.NewWriter(nil, "", slog.Default()))
	req := httptest.NewRequest(http.MethodDelete, "/", nil)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u1")
	c.SetParamNames("id")
	c.SetParamValues("missing")
	_ = h.DeleteHookConfig(c)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d", rec.Code)
	}
}

func TestDeleteHookConfig_DeleteError_Returns500(t *testing.T) {
	mock, meta := newMockStore(t)
	now := time.Now().UTC()
	mock.ExpectQuery(`SELECT .* FROM "HookConfig" WHERE id`).
		WithArgs("hc-1").
		WillReturnRows(pgxmock.NewRows(hookConfigCols).AddRow(makeHookConfigRow(now)...))
	mock.ExpectExec(`DELETE FROM "HookConfig"`).
		WithArgs("hc-1").
		WillReturnError(errors.New("db boom"))

	hubS := &hubSpy{}
	h := newHandler(mock, meta, hubS, audit.NewWriter(nil, "", slog.Default()))
	req := httptest.NewRequest(http.MethodDelete, "/", nil)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u1")
	c.SetParamNames("id")
	c.SetParamValues("hc-1")
	_ = h.DeleteHookConfig(c)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d", rec.Code)
	}
	if len(hubS.seen()) != 0 {
		t.Errorf("must not invalidate on store error")
	}
}

func TestDeleteHookConfig_Happy(t *testing.T) {
	mock, meta := newMockStore(t)
	now := time.Now().UTC()
	mock.ExpectQuery(`SELECT .* FROM "HookConfig" WHERE id`).
		WithArgs("hc-1").
		WillReturnRows(pgxmock.NewRows(hookConfigCols).AddRow(makeHookConfigRow(now)...))
	mock.ExpectExec(`DELETE FROM "HookConfig"`).
		WithArgs("hc-1").
		WillReturnResult(pgxmock.NewResult("DELETE", 1))
	mock.ExpectQuery(`SELECT value FROM system_metadata`).
		WillReturnRows(pgxmock.NewRows([]string{"value"}))
	mock.ExpectExec(`INSERT INTO system_metadata`).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	hubS := &hubSpy{}
	aspy := &auditSpy{}
	h := newHandler(mock, meta, hubS, audit.NewWriter(aspy, "audit", slog.Default()))
	req := httptest.NewRequest(http.MethodDelete, "/", nil)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u1")
	c.SetParamNames("id")
	c.SetParamValues("hc-1")
	if err := h.DeleteHookConfig(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if len(hubS.seen()) != 3 {
		t.Errorf("hub invalidations = %d; want 3", len(hubS.seen()))
	}
	deadline := time.Now().Add(500 * time.Millisecond)
	for aspy.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if aspy.count() != 1 {
		t.Errorf("audit count = %d; want 1", aspy.count())
	}
	entry := aspy.last()
	if entry["action"] != "delete" || entry["entityId"] != "hc-1" {
		t.Errorf("audit: %+v", entry)
	}
}

func TestReorderHooks_BadJSON_Returns400(t *testing.T) {
	h := newHandler(nil, nil, nil, audit.NewWriter(nil, "", slog.Default()))
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("not-json"))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u1")
	_ = h.ReorderHooks(c)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d", rec.Code)
	}
}

func TestReorderHooks_MissingStageOrIDs_Returns400(t *testing.T) {
	h := newHandler(nil, nil, nil, audit.NewWriter(nil, "", slog.Default()))
	for _, body := range []string{`{}`, `{"stage":""}`, `{"stage":"request"}`, `{"ids":["x"]}`} {
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		rec := httptest.NewRecorder()
		c, _ := echoCtx(req, rec, "u1")
		_ = h.ReorderHooks(c)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %q: status = %d; want 400", body, rec.Code)
		}
	}
}

func TestReorderHooks_StoreError_Returns400(t *testing.T) {
	mock, meta := newMockStore(t)
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM "HookConfig" WHERE stage`).
		WithArgs("request").
		WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(3))
	mock.ExpectRollback()

	hubS := &hubSpy{}
	h := newHandler(mock, meta, hubS, audit.NewWriter(nil, "", slog.Default()))
	body := `{"stage":"request","ids":["a"]}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u1")
	_ = h.ReorderHooks(c)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "must provide exactly") {
		t.Errorf("body: %s", rec.Body.String())
	}
	if len(hubS.seen()) != 0 {
		t.Errorf("must not invalidate on store error")
	}
}

func TestReorderHooks_Happy(t *testing.T) {
	mock, meta := newMockStore(t)
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM "HookConfig" WHERE stage`).
		WithArgs("request").
		WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(2))
	mock.ExpectExec(`UPDATE "HookConfig" SET priority`).
		WithArgs(0, "a", "request").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectExec(`UPDATE "HookConfig" SET priority`).
		WithArgs(1, "b", "request").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectCommit()
	// invalidate + version bump after success.
	mock.ExpectQuery(`SELECT value FROM system_metadata`).
		WillReturnRows(pgxmock.NewRows([]string{"value"}))
	mock.ExpectExec(`INSERT INTO system_metadata`).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	hubS := &hubSpy{}
	aspy := &auditSpy{}
	h := newHandler(mock, meta, hubS, audit.NewWriter(aspy, "audit", slog.Default()))
	body := `{"stage":"request","ids":["a","b"]}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u1")
	if err := h.ReorderHooks(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var body2 map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body2)
	if body2["ok"] != true || body2["stage"] != "request" || body2["count"] != float64(2) {
		t.Errorf("body: %+v", body2)
	}
	if len(hubS.seen()) != 3 {
		t.Errorf("hub invalidations = %d; want 3", len(hubS.seen()))
	}
	deadline := time.Now().Add(500 * time.Millisecond)
	for aspy.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if aspy.count() != 1 {
		t.Errorf("audit count = %d", aspy.count())
	}
}

func TestHookForceRefresh_Happy(t *testing.T) {
	hubS := &hubSpy{}
	aspy := &auditSpy{}
	h := newHandler(nil, nil, hubS, audit.NewWriter(aspy, "audit", slog.Default()))
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u1")
	if err := h.HookForceRefresh(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var b map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &b)
	if b["refreshed"] != true {
		t.Errorf("body: %+v", b)
	}
	if len(hubS.seen()) != 3 {
		t.Errorf("hub = %v", hubS.seen())
	}
	deadline := time.Now().Add(500 * time.Millisecond)
	for aspy.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if aspy.count() != 1 {
		t.Errorf("audit count = %d", aspy.count())
	}
}

// IAM denial — RegisterRoutes wiring exercises the iamMW arg

func TestRegisterRoutes_IAMDenialBlocksHandler(t *testing.T) {
	mock, meta := newMockStore(t)
	// No SQL should be issued because middleware denies first.
	_ = mock
	hubS := &hubSpy{}
	h := newHandler(mock, meta, hubS, audit.NewWriter(nil, "", slog.Default()))

	eng := cpiam.NewEngine(nil, slog.Default())
	e := echo.New()
	g := e.Group("/api/admin")
	iamMW := func(action string) echo.MiddlewareFunc {
		return middleware.RequireIAMPermission(eng, action, nil)
	}
	h.RegisterRoutes(g, iamMW)

	// No admin auth on context → engine sees nil principal → deny.
	req := httptest.NewRequest(http.MethodGet, "/api/admin/hooks", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden && rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d; want 401/403; body=%s", rec.Code, rec.Body.String())
	}
	if len(hubS.seen()) != 0 {
		t.Errorf("hub should not be touched on IAM denial")
	}
}

// Compile-time guards

// Catalog-level guard: ensure the ResourceHook + verb combinations referenced
// by RegisterRoutes are valid in the IAM catalog (mirrors the EntryFor panic
// at startup).
func TestRouteVerbsKnownToCatalog(t *testing.T) {
	for _, v := range []iam.Verb{iam.VerbRead, iam.VerbCreate, iam.VerbUpdate, iam.VerbDelete} {
		if !iam.ResourceHook.Allows(v) {
			t.Errorf("ResourceHook should allow verb %q", v)
		}
	}
}

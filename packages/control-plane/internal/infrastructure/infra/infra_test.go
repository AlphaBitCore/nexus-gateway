// Package infra tests. Covers handler.go helpers + node_runtime + service_urls
// + diag_silences + diagevents + diagmode + setup + hub_proxy + thing_overrides.
//
// Strategy:
//   - pgxmock + store.NewWithPgxPool for every DB-touching path that flows
//     through *store.DB methods.
//   - For the one direct *pgxpool.Pool site (service_urls.go) we exercise the
//     servicePublicURLsQueryFn function-seam introduced by this test port.
//   - httptest.Server stands in for Hub HTTP endpoints (hub_proxy /
//     thing_overrides preflight / setup relay).
//   - fakeHub / panicHub interface stubs cover the non-HTTP Hub surface
//     (NotifyConfigChange / InvalidateConfig / GetThingRuntime / etc.).
package infra

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
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/labstack/echo/v4"
	"github.com/pashagolub/pgxmock/v4"

	auth "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authn"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/iam"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/hub"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/store"
)

// Helpers — mirror sibling cache/passthrough/agent packages

// auditSpy captures MQ enqueues so tests can assert audit emission.
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

// fakeHub satisfies HubAPI for tests that don't need the HTTP-level Hub.
// hubBaseURL / hubToken default to "" so handlers that gate on
// h.hub.BaseURL() trip the not-configured branch unless a test fills them.
type fakeHub struct {
	mu sync.Mutex

	baseURL string
	token   string

	// NotifyConfigChange capture + programmable result.
	notifyHits int
	notifyReq  hub.ConfigChangeRequest
	notifyResp *hub.ConfigChangeResponse
	notifyErr  error

	// InvalidateConfig capture.
	invalidateHits int
	invalidateLast struct {
		thingType string
		configKey string
	}

	// ForceResyncAll capture.
	forceResyncHits int
	forceResyncResp map[string]any
	forceResyncErr  error

	// CreateEnrollmentToken capture.
	enrollHits int
	enrollResp *hub.CreateEnrollmentTokenResponse
	enrollErr  error

	// GetThingRuntime capture.
	runtimeBody   []byte
	runtimeStatus int
	runtimeErr    error

	// GetThingServiceMeta capture.
	serviceMeta    *hub.ThingServiceMeta
	serviceMetaErr error
}

func (f *fakeHub) BaseURL() string { return f.baseURL }
func (f *fakeHub) Token() string   { return f.token }

func (f *fakeHub) NotifyConfigChange(_ context.Context, req hub.ConfigChangeRequest) (*hub.ConfigChangeResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.notifyHits++
	f.notifyReq = req
	return f.notifyResp, f.notifyErr
}

func (f *fakeHub) InvalidateConfig(_ context.Context, thingType, configKey string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.invalidateHits++
	f.invalidateLast.thingType = thingType
	f.invalidateLast.configKey = configKey
}

func (f *fakeHub) ForceResyncAll(_ context.Context, _ string) (map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.forceResyncHits++
	return f.forceResyncResp, f.forceResyncErr
}

func (f *fakeHub) CreateEnrollmentToken(_ context.Context, _ hub.CreateEnrollmentTokenRequest) (*hub.CreateEnrollmentTokenResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enrollHits++
	return f.enrollResp, f.enrollErr
}

func (f *fakeHub) GetThingRuntime(_ context.Context, _ string) ([]byte, int, error) {
	return f.runtimeBody, f.runtimeStatus, f.runtimeErr
}

func (f *fakeHub) GetThingServiceMeta(_ context.Context, _ string) (*hub.ThingServiceMeta, error) {
	return f.serviceMeta, f.serviceMetaErr
}

// stubGroupLookup satisfies ThingOverrideGroupLookup for the override tests.
type stubGroupLookup struct {
	groups []string
	err    error
}

func (s *stubGroupLookup) ListGroupNamesForPrincipal(_ context.Context, _, _ string) ([]string, error) {
	return s.groups, s.err
}

// newMockDB constructs a pgxmock-backed *store.DB. QueryMatcherRegexp is the
// default, but we set MatchExpectationsInOrder(false) so tests can use the
// helpers across all four packages without worrying about expectation order
// (the in-tx expectations remain ordered relative to Begin/Commit).
func newMockDB(t *testing.T) (pgxmock.PgxPoolIface, *store.DB) {
	t.Helper()
	mock, err := pgxmock.NewPool(pgxmock.QueryMatcherOption(pgxmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mock.Close)
	return mock, store.NewWithPgxPool(mock)
}

// anyArgs returns a slice of pgxmock.AnyArg() of length n, useful for
// WithArgs when the test doesn't care about specific arg values.
func anyArgs(n int) []any {
	out := make([]any, n)
	for i := range out {
		out[i] = pgxmock.AnyArg()
	}
	return out
}

// newHandler builds a Handler wired with the supplied parts.
func newHandler(t *testing.T, db *store.DB, hub HubAPI, spy *auditSpy) *Handler {
	t.Helper()
	if spy == nil {
		spy = &auditSpy{}
	}
	return New(Deps{
		DB:     db,
		Hub:    hub,
		Audit:  audit.NewWriter(spy, "audit", slog.Default()),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

// echoCtx builds an Echo context with an authenticated admin pre-populated.
func echoCtx(method, path, body string, admin bool) (echo.Context, *httptest.ResponseRecorder) {
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
	if admin {
		middleware.WithAdminAuth(c, &auth.AdminAuth{
			KeyID: "admin-1", KeyName: "Alice", AuthPrincipalType: "admin_user",
		})
	}
	return c, rec
}

// echoCtxParam attaches path params after construction.
func echoCtxParam(method, path, body string, admin bool, names, values []string) (echo.Context, *httptest.ResponseRecorder) {
	c, rec := echoCtx(method, path, body, admin)
	c.SetParamNames(names...)
	c.SetParamValues(values...)
	return c, rec
}

// handler.go — helper-copies (R6 §4.2)

func TestNew_DefaultsLoggerWhenNil(t *testing.T) {
	h := New(Deps{})
	if h == nil {
		t.Fatal("expected non-nil Handler")
		return
	}
	if h.logger == nil {
		t.Error("expected logger defaulted to slog.Default()")
	}
}

func TestNew_WiresAllFields(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	hub := &fakeHub{}
	spy := &auditSpy{}
	aw := audit.NewWriter(spy, "audit", log)
	stub := &stubGroupLookup{}
	hubClient := &http.Client{Timeout: time.Second}
	cpClient := &http.Client{Timeout: time.Second}
	proxy := ProxyConfig{AIGatewayURL: "http://gw"}
	_, db := newMockDB(t)
	h := New(Deps{
		DB:                       db,
		Hub:                      hub,
		Audit:                    aw,
		Logger:                   log,
		ThingOverrideGroupLookup: stub,
		HubProxyClient:           hubClient,
		ComplianceProxyClient:    cpClient,
		Proxy:                    proxy,
	})
	if h.db != db || h.hub != hub || h.audit != aw || h.logger != log {
		t.Error("required fields not wired")
	}
	if h.thingOverrideGroupLookupRef != stub || h.hubProxyClientRef != hubClient || h.complianceProxyClient != cpClient {
		t.Error("optional fields not wired")
	}
	if h.proxy.AIGatewayURL != "http://gw" {
		t.Error("proxy config not wired")
	}
}

func TestErrJSON(t *testing.T) {
	got := errJSON("oops", "validation_error", "X")
	root, ok := got["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected error envelope; got %v", got)
	}
	if root["message"] != "oops" || root["type"] != "validation_error" || root["code"] != "X" {
		t.Errorf("envelope shape wrong: %+v", root)
	}
}

func TestActorFromContext_NilEmptyPopulated(t *testing.T) {
	c, _ := echoCtx(http.MethodGet, "/", "", false)
	a := actorFromContext(c)
	if a.UserID != "" || a.Name != "" {
		t.Errorf("no auth → empty; got %+v", a)
	}

	c2, _ := echoCtx(http.MethodGet, "/", "", true)
	a2 := actorFromContext(c2)
	if a2.UserID != "admin-1" || a2.Name != "Alice" {
		t.Errorf("populated actor; got %+v", a2)
	}
}

func TestSourceIP(t *testing.T) {
	c, _ := echoCtx(http.MethodGet, "/", "", false)
	if got := sourceIP(c); got == "" {
		t.Error("RealIP should be non-empty by default")
	}
}

func TestParsePagination(t *testing.T) {
	cases := []struct {
		name       string
		url        string
		wantLimit  int
		wantOffset int
	}{
		{"defaults", "/", 50, 0},
		{"limit + offset", "/?limit=10&offset=20", 10, 20},
		{"limit > 1000 clamped", "/?limit=5000", 1000, 0},
		{"non-positive limit ignored", "/?limit=0", 50, 0},
		{"negative offset ignored", "/?offset=-5", 50, 0},
		{"non-numeric ignored", "/?limit=abc&offset=xyz", 50, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := echoCtx(http.MethodGet, tc.url, "", false)
			p := parsePagination(c)
			if p.Limit != tc.wantLimit || p.Offset != tc.wantOffset {
				t.Errorf("limit=%d offset=%d; want %d/%d", p.Limit, p.Offset, tc.wantLimit, tc.wantOffset)
			}
		})
	}
}

func TestInternalServerError(t *testing.T) {
	c, rec := echoCtx(http.MethodGet, "/", "", false)
	if err := internalServerError(c, "boom"); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d; want 500", rec.Code)
	}
}

func TestDeref(t *testing.T) {
	if deref(nil) != "" {
		t.Error("nil → empty")
	}
	s := "x"
	if deref(&s) != "x" {
		t.Error("non-nil → value")
	}
}

func TestParseRFC3339Flexible(t *testing.T) {
	if _, ok := parseRFC3339Flexible(""); ok {
		t.Error("empty should fail")
	}
	if _, ok := parseRFC3339Flexible("garbage"); ok {
		t.Error("garbage should fail")
	}
	if _, ok := parseRFC3339Flexible("2026-05-17T00:00:00Z"); !ok {
		t.Error("RFC3339 should succeed")
	}
	if _, ok := parseRFC3339Flexible("2026-05-17T00:00:00.123456789Z"); !ok {
		t.Error("RFC3339Nano should succeed")
	}
}

func TestParseTimeRange(t *testing.T) {
	c, _ := echoCtx(http.MethodGet, "/?startTime=2026-05-17T00:00:00Z&endTime=2026-05-18T00:00:00Z", "", false)
	s, e := parseTimeRange(c)
	if s == nil || e == nil {
		t.Fatalf("expected populated; got %v %v", s, e)
	}

	c2, _ := echoCtx(http.MethodGet, "/?start=2026-05-17T00:00:00Z&end=2026-05-18T00:00:00Z", "", false)
	s2, e2 := parseTimeRange(c2)
	if s2 == nil || e2 == nil {
		t.Fatalf("expected populated via fallback names; got %v %v", s2, e2)
	}

	c3, _ := echoCtx(http.MethodGet, "/?startTime=garbage&endTime=garbage", "", false)
	s3, e3 := parseTimeRange(c3)
	if s3 != nil || e3 != nil {
		t.Error("invalid timestamps should yield nil")
	}

	c4, _ := echoCtx(http.MethodGet, "/", "", false)
	s4, e4 := parseTimeRange(c4)
	if s4 != nil || e4 != nil {
		t.Error("missing params should yield nil")
	}
}

func TestFirstNonEmpty(t *testing.T) {
	if firstNonEmpty("", "", "x", "y") != "x" {
		t.Error("first non-empty wins")
	}
	if firstNonEmpty("", "", "") != "" {
		t.Error("all-empty → empty")
	}
}

func TestParseFromTo(t *testing.T) {
	c, _ := echoCtx(http.MethodGet, "/", "", false)
	if _, _, he := parseFromTo(c); he == nil || he.status != http.StatusBadRequest {
		t.Error("missing → 400")
	}
	c2, _ := echoCtx(http.MethodGet, "/?from=garbage&to=2026-05-17T00:00:00Z", "", false)
	if _, _, he := parseFromTo(c2); he == nil {
		t.Error("invalid from → err")
	}
	c3, _ := echoCtx(http.MethodGet, "/?from=2026-05-17T00:00:00Z&to=garbage", "", false)
	if _, _, he := parseFromTo(c3); he == nil {
		t.Error("invalid to → err")
	}
	c4, _ := echoCtx(http.MethodGet, "/?from=2026-05-18T00:00:00Z&to=2026-05-17T00:00:00Z", "", false)
	if _, _, he := parseFromTo(c4); he == nil {
		t.Error("from >= to → err")
	}
	c5, _ := echoCtx(http.MethodGet, "/?from=2026-05-17T00:00:00Z&to=2026-05-18T00:00:00Z", "", false)
	from, to, he := parseFromTo(c5)
	if he != nil {
		t.Errorf("happy: unexpected err %+v", he)
	}
	if !from.Before(to) {
		t.Error("from < to invariant violated")
	}
}

func TestGetNodeRuntime_MissingID(t *testing.T) {
	h := newHandler(t, nil, &fakeHub{}, nil)
	c, rec := echoCtxParam(http.MethodGet, "/", "", true, []string{"id"}, []string{""})
	if err := h.GetNodeRuntime(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d; want 400", rec.Code)
	}
}

func TestGetNodeRuntime_NilHub(t *testing.T) {
	h := newHandler(t, nil, nil, nil)
	c, rec := echoCtxParam(http.MethodGet, "/", "", true, []string{"id"}, []string{"thing-1"})
	if err := h.GetNodeRuntime(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("code = %d; want 503", rec.Code)
	}
}

func TestGetNodeRuntime_HubError(t *testing.T) {
	hub := &fakeHub{runtimeErr: errors.New("connection refused")}
	h := newHandler(t, nil, hub, nil)
	c, rec := echoCtxParam(http.MethodGet, "/", "", true, []string{"id"}, []string{"thing-1"})
	if err := h.GetNodeRuntime(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusBadGateway {
		t.Errorf("code = %d; want 502", rec.Code)
	}
}

func TestGetNodeRuntime_Hub5xxMappedTo502(t *testing.T) {
	hub := &fakeHub{runtimeBody: []byte(`{"error":"oops"}`), runtimeStatus: 500}
	h := newHandler(t, nil, hub, nil)
	c, rec := echoCtxParam(http.MethodGet, "/", "", true, []string{"id"}, []string{"thing-1"})
	if err := h.GetNodeRuntime(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusBadGateway {
		t.Errorf("code = %d; want 502", rec.Code)
	}
}

func TestGetNodeRuntime_Happy(t *testing.T) {
	hub := &fakeHub{runtimeBody: []byte(`{"snapshot":{},"meta":{}}`), runtimeStatus: 200}
	h := newHandler(t, nil, hub, nil)
	c, rec := echoCtxParam(http.MethodGet, "/", "", true, []string{"id"}, []string{"thing-1"})
	if err := h.GetNodeRuntime(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d; want 200", rec.Code)
	}
}

func TestGetServicePublicURLs_QueryError(t *testing.T) {
	_, db := newMockDB(t)
	h := newHandler(t, db, &fakeHub{}, nil)
	h.servicePublicURLsQueryFn = func(_ context.Context) ([]servicePublicURLRow, error) {
		return nil, errors.New("boom")
	}
	c, rec := echoCtx(http.MethodGet, "/", "", true)
	if err := h.GetServicePublicURLs(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d; want 500", rec.Code)
	}
}

func TestGetServicePublicURLs_HappyAllTypes(t *testing.T) {
	_, db := newMockDB(t)
	h := newHandler(t, db, &fakeHub{}, nil)
	h.servicePublicURLsQueryFn = func(_ context.Context) ([]servicePublicURLRow, error) {
		return []servicePublicURLRow{
			{ThingType: "nexus-hub", PublicURL: "https://hub"},
			{ThingType: "control-plane", PublicURL: "https://cp"},
			{ThingType: "ai-gateway", PublicURL: "https://gw"},
			{ThingType: "compliance-proxy", PublicURL: "https://prx"},
			{ThingType: "unknown", PublicURL: "https://x"},
		}, nil
	}
	c, rec := echoCtx(http.MethodGet, "/", "", true)
	if err := h.GetServicePublicURLs(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d; want 200", rec.Code)
	}
	var out ServicePublicURLs
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Hub != "https://hub" || out.ControlPlane != "https://cp" || out.AIGateway != "https://gw" || out.ComplianceProxy != "https://prx" {
		t.Errorf("unexpected payload: %+v", out)
	}
}

func TestGetServicePublicURLs_ProdPathHappy(t *testing.T) {
	// Drive the real queryServicePublicURLs body via the pgxmock seam so the
	// rows-iterate + scan branches are covered.
	mock, db := newMockDB(t)
	h := newHandler(t, db, &fakeHub{}, nil)
	h.servicePoolOverride = mock
	mock.ExpectQuery(`FROM thing\s+WHERE metadata->'staticInfo'->>'publicUrl'`).
		WillReturnRows(pgxmock.NewRows([]string{"type", "public_url"}).
			AddRow("nexus-hub", "https://hub").
			AddRow("ai-gateway", "https://gw"))
	c, rec := echoCtx(http.MethodGet, "/", "", true)
	if err := h.GetServicePublicURLs(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d; want 200; body %s", rec.Code, rec.Body.String())
	}
	var out ServicePublicURLs
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Hub != "https://hub" || out.AIGateway != "https://gw" {
		t.Errorf("unexpected payload: %+v", out)
	}
}

func TestGetServicePublicURLs_ProdPathQueryError(t *testing.T) {
	mock, db := newMockDB(t)
	h := newHandler(t, db, &fakeHub{}, nil)
	h.servicePoolOverride = mock
	mock.ExpectQuery(`FROM thing`).WillReturnError(errors.New("boom"))
	c, rec := echoCtx(http.MethodGet, "/", "", true)
	if err := h.GetServicePublicURLs(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d; want 500", rec.Code)
	}
}

func TestGetServicePublicURLs_ProdPathScanContinues(t *testing.T) {
	// The per-row scan-error branch logs + continues to the next row. We
	// trigger it by supplying a RowError on row 0 — the handler skips that
	// row, processes row 1, and returns 200 with the second row's value.
	mock, db := newMockDB(t)
	h := newHandler(t, db, &fakeHub{}, nil)
	h.servicePoolOverride = mock
	rows := pgxmock.NewRows([]string{"type", "public_url"}).
		AddRow("nexus-hub", "https://hub").
		AddRow("ai-gateway", "https://gw").
		RowError(0, errors.New("scan boom"))
	mock.ExpectQuery(`FROM thing`).WillReturnRows(rows)
	c, rec := echoCtx(http.MethodGet, "/", "", true)
	if err := h.GetServicePublicURLs(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d; want 200; body %s", rec.Code, rec.Body.String())
	}
	var out ServicePublicURLs
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	// Row 0 was errored, row 1 (ai-gateway) was processed.
	if out.AIGateway != "https://gw" {
		t.Errorf("expected continue to row 1; got %+v", out)
	}
}

func TestGetServicePublicURLs_ProdPathRowsErr(t *testing.T) {
	// Trigger the rows.Err() != nil branch — pgxmock supports an explicit
	// CloseError that surfaces from rows.Err().
	mock, db := newMockDB(t)
	h := newHandler(t, db, &fakeHub{}, nil)
	h.servicePoolOverride = mock
	rows := pgxmock.NewRows([]string{"type", "public_url"}).
		AddRow("nexus-hub", "https://hub").
		CloseError(errors.New("rows close boom"))
	mock.ExpectQuery(`FROM thing`).WillReturnRows(rows)
	c, rec := echoCtx(http.MethodGet, "/", "", true)
	if err := h.GetServicePublicURLs(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	// rows.Err() result is returned from queryServicePublicURLs → handler 500.
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d; want 500; body %s", rec.Code, rec.Body.String())
	}
}

func TestGetServicePublicURLs_PoolNotConfigured(t *testing.T) {
	h := newHandler(t, nil, &fakeHub{}, nil)
	c, rec := echoCtx(http.MethodGet, "/", "", true)
	if err := h.GetServicePublicURLs(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d; want 500", rec.Code)
	}
}

const diagSilenceCols = `id,message_hash,level,silenced_by,silenced_at,expires_at,reason`

func TestDiagSilencesList_NilDB(t *testing.T) {
	h := newHandler(t, nil, &fakeHub{}, nil)
	c, rec := echoCtx(http.MethodGet, "/", "", true)
	if err := h.DiagSilencesList(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("code = %d; want 503", rec.Code)
	}
}

func TestDiagSilencesList_DBError(t *testing.T) {
	mock, db := newMockDB(t)
	h := newHandler(t, db, &fakeHub{}, nil)
	mock.ExpectQuery(`FROM diag_silence`).WillReturnError(errors.New("boom"))
	c, rec := echoCtx(http.MethodGet, "/", "", true)
	if err := h.DiagSilencesList(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d; want 500", rec.Code)
	}
}

func TestDiagSilencesList_HappyEmpty(t *testing.T) {
	mock, db := newMockDB(t)
	h := newHandler(t, db, &fakeHub{}, nil)
	mock.ExpectQuery(`FROM diag_silence`).
		WillReturnRows(pgxmock.NewRows(strings.Split(diagSilenceCols, ",")))
	c, rec := echoCtx(http.MethodGet, "/", "", true)
	if err := h.DiagSilencesList(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d; want 200", rec.Code)
	}
	var out struct {
		Data []store.DiagSilence `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Data == nil {
		t.Error("Data should not be null; want []")
	}
}

func TestDiagSilencesCreate_NilDB(t *testing.T) {
	h := newHandler(t, nil, &fakeHub{}, nil)
	c, rec := echoCtx(http.MethodPost, "/", `{}`, true)
	if err := h.DiagSilencesCreate(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("code = %d; want 503", rec.Code)
	}
}

func TestDiagSilencesCreate_BindError(t *testing.T) {
	_, db := newMockDB(t)
	h := newHandler(t, db, &fakeHub{}, nil)
	c, rec := echoCtx(http.MethodPost, "/", `{not json`, true)
	if err := h.DiagSilencesCreate(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d; want 400", rec.Code)
	}
}

func TestDiagSilencesCreate_ValidationGate(t *testing.T) {
	_, db := newMockDB(t)
	h := newHandler(t, db, &fakeHub{}, nil)
	cases := []struct {
		name string
		body string
	}{
		{"missing hash", `{"level":"error"}`},
		{"bad level", `{"messageHash":"h","level":"info-ish"}`},
		{"negative ttl", `{"messageHash":"h","level":"error","ttlSeconds":-1}`},
		{"ttl over cap", `{"messageHash":"h","level":"error","ttlSeconds":99999999}`},
		{"reason too long", `{"messageHash":"h","level":"error","reason":"` + strings.Repeat("x", 501) + `"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, rec := echoCtx(http.MethodPost, "/", tc.body, true)
			if err := h.DiagSilencesCreate(c); err != nil {
				t.Fatalf("err: %v", err)
			}
			if rec.Code != http.StatusBadRequest {
				t.Errorf("code = %d; want 400", rec.Code)
			}
		})
	}
}

func TestDiagSilencesCreate_StoreError(t *testing.T) {
	mock, db := newMockDB(t)
	h := newHandler(t, db, &fakeHub{}, nil)
	mock.ExpectQuery(`INSERT INTO diag_silence`).WithArgs(anyArgs(6)...).WillReturnError(errors.New("boom"))
	c, rec := echoCtx(http.MethodPost, "/", `{"messageHash":"h","level":"error"}`, true)
	if err := h.DiagSilencesCreate(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d; want 500", rec.Code)
	}
}

func TestDiagSilencesCreate_Happy(t *testing.T) {
	mock, db := newMockDB(t)
	spy := &auditSpy{}
	h := newHandler(t, db, &fakeHub{}, spy)
	exp := time.Now().Add(time.Hour)
	mock.ExpectQuery(`INSERT INTO diag_silence`).
		WithArgs(anyArgs(6)...).
		WillReturnRows(pgxmock.NewRows(strings.Split(diagSilenceCols, ",")).
			AddRow("sil-1", "hash-1", "error", "admin-1", time.Now(), &exp, (*string)(nil)))
	body := `{"messageHash":"hash-1","level":"error","ttlSeconds":3600,"reason":"noise"}`
	c, rec := echoCtx(http.MethodPost, "/", body, true)
	if err := h.DiagSilencesCreate(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d; want 200; body %s", rec.Code, rec.Body.String())
	}
	if spy.count() != 1 {
		t.Errorf("expected 1 audit entry; got %d", spy.count())
	}
}

func TestDiagSilencesCreate_PermanentNoTTL(t *testing.T) {
	mock, db := newMockDB(t)
	h := newHandler(t, db, &fakeHub{}, nil)
	mock.ExpectQuery(`INSERT INTO diag_silence`).
		WithArgs(anyArgs(6)...).
		WillReturnRows(pgxmock.NewRows(strings.Split(diagSilenceCols, ",")).
			AddRow("sil-1", "h", "warn", "admin-1", time.Now(), (*time.Time)(nil), (*string)(nil)))
	c, rec := echoCtx(http.MethodPost, "/", `{"messageHash":"h","level":"warn"}`, true)
	if err := h.DiagSilencesCreate(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d; want 200; body %s", rec.Code, rec.Body.String())
	}
}

func TestDiagSilencesDelete_NilDB(t *testing.T) {
	h := newHandler(t, nil, &fakeHub{}, nil)
	c, rec := echoCtxParam(http.MethodDelete, "/", "", true, []string{"id"}, []string{"x"})
	if err := h.DiagSilencesDelete(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("code = %d; want 503", rec.Code)
	}
}

func TestDiagSilencesDelete_EmptyID(t *testing.T) {
	_, db := newMockDB(t)
	h := newHandler(t, db, &fakeHub{}, nil)
	c, rec := echoCtxParam(http.MethodDelete, "/", "", true, []string{"id"}, []string{""})
	if err := h.DiagSilencesDelete(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d; want 400", rec.Code)
	}
}

func TestDiagSilencesDelete_GetNotFound(t *testing.T) {
	mock, db := newMockDB(t)
	h := newHandler(t, db, &fakeHub{}, nil)
	mock.ExpectQuery(`FROM diag_silence WHERE id = \$1`).
		WithArgs("sil-1").
		WillReturnError(pgx.ErrNoRows)
	c, rec := echoCtxParam(http.MethodDelete, "/", "", true, []string{"id"}, []string{"sil-1"})
	if err := h.DiagSilencesDelete(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("code = %d; want 404", rec.Code)
	}
}

func TestDiagSilencesDelete_GetGenericError(t *testing.T) {
	mock, db := newMockDB(t)
	h := newHandler(t, db, &fakeHub{}, nil)
	mock.ExpectQuery(`FROM diag_silence WHERE id = \$1`).
		WithArgs("sil-1").
		WillReturnError(errors.New("boom"))
	c, rec := echoCtxParam(http.MethodDelete, "/", "", true, []string{"id"}, []string{"sil-1"})
	if err := h.DiagSilencesDelete(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d; want 500", rec.Code)
	}
}

func TestDiagSilencesDelete_DeleteNotFound(t *testing.T) {
	mock, db := newMockDB(t)
	h := newHandler(t, db, &fakeHub{}, nil)
	mock.ExpectQuery(`FROM diag_silence WHERE id = \$1`).
		WithArgs("sil-1").
		WillReturnRows(pgxmock.NewRows(strings.Split(diagSilenceCols, ",")).
			AddRow("sil-1", "h", "error", "admin-1", time.Now(), (*time.Time)(nil), (*string)(nil)))
	mock.ExpectExec(`DELETE FROM diag_silence`).WithArgs("sil-1").WillReturnResult(pgxmock.NewResult("DELETE", 0))
	c, rec := echoCtxParam(http.MethodDelete, "/", "", true, []string{"id"}, []string{"sil-1"})
	if err := h.DiagSilencesDelete(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("code = %d; want 404 (delete-side notfound)", rec.Code)
	}
}

func TestDiagSilencesDelete_DeleteGenericError(t *testing.T) {
	mock, db := newMockDB(t)
	h := newHandler(t, db, &fakeHub{}, nil)
	mock.ExpectQuery(`FROM diag_silence WHERE id = \$1`).
		WithArgs("sil-1").
		WillReturnRows(pgxmock.NewRows(strings.Split(diagSilenceCols, ",")).
			AddRow("sil-1", "h", "error", "admin-1", time.Now(), (*time.Time)(nil), (*string)(nil)))
	mock.ExpectExec(`DELETE FROM diag_silence`).WithArgs("sil-1").WillReturnError(errors.New("boom"))
	c, rec := echoCtxParam(http.MethodDelete, "/", "", true, []string{"id"}, []string{"sil-1"})
	if err := h.DiagSilencesDelete(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d; want 500", rec.Code)
	}
}

func TestDiagSilencesDelete_Happy(t *testing.T) {
	mock, db := newMockDB(t)
	spy := &auditSpy{}
	h := newHandler(t, db, &fakeHub{}, spy)
	mock.ExpectQuery(`FROM diag_silence WHERE id = \$1`).
		WithArgs("sil-1").
		WillReturnRows(pgxmock.NewRows(strings.Split(diagSilenceCols, ",")).
			AddRow("sil-1", "h", "error", "admin-1", time.Now(), (*time.Time)(nil), (*string)(nil)))
	mock.ExpectExec(`DELETE FROM diag_silence`).WithArgs("sil-1").WillReturnResult(pgxmock.NewResult("DELETE", 1))
	c, rec := echoCtxParam(http.MethodDelete, "/", "", true, []string{"id"}, []string{"sil-1"})
	if err := h.DiagSilencesDelete(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d; want 200", rec.Code)
	}
	if spy.count() != 1 {
		t.Errorf("expected 1 audit entry; got %d", spy.count())
	}
}

const diagEventCols = `id,thing_id,thing_type,occurred_at,received_at,level,event_type,source,message,message_hash,attrs,stack_trace,repeat_count,agent_version,os_info`

func TestDiagEventsList_NilDB(t *testing.T) {
	h := newHandler(t, nil, &fakeHub{}, nil)
	c, rec := echoCtx(http.MethodGet, "/", "", true)
	if err := h.DiagEventsList(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("code = %d; want 503", rec.Code)
	}
}

func TestDiagEventsList_InvalidFrom(t *testing.T) {
	_, db := newMockDB(t)
	h := newHandler(t, db, &fakeHub{}, nil)
	c, rec := echoCtx(http.MethodGet, "/?from=garbage", "", true)
	if err := h.DiagEventsList(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d; want 400", rec.Code)
	}
}

func TestDiagEventsList_InvalidTo(t *testing.T) {
	_, db := newMockDB(t)
	h := newHandler(t, db, &fakeHub{}, nil)
	c, rec := echoCtx(http.MethodGet, "/?to=garbage", "", true)
	if err := h.DiagEventsList(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d; want 400", rec.Code)
	}
}

func TestDiagEventsList_InvalidLevel(t *testing.T) {
	_, db := newMockDB(t)
	h := newHandler(t, db, &fakeHub{}, nil)
	c, rec := echoCtx(http.MethodGet, "/?level=critical", "", true)
	if err := h.DiagEventsList(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d; want 400", rec.Code)
	}
}

func TestDiagEventsList_CursorDecodeBubblesAs400(t *testing.T) {
	mock, db := newMockDB(t)
	h := newHandler(t, db, &fakeHub{}, nil)
	// Bad cursor is decoded by the store helper before any SQL is issued —
	// the mock will not be asked.
	_ = mock
	c, rec := echoCtx(http.MethodGet, "/?cursor=not-a-valid-cursor", "", true)
	if err := h.DiagEventsList(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d; want 400 (cursor decode)", rec.Code)
	}
}

func TestDiagEventsList_DBGenericError(t *testing.T) {
	mock, db := newMockDB(t)
	h := newHandler(t, db, &fakeHub{}, nil)
	// No filters in request → only the limit+1 bind arg.
	mock.ExpectQuery(`FROM thing_diag_event`).WithArgs(anyArgs(1)...).WillReturnError(errors.New("boom"))
	c, rec := echoCtx(http.MethodGet, "/", "", true)
	if err := h.DiagEventsList(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d; want 500", rec.Code)
	}
}

func TestDiagEventsList_HappyEmpty(t *testing.T) {
	mock, db := newMockDB(t)
	h := newHandler(t, db, &fakeHub{}, nil)
	// 7 filter args: thing_id, level, event_type, source, from, to, search +
	// limit → 8 total bind args.
	mock.ExpectQuery(`FROM thing_diag_event`).WithArgs(anyArgs(8)...).
		WillReturnRows(pgxmock.NewRows(strings.Split(diagEventCols, ",")))
	c, rec := echoCtx(http.MethodGet, "/?nodeId=t1&level=error&eventType=crash&source=agent&q=panic&limit=5&from=2026-05-17T00:00:00Z&to=2026-05-18T00:00:00Z", "", true)
	if err := h.DiagEventsList(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d; want 200; body %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Data       []store.DiagEvent `json:"data"`
		NextCursor string            `json:"nextCursor"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Data == nil {
		t.Error("Data should be [] not null")
	}
}

func TestDiagEventsGroups_NilDB(t *testing.T) {
	h := newHandler(t, nil, &fakeHub{}, nil)
	c, rec := echoCtx(http.MethodGet, "/?from=2026-05-17T00:00:00Z&to=2026-05-18T00:00:00Z", "", true)
	if err := h.DiagEventsGroups(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("code = %d; want 503", rec.Code)
	}
}

func TestDiagEventsGroups_MissingFromTo(t *testing.T) {
	_, db := newMockDB(t)
	h := newHandler(t, db, &fakeHub{}, nil)
	c, rec := echoCtx(http.MethodGet, "/", "", true)
	if err := h.DiagEventsGroups(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d; want 400", rec.Code)
	}
}

func TestDiagEventsGroups_DBError(t *testing.T) {
	mock, db := newMockDB(t)
	h := newHandler(t, db, &fakeHub{}, nil)
	mock.ExpectQuery(`SELECT e\.message_hash`).WithArgs(anyArgs(4)...).WillReturnError(errors.New("boom"))
	c, rec := echoCtx(http.MethodGet, "/?from=2026-05-17T00:00:00Z&to=2026-05-18T00:00:00Z", "", true)
	if err := h.DiagEventsGroups(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d; want 500", rec.Code)
	}
}

func TestDiagEventsGroups_HappyEmpty(t *testing.T) {
	mock, db := newMockDB(t)
	h := newHandler(t, db, &fakeHub{}, nil)
	mock.ExpectQuery(`SELECT e\.message_hash`).WithArgs(anyArgs(4)...).
		WillReturnRows(pgxmock.NewRows([]string{
			"message_hash", "sample_message", "source", "affected_things",
			"total_occurrences", "first_seen", "last_seen", "max_level", "silenced",
		}))
	c, rec := echoCtx(http.MethodGet, "/?from=2026-05-17T00:00:00Z&to=2026-05-18T00:00:00Z&nodeType=agent&eventType=crash", "", true)
	if err := h.DiagEventsGroups(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d; want 200; body %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Data []store.DiagGroup `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Data == nil {
		t.Error("Data should be [] not null")
	}
}

func TestDiagEventsCrashCohorts_NilDB(t *testing.T) {
	h := newHandler(t, nil, &fakeHub{}, nil)
	c, rec := echoCtx(http.MethodGet, "/?from=2026-05-17T00:00:00Z&to=2026-05-18T00:00:00Z", "", true)
	if err := h.DiagEventsCrashCohorts(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("code = %d; want 503", rec.Code)
	}
}

func TestDiagEventsCrashCohorts_MissingFromTo(t *testing.T) {
	_, db := newMockDB(t)
	h := newHandler(t, db, &fakeHub{}, nil)
	c, rec := echoCtx(http.MethodGet, "/", "", true)
	if err := h.DiagEventsCrashCohorts(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d; want 400", rec.Code)
	}
}

func TestDiagEventsCrashCohorts_DBError(t *testing.T) {
	mock, db := newMockDB(t)
	h := newHandler(t, db, &fakeHub{}, nil)
	mock.ExpectQuery(`event_type = 'crash'`).WithArgs(anyArgs(2)...).WillReturnError(errors.New("boom"))
	c, rec := echoCtx(http.MethodGet, "/?from=2026-05-17T00:00:00Z&to=2026-05-18T00:00:00Z", "", true)
	if err := h.DiagEventsCrashCohorts(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d; want 500", rec.Code)
	}
}

func TestDiagEventsCrashCohorts_HappyEmpty(t *testing.T) {
	mock, db := newMockDB(t)
	h := newHandler(t, db, &fakeHub{}, nil)
	mock.ExpectQuery(`event_type = 'crash'`).WithArgs(anyArgs(2)...).
		WillReturnRows(pgxmock.NewRows([]string{
			"agent_version", "os", "os_version", "crash_count", "affected_things", "last_seen",
		}))
	c, rec := echoCtx(http.MethodGet, "/?from=2026-05-17T00:00:00Z&to=2026-05-18T00:00:00Z", "", true)
	if err := h.DiagEventsCrashCohorts(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d; want 200", rec.Code)
	}
}

const diagModeWindowCols = `id,thing_id,thing_type,started_at,ended_at,set_by,reason,created_at`
const diagModeInsertCols = `id,thing_id,started_at,ended_at,set_by,reason,created_at`

// expectEnableDiagModeTx wires the 4-step EnableDiagMode tx onto the mock.
// thingExists toggles the existence-check branch.
func expectEnableDiagModeTx(mock pgxmock.PgxPoolIface, thingID string, thingExists bool) {
	mock.ExpectBegin()
	if thingExists {
		mock.ExpectQuery(`SELECT id FROM thing WHERE id = \$1`).
			WithArgs(thingID).
			WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(thingID))
	} else {
		mock.ExpectQuery(`SELECT id FROM thing WHERE id = \$1`).
			WithArgs(thingID).
			WillReturnError(pgx.ErrNoRows)
		mock.ExpectRollback()
		return
	}
	mock.ExpectExec(`UPDATE thing_diag_mode_window`).WithArgs(thingID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	mock.ExpectQuery(`INSERT INTO thing_diag_mode_window`).WithArgs(anyArgs(4)...).
		WillReturnRows(pgxmock.NewRows(strings.Split(diagModeInsertCols, ",")).
			AddRow("w-1", thingID, time.Now(), time.Now().Add(time.Hour), (*string)(nil), (*string)(nil), time.Now()))
	mock.ExpectCommit()
}

// diagHubServer is an httptest stand-in for Hub's override API
// (PUT/DELETE /api/hub/things/:id/overrides/diag_mode). It replies with the
// given status so diag-mode handler tests exercise the override-delivery hop.
func diagHubServer(t *testing.T, status int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{}`))
	}))
}

func TestEnableDiagMode_NilDB(t *testing.T) {
	h := newHandler(t, nil, &fakeHub{}, nil)
	c, rec := echoCtxParam(http.MethodPost, "/", `{"until":""}`, true, []string{"nodeId"}, []string{"t1"})
	if err := h.EnableDiagMode(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("code = %d; want 503", rec.Code)
	}
}

func TestEnableDiagMode_MissingNodeID(t *testing.T) {
	_, db := newMockDB(t)
	h := newHandler(t, db, &fakeHub{}, nil)
	c, rec := echoCtxParam(http.MethodPost, "/", `{}`, true, []string{"nodeId"}, []string{""})
	if err := h.EnableDiagMode(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d; want 400", rec.Code)
	}
}

func TestEnableDiagMode_BindError(t *testing.T) {
	_, db := newMockDB(t)
	h := newHandler(t, db, &fakeHub{}, nil)
	c, rec := echoCtxParam(http.MethodPost, "/", `{not json`, true, []string{"nodeId"}, []string{"t1"})
	if err := h.EnableDiagMode(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d; want 400", rec.Code)
	}
}

func TestEnableDiagMode_UntilValidationCases(t *testing.T) {
	_, db := newMockDB(t)
	h := newHandler(t, db, &fakeHub{}, nil)
	cases := []struct {
		name string
		body string
	}{
		{"empty until", `{"until":""}`},
		{"invalid until", `{"until":"not-a-timestamp"}`},
		{"past until", `{"until":"2000-01-01T00:00:00Z"}`},
		{"too far future", `{"until":"3000-01-01T00:00:00Z"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, rec := echoCtxParam(http.MethodPost, "/", tc.body, true, []string{"nodeId"}, []string{"t1"})
			if err := h.EnableDiagMode(c); err != nil {
				t.Fatalf("err: %v", err)
			}
			if rec.Code != http.StatusBadRequest {
				t.Errorf("code = %d; want 400", rec.Code)
			}
		})
	}
}

func TestEnableDiagMode_ThingNotFound(t *testing.T) {
	_, db := newMockDB(t)
	// Override-first: Hub reports the thing missing, the handler echoes 404
	// without touching the local window store.
	hubSrv := diagHubServer(t, http.StatusNotFound)
	defer hubSrv.Close()
	h := newHandler(t, db, &fakeHub{baseURL: hubSrv.URL}, nil)
	h.hubProxyClientRef = hubSrv.Client()
	until := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	body := `{"until":"` + until + `"}`
	c, rec := echoCtxParam(http.MethodPost, "/", body, true, []string{"nodeId"}, []string{"t1"})
	if err := h.EnableDiagMode(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("code = %d; want 404", rec.Code)
	}
}

func TestEnableDiagMode_StoreError(t *testing.T) {
	mock, db := newMockDB(t)
	hubSrv := diagHubServer(t, http.StatusOK)
	defer hubSrv.Close()
	h := newHandler(t, db, &fakeHub{baseURL: hubSrv.URL}, nil)
	h.hubProxyClientRef = hubSrv.Client()
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT id FROM thing WHERE id = \$1`).
		WithArgs("t1").
		WillReturnError(errors.New("boom"))
	mock.ExpectRollback()
	until := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	body := `{"until":"` + until + `"}`
	c, rec := echoCtxParam(http.MethodPost, "/", body, true, []string{"nodeId"}, []string{"t1"})
	if err := h.EnableDiagMode(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d; want 500", rec.Code)
	}
}

func TestEnableDiagMode_HappyPath(t *testing.T) {
	mock, db := newMockDB(t)
	var gotMethod, gotPath string
	hubSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer hubSrv.Close()
	h := newHandler(t, db, &fakeHub{baseURL: hubSrv.URL}, nil)
	h.hubProxyClientRef = hubSrv.Client()
	expectEnableDiagModeTx(mock, "t1", true)
	until := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	body := `{"until":"` + until + `","reason":"investigate"}`
	c, rec := echoCtxParam(http.MethodPost, "/", body, true, []string{"nodeId"}, []string{"t1"})
	if err := h.EnableDiagMode(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d; want 200; body: %s", rec.Code, rec.Body.String())
	}
	// Delivery went out as a PUT diag_mode override; the window row was recorded.
	if gotMethod != http.MethodPut || gotPath != "/api/hub/things/t1/overrides/diag_mode" {
		t.Errorf("override call = %s %s; want PUT /api/hub/things/t1/overrides/diag_mode", gotMethod, gotPath)
	}
}

func TestDisableDiagMode_NilDB(t *testing.T) {
	h := newHandler(t, nil, &fakeHub{}, nil)
	c, rec := echoCtxParam(http.MethodDelete, "/", "", true, []string{"nodeId"}, []string{"t1"})
	if err := h.DisableDiagMode(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("code = %d; want 503", rec.Code)
	}
}

func TestDisableDiagMode_MissingID(t *testing.T) {
	_, db := newMockDB(t)
	h := newHandler(t, db, &fakeHub{}, nil)
	c, rec := echoCtxParam(http.MethodDelete, "/", "", true, []string{"nodeId"}, []string{""})
	if err := h.DisableDiagMode(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d; want 400", rec.Code)
	}
}

func TestDisableDiagMode_NoActiveWindow(t *testing.T) {
	mock, db := newMockDB(t)
	hubSrv := diagHubServer(t, http.StatusOK)
	defer hubSrv.Close()
	h := newHandler(t, db, &fakeHub{baseURL: hubSrv.URL}, nil)
	h.hubProxyClientRef = hubSrv.Client()
	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE thing_diag_mode_window`).WithArgs("t1").
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	mock.ExpectRollback()
	c, rec := echoCtxParam(http.MethodDelete, "/", "", true, []string{"nodeId"}, []string{"t1"})
	if err := h.DisableDiagMode(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("code = %d; want 404", rec.Code)
	}
}

func TestDisableDiagMode_StoreError(t *testing.T) {
	mock, db := newMockDB(t)
	hubSrv := diagHubServer(t, http.StatusOK)
	defer hubSrv.Close()
	h := newHandler(t, db, &fakeHub{baseURL: hubSrv.URL}, nil)
	h.hubProxyClientRef = hubSrv.Client()
	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE thing_diag_mode_window`).WithArgs("t1").WillReturnError(errors.New("boom"))
	mock.ExpectRollback()
	c, rec := echoCtxParam(http.MethodDelete, "/", "", true, []string{"nodeId"}, []string{"t1"})
	if err := h.DisableDiagMode(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d; want 500", rec.Code)
	}
}

func TestDisableDiagMode_Happy(t *testing.T) {
	mock, db := newMockDB(t)
	var gotMethod, gotPath string
	hubSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer hubSrv.Close()
	h := newHandler(t, db, &fakeHub{baseURL: hubSrv.URL}, nil)
	h.hubProxyClientRef = hubSrv.Client()
	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE thing_diag_mode_window`).WithArgs("t1").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectCommit()
	c, rec := echoCtxParam(http.MethodDelete, "/", "", true, []string{"nodeId"}, []string{"t1"})
	if err := h.DisableDiagMode(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d; want 200; body %s", rec.Code, rec.Body.String())
	}
	if gotMethod != http.MethodDelete || gotPath != "/api/hub/things/t1/overrides/diag_mode" {
		t.Errorf("override clear = %s %s; want DELETE /api/hub/things/t1/overrides/diag_mode", gotMethod, gotPath)
	}
}

func TestListDiagMode_NilDB(t *testing.T) {
	h := newHandler(t, nil, &fakeHub{}, nil)
	c, rec := echoCtx(http.MethodGet, "/", "", true)
	if err := h.ListDiagMode(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("code = %d; want 503", rec.Code)
	}
}

func TestListDiagMode_DBError(t *testing.T) {
	mock, db := newMockDB(t)
	h := newHandler(t, db, &fakeHub{}, nil)
	mock.ExpectQuery(`FROM thing_diag_mode_window`).WillReturnError(errors.New("boom"))
	c, rec := echoCtx(http.MethodGet, "/", "", true)
	if err := h.ListDiagMode(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d; want 500", rec.Code)
	}
}

func TestListDiagMode_HappyEmpty(t *testing.T) {
	mock, db := newMockDB(t)
	h := newHandler(t, db, &fakeHub{}, nil)
	mock.ExpectQuery(`FROM thing_diag_mode_window`).
		WillReturnRows(pgxmock.NewRows(strings.Split(diagModeWindowCols, ",")))
	c, rec := echoCtx(http.MethodGet, "/", "", true)
	if err := h.ListDiagMode(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d; want 200", rec.Code)
	}
	var out struct {
		Data []store.DiagModeWindow `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Data == nil {
		t.Error("Data should be [] not null")
	}
}

func TestBulkEnableDiagMode_NilDB(t *testing.T) {
	h := newHandler(t, nil, &fakeHub{}, nil)
	c, rec := echoCtx(http.MethodPost, "/", `{}`, true)
	if err := h.BulkEnableDiagMode(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("code = %d; want 503", rec.Code)
	}
}

func TestBulkEnableDiagMode_BindError(t *testing.T) {
	_, db := newMockDB(t)
	h := newHandler(t, db, &fakeHub{}, nil)
	c, rec := echoCtx(http.MethodPost, "/", `{not json`, true)
	if err := h.BulkEnableDiagMode(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d; want 400", rec.Code)
	}
}

func TestBulkEnableDiagMode_BadUntil(t *testing.T) {
	_, db := newMockDB(t)
	h := newHandler(t, db, &fakeHub{}, nil)
	c, rec := echoCtx(http.MethodPost, "/", `{"until":"garbage"}`, true)
	if err := h.BulkEnableDiagMode(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d; want 400", rec.Code)
	}
}

func TestBulkEnableDiagMode_ResolveError(t *testing.T) {
	mock, db := newMockDB(t)
	h := newHandler(t, db, &fakeHub{}, nil)
	mock.ExpectQuery(`FROM thing`).WithArgs(anyArgs(2)...).WillReturnError(errors.New("boom"))
	until := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	body := `{"filter":{"nodeIds":["t1"]},"until":"` + until + `"}`
	c, rec := echoCtx(http.MethodPost, "/", body, true)
	if err := h.BulkEnableDiagMode(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d; want 500", rec.Code)
	}
}

func TestBulkEnableDiagMode_EmptyResult(t *testing.T) {
	mock, db := newMockDB(t)
	h := newHandler(t, db, &fakeHub{}, nil)
	mock.ExpectQuery(`FROM thing`).WithArgs(anyArgs(2)...).
		WillReturnRows(pgxmock.NewRows([]string{"id"}))
	until := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	body := `{"filter":{"nodeIds":["t1"]},"until":"` + until + `"}`
	c, rec := echoCtx(http.MethodPost, "/", body, true)
	if err := h.BulkEnableDiagMode(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d; want 200", rec.Code)
	}
}

func TestBulkEnableDiagMode_PerThingMixed(t *testing.T) {
	mock, db := newMockDB(t)
	hubSrv := diagHubServer(t, http.StatusOK)
	defer hubSrv.Close()
	h := newHandler(t, db, &fakeHub{baseURL: hubSrv.URL}, nil)
	h.hubProxyClientRef = hubSrv.Client()
	// Resolve returns 2 ids.
	mock.ExpectQuery(`FROM thing`).WithArgs(anyArgs(2)...).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("t1").AddRow("t2"))
	// Override delivery succeeds for both; the window store records t1 and
	// rejects t2 (thing not found) → t2 is the partial failure.
	expectEnableDiagModeTx(mock, "t1", true)
	expectEnableDiagModeTx(mock, "t2", false)

	until := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	body := `{"filter":{"nodeIds":["t1","t2"]},"until":"` + until + `","reason":"sweep"}`
	c, rec := echoCtx(http.MethodPost, "/", body, true)
	if err := h.BulkEnableDiagMode(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusMultiStatus {
		t.Errorf("code = %d; want 207 (partial failure); body %s", rec.Code, rec.Body.String())
	}
}

func TestBulkEnableDiagMode_AllSuccess(t *testing.T) {
	mock, db := newMockDB(t)
	hubSrv := diagHubServer(t, http.StatusOK)
	defer hubSrv.Close()
	h := newHandler(t, db, &fakeHub{baseURL: hubSrv.URL}, nil)
	h.hubProxyClientRef = hubSrv.Client()
	mock.ExpectQuery(`FROM thing`).WithArgs(anyArgs(2)...).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("t1"))
	expectEnableDiagModeTx(mock, "t1", true)
	until := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	body := `{"filter":{"nodeIds":["t1"]},"until":"` + until + `"}`
	c, rec := echoCtx(http.MethodPost, "/", body, true)
	if err := h.BulkEnableDiagMode(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d; want 200; body %s", rec.Code, rec.Body.String())
	}
}

func TestParseUntil(t *testing.T) {
	if _, he := parseUntil(""); he == nil {
		t.Error("empty should err")
	}
	if _, he := parseUntil("garbage"); he == nil {
		t.Error("garbage should err")
	}
	if _, he := parseUntil("2000-01-01T00:00:00Z"); he == nil {
		t.Error("past should err")
	}
	far := time.Now().Add(72 * time.Hour).UTC().Format(time.RFC3339)
	if _, he := parseUntil(far); he == nil {
		t.Error("too far should err")
	}
	ok := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	if _, he := parseUntil(ok); he != nil {
		t.Errorf("happy: %+v", he)
	}
}

func TestSetupClient_DefaultAndOverride(t *testing.T) {
	h := newHandler(t, nil, &fakeHub{}, nil)
	if h.setupClient() != defaultSetupHTTPClient {
		t.Error("expected default client when complianceProxyClient is nil")
	}
	custom := &http.Client{Timeout: time.Second}
	h.complianceProxyClient = custom
	if h.setupClient() != custom {
		t.Error("expected override")
	}
}

// Helper: the signature returns (url string, handled bool) where handled=true
// means resolveManagementURL already wrote the error response and the caller
// must return without proceeding. Tests check rec.Code + handled +
// the empty url on the unhappy paths.
func TestResolveManagementURL_NotFound(t *testing.T) {
	hub := &fakeHub{serviceMetaErr: errors.New("not found")}
	h := newHandler(t, nil, hub, nil)
	c, rec := echoCtx(http.MethodGet, "/", "", true)
	url, handled := h.resolveManagementURL(c, "t1")
	if url != "" || !handled {
		t.Errorf("expected empty url + handled=true; got url=%q handled=%v", url, handled)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("code = %d; want 404", rec.Code)
	}
}

func TestResolveManagementURL_HubError(t *testing.T) {
	hub := &fakeHub{serviceMetaErr: errors.New("connection refused")}
	h := newHandler(t, nil, hub, nil)
	c, rec := echoCtx(http.MethodGet, "/", "", true)
	url, handled := h.resolveManagementURL(c, "t1")
	if url != "" || !handled {
		t.Errorf("expected empty url + handled=true; got url=%q handled=%v", url, handled)
	}
	if rec.Code != http.StatusBadGateway {
		t.Errorf("code = %d; want 502", rec.Code)
	}
}

func TestResolveManagementURL_NoManagementURL(t *testing.T) {
	hub := &fakeHub{serviceMeta: &hub.ThingServiceMeta{ThingID: "t1", ManagementURL: ""}}
	h := newHandler(t, nil, hub, nil)
	c, rec := echoCtx(http.MethodGet, "/", "", true)
	url, handled := h.resolveManagementURL(c, "t1")
	if url != "" || !handled {
		t.Errorf("expected empty url + handled=true; got url=%q handled=%v", url, handled)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("code = %d; want 404", rec.Code)
	}
}

func TestResolveManagementURL_Happy(t *testing.T) {
	hub := &fakeHub{serviceMeta: &hub.ThingServiceMeta{ThingID: "t1", ManagementURL: "http://mgmt"}}
	h := newHandler(t, nil, hub, nil)
	c, _ := echoCtx(http.MethodGet, "/", "", true)
	url, handled := h.resolveManagementURL(c, "t1")
	if url != "http://mgmt" || handled {
		t.Errorf("got url=%q handled=%v", url, handled)
	}
}

func TestSetupGetCACert_ResolveFails(t *testing.T) {
	hub := &fakeHub{serviceMetaErr: errors.New("not found")}
	h := newHandler(t, nil, hub, nil)
	c, rec := echoCtxParam(http.MethodGet, "/", "", true, []string{"thingId"}, []string{"t1"})
	if err := h.SetupGetCACert(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("code = %d; want 404", rec.Code)
	}
}

func TestSetupGetCACert_RelayError(t *testing.T) {
	// resolveManagementURL succeeds; the relay HTTP call fails because the
	// management URL is unreachable.
	hub := &fakeHub{serviceMeta: &hub.ThingServiceMeta{ManagementURL: "http://127.0.0.1:1"}}
	h := newHandler(t, nil, hub, nil)
	h.complianceProxyClient = &http.Client{Timeout: 100 * time.Millisecond}
	c, rec := echoCtxParam(http.MethodGet, "/", "", true, []string{"thingId"}, []string{"t1"})
	if err := h.SetupGetCACert(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusBadGateway {
		t.Errorf("code = %d; want 502", rec.Code)
	}
}

func TestSetupGetCACert_Non200FromProxy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`oops`))
	}))
	defer srv.Close()
	hub := &fakeHub{serviceMeta: &hub.ThingServiceMeta{ManagementURL: srv.URL}}
	h := newHandler(t, nil, hub, nil)
	h.complianceProxyClient = srv.Client()
	c, rec := echoCtxParam(http.MethodGet, "/", "", true, []string{"thingId"}, []string{"t1"})
	if err := h.SetupGetCACert(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusBadGateway {
		t.Errorf("code = %d; want 502", rec.Code)
	}
}

func TestSetupGetCACert_Happy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/management/ca-cert" {
			t.Errorf("unexpected path: %q", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("PEM"))
	}))
	defer srv.Close()
	hub := &fakeHub{serviceMeta: &hub.ThingServiceMeta{ManagementURL: srv.URL}}
	h := newHandler(t, nil, hub, nil)
	h.complianceProxyClient = srv.Client()
	c, rec := echoCtxParam(http.MethodGet, "/", "", true, []string{"thingId"}, []string{"t1"})
	if err := h.SetupGetCACert(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d; want 200", rec.Code)
	}
	if rec.Body.String() != "PEM" {
		t.Errorf("body = %q; want PEM", rec.Body.String())
	}
	if got := rec.Header().Get("Content-Disposition"); got == "" {
		t.Error("expected Content-Disposition header")
	}
}

func TestSetupGetMDMProfile_ResolveFails(t *testing.T) {
	hub := &fakeHub{serviceMetaErr: errors.New("not found")}
	h := newHandler(t, nil, hub, nil)
	c, rec := echoCtxParam(http.MethodGet, "/", "", true, []string{"thingId"}, []string{"t1"})
	if err := h.SetupGetMDMProfile(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("code = %d; want 404", rec.Code)
	}
}

func TestSetupGetMDMProfile_RelayError(t *testing.T) {
	hub := &fakeHub{serviceMeta: &hub.ThingServiceMeta{ManagementURL: "http://127.0.0.1:1"}}
	h := newHandler(t, nil, hub, nil)
	h.complianceProxyClient = &http.Client{Timeout: 100 * time.Millisecond}
	c, rec := echoCtxParam(http.MethodGet, "/", "", true, []string{"thingId"}, []string{"t1"})
	if err := h.SetupGetMDMProfile(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusBadGateway {
		t.Errorf("code = %d; want 502", rec.Code)
	}
}

func TestSetupGetMDMProfile_HappyDefaultOrg(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("PEM"))
	}))
	defer srv.Close()
	hub := &fakeHub{serviceMeta: &hub.ThingServiceMeta{ManagementURL: srv.URL}}
	h := newHandler(t, nil, hub, nil)
	h.complianceProxyClient = srv.Client()
	c, rec := echoCtxParam(http.MethodGet, "/", "", true, []string{"thingId"}, []string{"t1"})
	if err := h.SetupGetMDMProfile(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d; want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Nexus Gateway Proxy CA") {
		t.Errorf("template not rendered; body=%s", rec.Body.String())
	}
}

func TestSetupGetMDMProfile_HappyCustomOrg(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("PEM"))
	}))
	defer srv.Close()
	hub := &fakeHub{serviceMeta: &hub.ThingServiceMeta{ManagementURL: srv.URL}}
	h := newHandler(t, nil, hub, nil)
	h.complianceProxyClient = srv.Client()
	c, rec := echoCtxParam(http.MethodGet, "/?organization=ACME", "", true, []string{"thingId"}, []string{"t1"})
	if err := h.SetupGetMDMProfile(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(rec.Body.String(), "ACME") {
		t.Errorf("custom org not interpolated; body=%s", rec.Body.String())
	}
}

func TestSetupGetMDMProfile_ProxyReturnsNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	hub := &fakeHub{serviceMeta: &hub.ThingServiceMeta{ManagementURL: srv.URL}}
	h := newHandler(t, nil, hub, nil)
	h.complianceProxyClient = srv.Client()
	c, rec := echoCtxParam(http.MethodGet, "/", "", true, []string{"thingId"}, []string{"t1"})
	if err := h.SetupGetMDMProfile(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusBadGateway {
		t.Errorf("code = %d; want 502", rec.Code)
	}
}

func TestSetupGetPACFile_MissingParams(t *testing.T) {
	_, db := newMockDB(t)
	h := newHandler(t, db, &fakeHub{}, nil)
	c, rec := echoCtx(http.MethodGet, "/", "", true)
	if err := h.SetupGetPACFile(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d; want 400", rec.Code)
	}
}

func TestSetupGetPACFile_DBError(t *testing.T) {
	mock, db := newMockDB(t)
	h := newHandler(t, db, &fakeHub{}, nil)
	mock.ExpectQuery(`FROM "interception_domain"`).WillReturnError(errors.New("boom"))
	c, rec := echoCtx(http.MethodGet, "/?proxyHost=proxy.example.com&proxyPort=3128", "", true)
	if err := h.SetupGetPACFile(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d; want 500", rec.Code)
	}
}

func TestSetupGetPACFile_NoDomains(t *testing.T) {
	mock, db := newMockDB(t)
	h := newHandler(t, db, &fakeHub{}, nil)
	// Empty result: attachPaths short-circuits on len==0; only the main query fires.
	mock.ExpectQuery(`FROM "interception_domain"`).
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "name", "description", "host_pattern", "host_match_type",
			"adapter_id", "adapter_config", "enabled", "priority", "default_path_action",
			"on_adapter_error", "network_zone", "source", "created_at", "updated_at", "created_by",
		}))
	c, rec := echoCtx(http.MethodGet, "/?proxyHost=p&proxyPort=3128", "", true)
	if err := h.SetupGetPACFile(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d; want 200; body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "FindProxyForURL") {
		t.Errorf("template not rendered; body=%s", rec.Body.String())
	}
}

func TestSetupGetPACFile_HappyWithDomainsAndFailOpen(t *testing.T) {
	mock, db := newMockDB(t)
	h := newHandler(t, db, &fakeHub{}, nil)
	now := time.Now()
	// ListEnabledInterceptionDomains' main SELECT takes 0 args.
	mock.ExpectQuery(`FROM "interception_domain"`).
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "name", "description", "host_pattern", "host_match_type",
			"adapter_id", "adapter_config", "enabled", "priority", "default_path_action",
			"on_adapter_error", "network_zone", "source",
			"created_at", "updated_at", "created_by",
		}).
			AddRow("d-1", "anthropic", (*string)(nil), "api.anthropic.com", "exact", "anthropic",
				[]byte(`{}`), true, 100, "block", "passthrough", "internet", "builtin", now, now, (*string)(nil)).
			AddRow("d-2", "anthropic-sub", (*string)(nil), "*.anthropic.com", "wildcard", "anthropic",
				[]byte(`{}`), true, 90, "block", "passthrough", "internet", "builtin", now, now, (*string)(nil)))
	// attachPaths sub-query takes 1 arg (ids slice).
	mock.ExpectQuery(`FROM interception_path`).WithArgs(anyArgs(1)...).
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "domain_id", "path_pattern", "match_type", "action",
			"priority", "description", "enabled", "created_at", "updated_at",
		}))
	c, rec := echoCtx(http.MethodGet, "/?proxyHost=p&proxyPort=3128&failOpen=true", "", true)
	if err := h.SetupGetPACFile(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d; want 200; body %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "api.anthropic.com") || !strings.Contains(body, ".anthropic.com") {
		t.Errorf("expected both domain fragments; body=%s", body)
	}
	if !strings.Contains(body, "DIRECT") {
		t.Errorf("expected DIRECT (failOpen); body=%s", body)
	}
}

func TestSetupPatchOnboarding_BindError(t *testing.T) {
	h := newHandler(t, nil, &fakeHub{}, nil)
	c, rec := echoCtxParam(http.MethodPatch, "/", `{not json`, true, []string{"thingId"}, []string{"t1"})
	if err := h.SetupPatchOnboarding(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d; want 400", rec.Code)
	}
}

func TestSetupPatchOnboarding_HubError(t *testing.T) {
	// serviceMeta must resolve so we reach the config push; the notify error
	// then surfaces as 502 (the up-front thing existence check).
	fh := &fakeHub{
		serviceMeta: &hub.ThingServiceMeta{ThingID: "t1", ManagementURL: "http://proxy"},
		notifyErr:   errors.New("boom"),
	}
	h := newHandler(t, nil, fh, nil)
	c, rec := echoCtxParam(http.MethodPatch, "/", `{"enabled":true}`, true, []string{"thingId"}, []string{"t1"})
	if err := h.SetupPatchOnboarding(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusBadGateway {
		t.Errorf("code = %d; want 502", rec.Code)
	}
}

func TestSetupPatchOnboarding_Happy(t *testing.T) {
	fh := &fakeHub{
		serviceMeta: &hub.ThingServiceMeta{ThingID: "t1", ManagementURL: "http://proxy"},
	}
	h := newHandler(t, nil, fh, nil)
	c, rec := echoCtxParam(http.MethodPatch, "/", `{"enabled":true}`, true, []string{"thingId"}, []string{"t1"})
	if err := h.SetupPatchOnboarding(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d; want 200", rec.Code)
	}
	if fh.notifyHits != 1 || fh.notifyReq.ThingType != "compliance-proxy" || fh.notifyReq.ConfigKey != "onboarding" {
		t.Errorf("unexpected NotifyConfigChange call: %+v", fh.notifyReq)
	}
}

// withHubServer returns a *fakeHub pointing at an httptest.Server that runs
// the given handler. cleanup is registered via t.Cleanup.
func withHubServer(t *testing.T, h http.Handler) (*fakeHub, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &fakeHub{baseURL: srv.URL, token: "tok"}, srv
}

func TestHubProxyClient_DefaultAndOverride(t *testing.T) {
	h := newHandler(t, nil, &fakeHub{}, nil)
	if h.hubProxyClient() != defaultHubHTTPClient {
		t.Error("expected default")
	}
	custom := &http.Client{Timeout: time.Second}
	h.hubProxyClientRef = custom
	if h.hubProxyClient() != custom {
		t.Error("expected override")
	}
}

func TestHubForward_NoHub(t *testing.T) {
	h := newHandler(t, nil, nil, nil)
	c, rec := echoCtx(http.MethodGet, "/", "", true)
	if err := h.hubForward(c, http.MethodGet, "/api/hub/things", nil); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("code = %d; want 503", rec.Code)
	}
}

func TestHubForward_HubUnreachable(t *testing.T) {
	hub := &fakeHub{baseURL: "http://127.0.0.1:1", token: "tok"}
	h := newHandler(t, nil, hub, nil)
	h.hubProxyClientRef = &http.Client{Timeout: 100 * time.Millisecond}
	c, rec := echoCtx(http.MethodGet, "/", "", true)
	if err := h.hubForward(c, http.MethodGet, "/api/hub/things", nil); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusBadGateway {
		t.Errorf("code = %d; want 502", rec.Code)
	}
}

func TestHubForward_Happy(t *testing.T) {
	hub, _ := withHubServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("expected Bearer token; got %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	h := newHandler(t, nil, hub, nil)
	c, rec := echoCtx(http.MethodGet, "/api/admin/nodes", "", true)
	if err := h.hubForward(c, http.MethodGet, "/api/hub/things", nil); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d; want 200; body %s", rec.Code, rec.Body.String())
	}
}

func TestHubForward_PostBodyAndAuth(t *testing.T) {
	hub, _ := withHubServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s; want POST", r.Method)
		}
		body, _ := io.ReadAll(r.Body)
		if !bytes.Contains(body, []byte(`"x"`)) {
			t.Errorf("body forwarded: %s", string(body))
		}
		if r.Header.Get("X-Nexus-Actor-Id") != "admin-1" || r.Header.Get("X-Nexus-Actor-Name") != "Alice" {
			t.Errorf("actor headers missing")
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	h := newHandler(t, nil, hub, nil)
	c, rec := echoCtx(http.MethodPost, "/api/admin/test", `{"x":1}`, true)
	if err := h.hubForward(c, http.MethodPost, "/api/hub/things", nil); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d; want 200; body %s", rec.Code, rec.Body.String())
	}
}

func TestHubForward_RenameRuns(t *testing.T) {
	hub, _ := withHubServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"orig":1}`))
	}))
	h := newHandler(t, nil, hub, nil)
	called := false
	rename := func(in []byte) ([]byte, error) {
		called = true
		return []byte(`{"renamed":true}`), nil
	}
	c, rec := echoCtx(http.MethodGet, "/", "", true)
	if err := h.hubForward(c, http.MethodGet, "/api/hub/things", rename); err != nil {
		t.Fatalf("err: %v", err)
	}
	if !called {
		t.Error("rename not invoked")
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"renamed"`)) {
		t.Errorf("renamed body not surfaced: %s", rec.Body.String())
	}
}

func TestHubForward_RenameError(t *testing.T) {
	hub, _ := withHubServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	h := newHandler(t, nil, hub, nil)
	rename := func(_ []byte) ([]byte, error) { return nil, errors.New("rename boom") }
	c, rec := echoCtx(http.MethodGet, "/", "", true)
	if err := h.hubForward(c, http.MethodGet, "/api/hub/things", rename); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusBadGateway {
		t.Errorf("code = %d; want 502", rec.Code)
	}
}

func TestNodesList_Happy(t *testing.T) {
	hub, _ := withHubServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"items":[]}`))
	}))
	h := newHandler(t, nil, hub, nil)
	c, rec := echoCtx(http.MethodGet, "/", "", true)
	if err := h.NodesList(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d; want 200", rec.Code)
	}
}

func TestNodesGet_Happy(t *testing.T) {
	hub, _ := withHubServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"x"}`))
	}))
	h := newHandler(t, nil, hub, nil)
	c, rec := echoCtxParam(http.MethodGet, "/", "", true, []string{"id"}, []string{"t-1"})
	if err := h.NodesGet(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d; want 200; body %s", rec.Code, rec.Body.String())
	}
}

func TestConfigSyncOutOfSync_Happy(t *testing.T) {
	hub, _ := withHubServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	h := newHandler(t, nil, hub, nil)
	c, rec := echoCtx(http.MethodGet, "/", "", true)
	if err := h.ConfigSyncOutOfSync(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d; want 200", rec.Code)
	}
}

func TestConfigSyncHistory_RewritesQueryParam(t *testing.T) {
	var seenQuery string
	hub, _ := withHubServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	h := newHandler(t, nil, hub, nil)
	c, rec := echoCtx(http.MethodGet, "/?nodeType=agent", "", true)
	if err := h.ConfigSyncHistory(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d; want 200", rec.Code)
	}
	if !strings.Contains(seenQuery, "thingType=agent") || strings.Contains(seenQuery, "nodeType") {
		t.Errorf("nodeType→thingType rewrite failed: query=%q", seenQuery)
	}
}

func TestConfigSyncHistory_QueryAlreadyHasThingType(t *testing.T) {
	var seenQuery string
	hub, _ := withHubServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	h := newHandler(t, nil, hub, nil)
	c, _ := echoCtx(http.MethodGet, "/?thingType=agent&nodeType=other", "", true)
	if err := h.ConfigSyncHistory(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	// Should NOT clobber an existing thingType.
	if !strings.Contains(seenQuery, "thingType=agent") {
		t.Errorf("existing thingType not preserved: %q", seenQuery)
	}
}

func TestConfigSyncCatalog_Happy(t *testing.T) {
	hub, _ := withHubServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	h := newHandler(t, nil, hub, nil)
	c, rec := echoCtx(http.MethodGet, "/", "", true)
	if err := h.ConfigSyncCatalog(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d; want 200", rec.Code)
	}
}

func TestConfigSyncUpdate_BindError(t *testing.T) {
	h := newHandler(t, nil, &fakeHub{}, nil)
	c, rec := echoCtx(http.MethodPost, "/", `{not json`, true)
	if err := h.ConfigSyncUpdate(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d; want 400", rec.Code)
	}
}

func TestConfigSyncUpdate_MissingFields(t *testing.T) {
	h := newHandler(t, nil, &fakeHub{}, nil)
	c, rec := echoCtx(http.MethodPost, "/", `{"nodeType":""}`, true)
	if err := h.ConfigSyncUpdate(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d; want 400", rec.Code)
	}
}

func TestConfigSyncUpdate_HappyWithStateAndAction(t *testing.T) {
	hub, _ := withHubServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !bytes.Contains(body, []byte(`"thingType":"agent"`)) {
			t.Errorf("nodeType→thingType rewrite missing: %s", string(body))
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	spy := &auditSpy{}
	h := newHandler(t, nil, hub, spy)
	body := `{"nodeType":"agent","configKey":"routing","state":{"x":1},"action":"toggle"}`
	c, rec := echoCtx(http.MethodPost, "/", body, true)
	if err := h.ConfigSyncUpdate(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d; want 200; body %s", rec.Code, rec.Body.String())
	}
	if spy.count() != 1 {
		t.Errorf("expected 1 audit entry; got %d", spy.count())
	}
}

func TestConfigSyncUpdate_HubError(t *testing.T) {
	hub, _ := withHubServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{}`))
	}))
	spy := &auditSpy{}
	h := newHandler(t, nil, hub, spy)
	c, rec := echoCtx(http.MethodPost, "/", `{"nodeType":"agent","configKey":"x"}`, true)
	if err := h.ConfigSyncUpdate(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d; want 500", rec.Code)
	}
	if spy.count() != 0 {
		t.Errorf("expected no audit on hub error")
	}
}

func TestJobsList_Happy(t *testing.T) {
	hub, _ := withHubServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	h := newHandler(t, nil, hub, nil)
	c, rec := echoCtx(http.MethodGet, "/", "", true)
	if err := h.JobsList(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d; want 200", rec.Code)
	}
}

func TestJobsGet_Happy(t *testing.T) {
	hub, _ := withHubServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	h := newHandler(t, nil, hub, nil)
	c, rec := echoCtxParam(http.MethodGet, "/", "", true, []string{"id"}, []string{"j-1"})
	if err := h.JobsGet(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d; want 200", rec.Code)
	}
}

func TestJobsListRuns_Happy(t *testing.T) {
	hub, _ := withHubServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	h := newHandler(t, nil, hub, nil)
	c, rec := echoCtxParam(http.MethodGet, "/", "", true, []string{"id"}, []string{"j-1"})
	if err := h.JobsListRuns(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d; want 200", rec.Code)
	}
}

func TestJobsUpdate_HappyEmitsAudit(t *testing.T) {
	hub, _ := withHubServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	spy := &auditSpy{}
	h := newHandler(t, nil, hub, spy)
	c, rec := echoCtxParam(http.MethodPut, "/", `{"enabled":true}`, true, []string{"id"}, []string{"j-1"})
	if err := h.JobsUpdate(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d; want 200", rec.Code)
	}
	if spy.count() != 1 {
		t.Errorf("expected 1 audit entry; got %d", spy.count())
	}
}

func TestJobsUpdate_HubError(t *testing.T) {
	hub, _ := withHubServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{}`))
	}))
	spy := &auditSpy{}
	h := newHandler(t, nil, hub, spy)
	c, rec := echoCtxParam(http.MethodPut, "/", `{"enabled":true}`, true, []string{"id"}, []string{"j-1"})
	if err := h.JobsUpdate(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d; want 500", rec.Code)
	}
	if spy.count() != 0 {
		t.Errorf("expected no audit on hub error")
	}
}

func TestJobsTrigger_HappyEmitsAudit(t *testing.T) {
	hub, _ := withHubServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	spy := &auditSpy{}
	h := newHandler(t, nil, hub, spy)
	c, rec := echoCtxParam(http.MethodPost, "/", "", true, []string{"id"}, []string{"j-1"})
	if err := h.JobsTrigger(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d; want 200", rec.Code)
	}
	if spy.count() != 1 {
		t.Errorf("expected 1 audit entry; got %d", spy.count())
	}
}

func TestJobsTrigger_HubError(t *testing.T) {
	hub, _ := withHubServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{}`))
	}))
	spy := &auditSpy{}
	h := newHandler(t, nil, hub, spy)
	c, rec := echoCtxParam(http.MethodPost, "/", "", true, []string{"id"}, []string{"j-1"})
	if err := h.JobsTrigger(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d; want 500", rec.Code)
	}
	if spy.count() != 0 {
		t.Errorf("expected no audit on hub error")
	}
}

func TestEnrollmentListTokens_Happy(t *testing.T) {
	hub, _ := withHubServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[]`))
	}))
	h := newHandler(t, nil, hub, nil)
	c, rec := echoCtx(http.MethodGet, "/", "", true)
	if err := h.EnrollmentListTokens(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d; want 200", rec.Code)
	}
}

func TestEnrollmentCreateToken_HappyEmitsAudit(t *testing.T) {
	hub, _ := withHubServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"token":"x"}`))
	}))
	spy := &auditSpy{}
	h := newHandler(t, nil, hub, spy)
	c, rec := echoCtx(http.MethodPost, "/", `{"label":"x"}`, true)
	if err := h.EnrollmentCreateToken(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("code = %d; want 201", rec.Code)
	}
	if spy.count() != 1 {
		t.Errorf("expected 1 audit entry; got %d", spy.count())
	}
}

func TestEnrollmentCreateToken_HubError(t *testing.T) {
	hub, _ := withHubServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{}`))
	}))
	spy := &auditSpy{}
	h := newHandler(t, nil, hub, spy)
	c, rec := echoCtx(http.MethodPost, "/", `{"label":"x"}`, true)
	if err := h.EnrollmentCreateToken(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d; want 500", rec.Code)
	}
	if spy.count() != 0 {
		t.Errorf("expected no audit on hub error")
	}
}

// GetNodeDeviceAssignments hits store.ListDeviceAssignments via the mock pool.
const deviceAssignmentCols = `id,device_id,user_id,assigned_at,released_at,source,login_method,ip_address,token_jti,display_name,os_username,os_domain`

func TestGetNodeDeviceAssignments_DBError(t *testing.T) {
	mock, db := newMockDB(t)
	h := newHandler(t, db, &fakeHub{}, nil)
	mock.ExpectQuery(`FROM "DeviceAssignment"`).WithArgs("t-1").WillReturnError(errors.New("boom"))
	c, rec := echoCtxParam(http.MethodGet, "/", "", true, []string{"id"}, []string{"t-1"})
	if err := h.GetNodeDeviceAssignments(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d; want 500", rec.Code)
	}
}

func TestGetNodeDeviceAssignments_HappyEmpty(t *testing.T) {
	mock, db := newMockDB(t)
	h := newHandler(t, db, &fakeHub{}, nil)
	mock.ExpectQuery(`FROM "DeviceAssignment"`).WithArgs("t-1").
		WillReturnRows(pgxmock.NewRows(strings.Split(deviceAssignmentCols, ",")))
	c, rec := echoCtxParam(http.MethodGet, "/", "", true, []string{"id"}, []string{"t-1"})
	if err := h.GetNodeDeviceAssignments(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d; want 200; body %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Data  []store.DeviceAssignmentDetail `json:"data"`
		Total int                            `json:"total"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Data == nil {
		t.Error("Data should be [] not null")
	}
}

// thing_overrides.go — pure helpers

func TestThingTypeRBACDecision(t *testing.T) {
	cases := []struct {
		name      string
		groups    []string
		thingType string
		want      bool
	}{
		{"super admin agent", []string{"super-admins"}, "agent", true},
		{"super admin service", []string{"super-admins"}, "ai-gateway", true},
		{"provider mgr service", []string{"provider-managers"}, "ai-gateway", true},
		{"provider mgr agent denied", []string{"provider-managers"}, "agent", false},
		{"compliance team agent", []string{"compliance-team"}, "agent", true},
		{"compliance team alias", []string{"compliance-officers"}, "agent", true},
		{"compliance team service denied", []string{"compliance-team"}, "ai-gateway", false},
		{"unknown group denied", []string{"random"}, "agent", false},
		{"empty groups denied", nil, "agent", false},
		{"multiple includes super", []string{"random", "super-admins"}, "agent", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := thingTypeRBACDecision(tc.groups, tc.thingType); got != tc.want {
				t.Errorf("got %v; want %v", got, tc.want)
			}
		})
	}
}

func TestErrString(t *testing.T) {
	if errString(nil) != "" {
		t.Error("nil → empty")
	}
	if errString(errors.New("oops")) != "oops" {
		t.Error("non-nil → message")
	}
}

func TestIsJSONObjectAdmin(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
	}{
		{`{}`, true},
		{`{"a":1}`, true},
		{`[]`, false},
		// JSON `null` unmarshals into a Go map without error (the map ends up
		// as the zero-value nil). The helper therefore accepts null — this is
		// the documented invariant, even if it surprises an admin reader.
		{`null`, true},
		{`"x"`, false},
		{`not json`, false},
		{``, false},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			if got := isJSONObjectAdmin(json.RawMessage(tc.raw)); got != tc.want {
				t.Errorf("got %v; want %v for %q", got, tc.want, tc.raw)
			}
		})
	}
}

func TestValidateAdminOverrideBody(t *testing.T) {
	in10m := time.Now().Add(10 * time.Minute)
	in5d := time.Now().Add(5 * 24 * time.Hour)
	in1m := time.Now().Add(1 * time.Minute)
	in40d := time.Now().Add(40 * 24 * time.Hour)
	longReason := strings.Repeat("x", 501)
	okReason := "valid"
	cases := []struct {
		name      string
		configKey string
		body      adminSetOverrideBody
		wantSub   string
	}{
		{"blacklisted key", "credentials", adminSetOverrideBody{State: json.RawMessage(`{}`)}, "not overridable"},
		{"missing state", "routing", adminSetOverrideBody{}, "state is required"},
		{"state not object", "routing", adminSetOverrideBody{State: json.RawMessage(`[]`)}, "must be a JSON object"},
		{"reason too long", "routing", adminSetOverrideBody{State: json.RawMessage(`{}`), Reason: &longReason}, "reason exceeds"},
		{"expiry too soon", "routing", adminSetOverrideBody{State: json.RawMessage(`{}`), ExpiresAt: &in1m}, "out of range"},
		{"expiry too far", "routing", adminSetOverrideBody{State: json.RawMessage(`{}`), ExpiresAt: &in40d}, "out of range"},
		{"happy minimum", "routing", adminSetOverrideBody{State: json.RawMessage(`{}`), ExpiresAt: &in10m, Reason: &okReason}, ""},
		{"happy max", "routing", adminSetOverrideBody{State: json.RawMessage(`{}`), ExpiresAt: &in5d}, ""},
		{"happy no-expiry no-reason", "routing", adminSetOverrideBody{State: json.RawMessage(`{}`)}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := validateAdminOverrideBody(tc.configKey, tc.body)
			if tc.wantSub == "" && got != "" {
				t.Errorf("expected empty; got %q", got)
			}
			if tc.wantSub != "" && !strings.Contains(got, tc.wantSub) {
				t.Errorf("expected substr %q; got %q", tc.wantSub, got)
			}
		})
	}
}

func TestListAdminGroups(t *testing.T) {
	// No AdminAuth on context → nil.
	c, _ := echoCtx(http.MethodGet, "/", "", false)
	h := newHandler(t, nil, &fakeHub{}, nil)
	if g := h.listAdminGroups(c); g != nil {
		t.Errorf("no auth → nil; got %v", g)
	}

	// AdminAuth + nil lookup → nil.
	c2, _ := echoCtx(http.MethodGet, "/", "", true)
	if g := h.listAdminGroups(c2); g != nil {
		t.Errorf("nil lookup → nil; got %v", g)
	}

	// AdminAuth + lookup returns groups.
	stub := &stubGroupLookup{groups: []string{"super-admins"}}
	h.thingOverrideGroupLookupRef = stub
	c3, _ := echoCtx(http.MethodGet, "/", "", true)
	g := h.listAdminGroups(c3)
	if len(g) != 1 || g[0] != "super-admins" {
		t.Errorf("expected [super-admins]; got %v", g)
	}

	// lookup returns error → nil.
	stub.err = errors.New("boom")
	stub.groups = nil
	if g := h.listAdminGroups(c3); g != nil {
		t.Errorf("lookup err → nil; got %v", g)
	}
}

func TestThingOverrideGroupLookupFn(t *testing.T) {
	h := newHandler(t, nil, &fakeHub{}, nil)
	if h.thingOverrideGroupLookupFn() != nil {
		t.Error("nil ref + nil db → nil")
	}
	stub := &stubGroupLookup{}
	h.thingOverrideGroupLookupRef = stub
	if h.thingOverrideGroupLookupFn() == nil {
		t.Error("ref set → non-nil")
	}
	h.thingOverrideGroupLookupRef = nil
	_, db := newMockDB(t)
	h.db = db
	if h.thingOverrideGroupLookupFn() == nil {
		t.Error("db set → non-nil via *store.DB fallback")
	}
}

func TestWritePreflightError_AllBranches(t *testing.T) {
	h := newHandler(t, nil, &fakeHub{}, nil)
	cases := []struct {
		name string
		r    hubPreflightResult
		want int
	}{
		{"not configured", hubPreflightResult{NotConfigured: true}, http.StatusServiceUnavailable},
		{"not found", hubPreflightResult{NotFound: true}, http.StatusNotFound},
		{"passthrough 400", hubPreflightResult{Passthrough: &hubPassthrough{Status: 400, Body: []byte(`{"error":"bad"}`)}}, http.StatusBadRequest},
		{"bad gateway", hubPreflightResult{BadGateway: errors.New("boom")}, http.StatusBadGateway},
		{"bad gateway nil err", hubPreflightResult{BadGateway: nil}, http.StatusBadGateway},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, rec := echoCtx(http.MethodGet, "/", "", true)
			if err := h.writePreflightError(c, "test", "t1", tc.r); err != nil {
				t.Fatalf("err: %v", err)
			}
			if rec.Code != tc.want {
				t.Errorf("code = %d; want %d", rec.Code, tc.want)
			}
		})
	}
}

func TestFetchThingType_HubNotConfigured(t *testing.T) {
	h := newHandler(t, nil, &fakeHub{}, nil) // baseURL empty
	c, _ := echoCtx(http.MethodGet, "/", "", true)
	r := h.fetchThingType(c, "t1")
	if !r.NotConfigured {
		t.Errorf("expected NotConfigured; got %+v", r)
	}
}

func TestFetchThingType_TransportError(t *testing.T) {
	hub := &fakeHub{baseURL: "http://127.0.0.1:1", token: "tok"}
	h := newHandler(t, nil, hub, nil)
	h.hubProxyClientRef = &http.Client{Timeout: 100 * time.Millisecond}
	c, _ := echoCtx(http.MethodGet, "/", "", true)
	r := h.fetchThingType(c, "t1")
	if r.BadGateway == nil {
		t.Errorf("expected BadGateway; got %+v", r)
	}
}

func TestFetchThingType_404(t *testing.T) {
	hub, _ := withHubServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	h := newHandler(t, nil, hub, nil)
	c, _ := echoCtx(http.MethodGet, "/", "", true)
	r := h.fetchThingType(c, "t1")
	if !r.NotFound {
		t.Errorf("expected NotFound; got %+v", r)
	}
}

func TestFetchThingType_Happy(t *testing.T) {
	hub, _ := withHubServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"type":"agent"}`))
	}))
	h := newHandler(t, nil, hub, nil)
	c, _ := echoCtx(http.MethodGet, "/", "", true)
	r := h.fetchThingType(c, "t1")
	if r.ThingType != "agent" {
		t.Errorf("expected ThingType=agent; got %+v", r)
	}
}

func TestFetchThingType_DecodeError(t *testing.T) {
	hub, _ := withHubServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{not json`))
	}))
	h := newHandler(t, nil, hub, nil)
	c, _ := echoCtx(http.MethodGet, "/", "", true)
	r := h.fetchThingType(c, "t1")
	if r.BadGateway == nil {
		t.Errorf("expected BadGateway on decode error; got %+v", r)
	}
}

func TestFetchThingType_EmptyType(t *testing.T) {
	hub, _ := withHubServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"type":""}`))
	}))
	h := newHandler(t, nil, hub, nil)
	c, _ := echoCtx(http.MethodGet, "/", "", true)
	r := h.fetchThingType(c, "t1")
	if r.BadGateway == nil {
		t.Errorf("expected BadGateway on empty type; got %+v", r)
	}
}

func TestFetchThingType_4xxPassthrough(t *testing.T) {
	hub, _ := withHubServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"hub validation"}`))
	}))
	h := newHandler(t, nil, hub, nil)
	c, _ := echoCtx(http.MethodGet, "/", "", true)
	r := h.fetchThingType(c, "t1")
	if r.Passthrough == nil || r.Passthrough.Status != 400 {
		t.Errorf("expected Passthrough 400; got %+v", r)
	}
}

func TestFetchThingType_5xx(t *testing.T) {
	hub, _ := withHubServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	h := newHandler(t, nil, hub, nil)
	c, _ := echoCtx(http.MethodGet, "/", "", true)
	r := h.fetchThingType(c, "t1")
	if r.BadGateway == nil {
		t.Errorf("expected BadGateway on 5xx; got %+v", r)
	}
}

func TestListNodeOverrides_MissingID(t *testing.T) {
	h := newHandler(t, nil, &fakeHub{}, nil)
	c, rec := echoCtxParam(http.MethodGet, "/", "", true, []string{"id"}, []string{""})
	if err := h.ListNodeOverrides(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d; want 400", rec.Code)
	}
}

func TestListNodeOverrides_Happy(t *testing.T) {
	hub, _ := withHubServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[]`))
	}))
	h := newHandler(t, nil, hub, nil)
	c, rec := echoCtxParam(http.MethodGet, "/", "", true, []string{"id"}, []string{"t-1"})
	if err := h.ListNodeOverrides(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d; want 200", rec.Code)
	}
}

func TestSetNodeOverride_MissingIDOrKey(t *testing.T) {
	h := newHandler(t, nil, &fakeHub{}, nil)
	cases := []struct {
		idVal, keyVal string
	}{
		{"", "x"},
		{"t1", ""},
		{"", ""},
	}
	for _, tc := range cases {
		c, rec := echoCtxParam(http.MethodPut, "/", `{}`, true,
			[]string{"id", "configKey"}, []string{tc.idVal, tc.keyVal})
		if err := h.SetNodeOverride(c); err != nil {
			t.Fatalf("err: %v", err)
		}
		if rec.Code != http.StatusBadRequest {
			t.Errorf("(%q,%q): code = %d; want 400", tc.idVal, tc.keyVal, rec.Code)
		}
	}
}

func TestSetNodeOverride_BodyInvalidJSON(t *testing.T) {
	h := newHandler(t, nil, &fakeHub{}, nil)
	c, rec := echoCtxParam(http.MethodPut, "/", `{not json`, true,
		[]string{"id", "configKey"}, []string{"t-1", "routing"})
	if err := h.SetNodeOverride(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d; want 400", rec.Code)
	}
}

func TestSetNodeOverride_ValidationFails(t *testing.T) {
	h := newHandler(t, nil, &fakeHub{}, nil)
	c, rec := echoCtxParam(http.MethodPut, "/", `{}`, true,
		[]string{"id", "configKey"}, []string{"t-1", "credentials"})
	if err := h.SetNodeOverride(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d; want 400 (blacklisted key)", rec.Code)
	}
}

func TestSetNodeOverride_PreflightFailure(t *testing.T) {
	// Body validates OK, but hub is not configured → preflight error.
	h := newHandler(t, nil, &fakeHub{}, nil)
	body := `{"state":{}}`
	c, rec := echoCtxParam(http.MethodPut, "/", body, true,
		[]string{"id", "configKey"}, []string{"t-1", "routing"})
	if err := h.SetNodeOverride(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("code = %d; want 503 (preflight)", rec.Code)
	}
}

func TestSetNodeOverride_RBACDenied(t *testing.T) {
	hub, _ := withHubServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"type":"ai-gateway"}`))
	}))
	stub := &stubGroupLookup{groups: []string{"compliance-team"}} // service type not allowed
	h := newHandler(t, nil, hub, nil)
	h.thingOverrideGroupLookupRef = stub
	body := `{"state":{}}`
	c, rec := echoCtxParam(http.MethodPut, "/", body, true,
		[]string{"id", "configKey"}, []string{"t-1", "routing"})
	if err := h.SetNodeOverride(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("code = %d; want 403", rec.Code)
	}
}

func TestSetNodeOverride_HappyForwardsToHub(t *testing.T) {
	// Hub server answers both the preflight GET and the override PUT.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_, _ = w.Write([]byte(`{"type":"agent"}`))
		case http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			if !bytes.Contains(body, []byte(`"state":`)) {
				t.Errorf("body not forwarded: %s", body)
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			t.Errorf("unexpected method %s", r.Method)
		}
	}))
	defer srv.Close()
	hub := &fakeHub{baseURL: srv.URL, token: "tok"}
	stub := &stubGroupLookup{groups: []string{"super-admins"}}
	h := newHandler(t, nil, hub, nil)
	h.thingOverrideGroupLookupRef = stub
	body := `{"state":{"a":1}}`
	c, rec := echoCtxParam(http.MethodPut, "/", body, true,
		[]string{"id", "configKey"}, []string{"t-1", "routing"})
	if err := h.SetNodeOverride(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d; want 200; body %s", rec.Code, rec.Body.String())
	}
}

func TestClearNodeOverride_MissingIDOrKey(t *testing.T) {
	h := newHandler(t, nil, &fakeHub{}, nil)
	c, rec := echoCtxParam(http.MethodDelete, "/", "", true,
		[]string{"id", "configKey"}, []string{"", ""})
	if err := h.ClearNodeOverride(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d; want 400", rec.Code)
	}
}

func TestClearNodeOverride_PreflightFails(t *testing.T) {
	h := newHandler(t, nil, &fakeHub{}, nil) // hub not configured
	c, rec := echoCtxParam(http.MethodDelete, "/", "", true,
		[]string{"id", "configKey"}, []string{"t-1", "routing"})
	if err := h.ClearNodeOverride(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("code = %d; want 503", rec.Code)
	}
}

func TestClearNodeOverride_RBACDenied(t *testing.T) {
	hub, _ := withHubServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"type":"agent"}`))
	}))
	stub := &stubGroupLookup{groups: []string{"provider-managers"}}
	h := newHandler(t, nil, hub, nil)
	h.thingOverrideGroupLookupRef = stub
	c, rec := echoCtxParam(http.MethodDelete, "/", "", true,
		[]string{"id", "configKey"}, []string{"t-1", "routing"})
	if err := h.ClearNodeOverride(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("code = %d; want 403", rec.Code)
	}
}

func TestClearNodeOverride_Happy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"type":"agent"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	hub := &fakeHub{baseURL: srv.URL, token: "tok"}
	stub := &stubGroupLookup{groups: []string{"super-admins"}}
	h := newHandler(t, nil, hub, nil)
	h.thingOverrideGroupLookupRef = stub
	c, rec := echoCtxParam(http.MethodDelete, "/", "", true,
		[]string{"id", "configKey"}, []string{"t-1", "routing"})
	if err := h.ClearNodeOverride(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d; want 200", rec.Code)
	}
}

func TestListGlobalOverrides_Happy(t *testing.T) {
	hub, _ := withHubServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	}))
	h := newHandler(t, nil, hub, nil)
	c, rec := echoCtx(http.MethodGet, "/", "", true)
	if err := h.ListGlobalOverrides(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d; want 200", rec.Code)
	}
}

func TestAdminResyncNode_MissingID(t *testing.T) {
	h := newHandler(t, nil, &fakeHub{}, nil)
	c, rec := echoCtxParam(http.MethodPost, "/", `{}`, true, []string{"id"}, []string{""})
	if err := h.AdminResyncNode(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d; want 400", rec.Code)
	}
}

func TestAdminResyncNode_BadBody(t *testing.T) {
	h := newHandler(t, nil, &fakeHub{}, nil)
	c, rec := echoCtxParam(http.MethodPost, "/", `{not json`, true, []string{"id"}, []string{"t-1"})
	if err := h.AdminResyncNode(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d; want 400", rec.Code)
	}
}

func TestAdminResyncNode_PreflightFails(t *testing.T) {
	h := newHandler(t, nil, &fakeHub{}, nil) // not configured
	c, rec := echoCtxParam(http.MethodPost, "/", `{}`, true, []string{"id"}, []string{"t-1"})
	if err := h.AdminResyncNode(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("code = %d; want 503", rec.Code)
	}
}

func TestAdminResyncNode_RBACDenied(t *testing.T) {
	hub, _ := withHubServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"type":"agent"}`))
	}))
	stub := &stubGroupLookup{groups: []string{"provider-managers"}}
	h := newHandler(t, nil, hub, nil)
	h.thingOverrideGroupLookupRef = stub
	c, rec := echoCtxParam(http.MethodPost, "/", `{}`, true, []string{"id"}, []string{"t-1"})
	if err := h.AdminResyncNode(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("code = %d; want 403", rec.Code)
	}
}

func TestAdminResyncNode_HappySingleKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"type":"agent"}`))
			return
		}
		body, _ := io.ReadAll(r.Body)
		if !bytes.Contains(body, []byte(`"configKey":"routing"`)) {
			t.Errorf("body forwarded with configKey: %s", string(body))
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	hub := &fakeHub{baseURL: srv.URL, token: "tok"}
	stub := &stubGroupLookup{groups: []string{"super-admins"}}
	spy := &auditSpy{}
	h := newHandler(t, nil, hub, spy)
	h.thingOverrideGroupLookupRef = stub
	c, rec := echoCtxParam(http.MethodPost, "/", `{"configKey":"routing"}`, true, []string{"id"}, []string{"t-1"})
	if err := h.AdminResyncNode(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d; want 200", rec.Code)
	}
	if spy.count() != 1 {
		t.Errorf("expected 1 audit entry; got %d", spy.count())
	}
	// Spot-check afterState carries scope=single-key when configKey was supplied.
	if last := spy.last(); last != nil {
		if state, ok := last["afterState"].(map[string]any); ok {
			if state["scope"] != "single-key" {
				t.Errorf("afterState.scope = %v; want single-key", state["scope"])
			}
		}
	}
}

func TestAdminResyncNode_HappyWholeThing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"type":"agent"}`))
			return
		}
		body, _ := io.ReadAll(r.Body)
		// Empty body → empty configKey skipped → "{}"
		if !bytes.Equal(bytes.TrimSpace(body), []byte(`{}`)) {
			t.Errorf("expected empty config map; got %s", string(body))
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	hub := &fakeHub{baseURL: srv.URL, token: "tok"}
	stub := &stubGroupLookup{groups: []string{"super-admins"}}
	spy := &auditSpy{}
	h := newHandler(t, nil, hub, spy)
	h.thingOverrideGroupLookupRef = stub
	c, rec := echoCtxParam(http.MethodPost, "/", `{}`, true, []string{"id"}, []string{"t-1"})
	if err := h.AdminResyncNode(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d; want 200", rec.Code)
	}
	if spy.count() != 1 {
		t.Errorf("expected 1 audit entry; got %d", spy.count())
	}
}

func TestAdminResyncNode_EmptyBodyAllowed(t *testing.T) {
	// AdminResyncNode allows an empty body — it's the "whole-thing replay"
	// branch. Make sure the empty-bytes io.ReadAll path runs.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"type":"agent"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	hub := &fakeHub{baseURL: srv.URL, token: "tok"}
	stub := &stubGroupLookup{groups: []string{"super-admins"}}
	h := newHandler(t, nil, hub, nil)
	h.thingOverrideGroupLookupRef = stub
	c, rec := echoCtxParam(http.MethodPost, "/", "", true, []string{"id"}, []string{"t-1"})
	if err := h.AdminResyncNode(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d; want 200", rec.Code)
	}
}

func TestAdminResyncNode_HubErrorNoAudit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"type":"agent"}`))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	hub := &fakeHub{baseURL: srv.URL, token: "tok"}
	stub := &stubGroupLookup{groups: []string{"super-admins"}}
	spy := &auditSpy{}
	h := newHandler(t, nil, hub, spy)
	h.thingOverrideGroupLookupRef = stub
	c, rec := echoCtxParam(http.MethodPost, "/", `{"configKey":"routing"}`, true, []string{"id"}, []string{"t-1"})
	if err := h.AdminResyncNode(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d; want 500", rec.Code)
	}
	if spy.count() != 0 {
		t.Errorf("expected no audit on hub error")
	}
}

// RegisterRoutes smoke test — confirms every infra route is mounted and
// gated by the supplied iamMW.

func TestRegisterRoutes_IAMDeniesUnauthenticated(t *testing.T) {
	_, db := newMockDB(t)
	h := newHandler(t, db, &fakeHub{}, nil)
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
		// node_runtime
		{http.MethodGet, "/api/admin/nodes/t-1/runtime"},
		// thing_overrides
		{http.MethodGet, "/api/admin/nodes/overrides"},
		{http.MethodGet, "/api/admin/nodes/t-1/overrides"},
		{http.MethodPut, "/api/admin/nodes/t-1/overrides/routing"},
		{http.MethodDelete, "/api/admin/nodes/t-1/overrides/routing"},
		{http.MethodPost, "/api/admin/nodes/t-1/resync"},
		// service_urls
		{http.MethodGet, "/api/admin/services/public-urls"},
		// hub_proxy nodes
		{http.MethodGet, "/api/admin/nodes"},
		{http.MethodGet, "/api/admin/nodes/t-1"},
		{http.MethodGet, "/api/admin/nodes/t-1/device-assignments"},
		// hub_proxy config-sync
		{http.MethodGet, "/api/admin/config-sync/out-of-sync"},
		{http.MethodGet, "/api/admin/config-sync/history"},
		{http.MethodGet, "/api/admin/config-sync/catalog"},
		{http.MethodPost, "/api/admin/config-sync/update"},
		// hub_proxy jobs
		{http.MethodGet, "/api/admin/jobs"},
		{http.MethodGet, "/api/admin/jobs/j-1"},
		{http.MethodGet, "/api/admin/jobs/j-1/runs"},
		{http.MethodPut, "/api/admin/jobs/j-1"},
		{http.MethodPost, "/api/admin/jobs/j-1/trigger"},
		// hub_proxy enrollment
		{http.MethodGet, "/api/admin/enrollment/tokens"},
		{http.MethodPost, "/api/admin/enrollment/token"},
		// setup
		{http.MethodGet, "/api/admin/setup/proxy/t-1/ca-cert"},
		{http.MethodGet, "/api/admin/setup/proxy/t-1/mdm-profile"},
		{http.MethodGet, "/api/admin/setup/proxy/t-1/pac-file"},
		{http.MethodPatch, "/api/admin/setup/proxy/t-1/onboarding"},
		// diag_silences
		{http.MethodGet, "/api/admin/diag-silences"},
		{http.MethodPost, "/api/admin/diag-silences"},
		{http.MethodDelete, "/api/admin/diag-silences/s-1"},
		// diagevents
		{http.MethodGet, "/api/admin/diag-events"},
		{http.MethodGet, "/api/admin/diag-events/groups"},
		{http.MethodGet, "/api/admin/diag-events/crash-cohorts"},
		// diagmode
		{http.MethodGet, "/api/admin/agents/diagnostic-mode"},
		{http.MethodPost, "/api/admin/agents/diagnostic-mode/bulk"},
		{http.MethodPost, "/api/admin/agents/t-1/diagnostic-mode"},
		{http.MethodDelete, "/api/admin/agents/t-1/diagnostic-mode"},
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

// Compile-time guards: ensure store.DB and pgxpool.Pool keep satisfying the
// expected seams so future refactors won't silently break these tests.
var _ ThingOverrideGroupLookup = (*stubGroupLookup)(nil)

// Ensure pgconn errors compile against the imported package.
var _ = &pgconn.PgError{}

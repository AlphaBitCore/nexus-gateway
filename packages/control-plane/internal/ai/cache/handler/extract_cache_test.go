// packages/control-plane/internal/ai/cache/handler/extract_cache_test.go —
// unit tests for the ExtractCacheHandler GET/PUT endpoints.
//
// Mirrors semanticcache_test.go fixture pattern:
//   - stubExtractStore: in-memory double for ExtractCacheStore
//   - fakeExtractHub: captures NotifyConfigChange calls
//
// No live DB / Hub; everything runs through Echo's httptest.

package cache_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	cache "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/ai/cache/handler"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/hub"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/configstore"
)

// newExtractAuditWriter constructs an audit.Writer backed by the spy publisher.
func newExtractAuditWriter(spy *extractAuditSpy) *audit.Writer {
	return audit.NewWriter(spy, "audit", slog.Default())
}

// ── Fixtures ────────────────────────────────────────────────────────────────

type stubExtractStore struct {
	mu      sync.Mutex
	row     *configstore.ExtractCacheConfigRow
	getErr  error
	saveErr error
	lastIn  configstore.ExtractCacheSaveInput
}

func (s *stubExtractStore) Get(_ context.Context) (*configstore.ExtractCacheConfigRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.getErr != nil {
		return nil, s.getErr
	}
	if s.row == nil {
		return &configstore.ExtractCacheConfigRow{
			ID:                  "singleton",
			Enabled:             true,
			TTLSeconds:          3600,
			ApplyFreshnessRules: true,
		}, nil
	}
	cp := *s.row
	return &cp, nil
}

func (s *stubExtractStore) Save(_ context.Context, in configstore.ExtractCacheSaveInput) (*configstore.ExtractCacheConfigRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.saveErr != nil {
		return nil, s.saveErr
	}
	s.lastIn = in
	s.row = &configstore.ExtractCacheConfigRow{
		ID:                  "singleton",
		Enabled:             in.Enabled,
		TTLSeconds:          in.TTLSeconds,
		ApplyFreshnessRules: in.ApplyFreshnessRules,
		UpdatedAt:           time.Now().UTC(),
	}
	if in.UpdatedBy != "" {
		s.row.UpdatedBy = &in.UpdatedBy
	}
	return s.row, nil
}

type fakeExtractHub struct {
	mu    sync.Mutex
	calls []hub.ConfigChangeRequest
	err   error
}

func (h *fakeExtractHub) NotifyConfigChange(_ context.Context, req hub.ConfigChangeRequest) (*hub.ConfigChangeResponse, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, req)
	if h.err != nil {
		return nil, h.err
	}
	return &hub.ConfigChangeResponse{OK: true, Version: 1, ThingsNotified: 1, ThingsOnline: 1}, nil
}

func newExtractHandler(store cache.ExtractCacheStore, hubPusher cache.ExtractCacheHubPusher) *cache.ExtractCacheHandler {
	return cache.NewExtractCacheHandler(cache.ExtractCacheHandlerDeps{
		Store:  store,
		Hub:    hubPusher,
		Audit:  nil, // tests skip audit emit
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

func mountExtractRoutes(t *testing.T, h *cache.ExtractCacheHandler) *echo.Echo {
	t.Helper()
	e := echo.New()
	g := e.Group("/api/admin")
	noop := func(_ string) echo.MiddlewareFunc {
		return func(next echo.HandlerFunc) echo.HandlerFunc { return next }
	}
	h.RegisterExtractCacheRoutes(g, noop)
	return e
}

// ── Tests ───────────────────────────────────────────────────────────────────

func TestExtractCacheGetConfig_ReturnsRow(t *testing.T) {
	store := &stubExtractStore{
		row: &configstore.ExtractCacheConfigRow{
			ID: "singleton", Enabled: false, TTLSeconds: 7200, ApplyFreshnessRules: false,
		},
	}
	e := mountExtractRoutes(t, newExtractHandler(store, nil))

	req := httptest.NewRequest(http.MethodGet, "/api/admin/extract-cache/config", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"enabled":false`) || !strings.Contains(body, `"ttlSeconds":7200`) {
		t.Errorf("body missing expected fields: %s", body)
	}
}

func TestExtractCacheGetConfig_StoreError_Returns500(t *testing.T) {
	store := &stubExtractStore{getErr: errors.New("db down")}
	e := mountExtractRoutes(t, newExtractHandler(store, nil))

	req := httptest.NewRequest(http.MethodGet, "/api/admin/extract-cache/config", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", rec.Code)
	}
}

func TestExtractCachePutConfig_Saves_PushesHub(t *testing.T) {
	store := &stubExtractStore{}
	hubStub := &fakeExtractHub{}
	e := mountExtractRoutes(t, newExtractHandler(store, hubStub))

	body := bytes.NewBufferString(`{"enabled":true,"ttlSeconds":7200,"applyFreshnessRules":false}`)
	req := httptest.NewRequest(http.MethodPut, "/api/admin/extract-cache/config", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if store.lastIn.TTLSeconds != 7200 {
		t.Errorf("ttl persisted = %d, want 7200", store.lastIn.TTLSeconds)
	}
	if store.lastIn.ApplyFreshnessRules != false {
		t.Errorf("applyFreshnessRules persisted = %v, want false", store.lastIn.ApplyFreshnessRules)
	}
	// Hub push fires with full state.
	if len(hubStub.calls) != 1 {
		t.Fatalf("hub.NotifyConfigChange call count = %d, want 1", len(hubStub.calls))
	}
	if hubStub.calls[0].ConfigKey != "response_cache.extract_config" {
		t.Errorf("hub push key = %q, want response_cache.extract_config", hubStub.calls[0].ConfigKey)
	}
	if state, ok := hubStub.calls[0].State.(map[string]any); !ok || state["ttlSeconds"] != 7200 {
		t.Errorf("hub push state malformed: %#v", hubStub.calls[0].State)
	}
}

func TestExtractCachePutConfig_OutOfRangeTTL_Returns400(t *testing.T) {
	store := &stubExtractStore{}
	e := mountExtractRoutes(t, newExtractHandler(store, nil))

	cases := []struct {
		name string
		body string
	}{
		{"too-low", `{"enabled":true,"ttlSeconds":30,"applyFreshnessRules":true}`},
		{"too-high", `{"enabled":true,"ttlSeconds":700000,"applyFreshnessRules":true}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPut, "/api/admin/extract-cache/config", bytes.NewBufferString(tc.body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("want 400, got %d: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestExtractCachePutConfig_DisabledSkipsTTLCheck(t *testing.T) {
	store := &stubExtractStore{}
	e := mountExtractRoutes(t, newExtractHandler(store, nil))

	// enabled=false → ttlSeconds out-of-range is allowed (store clamps).
	body := bytes.NewBufferString(`{"enabled":false,"ttlSeconds":0,"applyFreshnessRules":true}`)
	req := httptest.NewRequest(http.MethodPut, "/api/admin/extract-cache/config", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 (TTL guard only applies when enabled=true), got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestExtractCachePutConfig_MalformedJSON_Returns400(t *testing.T) {
	store := &stubExtractStore{}
	e := mountExtractRoutes(t, newExtractHandler(store, nil))

	req := httptest.NewRequest(http.MethodPut, "/api/admin/extract-cache/config", bytes.NewBufferString(`not json`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rec.Code)
	}
}

func TestExtractCachePutConfig_StoreError_Returns500(t *testing.T) {
	store := &stubExtractStore{saveErr: errors.New("db down")}
	e := mountExtractRoutes(t, newExtractHandler(store, nil))

	body := bytes.NewBufferString(`{"enabled":true,"ttlSeconds":3600,"applyFreshnessRules":true}`)
	req := httptest.NewRequest(http.MethodPut, "/api/admin/extract-cache/config", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", rec.Code)
	}
}

// A dropped Hub push escalates to 502 with the propagation_error envelope
// (mirrors cache.go) instead of silently returning 200, so the admin sees the
// failure immediately. The configreconcile watch for this key (F-0102/F-0345)
// additionally heals it within one cycle, so the unified message's
// auto-recovery promise holds.
func TestExtractCachePutConfig_HubErrorReturns502(t *testing.T) {
	store := &stubExtractStore{}
	hubStub := &fakeExtractHub{err: errors.New("hub down")}
	e := mountExtractRoutes(t, newExtractHandler(store, hubStub))

	body := bytes.NewBufferString(`{"enabled":true,"ttlSeconds":3600,"applyFreshnessRules":true}`)
	req := httptest.NewRequest(http.MethodPut, "/api/admin/extract-cache/config", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("want 502 (no reconcile backstop for this key), got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "propagation_error") {
		t.Errorf("missing propagation_error envelope; body=%s", rec.Body.String())
	}
}

// Exercises the nil-logger branch in NewExtractCacheHandler (which falls back
// to slog.Default()).
func TestExtractCacheHandler_NilLoggerFallsBack(t *testing.T) {
	store := &stubExtractStore{}
	h := cache.NewExtractCacheHandler(cache.ExtractCacheHandlerDeps{
		Store:  store,
		Logger: nil, // exercise the default-logger branch
	})
	if h == nil {
		t.Fatal("NewExtractCacheHandler with nil logger returned nil handler")
	}
	// Smoke: handler is usable end-to-end.
	e := echo.New()
	g := e.Group("/api/admin")
	noop := func(_ string) echo.MiddlewareFunc {
		return func(next echo.HandlerFunc) echo.HandlerFunc { return next }
	}
	h.RegisterExtractCacheRoutes(g, noop)
	req := httptest.NewRequest(http.MethodGet, "/api/admin/extract-cache/config", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("smoke GET with nil-logger handler: want 200, got %d", rec.Code)
	}
}

// Exercises the audit-emit branch in PutConfig (audit != nil).
type extractAuditSpy struct {
	mu    sync.Mutex
	calls int
}

func (a *extractAuditSpy) Publish(context.Context, string, []byte) error { return nil }
func (a *extractAuditSpy) Enqueue(_ context.Context, _ string, _ []byte) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	return nil
}
func (a *extractAuditSpy) Close() error { return nil }

func TestExtractCachePutConfig_EmitsAudit(t *testing.T) {
	store := &stubExtractStore{}
	spy := &extractAuditSpy{}
	aw := newExtractAuditWriter(spy)
	h := cache.NewExtractCacheHandler(cache.ExtractCacheHandlerDeps{
		Store:  store,
		Audit:  aw,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	e := echo.New()
	g := e.Group("/api/admin")
	noop := func(_ string) echo.MiddlewareFunc {
		return func(next echo.HandlerFunc) echo.HandlerFunc { return next }
	}
	h.RegisterExtractCacheRoutes(g, noop)

	body := bytes.NewBufferString(`{"enabled":true,"ttlSeconds":3600,"applyFreshnessRules":true}`)
	req := httptest.NewRequest(http.MethodPut, "/api/admin/extract-cache/config", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	// Audit writer drains via goroutine; give it a moment.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		spy.mu.Lock()
		n := spy.calls
		spy.mu.Unlock()
		if n > 0 {
			return // audit fired
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("audit was never emitted")
}

// Exercises the nil-hub branch in PutConfig (Hub push is skipped silently).
func TestExtractCachePutConfig_NilHubSkipsPush(t *testing.T) {
	store := &stubExtractStore{}
	e := mountExtractRoutes(t, newExtractHandler(store, nil)) // hub=nil

	body := bytes.NewBufferString(`{"enabled":true,"ttlSeconds":3600,"applyFreshnessRules":true}`)
	req := httptest.NewRequest(http.MethodPut, "/api/admin/extract-cache/config", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 (nil hub is a fire-and-forget no-op), got %d", rec.Code)
	}
	if store.lastIn.TTLSeconds != 3600 {
		t.Errorf("DB still updated even when hub is nil; ttl = %d", store.lastIn.TTLSeconds)
	}
}

func TestExtractCacheRoutes_GatedByIAMAction(t *testing.T) {
	// Verify the right IAM actions are referenced (the noop middleware in
	// our fixture doesn't enforce; we test the action strings are right by
	// confirming the resource constant resolves).
	if got := iam.ResourceExtractCache.Action(iam.VerbRead); got != "admin:extract-cache.read" {
		t.Errorf("read action = %q, want admin:extract-cache.read", got)
	}
	if got := iam.ResourceExtractCache.Action(iam.VerbUpdate); got != "admin:extract-cache.update" {
		t.Errorf("update action = %q, want admin:extract-cache.update", got)
	}
}

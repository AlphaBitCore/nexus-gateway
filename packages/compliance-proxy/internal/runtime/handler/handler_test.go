package handler

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/proxy/conn"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/runtime/config"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/runtime/killswitch"
)

// mockConnManager implements config.ConnManagerReader for testing.
type mockConnManager struct {
	count int64
	conns []conn.ConnInfo
}

func (m *mockConnManager) ActiveCount() int64 { return m.count }
func (m *mockConnManager) ActiveConnections() []conn.ConnInfo {
	if m.conns == nil {
		return []conn.ConnInfo{}
	}
	return m.conns
}

// newTestDeps builds a RuntimeDeps with sensible defaults for handler tests.
func newTestDeps(t *testing.T) RuntimeDeps {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	readiness := &atomic.Bool{}
	readiness.Store(true)
	ks := killswitch.NewKillSwitch(logger)
	return RuntimeDeps{
		KillSwitch:  ks,
		ConnManager: &mockConnManager{count: 3},
		StartTime:   time.Now().Add(-30 * time.Second),
		Logger:      logger,
		Readiness:   readiness,
	}
}

// TestHandleHealthz_Ready covers the happy path:
//   - Method=GET, readiness=true, kill-switch disabled → 200 + status:"ok"
//   - redisChecker=nil → redisConnected:false (safe fallback, no panic)
func TestHandleHealthz_Ready(t *testing.T) {
	deps := newTestDeps(t)
	// No redis checker — must default to false, not panic.
	deps.RedisChecker = nil

	h := HandleHealthz(deps)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body HealthzResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Status != "ok" {
		t.Errorf("status: got %q, want ok", body.Status)
	}
	if body.RedisConnected {
		t.Error("redisConnected should be false when no checker is wired")
	}
	if body.UptimeSeconds < 30 {
		t.Errorf("uptimeSeconds = %v, want >= 30", body.UptimeSeconds)
	}
}

// TestHandleHealthz_RedisCheckerReturnsTrue verifies the redis checker is
// called and its result forwarded into the response body.
func TestHandleHealthz_RedisCheckerReturnsTrue(t *testing.T) {
	deps := newTestDeps(t)
	deps.RedisChecker = func() bool { return true }

	h := HandleHealthz(deps)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	var body HealthzResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.RedisConnected {
		t.Error("redisConnected should be true when checker returns true")
	}
}

// TestHandleHealthz_ShuttingDown covers the readiness=false branch.
// During graceful shutdown, the readiness flag is cleared so /healthz
// returns 503 + status:"shutting_down". Load balancers drain on 503.
func TestHandleHealthz_ShuttingDown(t *testing.T) {
	deps := newTestDeps(t)
	deps.Readiness.Store(false)

	h := HandleHealthz(deps)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	var body HealthzResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Status != "shutting_down" {
		t.Errorf("status: got %q, want shutting_down", body.Status)
	}
}

// TestHandleHealthz_NilReadiness covers the `deps.Readiness == nil` branch.
// When the readiness flag is not wired (should not happen in prod but
// defensively guarded), the handler must treat the proxy as ready and
// return 200 — not panic on nil.Load().
func TestHandleHealthz_NilReadiness(t *testing.T) {
	deps := newTestDeps(t)
	deps.Readiness = nil

	h := HandleHealthz(deps)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("nil Readiness: status = %d, want 200", rec.Code)
	}
	var body HealthzResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Status != "ok" {
		t.Errorf("nil Readiness: status = %q, want ok", body.Status)
	}
}

// TestHandleHealthz_BumpDisabledByKillSwitch verifies that when the
// kill-switch is enabled, BumpEnabled=false in the response — callers
// monitoring the bump status detect this without parsing logs.
func TestHandleHealthz_BumpDisabledByKillSwitch(t *testing.T) {
	deps := newTestDeps(t)
	deps.KillSwitch.Toggle(true, "test")

	h := HandleHealthz(deps)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	var body HealthzResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.BumpEnabled {
		t.Error("BumpEnabled should be false when kill-switch is enabled")
	}
}

// TestHandleHealthz_MethodNotAllowed covers the POST→405 branch.
// Named failure mode: callers that accidentally POST to /healthz must get a
// clear 405, not a 200 with bogus data or a 500.
func TestHandleHealthz_MethodNotAllowed(t *testing.T) {
	deps := newTestDeps(t)
	h := HandleHealthz(deps)
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(method, "/healthz", nil))
			if rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s: status = %d, want 405", method, rec.Code)
			}
		})
	}
}

// TestHandleConnections_NoFilter returns all connections when no targetHost
// filter is specified.
func TestHandleConnections_NoFilter(t *testing.T) {
	deps := newTestDeps(t)
	deps.ConnManager = &mockConnManager{
		count: 2,
		conns: []conn.ConnInfo{
			{ID: "c1", SourceIP: "10.0.0.1", TargetHost: "api.openai.com:443"},
			{ID: "c2", SourceIP: "10.0.0.2", TargetHost: "api.anthropic.com:443"},
		},
	}

	h := HandleConnections(deps)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/connections", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body ConnectionsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Total != 2 {
		t.Errorf("Total = %d, want 2", body.Total)
	}
}

// TestHandleConnections_Filter covers the targetHost query-parameter filter.
// Only connections whose TargetHost matches the filter should be returned.
func TestHandleConnections_Filter(t *testing.T) {
	deps := newTestDeps(t)
	deps.ConnManager = &mockConnManager{
		count: 3,
		conns: []conn.ConnInfo{
			{ID: "c1", TargetHost: "api.openai.com:443"},
			{ID: "c2", TargetHost: "api.anthropic.com:443"},
			{ID: "c3", TargetHost: "api.openai.com:443"},
		},
	}

	h := HandleConnections(deps)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/connections?targetHost=api.openai.com:443", nil)
	h.ServeHTTP(rec, req)

	var body ConnectionsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Total != 2 {
		t.Errorf("Total = %d, want 2 (only openai connections)", body.Total)
	}
	for _, c := range body.Connections {
		if c.TargetHost != "api.openai.com:443" {
			t.Errorf("unexpected connection in filtered result: %+v", c)
		}
	}
}

// TestHandleConnections_NilConnList covers the `if conns == nil` branch —
// when ActiveConnections returns nil, the response must use an empty slice
// (not null) so JSON clients can iterate without a nil-check.
func TestHandleConnections_NilConnList(t *testing.T) {
	deps := newTestDeps(t)
	deps.ConnManager = &mockConnManager{count: 0, conns: nil}

	h := HandleConnections(deps)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/connections", nil))

	var body struct {
		Connections []conn.ConnInfo `json:"connections"`
		Total       int             `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Connections == nil {
		t.Error("connections must be [] not null when manager returns nil")
	}
	if body.Total != 0 {
		t.Errorf("Total = %d, want 0", body.Total)
	}
}

// TestHandleConnections_MethodNotAllowed covers the POST→405 branch.
func TestHandleConnections_MethodNotAllowed(t *testing.T) {
	deps := newTestDeps(t)
	h := HandleConnections(deps)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/connections", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
}

// WriteJSON (config package, exercised as part of handler coverage)

// TestWriteJSON_SetsContentTypeAndStatus verifies the shared WriteJSON helper
// sets Content-Type and the given status code correctly.
func TestWriteJSON_SetsContentTypeAndStatus(t *testing.T) {
	rec := httptest.NewRecorder()
	config.WriteJSON(rec, http.StatusCreated, map[string]string{"k": "v"})
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("status = %d, want 201", rec.Code)
	}
}

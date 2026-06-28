package server

import (
	"github.com/goccy/go-json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/proxy/conn"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/runtime/auth"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/runtime/breakglass"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/runtime/handler"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/runtime/killswitch"
)

// mockConnManager implements handler.ConnManagerReader for testing.
type mockConnManager struct {
	count int64
	conns []conn.ConnInfo
}

func (m *mockConnManager) ActiveCount() int64 {
	return m.count
}

func (m *mockConnManager) ActiveConnections() []conn.ConnInfo {
	if m.conns == nil {
		return []conn.ConnInfo{}
	}
	return m.conns
}

// newTestDeps creates handler.RuntimeDeps suitable for testing.
func newTestDeps() handler.RuntimeDeps {
	readiness := &atomic.Bool{}
	readiness.Store(true)
	return handler.RuntimeDeps{
		KillSwitch:   killswitch.NewKillSwitch(slog.New(slog.NewTextHandler(os.Stderr, nil))),
		ConnManager:  &mockConnManager{count: 42},
		StartTime:    time.Now().Add(-10 * time.Second),
		RedisChecker: func() bool { return true },
		Logger:       slog.New(slog.NewTextHandler(os.Stderr, nil)),
		Readiness:    readiness,
	}
}

// testToken is the bearer token used for all auth-gated endpoint calls in tests.
const testToken = "test-token"

// newTestServer creates a test server with testToken as the runtime API token.
// Tests that hit auth-gated endpoints must include Authorization: Bearer test-token.
func newTestServer(t *testing.T, deps handler.RuntimeDeps) *httptest.Server {
	t.Helper()
	tokenAuth := auth.NewTokenAuth(testToken)
	srv := NewServer(":0", deps, tokenAuth)
	return httptest.NewServer(srv.httpServer.Handler)
}

// bearerReq returns a new request with the test bearer token attached.
func bearerReq(t *testing.T, method, url string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatalf("bearerReq: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	return req
}

func TestServer_Healthz(t *testing.T) {
	deps := newTestDeps()
	ts := newTestServer(t, deps)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz failed: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}

	var body handler.HealthzResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if body.Status != "ok" {
		t.Errorf("expected status 'ok', got %q", body.Status)
	}
	if body.ConnectionsActive != 42 {
		t.Errorf("expected 42 active connections, got %d", body.ConnectionsActive)
	}
	if !body.BumpEnabled {
		t.Error("expected bumpEnabled=true")
	}
	if !body.RedisConnected {
		t.Error("expected redisConnected=true")
	}
	if body.UptimeSeconds < 10 {
		t.Errorf("expected uptime >= 10s, got %.2f", body.UptimeSeconds)
	}
}

func TestServer_Connections(t *testing.T) {
	deps := newTestDeps()
	ts := newTestServer(t, deps)
	defer ts.Close()

	resp, err := http.DefaultClient.Do(bearerReq(t, http.MethodGet, ts.URL+"/connections"))
	if err != nil {
		t.Fatalf("GET /connections failed: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}

	var body handler.ConnectionsResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if body.Total != 0 {
		t.Errorf("expected total=0 (no active connections in mock), got %d", body.Total)
	}
	if len(body.Connections) != 0 {
		t.Errorf("expected empty connections list, got %d items", len(body.Connections))
	}
}

// TestServer_Healthz_MethodNotAllowed covers the
// `if r.Method != http.MethodGet` branch in HandleHealthz — POST/PUT
// to /healthz must surface 405, not crash.
func TestServer_Healthz_MethodNotAllowed(t *testing.T) {
	ts := newTestServer(t, newTestDeps())
	defer ts.Close()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/healthz", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", resp.StatusCode)
	}
}

// TestServer_Healthz_ShuttingDown covers the `ready=false` branch —
// during graceful shutdown the readiness flag flips and /healthz
// should return 503 + status:shutting_down so the load balancer
// drains.
func TestServer_Healthz_ShuttingDown(t *testing.T) {
	deps := newTestDeps()
	deps.Readiness.Store(false)
	ts := newTestServer(t, deps)
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", resp.StatusCode)
	}
	var body handler.HealthzResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Status != "shutting_down" {
		t.Errorf("status: got %q, want shutting_down", body.Status)
	}
}

// TestServer_Connections_TargetHostFilter covers the
// `if filter := r.URL.Query().Get("targetHost"); filter != ""` branch
// in HandleConnections. Two connections to different hosts; the
// filter should return only the matching one.
func TestServer_Connections_TargetHostFilter(t *testing.T) {
	deps := newTestDeps()
	deps.ConnManager = &mockConnManager{
		count: 2,
		conns: []conn.ConnInfo{
			{ID: "c1", SourceIP: "10.0.0.1", TargetHost: "api.openai.com:443"},
			{ID: "c2", SourceIP: "10.0.0.2", TargetHost: "api.anthropic.com:443"},
		},
	}
	ts := newTestServer(t, deps)
	defer ts.Close()
	resp, err := http.DefaultClient.Do(bearerReq(t, http.MethodGet, ts.URL+"/connections?targetHost=api.openai.com:443"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close() //nolint:errcheck
	var body handler.ConnectionsResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Total != 1 {
		t.Fatalf("filtered total: got %d, want 1", body.Total)
	}
	if body.Connections[0].ID != "c1" {
		t.Errorf("filtered conn id: got %q, want c1", body.Connections[0].ID)
	}
}

// TestServer_Connections_MethodNotAllowed covers the
// `if r.Method != http.MethodGet` branch in HandleConnections.
// Auth is required so the POST reaches the handler (without a bearer token
// the middleware would intercept with 401 before the method check fires).
func TestServer_Connections_MethodNotAllowed(t *testing.T) {
	ts := newTestServer(t, newTestDeps())
	defer ts.Close()
	req := bearerReq(t, http.MethodPost, ts.URL+"/connections")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", resp.StatusCode)
	}
}

// TestEventLog_Path covers the trivial Path() accessor — the
// break-glass handler logs the path on every append and tests pin
// behavior against it.
func TestEventLog_Path(t *testing.T) {
	el := breakglass.NewEventLog("/tmp/test-ev")
	want := "/tmp/test-ev/break_glass_events.jsonl"
	if got := el.Path(); got != want {
		t.Errorf("Path = %q, want %q", got, want)
	}
}

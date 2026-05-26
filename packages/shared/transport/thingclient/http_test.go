package thingclient

import (
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

	"github.com/prometheus/client_golang/prometheus"
)

func newHTTPTestClient(t *testing.T, serverURL string) *Client {
	t.Helper()
	reg := prometheus.NewRegistry()
	c, err := New(Config{
		HubURL:            "ws://dummy:9999/ws",
		HubHTTPURL:        serverURL,
		ThingType:         "ai-gateway",
		ThingID:           "test-thing-001",
		ThingVersion:      "1.0.0",
		Token:             "test-token",
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		MetricsRegisterer: reg,
	})
	if err != nil {
		t.Fatalf("newTestClient: %v", err)
	}
	return c
}

func TestHTTPRegister_Success(t *testing.T) {
	want := registerResponse{
		ThingID: "test-thing-001",
		Desired: map[string]ConfigState{
			"routing": {State: json.RawMessage(`{"rules":[]}`), Version: 1},
		},
		DesiredVer: 5,
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/internal/things/register" {
			t.Errorf("path = %q, want /api/internal/things/register", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q, want %q", got, "Bearer test-token")
		}

		var req registerRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request body: %v", err)
			return
		}
		if req.ID != "test-thing-001" {
			t.Errorf("req.ID = %q, want %q", req.ID, "test-thing-001")
		}
		if req.Type != "ai-gateway" {
			t.Errorf("req.Type = %q, want %q", req.Type, "ai-gateway")
		}
		if req.Version != "1.0.0" {
			t.Errorf("req.Version = %q, want %q", req.Version, "1.0.0")
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(want)
	}))
	defer srv.Close()

	c := newHTTPTestClient(t, srv.URL)

	resp, err := c.httpRegister(context.Background())
	if err != nil {
		t.Fatalf("httpRegister() error: %v", err)
	}
	if resp.ThingID != want.ThingID {
		t.Errorf("ThingID = %q, want %q", resp.ThingID, want.ThingID)
	}
	if resp.DesiredVer != want.DesiredVer {
		t.Errorf("DesiredVer = %d, want %d", resp.DesiredVer, want.DesiredVer)
	}
	if len(resp.Desired) != 1 {
		t.Fatalf("len(Desired) = %d, want 1", len(resp.Desired))
	}
	if _, ok := resp.Desired["routing"]; !ok {
		t.Error("Desired missing key 'routing'")
	}
}

func TestHTTPRegister_Failure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("internal error"))
	}))
	defer srv.Close()

	c := newHTTPTestClient(t, srv.URL)

	resp, err := c.httpRegister(context.Background())
	if err == nil {
		t.Fatal("expected error for HTTP 500 response")
	}
	if resp != nil {
		t.Errorf("expected nil response on failure, got %+v", resp)
	}
	if !strings.Contains(err.Error(), "HTTP 500") {
		t.Errorf("error = %q, want substring %q", err.Error(), "HTTP 500")
	}
}

func TestHTTPHeartbeat_InSync(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/internal/things/heartbeat" {
			t.Errorf("path = %q, want /api/internal/things/heartbeat", r.URL.Path)
		}

		var req heartbeatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		if req.ReportedVer != 5 {
			t.Errorf("ReportedVer = %d, want 5", req.ReportedVer)
		}
		if req.Status != "online" {
			t.Errorf("Status = %q, want %q", req.Status, "online")
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(heartbeatResponse{
			Ack:        true,
			DesiredVer: 5,
		})
	}))
	defer srv.Close()

	c := newHTTPTestClient(t, srv.URL)
	c.reportedVer.Store(5)

	configApplied := false
	c.OnConfigChanged(func(desired map[string]ConfigState) (map[string]ConfigState, error) {
		configApplied = true
		return desired, nil
	})

	resp, err := c.httpHeartbeat(context.Background())
	if err != nil {
		t.Fatalf("httpHeartbeat() error: %v", err)
	}
	if !resp.Ack {
		t.Error("Ack = false, want true")
	}
	if resp.DesiredVer != 5 {
		t.Errorf("DesiredVer = %d, want 5", resp.DesiredVer)
	}

	// Replicate the runHTTPFallback conditional: versions match, so no apply.
	if resp.DesiredVer > c.reportedVer.Load() {
		c.applyConfig(resp.Desired, resp.DesiredVer)
	}
	if configApplied {
		t.Error("config callback should not be invoked when versions match")
	}
}

func TestHTTPHeartbeat_ConfigChanged(t *testing.T) {
	desiredConfig := map[string]ConfigState{
		"routing": {State: json.RawMessage(`{"rules":[{"path":"/v1"}]}`), Version: 2},
	}

	var (
		mu             sync.Mutex
		shadowReceived shadowRequest
		shadowCalled   bool
	)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/internal/things/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(heartbeatResponse{
			Ack:        true,
			DesiredVer: 10,
			Desired:    desiredConfig,
		})
	})
	mux.HandleFunc("/api/internal/things/shadow", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		shadowCalled = true
		_ = json.NewDecoder(r.Body).Decode(&shadowReceived)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newHTTPTestClient(t, srv.URL)
	c.reportedVer.Store(1)
	c.setMode(ModeHTTPFallback)

	var callbackDesired map[string]ConfigState
	c.OnConfigChanged(func(desired map[string]ConfigState) (map[string]ConfigState, error) {
		callbackDesired = desired
		return desired, nil
	})

	resp, err := c.httpHeartbeat(context.Background())
	if err != nil {
		t.Fatalf("httpHeartbeat() error: %v", err)
	}

	// Replicate runHTTPFallback: higher desiredVer with inline desired → apply.
	if resp.DesiredVer > c.reportedVer.Load() {
		if resp.Desired != nil {
			c.desiredVer.Store(resp.DesiredVer)
			c.applyConfig(resp.Desired, resp.DesiredVer)
		}
	}

	if callbackDesired == nil {
		t.Fatal("config callback was not invoked")
	}
	if _, ok := callbackDesired["routing"]; !ok {
		t.Error("callback desired missing key 'routing'")
	}
	if c.reportedVer.Load() != 10 {
		t.Errorf("reportedVer = %d, want 10", c.reportedVer.Load())
	}

	mu.Lock()
	defer mu.Unlock()
	if !shadowCalled {
		t.Fatal("shadow report was not sent")
	}
	if shadowReceived.ID != "test-thing-001" {
		t.Errorf("shadow ID = %q, want %q", shadowReceived.ID, "test-thing-001")
	}
	if shadowReceived.ReportedVer != 10 {
		t.Errorf("shadow ReportedVer = %d, want 10", shadowReceived.ReportedVer)
	}
}

func TestHTTPHeartbeat_VersionMismatchPull(t *testing.T) {
	pullConfig := map[string]ConfigState{
		"quota":   {State: json.RawMessage(`{"limit":1000}`), Version: 3},
		"routing": {State: json.RawMessage(`{"rules":[]}`), Version: 3},
	}

	var (
		mu         sync.Mutex
		pullCalled bool
	)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/internal/things/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(heartbeatResponse{
			Ack:        true,
			DesiredVer: 8,
		})
	})
	mux.HandleFunc("/api/internal/things/config", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		pullCalled = true
		mu.Unlock()

		if r.Method != http.MethodGet {
			t.Errorf("config pull method = %q, want GET", r.Method)
		}
		if got := r.URL.Query().Get("type"); got != "ai-gateway" {
			t.Errorf("config pull type param = %q, want %q", got, "ai-gateway")
		}
		if got := r.URL.Query().Get("id"); got != "test-thing-001" {
			t.Errorf("config pull id param = %q, want test-thing-001", got)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(configPullResponse{
			Configs:    pullConfig,
			DesiredVer: 8,
		})
	})
	mux.HandleFunc("/api/internal/things/shadow", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newHTTPTestClient(t, srv.URL)
	c.reportedVer.Store(2)
	c.setMode(ModeHTTPFallback)

	var callbackDesired map[string]ConfigState
	c.OnConfigChanged(func(desired map[string]ConfigState) (map[string]ConfigState, error) {
		callbackDesired = desired
		return desired, nil
	})

	resp, err := c.httpHeartbeat(context.Background())
	if err != nil {
		t.Fatalf("httpHeartbeat() error: %v", err)
	}

	// Replicate runHTTPFallback: higher desiredVer with nil desired → config pull.
	if resp.DesiredVer > c.reportedVer.Load() {
		if resp.Desired != nil {
			c.desiredVer.Store(resp.DesiredVer)
			c.applyConfig(resp.Desired, resp.DesiredVer)
		} else {
			pullResp, err := c.httpConfigPull(context.Background())
			if err != nil {
				t.Fatalf("httpConfigPull() error: %v", err)
			}
			c.desiredVer.Store(pullResp.DesiredVer)
			c.applyConfig(pullResp.Configs, pullResp.DesiredVer)
		}
	}

	mu.Lock()
	if !pullCalled {
		t.Fatal("config pull was not invoked")
	}
	mu.Unlock()

	if callbackDesired == nil {
		t.Fatal("config callback was not invoked after pull")
	}
	if len(callbackDesired) != 2 {
		t.Errorf("len(callbackDesired) = %d, want 2", len(callbackDesired))
	}
	if c.reportedVer.Load() != 8 {
		t.Errorf("reportedVer = %d, want 8", c.reportedVer.Load())
	}
}

func TestHTTPShadowReport_Success(t *testing.T) {
	var (
		mu       sync.Mutex
		received shadowRequest
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/internal/things/shadow" {
			t.Errorf("path = %q, want /api/internal/things/shadow", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.Method)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", r.Header.Get("Content-Type"))
		}

		mu.Lock()
		_ = json.NewDecoder(r.Body).Decode(&received)
		mu.Unlock()

		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newHTTPTestClient(t, srv.URL)

	reported := map[string]ConfigState{
		"routing": {State: json.RawMessage(`{"applied":true}`), Version: 3},
		"quota":   {State: json.RawMessage(`{"limit":500}`), Version: 3},
	}
	err := c.httpShadowReport(context.Background(), reported, 7)
	if err != nil {
		t.Fatalf("httpShadowReport() error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if received.ID != "test-thing-001" {
		t.Errorf("ID = %q, want %q", received.ID, "test-thing-001")
	}
	if received.ReportedVer != 7 {
		t.Errorf("ReportedVer = %d, want 7", received.ReportedVer)
	}
	if len(received.Reported) != 2 {
		t.Fatalf("len(Reported) = %d, want 2", len(received.Reported))
	}
	for _, key := range []string{"routing", "quota"} {
		if _, ok := received.Reported[key]; !ok {
			t.Errorf("Reported missing key %q", key)
		}
	}
}

func TestHTTPConfigPull_Success(t *testing.T) {
	want := configPullResponse{
		Configs: map[string]ConfigState{
			"routing": {State: json.RawMessage(`{"rules":[]}`), Version: 4},
			"quota":   {State: json.RawMessage(`{"limit":500}`), Version: 4},
		},
		DesiredVer: 4,
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %q, want GET", r.Method)
		}
		if !strings.HasPrefix(r.URL.Path, "/api/internal/things/config") {
			t.Errorf("path = %q, want prefix /api/internal/things/config", r.URL.Path)
		}
		if got := r.URL.Query().Get("type"); got != "ai-gateway" {
			t.Errorf("type = %q, want %q", got, "ai-gateway")
		}
		if got := r.URL.Query().Get("id"); got != "test-thing-001" {
			t.Errorf("id = %q, want test-thing-001", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q, want %q", got, "Bearer test-token")
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(want)
	}))
	defer srv.Close()

	c := newHTTPTestClient(t, srv.URL)

	resp, err := c.httpConfigPull(context.Background())
	if err != nil {
		t.Fatalf("httpConfigPull() error: %v", err)
	}
	if resp.DesiredVer != want.DesiredVer {
		t.Errorf("DesiredVer = %d, want %d", resp.DesiredVer, want.DesiredVer)
	}
	if len(resp.Configs) != 2 {
		t.Fatalf("len(Configs) = %d, want 2", len(resp.Configs))
	}
	for key := range want.Configs {
		if _, ok := resp.Configs[key]; !ok {
			t.Errorf("Configs missing key %q", key)
		}
	}
}

func TestHTTPDeregister_BestEffort(t *testing.T) {
	t.Run("connection_refused", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		srv.Close()

		c := newHTTPTestClient(t, srv.URL)
		c.httpDeregister(context.Background())
	})

	t.Run("server_error_status", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/internal/things/deregister" {
				t.Errorf("path = %q, want /api/internal/things/deregister", r.URL.Path)
			}
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()

		c := newHTTPTestClient(t, srv.URL)
		c.httpDeregister(context.Background())
	})

	t.Run("context_cancelled", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		c := newHTTPTestClient(t, srv.URL)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		c.httpDeregister(ctx)
	})
}

func TestHTTPFallback_Entry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(registerResponse{
			ThingID:    "test-thing-001",
			DesiredVer: 0,
		})
	}))
	defer srv.Close()

	c := newHTTPTestClient(t, srv.URL)

	c.wsConsecutiveFailures.Store(int32(c.cfg.WSFailureThreshold))
	if int(c.wsConsecutiveFailures.Load()) < c.cfg.WSFailureThreshold {
		t.Fatal("WS failure threshold should be met")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	c.runHTTPFallback(ctx)

	if got := c.Mode(); got != ModeHTTPFallback {
		t.Errorf("Mode() = %v, want %v", got, ModeHTTPFallback)
	}
}

func TestHTTPFallback_NoReapplyOnSync(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(registerResponse{
			ThingID: "test-thing-001",
			Desired: map[string]ConfigState{
				"routing": {State: json.RawMessage(`{"rules":[]}`), Version: 5},
			},
			DesiredVer: 5,
		})
	}))
	defer srv.Close()

	c := newHTTPTestClient(t, srv.URL)
	c.reportedVer.Store(5)
	c.desiredVer.Store(5)

	configApplied := false
	c.OnConfigChanged(func(desired map[string]ConfigState) (map[string]ConfigState, error) {
		configApplied = true
		return desired, nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	c.runHTTPFallback(ctx)

	if configApplied {
		t.Error("config should not be reapplied when versions are already in sync")
	}
}

// TestHTTPFallback_WSRecoveryUsesBackoff verifies that runHTTPFallback retries
// WebSocket recovery using exponential backoff (not a fixed-interval ticker).
// Each failed attempt should wait longer than the previous, capped at
// ReconnectMaxBackoff.
func TestHTTPFallback_WSRecoveryUsesBackoff(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/internal/things/register", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(registerResponse{
			ThingID:    "test-thing-001",
			DesiredVer: 0,
		})
	})
	mux.HandleFunc("/api/internal/things/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(heartbeatResponse{
			Ack:        true,
			DesiredVer: 0,
		})
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	reg := prometheus.NewRegistry()
	c, err := New(Config{
		HubURL:                  "ws://dummy:9999/ws",
		HubHTTPURL:              srv.URL,
		ThingType:               "ai-gateway",
		ThingID:                 "test-thing-001",
		ThingVersion:            "1.0.0",
		Token:                   "test-token",
		Logger:                  slog.New(slog.NewTextHandler(io.Discard, nil)),
		MetricsRegisterer:       reg,
		ReconnectInitialBackoff: 10 * time.Millisecond,
		ReconnectMaxBackoff:     200 * time.Millisecond,
		HeartbeatInterval:       10 * time.Second, // large: should not fire during test
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var (
		mu       sync.Mutex
		attempts []time.Time
	)
	c.connectWSFn = func(ctx context.Context) error {
		mu.Lock()
		attempts = append(attempts, time.Now())
		mu.Unlock()
		return errAlwaysFail
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	c.runHTTPFallback(ctx)

	mu.Lock()
	defer mu.Unlock()

	if len(attempts) < 3 {
		t.Fatalf("want >= 3 WS recovery attempts, got %d", len(attempts))
	}

	gap1 := attempts[1].Sub(attempts[0])
	gap2 := attempts[2].Sub(attempts[1])

	// calculateBackoffFor returns base*2^(n-1) plus up to 25% additive jitter,
	// so gap1 ∈ [base, 1.25·base] and gap2 ∈ [2·base, 2.5·base]. The worst-case
	// ratio gap2/gap1 is 2/1.25 = 1.6, so require 1.4× to leave 0.2 safety
	// margin and avoid flakes when the first gap drew near its jitter max.
	if gap2*10 < gap1*14 {
		t.Errorf("backoff did not grow: gap1=%v gap2=%v (want gap2 >= 1.4*gap1)", gap1, gap2)
	}
}

var errAlwaysFail = errors.New("connect failed")

func TestDeriveHTTPURL(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "wss_with_path",
			input: "wss://hub.nexus.internal:3060/ws",
			want:  "https://hub.nexus.internal:3060",
		},
		{
			name:  "ws_with_path",
			input: "ws://localhost:3060/ws",
			want:  "http://localhost:3060",
		},
		{
			name:  "wss_no_path",
			input: "wss://hub.nexus.internal:3060",
			want:  "https://hub.nexus.internal:3060",
		},
		{
			name:  "ws_no_path",
			input: "ws://localhost:3060",
			want:  "http://localhost:3060",
		},
		{
			name:  "wss_deep_path",
			input: "wss://hub.nexus.internal:3060/ws/v2/connect",
			want:  "https://hub.nexus.internal:3060",
		},
		{
			name:  "ws_no_port",
			input: "ws://hub.nexus.internal/ws",
			want:  "http://hub.nexus.internal",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := deriveHTTPURL(tt.input)
			if got != tt.want {
				t.Errorf("deriveHTTPURL(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

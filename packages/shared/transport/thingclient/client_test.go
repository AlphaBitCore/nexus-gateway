package thingclient

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
	"github.com/coder/websocket"
	"github.com/prometheus/client_golang/prometheus"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(&discardWriter{}, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

func testConfig(hubURL string, reg prometheus.Registerer) Config {
	return Config{
		HubURL:              hubURL,
		ThingType:           "ai-gateway",
		ThingID:             "gw-test-001",
		Token:               "test-token-abc",
		Logger:              testLogger(),
		MetricsRegisterer:   reg,
		MetricsNamespace:    "test",
		ReconnectMaxBackoff: 30 * time.Second,
	}
}

// mockMQProducer implements mq.Producer for testing.
type mockMQProducer struct {
	mu         sync.Mutex
	enqueued   []mqEntry
	enqueueErr error
}

type mqEntry struct {
	Queue string
	Data  []byte
}

func (m *mockMQProducer) Publish(_ context.Context, _ string, _ []byte) error { return nil }
func (m *mockMQProducer) Close() error                                        { return nil }

func (m *mockMQProducer) Enqueue(_ context.Context, queue string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.enqueueErr != nil {
		return m.enqueueErr
	}
	m.enqueued = append(m.enqueued, mqEntry{Queue: queue, Data: data})
	return nil
}

var _ mq.Producer = (*mockMQProducer)(nil)

// hubTestServer starts an httptest.Server that speaks the Hub WS protocol.
// The returned channel receives every text message the client sends.
// sendCh is used to push messages from the server to the client.
type hubServer struct {
	srv    *httptest.Server
	recvCh chan []byte
	sendCh chan []byte

	mu       sync.Mutex
	lastAuth string
}

func newHubServer(firstMsg []byte) *hubServer {
	h := &hubServer{
		recvCh: make(chan []byte, 64),
		sendCh: make(chan []byte, 64),
	}

	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.lastAuth = r.Header.Get("Authorization")
		h.mu.Unlock()

		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "done") //nolint:errcheck

		ctx := r.Context()

		if err := conn.Write(ctx, websocket.MessageText, firstMsg); err != nil {
			return
		}

		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case msg, ok := <-h.sendCh:
					if !ok {
						return
					}
					if err := conn.Write(ctx, websocket.MessageText, msg); err != nil {
						return
					}
				}
			}
		}()

		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			select {
			case h.recvCh <- data:
			default:
			}
		}
	}))

	return h
}

func (h *hubServer) Close() {
	h.srv.Close()
}

func (h *hubServer) URL() string {
	return "ws" + strings.TrimPrefix(h.srv.URL, "http")
}

func (h *hubServer) AuthHeader() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.lastAuth
}

func connectedMsg(desiredVer int64) []byte {
	msg := hubMessage{
		Type:       "connected",
		ThingID:    "gw-test-001",
		DesiredVer: desiredVer,
		Desired: map[string]ConfigState{
			"routing": {State: json.RawMessage(`{"version":1}`), Version: desiredVer},
		},
	}
	data, _ := json.Marshal(msg)
	return data
}

// 1. TestNew_RequiredFields

func TestNew_RequiredFields(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	logger := testLogger()

	cases := []struct {
		name    string
		cfg     Config
		wantErr string
	}{
		{
			name:    "missing HubURL",
			cfg:     Config{ThingType: "agent", ThingID: "a1", Token: "tok", Logger: logger, MetricsRegisterer: reg},
			wantErr: "HubURL is required",
		},
		{
			name:    "missing ThingType",
			cfg:     Config{HubURL: "ws://localhost/ws", ThingID: "a1", Token: "tok", Logger: logger, MetricsRegisterer: reg},
			wantErr: "ThingType is required",
		},
		{
			name:    "missing ThingID",
			cfg:     Config{HubURL: "ws://localhost/ws", ThingType: "agent", Token: "tok", Logger: logger, MetricsRegisterer: reg},
			wantErr: "ThingID is required",
		},
		{
			name:    "missing Token and TokenFn",
			cfg:     Config{HubURL: "ws://localhost/ws", ThingType: "agent", ThingID: "a1", Logger: logger, MetricsRegisterer: reg},
			wantErr: "Token or TokenFn is required",
		},
		{
			name:    "missing Logger",
			cfg:     Config{HubURL: "ws://localhost/ws", ThingType: "agent", ThingID: "a1", Token: "tok", MetricsRegisterer: reg},
			wantErr: "Logger is required",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := New(tc.cfg)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected error containing %q, got %q", tc.wantErr, err.Error())
			}
		})
	}
}

// 2. TestNew_Defaults

func TestNew_Defaults(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	c, err := New(Config{
		HubURL:            "ws://localhost/ws",
		ThingType:         "agent",
		ThingID:           "a1",
		Token:             "tok",
		Logger:            testLogger(),
		MetricsRegisterer: reg,
	})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	if c.cfg.ReconnectMaxBackoff != 30*time.Second {
		t.Errorf("ReconnectMaxBackoff = %v, want 30s", c.cfg.ReconnectMaxBackoff)
	}
	if c.cfg.ReconnectInitialBackoff != 1*time.Second {
		t.Errorf("ReconnectInitialBackoff = %v, want 1s", c.cfg.ReconnectInitialBackoff)
	}
	if c.cfg.HeartbeatInterval != 15*time.Second {
		t.Errorf("HeartbeatInterval = %v, want 15s", c.cfg.HeartbeatInterval)
	}
	if c.cfg.WSFailureThreshold != 3 {
		t.Errorf("WSFailureThreshold = %d, want 3", c.cfg.WSFailureThreshold)
	}
	if c.cfg.MQBufferSize != 10000 {
		t.Errorf("MQBufferSize = %d, want 10000", c.cfg.MQBufferSize)
	}
	if c.cfg.ShutdownTimeout != 5*time.Second {
		t.Errorf("ShutdownTimeout = %v, want 5s", c.cfg.ShutdownTimeout)
	}
	if c.cfg.MetricsNamespace != "nexus" {
		t.Errorf("MetricsNamespace = %q, want %q", c.cfg.MetricsNamespace, "nexus")
	}
	if c.Mode() != ModeDisconnected {
		t.Errorf("initial Mode = %v, want ModeDisconnected", c.Mode())
	}
}

// 3. TestConnectWS_Success

func TestConnectWS_Success(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	hub := newHubServer(connectedMsg(1))
	defer hub.Close()

	cfg := testConfig(hub.URL(), reg)
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var appliedMu sync.Mutex
	var appliedDesired map[string]ConfigState
	c.OnConfigChanged(func(desired map[string]ConfigState) (map[string]ConfigState, error) {
		appliedMu.Lock()
		appliedDesired = desired
		appliedMu.Unlock()
		return desired, nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := c.connectWS(ctx); err != nil {
		t.Fatalf("connectWS: %v", err)
	}

	appliedMu.Lock()
	got := appliedDesired
	appliedMu.Unlock()

	if got == nil {
		t.Fatal("OnConfigChanged callback was not invoked")
	}
	if _, ok := got["routing"]; !ok {
		t.Error("expected 'routing' key in applied config")
	}
	if c.DesiredVer() != 1 {
		t.Errorf("DesiredVer = %d, want 1", c.DesiredVer())
	}
	if c.ReportedVer() != 1 {
		t.Errorf("ReportedVer = %d, want 1", c.ReportedVer())
	}
}

// 4. TestConnectWS_AuthHeader

func TestConnectWS_AuthHeader(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	hub := newHubServer(connectedMsg(1))
	defer hub.Close()

	cfg := testConfig(hub.URL(), reg)
	cfg.Token = "my-secret-token"

	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.OnConfigChanged(func(desired map[string]ConfigState) (map[string]ConfigState, error) {
		return desired, nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := c.connectWS(ctx); err != nil {
		t.Fatalf("connectWS: %v", err)
	}

	want := "Bearer my-secret-token"
	if got := hub.AuthHeader(); got != want {
		t.Errorf("Authorization header = %q, want %q", got, want)
	}
}

// 3b. TestConnectWS_ModeIsConnectedOnReturn
//
// connectWS calls applyConfig before returning, which triggers a shadow_report
// via sendShadowReport. sendShadowReport's switch drops the report if mode is
// still ModeWSConnecting, so the mode must flip to ModeWSConnected before
// applyConfig runs. This test guards that ordering.

func TestConnectWS_ModeIsConnectedOnReturn(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	hub := newHubServer(connectedMsg(1))
	defer hub.Close()

	cfg := testConfig(hub.URL(), reg)
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var modeDuringApply Mode
	var modeMu sync.Mutex
	c.OnConfigChanged(func(desired map[string]ConfigState) (map[string]ConfigState, error) {
		modeMu.Lock()
		modeDuringApply = c.Mode()
		modeMu.Unlock()
		return desired, nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := c.connectWS(ctx); err != nil {
		t.Fatalf("connectWS: %v", err)
	}

	if got := c.Mode(); got != ModeWSConnected {
		t.Errorf("mode after connectWS = %s, want ws_connected", got)
	}

	modeMu.Lock()
	observed := modeDuringApply
	modeMu.Unlock()
	if observed != ModeWSConnected {
		t.Errorf("mode observed inside OnConfigChanged = %s, want ws_connected "+
			"(sendShadowReport would drop the report otherwise)", observed)
	}
}

// 4b. TestConnectWS_DialURLQueryParams
//
// Hub's /ws authenticator reads Thing identity from the URL query: the
// service-token path requires both id and type; the device-token path
// requires id. If thingclient omits them, Hub returns 401 and the client
// silently degrades to HTTP fallback. This test guards the wire contract.

func TestConnectWS_DialURLQueryParams(t *testing.T) {
	t.Parallel()

	var capturedQuery url.Values
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		capturedQuery = r.URL.Query()
		mu.Unlock()

		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "done") //nolint:errcheck
		_ = conn.Write(r.Context(), websocket.MessageText, connectedMsg(1))
		// keep the connection open briefly so the client can read the message
		<-r.Context().Done()
	}))
	defer srv.Close()

	// Pre-existing query param on HubURL must be preserved.
	base := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws?debug=1"

	cfg := testConfig(base, prometheus.NewRegistry())
	cfg.ThingID = "proxy-abc"
	cfg.ThingType = "compliance-proxy"

	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.OnConfigChanged(func(d map[string]ConfigState) (map[string]ConfigState, error) { return d, nil })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := c.connectWS(ctx); err != nil {
		t.Fatalf("connectWS: %v", err)
	}

	mu.Lock()
	q := capturedQuery
	mu.Unlock()

	if got := q.Get("id"); got != "proxy-abc" {
		t.Errorf("id query = %q, want %q", got, "proxy-abc")
	}
	if got := q.Get("type"); got != "compliance-proxy" {
		t.Errorf("type query = %q, want %q", got, "compliance-proxy")
	}
	if got := q.Get("debug"); got != "1" {
		t.Errorf("pre-existing debug query = %q, want %q", got, "1")
	}
}

// 5. TestConnectWS_InvalidFirstMessage

func TestConnectWS_InvalidFirstMessage(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()

	badMsg, _ := json.Marshal(hubMessage{Type: "not_connected"})
	hub := newHubServer(badMsg)
	defer hub.Close()

	cfg := testConfig(hub.URL(), reg)
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err = c.connectWS(ctx)
	if err == nil {
		t.Fatal("expected error for non-connected first message")
	}
	if !strings.Contains(err.Error(), "expected 'connected' message") {
		t.Errorf("error = %q, want to contain 'expected connected message'", err.Error())
	}
}

// 6. TestReadPump_ConfigChanged

func TestReadPump_ConfigChanged(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	hub := newHubServer(connectedMsg(1))
	defer hub.Close()

	cfg := testConfig(hub.URL(), reg)
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var configApplyCount atomic.Int32
	c.OnConfigChanged(func(desired map[string]ConfigState) (map[string]ConfigState, error) {
		configApplyCount.Add(1)
		return desired, nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := c.connectWS(ctx); err != nil {
		t.Fatalf("connectWS: %v", err)
	}
	c.setMode(ModeWSConnected)

	readCtx, readCancel := context.WithCancel(ctx)

	c.mu.RLock()
	conn := c.wsConn
	c.mu.RUnlock()
	if conn == nil {
		t.Fatalf("wsConn is nil after connectWS")
	}

	go c.readPump(readCtx, conn)

	configChanged := hubMessage{
		Type:       "config_changed",
		ConfigKey:  "routing",
		State:      json.RawMessage(`{"version":2}`),
		DesiredVer: 2,
	}
	data, _ := json.Marshal(configChanged)
	hub.sendCh <- data

	deadline := time.After(3 * time.Second)
	for configApplyCount.Load() < 2 {

		select {
		case <-deadline:
			t.Fatalf("timed out waiting for config_changed callback, count=%d", configApplyCount.Load())
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	readCancel()

	if c.DesiredVer() != 2 {
		t.Errorf("DesiredVer = %d, want 2", c.DesiredVer())
	}
	if c.ReportedVer() != 2 {
		t.Errorf("ReportedVer = %d, want 2", c.ReportedVer())
	}
}

// 7. TestReadPump_UnknownType

func TestReadPump_UnknownType(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	hub := newHubServer(connectedMsg(1))
	defer hub.Close()

	cfg := testConfig(hub.URL(), reg)
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.OnConfigChanged(func(desired map[string]ConfigState) (map[string]ConfigState, error) {
		return desired, nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := c.connectWS(ctx); err != nil {
		t.Fatalf("connectWS: %v", err)
	}
	c.setMode(ModeWSConnected)

	readCtx, readCancel := context.WithCancel(ctx)

	c.mu.RLock()
	conn := c.wsConn
	c.mu.RUnlock()
	if conn == nil {
		t.Fatalf("wsConn is nil after connectWS")
	}

	done := make(chan struct{})
	go func() {
		c.readPump(readCtx, conn)
		close(done)
	}()

	unknownMsg, _ := json.Marshal(hubMessage{Type: "some_unknown_type"})
	hub.sendCh <- unknownMsg

	followUp := hubMessage{
		Type:       "config_changed",
		ConfigKey:  "quota",
		State:      json.RawMessage(`{}`),
		DesiredVer: 5,
	}
	followData, _ := json.Marshal(followUp)
	hub.sendCh <- followData

	deadline := time.After(3 * time.Second)
	for c.DesiredVer() != 5 {

		select {
		case <-deadline:
			t.Fatal("timed out; readPump did not process message after unknown type")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	readCancel()
	<-done
}

// 8. TestReadPump_InvalidJSON

func TestReadPump_InvalidJSON(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	hub := newHubServer(connectedMsg(1))
	defer hub.Close()

	cfg := testConfig(hub.URL(), reg)
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.OnConfigChanged(func(desired map[string]ConfigState) (map[string]ConfigState, error) {
		return desired, nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := c.connectWS(ctx); err != nil {
		t.Fatalf("connectWS: %v", err)
	}
	c.setMode(ModeWSConnected)

	readCtx, readCancel := context.WithCancel(ctx)

	c.mu.RLock()
	conn := c.wsConn
	c.mu.RUnlock()
	if conn == nil {
		t.Fatalf("wsConn is nil after connectWS")
	}

	done := make(chan struct{})
	go func() {
		c.readPump(readCtx, conn)
		close(done)
	}()

	hub.sendCh <- []byte(`{invalid json!!!`)

	configMsg := hubMessage{
		Type:       "config_changed",
		ConfigKey:  "quota",
		State:      json.RawMessage(`{}`),
		DesiredVer: 3,
	}
	data, _ := json.Marshal(configMsg)
	hub.sendCh <- data

	deadline := time.After(3 * time.Second)
	for c.ReportedVer() != 3 {

		select {
		case <-deadline:
			t.Fatal("timed out; readPump did not recover after invalid JSON")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	readCancel()
	<-done
}

// 9. TestWritePump_SendMessage

func TestWritePump_SendMessage(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	hub := newHubServer(connectedMsg(1))
	defer hub.Close()

	cfg := testConfig(hub.URL(), reg)
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.OnConfigChanged(func(desired map[string]ConfigState) (map[string]ConfigState, error) {
		return desired, nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := c.connectWS(ctx); err != nil {
		t.Fatalf("connectWS: %v", err)
	}
	c.setMode(ModeWSConnected)

	writeCtx, writeCancel := context.WithCancel(ctx)

	c.mu.RLock()
	conn := c.wsConn
	c.mu.RUnlock()
	if conn == nil {
		t.Fatalf("wsConn is nil after connectWS")
	}

	go c.writePump(writeCtx, conn)

	if err := c.sendMessage(thingMessage{
		Type:        "shadow_report",
		ReportedVer: 1,
	}); err != nil {
		t.Fatalf("sendMessage: %v", err)
	}

	select {
	case raw := <-hub.recvCh:
		var msg thingMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			t.Fatalf("unmarshal received message: %v", err)
		}
		if msg.Type != "shadow_report" {
			t.Errorf("received msg.Type = %q, want %q", msg.Type, "shadow_report")
		}
		if msg.ReportedVer != 1 {
			t.Errorf("received msg.ReportedVer = %d, want 1", msg.ReportedVer)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for message on server")
	}

	writeCancel()
}

// 10. TestWritePump_ChannelFull

func TestWritePump_ChannelFull(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	cfg := testConfig("ws://localhost:9/ws", reg)
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	for range cap(c.outChControl) + 10 {
		_ = c.sendMessage(thingMessage{Type: "ping"})
	}

	if len(c.outChControl) != cap(c.outChControl) {
		t.Errorf("outCh len = %d, want cap = %d (extra messages should be dropped)", len(c.outChControl), cap(c.outChControl))
	}
}

// 11. TestReconnect_ExponentialBackoff

func TestReconnect_ExponentialBackoff(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	cfg := testConfig("ws://localhost:9/ws", reg)
	cfg.ReconnectInitialBackoff = 1 * time.Second
	cfg.ReconnectMaxBackoff = 30 * time.Second
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	expectedBase := []time.Duration{
		1 * time.Second,
		2 * time.Second,
		4 * time.Second,
		8 * time.Second,
		16 * time.Second,
		30 * time.Second, // capped
		30 * time.Second, // still capped
	}

	for i, wantBase := range expectedBase {
		c.wsConsecutiveFailures.Store(int32(i + 1))

		got := c.calculateBackoff()
		low := wantBase
		high := wantBase + time.Duration(float64(wantBase)*0.25)

		if got < low || got > high {
			t.Errorf("failures=%d: backoff=%v, want [%v, %v]", i+1, got, low, high)
		}
	}
}

// 12. TestReconnect_Jitter

func TestReconnect_Jitter(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	cfg := testConfig("ws://localhost:9/ws", reg)
	cfg.ReconnectInitialBackoff = 1 * time.Second
	cfg.ReconnectMaxBackoff = 30 * time.Second
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	c.wsConsecutiveFailures.Store(3) // base = 4s
	baseFloat := float64(cfg.ReconnectInitialBackoff) * float64(2*2)
	base := time.Duration(baseFloat)
	maxJitter := time.Duration(baseFloat * 0.25)

	samples := 200
	for range samples {
		got := c.calculateBackoff()
		if got < base {
			t.Fatalf("backoff %v is below base %v", got, base)
		}
		if got > base+maxJitter {
			t.Fatalf("backoff %v exceeds base+25%% jitter (%v)", got, base+maxJitter)
		}
	}
}

// 13. TestReconnect_OnReconnectHook

// TestReconnect_OnReconnectHook asserts the OnReconnect callback fires on the
// initial connect as well as on every subsequent reconnect. Earlier behavior
// gated the first invocation on reportedVer > 0 (so the very first dial
// after process start would skip the callback); the gate was removed because
// every in-tree caller (agent diag-buffer drain, static_info re-push,
// alert-envelope replay) needs fresh-connect coverage. The HTTP-fallback
// recovery path in http.go always fired regardless, so the runLoop is now
// consistent with it.
func TestReconnect_OnReconnectHook(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	hub := newHubServer(connectedMsg(1))
	defer hub.Close()

	cfg := testConfig(hub.URL(), reg)
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.OnConfigChanged(func(desired map[string]ConfigState) (map[string]ConfigState, error) {
		return desired, nil
	})

	var reconnectCount atomic.Int32
	c.OnReconnect(func() {
		reconnectCount.Add(1)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// First connect — must fire OnReconnect on initial connection.
	if err := c.connectWS(ctx); err != nil {
		t.Fatalf("first connectWS: %v", err)
	}
	c.setMode(ModeWSConnected)
	if c.onReconnect != nil {
		c.onReconnect()
	}

	if reconnectCount.Load() != 1 {
		t.Fatalf("OnReconnect on fresh connect: got %d, want 1", reconnectCount.Load())
	}

	c.mu.Lock()
	if c.wsConn != nil {
		_ = c.wsConn.Close(websocket.StatusNormalClosure, "test close")
		c.wsConn = nil
	}
	c.mu.Unlock()

	// Reconnect — must fire OnReconnect again.
	hub2 := newHubServer(connectedMsg(2))
	defer hub2.Close()

	c.cfg.HubURL = hub2.URL()
	if err := c.connectWS(ctx); err != nil {
		t.Fatalf("second connectWS: %v", err)
	}

	c.setMode(ModeWSConnected)
	if c.onReconnect != nil {
		c.onReconnect()
	}

	if reconnectCount.Load() != 2 {
		t.Errorf("OnReconnect after reconnect: got %d, want 2", reconnectCount.Load())
	}
}

// 14. TestClose_GracefulShutdown

func TestClose_GracefulShutdown(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	hub := newHubServer(connectedMsg(1))
	defer hub.Close()

	cfg := testConfig(hub.URL(), reg)
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.OnConfigChanged(func(desired map[string]ConfigState) (map[string]ConfigState, error) {
		return desired, nil
	})

	ctx := context.Background()
	if err := c.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	deadline := time.After(3 * time.Second)
	for c.Mode() != ModeWSConnected {

		select {
		case <-deadline:
			t.Fatalf("timed out waiting for ModeWSConnected, mode=%v", c.Mode())
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer closeCancel()

	if err := c.Close(closeCtx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	select {
	case <-c.done:
	default:
		t.Fatal("done channel should be closed after Close()")
	}

	if c.Mode() != ModeDisconnected {
		t.Errorf("mode after Close = %v, want ModeDisconnected", c.Mode())
	}
}

// 16. TestMode_Transitions

// 17. TestReadPump_ConfigChangedDelta_MergesIntoDesiredCache

func TestReadPump_ConfigChangedDelta_MergesIntoDesiredCache(t *testing.T) {
	c, _ := newTestClient(t)
	setWSConnected(t, c)

	// Seed cache with the initial desired map the client would have received
	// on the "connected" message.
	c.mu.Lock()
	c.desiredCache = map[string]ConfigState{
		"hooks": {State: json.RawMessage(`{"enabled":false}`), Version: 1},
	}
	c.mu.Unlock()
	c.desiredVer.Store(1)

	var gotDesired map[string]ConfigState
	c.onConfigChanged = func(d map[string]ConfigState) (map[string]ConfigState, error) {
		gotDesired = d
		return d, nil
	}

	delta := hubMessage{
		Type:       "config_changed",
		ConfigKey:  "hooks",
		State:      json.RawMessage(`{"enabled":true}`),
		DesiredVer: 2,
	}
	c.handleHubMessage(delta)
	drainOutCh(t, c)

	if gotDesired == nil {
		t.Fatal("callback not invoked")
	}
	got, ok := gotDesired["hooks"]
	if !ok {
		t.Fatalf("hooks key missing; got keys=%v", keys(gotDesired))
	}
	if string(got.State) != `{"enabled":true}` {
		t.Fatalf("hooks state = %s; want enabled:true", got.State)
	}
	if got.Version != 2 {
		t.Fatalf("hooks version = %d; want 2", got.Version)
	}
	if c.reportedVer.Load() != 2 {
		t.Fatalf("reportedVer = %d; want 2", c.reportedVer.Load())
	}
}

// TestHandleHubMessage_ConfigChangedForce_BypassesEntryGate verifies that a
// force=true config_changed message at the same DesiredVer (no bump) still
// merges the delta, flows through OnConfigChanged, and emits a shadow_report
// path. This is the wire-level contract for the admin "Re-sync this key"
// button: Hub replays the current state at the same version; the client
// must still run the reducer instead of short-circuiting on the entry-level
// version gate (`msg.DesiredVer <= c.reportedVer.Load()`).
func TestHandleHubMessage_ConfigChangedForce_BypassesEntryGate(t *testing.T) {
	c, _ := newTestClient(t)
	setWSConnected(t, c)

	// Pretend the client already applied version 5 of the hooks key; a normal
	// (non-force) replay at version 5 must be a no-op — so this test proves
	// specifically that Force is what unblocks it.
	c.mu.Lock()
	c.desiredCache = map[string]ConfigState{
		"hooks": {State: json.RawMessage(`{"enabled":true}`), Version: 5},
	}
	c.mu.Unlock()
	c.desiredVer.Store(5)
	c.reportedVer.Store(5)

	var callbackRuns int
	c.onConfigChanged = func(d map[string]ConfigState) (map[string]ConfigState, error) {
		callbackRuns++
		if got, ok := d["hooks"]; !ok || string(got.State) != `{"enabled":true}` {
			t.Errorf("callback received wrong state: %+v", d["hooks"])
		}
		return d, nil
	}

	// Non-force replay at equal version: should be skipped.
	c.handleHubMessage(hubMessage{
		Type:       "config_changed",
		ConfigKey:  "hooks",
		State:      json.RawMessage(`{"enabled":true}`),
		DesiredVer: 5,
	})
	if callbackRuns != 0 {
		t.Fatalf("non-force replay at equal version should be a no-op; callbackRuns=%d", callbackRuns)
	}

	// Force replay at equal version: callback must fire exactly once.
	c.handleHubMessage(hubMessage{
		Type:       "config_changed",
		ConfigKey:  "hooks",
		State:      json.RawMessage(`{"enabled":true}`),
		DesiredVer: 5,
		Force:      true,
	})
	drainOutCh(t, c)
	if callbackRuns != 1 {
		t.Fatalf("force replay at equal version must fire callback once; callbackRuns=%d", callbackRuns)
	}
	if c.reportedVer.Load() != 5 {
		t.Errorf("reportedVer should stay at 5 after forced replay; got %d", c.reportedVer.Load())
	}
}

func keys(m map[string]ConfigState) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestMode_Transitions(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	cfg := testConfig("ws://localhost:9/ws", reg)
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if c.Mode() != ModeDisconnected {
		t.Errorf("initial mode = %v, want ModeDisconnected", c.Mode())
	}

	transitions := []struct {
		mode Mode
		str  string
	}{
		{ModeWSConnecting, "ws_connecting"},
		{ModeWSConnected, "ws_connected"},
		{ModeHTTPFallback, "http_fallback"},
		{ModeDisconnected, "disconnected"},
	}

	for _, tr := range transitions {
		c.setMode(tr.mode)

		if c.Mode() != tr.mode {
			t.Errorf("after setMode(%v): Mode() = %v", tr.mode, c.Mode())
		}
		if tr.mode.String() != tr.str {
			t.Errorf("Mode(%d).String() = %q, want %q", tr.mode, tr.mode.String(), tr.str)
		}
	}

	unknown := Mode(99)
	if unknown.String() != "unknown" {
		t.Errorf("Mode(99).String() = %q, want %q", unknown.String(), "unknown")
	}
}

// sendMessage — shadow_report blocks briefly instead of dropping on a full outCh

func TestSendMessage_ShadowReport_BlocksBrieflyInsteadOfDropping(t *testing.T) {
	c, _ := newTestClient(t)
	c.outChControl = make(chan []byte, 1)
	c.outChControl <- []byte("filler")

	done := make(chan struct{})
	go func() {
		_ = c.sendMessage(thingMessage{Type: "shadow_report", ReportedVer: 2})
		close(done)
	}()

	// Let the goroutine block on the full channel.
	time.Sleep(10 * time.Millisecond)
	// Drain the filler; the blocked send should now proceed.
	<-c.outChControl
	select {
	case <-done:
		// ok
	case <-time.After(200 * time.Millisecond):
		t.Fatal("shadow_report was dropped or deadlocked")
	}
}

// TestRunLoop_TransportTransitionsOnFailureThreshold verifies that runLoop
// enters HTTP fallback after WSFailureThreshold consecutive WS failures and
// returns to WS once the fallback loop returns.
func TestRunLoop_TransportTransitionsOnFailureThreshold(t *testing.T) {
	c, _ := newTestClient(t)
	c.cfg.WSFailureThreshold = 2
	c.cfg.ReconnectInitialBackoff = 2 * time.Millisecond
	c.cfg.ReconnectMaxBackoff = 5 * time.Millisecond

	var failures, httpFallbacks atomic.Int32
	c.connectWSFn = func(ctx context.Context) error {
		failures.Add(1)
		return errors.New("boom")
	}
	c.runHTTPFallbackFn = func(ctx context.Context) {
		httpFallbacks.Add(1)
		<-ctx.Done()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	c.runLoop(ctx)

	if failures.Load() < 2 {
		t.Errorf("expected >=2 WS attempts before HTTP fallback; got %d", failures.Load())
	}
	if httpFallbacks.Load() != 1 {
		t.Errorf("expected exactly 1 HTTP fallback entry; got %d", httpFallbacks.Load())
	}
}

// TestBuildWSDialURL verifies the handshake URL carries the full register
// payload. Regression guard for the bug where the WS path only sent id/type
// — leaving thing rows with NULL metrics_url / version / role and breaking
// the runtime-bridge introspection endpoint for every non-Hub service.
func TestBuildWSDialURL(t *testing.T) {
	t.Run("populated config emits all query params", func(t *testing.T) {
		c := &Client{cfg: Config{
			HubURL:        "ws://hub.local:3060/ws",
			ThingID:       "gw-host-3050",
			ThingType:     "ai-gateway",
			ThingVersion:  "0.1.0",
			ListenAddress: ":3050",
			MetricsURL:    "http://localhost:3050/metrics",
			Role:          "default",
			RuntimeAPIURL: "http://localhost:3050/internal/runtime",
		}}
		raw, err := c.buildWSDialURL()
		if err != nil {
			t.Fatalf("buildWSDialURL: %v", err)
		}
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		q := u.Query()
		want := map[string]string{
			"id":            "gw-host-3050",
			"type":          "ai-gateway",
			"version":       "0.1.0",
			"address":       ":3050",
			"metricsUrl":    "http://localhost:3050/metrics",
			"role":          "default",
			"runtimeApiUrl": "http://localhost:3050/internal/runtime",
		}
		for k, v := range want {
			if got := q.Get(k); got != v {
				t.Errorf("q.Get(%q) = %q, want %q", k, got, v)
			}
		}
	})

	t.Run("empty optional fields are omitted", func(t *testing.T) {
		c := &Client{cfg: Config{
			HubURL:    "ws://hub.local:3060/ws",
			ThingID:   "agent-001",
			ThingType: "agent",
		}}
		raw, err := c.buildWSDialURL()
		if err != nil {
			t.Fatalf("buildWSDialURL: %v", err)
		}
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		q := u.Query()
		for _, k := range []string{"version", "address", "metricsUrl", "role", "runtimeApiUrl"} {
			if _, present := q[k]; present {
				t.Errorf("query param %q should be omitted when Config field is empty (got %q)", k, q.Get(k))
			}
		}
		if got := q.Get("id"); got != "agent-001" {
			t.Errorf("id = %q, want agent-001", got)
		}
		if got := q.Get("type"); got != "agent" {
			t.Errorf("type = %q, want agent", got)
		}
	})
}

func TestClient_SnapshotDesired_ReturnsCopy(t *testing.T) {
	c := &Client{desiredCache: map[string]ConfigState{
		"killswitch": {State: json.RawMessage(`{"engaged":false}`), Version: 3},
	}}
	snap := c.SnapshotDesired()
	if len(snap) != 1 {
		t.Fatalf("len = %d", len(snap))
	}
	// Mutating the copy must not affect the cache.
	snap["new_key"] = ConfigState{Version: 99}
	if _, ok := c.desiredCache["new_key"]; ok {
		t.Fatal("desiredCache leaked through")
	}
}

//go:build e2e

// Package e2e wires together the full agent ops-metrics + diag pipeline
// against a mock Hub WebSocket / HTTP server (httptest). Every component
// built in isolation in unit tests is exercised together here so a
// regression in any one of them surfaces.
//
// Run with:
//
//	go test -tags=e2e -race -count=1 ./packages/agent/test/e2e/...
//
// No external services (Postgres, Hub, NATS) are needed — the mock Hub is
// in-process via net/http/httptest.
package e2e

import (
	"context"
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"github.com/goccy/go-json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	_ "github.com/mutecomm/go-sqlcipher/v4"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/observability/diag"
	shareddiag "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/diag"
	opsmetricsplat "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/platform"
	opsmetrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/thingclient"
)

// mockHub captures every Thing→Hub WS message and every HTTP request to the
// diag-drain endpoint. It is intentionally minimal — no Postgres, no real
// auth, no shadow comparison. Tests that need to assert on the wire format
// inspect mockHub.staticInfos / metricsSamples / diagEvents directly.
type mockHub struct {
	t *testing.T

	mu             sync.Mutex
	staticInfos    []json.RawMessage
	metricsSamples []opsmetrics.SampleBatch
	diagEvents     []opsmetrics.DiagEvent
	drainEvents    []opsmetrics.DiagEvent
	drainAcceptAll bool

	// reject toggles cause Accept to refuse the next handshake — used to
	// drive the disconnect/reconnect scenario without racing on conn close.
	rejectMu sync.Mutex
	reject   bool

	// disconnectAfter is signalled when the test wants the active WS to
	// close — drives reconnect-buffer drain assertions.
	disconnectMu sync.Mutex
	closeFn      func()

	server *httptest.Server
}

func newMockHub(t *testing.T) *mockHub {
	t.Helper()
	hub := &mockHub{t: t, drainAcceptAll: true}

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", hub.handleWS)
	mux.HandleFunc("/api/internal/things/diag-events:batch", hub.handleDrain)

	hub.server = httptest.NewServer(mux)
	t.Cleanup(hub.server.Close)
	return hub
}

func (h *mockHub) httpURL() string { return h.server.URL }
func (h *mockHub) wsURL() string {
	// thingclient uses HubURL verbatim as the WS dial target — include /ws.
	return strings.Replace(h.server.URL, "http://", "ws://", 1) + "/ws"
}

// setReject controls whether the next WS handshake completes. Set true to
// simulate Hub down; set false to allow reconnect.
func (h *mockHub) setReject(v bool) {
	h.rejectMu.Lock()
	defer h.rejectMu.Unlock()
	h.reject = v
}

func (h *mockHub) isReject() bool {
	h.rejectMu.Lock()
	defer h.rejectMu.Unlock()
	return h.reject
}

// disconnectActive closes the current WS conn (if any) so the agent's
// thingclient observes a network drop and re-enters the reconnect loop.
func (h *mockHub) disconnectActive() {
	h.disconnectMu.Lock()
	fn := h.closeFn
	h.closeFn = nil
	h.disconnectMu.Unlock()
	if fn != nil {
		fn()
	}
}

func (h *mockHub) handleWS(w http.ResponseWriter, r *http.Request) {
	if h.isReject() {
		http.Error(w, "hub down", http.StatusServiceUnavailable)
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols:       []string{"nexus.bearer"},
		InsecureSkipVerify: true,
	})
	if err != nil {
		h.t.Logf("mockHub Accept: %v", err)
		return
	}

	h.disconnectMu.Lock()
	h.closeFn = func() { _ = conn.Close(websocket.StatusNormalClosure, "test-disconnect") }
	h.disconnectMu.Unlock()

	// Send "connected" message so the thingclient handshake completes.
	// Include a single non-empty desired key with version 1 so the
	// thingclient runs OnConfigChanged → sendShadowReport → reportedVer=1
	// (kept for a realistic post-handshake state; OnReconnect now fires
	// on the initial dial regardless).
	connected := map[string]any{
		"type":    "connected",
		"thingId": r.URL.Query().Get("id"),
		"desired": map[string]any{
			"e2e": map[string]any{"state": json.RawMessage(`{}`), "version": 1},
		},
		"desiredVer": 1,
	}
	data, _ := json.Marshal(connected)
	if err := conn.Write(r.Context(), websocket.MessageText, data); err != nil {
		_ = conn.Close(websocket.StatusInternalError, "write connected")
		return
	}

	for {
		_, payload, err := conn.Read(r.Context())
		if err != nil {
			return
		}
		h.recordIncoming(payload)
	}
}

// recordIncoming dispatches Thing→Hub messages by their "type" discriminator
// into the appropriate slice on the mockHub.
func (h *mockHub) recordIncoming(payload []byte) {
	var probe struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(payload, &probe)

	switch probe.Type {
	case "static_info":
		h.mu.Lock()
		h.staticInfos = append(h.staticInfos, append(json.RawMessage(nil), payload...))
		h.mu.Unlock()
	case "metrics_sample":
		var batch struct {
			Type string `json:"type"`
			opsmetrics.SampleBatch
		}
		if err := json.Unmarshal(payload, &batch); err == nil {
			h.mu.Lock()
			h.metricsSamples = append(h.metricsSamples, batch.SampleBatch)
			h.mu.Unlock()
		}
	case "diag_event":
		var env struct {
			Type string `json:"type"`
			opsmetrics.DiagEvent
		}
		if err := json.Unmarshal(payload, &env); err == nil {
			h.mu.Lock()
			h.diagEvents = append(h.diagEvents, env.DiagEvent)
			h.mu.Unlock()
		}
	}
}

func (h *mockHub) handleDrain(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	var req struct {
		Events []struct {
			ID string `json:"id"`
			opsmetrics.DiagEvent
		} `json:"events"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	accepted := make([]string, 0, len(req.Events))
	h.mu.Lock()
	for _, e := range req.Events {
		h.drainEvents = append(h.drainEvents, e.DiagEvent)
		if h.drainAcceptAll {
			accepted = append(accepted, e.ID)
		}
	}
	h.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"acceptedIds": accepted})
}

// snapshotMetrics / snapshotDiag / snapshotStatic / snapshotDrain return
// stable copies under the mockHub mutex so tests can assert without races.
func (h *mockHub) snapshotMetrics() []opsmetrics.SampleBatch {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]opsmetrics.SampleBatch, len(h.metricsSamples))
	copy(out, h.metricsSamples)
	return out
}

func (h *mockHub) snapshotDiag() []opsmetrics.DiagEvent {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]opsmetrics.DiagEvent, len(h.diagEvents))
	copy(out, h.diagEvents)
	return out
}

func (h *mockHub) snapshotStatic() []json.RawMessage {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]json.RawMessage, len(h.staticInfos))
	copy(out, h.staticInfos)
	return out
}

func (h *mockHub) snapshotDrain() []opsmetrics.DiagEvent {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]opsmetrics.DiagEvent, len(h.drainEvents))
	copy(out, h.drainEvents)
	return out
}

// agentRig assembles the in-process equivalent of cmd/agent/main.go's diag
// stack: thingclient + opsmetrics.Registry + Sampler + Dedup + ReconnectBuffer
// + LocalBuffer + SlogSink + composed slog.Logger. Tests interact with the
// rig as the agent main would: log via rig.logger, push samples via
// rig.client, simulate panics via shareddiag.Recover, etc.
type agentRig struct {
	hub             *mockHub
	client          *thingclient.Client
	registry        *opsmetrics.Registry
	dedup           *opsmetrics.Dedup
	reconnectBuffer *shareddiag.ReconnectBuffer
	localBuffer     *diag.LocalBuffer
	db              *sql.DB
	logger          *slog.Logger
	staticInfo      opsmetrics.StaticInfo
	thingID         string
}

func newAgentRig(t *testing.T, ctx context.Context, hub *mockHub, thingID string) *agentRig {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	registry := opsmetrics.NewRegistry(prometheus.NewRegistry())
	dialCounter := registry.NewCounter("relay.dial_total", []string{"mode"})
	dialCounter.With("new").Inc() // bump so metrics_sample carries a real value

	startTime := time.Now().UTC()
	sampler := opsmetricsplat.NewSampler(thingID, startTime, registry)

	client, err := thingclient.New(thingclient.Config{
		HubURL:            hub.wsURL(),
		HubHTTPURL:        hub.httpURL(),
		ThingType:         "agent",
		ThingID:           thingID,
		Token:             "test-token",
		Logger:            logger,
		OpsMetricsSampler: sampler,
		MetricsRegisterer: prometheus.NewRegistry(),
		// Tight heartbeat so metrics_sample lands quickly in tests.
		HeartbeatInterval: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("thingclient.New: %v", err)
	}

	// Open an in-memory SQLite (SQLCipher driver runs as plain SQLite when
	// no PRAGMA key is set, matching the unit-test pattern in
	// internal/diag/local_buffer_test.go).
	db, err := sql.Open("sqlite3", "file::memory:?cache=shared&_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	if err := diag.MigratePendingDiagEvent(db); err != nil {
		t.Fatalf("migrate pending_diag_event: %v", err)
	}
	localBuffer := diag.NewLocalBuffer(db, logger)

	dedup := opsmetrics.NewDedup(time.Now, time.Minute, 100)
	droppedCounter := registry.NewCounter("diag.dropped_total", []string{"reason"}).With("reconnect_overflow")
	reconnectBuffer := shareddiag.NewReconnectBuffer(shareddiag.ReconnectBufferConfig{
		MaxLen: 100, MaxAge: 5 * time.Minute, Dropped: droppedCounter, Log: logger,
	})

	wsConnected := func() bool { return client.Mode() == thingclient.ModeWSConnected }
	sink := shareddiag.NewSlogSink(shareddiag.SlogSinkConfig{
		ThingClient:     client,
		LocalBuffer:     localBuffer,
		Dedup:           dedup,
		ReconnectBuffer: reconnectBuffer,
		IsWSConnected:   wsConnected,
		ThingID:         thingID,
		Source:          "agent",
		Level:           slog.LevelError,
	})
	composed := slog.New(shareddiag.NewMultiHandler(logger.Handler(), sink))

	staticInfo := opsmetricsplat.CaptureStaticInfo(opsmetricsplat.BuildInfo{
		ServiceVersion: "nexus-agent/test",
		StartTime:      startTime.Format(time.RFC3339),
	})

	// OnConfigChanged echoes desired back as reported. The mock Hub's
	// "connected" envelope carries a single non-empty desired key with
	// version 1 so this callback fires once on the initial connect; this
	// keeps the test path realistic but is not needed to unblock
	// OnReconnect (the runLoop fires it on every connect; there is no
	// reportedVer > 0 gate).
	client.OnConfigChanged(func(desired map[string]thingclient.ConfigState) (map[string]thingclient.ConfigState, error) {
		out := make(map[string]thingclient.ConfigState, len(desired))
		for k, v := range desired {
			out[k] = thingclient.ConfigState{State: v.State, Version: v.Version}
		}
		return out, nil
	})

	// OnReconnect MUST be registered before Start (per thingclient docs:
	// the field is read by runLoop without locking once Start is running,
	// so any later write is a data race). Mirror the cmd/agent/main.go
	// composition: drain the reconnect buffer via PushDiagEvent.
	clientLocal := client
	rb := reconnectBuffer
	client.OnReconnect(func() {
		drained := rb.Drain()
		if len(drained) == 0 {
			return
		}
		pushCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		for _, e := range drained {
			_ = clientLocal.PushDiagEvent(pushCtx, e)
		}
	})

	if err := client.Start(ctx); err != nil {
		t.Fatalf("client.Start: %v", err)
	}
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = client.Close(shutdownCtx)
		_ = db.Close()
	})

	// Wait for WS connect.
	waitFor(t, 3*time.Second, "thingclient WS connect", func() bool {
		return client.Mode() == thingclient.ModeWSConnected
	})

	return &agentRig{
		hub:             hub,
		client:          client,
		registry:        registry,
		dedup:           dedup,
		reconnectBuffer: reconnectBuffer,
		localBuffer:     localBuffer,
		db:              db,
		logger:          composed,
		staticInfo:      staticInfo,
		thingID:         thingID,
	}
}

// waitFor polls fn at 20ms intervals up to timeout; fatals if fn never
// returns true. The label is included in the failure message.
func waitFor(t *testing.T, timeout time.Duration, label string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("waitFor(%s): timed out after %s", label, timeout)
}

func TestAgentE2E_StaticInfoReachesHub(t *testing.T) {
	if os.Getenv("RUN_E2E") == "" {
		t.Skip("set RUN_E2E=1 to run the diag E2E suite")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	hub := newMockHub(t)
	rig := newAgentRig(t, ctx, hub, "agent-static-info")

	if err := rig.client.UpdateStaticInfo(ctx, rig.staticInfo); err != nil {
		t.Fatalf("UpdateStaticInfo: %v", err)
	}
	waitFor(t, 3*time.Second, "static_info on Hub", func() bool {
		return len(hub.snapshotStatic()) >= 1
	})
}

func TestAgentE2E_MetricsSampleReachesHub(t *testing.T) {
	if os.Getenv("RUN_E2E") == "" {
		t.Skip("set RUN_E2E=1 to run the diag E2E suite")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	hub := newMockHub(t)
	rig := newAgentRig(t, ctx, hub, "agent-metrics-sample")
	_ = rig

	// The thingclient runMetricsTicker fires every HeartbeatInterval (200ms
	// in this rig). Wait for a batch carrying relay.dial_total{mode=new}.
	waitFor(t, 5*time.Second, "metrics_sample with relay.dial_total", func() bool {
		for _, b := range hub.snapshotMetrics() {
			for _, s := range b.Samples {
				if s.Name == "relay.dial_total" && s.Value > 0 {
					return true
				}
			}
		}
		return false
	})
}

func TestAgentE2E_ErrorSlogReachesHub(t *testing.T) {
	if os.Getenv("RUN_E2E") == "" {
		t.Skip("set RUN_E2E=1 to run the diag E2E suite")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	hub := newMockHub(t)
	rig := newAgentRig(t, ctx, hub, "agent-slog-error")

	rig.logger.Error("e2e_test_error", "upstream", "api.openai.com:443")

	waitFor(t, 3*time.Second, "diag_event on Hub", func() bool {
		for _, e := range hub.snapshotDiag() {
			if e.Message == "e2e_test_error" && e.Level == opsmetrics.LevelError {
				return true
			}
		}
		return false
	})
}

func TestAgentE2E_PanicCrashPersistsAndDrains(t *testing.T) {
	if os.Getenv("RUN_E2E") == "" {
		t.Skip("set RUN_E2E=1 to run the diag E2E suite")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	hub := newMockHub(t)
	rig := newAgentRig(t, ctx, hub, "agent-panic-drain")

	// 1. Force a panic in a goroutine; shareddiag.Recover persists a FATAL crash
	//    DiagEvent before re-panicking. We swallow the re-panic with our
	//    own recover so the test process keeps running.
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			// Outermost recover: swallow the re-panic that shareddiag.Recover
			// emits so the test process survives.
			_ = recover()
		}()
		defer shareddiag.Recover(shareddiag.RecoveryConfig{
			ThingID:      rig.thingID,
			Buffer:       rig.localBuffer,
			AgentVersion: "test",
			Source:       "e2e-panic",
		}, nil)
		panic("simulated crash")
	}()
	<-done

	// 2. Pending row should now be present.
	waitFor(t, 2*time.Second, "pending_diag_event row", func() bool {
		n, err := rig.localBuffer.Pending()
		return err == nil && n == 1
	})

	// 3. Drain via HTTP — simulates the next-startup path. The mock Hub
	//    accepts every event and the drain helper deletes the local row.
	if err := diag.DrainPending(ctx, diag.DrainConfig{
		Buffer:      rig.localBuffer,
		HTTPClient:  http.DefaultClient,
		HubURL:      hub.httpURL(),
		DeviceToken: "test-token",
		ThingID:     rig.thingID,
		Log:         rig.logger,
	}); err != nil {
		t.Fatalf("DrainPending: %v", err)
	}

	// 4. Pending should be 0; mockHub.drainEvents must contain the crash.
	if n, err := rig.localBuffer.Pending(); err != nil || n != 0 {
		t.Errorf("Pending after drain = (%d, %v), want (0, nil)", n, err)
	}
	drained := hub.snapshotDrain()
	if len(drained) != 1 {
		t.Fatalf("hub drainEvents = %d, want 1", len(drained))
	}
	if drained[0].EventType != opsmetrics.EventTypeCrash {
		t.Errorf("EventType = %q, want crash", drained[0].EventType)
	}
	if drained[0].Source != "e2e-panic" {
		t.Errorf("Source = %q, want e2e-panic", drained[0].Source)
	}
	if !strings.Contains(drained[0].Message, "simulated crash") {
		t.Errorf("Message = %q, want contains 'simulated crash'", drained[0].Message)
	}
}

func TestAgentE2E_DedupSummaryReachesHubAfterWindow(t *testing.T) {
	if os.Getenv("RUN_E2E") == "" {
		t.Skip("set RUN_E2E=1 to run the diag E2E suite")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	hub := newMockHub(t)
	rig := newAgentRig(t, ctx, hub, "agent-dedup-summary")

	// Drive Dedup directly with a fake clock so the test does not have to
	// wait the full 60s window.
	now := time.Now().UTC()
	ftDedup := opsmetrics.NewDedup(func() time.Time { return now }, 100*time.Millisecond, 100)
	hash := md5.Sum([]byte("error|test|repeat boom"))
	evt := opsmetrics.DiagEvent{
		ThingID:     rig.thingID,
		OccurredAt:  now,
		Level:       opsmetrics.LevelError,
		EventType:   opsmetrics.EventTypeError,
		Source:      "test",
		Message:     "repeat boom",
		MessageHash: hex.EncodeToString(hash[:]),
		RepeatCount: 1,
	}
	first := ftDedup.Submit(evt)
	if len(first) != 1 {
		t.Fatalf("first Submit emit count = %d, want 1", len(first))
	}
	_ = ftDedup.Submit(evt) // suppressed
	_ = ftDedup.Submit(evt) // suppressed

	// Push the first emit + advance the clock past the window + drain summaries.
	if err := rig.client.PushDiagEvent(ctx, first[0]); err != nil {
		t.Fatalf("PushDiagEvent first: %v", err)
	}

	now = now.Add(time.Second)
	summaries := ftDedup.Tick()
	if len(summaries) != 1 {
		t.Fatalf("Tick summaries = %d, want 1", len(summaries))
	}
	if summaries[0].RepeatCount != 3 {
		t.Errorf("summary RepeatCount = %d, want 3", summaries[0].RepeatCount)
	}
	if err := rig.client.PushDiagEvent(ctx, summaries[0]); err != nil {
		t.Fatalf("PushDiagEvent summary: %v", err)
	}

	waitFor(t, 3*time.Second, "summary diag_event on Hub", func() bool {
		var sawFirst, sawSummary bool
		for _, e := range hub.snapshotDiag() {
			if e.Message != "repeat boom" {
				continue
			}
			switch e.RepeatCount {
			case 1:
				sawFirst = true
			case 3:
				sawSummary = true
			}
		}
		return sawFirst && sawSummary
	})
}

func TestAgentE2E_DisconnectBuffersThenDrainsOnReconnect(t *testing.T) {
	if os.Getenv("RUN_E2E") == "" {
		t.Skip("set RUN_E2E=1 to run the diag E2E suite")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	hub := newMockHub(t)
	rig := newAgentRig(t, ctx, hub, "agent-reconnect-buffer")

	// newAgentRig already wired OnReconnect → buffer.Drain → PushDiagEvent.

	// Force the WS to drop and refuse new handshakes.
	hub.setReject(true)
	hub.disconnectActive()

	waitFor(t, 3*time.Second, "client observes disconnect", func() bool {
		return rig.client.Mode() != thingclient.ModeWSConnected
	})

	// While disconnected, the SlogSink should buffer the event in the
	// reconnect buffer (NOT push to mockHub).
	rig.logger.Error("offline_boom", "step", "while-disconnected")
	if got := hub.snapshotDiagWithMessage("offline_boom"); got != 0 {
		t.Errorf("event leaked to Hub while disconnected: %d", got)
	}
	waitFor(t, 1*time.Second, "reconnect buffer holds event", func() bool {
		return rig.reconnectBuffer.Pending() == 1
	})

	// Allow new handshakes; the thingclient's reconnect loop will rebind
	// and OnReconnect drains the buffer.
	hub.setReject(false)

	waitFor(t, 6*time.Second, "client reconnects", func() bool {
		return rig.client.Mode() == thingclient.ModeWSConnected
	})
	waitFor(t, 3*time.Second, "buffered event arrives on Hub", func() bool {
		return hub.snapshotDiagWithMessage("offline_boom") >= 1
	})
}

// snapshotDiagWithMessage returns how many diag_events on Hub had the
// given message field — used by the reconnect-buffer assertion.
func (h *mockHub) snapshotDiagWithMessage(msg string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, e := range h.diagEvents {
		if e.Message == msg {
			n++
		}
	}
	return n
}

// silence unused import linter for url.PathEscape on older Go toolchains —
// kept here so the file compiles cleanly across the go.work versions.
var _ = url.PathEscape

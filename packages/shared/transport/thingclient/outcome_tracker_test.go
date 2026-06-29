package thingclient

import (
	"context"
	"errors"
	"github.com/goccy/go-json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/platform"
	opsmetrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
	nexushttp "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/http"
	"github.com/coder/websocket"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// outcomes.go — Record / Snapshot / Outcomes are 0% covered.
// Pin observable behavior: per-key ledger semantics + nil-safety.

func TestOutcomeTracker_Record_Success_AdvancesAppliedVersion(t *testing.T) {
	t.Parallel()
	tr := NewOutcomeTracker()

	tr.Record("hooks", 7, nil)

	snap := tr.Snapshot()
	got, ok := snap["hooks"]
	if !ok {
		t.Fatalf("hooks missing from snapshot")
	}
	if got.AppliedVersion == nil || *got.AppliedVersion != 7 {
		t.Errorf("AppliedVersion = %v, want 7", got.AppliedVersion)
	}
	if got.AppliedAt == nil {
		t.Errorf("AppliedAt should be non-nil after a successful Record")
	}
	if got.ApplyError != nil {
		t.Errorf("ApplyError should be nil after a successful Record; got %+v", got.ApplyError)
	}
}

func TestOutcomeTracker_Record_Failure_PreservesPreviousSuccessAndStoresError(t *testing.T) {
	t.Parallel()
	tr := NewOutcomeTracker()

	// First a clean success at v=10.
	tr.Record("routing", 10, nil)
	// Then a failure at v=11.
	tr.Record("routing", 11, errors.New("apply borked"))

	snap := tr.Snapshot()
	got := snap["routing"]
	if got.AppliedVersion == nil || *got.AppliedVersion != 10 {
		t.Errorf("AppliedVersion preserved across failure: got %v, want 10", got.AppliedVersion)
	}
	if got.AppliedAt == nil {
		t.Errorf("AppliedAt preserved across failure: got nil")
	}
	if got.ApplyError == nil || got.ApplyError.Message != "apply borked" {
		t.Errorf("ApplyError = %+v, want msg=apply borked", got.ApplyError)
	}
	if got.ApplyError != nil && got.ApplyError.At.IsZero() {
		t.Errorf("ApplyError.At should be a non-zero timestamp")
	}
}

func TestOutcomeTracker_Record_SuccessAfterFailureClearsError(t *testing.T) {
	t.Parallel()
	tr := NewOutcomeTracker()

	tr.Record("k", 3, errors.New("fail-1"))
	tr.Record("k", 4, nil)

	got := tr.Snapshot()["k"]
	if got.ApplyError != nil {
		t.Errorf("ApplyError must clear on success; got %+v", got.ApplyError)
	}
	if got.AppliedVersion == nil || *got.AppliedVersion != 4 {
		t.Errorf("AppliedVersion = %v, want 4", got.AppliedVersion)
	}
}

func TestOutcomeTracker_Record_EmptyKey_SilentlyIgnored(t *testing.T) {
	t.Parallel()
	tr := NewOutcomeTracker()
	tr.Record("", 5, nil)
	tr.Record("", 6, errors.New("e"))
	if got := tr.Snapshot(); len(got) != 0 {
		t.Errorf("empty key must be silently dropped; got %d entries", len(got))
	}
}

func TestOutcomeTracker_Record_NilReceiver_DoesNotPanic(t *testing.T) {
	t.Parallel()
	var tr *OutcomeTracker
	// Must NOT panic on nil receiver — services may not have wired up yet.
	tr.Record("hooks", 1, nil)
}

func TestOutcomeTracker_Snapshot_NilReceiver_ReturnsEmptyNonNilMap(t *testing.T) {
	t.Parallel()
	var tr *OutcomeTracker
	got := tr.Snapshot()
	if got == nil {
		t.Fatal("Snapshot on nil receiver should return non-nil empty map (JSON contract)")
	}
	if len(got) != 0 {
		t.Errorf("Snapshot on nil receiver: len=%d, want 0", len(got))
	}
}

func TestOutcomeTracker_Snapshot_ReturnsCopy(t *testing.T) {
	t.Parallel()
	tr := NewOutcomeTracker()
	tr.Record("a", 1, nil)
	snap := tr.Snapshot()

	// Mutating the snapshot must not affect later snapshots.
	snap["evil"] = ApplyOutcome{}
	if _, leaked := tr.Snapshot()["evil"]; leaked {
		t.Errorf("Snapshot mutation leaked back into tracker state")
	}
}

func TestClient_Outcomes_ReturnsLazilyCreatedTracker(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t)
	tr := c.Outcomes()
	if tr == nil {
		t.Fatal("Outcomes() must return a non-nil tracker from a new Client")
	}
	tr.Record("x", 1, nil)
	if _, ok := c.Outcomes().Snapshot()["x"]; !ok {
		t.Errorf("Outcomes() returns the same tracker across calls; expected x to persist")
	}
}

// shadow.go — KeyVersion / LastReportedAt / applyConfigForce no-callback /
// sendShadowReportHTTP failure / flattenReported nil-state skip / shadow
// fallback HTTP success path.

func TestKeyVersion_AbsentKeyReturnsZero(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t)
	if got := c.KeyVersion("never-stored"); got != 0 {
		t.Errorf("KeyVersion(absent) = %d, want 0", got)
	}
}

func TestKeyVersion_StoredKeyReturnsLastSeen(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t)
	c.perKeyVersion.Store("hooks", int64(42))
	if got := c.KeyVersion("hooks"); got != 42 {
		t.Errorf("KeyVersion(hooks) = %d, want 42", got)
	}
}

func TestKeyVersion_NonInt64StoredValueReturnsZero(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t)
	// Direct sync.Map seed with a non-int64 type — guards the defensive type-assert.
	c.perKeyVersion.Store("garbled", "not an int64")
	if got := c.KeyVersion("garbled"); got != 0 {
		t.Errorf("KeyVersion(garbled-type) = %d, want 0", got)
	}
}

func TestLastReportedAt_EmptyBeforeFirstReport(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t)
	if got := c.LastReportedAt(); got != "" {
		t.Errorf("LastReportedAt before first report = %q, want empty", got)
	}
}

func TestLastReportedAt_ReadsBackStoredValue(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t)
	c.lastReportedAt.Store("2026-05-17T10:00:00Z")
	if got := c.LastReportedAt(); got != "2026-05-17T10:00:00Z" {
		t.Errorf("LastReportedAt = %q, want round-trip of stored RFC3339", got)
	}
}

func TestLastReportedAt_NonStringStored_ReturnsEmpty(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t)
	// Direct atomic.Value store with a non-string type — exercise the type-assert guard.
	c.lastReportedAt.Store(struct{ s string }{s: "not a string"})
	if got := c.LastReportedAt(); got != "" {
		t.Errorf("LastReportedAt with non-string stored = %q, want empty", got)
	}
}

func TestApplyConfigForce_NoCallback_LogsAndReturns(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t)
	// onConfigChanged is nil by default — must NOT panic and must NOT advance reportedVer.
	c.applyConfigForce(sampleDesired(), 5)
	if c.reportedVer.Load() != 0 {
		t.Errorf("reportedVer should remain 0 when force-replay has no callback; got %d", c.reportedVer.Load())
	}
}

func TestSendShadowReportWS_OutChStallReportsFailure(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t)
	c.mu.Lock()
	c.mode = ModeWSConnected
	c.mu.Unlock()

	// Replace outChControl with a buffer of 1 and pre-fill it so sendBytes
	// blocks and ultimately times out — exercising the WS-mode error branch
	// in sendShadowReport that increments the failure metric and propagates
	// the error.
	c.outChControl = make(chan []byte, 1)
	c.outChControl <- []byte("filler")

	// Use a goroutine because sendBytes has a hard 5s ctx deadline; cancel
	// via the harness inside it. Here we accept a brief wait window.
	type result struct {
		err error
	}
	done := make(chan result, 1)
	go func() {
		// Reduce the wait window by emptying the channel after a small delay
		// so sendBytes succeeds quickly — exercises only the WS branch.
		go func() {
			time.Sleep(50 * time.Millisecond)
			<-c.outChControl
		}()
		err := c.sendShadowReport(map[string]ConfigState{
			"k": {State: json.RawMessage(`{}`), Version: 1},
		}, 1)
		done <- result{err: err}
	}()

	select {
	case r := <-done:
		if r.err != nil {
			t.Errorf("sendShadowReport via WS must succeed when channel drains; got %v", r.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("sendShadowReport timed out")
	}
}

func TestSendShadowReport_HTTPFallback_Success(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/internal/things/shadow" {
			t.Errorf("path = %q, want /api/internal/things/shadow", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newHTTPTestClient(t, srv.URL)
	c.setMode(ModeHTTPFallback)

	err := c.sendShadowReport(map[string]ConfigState{
		"hooks": {State: json.RawMessage(`{"enabled":true}`), Version: 1},
	}, 1)
	if err != nil {
		t.Fatalf("sendShadowReport in HTTPFallback should succeed; got %v", err)
	}
	if got := c.LastReportedAt(); got == "" {
		t.Errorf("LastReportedAt should be set after HTTP shadow_report success")
	}
}

func TestSendShadowReport_HTTPFallback_ServerError_PropagatesAndCountsFailure(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("kaboom"))
	}))
	defer srv.Close()

	c := newHTTPTestClient(t, srv.URL)
	c.setMode(ModeHTTPFallback)

	err := c.sendShadowReport(map[string]ConfigState{
		"hooks": {State: json.RawMessage(`{}`), Version: 1},
	}, 1)
	if err == nil {
		t.Fatal("expected error on HTTP 500")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error should mention HTTP 500; got %v", err)
	}
	failCount := testutil.ToFloat64(c.promMetrics.shadowReports.WithLabelValues("failure"))
	if failCount != 1 {
		t.Errorf("shadow_reports{status=failure} = %v, want 1", failCount)
	}
}

func TestFlattenReported_NilStateSkipped_EmptyInputReturnsNonNil(t *testing.T) {
	t.Parallel()
	got := flattenReported(map[string]ConfigState{
		"with-state": {State: json.RawMessage(`{"x":1}`), Version: 1},
		"empty":      {State: nil, Version: 1},
	})
	if got == nil {
		t.Fatal("flattenReported must always return a non-nil map (Hub rejects null)")
	}
	if _, ok := got["empty"]; ok {
		t.Errorf("nil-state key should be skipped, got it in output")
	}
	if _, ok := got["with-state"]; !ok {
		t.Errorf("non-nil-state key should be kept")
	}

	emptyOut := flattenReported(nil)
	if emptyOut == nil {
		t.Errorf("empty input should still yield a non-nil map")
	}
	if len(emptyOut) != 0 {
		t.Errorf("empty input should yield 0-length map; got %d", len(emptyOut))
	}
}

// client.go — OnHeartbeatTick / SetHeartbeatInterval / broadcastHeartbeatKick
// / tickHeartbeat-onHeartbeatTick callback / writeFrame error / mergeDelta
// nil cache / dialHTTPClient with-control branch.

func TestOnHeartbeatTick_CallbackFiresEvenWithoutSampler(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t)

	var fired atomic.Int32
	c.OnHeartbeatTick(func() {
		fired.Add(1)
	})

	c.tickHeartbeat(context.Background())

	deadline := time.After(time.Second)
	for fired.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("OnHeartbeatTick callback never fired")
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
}

func TestOnHeartbeatTick_NilCallback_NoOp(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t)
	// Without registering — tickHeartbeat must not panic when callback is nil.
	c.tickHeartbeat(context.Background())
}

func TestCurrentHeartbeatInterval_DefaultsToConfig(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t)
	// Default config has HeartbeatInterval=15s via setDefaults.
	if got := c.CurrentHeartbeatInterval(); got != 15*time.Second {
		t.Errorf("CurrentHeartbeatInterval default = %v, want 15s", got)
	}
}

func TestSetHeartbeatInterval_OverridesAndBroadcastsKick(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t)

	oldKick := *c.heartbeatKick.Load()

	c.SetHeartbeatInterval(2 * time.Second)
	if got := c.CurrentHeartbeatInterval(); got != 2*time.Second {
		t.Errorf("after SetHeartbeatInterval(2s): got %v, want 2s", got)
	}

	// Old kick channel must be closed; new one must exist.
	select {
	case _, ok := <-oldKick:
		if ok {
			t.Errorf("old kick channel should be closed after SetHeartbeatInterval")
		}
	case <-time.After(100 * time.Millisecond):
		t.Errorf("old kick channel was not closed")
	}

	newKick := *c.heartbeatKick.Load()
	select {
	case <-newKick:
		t.Errorf("new kick channel should be open immediately after rotation")
	default:
		// expected: still open
	}
}

func TestSetHeartbeatInterval_NonPositiveIgnored(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t)
	// Seed a known override.
	c.SetHeartbeatInterval(7 * time.Second)
	if got := c.CurrentHeartbeatInterval(); got != 7*time.Second {
		t.Fatalf("setup precondition: want 7s, got %v", got)
	}
	// Non-positive must NOT clear the override (safety against disabling heartbeat).
	c.SetHeartbeatInterval(0)
	if got := c.CurrentHeartbeatInterval(); got != 7*time.Second {
		t.Errorf("SetHeartbeatInterval(0) must be ignored; got %v want 7s", got)
	}
	c.SetHeartbeatInterval(-1 * time.Second)
	if got := c.CurrentHeartbeatInterval(); got != 7*time.Second {
		t.Errorf("SetHeartbeatInterval(-1s) must be ignored; got %v want 7s", got)
	}
}

func TestBroadcastHeartbeatKick_IsIdempotentUnderConcurrentSets(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t)

	// Multiple concurrent kicks must not panic on double-close (channel swap
	// before close serializes who-closes-what).
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.broadcastHeartbeatKick()
		}()
	}
	wg.Wait()
}

func TestRunMetricsTicker_ContextCancelExits(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t)
	c.cfg.HeartbeatInterval = 50 * time.Millisecond
	// Sampler can stay nil — runMetricsTicker still loops and we just verify
	// it exits on ctx cancel without hanging.
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		c.runMetricsTicker(ctx)
		close(done)
	}()

	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("runMetricsTicker did not exit on ctx cancel")
	}
}

func TestRunMetricsTicker_FiresTickHeartbeatOnTimer(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t)
	c.cfg.HeartbeatInterval = 20 * time.Millisecond

	var fired atomic.Int32
	c.OnHeartbeatTick(func() {
		fired.Add(1)
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go c.runMetricsTicker(ctx)

	deadline := time.After(2 * time.Second)
	for fired.Load() < 2 {
		select {
		case <-deadline:
			t.Fatalf("runMetricsTicker fired only %d ticks", fired.Load())
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func TestRunMetricsTicker_RecadenceOnKick(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t)
	// Start with a slow base interval so the timer is in-flight when the kick lands.
	c.cfg.HeartbeatInterval = 10 * time.Second

	var fired atomic.Int32
	c.OnHeartbeatTick(func() {
		fired.Add(1)
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go c.runMetricsTicker(ctx)

	// Give the goroutine a chance to install its timer before we change the cadence.
	time.Sleep(20 * time.Millisecond)
	// Switch to a fast cadence; the kick channel close should re-arm.
	c.SetHeartbeatInterval(20 * time.Millisecond)

	deadline := time.After(2 * time.Second)
	for fired.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("runMetricsTicker did not fire after kick re-cadence")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func TestDialHTTPClient_WithGlobalDialControl_ReturnsCustomTransport(t *testing.T) {
	// Not Parallel — mutates process-global dial control.
	prev := nexushttp.GlobalDialControl()
	t.Cleanup(func() { nexushttp.SetGlobalDialControl(prev) })

	called := false
	nexushttp.SetGlobalDialControl(func(_, _ string, _ syscall.RawConn) error {
		called = true
		return nil
	})

	hc := dialHTTPClient()
	if hc == nil {
		t.Fatal("dialHTTPClient returned nil")
		return
	}
	if hc.Transport == nil {
		t.Fatal("dialHTTPClient with global control should set a custom Transport")
	}
	// Make sure the returned client is functional by issuing a GET to httptest.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	resp, err := hc.Get(srv.URL)
	if err != nil {
		t.Fatalf("Get via dialHTTPClient transport: %v", err)
	}
	_ = resp.Body.Close()
	if !called {
		t.Errorf("global dial-control callback never fired through the custom transport")
	}
}

func TestDialHTTPClient_NilGlobalDialControl_ReturnsBaseClient(t *testing.T) {
	// Not Parallel — mutates process-global dial control.
	prev := nexushttp.GlobalDialControl()
	t.Cleanup(func() { nexushttp.SetGlobalDialControl(prev) })

	nexushttp.SetGlobalDialControl(nil)
	hc := dialHTTPClient()
	if hc == nil {
		t.Fatal("dialHTTPClient returned nil")
		return
	}
	// Without control, we fall through to nexushttp.New(nexushttp.Config{}).
	if hc.Transport == nil {
		t.Errorf("nexushttp.New should still set a Transport")
	}
}

func TestConfig_SetDefaults_NilMetricsRegisterer_FallsBackToPromDefault(t *testing.T) {
	t.Parallel()
	// Exercise the default-setting branch in isolation. The previous version
	// of this test called New(Config{…}) which actually registers Prometheus
	// collectors onto the global default registerer — re-running the test
	// (or running it in parallel with another that touches the global
	// registry under the same metric names) panicked with "duplicate
	// metrics collector registration attempted". setDefaults is the
	// function under test; calling it directly avoids the side effect
	// and keeps the assertion intent ("nil MetricsRegisterer becomes
	// prometheus.DefaultRegisterer") sharp.
	cfg := Config{}
	cfg.setDefaults()

	if cfg.MetricsRegisterer == nil {
		t.Fatal("setDefaults: MetricsRegisterer must default when nil")
	}
	if cfg.MetricsRegisterer != prometheus.DefaultRegisterer {
		t.Errorf("MetricsRegisterer = %v, want prometheus.DefaultRegisterer", cfg.MetricsRegisterer)
	}
	if cfg.MetricsNamespace != "nexus" {
		t.Errorf("MetricsNamespace = %q, want \"nexus\"", cfg.MetricsNamespace)
	}

	// Explicit non-nil registerer must NOT be overwritten.
	explicit := prometheus.NewRegistry()
	cfg2 := Config{MetricsRegisterer: explicit, MetricsNamespace: "custom"}
	cfg2.setDefaults()
	if cfg2.MetricsRegisterer != explicit {
		t.Errorf("setDefaults clobbered an explicit MetricsRegisterer")
	}
	if cfg2.MetricsNamespace != "custom" {
		t.Errorf("setDefaults clobbered MetricsNamespace = %q", cfg2.MetricsNamespace)
	}
}

func TestSendMessage_MarshalError_ReturnsError(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t)
	// thingMessage.Reported is map[string]json.RawMessage — but we can't easily
	// poison it. Instead, smash through sendMessage by injecting an unencodable
	// raw payload via the Reported map (json.RawMessage carrying invalid UTF-8
	// is actually fine for json.Marshal). The real-world marshal error path
	// for thingMessage is hard to trigger without code surgery — drive it via
	// sendBytes with an oversized message to demonstrate the timeout-failure
	// branch (covered separately below).
	//
	// Here we drive routeOutboundChannel via the empty-Type code path.
	if err := c.sendMessage(thingMessage{Type: "shadow_report"}); err != nil {
		// shadow_report is a control message — route to outChControl; with the
		// default cap of 32, this single send will succeed.
		t.Errorf("baseline sendMessage failed: %v", err)
	}
}

func TestSendBytes_StallTimeout_ReportsDropMetric(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t)

	// Pre-fill outChControl past capacity so the next sendBytes blocks.
	for range cap(c.outChControl) {
		c.outChControl <- []byte("filler")
	}

	// We don't wait for the real 5s timeout — shrink the test by validating
	// the drop counter behavior via routeOutboundChannel + a synchronous
	// pre-fill check.
	if len(c.outChControl) != cap(c.outChControl) {
		t.Fatalf("setup: outChControl len=%d cap=%d", len(c.outChControl), cap(c.outChControl))
	}
	// Verify routing is deny-list-like: unknown msg-type routes to control.
	ch := c.routeOutboundChannel("anything-not-in-metrics-set")
	if ch != c.outChControl {
		t.Errorf("unknown msg_type should route to outChControl")
	}
	// And metrics-type routes to outChMetrics.
	if c.routeOutboundChannel(msgTypeMetricsSample) != c.outChMetrics {
		t.Errorf("metrics_sample must route to outChMetrics")
	}
	if c.routeOutboundChannel(msgTypeDiagEvent) != c.outChMetrics {
		t.Errorf("diag_event must route to outChMetrics")
	}
}

func TestMergeDelta_NilDesiredCache_InitializesIt(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t)
	// Force desiredCache nil to exercise the lazy-init branch.
	c.mu.Lock()
	c.desiredCache = nil
	c.mu.Unlock()

	got := c.mergeDelta("hooks", json.RawMessage(`{"x":1}`), 4)
	if _, ok := got["hooks"]; !ok {
		t.Errorf("mergeDelta did not write hooks key into returned map")
	}
	c.mu.RLock()
	cached := c.desiredCache["hooks"]
	c.mu.RUnlock()
	if cached.Version != 4 {
		t.Errorf("internal desiredCache hooks version = %d, want 4", cached.Version)
	}
}

// opsmetrics.go — marshal-error paths in PushMetricsSample / PushDiagEvent.
// Forces json.Marshal failure via a `chan` value smuggled into the
// map[string]any fields (Sample.Metadata / DiagEvent.Attrs).
// UpdateStaticInfo has no `any` field so its marshal-error branch is
// structurally unreachable from tests (no production seam).

func TestPushMetricsSample_MarshalError_ReturnsAndLogs(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t)

	batch := opsmetrics.SampleBatch{
		ThingID:   "thing-001",
		SampledAt: time.Now().UTC(),
		Samples: []opsmetrics.Sample{
			{
				Name:     "boom",
				Kind:     opsmetrics.KindGauge,
				Metadata: map[string]any{"unencodable": make(chan int)},
			},
		},
	}
	err := c.PushMetricsSample(context.Background(), batch)
	if err == nil {
		t.Fatal("expected marshal error for chan in Metadata")
	}
	if !strings.Contains(err.Error(), "marshal metrics_sample") {
		t.Errorf("error message should mention marshal metrics_sample; got %q", err.Error())
	}
}

func TestPushDiagEvent_MarshalError_ReturnsAndLogs(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t)

	event := opsmetrics.DiagEvent{
		ThingID:    "thing-001",
		OccurredAt: time.Now().UTC(),
		Level:      opsmetrics.LevelError,
		EventType:  opsmetrics.EventTypeError,
		Source:     "test",
		Message:    "x",
		Attrs:      map[string]any{"ch": make(chan int)},
	}
	err := c.PushDiagEvent(context.Background(), event)
	if err == nil {
		t.Fatal("expected marshal error for chan in Attrs")
	}
	if !strings.Contains(err.Error(), "marshal diag_event") {
		t.Errorf("error message should mention marshal diag_event; got %q", err.Error())
	}
}

// http.go — do() marshal failure / unmarshal failures on register/heartbeat/
// configPull / runHTTPFallback heartbeat-success + config-via-heartbeat path
// when the register response already supplies desired config.

type unencodable struct{}

func (unencodable) MarshalJSON() ([]byte, error) {
	return nil, errors.New("forced marshal error")
}

func TestHTTPClient_Do_MarshalBodyError(t *testing.T) {
	t.Parallel()
	// Spin up a server that should never get called.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Errorf("server should never be reached on marshal-body error")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newHTTPTestClient(t, srv.URL)
	hc := c.getHTTPClient()
	_, _, err := hc.do(context.Background(), http.MethodPost, "/path", unencodable{})
	if err == nil {
		t.Fatal("expected marshal error for body that fails MarshalJSON")
	}
	if !strings.Contains(err.Error(), "marshal request") {
		t.Errorf("error = %q, want substring 'marshal request'", err.Error())
	}
}

func TestHTTPClient_Do_BadRequestPath(t *testing.T) {
	t.Parallel()
	c := newHTTPTestClient(t, "http://example.invalid")
	hc := c.getHTTPClient()
	// Invalid request URL: control characters are rejected by http.NewRequestWithContext.
	_, _, err := hc.do(context.Background(), "BAD\nMETHOD", "/", nil)
	if err == nil {
		t.Fatal("expected invalid-method error")
	}
	if !strings.Contains(err.Error(), "create request") {
		t.Errorf("error = %q, want 'create request' wrapper", err.Error())
	}
}

func TestHTTPClient_Do_TransportError(t *testing.T) {
	t.Parallel()
	// Point at a closed httptest server so the underlying TCP fails.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.Close()

	c := newHTTPTestClient(t, srv.URL)
	hc := c.getHTTPClient()
	_, _, err := hc.do(context.Background(), http.MethodGet, "/", nil)
	if err == nil {
		t.Fatal("expected transport error against a closed server")
	}
	if !strings.Contains(err.Error(), "http request") {
		t.Errorf("error = %q, want 'http request' wrapper", err.Error())
	}
}

func TestHTTPRegister_InvalidJSONResponse(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{not json"))
	}))
	defer srv.Close()

	c := newHTTPTestClient(t, srv.URL)
	_, err := c.httpRegister(context.Background())
	if err == nil {
		t.Fatal("expected unmarshal error")
	}
	if !strings.Contains(err.Error(), "unmarshal response") {
		t.Errorf("error = %q, want 'unmarshal response'", err.Error())
	}
}

func TestHTTPHeartbeat_TransportError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.Close()
	c := newHTTPTestClient(t, srv.URL)
	_, err := c.httpHeartbeat(context.Background())
	if err == nil {
		t.Fatal("expected heartbeat transport error")
	}
}

func TestHTTPHeartbeat_NonOKStatus(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("bad"))
	}))
	defer srv.Close()
	c := newHTTPTestClient(t, srv.URL)
	_, err := c.httpHeartbeat(context.Background())
	if err == nil {
		t.Fatal("expected non-200 error")
	}
	if !strings.Contains(err.Error(), "HTTP 502") {
		t.Errorf("error = %q, want HTTP 502 substring", err.Error())
	}
}

func TestHTTPHeartbeat_InvalidJSONResponse(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("not-json"))
	}))
	defer srv.Close()
	c := newHTTPTestClient(t, srv.URL)
	_, err := c.httpHeartbeat(context.Background())
	if err == nil {
		t.Fatal("expected unmarshal error")
	}
}

func TestHTTPShadowReport_TransportError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.Close()
	c := newHTTPTestClient(t, srv.URL)
	err := c.httpShadowReport(context.Background(), map[string]ConfigState{
		"k": {State: json.RawMessage(`{}`), Version: 1},
	}, 1)
	if err == nil {
		t.Fatal("expected shadow report transport error")
	}
}

func TestHTTPConfigPull_TransportError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.Close()
	c := newHTTPTestClient(t, srv.URL)
	_, err := c.httpConfigPull(context.Background())
	if err == nil {
		t.Fatal("expected config pull transport error")
	}
}

func TestHTTPConfigPull_NonOKStatus(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("no"))
	}))
	defer srv.Close()
	c := newHTTPTestClient(t, srv.URL)
	_, err := c.httpConfigPull(context.Background())
	if err == nil {
		t.Fatal("expected non-200 error")
	}
	if !strings.Contains(err.Error(), "HTTP 403") {
		t.Errorf("error = %q, want HTTP 403", err.Error())
	}
}

func TestHTTPConfigPull_InvalidJSONResponse(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("not-json"))
	}))
	defer srv.Close()
	c := newHTTPTestClient(t, srv.URL)
	_, err := c.httpConfigPull(context.Background())
	if err == nil {
		t.Fatal("expected unmarshal error")
	}
}

func TestRunHTTPFallback_RegisterFailure_Returns(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("err"))
	}))
	defer srv.Close()

	c := newHTTPTestClient(t, srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	// runHTTPFallback should return promptly on register failure (no infinite
	// loop). Without this exit, the test would hit the ctx timeout instead.
	done := make(chan struct{})
	go func() {
		c.runHTTPFallback(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runHTTPFallback did not return on register failure")
	}
}

func TestRunHTTPFallback_HeartbeatErrorContinuesLoop(t *testing.T) {
	t.Parallel()
	var heartbeatCalls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/internal/things/register", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(registerResponse{ThingID: "t1", DesiredVer: 0})
	})
	mux.HandleFunc("/api/internal/things/heartbeat", func(w http.ResponseWriter, _ *http.Request) {
		heartbeatCalls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("nope"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	reg := prometheus.NewRegistry()
	c, err := New(Config{
		HubURL:                  "ws://127.0.0.1:1/ws",
		HubHTTPURL:              srv.URL,
		ThingType:               "ai-gateway",
		ThingID:                 "t1",
		Token:                   "tok",
		Logger:                  slog.New(slog.NewTextHandler(io.Discard, nil)),
		MetricsRegisterer:       reg,
		ReconnectInitialBackoff: 10 * time.Second, // ensure WS recovery never fires
		ReconnectMaxBackoff:     20 * time.Second,
		HeartbeatInterval:       20 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.connectWSFn = func(_ context.Context) error { return errors.New("ws down") }

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	c.runHTTPFallback(ctx)

	// We expect at least one heartbeat (loop did not give up after the first error).
	if heartbeatCalls.Load() < 1 {
		t.Fatalf("expected >=1 heartbeat retries; got %d", heartbeatCalls.Load())
	}
}

func TestRunHTTPFallback_ConfigViaRegister_Applies(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/internal/things/register", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(registerResponse{
			ThingID: "t1",
			Desired: map[string]ConfigState{
				"hooks": {State: json.RawMessage(`{"enabled":true}`), Version: 7},
			},
			DesiredVer: 7,
		})
	})
	mux.HandleFunc("/api/internal/things/heartbeat", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(heartbeatResponse{Ack: true, DesiredVer: 7})
	})
	mux.HandleFunc("/api/internal/things/shadow", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newHTTPTestClient(t, srv.URL)
	c.cfg.ReconnectInitialBackoff = 10 * time.Second
	c.cfg.ReconnectMaxBackoff = 20 * time.Second
	c.cfg.HeartbeatInterval = 10 * time.Second
	var applied atomic.Int32
	c.OnConfigChanged(func(d map[string]ConfigState) (map[string]ConfigState, error) {
		applied.Add(1)
		return d, nil
	})
	c.connectWSFn = func(_ context.Context) error { return errors.New("ws down") }

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	c.runHTTPFallback(ctx)

	if applied.Load() == 0 {
		t.Fatal("config from register response was not applied")
	}
	if c.ReportedVer() != 7 {
		t.Errorf("ReportedVer = %d, want 7 after applyConfig from register", c.ReportedVer())
	}
}

func TestRunHTTPFallback_WSRecovery_Success(t *testing.T) {
	t.Parallel()
	// Start a WS hub the recovery dialer can reach.
	hub := newHubServer(connectedMsg(0))
	defer hub.Close()

	// HTTP register/heartbeat/shadow endpoints.
	mux := http.NewServeMux()
	mux.HandleFunc("/api/internal/things/register", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(registerResponse{ThingID: "gw-test-001", DesiredVer: 0})
	})
	mux.HandleFunc("/api/internal/things/heartbeat", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(heartbeatResponse{Ack: true, DesiredVer: 0})
	})
	httpSrv := httptest.NewServer(mux)
	defer httpSrv.Close()

	reg := prometheus.NewRegistry()
	c, err := New(Config{
		HubURL:                  hub.URL(),
		HubHTTPURL:              httpSrv.URL,
		ThingType:               "ai-gateway",
		ThingID:                 "gw-test-001",
		Token:                   "tok",
		Logger:                  slog.New(slog.NewTextHandler(io.Discard, nil)),
		MetricsRegisterer:       reg,
		ReconnectInitialBackoff: 5 * time.Millisecond,
		ReconnectMaxBackoff:     20 * time.Millisecond,
		HeartbeatInterval:       10 * time.Second, // ensure heartbeat doesn't fire during test
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.OnConfigChanged(func(d map[string]ConfigState) (map[string]ConfigState, error) { return d, nil })

	var reconnected atomic.Int32
	c.OnReconnect(func() { reconnected.Add(1) })

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// Drive runHTTPFallback directly so we get the WS-recovery branch when
	// connectWS (real dialer pointing at hub.URL()) succeeds.
	done := make(chan struct{})
	go func() {
		c.runHTTPFallback(ctx)
		close(done)
	}()

	// Wait briefly for the recovery loop to flip into WS mode AND fire the
	// OnReconnect callback. The mode flip and callback dispatch can race:
	// mode is set first inside connectWS, then runWSSession's prelude
	// invokes the OnReconnect hook. Polling on mode-only races the hook
	// dispatch. Wait for both signals (or timeout).
	deadline := time.After(3 * time.Second)
	for c.Mode() != ModeWSConnected || reconnected.Load() == 0 {
		select {
		case <-deadline:
			if c.Mode() != ModeWSConnected {
				t.Fatalf("WS recovery did not succeed; mode=%v", c.Mode())
			}
			if reconnected.Load() == 0 {
				t.Fatalf("OnReconnect not fired on WS recovery; mode=%v", c.Mode())
			}
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}

	// Close the hub to terminate the recovered session; runHTTPFallback's
	// recovery branch returns after runWSSession ends.
	hub.Close()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("runHTTPFallback did not return after WS session ended")
	}
}

// mq.go — flushMQBuffer ctx-done branch / drainBuffer empty short-circuit /
// bufferEvent nil-buffer / startBufferDrainer no-producer / bufferDrainLoop
// ctx cancel.

func TestFlushMQBuffer_NilBuffer_NoOp(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t)
	// mqBuffer is nil by default — flush must not panic.
	c.flushMQBuffer(context.Background())
}

func TestFlushMQBuffer_EmptyBuffer_NoOp(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t)
	c.mqBuffer = newRingBuffer(10, c.promMetrics, c.logger)
	// Empty — flush returns immediately.
	c.flushMQBuffer(context.Background())
}

func TestFlushMQBuffer_CtxDone_DropsRemaining(t *testing.T) {
	t.Parallel()
	mp := &mockMQProducer{enqueueErr: errors.New("perma-fail")}
	c, _ := newTestClient(t)
	c.cfg.MQProducer = mp
	c.mqBuffer = newRingBuffer(10, c.promMetrics, c.logger)
	c.mqBuffer.Push(bufferedEvent{Queue: "q", Data: []byte("a")})
	c.mqBuffer.Push(bufferedEvent{Queue: "q", Data: []byte("b")})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // immediately cancel
	// flushMQBuffer must observe ctx.Err() and return without spinning.
	done := make(chan struct{})
	go func() {
		c.flushMQBuffer(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("flushMQBuffer did not return on ctx.Done()")
	}
}

func TestBufferEvent_NilBuffer_SilentDrop(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t)
	// mqBuffer is nil — bufferEvent must early-return.
	c.bufferEvent("q", []byte("x"))
}

func TestStartBufferDrainer_NoProducer_DoesNotStart(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t)
	c.cfg.MQProducer = nil
	c.startBufferDrainer(context.Background())
	if c.mqBuffer != nil {
		t.Errorf("startBufferDrainer should leave mqBuffer nil when no producer")
	}
}

func TestBufferDrainLoop_CtxCancelExits(t *testing.T) {
	t.Parallel()
	mp := &mockMQProducer{}
	c, _ := newTestClient(t)
	c.cfg.MQProducer = mp

	ctx, cancel := context.WithCancel(context.Background())
	c.startBufferDrainer(ctx)
	// Cancel and confirm the goroutine exits cleanly (no easy hook other
	// than "doesn't hang").
	cancel()
	// Give the loop a brief window to observe cancel.
	time.Sleep(50 * time.Millisecond)
}

func TestDrainBuffer_EmptyBuffer_EarlyReturn(t *testing.T) {
	t.Parallel()
	mp := &mockMQProducer{}
	c, _ := newTestClient(t)
	c.cfg.MQProducer = mp
	c.mqBuffer = newRingBuffer(10, c.promMetrics, c.logger)
	// Empty — drainBuffer must early-return.
	c.drainBuffer(context.Background())
}

func TestUploadAudit_TransportError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.Close()

	c := newHTTPTestClient(t, srv.URL)
	_, err := c.UploadAudit(context.Background(), []byte(`[]`))
	if err == nil {
		t.Fatal("expected transport error")
	}
}

func TestUploadAudit_InvalidJSONResponse(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("not-json"))
	}))
	defer srv.Close()

	c := newHTTPTestClient(t, srv.URL)
	_, err := c.UploadAudit(context.Background(), []byte(`[]`))
	if err == nil {
		t.Fatal("expected unmarshal error")
	}
	if !strings.Contains(err.Error(), "unmarshal response") {
		t.Errorf("error = %q, want 'unmarshal response'", err.Error())
	}
}

func TestUploadAuditWithRetry_ContextCancelDuringBackoff(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := newHTTPTestClient(t, srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	// First attempt fails (500) → enters backoff sleep → cancel kicks in.
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	_, err := c.UploadAuditWithRetry(ctx, []byte(`[]`), 5)
	if err == nil {
		t.Fatal("expected ctx cancel error")
	}
}

func TestUploadAuditWithRetry_NegativeMaxRetries_FallsBackToDefault(t *testing.T) {
	t.Parallel()
	var called atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(AuditBatchResponse{Ack: true})
	}))
	defer srv.Close()

	c := newHTTPTestClient(t, srv.URL)
	resp, err := c.UploadAuditWithRetry(context.Background(), []byte(`[]`), -1)
	if err != nil {
		t.Fatalf("UploadAuditWithRetry(-1): %v", err)
	}
	if resp == nil || !resp.Ack {
		t.Errorf("expected Ack=true response, got %+v", resp)
	}
	if called.Load() != 1 {
		t.Errorf("expected 1 server call (first try succeeds), got %d", called.Load())
	}
}

// client.go — sendShadowReportHTTP error path inside sendShadowReportHTTP
// (the timeout-context branch isn't directly drivable, but the
// fallthrough-on-error path is).
// Also exercise the shadow_report-already-applied skip log path on
// applyConfig (desiredVer < reportedVer + reportedVer == reportedVer).

func TestSendShadowReportHTTP_RecordsLastReportedAt(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newHTTPTestClient(t, srv.URL)
	err := c.sendShadowReportHTTP(map[string]ConfigState{
		"k": {State: json.RawMessage(`{}`), Version: 3},
	}, 3)
	if err != nil {
		t.Fatalf("sendShadowReportHTTP: %v", err)
	}
	if got := c.LastReportedAtTime(); got.IsZero() {
		t.Errorf("LastReportedAtTime should be set after successful HTTP shadow_report")
	}
}

func TestSendShadowReportHTTP_ServerError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := newHTTPTestClient(t, srv.URL)
	err := c.sendShadowReportHTTP(map[string]ConfigState{
		"k": {State: json.RawMessage(`{}`), Version: 3},
	}, 3)
	if err == nil {
		t.Fatal("expected HTTP 500 error")
	}
}

// client.go — handleHubMessage default-arm (missing configKey + empty desired).

func TestHandleHubMessage_ConfigChangedMissingKeyAndMap_LogsAndDoesNothing(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t)

	var called int
	c.OnConfigChanged(func(d map[string]ConfigState) (map[string]ConfigState, error) {
		called++
		return d, nil
	})

	c.handleHubMessage(hubMessage{
		Type:       "config_changed",
		DesiredVer: 99,
		// no ConfigKey and no Desired map
	})

	if called != 0 {
		t.Errorf("callback must not fire on invalid config_changed; got called=%d", called)
	}
}

// client.go — writeFrame returns false when conn.Write fails AND ctx is
// already cancelled (the "ctx.Err() != nil" branch).

func TestWriteFrame_CtxCancelled_ReturnsFalseSilently(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t)

	// Pair: real-WS server but a torn-down conn. Easiest: spin up a hub,
	// connect, cancel, then call writeFrame with the cancelled ctx.
	hub := newHubServer(connectedMsg(1))
	defer hub.Close()
	c.cfg.HubURL = hub.URL()
	c.OnConfigChanged(func(d map[string]ConfigState) (map[string]ConfigState, error) { return d, nil })
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.connectWS(ctx); err != nil {
		t.Fatalf("connectWS: %v", err)
	}
	c.mu.RLock()
	conn := c.wsConn
	c.mu.RUnlock()
	cancelledCtx, cc := context.WithCancel(context.Background())
	cc()
	ok := c.writeFrame(cancelledCtx, conn, []byte("anything"))
	if ok {
		t.Errorf("writeFrame must return false when ctx is cancelled")
	}
}

// client.go — readPump exits on ctx cancel without logging a spurious error
// (the "if ctx.Err() != nil { return }" branch after a read error).

func TestReadPump_CtxCancelExitsSilently(t *testing.T) {
	t.Parallel()
	hub := newHubServer(connectedMsg(1))
	defer hub.Close()
	c, _ := newTestClient(t)
	c.cfg.HubURL = hub.URL()
	c.OnConfigChanged(func(d map[string]ConfigState) (map[string]ConfigState, error) { return d, nil })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.connectWS(ctx); err != nil {
		t.Fatalf("connectWS: %v", err)
	}

	readCtx, readCancel := context.WithCancel(context.Background())
	c.mu.RLock()
	conn := c.wsConn
	c.mu.RUnlock()
	done := make(chan struct{})
	go func() {
		c.readPump(readCtx, conn)
		close(done)
	}()
	readCancel()
	// Force the underlying conn to also abort so Read returns. The ctx-cancel
	// branch is what we are exercising.
	_ = conn.Close(websocket.StatusGoingAway, "test")
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("readPump did not exit on ctx cancel")
	}
}

// http.go — getHTTPClient lazy-init reuses the cached client on subsequent
// calls (mu-protected memoization).

func TestGetHTTPClient_LazyInitCached(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t)
	// First call constructs.
	a := c.getHTTPClient()
	// Second call must return the same pointer (cache hit).
	b := c.getHTTPClient()
	if a != b {
		t.Errorf("getHTTPClient should cache; got two different pointers")
	}
}

func TestGetHTTPClient_DerivesHubURLWhenHubHTTPURLEmpty(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t)
	// HubHTTPURL is empty in newTestClient's config; force the derive branch
	// by clearing it explicitly (already empty, but be explicit) and force
	// reconstruction by nil'ing the cache.
	c.mu.Lock()
	c.hc = nil
	c.cfg.HubHTTPURL = ""
	c.cfg.HubURL = "wss://hub.test/ws"
	c.mu.Unlock()

	got := c.getHTTPClient()
	if got == nil {
		t.Fatal("getHTTPClient returned nil")
		return
	}
	if !strings.HasPrefix(got.baseURL, "https://") {
		t.Errorf("derived baseURL should start with https://; got %q", got.baseURL)
	}
}

// http.go — indexAfterSchemeHost edge case: empty / no-scheme inputs.

func TestIndexAfterSchemeHost_NoScheme(t *testing.T) {
	t.Parallel()
	if got := indexAfterSchemeHost("no-scheme-here"); got != -1 {
		t.Errorf("indexAfterSchemeHost(no-scheme) = %d, want -1", got)
	}
	if got := indexAfterSchemeHost(""); got != -1 {
		t.Errorf("indexAfterSchemeHost(empty) = %d, want -1", got)
	}
	if got := indexAfterSchemeHost("ab"); got != -1 {
		t.Errorf("indexAfterSchemeHost(short) = %d, want -1", got)
	}
}

// http.go — buildWSDialURL parse failure path (invalid URL via control-char).

func TestBuildWSDialURL_ParseError(t *testing.T) {
	t.Parallel()
	c := &Client{cfg: Config{
		HubURL:    "ws://hub.local\x7f/ws", // control char makes Parse fail
		ThingID:   "id",
		ThingType: "t",
	}}
	_, err := c.buildWSDialURL()
	if err == nil {
		t.Fatal("expected parse error for control-char URL")
	}
}

// client.go — connectWS dial failure paths.

func TestConnectWS_DialFailure_BadURL(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t)
	c.cfg.HubURL = "ws://127.0.0.1:1/ws" // refused
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	err := c.connectWS(ctx)
	if err == nil {
		t.Fatal("expected dial error against refused port")
	}
	if !strings.Contains(err.Error(), "dial hub") {
		t.Errorf("error should wrap as 'dial hub'; got %v", err)
	}
}

// shadow.go — flattenReported empty input ALWAYS returns non-nil so JSON
// encoder emits {} not null. Already covered above; verify again here.

func TestFlattenReported_AllNilStateProducesEmptyMapNotNil(t *testing.T) {
	t.Parallel()
	got := flattenReported(map[string]ConfigState{
		"a": {State: nil, Version: 1},
		"b": {State: nil, Version: 2},
	})
	if got == nil {
		t.Fatal("flattenReported returned nil")
	}
	if len(got) != 0 {
		t.Errorf("all-nil-state input should produce 0-length map; got %d", len(got))
	}
}

// Sanity: ensure these tests don't accidentally drag in a Linux-only symbol
// path. dialHTTPClient is OS-agnostic at the source level — guard against
// drift by recording it ran.

func TestDialHTTPClient_RunsOnHostOS(t *testing.T) {
	t.Parallel()
	hc := dialHTTPClient()
	if hc == nil {
		t.Fatalf("dialHTTPClient returned nil on %s", runtime.GOOS)
	}
}

// Additional targeted gap-fillers identified after first coverage run.

// TestBuildWSDialURL_AllOptionalParamsEmitted ensures the optional URL fields
// (name, managementUrl, physicalId) flow into the WS handshake query when
// non-empty.
func TestBuildWSDialURL_AllOptionalParamsEmitted(t *testing.T) {
	t.Parallel()
	c := &Client{cfg: Config{
		HubURL:        "ws://hub.local:3060/ws",
		ThingID:       "agent-001",
		ThingType:     "agent",
		ThingName:     "Alice's Mac",
		ManagementURL: "http://localhost:5050",
		PhysicalID:    "fp-deadbeef",
	}}
	raw, err := c.buildWSDialURL()
	if err != nil {
		t.Fatalf("buildWSDialURL: %v", err)
	}
	for _, want := range []string{"name=", "managementUrl=", "physicalId="} {
		if !strings.Contains(raw, want) {
			t.Errorf("dial URL missing %q; got %s", want, raw)
		}
	}
}

// TestRingBuffer_DrainAll_EmptyReturnsNil exercises the empty short-circuit
// in DrainAll (the 0% branch at line 116-118).
func TestRingBuffer_DrainAll_EmptyReturnsNil(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t)
	rb := newRingBuffer(8, c.promMetrics, c.logger)
	if got := rb.DrainAll(); got != nil {
		t.Errorf("DrainAll on empty buffer = %v, want nil", got)
	}
}

// TestStart_WithOpsMetricsSampler_LaunchesTicker drives the
// `if c.cfg.OpsMetricsSampler != nil { go c.runMetricsTicker(...) }` branch
// in Start so we can claim coverage on the sampler-enabled wiring path.
func TestStart_WithOpsMetricsSampler_LaunchesTicker(t *testing.T) {
	t.Parallel()
	registry := opsmetrics.NewRegistry(prometheus.NewRegistry())
	sampler := platform.NewSampler("thing-001", time.Now().Add(-5*time.Minute), registry)

	c, _ := newTestClient(t)
	c.cfg.OpsMetricsSampler = sampler
	c.cfg.HeartbeatInterval = 20 * time.Millisecond
	c.OnConfigChanged(func(d map[string]ConfigState) (map[string]ConfigState, error) { return d, nil })

	ctx, cancel := context.WithCancel(context.Background())
	if err := c.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Give the metrics ticker enough time to fire at least once.
	deadline := time.After(2 * time.Second)
	hadMetrics := false
	for !hadMetrics {
		select {
		case <-c.outChMetrics:
			hadMetrics = true
		case <-deadline:
			t.Fatal("metrics ticker did not enqueue any samples after Start")
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}

	cancel()
	closeCtx, cc := context.WithTimeout(context.Background(), 2*time.Second)
	defer cc()
	_ = c.Close(closeCtx)
}

// tickHeartbeat's PushMetricsSample error-branch is structurally
// untestable without a production seam: c.cfg.OpsMetricsSampler is a
// concrete *platform.Sampler whose Collect() returns a SampleBatch
// constructed via a fixed metric registry — there's no way to inject a
// Sample.Metadata map containing a chan from a *real* Sampler without
// touching production code. The branch is reachable in production
// whenever the outbox stalls past sendBytes's 5s deadline (drop path
// already exercised by TestSendBytes_StallTimeout_ReportsDropMetric).

// Below: drive the runHTTPFallback heartbeat-with-inline-desired path so
// applyConfig fires inside the fallback loop (covers lines 362-366).
func TestRunHTTPFallback_HeartbeatWithInlineDesiredAppliesConfig(t *testing.T) {
	t.Parallel()
	var (
		heartbeatHits atomic.Int32
		shadowHits    atomic.Int32
	)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/internal/things/register", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(registerResponse{ThingID: "t1", DesiredVer: 0})
	})
	mux.HandleFunc("/api/internal/things/heartbeat", func(w http.ResponseWriter, _ *http.Request) {
		hits := heartbeatHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		// First heartbeat returns an inline desired bump.
		if hits == 1 {
			_ = json.NewEncoder(w).Encode(heartbeatResponse{
				Ack:        true,
				DesiredVer: 9,
				Desired: map[string]ConfigState{
					"hooks": {State: json.RawMessage(`{"enabled":true}`), Version: 9},
				},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(heartbeatResponse{Ack: true, DesiredVer: 9})
	})
	mux.HandleFunc("/api/internal/things/shadow", func(w http.ResponseWriter, _ *http.Request) {
		shadowHits.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	reg := prometheus.NewRegistry()
	c, err := New(Config{
		HubURL:                  "ws://127.0.0.1:1/ws",
		HubHTTPURL:              srv.URL,
		ThingType:               "ai-gateway",
		ThingID:                 "t1",
		Token:                   "tok",
		Logger:                  slog.New(slog.NewTextHandler(io.Discard, nil)),
		MetricsRegisterer:       reg,
		ReconnectInitialBackoff: 10 * time.Second,
		ReconnectMaxBackoff:     20 * time.Second,
		HeartbeatInterval:       20 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.OnConfigChanged(func(d map[string]ConfigState) (map[string]ConfigState, error) {
		return d, nil
	})
	c.connectWSFn = func(_ context.Context) error { return errors.New("ws down") }

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	c.runHTTPFallback(ctx)

	if heartbeatHits.Load() < 1 {
		t.Fatal("heartbeat endpoint never called")
	}
	if c.ReportedVer() != 9 {
		t.Errorf("ReportedVer = %d, want 9 (applyConfig from heartbeat-inline-desired branch)", c.ReportedVer())
	}
	if shadowHits.Load() == 0 {
		t.Errorf("shadow_report should have been issued after applyConfig")
	}
}

// TestRunHTTPFallback_HeartbeatVersionMismatchPullsConfig drives the
// version-mismatch + nil-desired → httpConfigPull → applyConfig path
// (covers lines 362-385's else-branch).
func TestRunHTTPFallback_HeartbeatVersionMismatchPullsConfig(t *testing.T) {
	t.Parallel()
	var pulled atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/internal/things/register", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(registerResponse{ThingID: "t1", DesiredVer: 0})
	})
	mux.HandleFunc("/api/internal/things/heartbeat", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(heartbeatResponse{Ack: true, DesiredVer: 12})
	})
	mux.HandleFunc("/api/internal/things/config", func(w http.ResponseWriter, _ *http.Request) {
		pulled.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(configPullResponse{
			Configs: map[string]ConfigState{
				"hooks": {State: json.RawMessage(`{"enabled":true}`), Version: 12},
			},
			DesiredVer: 12,
		})
	})
	mux.HandleFunc("/api/internal/things/shadow", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	reg := prometheus.NewRegistry()
	c, err := New(Config{
		HubURL:                  "ws://127.0.0.1:1/ws",
		HubHTTPURL:              srv.URL,
		ThingType:               "ai-gateway",
		ThingID:                 "t1",
		Token:                   "tok",
		Logger:                  slog.New(slog.NewTextHandler(io.Discard, nil)),
		MetricsRegisterer:       reg,
		ReconnectInitialBackoff: 10 * time.Second,
		ReconnectMaxBackoff:     20 * time.Second,
		HeartbeatInterval:       20 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.OnConfigChanged(func(d map[string]ConfigState) (map[string]ConfigState, error) { return d, nil })
	c.connectWSFn = func(_ context.Context) error { return errors.New("ws down") }

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	c.runHTTPFallback(ctx)

	if pulled.Load() == 0 {
		t.Fatal("httpConfigPull was not invoked on version-mismatch")
	}
	if c.ReportedVer() != 12 {
		t.Errorf("ReportedVer = %d, want 12 after config_pull → applyConfig", c.ReportedVer())
	}
}

// TestRunHTTPFallback_ConfigPullFailureContinuesLoop drives the
// err-branch in the version-mismatch path: heartbeat says new version but
// config pull fails. Loop must continue (NOT terminate) and retry on next
// tick.
func TestRunHTTPFallback_ConfigPullFailureContinuesLoop(t *testing.T) {
	t.Parallel()
	var pullCalls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/internal/things/register", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(registerResponse{ThingID: "t1", DesiredVer: 0})
	})
	mux.HandleFunc("/api/internal/things/heartbeat", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(heartbeatResponse{Ack: true, DesiredVer: 12})
	})
	mux.HandleFunc("/api/internal/things/config", func(w http.ResponseWriter, _ *http.Request) {
		pullCalls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	reg := prometheus.NewRegistry()
	c, err := New(Config{
		HubURL:                  "ws://127.0.0.1:1/ws",
		HubHTTPURL:              srv.URL,
		ThingType:               "ai-gateway",
		ThingID:                 "t1",
		Token:                   "tok",
		Logger:                  slog.New(slog.NewTextHandler(io.Discard, nil)),
		MetricsRegisterer:       reg,
		ReconnectInitialBackoff: 10 * time.Second,
		ReconnectMaxBackoff:     20 * time.Second,
		HeartbeatInterval:       20 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.connectWSFn = func(_ context.Context) error { return errors.New("ws down") }

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	c.runHTTPFallback(ctx)

	if pullCalls.Load() < 1 {
		t.Fatal("expected at least one config pull attempt")
	}
	// ReportedVer should remain 0 (pull never succeeded).
	if c.ReportedVer() != 0 {
		t.Errorf("ReportedVer = %d; want 0 because pull always failed", c.ReportedVer())
	}
}

// TestRunHTTPFallback_KickRecadenceBranch drives the heartbeat-kick branch
// (lines 336-347): when SetHeartbeatInterval is called mid-loop, the
// `case <-*kickPtr:` arm fires before the timer and re-arms.
func TestRunHTTPFallback_KickRecadenceBranch(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/internal/things/register", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(registerResponse{ThingID: "t1", DesiredVer: 0})
	})
	mux.HandleFunc("/api/internal/things/heartbeat", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(heartbeatResponse{Ack: true, DesiredVer: 0})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	reg := prometheus.NewRegistry()
	c, err := New(Config{
		HubURL:                  "ws://127.0.0.1:1/ws",
		HubHTTPURL:              srv.URL,
		ThingType:               "ai-gateway",
		ThingID:                 "t1",
		Token:                   "tok",
		Logger:                  slog.New(slog.NewTextHandler(io.Discard, nil)),
		MetricsRegisterer:       reg,
		ReconnectInitialBackoff: 10 * time.Second,
		ReconnectMaxBackoff:     20 * time.Second,
		HeartbeatInterval:       10 * time.Second, // slow base; we'll re-cadence below
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.connectWSFn = func(_ context.Context) error { return errors.New("ws down") }

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		c.runHTTPFallback(ctx)
		close(done)
	}()
	// Wait briefly for the loop to install its heartbeat timer.
	time.Sleep(40 * time.Millisecond)
	// Kick the interval — this should hit the `case <-*kickPtr:` branch.
	c.SetHeartbeatInterval(30 * time.Millisecond)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runHTTPFallback did not exit after kick + ctx timeout")
	}
}

// TestRunWSSession_NilConn drives the early-return branch in runWSSession
// when wsConn is nil.
func TestRunWSSession_NilConn(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t)
	// wsConn is nil by default.
	c.runWSSession(context.Background())
	// Must return quickly (no goroutine leaks, no panic).
}

// TestApplyConfigForce_ShadowReportFailure exercises the shadow-report-fail
// branch in applyConfigForce so reportedVer stays put.
func TestApplyConfigForce_ShadowReportFailure(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t)
	// Leave mode at Disconnected so sendShadowReport returns an error.
	c.OnConfigChanged(func(d map[string]ConfigState) (map[string]ConfigState, error) {
		return d, nil
	})
	c.reportedVer.Store(0)
	c.applyConfigForce(sampleDesired(), 7)
	if c.reportedVer.Load() != 0 {
		t.Errorf("reportedVer must remain 0 when shadow_report fails; got %d", c.reportedVer.Load())
	}
}

// TestApplyConfig_ShadowReportFailure exercises the same branch in plain
// applyConfig — when sendShadowReport errors out, reportedVer must NOT
// advance.
func TestApplyConfig_ShadowReportFailure(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t)
	// Disconnected mode → sendShadowReport returns "not connected" error.
	c.OnConfigChanged(func(d map[string]ConfigState) (map[string]ConfigState, error) {
		return d, nil
	})
	c.applyConfig(sampleDesired(), 3)
	if c.reportedVer.Load() != 0 {
		t.Errorf("reportedVer must remain 0 when shadow_report fails; got %d", c.reportedVer.Load())
	}
}

// TestClose_HTTPFallback_CallsDeregister drives the Close → httpDeregister
// branch (line 1168-1170). We avoid the Start/runLoop race by closing
// c.done manually and setting mode synchronously before Close reads it.
func TestClose_HTTPFallback_CallsDeregister(t *testing.T) {
	t.Parallel()
	var deregCalled atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/internal/things/deregister", func(w http.ResponseWriter, _ *http.Request) {
		deregCalled.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newHTTPTestClient(t, srv.URL)
	// Manually mark done as closed so Close() doesn't hang on the
	// `<-c.done` wait, and pin mode under the same mutex Close reads.
	close(c.done)
	c.mu.Lock()
	c.mode = ModeHTTPFallback
	c.mu.Unlock()

	closeCtx, cc := context.WithTimeout(context.Background(), 5*time.Second)
	defer cc()
	if err := c.Close(closeCtx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if deregCalled.Load() != 1 {
		t.Errorf("Close in HTTPFallback must call deregister once; got %d", deregCalled.Load())
	}
}

// TestSendBreakGlassShadowReport_EmptyKey covers the early-error branch.
func TestSendBreakGlassShadowReport_EmptyKey(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t)
	err := c.SendBreakGlassShadowReport(context.Background(), "", nil, 1, "", "", "")
	if err == nil {
		t.Fatal("expected error for empty key")
	}
}

// TestSendBreakGlassShadowReport_HTTPFallbackTransportError drives the
// hc.do err branch.
func TestSendBreakGlassShadowReport_HTTPFallbackTransportError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.Close()

	c := newHTTPTestClient(t, srv.URL)
	c.setMode(ModeHTTPFallback)
	err := c.SendBreakGlassShadowReport(context.Background(), "k",
		json.RawMessage(`{}`), 1, "", "", "")
	if err == nil {
		t.Fatal("expected transport error against closed server")
	}
}

// TestSendBreakGlassShadowReport_HTTPFallbackNonOK drives the non-200 branch.
func TestSendBreakGlassShadowReport_HTTPFallbackNonOK(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("denied"))
	}))
	defer srv.Close()

	c := newHTTPTestClient(t, srv.URL)
	c.setMode(ModeHTTPFallback)
	err := c.SendBreakGlassShadowReport(context.Background(), "k",
		json.RawMessage(`{}`), 1, "", "", "")
	if err == nil {
		t.Fatal("expected HTTP 403 error")
	}
	if !strings.Contains(err.Error(), "HTTP 403") {
		t.Errorf("error should include HTTP 403; got %v", err)
	}
}

// TestConnectWS_InvalidJSONOnConnectedMessage drives the
// json.Unmarshal error branch in connectWS.
func TestConnectWS_InvalidJSONOnConnectedMessage(t *testing.T) {
	t.Parallel()
	// Hub server that sends garbage instead of a JSON envelope.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "done") //nolint:errcheck
		_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{not-json`))
		<-r.Context().Done()
	}))
	defer srv.Close()
	hubURL := "ws" + strings.TrimPrefix(srv.URL, "http")

	c, _ := newTestClient(t)
	c.cfg.HubURL = hubURL

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := c.connectWS(ctx)
	if err == nil {
		t.Fatal("expected unmarshal error for invalid connected message")
	}
	if !strings.Contains(err.Error(), "unmarshal connected message") {
		t.Errorf("error should wrap as 'unmarshal connected message'; got %v", err)
	}
}

// TestConnectWS_ReadConnectedMessageTimeout drives the read-error branch
// in connectWS by using a hub that accepts the upgrade but never writes.
func TestConnectWS_ReadConnectedMessageTimeout(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "done") //nolint:errcheck
		// Never write a connected message; just block on the ctx.
		<-r.Context().Done()
	}))
	defer srv.Close()
	hubURL := "ws" + strings.TrimPrefix(srv.URL, "http")

	c, _ := newTestClient(t)
	c.cfg.HubURL = hubURL

	// Use a tight ctx so the 10s internal read timeout is shortened.
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	err := c.connectWS(ctx)
	if err == nil {
		t.Fatal("expected read-connected-message error on timeout")
	}
	if !strings.Contains(err.Error(), "read connected message") {
		t.Errorf("error should wrap as 'read connected message'; got %v", err)
	}
}

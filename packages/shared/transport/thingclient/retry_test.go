package thingclient

import (
	"context"
	"fmt"
	"github.com/goccy/go-json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// ── live token via TokenFn ──────────────────────────────────────────────────

func TestCurrentToken_FallbackAndLive(t *testing.T) {
	t.Parallel()

	// nil TokenFn → static Token.
	c, _ := newTestClient(t)
	if got := c.currentToken(); got != "test-token" {
		t.Errorf("nil TokenFn: currentToken = %q, want static Token", got)
	}

	// TokenFn returning "" → fall back to static Token (never send empty bearer).
	c.cfg.TokenFn = func() string { return "" }
	if got := c.currentToken(); got != "test-token" {
		t.Errorf("empty TokenFn: currentToken = %q, want static Token fallback", got)
	}

	// TokenFn returning a value → that value wins, and a later rotation is
	// reflected immediately (no captured snapshot).
	var live atomic.Value
	live.Store("tok-v1")
	c.cfg.TokenFn = func() string { return live.Load().(string) }
	if got := c.currentToken(); got != "tok-v1" {
		t.Errorf("live TokenFn: currentToken = %q, want tok-v1", got)
	}
	live.Store("tok-v2")
	if got := c.currentToken(); got != "tok-v2" {
		t.Errorf("after rotation: currentToken = %q, want tok-v2 (live, not snapshot)", got)
	}
}

// TestConnectWS_UsesLiveTokenOnReconnect proves the WS handshake reads the
// rotated token on the next dial rather than the value captured at Start:
// the Authorization header the Hub sees changes after rotation.
func TestConnectWS_UsesLiveTokenOnReconnect(t *testing.T) {
	reg := prometheus.NewRegistry()
	hub := newHubServer(connectedMsg(1))
	defer hub.Close()

	var live atomic.Value
	live.Store("tok-before")

	cfg := testConfig(hub.URL(), reg)
	cfg.Token = "" // force TokenFn to be the only source
	cfg.TokenFn = func() string { return live.Load().(string) }

	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.OnConfigChanged(func(d map[string]ConfigState) (map[string]ConfigState, error) { return d, nil })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := c.connectWS(ctx); err != nil {
		t.Fatalf("connectWS (before): %v", err)
	}
	if got := hub.AuthHeader(); got != "Bearer tok-before" {
		t.Fatalf("handshake auth = %q, want Bearer tok-before", got)
	}

	// Rotate the token; the next dial must carry the new value.
	live.Store("tok-after")
	if err := c.connectWS(ctx); err != nil {
		t.Fatalf("connectWS (after): %v", err)
	}
	if got := hub.AuthHeader(); got != "Bearer tok-after" {
		t.Fatalf("handshake auth after rotation = %q, want Bearer tok-after (live token)", got)
	}
}

// TestHTTPFallback_UsesLiveTokenPerRequest proves the HTTP fallback transport
// reads the live token on every request: rotating the token between
// two heartbeats changes the Authorization header the server observes.
func TestHTTPFallback_UsesLiveTokenPerRequest(t *testing.T) {
	var seen []string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(heartbeatResponse{Ack: true, DesiredVer: 0})
	}))
	defer srv.Close()

	var live atomic.Value
	live.Store("http-tok-v1")

	reg := prometheus.NewRegistry()
	c, err := New(Config{
		HubURL:            "ws://unused/ws",
		HubHTTPURL:        srv.URL,
		ThingType:         "ai-gateway",
		ThingID:           "gw-1",
		TokenFn:           func() string { return live.Load().(string) },
		Logger:            testLogger(),
		MetricsRegisterer: reg,
		MetricsNamespace:  "test",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := context.Background()
	if _, err := c.httpHeartbeat(ctx); err != nil {
		t.Fatalf("heartbeat 1: %v", err)
	}
	live.Store("http-tok-v2")
	if _, err := c.httpHeartbeat(ctx); err != nil {
		t.Fatalf("heartbeat 2: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("server saw %d requests, want 2", len(seen))
	}
	if seen[0] != "Bearer http-tok-v1" {
		t.Errorf("request 1 auth = %q, want Bearer http-tok-v1", seen[0])
	}
	if seen[1] != "Bearer http-tok-v2" {
		t.Errorf("request 2 auth = %q, want Bearer http-tok-v2 (live per-request token)", seen[1])
	}
}

// ── per-key delta dispatches only the changed key ───────────────────────────

// TestConfigChangedDelta_DispatchesOnlyChangedKey proves a per-key
// config_changed delta re-applies ONLY the changed key — siblings already in
// the desired cache are not re-run — and the resulting shadow_report carries
// only that one key (so the Hub per-key merge updates it without touching
// siblings).
func TestConfigChangedDelta_DispatchesOnlyChangedKey(t *testing.T) {
	c, _ := newTestClient(t)
	setWSConnected(t, c)

	// Cache holds two keys from the connect snapshot, already converged at v1.
	c.mu.Lock()
	c.desiredCache = map[string]ConfigState{
		"alpha": {State: json.RawMessage(`{"a":1}`), Version: 1},
		"beta":  {State: json.RawMessage(`{"b":1}`), Version: 1},
	}
	c.mu.Unlock()
	c.desiredVer.Store(1)
	c.reportedVer.Store(1)

	var gotKeys []string
	c.OnConfigChanged(func(d map[string]ConfigState) (map[string]ConfigState, error) {
		for k := range d {
			gotKeys = append(gotKeys, k)
		}
		return d, nil
	})

	// Only alpha changes, at v2.
	c.handleHubMessage(hubMessage{
		Type:       "config_changed",
		ConfigKey:  "alpha",
		State:      json.RawMessage(`{"a":2}`),
		DesiredVer: 2,
	})

	if len(gotKeys) != 1 || gotKeys[0] != "alpha" {
		t.Fatalf("callback received keys %v; want exactly [alpha] (per-key dispatch, no sibling re-apply)", gotKeys)
	}

	msg := readShadowReport(t, c)
	if _, ok := msg.Reported["alpha"]; !ok {
		t.Error("shadow_report missing the changed key 'alpha'")
	}
	if _, ok := msg.Reported["beta"]; ok {
		t.Error("shadow_report must NOT carry sibling 'beta' on a single-key delta")
	}
	if c.reportedVer.Load() != 2 {
		t.Errorf("reportedVer = %d, want 2 after single-key apply", c.reportedVer.Load())
	}
	// desiredCache still holds both keys for SnapshotDesired / retry.
	if len(c.SnapshotDesired()) != 2 {
		t.Errorf("desiredCache lost a key; SnapshotDesired = %v", c.SnapshotDesired())
	}
}

// ── bounded proactive retry that clears on success ──────────────────────────

// flakyCallback fails the named key for the first failFor apply attempts, then
// succeeds. Thread-safe (apply runs on the dispatch goroutine + retry timer).
func flakyCallback(c *Client, key string, failFor int32) OnConfigChangedFunc {
	var attempts atomic.Int32
	return func(d map[string]ConfigState) (map[string]ConfigState, error) {
		n := attempts.Add(1)
		cs, ok := d[key]
		if !ok {
			return map[string]ConfigState{}, nil
		}
		if n <= failFor {
			err := fmt.Errorf("transient failure attempt %d", n)
			c.Outcomes().Record(key, cs.Version, err)
			return map[string]ConfigState{}, err
		}
		c.Outcomes().Record(key, cs.Version, nil)
		return map[string]ConfigState{key: cs}, nil
	}
}

// startedTestClient returns a client wired as if Start() ran (running + a live
// retryCtx) with a tiny retry backoff so the proactive retry fires fast.
func startedTestClient(t *testing.T) (*Client, context.CancelFunc) {
	t.Helper()
	reg := prometheus.NewRegistry()
	c, err := New(Config{
		HubURL:                 "wss://hub.test/ws",
		ThingType:              "agent",
		ThingID:                "agent-001",
		Token:                  "tok",
		Logger:                 testLogger(),
		MetricsRegisterer:      reg,
		MetricsNamespace:       "test",
		RetryInitialBackoff:    time.Millisecond,
		RetryMaxBackoff:        5 * time.Millisecond,
		MaxConfigRetryAttempts: 5,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.retryMu.Lock()
	c.retryCtx = ctx
	c.retryMu.Unlock()
	c.running.Store(true)
	return c, cancel
}

func TestRetry_FailedKeyRetriedAndClearsOnSuccess(t *testing.T) {
	c, cancel := startedTestClient(t)
	defer cancel()
	setWSConnected(t, c)

	// Drain reports in the background so the WS outbox never blocks.
	go func() {
		for {
			select {
			case <-c.outChControl:
			case <-time.After(2 * time.Second):
				return
			}
		}
	}()

	c.mu.Lock()
	c.desiredCache = map[string]ConfigState{"hooks": {State: json.RawMessage(`{"x":1}`), Version: 5}}
	c.mu.Unlock()
	c.desiredVer.Store(5)

	// Fail the first apply, succeed on the retry.
	c.OnConfigChanged(flakyCallback(c, "hooks", 1))

	// Initial delta fails → key parked, reportedVer held, retry timer armed.
	c.handleHubMessage(hubMessage{
		Type: "config_changed", ConfigKey: "hooks",
		State: json.RawMessage(`{"x":1}`), DesiredVer: 5,
	})
	if c.outstandingFailures() != 1 {
		t.Fatalf("expected hooks parked after initial failure; outstanding=%d", c.outstandingFailures())
	}
	if c.reportedVer.Load() != 0 {
		t.Fatalf("reportedVer advanced to %d on failure; want 0 (held)", c.reportedVer.Load())
	}

	// The proactive retry must converge the key without any further delta.
	waitFor(t, time.Second, func() bool {
		return c.outstandingFailures() == 0 && c.reportedVer.Load() == 5
	})

	// Timer cancelled once nothing is outstanding.
	c.retryMu.Lock()
	timerNil := c.retryTimer == nil
	attempt := c.retryAttempt
	c.retryMu.Unlock()
	if !timerNil {
		t.Error("retry timer should be cancelled after convergence")
	}
	if attempt != 0 {
		t.Errorf("retryAttempt should reset to 0 after convergence; got %d", attempt)
	}
	if got := testutil.ToFloat64(c.promMetrics.configApplies.WithLabelValues("success")); got < 1 {
		t.Errorf("expected at least one success apply after retry; got %v", got)
	}
}

func TestRetry_BoundedGivesUpAfterMaxAttempts(t *testing.T) {
	c, cancel := startedTestClient(t)
	defer cancel()
	setWSConnected(t, c)
	go func() {
		for {
			select {
			case <-c.outChControl:
			case <-time.After(2 * time.Second):
				return
			}
		}
	}()

	c.mu.Lock()
	c.desiredCache = map[string]ConfigState{"hooks": {State: json.RawMessage(`{"x":1}`), Version: 5}}
	c.mu.Unlock()
	c.desiredVer.Store(5)

	// Always fail.
	c.OnConfigChanged(flakyCallback(c, "hooks", 1<<30))

	c.handleHubMessage(hubMessage{
		Type: "config_changed", ConfigKey: "hooks",
		State: json.RawMessage(`{"x":1}`), DesiredVer: 5,
	})

	// Budget (5) is finite: the timer eventually stops re-arming and the key
	// stays parked (drift held) for the next reconnect snapshot.
	waitFor(t, 2*time.Second, func() bool {
		c.retryMu.Lock()
		defer c.retryMu.Unlock()
		return c.retryTimer == nil && c.retryAttempt >= c.cfg.MaxConfigRetryAttempts
	})

	if c.outstandingFailures() != 1 {
		t.Errorf("failed key should remain parked after budget exhaustion; outstanding=%d", c.outstandingFailures())
	}
	if c.reportedVer.Load() != 0 {
		t.Errorf("reportedVer must stay held at 0 while the key never applies; got %d", c.reportedVer.Load())
	}
}

func TestFireRetry_NoOpWhenCancelledOrEmpty(t *testing.T) {
	t.Parallel()
	c, cancel := startedTestClient(t)
	called := false
	c.OnConfigChanged(func(d map[string]ConfigState) (map[string]ConfigState, error) {
		called = true
		return d, nil
	})

	// failed empty → fireRetry returns without dispatching.
	c.fireRetry()
	if called {
		t.Error("fireRetry dispatched with no failed keys")
	}

	// cancelled retryCtx → fireRetry returns even with a failed key present.
	c.retryMu.Lock()
	c.failed["hooks"] = failureEntry{state: ConfigState{Version: 1}}
	c.retryMu.Unlock()
	cancel()
	c.fireRetry()
	if called {
		t.Error("fireRetry dispatched after retryCtx cancellation")
	}
}

// TestDispatch_CleanRoundHeldByPriorFailure proves the cross-round hold: a
// round that applies cleanly does NOT advance the global reportedVer while a
// key from a prior round is still outstanding (per-key dispatch removed the
// implicit full-map retry, so the global version must stay behind until every
// key converges).
func TestDispatch_CleanRoundHeldByPriorFailure(t *testing.T) {
	c, _ := newTestClient(t) // not started → no background timer
	setWSConnected(t, c)

	// beta is a still-desired key with a prior, unresolved failure; alpha is
	// the key this round applies cleanly. Both live in desiredCache (a failed
	// key is always still desired), so the prune keeps beta parked.
	c.mu.Lock()
	c.desiredCache = map[string]ConfigState{
		"alpha": {State: json.RawMessage(`{"a":1}`), Version: 3},
		"beta":  {State: json.RawMessage(`{"b":1}`), Version: 2},
	}
	c.mu.Unlock()
	c.retryMu.Lock()
	c.failed["beta"] = failureEntry{state: ConfigState{State: json.RawMessage(`{"b":1}`), Version: 2}}
	c.retryMu.Unlock()

	c.OnConfigChanged(func(d map[string]ConfigState) (map[string]ConfigState, error) {
		return d, nil // alpha applies cleanly
	})

	c.dispatchConfig(map[string]ConfigState{
		"alpha": {State: json.RawMessage(`{"a":1}`), Version: 3},
	}, 3, false)

	msg := readShadowReport(t, c)
	if _, ok := msg.Reported["alpha"]; !ok {
		t.Error("clean round should still report its applied key 'alpha'")
	}
	if msg.ReportedVer != 0 {
		t.Errorf("held report wire version = %d, want 0 (prior failure holds it)", msg.ReportedVer)
	}
	if c.reportedVer.Load() != 0 {
		t.Errorf("reportedVer advanced to %d despite outstanding 'beta'; want 0", c.reportedVer.Load())
	}
	if c.outstandingFailures() != 1 {
		t.Errorf("'beta' should still be outstanding; got %d", c.outstandingFailures())
	}
	// The clean apply still counts as a success.
	if got := testutil.ToFloat64(c.promMetrics.configApplies.WithLabelValues("success")); got != 1 {
		t.Errorf("config_applies{success} = %v, want 1", got)
	}
}

// TestRetryTimer_CancelOnClearAndAlreadyPending exercises the timer lifecycle
// directly: a second arm while one is pending is a no-op, and clearing the last
// failure cancels the pending timer (the "cancels on success" half).
// A large backoff guarantees the timer is still pending (not yet fired) when we
// clear it.
func TestRetryTimer_CancelOnClearAndAlreadyPending(t *testing.T) {
	c, cancel := startedTestClient(t)
	defer cancel()
	c.cfg.RetryInitialBackoff = 10 * time.Second // won't fire during the test
	c.cfg.RetryMaxBackoff = 10 * time.Second

	hooks := map[string]ConfigState{"hooks": {State: json.RawMessage(`{"x":1}`), Version: 5}}
	c.mu.Lock()
	c.desiredCache = hooks
	c.mu.Unlock()

	// First reconcile with a failed key arms a pending timer.
	c.reconcileFailures(hooks, map[string]ConfigState{})
	c.retryMu.Lock()
	armed := c.retryTimer != nil
	// Arming again while one is pending must be a no-op (no leaked timer swap).
	before := c.retryTimer
	c.armRetryLocked()
	samePending := c.retryTimer == before
	c.retryMu.Unlock()
	if !armed {
		t.Fatal("first reconcile should arm a pending retry timer")
	}
	if !samePending {
		t.Error("armRetryLocked should be a no-op while a timer is already pending")
	}

	// Clearing the last failure must cancel the pending timer.
	c.reconcileFailures(hooks, hooks) // hooks now applied → cleared
	c.retryMu.Lock()
	cleared := c.retryTimer == nil && len(c.failed) == 0 && c.retryAttempt == 0
	c.retryMu.Unlock()
	if !cleared {
		t.Error("clearing the last failure must cancel the pending timer and reset state")
	}
}

// TestReconcileFailures_PrunesKeyRemovedFromDesired proves a failed key whose
// template is later deleted (no longer in desiredCache) is dropped from the
// failure registry, so it cannot hold reportedVer behind desired_ver forever.
func TestReconcileFailures_PrunesKeyRemovedFromDesired(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t)

	// gamma failed earlier and is still desired; delta failed earlier but its
	// template has since been deleted (absent from desiredCache).
	c.mu.Lock()
	c.desiredCache = map[string]ConfigState{"gamma": {State: json.RawMessage(`{"g":1}`), Version: 4}}
	c.mu.Unlock()
	c.retryMu.Lock()
	c.failed["gamma"] = failureEntry{state: ConfigState{Version: 4}}
	c.failed["delta"] = failureEntry{state: ConfigState{Version: 3}}
	c.retryMu.Unlock()

	// A no-op reconcile (empty round) still runs the prune.
	c.reconcileFailures(map[string]ConfigState{}, map[string]ConfigState{})

	c.retryMu.Lock()
	_, gammaKept := c.failed["gamma"]
	_, deltaKept := c.failed["delta"]
	n := len(c.failed)
	c.retryMu.Unlock()
	if !gammaKept {
		t.Error("gamma is still desired and failing; must stay parked")
	}
	if deltaKept {
		t.Error("delta was removed from desired; must be pruned from the failure registry")
	}
	if n != 1 {
		t.Errorf("failed registry size = %d, want 1", n)
	}
}

func TestRetryBackoff_MonotonicAndCapped(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t)
	c.cfg.RetryInitialBackoff = time.Second
	c.cfg.RetryMaxBackoff = 8 * time.Second

	// attempt < 1 clamps to 1 (≈ initial + jitter).
	if d := c.retryBackoff(0); d < time.Second || d > time.Second*5/4 {
		t.Errorf("retryBackoff(0) = %v; want ~[1s,1.25s]", d)
	}
	// Grows exponentially up to the cap (with up to 25 percent jitter).
	if d := c.retryBackoff(2); d < 2*time.Second || d > 2*time.Second*5/4 {
		t.Errorf("retryBackoff(2) = %v; want ~[2s,2.5s]", d)
	}
	// A large attempt is capped at RetryMaxBackoff (+jitter).
	capped := c.retryBackoff(50)
	if capped < 8*time.Second || capped > 8*time.Second*5/4 {
		t.Errorf("retryBackoff(50) = %v; want capped near 8s", capped)
	}
}

func TestArmRetry_NoOpWhenNotRunning(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t) // running=false, retryCtx=nil
	c.retryMu.Lock()
	c.failed["hooks"] = failureEntry{state: ConfigState{Version: 1}}
	c.armRetryLocked()
	timer := c.retryTimer
	c.retryMu.Unlock()
	if timer != nil {
		t.Error("armRetryLocked must not schedule a timer before Start()")
	}
}

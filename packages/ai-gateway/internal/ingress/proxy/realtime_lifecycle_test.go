package proxy

// realtime_lifecycle_test.go — AC-4's session-concurrency caps and AC-5's
// lifecycle machinery: revoked-VK sever within a recheck tick, fail-open on
// indeterminate recheck errors, the hard session guard, the
// dial-succeeded/accept-failed no-orphan window, and finalize-exactly-once
// under -race.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/auth/vkauth"
)

// TestRealtimeCaps_ThirdSessionRefused is AC-4: the built-in per-VK session
// cap (2) refuses a third concurrent session with 429 + Retry-After, counts
// live sessions on the gauge, and admits a new session once one closes.
func TestRealtimeCaps_ThirdSessionRefused(t *testing.T) {
	provider := newRTProvider(t, rtHoldScript)
	deps, _ := realtimeDeps(t, provider.srv.URL)
	srv, done := rtServer(t, deps)

	c1, _, err := rtDial(t, srv.URL, "gpt-realtime", nil)
	if err != nil {
		t.Fatalf("session 1: %v", err)
	}
	c2, _, err := rtDial(t, srv.URL, "gpt-realtime", nil)
	if err != nil {
		t.Fatalf("session 2: %v", err)
	}
	waitForGauge(t, 2)

	_, resp3, err := rtDial(t, srv.URL, "gpt-realtime", nil)
	if err == nil {
		t.Fatal("third concurrent session was admitted; want 429")
	}
	if resp3 == nil || resp3.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("third session response = %+v, want 429", resp3)
	}
	if resp3.Header.Get("Retry-After") != "1" {
		t.Errorf("Retry-After = %q, want 1", resp3.Header.Get("Retry-After"))
	}
	waitHandlerDone(t, done, 1) // the refused upgrade completed

	// Closing one session frees its slot; the next dial admits.
	_ = c1.Close(websocket.StatusNormalClosure, "done")
	waitHandlerDone(t, done, 1)
	c4, _, err := rtDial(t, srv.URL, "gpt-realtime", nil)
	if err != nil {
		t.Fatalf("session after slot release: %v", err)
	}
	_ = c4.Close(websocket.StatusNormalClosure, "done")
	_ = c2.Close(websocket.StatusNormalClosure, "done")
	waitHandlerDone(t, done, 2)
	waitForGauge(t, 0)
}

func waitForGauge(t *testing.T, want float64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if testutil.ToFloat64(realtimeSessionsActive) == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("nexus_realtime_sessions_active never reached %v (now %v)",
		want, testutil.ToFloat64(realtimeSessionsActive))
}

// TestRealtimeRecheck_RevokedSevers is AC-5's sever arm: a definitive
// negative on the periodic by-hash recheck sends one policy error event and
// closes 1008 within a tick.
func TestRealtimeRecheck_RevokedSevers(t *testing.T) {
	setRealtimeTimers(t, time.Hour, time.Second, 30*time.Millisecond, time.Hour)
	provider := newRTProvider(t, rtHoldScript)
	auth := &rtVKAuth{meta: entitledVKMeta("vk-rev"), hash: "h-rev",
		recheckFn: func(_ int) (vkauth.RecheckOutcome, error) { return vkauth.RecheckRevoked, nil }}
	deps, prod := realtimeDeps(t, provider.srv.URL, func(d *Deps) { d.VKAuth = auth })
	srv, done := rtServer(t, deps)

	c, _, err := rtDial(t, srv.URL, "gpt-realtime", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	frames, closeStatus := readUntilClose(t, c, 5*time.Second)
	if closeStatus != websocket.StatusPolicyViolation {
		t.Errorf("close status = %v, want 1008", closeStatus)
	}
	var sawEvent bool
	for _, f := range frames {
		if strings.Contains(string(f), "REALTIME_VK_REVOKED") {
			sawEvent = true
		}
	}
	if !sawEvent {
		t.Error("no terminal error event before the revocation close")
	}
	if st := provider.waitClosed(t, 5*time.Second); st != websocket.StatusPolicyViolation {
		t.Errorf("provider close = %v, want 1008 (no orphaned paid session)", st)
	}
	waitHandlerDone(t, done, 1)
	_, sessions := splitRealtimeRows(drainAudit(t, deps.AuditWriter, prod))
	if len(sessions) != 1 || sessions[0].ErrorCode == nil || *sessions[0].ErrorCode != "REALTIME_VK_REVOKED" {
		t.Fatalf("session rows = %+v, want one REALTIME_VK_REVOKED", sessions)
	}
}

// TestRealtimeRecheck_IndeterminateFailsOpen is AC-5's fail-open arm: a
// lookup error does NOT sever — the session keeps relaying, the failure
// counter increments, and the next tick retries.
func TestRealtimeRecheck_IndeterminateFailsOpen(t *testing.T) {
	setRealtimeTimers(t, time.Hour, time.Second, 20*time.Millisecond, time.Hour)
	provider := newRTProvider(t, nil) // echo
	auth := &rtVKAuth{meta: entitledVKMeta("vk-blip"), hash: "h-blip",
		recheckFn: func(_ int) (vkauth.RecheckOutcome, error) {
			return vkauth.RecheckActive, errors.New("redis blip")
		}}
	deps, _ := realtimeDeps(t, provider.srv.URL, func(d *Deps) { d.VKAuth = auth })
	srv, done := rtServer(t, deps)

	before := testutil.ToFloat64(realtimeVKRecheckFailures)
	c, _, err := rtDial(t, srv.URL, "gpt-realtime", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	// Let several ticks fail, then prove the session still relays.
	deadline := time.Now().Add(5 * time.Second)
	for auth.calls() < 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if auth.calls() < 3 {
		t.Fatal("recheck ticks did not retry after the indeterminate error")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Write(ctx, websocket.MessageText, []byte(`{"type":"ping"}`)); err != nil {
		t.Fatalf("session severed by an indeterminate recheck error: %v", err)
	}
	if _, _, err := c.Read(ctx); err != nil {
		t.Fatalf("echo after failed rechecks: %v", err)
	}
	if delta := testutil.ToFloat64(realtimeVKRecheckFailures) - before; delta < 3 {
		t.Errorf("recheck failure counter advanced %v, want >= 3", delta)
	}
	_ = c.Close(websocket.StatusNormalClosure, "done")
	waitHandlerDone(t, done, 1)
}

// TestRealtimeSessionGuard_Fires is AC-5's guard arm: the hard cap closes a
// wedged session (with a live client) and stamps the reason. The short ping
// interval also exercises the healthy-ping branch alongside.
func TestRealtimeSessionGuard_Fires(t *testing.T) {
	setRealtimeTimers(t, 30*time.Millisecond, time.Second, time.Hour, 250*time.Millisecond)
	provider := newRTProvider(t, rtHoldScript)
	deps, prod := realtimeDeps(t, provider.srv.URL)
	srv, done := rtServer(t, deps)

	start := time.Now()
	c, _, err := rtDial(t, srv.URL, "gpt-realtime", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	frames, _ := readUntilClose(t, c, 5*time.Second)
	if elapsed := time.Since(start); elapsed < 200*time.Millisecond {
		t.Errorf("session ended after %v — before the guard could have fired", elapsed)
	}
	var sawEvent bool
	for _, f := range frames {
		if strings.Contains(string(f), "REALTIME_SESSION_GUARD") {
			sawEvent = true
		}
	}
	if !sawEvent {
		t.Error("no guard error event before the close")
	}
	provider.waitClosed(t, 5*time.Second)
	waitHandlerDone(t, done, 1)
	_, sessions := splitRealtimeRows(drainAudit(t, deps.AuditWriter, prod))
	if len(sessions) != 1 || sessions[0].ErrorCode == nil || *sessions[0].ErrorCode != "REALTIME_SESSION_GUARD" {
		t.Fatalf("session rows = %+v, want one REALTIME_SESSION_GUARD", sessions)
	}
}

// TestRealtimeAcceptFail_NoOrphanNoLeak is AC-5's dial-succeeded/
// accept-failed window: the provider conn is closed (no orphaned paid
// session), the caps slot is released (three sequential failures on a
// cap-2 VK — a leak would 429 the third), and each attempt stamps its own
// session row.
func TestRealtimeAcceptFail_NoOrphanNoLeak(t *testing.T) {
	provider := newRTProvider(t, rtHoldScript)
	deps, prod := realtimeDeps(t, provider.srv.URL)
	h := NewHandler(deps).ServeRealtime(rtIngress())

	for i := range 3 {
		// A recorder is not an http.Hijacker, so Accept fails AFTER the
		// upstream dial succeeded — exactly the window under test.
		req := httptest.NewRequest(http.MethodGet, "/v1/realtime?model=gpt-realtime", nil)
		req.Header.Set("Upgrade", "websocket")
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Sec-WebSocket-Version", "13")
		req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
		rr := httptest.NewRecorder()
		h(rr, req)
		if rr.Code == http.StatusTooManyRequests {
			t.Fatalf("attempt %d refused 429 — accept-failure leaked a caps slot", i+1)
		}
		// The dialed provider session must be torn down promptly.
		provider.waitClosed(t, 5*time.Second)
	}

	_, sessions := splitRealtimeRows(drainAudit(t, deps.AuditWriter, prod))
	if len(sessions) != 3 {
		t.Fatalf("session rows = %d, want 3 (one per attempt)", len(sessions))
	}
	for _, sessRow := range sessions {
		if sessRow.ErrorCode == nil || *sessRow.ErrorCode != "REALTIME_ACCEPT_FAILED" {
			t.Errorf("session row error = %v, want REALTIME_ACCEPT_FAILED", sessRow.ErrorCode)
		}
	}
}

// TestRealtimeFinalizeOnce_Race runs several sessions whose two sides close
// simultaneously and asserts exactly ONE session row each — the
// finalize-once contract — with the race detector watching the teardown
// paths.
func TestRealtimeFinalizeOnce_Race(t *testing.T) {
	provider := newRTProvider(t, func(ctx context.Context, c *websocket.Conn, _ *rtProvider) {
		// Close from the provider side the moment the first frame arrives —
		// racing the client's own close below.
		_, _, _ = c.Read(ctx)
		_ = c.Close(websocket.StatusNormalClosure, "provider race close")
	})
	deps, prod := realtimeDeps(t, provider.srv.URL, func(d *Deps) {
		// Distinct VKs so the cap never queues the racers.
		d.VKAuth = &rtVKAuth{meta: entitledVKMeta("vk-race"), hash: "h"}
	})
	srv, done := rtServer(t, deps)

	const sessions = 6
	var wg sync.WaitGroup
	for range sessions {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, _, err := rtDial(t, srv.URL, "gpt-realtime", nil)
			if err != nil {
				return // cap contention (cap=2) — refused upgrades finalize too
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"x"}`))
			_ = c.Close(websocket.StatusNormalClosure, "client race close")
		}()
		// Stagger slightly so admitted sessions turn over their cap slots.
		time.Sleep(20 * time.Millisecond)
	}
	wg.Wait()
	waitHandlerDone(t, done, sessions)

	msgs := drainAudit(t, deps.AuditWriter, prod)
	// EXACTLY one row per handler invocation: every session (admitted or
	// cap-refused) finalizes once — never zero, never double.
	if len(msgs) != sessions {
		t.Fatalf("audit rows = %d, want exactly %d (finalize-once per invocation)", len(msgs), sessions)
	}
	seen := map[string]bool{}
	for _, m := range msgs {
		if m.TraceID == "" {
			t.Error("row without a trace id")
		}
		if seen[m.TraceID] {
			t.Errorf("trace id %q appears on two SESSION rows — a session finalized twice", m.TraceID)
		}
		seen[m.TraceID] = true
	}
}

package proxy

// realtime_unit_test.go — direct unit coverage of the relay's failure arms
// that the end-to-end tests cannot reach deterministically: write-failure
// classification, panic recovery in pump and supervisor, dead-client ping
// teardown, abrupt (no-close-frame) upstream drops, dial failover budget +
// pinning, and the dial header/pricing helper edges.

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

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/auth/vkauth"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/ingress/realtimeproxy"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/quota"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	provtarget "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/target"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
)

// newWSPair returns both ends of a live WebSocket: the accepted server-side
// conn (installed into a session under test) and the test's peer conn.
func newWSPair(t *testing.T) (server *websocket.Conn, peer *websocket.Conn) {
	t.Helper()
	connCh := make(chan *websocket.Conn, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{})
		if err != nil {
			t.Errorf("pair accept: %v", err)
			return
		}
		connCh <- c // the hijacked conn outlives the handler return
	}))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	peer, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("pair dial: %v", err)
	}
	t.Cleanup(func() { _ = peer.CloseNow() })
	select {
	case server = <-connCh:
	case <-time.After(5 * time.Second):
		t.Fatal("pair server conn never arrived")
	}
	t.Cleanup(func() { _ = server.CloseNow() })
	return server, peer
}

// newConnSession builds a session wired to two live conn pairs, returning
// the session plus the client-side and upstream-side test peers.
func newConnSession(t *testing.T, q realtimeQuota) (s *realtimeSession, clientPeer, upstreamPeer *websocket.Conn) {
	t.Helper()
	clientSrv, cPeer := newWSPair(t)
	upSrv, uPeer := newWSPair(t)
	s, _ = newMeterTestSession(t, q)
	s.client = clientSrv
	s.upstream = upSrv
	s.frameLimit = 1 << 20
	return s, cPeer, uPeer
}

// drainPeers keeps test peers reading so a session teardown's close
// handshake completes immediately instead of waiting out its internal
// timeout against a silent peer.
func drainPeers(t *testing.T, peers ...*websocket.Conn) {
	t.Helper()
	for _, p := range peers {
		go func(c *websocket.Conn) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			for {
				if _, _, err := c.Read(ctx); err != nil {
					return
				}
			}
		}(p)
	}
}

func TestHandleWriteFailure_Arms(t *testing.T) {
	t.Run("write deadline is a relay error", func(t *testing.T) {
		s, cp, up := newConnSession(t, nil)
		drainPeers(t, cp, up)
		ctx, cancel := context.WithCancel(context.Background())
		s.ctx, s.cancel = ctx, cancel
		s.handleWriteFailure(context.DeadlineExceeded, false)
		_, reason, code := s.state.Snapshot()
		if reason != realtimeproxy.ReasonRelayError || code != "REALTIME_RELAY_ERROR" {
			t.Errorf("= (%v, %q), want relay-error classification", reason, code)
		}
	})
	t.Run("client-destination close attributed to client", func(t *testing.T) {
		s, cp, up := newConnSession(t, nil)
		drainPeers(t, cp, up)
		ctx, cancel := context.WithCancel(context.Background())
		s.ctx, s.cancel = ctx, cancel
		s.handleWriteFailure(websocket.CloseError{Code: websocket.StatusGoingAway}, true)
		_, reason, _ := s.state.Snapshot()
		if reason != realtimeproxy.ReasonClientClosed {
			t.Errorf("reason = %v, want ReasonClientClosed (dst was the client)", reason)
		}
	})
	t.Run("upstream-destination generic failure attributed to upstream", func(t *testing.T) {
		s, cp, up := newConnSession(t, nil)
		drainPeers(t, cp, up)
		ctx, cancel := context.WithCancel(context.Background())
		s.ctx, s.cancel = ctx, cancel
		s.handleWriteFailure(errors.New("broken pipe"), false)
		_, reason, _ := s.state.Snapshot()
		if reason != realtimeproxy.ReasonUpstreamClosed {
			t.Errorf("reason = %v, want ReasonUpstreamClosed", reason)
		}
	})
	t.Run("cascade after teardown changes nothing", func(t *testing.T) {
		s, cp, up := newConnSession(t, nil)
		drainPeers(t, cp, up)
		ctx, cancel := context.WithCancel(context.Background())
		s.ctx, s.cancel = ctx, cancel
		s.teardown(realtimeproxy.ReasonQuotaExceeded, websocket.StatusPolicyViolation)
		s.handleWriteFailure(errors.New("late failure"), false)
		_, reason, _ := s.state.Snapshot()
		if reason != realtimeproxy.ReasonQuotaExceeded {
			t.Errorf("cascade overwrote the originating reason: %v", reason)
		}
	})
}

// TestTeardown_FirstWinsExactlyOnce: the first teardown's reason + status
// reach both peers; later teardowns are no-ops (the finalize-once core).
func TestTeardown_FirstWinsExactlyOnce(t *testing.T) {
	s, cPeer, uPeer := newConnSession(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	s.ctx, s.cancel = ctx, cancel

	// Read both peers concurrently with the teardown so its close handshake
	// completes promptly (a WS close is only acknowledged by a reading peer).
	statusCh := make(chan websocket.StatusCode, 2)
	for _, peer := range []*websocket.Conn{cPeer, uPeer} {
		go func(p *websocket.Conn) {
			rctx, rcancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer rcancel()
			for {
				if _, _, err := p.Read(rctx); err != nil {
					statusCh <- websocket.CloseStatus(err)
					return
				}
			}
		}(peer)
	}

	s.teardown(realtimeproxy.ReasonNormal, websocket.StatusNormalClosure)
	s.teardown(realtimeproxy.ReasonBinaryFrame, websocket.StatusUnsupportedData) // must lose

	for range 2 {
		select {
		case st := <-statusCh:
			if st != websocket.StatusNormalClosure {
				t.Errorf("peer close = %v, want the FIRST teardown's 1000", st)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("peer never observed the close")
		}
	}
	_, reason, code := s.state.Snapshot()
	if reason != realtimeproxy.ReasonNormal || code != "" {
		t.Errorf("= (%v, %q), want the first (normal) reason with no error code", reason, code)
	}
	if s.ctx.Err() == nil {
		t.Error("teardown did not cancel the session context")
	}
}

func TestRecheckVK_EdgeArms(t *testing.T) {
	t.Run("no seam skips silently", func(t *testing.T) {
		s := &realtimeSession{}
		if s.recheckVK() {
			t.Error("recheck severed without a seam")
		}
	})
	t.Run("active outcome continues", func(t *testing.T) {
		s, cp, up := newConnSession(t, nil)
		drainPeers(t, cp, up)
		ctx, cancel := context.WithCancel(context.Background())
		s.ctx, s.cancel = ctx, cancel
		s.rechecker = &rtVKAuth{meta: entitledVKMeta("vk-a"), hash: "h"}
		s.keyHash = "h"
		if s.recheckVK() {
			t.Error("recheck severed an ACTIVE VK")
		}
		if s.ctx.Err() != nil {
			t.Error("active recheck tore the session down")
		}
	})
}

// panicQuota drives the pump's panic-recovery arm through a realistic
// in-tap failure.
type panicQuota struct{}

func (panicQuota) OrgParents() map[string]string { return nil }
func (panicQuota) Check(_ context.Context, _ []quota.CheckLevel, _ quota.CostEstimate, _ *vkauth.VKMeta) *quota.Decision {
	panic("injected quota panic")
}
func (panicQuota) Reconcile(_ context.Context, _ *quota.Decision, _ quota.ActualUsage) {}

// TestRealtimePumpPanic_TearsDownSessionOnly: a panic inside the tap chain
// is recovered, tears down THIS session with 1011, and stamps the relay
// error — the process survives (the test itself is the proof).
func TestRealtimePumpPanic_TearsDownSessionOnly(t *testing.T) {
	s, cPeer, uPeer := newConnSession(t, panicQuota{})

	runDone := make(chan struct{})
	go func() {
		s.run(context.Background())
		close(runDone)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Feed a metered response.done through the upstream side; the tap's
	// quota settle panics.
	if err := uPeer.Write(ctx, websocket.MessageText, []byte(rtDoneFrame(100, 0, 0, 0, 10, 0))); err != nil {
		t.Fatalf("upstream write: %v", err)
	}
	// Drain the upstream peer too — a non-reading peer would stall the
	// teardown close handshake at its internal timeout.
	go func() {
		dctx, dcancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer dcancel()
		for {
			if _, _, err := uPeer.Read(dctx); err != nil {
				return
			}
		}
	}()
	_, closeStatus := readUntilClose(t, cPeer, 10*time.Second)
	if closeStatus != websocket.StatusInternalError {
		t.Errorf("client close = %v, want 1011", closeStatus)
	}
	select {
	case <-runDone:
	case <-time.After(15 * time.Second):
		t.Fatal("session did not finish after the pump panic")
	}
	_, reason, code := s.state.Snapshot()
	if reason != realtimeproxy.ReasonRelayError || code != "REALTIME_RELAY_ERROR" {
		t.Errorf("= (%v, %q), want relay-error", reason, code)
	}
	_ = uPeer.CloseNow()
}

// TestRealtimeSupervisorPanic_TearsDownSessionOnly: a panic in the recheck
// (which touches cache/DB code) is recovered by the supervisor.
func TestRealtimeSupervisorPanic_TearsDownSessionOnly(t *testing.T) {
	setRealtimeTimers(t, time.Hour, time.Second, 20*time.Millisecond, time.Hour)
	provider := newRTProvider(t, rtHoldScript)
	auth := &rtVKAuth{meta: entitledVKMeta("vk-p"), hash: "h-p",
		recheckFn: func(_ int) (vkauth.RecheckOutcome, error) { panic("injected recheck panic") }}
	deps, prod := realtimeDeps(t, provider.srv.URL, func(d *Deps) { d.VKAuth = auth })
	srv, done := rtServer(t, deps)

	c, _, err := rtDial(t, srv.URL, "gpt-realtime", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_, closeStatus := readUntilClose(t, c, 5*time.Second)
	if closeStatus != websocket.StatusInternalError {
		t.Errorf("close = %v, want 1011", closeStatus)
	}
	waitHandlerDone(t, done, 1)
	_, sessions := splitRealtimeRows(drainAudit(t, deps.AuditWriter, prod))
	if len(sessions) != 1 || sessions[0].ErrorCode == nil || *sessions[0].ErrorCode != "REALTIME_RELAY_ERROR" {
		t.Fatalf("session rows = %+v, want one REALTIME_RELAY_ERROR", sessions)
	}
}

// TestRealtimeDeadClient_PingFrees: a client that never reads (so never
// pongs) is detected by the ping loop and its session freed — slots must
// not be pinned by dead clients.
func TestRealtimeDeadClient_PingFrees(t *testing.T) {
	setRealtimeTimers(t, 30*time.Millisecond, 80*time.Millisecond, time.Hour, time.Hour)
	provider := newRTProvider(t, rtHoldScript)
	deps, prod := realtimeDeps(t, provider.srv.URL)
	srv, done := rtServer(t, deps)

	c, _, err := rtDial(t, srv.URL, "gpt-realtime", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	// Never read from c: pings cannot be answered.
	waitHandlerDone(t, done, 1)
	provider.waitClosed(t, 5*time.Second)
	_ = c.CloseNow()

	_, sessions := splitRealtimeRows(drainAudit(t, deps.AuditWriter, prod))
	// Dead-client ping freed the session — a benign close (no error code).
	if len(sessions) != 1 || !benignRealtimeErrorCode(sessions[0].ErrorCode) {
		t.Fatalf("session rows = %+v, want one benign (no error code)", sessions)
	}
}

// TestRealtimeUpstreamAbruptDrop: a provider TCP drop (no close frame) —
// the client still gets the terminal error event and a clean close.
func TestRealtimeUpstreamAbruptDrop(t *testing.T) {
	provider := newRTProvider(t, func(ctx context.Context, c *websocket.Conn, _ *rtProvider) {
		_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"session.created"}`))
		_ = c.CloseNow() // abrupt: no close handshake
	})
	deps, prod := realtimeDeps(t, provider.srv.URL)
	srv, done := rtServer(t, deps)

	c, _, err := rtDial(t, srv.URL, "gpt-realtime", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	frames, closeStatus := readUntilClose(t, c, 5*time.Second)
	if closeStatus != websocket.StatusNormalClosure {
		t.Errorf("client close = %v, want 1000 (nothing to propagate)", closeStatus)
	}
	var sawErrorEvent bool
	for _, f := range frames {
		if strings.Contains(string(f), "REALTIME_UPSTREAM_CLOSED") {
			sawErrorEvent = true
		}
	}
	if !sawErrorEvent {
		t.Error("no terminal error event on the abrupt upstream drop")
	}
	waitHandlerDone(t, done, 1)
	_, sessions := splitRealtimeRows(drainAudit(t, deps.AuditWriter, prod))
	// An upstream drop with no provider error event is a benign session end
	// (the client still received the terminal gateway event above); the row
	// carries no error code.
	if len(sessions) != 1 || !benignRealtimeErrorCode(sessions[0].ErrorCode) {
		t.Fatalf("session rows = %+v, want one benign (no error code)", sessions)
	}
}

// benignRealtimeErrorCode reports whether a session row's error code is the
// no-error sentinel (nil pointer or empty string) — the expected stamp for a
// clean client/upstream close or a dead-client ping teardown.
func benignRealtimeErrorCode(code *string) bool {
	return code == nil || *code == ""
}

// rtPerProviderResolver resolves each provider id to its own base URL, so a
// failover test can make target[0] unreachable and target[1] live.
type rtPerProviderResolver struct{ byProvider map[string]string }

func (r rtPerProviderResolver) Resolve(_ context.Context, providerID, _ string, _ provtarget.ResolveHints) (provcore.CallTarget, error) {
	base, ok := r.byProvider[providerID]
	if !ok {
		return provcore.CallTarget{}, errors.New("no base for provider")
	}
	return provcore.CallTarget{
		ProviderID: providerID, ProviderName: providerID,
		Format: provcore.FormatOpenAI, BaseURL: base, APIKey: "sk-upstream",
		CredentialID: "cred-" + providerID, CredentialName: "cred",
		ProviderModelID: "gpt-realtime-x",
	}, nil
}

// TestRealtimeFailover_AttributesPinnedTarget is the regression for the
// targets[0]-vs-pinned-target bug: when the first routed target fails to
// dial, the session pins targets[1], and EVERY row must attribute the
// PINNED provider/model — not the never-connected first target — or cost
// analytics and the pricing-completeness gate would key off the wrong model.
func TestRealtimeFailover_AttributesPinnedTarget(t *testing.T) {
	live := newRTProvider(t, rtHoldScript)
	deps, prod := realtimeDeps(t, live.srv.URL, func(d *Deps) {
		d.Router = &stubRouterCacheTest{targets: []routingcore.RoutingTarget{
			{ProviderID: "p-dead", ProviderName: "p-dead", ModelID: "m-dead",
				ModelName: "Dead", ProviderModelID: "gpt-dead", AdapterType: "openai"},
			{ProviderID: "p-live", ProviderName: "p-live", ModelID: "m-live",
				ModelName: "Live", ProviderModelID: "gpt-realtime-x", AdapterType: "openai"},
		}}
		// p-dead resolves to a closed port (dial fails fast → failover);
		// p-live resolves to the running fake provider.
		d.Resolver = rtPerProviderResolver{byProvider: map[string]string{
			"p-dead": "http://127.0.0.1:1",
			"p-live": live.srv.URL,
		}}
	})
	srv, done := rtServer(t, deps)

	c, _, err := rtDial(t, srv.URL, "gpt-realtime", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = c.Close(websocket.StatusNormalClosure, "done")
	waitHandlerDone(t, done, 1)

	_, sessions := splitRealtimeRows(drainAudit(t, deps.AuditWriter, prod))
	if len(sessions) != 1 {
		t.Fatalf("session rows = %d, want 1", len(sessions))
	}
	s := sessions[0]
	if s.RoutedModelID != "m-live" || s.RoutedProviderID != "p-live" || s.ModelID != "m-live" {
		t.Fatalf("attribution = model %q / routed %q / provider %q; want the PINNED p-live/m-live, not targets[0] p-dead/m-dead",
			s.ModelID, s.RoutedModelID, s.RoutedProviderID)
	}
	// The pinned provider is the one that received the handshake.
	if q := live.query(0); q.Get("model") != "gpt-realtime-x" {
		t.Fatalf("live provider handshake model = %q, want the resolved gpt-realtime-x", q.Get("model"))
	}
}

// --- dial failover budget + pinning ---------------------------------------

// countingResolver maps providerID → baseURL and counts Resolve calls.
type countingResolver struct {
	mu    sync.Mutex
	bases map[string]string
	calls int
}

func (r *countingResolver) Resolve(_ context.Context, providerID, _ string, _ provtarget.ResolveHints) (provcore.CallTarget, error) {
	r.mu.Lock()
	r.calls++
	base, ok := r.bases[providerID]
	r.mu.Unlock()
	if !ok {
		return provcore.CallTarget{}, errors.New("unknown provider")
	}
	return provcore.CallTarget{
		ProviderID: providerID, ProviderName: "openai", Format: provcore.FormatOpenAI,
		BaseURL: base, APIKey: "sk-upstream", CredentialID: "cred-1",
		ProviderModelID: "gpt-realtime-x",
	}, nil
}

func rtTargetFor(provider string) routingcore.RoutingTarget {
	return routingcore.RoutingTarget{
		ProviderID: provider, ProviderName: "openai", ModelID: "m-rt",
		ModelName: "GPT Realtime", ProviderModelID: "gpt-realtime-x", AdapterType: "openai",
	}
}

// TestRealtimeDial_FailoverPinsFirstSuccess: a bad first target fails over
// to the healthy second within the budget; the session runs pinned to it.
func TestRealtimeDial_FailoverPinsFirstSuccess(t *testing.T) {
	provider := newRTProvider(t, nil) // echo
	resolver := &countingResolver{bases: map[string]string{
		"p-bad":    "ftp://unroutable", // scheme derivation fails → next target
		"p-openai": provider.srv.URL,
	}}
	deps, _ := realtimeDeps(t, provider.srv.URL, func(d *Deps) {
		d.Resolver = resolver
		d.Router = &stubRouterCacheTest{targets: []routingcore.RoutingTarget{
			rtTargetFor("p-bad"), rtTargetFor("p-openai"),
		}}
	})
	srv, done := rtServer(t, deps)

	c, _, err := rtDial(t, srv.URL, "gpt-realtime", nil)
	if err != nil {
		t.Fatalf("failover dial: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Write(ctx, websocket.MessageText, []byte(`{"type":"x"}`)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, _, err := c.Read(ctx); err != nil {
		t.Fatalf("echo through the failed-over target: %v", err)
	}
	_ = c.Close(websocket.StatusNormalClosure, "done")
	waitHandlerDone(t, done, 1)
	if provider.handshakes() == nil || len(provider.handshakes()) != 1 {
		t.Errorf("provider handshakes = %d, want exactly 1 (pinned)", len(provider.handshakes()))
	}
}

// TestRealtimeDial_BudgetCapsAtThreeTargets: with four routed targets all
// failing, only three are attempted before the static 502.
func TestRealtimeDial_BudgetCapsAtThreeTargets(t *testing.T) {
	resolver := &countingResolver{bases: map[string]string{
		"p1": "ftp://x", "p2": "ftp://x", "p3": "ftp://x", "p4": "ftp://x",
	}}
	deps, _ := realtimeDeps(t, "http://127.0.0.1:0", func(d *Deps) {
		d.Resolver = resolver
		d.Router = &stubRouterCacheTest{targets: []routingcore.RoutingTarget{
			rtTargetFor("p1"), rtTargetFor("p2"), rtTargetFor("p3"), rtTargetFor("p4"),
		}}
	})
	rr := doRealtimeHTTP(deps, "/v1/realtime?model=gpt-realtime", true)
	assertErrCode(t, rr, http.StatusBadGateway, "REALTIME_UPSTREAM_UNAVAILABLE")
	resolver.mu.Lock()
	defer resolver.mu.Unlock()
	if resolver.calls != 3 {
		t.Errorf("resolve attempts = %d, want the 3-target budget", resolver.calls)
	}
}

// TestRealtimeDial_EmptyKeyAuthArm: an adapter that refuses to stamp auth
// (empty API key) burns the attempt and ends in the static 502.
func TestRealtimeDial_EmptyKeyAuthArm(t *testing.T) {
	resolver := &countingResolver{bases: map[string]string{"p-openai": "https://api.example.com"}}
	deps, _ := realtimeDeps(t, "http://127.0.0.1:0", func(d *Deps) {
		d.Resolver = emptyKeyResolver{inner: resolver}
	})
	rr := doRealtimeHTTP(deps, "/v1/realtime?model=gpt-realtime", true)
	assertErrCode(t, rr, http.StatusBadGateway, "REALTIME_UPSTREAM_UNAVAILABLE")
}

type emptyKeyResolver struct{ inner *countingResolver }

func (r emptyKeyResolver) Resolve(ctx context.Context, providerID, modelID string, h provtarget.ResolveHints) (provcore.CallTarget, error) {
	ct, err := r.inner.Resolve(ctx, providerID, modelID, h)
	ct.APIKey = ""
	return ct, err
}

// --- helper edges ----------------------------------------------------------

func TestRealtimeDialHeader_Arms(t *testing.T) {
	deps, _ := realtimeDeps(t, "http://127.0.0.1:0")
	h := NewHandler(deps)

	t.Run("unknown adapter format", func(t *testing.T) {
		_, err := h.realtimeDialHeader(provcore.CallTarget{Format: "no-such-format", BaseURL: "https://x", APIKey: "k"})
		if err == nil {
			t.Fatal("want error for an unregistered format")
		}
	})
	t.Run("unparseable base URL", func(t *testing.T) {
		_, err := h.realtimeDialHeader(provcore.CallTarget{Format: provcore.FormatOpenAI, BaseURL: "ht tp://bad url", APIKey: "k"})
		if err == nil {
			t.Fatal("want error for an unparseable base URL")
		}
	})
	t.Run("empty key refused by the adapter", func(t *testing.T) {
		_, err := h.realtimeDialHeader(provcore.CallTarget{Format: provcore.FormatOpenAI, BaseURL: "https://x", APIKey: ""})
		if err == nil {
			t.Fatal("want the adapter's empty-key refusal")
		}
	})
	t.Run("stamps only the provider auth", func(t *testing.T) {
		hdr, err := h.realtimeDialHeader(provcore.CallTarget{Format: provcore.FormatOpenAI, BaseURL: "https://x", APIKey: "sk-abc"})
		if err != nil {
			t.Fatalf("header build: %v", err)
		}
		if hdr.Get("Authorization") != "Bearer sk-abc" {
			t.Errorf("Authorization = %q", hdr.Get("Authorization"))
		}
	})
}

func TestRealtimeDialURL_ErrorMessage(t *testing.T) {
	_, err := realtimeDialURL("ftp://host", "m")
	if err == nil || !strings.Contains(err.Error(), "unsupported base URL scheme") {
		t.Fatalf("err = %v, want the unsupported-scheme message", err)
	}
}

func TestRealtimePricing_Arms(t *testing.T) {
	ctx := context.Background()
	t.Run("nil models dependency", func(t *testing.T) {
		h := &Handler{deps: &Deps{}}
		if m, priced := h.realtimePricing(ctx, "m-rt"); m != nil || priced {
			t.Error("nil Models must report unpriced")
		}
	})
	t.Run("lookup error", func(t *testing.T) {
		h := &Handler{deps: &Deps{Models: rtStubModels{model: nil}}}
		if m, priced := h.realtimePricing(ctx, "m-rt"); m != nil || priced {
			t.Error("lookup failure must report unpriced")
		}
	})
	t.Run("fully priced", func(t *testing.T) {
		h := &Handler{deps: &Deps{Models: rtStubModels{model: rtPricedModel()}}}
		if m, priced := h.realtimePricing(ctx, "m-rt"); m == nil || !priced {
			t.Error("fully-priced model must report priced")
		}
	})
}

func TestRealtimeModelPrices_NilModel(t *testing.T) {
	p := realtimeModelPrices(nil)
	if p.InputUsdPerM != nil || p.AudioInputUsdPerM != nil {
		t.Error("nil model must map to all-nil prices (every component $0)")
	}
}

func TestWarnMeterOnce_NilLoggerAndDedupe(t *testing.T) {
	s, _ := newMeterTestSession(t, nil)
	s.h.deps.Logger = nil
	s.warnMeterOnce("unit:nil-logger", "must not panic without a logger")

	s2, _ := newMeterTestSession(t, nil)
	// First call stores the key; the second must be suppressed (dedupe) —
	// observable as no second WARN, pinned via the sync.Map contract.
	s2.warnMeterOnce("unit:dedupe-key", "first")
	if _, loaded := realtimeMeterWarned.Load("unit:dedupe-key"); !loaded {
		t.Fatal("warn key not recorded for dedupe")
	}
	s2.warnMeterOnce("unit:dedupe-key", "second (suppressed)")
}

// TestBuildResponseRecord_IdentityStamps: response rows carry the VK
// attribution (ApplyVKMeta + fingerprint) and the session's provider fields.
func TestBuildResponseRecord_IdentityStamps(t *testing.T) {
	s, _ := newMeterTestSession(t, nil)
	s.vkMeta.Fingerprint = "fp-16-chars"
	s.vkMeta.Class = "nvk_"
	usage := realtimeproxy.Usage{TextInput: 10, TextOutput: 5}
	rec := s.buildResponseRecord(usage, 0.01, 42, time.Now())
	if rec.VirtualKeyID != s.vkMeta.ID || rec.APIKeyFingerprint != "fp-16-chars" || rec.APIKeyClass != "nvk_" {
		t.Errorf("identity stamps missing: %+v", rec)
	}
	if rec.ProviderID != "p-openai" || rec.ModelID != "m-rt" {
		t.Errorf("provider/model stamps: %q/%q", rec.ProviderID, rec.ModelID)
	}
	if rec.TraceID != "sess-uuid-1" || rec.RequestID == "" || rec.RequestID == rec.TraceID {
		t.Errorf("row identity: id=%q trace=%q", rec.RequestID, rec.TraceID)
	}
	if rec.LatencyMs != 42 || rec.UpstreamTotalMs == nil || *rec.UpstreamTotalMs != 42 {
		t.Errorf("latency stamps: %d / %v", rec.LatencyMs, rec.UpstreamTotalMs)
	}
	if rec.ComplianceCoverage != "none" {
		t.Errorf("coverage = %q, want none", rec.ComplianceCoverage)
	}
}

//go:build darwin

package darwin

import (
	"bytes"
	"fmt"
	"github.com/goccy/go-json"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/platform/api"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/platform/darwin/flow"
	nepkg "github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/platform/darwin/ne"
)

// mockHandler implements api.ConnectionHandler with a fixed decision.
type mockHandler struct {
	decision api.Decision
}

func (m *mockHandler) HandleConnection(api.InterceptedConn) api.Decision { return m.decision }

type gatingHandler struct {
	mockHandler
	engaged bool
}

func (g *gatingHandler) IsKillSwitchEngaged() bool { return g.engaged }

type throttle struct{ on bool }

func (t throttle) IsThrottled() bool { return t.on }

// progHandler returns a per-host decision and can block a host's
// decision on a release gate — used to prove decisions run concurrently
// and never cross between flows.
type progHandler struct {
	byHost map[string]api.Decision
	block  map[string]chan struct{}
	seen   func(api.InterceptedConn)
}

func (h *progHandler) HandleConnection(c api.InterceptedConn) api.Decision {
	if h.seen != nil {
		h.seen(c)
	}
	if h.block != nil {
		if gate, ok := h.block[c.DstHost]; ok {
			<-gate
		}
	}
	if d, ok := h.byHost[c.DstHost]; ok {
		return d
	}
	return api.DecisionPassthrough
}

func newPlatform(h api.ConnectionHandler) *DarwinPlatform {
	return &DarwinPlatform{handler: h, flowSem: make(chan struct{}, flowDecisionConcurrency)}
}

// runFlowNew drives handleNewFlow to completion (including the async
// decision worker) and returns the decision the daemon replied with.
func runFlowNew(t *testing.T, p *DarwinPlatform, msg nepkg.FlowMsg) string {
	t.Helper()
	nc := &neConn{respCh: make(chan []byte, 16)}
	p.handleNewFlow(nc, msg)
	nc.workers.Wait()
	close(nc.respCh)
	last := ""
	for data := range nc.respCh {
		var d nepkg.DecisionMsg
		if err := json.Unmarshal(bytes.TrimSpace(data), &d); err == nil {
			if d.FlowID != msg.FlowID && d.FlowID != "" {
				t.Fatalf("reply flow_id %q does not match request %q — cross-flow bleed", d.FlowID, msg.FlowID)
			}
			last = d.Decision
		}
	}
	return last
}

func TestHandleNewFlow_HandlerNil_Drops(t *testing.T) {
	p := &DarwinPlatform{flowSem: make(chan struct{}, 1)} // handler nil
	if got := runFlowNew(t, p, nepkg.FlowMsg{FlowID: "f1"}); got != "" {
		t.Fatalf("handler-nil must not reply, got %q", got)
	}
}

func TestHandleNewFlow_KillSwitch_FailsOpenPassthrough(t *testing.T) {
	// Kill-switch engaged → MUST reply passthrough and MUST NOT track the
	// flow (paused state must not leak into the audit DB). NE fail-open
	// binding for the kill-switch path.
	p := newPlatform(&gatingHandler{mockHandler: mockHandler{decision: api.DecisionInspect}, engaged: true})
	if got := runFlowNew(t, p, nepkg.FlowMsg{FlowID: "f1", RemoteHost: "api.openai.com"}); got != "passthrough" {
		t.Fatalf("kill-switch must reply passthrough, got %q", got)
	}
	if _, ok := p.loadFlow("f1"); ok {
		t.Fatal("kill-switch flow must NOT be tracked")
	}
}

func TestHandleNewFlow_Backpressure_FailsOpenPassthrough(t *testing.T) {
	p := newPlatform(&mockHandler{decision: api.DecisionInspect})
	p.backpressure = throttle{on: true}
	if got := runFlowNew(t, p, nepkg.FlowMsg{FlowID: "f1"}); got != "passthrough" {
		t.Fatalf("backpressure must shed via passthrough, got %q", got)
	}
	if _, ok := p.loadFlow("f1"); ok {
		t.Fatal("shed flow must not be tracked")
	}
}

func TestHandleNewFlow_Decisions(t *testing.T) {
	for _, tc := range []struct {
		dec  api.Decision
		want string
	}{
		{api.DecisionInspect, "inspect"},
		{api.DecisionDeny, "deny"},
		{api.DecisionPassthrough, "passthrough"},
	} {
		p := newPlatform(&mockHandler{decision: tc.dec})
		if got := runFlowNew(t, p, nepkg.FlowMsg{FlowID: "f1", RemoteHost: "api.x", PID: os.Getpid()}); got != tc.want {
			t.Fatalf("decision %v → %q, want %q", tc.dec, got, tc.want)
		}
		fs, ok := p.loadFlow("f1")
		if !ok || !fs.Ready.Load() || fs.DecisionInt != int(tc.dec) {
			t.Fatalf("flow must be tracked + Ready with decision %v: %+v ok=%v", tc.dec, fs, ok)
		}
		// PID=self → proc meta resolved and published.
		if fs.ProcPID != os.Getpid() {
			t.Errorf("proc meta not published: ProcPID=%d want %d", fs.ProcPID, os.Getpid())
		}
	}
}

func TestHandleNewFlow_ProcessInfoErrNonFatal(t *testing.T) {
	// A non-existent PID makes ProcessInfo fail; the flow is still handled
	// (proc meta empty, decision still written).
	p := newPlatform(&mockHandler{decision: api.DecisionInspect})
	if got := runFlowNew(t, p, nepkg.FlowMsg{FlowID: "f1", PID: 1<<30 + 3}); got != "inspect" {
		t.Fatalf("proc-info failure must be non-fatal, got %q", got)
	}
	fs, ok := p.loadFlow("f1")
	if !ok || !fs.Ready.Load() {
		t.Fatal("flow must be tracked + ready even when proc lookup fails")
	}
}

// TestDecideFlow_PrefersNEBundleID verifies the bundle-attribution fix:
// the NE's kernel-attested sourceAppSigningIdentifier (FlowMsg.BundleID)
// is authoritative and overrides the racy PID→Info.plist lookup. Without
// it, sandboxed / CLI-helper flows (Cursor's `node`, codex) land in the
// audit DB with an empty or wrong bundle and cannot be attributed to the
// app that originated them.
func TestDecideFlow_PrefersNEBundleID(t *testing.T) {
	t.Run("adopted when PID lookup yields nothing", func(t *testing.T) {
		// Bogus PID → ProcessInfoCached fails → PID-derived bundle empty.
		// The NE-supplied bundle must still reach the flow state.
		p := newPlatform(&mockHandler{decision: api.DecisionInspect})
		msg := nepkg.FlowMsg{FlowID: "f1", PID: 1<<30 + 7, BundleID: "com.anthropic.claude-code"}
		if got := runFlowNew(t, p, msg); got != "inspect" {
			t.Fatalf("decision = %q, want inspect", got)
		}
		fs, ok := p.loadFlow("f1")
		if !ok {
			t.Fatal("flow not tracked")
		}
		if fs.ProcBundleID != "com.anthropic.claude-code" {
			t.Fatalf("ProcBundleID = %q, want NE-supplied com.anthropic.claude-code", fs.ProcBundleID)
		}
	})

	t.Run("overrides PID-derived bundle", func(t *testing.T) {
		// Real PID resolves to the test binary (no Info.plist bundle), but
		// the NE bundle is authoritative and must be the published value.
		// The PID path still runs for name/path/user attribution.
		p := newPlatform(&mockHandler{decision: api.DecisionInspect})
		msg := nepkg.FlowMsg{FlowID: "f2", PID: os.Getpid(), BundleID: "codex"}
		if got := runFlowNew(t, p, msg); got != "inspect" {
			t.Fatalf("decision = %q, want inspect", got)
		}
		fs, _ := p.loadFlow("f2")
		if fs.ProcBundleID != "codex" {
			t.Fatalf("ProcBundleID = %q, want NE-supplied codex", fs.ProcBundleID)
		}
		if fs.ProcPID != os.Getpid() {
			t.Errorf("ProcPID = %d, want %d (PID path still runs)", fs.ProcPID, os.Getpid())
		}
	})

	t.Run("empty NE bundle leaves PID path as the source", func(t *testing.T) {
		// Unsigned binary → extension supplies no identifier; the PID→
		// Info.plist result is the fallback and the PID path still runs.
		p := newPlatform(&mockHandler{decision: api.DecisionInspect})
		msg := nepkg.FlowMsg{FlowID: "f3", PID: os.Getpid(), BundleID: ""}
		if got := runFlowNew(t, p, msg); got != "inspect" {
			t.Fatalf("decision = %q, want inspect", got)
		}
		fs, _ := p.loadFlow("f3")
		if fs.ProcPID != os.Getpid() {
			t.Errorf("ProcPID = %d, want %d", fs.ProcPID, os.Getpid())
		}
	})
}

// TestDecideFlow_NoCrossFlowBleed is the core data-flow-integrity test
// for the lock-free concurrent decision path: many flows decided
// concurrently, each with a DISTINCT per-host decision, must each get
// THEIR OWN decision on THEIR OWN flow_id and THEIR OWN tracked state —
// never another flow's. A bug that shares state or mis-keys a reply
// would surface as a mismatch here.
func TestDecideFlow_NoCrossFlowBleed(t *testing.T) {
	const n = 200
	hosts := make(map[string]api.Decision, n)
	wantByFlow := make(map[string]string, n)
	for i := range n {
		host := fmt.Sprintf("host-%d.example", i)
		// Deterministic per-flow decision spread across all three values.
		dec := []api.Decision{api.DecisionInspect, api.DecisionPassthrough, api.DecisionDeny}[i%3]
		hosts[host] = dec
		wantByFlow[fmt.Sprintf("flow-%d", i)] = decisionString(dec)
	}
	p := newPlatform(&progHandler{byHost: hosts})

	nc := &neConn{respCh: make(chan []byte, n)}
	for i := range n {
		p.handleNewFlow(nc, nepkg.FlowMsg{
			FlowID:     fmt.Sprintf("flow-%d", i),
			RemoteHost: fmt.Sprintf("host-%d.example", i),
		})
	}
	nc.workers.Wait()
	close(nc.respCh)

	gotByFlow := make(map[string]string, n)
	for data := range nc.respCh {
		var d nepkg.DecisionMsg
		if err := json.Unmarshal(bytes.TrimSpace(data), &d); err != nil {
			t.Fatalf("decode reply: %v", err)
		}
		if _, dup := gotByFlow[d.FlowID]; dup {
			t.Fatalf("flow %s replied twice", d.FlowID)
		}
		gotByFlow[d.FlowID] = d.Decision
	}
	if len(gotByFlow) != n {
		t.Fatalf("got %d replies, want %d", len(gotByFlow), n)
	}
	for flowID, want := range wantByFlow {
		if got := gotByFlow[flowID]; got != want {
			t.Fatalf("flow %s reply=%q want %q — cross-flow bleed in the reply path", flowID, got, want)
		}
		// And the tracked state must carry the SAME flow's decision.
		fs, ok := p.loadFlow(flowID)
		if !ok || decisionString(api.Decision(fs.DecisionInt)) != want {
			t.Fatalf("flow %s tracked decision mismatch: %+v want %q", flowID, fs, want)
		}
		// Identity field must be this flow's own host, never another's.
		if fs.DstHost() != "host-"+strings.TrimPrefix(flowID, "flow-")+".example" {
			t.Fatalf("flow %s tracked host bled: %q", flowID, fs.DstHost())
		}
	}
}

// TestDecideFlow_ConcurrencyIsReal proves the worker pool is actually
// engaged: a flow whose decision blocks must NOT hold up a second flow.
// If the path were still serial, the fast flow's reply could not arrive
// while the slow flow is parked in HandleConnection.
func TestDecideFlow_ConcurrencyIsReal(t *testing.T) {
	slowGate := make(chan struct{})
	h := &progHandler{
		byHost: map[string]api.Decision{"slow": api.DecisionInspect, "fast": api.DecisionPassthrough},
		block:  map[string]chan struct{}{"slow": slowGate},
	}
	p := newPlatform(h)
	nc := &neConn{respCh: make(chan []byte, 4)}

	p.handleNewFlow(nc, nepkg.FlowMsg{FlowID: "slow", RemoteHost: "slow"}) // parks in handler
	p.handleNewFlow(nc, nepkg.FlowMsg{FlowID: "fast", RemoteHost: "fast"})

	// The fast flow must reply while slow is still blocked.
	select {
	case data := <-nc.respCh:
		var d nepkg.DecisionMsg
		_ = json.Unmarshal(bytes.TrimSpace(data), &d)
		if d.FlowID != "fast" {
			t.Fatalf("first reply was %q, expected the non-blocked 'fast' flow — decision path is serial, not concurrent", d.FlowID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("fast flow did not reply while slow flow blocked — concurrency not engaged")
	}

	close(slowGate) // release slow
	nc.workers.Wait()
}

func TestHandleFlowUpdateHostAndLookup(t *testing.T) {
	p := newPlatform(&mockHandler{decision: api.DecisionInspect})
	fs := &flow.State{FlowID: "f1", DstPort: 443, ProcName: "curl"}
	fs.SetDstHost("1.2.3.4")
	fs.Ready.Store(true)
	p.storeFlow(fs)

	p.handleFlowUpdateHost(nepkg.FlowMsg{FlowID: "f1", Hostname: ""})     // no-op
	p.handleFlowUpdateHost(nepkg.FlowMsg{FlowID: "ghost", Hostname: "x"}) // unknown, no panic
	p.handleFlowUpdateHost(nepkg.FlowMsg{FlowID: "f1", Hostname: "api.openai.com"})

	host, port, _, pm, ok := p.LookupFlowDestination("f1")
	if !ok || host != "api.openai.com" || port != 443 || pm.Name != "curl" {
		t.Fatalf("lookup after update: host=%q port=%d pm=%+v ok=%v", host, port, pm, ok)
	}
	if _, _, _, _, ok := p.LookupFlowDestination("ghost"); ok {
		t.Fatal("unknown flow lookup must return ok=false")
	}
}

// TestDstHost_ReaderUpdateVsBridgeLookup_RaceClean is the regression test
// for the cross-goroutine DstHost race the arch review caught: the IPC
// reader updates DstHost (SNI flow_update_host) while a per-connection
// bridge goroutine reads it via LookupFlowDestination at the same time.
// Plain-field access was a data race (torn string read); DstHost is now an
// atomic.Pointer accessed only through the methods. Under -race, clean.
func TestDstHost_ReaderUpdateVsBridgeLookup_RaceClean(t *testing.T) {
	p := newPlatform(&mockHandler{decision: api.DecisionInspect})
	fs := &flow.State{FlowID: "f1", DstPort: 443}
	fs.SetDstHost("1.2.3.4")
	fs.Ready.Store(true)
	p.storeFlow(fs)

	const iters = 2000
	done := make(chan struct{}, 2)
	go func() { // reader: rewrites DstHost like handleFlowUpdateHost
		for i := range iters {
			p.handleFlowUpdateHost(nepkg.FlowMsg{FlowID: "f1", Hostname: fmt.Sprintf("h%d.example", i)})
		}
		done <- struct{}{}
	}()
	go func() { // bridge: reads DstHost like handleBridgeFlow
		for range iters {
			if host, _, _, _, ok := p.LookupFlowDestination("f1"); !ok || host == "" {
				t.Errorf("lookup returned ok=%v host=%q", ok, host)
				break
			}
		}
		done <- struct{}{}
	}()
	<-done
	<-done
}

// TestLookupFlowDestination_ReadyGate proves the happens-before publish:
// before the worker stores Ready, a reader sees empty process metadata
// (never a half-written struct); after Ready it sees the published meta.
func TestLookupFlowDestination_ReadyGate(t *testing.T) {
	p := newPlatform(&mockHandler{decision: api.DecisionInspect})
	fs := &flow.State{FlowID: "f1", DstPort: 443}
	fs.SetDstHost("h")
	p.storeFlow(fs) // not ready yet

	if _, _, _, pm, ok := p.LookupFlowDestination("f1"); !ok || pm.Name != "" || pm.PID != 0 {
		t.Fatalf("pre-Ready lookup must yield empty proc meta, got %+v ok=%v", pm, ok)
	}
	fs.ProcName = "curl"
	fs.ProcPID = 42
	fs.Ready.Store(true)
	if _, _, _, pm, ok := p.LookupFlowDestination("f1"); !ok || pm.Name != "curl" || pm.PID != 42 {
		t.Fatalf("post-Ready lookup must yield published proc meta, got %+v ok=%v", pm, ok)
	}
}

func TestHandleFlowClosed_UnknownAndNotReady(t *testing.T) {
	p := newPlatform(&mockHandler{decision: api.DecisionPassthrough})
	// Unknown flow → warn + return, no panic.
	p.handleFlowClosed(nepkg.FlowMsg{FlowID: "ghost"})
	// Known but not-ready flow → skipped (no read of unpublished fields).
	fs := &flow.State{FlowID: "f1"}
	fs.SetDstHost("h")
	p.storeFlow(fs)
	p.handleFlowClosed(nepkg.FlowMsg{FlowID: "f1"})
	if _, ok := p.loadFlow("f1"); ok {
		t.Fatal("flow_closed must remove the flow from tracking")
	}
}

// auditingHandler records OnFlowComplete calls so handleFlowClosed's
// audit-emitting branch can be asserted.
type auditingHandler struct {
	mockHandler
	mu      sync.Mutex
	results []api.FlowResult
}

func (h *auditingHandler) OnFlowComplete(r api.FlowResult) {
	h.mu.Lock()
	h.results = append(h.results, r)
	h.mu.Unlock()
}

// TestHandleFlowClosed_PassthroughWritesAuditRow covers the non-inspect
// branch: a ready passthrough flow emits exactly one FlowResult carrying
// that flow's own identity + decision (no cross-flow bleed), with the
// transport metrics from the flow_closed frame.
func TestHandleFlowClosed_PassthroughWritesAuditRow(t *testing.T) {
	h := &auditingHandler{mockHandler: mockHandler{decision: api.DecisionPassthrough}}
	p := newPlatform(h)

	fs := &flow.State{FlowID: "f1", DstIP: "1.2.3.4", DstPort: 443, DecisionInt: int(api.DecisionPassthrough)}
	fs.SetDstHost("api.openai.com")
	fs.Ready.Store(true)
	p.storeFlow(fs)

	itm := 7
	p.handleFlowClosed(nepkg.FlowMsg{FlowID: "f1", BytesIn: 11, BytesOut: 22, DurationMs: 33, InterceptMs: &itm})

	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.results) != 1 {
		t.Fatalf("expected exactly 1 audit row for a passthrough flow, got %d", len(h.results))
	}
	r := h.results[0]
	if r.FlowID != "f1" || r.DstHost != "api.openai.com" || r.Decision != api.DecisionPassthrough {
		t.Fatalf("audit row identity mismatch: %+v", r)
	}
	if r.BytesIn != 11 || r.BytesOut != 22 || r.DurationMs != 33 {
		t.Fatalf("audit row metrics mismatch: %+v", r)
	}
	if r.LatencyBreakdown["intercept_ms"] != 7 {
		t.Fatalf("intercept_ms not threaded: %+v", r.LatencyBreakdown)
	}
}

// TestHandleFlowClosed_InspectSkipsAuditRow covers the inspect early
// return: bumped inspect flows audit per-request inside tlsbump, so
// handleFlowClosed must NOT also write a flow-level row.
func TestHandleFlowClosed_InspectSkipsAuditRow(t *testing.T) {
	h := &auditingHandler{mockHandler: mockHandler{decision: api.DecisionInspect}}
	p := newPlatform(h)

	fs := &flow.State{FlowID: "f1", DecisionInt: int(api.DecisionInspect)}
	fs.SetDstHost("h")
	fs.Ready.Store(true)
	p.storeFlow(fs)

	p.handleFlowClosed(nepkg.FlowMsg{FlowID: "f1", BytesIn: 1})

	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.results) != 0 {
		t.Fatalf("inspect flow must NOT write a flow-level audit row, got %d", len(h.results))
	}
}

// TestHandleFlowClosed_EmitsFlowVerdict pins the single-line per-flow
// diagnostic: every closed flow logs exactly one `flow verdict` carrying
// the NE-attested bundle, the decision, and a `captured` flag that is true
// ONLY for a successfully-bumped inspect flow. The captured logic is the
// load-bearing part — it answers "I configured inspection but see no body"
// (a pinned host reports decision=inspect, captured=false).
func TestHandleFlowClosed_EmitsFlowVerdict(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(old) })

	p := newPlatform(&auditingHandler{mockHandler: mockHandler{decision: api.DecisionInspect}})

	// inspect + BUMP_SUCCESS → captured=true, bundle threaded through.
	fs := &flow.State{FlowID: "f1", DecisionInt: int(api.DecisionInspect), ProcBundleID: "codex", ProcName: "codex"}
	fs.SetDstHost("chatgpt.com")
	fs.Ready.Store(true)
	p.storeFlow(fs)
	p.handleFlowClosed(nepkg.FlowMsg{FlowID: "f1", BumpStatus: "BUMP_SUCCESS", BytesIn: 10, BytesOut: 20})

	// inspect but bump did NOT succeed (pinned host → opaqueRelay): the
	// decision is still inspect, but no body was captured.
	fsPin := &flow.State{FlowID: "f3", DecisionInt: int(api.DecisionInspect), ProcBundleID: "com.openai.codex"}
	fsPin.SetDstHost("chatgpt.com")
	fsPin.Ready.Store(true)
	p.storeFlow(fsPin)
	p.handleFlowClosed(nepkg.FlowMsg{FlowID: "f3", BumpStatus: ""})

	// passthrough → captured=false.
	fs2 := &flow.State{FlowID: "f2", DecisionInt: int(api.DecisionPassthrough), ProcBundleID: "com.tencent.xinWeChat"}
	fs2.SetDstHost("weixin.qq.com")
	fs2.Ready.Store(true)
	p.storeFlow(fs2)
	p.handleFlowClosed(nepkg.FlowMsg{FlowID: "f2", BumpStatus: ""})

	verdicts := map[string]map[string]any{}
	for _, line := range bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n")) {
		var rec map[string]any
		if json.Unmarshal(line, &rec) != nil || rec["msg"] != "flow verdict" {
			continue
		}
		if id, ok := rec["flow_id"].(string); ok {
			verdicts[id] = rec
		}
	}
	if len(verdicts) != 3 {
		t.Fatalf("want 3 flow verdict lines, got %d: %s", len(verdicts), buf.String())
	}
	if v := verdicts["f1"]; v["bundle"] != "codex" || v["host"] != "chatgpt.com" || v["decision"] != "inspect" || v["captured"] != true {
		t.Errorf("f1 (bumped inspect) verdict wrong: %v", v)
	}
	if v := verdicts["f3"]; v["decision"] != "inspect" || v["captured"] != false || v["bundle"] != "com.openai.codex" {
		t.Errorf("f3 (pinned inspect) must report captured=false: %v", v)
	}
	if v := verdicts["f2"]; v["decision"] != "passthrough" || v["captured"] != false || v["bundle"] != "com.tencent.xinWeChat" {
		t.Errorf("f2 (passthrough) verdict wrong: %v", v)
	}
}

func TestLooksLikeIPLiteral(t *testing.T) {
	ip := []string{"1.2.3.4", "10.0.0.255", "::1", "fe80::1"}
	for _, s := range ip {
		if !looksLikeIPLiteral(s) {
			t.Errorf("looksLikeIPLiteral(%q) = false, want true", s)
		}
	}
	notIP := []string{"", "api.openai.com", "host", "1.2.3.x"}
	for _, s := range notIP {
		if looksLikeIPLiteral(s) {
			t.Errorf("looksLikeIPLiteral(%q) = true, want false", s)
		}
	}
}

func TestHandleNEConn_DispatchesFlowNew(t *testing.T) {
	// Feed a flow_new frame over a pipe; the decision must come back on
	// the same connection via the single-writer goroutine.
	p := newPlatform(&mockHandler{decision: api.DecisionInspect})
	srv, cli := net.Pipe()
	done := make(chan struct{})
	go func() { p.handleNEConn(srv); close(done) }()

	frame, _ := json.Marshal(nepkg.FlowMsg{Type: "flow_new", FlowID: "f1", RemoteHost: "api.x", PID: os.Getpid()})
	if _, err := cli.Write(append(frame, '\n')); err != nil {
		t.Fatalf("write frame: %v", err)
	}
	buf := make([]byte, 256)
	n, err := cli.Read(buf)
	if err != nil {
		t.Fatalf("read decision: %v", err)
	}
	if !strings.Contains(string(buf[:n]), `"inspect"`) {
		t.Fatalf("expected inspect decision, got %q", string(buf[:n]))
	}
	_ = cli.Close()
	_ = srv.Close()
	<-done
}

// TestHandleNEConn_ConcurrentFrames_RaceClean drives a realistic
// interleaving of flow_new / flow_update_host / flow_closed frames for
// many flows through the real scanner+worker path and reads every reply
// back. Run under -race, it must be clean and every reply must be keyed
// to its own flow. This is the end-to-end guard against concurrent
// state corruption in the lock-free path.
func TestHandleNEConn_ConcurrentFrames_RaceClean(t *testing.T) {
	const n = 100
	p := newPlatform(&mockHandler{decision: api.DecisionPassthrough})
	srv, cli := net.Pipe()
	done := make(chan struct{})
	go func() { p.handleNEConn(srv); close(done) }()

	// Reader: collect decision replies keyed by flow_id.
	got := make(map[string]bool, n)
	var gotMu sync.Mutex
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		dec := json.NewDecoder(cli)
		for range n {
			var d nepkg.DecisionMsg
			if err := dec.Decode(&d); err != nil {
				return
			}
			gotMu.Lock()
			got[d.FlowID] = true
			gotMu.Unlock()
		}
	}()

	// Writer: for each flow emit flow_new, then host update, then close.
	enc := json.NewEncoder(cli)
	for i := range n {
		id := fmt.Sprintf("f%d", i)
		if err := enc.Encode(nepkg.FlowMsg{Type: "flow_new", FlowID: id, RemoteHost: "h", PID: os.Getpid()}); err != nil {
			t.Fatalf("encode flow_new: %v", err)
		}
		_ = enc.Encode(nepkg.FlowMsg{Type: "flow_update_host", FlowID: id, Hostname: "api.openai.com"})
		_ = enc.Encode(nepkg.FlowMsg{Type: "flow_closed", FlowID: id, BytesIn: 1, BytesOut: 1})
	}

	select {
	case <-readDone:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out reading decision replies")
	}
	gotMu.Lock()
	count := len(got)
	gotMu.Unlock()
	if count != n {
		t.Fatalf("got %d distinct flow replies, want %d", count, n)
	}
	_ = cli.Close()
	_ = srv.Close()
	<-done
}

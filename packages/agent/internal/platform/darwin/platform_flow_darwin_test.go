//go:build darwin

package darwin

import (
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"os"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/platform/api"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/platform/darwin/flow"
	nepkg "github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/platform/darwin/ne"
)

// fakeConn captures bytes written by handleNewFlow's decision reply. Only
// Write is exercised by the flow handler; the embedded nil net.Conn would
// panic on any other method, which is exactly what we want (a regression that
// starts calling Read/Close here should fail loudly).
type fakeConn struct {
	net.Conn
	buf      bytes.Buffer
	writeErr error
}

func (f *fakeConn) Write(b []byte) (int, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	return f.buf.Write(b)
}

func (f *fakeConn) decision(t *testing.T) string {
	t.Helper()
	if f.buf.Len() == 0 {
		return ""
	}
	var d nepkg.DecisionMsg
	if err := json.Unmarshal(bytes.TrimSpace(f.buf.Bytes()), &d); err != nil {
		t.Fatalf("decode decision %q: %v", f.buf.String(), err)
	}
	return d.Decision
}

// mockHandler implements api.ConnectionHandler. Kill-switch gating is
// exercised separately via gatingHandler, which embeds this type.
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

func newPlatform(h api.ConnectionHandler) *DarwinPlatform {
	return &DarwinPlatform{handler: h, activeFlows: map[string]*flow.State{}}
}

func TestHandleNewFlow_HandlerNil_Drops(t *testing.T) {
	p := &DarwinPlatform{activeFlows: map[string]*flow.State{}} // handler nil
	c := &fakeConn{}
	p.handleNewFlow(c, nepkg.FlowMsg{FlowID: "f1"})
	if c.buf.Len() != 0 {
		t.Fatalf("handler-nil must not write a decision, wrote %q", c.buf.String())
	}
}

func TestHandleNewFlow_KillSwitch_FailsOpenPassthrough(t *testing.T) {
	// Kill-switch engaged → MUST reply passthrough and MUST NOT track the flow
	// (paused state must not leak into the audit DB). This is the NE fail-open
	// binding for the kill-switch path.
	p := newPlatform(&gatingHandler{mockHandler: mockHandler{decision: api.DecisionInspect}, engaged: true})
	c := &fakeConn{}
	p.handleNewFlow(c, nepkg.FlowMsg{FlowID: "f1", RemoteHost: "api.openai.com"})
	if c.decision(t) != "passthrough" {
		t.Fatalf("kill-switch must reply passthrough, got %q", c.decision(t))
	}
	if _, ok := p.activeFlows["f1"]; ok {
		t.Fatal("kill-switch flow must NOT be tracked in activeFlows")
	}
	// Write error on the passthrough reply is swallowed (no panic, still no track).
	p.handleNewFlow(&fakeConn{writeErr: errors.New("epipe")}, nepkg.FlowMsg{FlowID: "f2"})
	if _, ok := p.activeFlows["f2"]; ok {
		t.Fatal("kill-switch flow must not be tracked even on write error")
	}
}

func TestHandleNewFlow_Backpressure_FailsOpenPassthrough(t *testing.T) {
	p := newPlatform(&mockHandler{decision: api.DecisionInspect})
	p.backpressure = throttle{on: true}
	c := &fakeConn{}
	p.handleNewFlow(c, nepkg.FlowMsg{FlowID: "f1"})
	if c.decision(t) != "passthrough" {
		t.Fatalf("backpressure must shed via passthrough, got %q", c.decision(t))
	}
	if _, ok := p.activeFlows["f1"]; ok {
		t.Fatal("shed flow must not be tracked")
	}
	// Write error path.
	p.handleNewFlow(&fakeConn{writeErr: errors.New("epipe")}, nepkg.FlowMsg{FlowID: "f2"})
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
		c := &fakeConn{}
		// PID = self so ProcessInfo succeeds (proc-meta populated path).
		p.handleNewFlow(c, nepkg.FlowMsg{FlowID: "f1", RemoteHost: "api.x", PID: os.Getpid()})
		if c.decision(t) != tc.want {
			t.Fatalf("decision %v → %q, want %q", tc.dec, c.decision(t), tc.want)
		}
		fs, ok := p.activeFlows["f1"]
		if !ok || fs.DecisionInt != int(tc.dec) {
			t.Fatalf("flow must be tracked with decision %v: %+v ok=%v", tc.dec, fs, ok)
		}
	}
}

func TestHandleNewFlow_ProcessInfoErrNonFatal_AndWriteErr(t *testing.T) {
	// A non-existent PID makes ProcessInfo fail; the flow is still handled
	// (proc meta empty, decision still written).
	p := newPlatform(&mockHandler{decision: api.DecisionInspect})
	c := &fakeConn{}
	p.handleNewFlow(c, nepkg.FlowMsg{FlowID: "f1", PID: 1<<30 + 3})
	if c.decision(t) != "inspect" {
		t.Fatalf("proc-info failure must be non-fatal, got %q", c.decision(t))
	}
	// Decision-reply write error is logged, not fatal.
	p.handleNewFlow(&fakeConn{writeErr: errors.New("epipe")}, nepkg.FlowMsg{FlowID: "f2", PID: os.Getpid()})
	if _, ok := p.activeFlows["f2"]; !ok {
		t.Fatal("flow should still be tracked even when the reply write fails")
	}
}

func TestHandleFlowUpdateHostAndLookup(t *testing.T) {
	p := newPlatform(&mockHandler{decision: api.DecisionInspect})
	p.activeFlows["f1"] = &flow.State{FlowID: "f1", DstHost: "1.2.3.4", DstPort: 443, ProcName: "curl"}

	// Empty hostname → no-op.
	p.handleFlowUpdateHost(nepkg.FlowMsg{FlowID: "f1", Hostname: ""})
	// Unknown flow → no-op (no panic).
	p.handleFlowUpdateHost(nepkg.FlowMsg{FlowID: "ghost", Hostname: "x"})
	// Known flow → host rewritten from SNI.
	p.handleFlowUpdateHost(nepkg.FlowMsg{FlowID: "f1", Hostname: "api.openai.com"})

	host, port, _, pm, ok := p.LookupFlowDestination("f1")
	if !ok || host != "api.openai.com" || port != 443 || pm.Name != "curl" {
		t.Fatalf("lookup after update: host=%q port=%d pm=%+v ok=%v", host, port, pm, ok)
	}
	if _, _, _, _, ok := p.LookupFlowDestination("ghost"); ok {
		t.Fatal("unknown flow lookup must return ok=false")
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
	// Feed a flow_new frame over a pipe; the handler's decision must come back
	// on the same connection. Exercises the scanner/dispatch loop.
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

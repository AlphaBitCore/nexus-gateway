package proxy

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

// TestAgentMarkerInject_Full verifies that a fully-populated AgentMarker
// stamps the canonical X-Nexus-* response markers (via, mode chain, hook
// chain, trace-id reflect) and the CORS expose list.
func TestAgentMarkerInject_Full(t *testing.T) {
	m := &AgentMarker{
		FlowID:      "flow-1",
		HookOutcome: traffic.HookOutcomeInput{Passed: []string{"pii-redact"}},
	}
	h := http.Header{}
	m.injectInto(h)

	for _, want := range []struct{ k, v string }{
		{"X-Nexus-Via", "agent"},
		{"X-Nexus-Request-Id", "flow-1"},
		{"X-Nexus-Hook", "passed:pii-redact"},
	} {
		if got := h.Get(want.k); got != want.v {
			t.Errorf("%s: got %q want %q", want.k, got, want.v)
		}
	}
	expose := h.Get("Access-Control-Expose-Headers")
	if !strings.Contains(expose, "X-Nexus-Hook") {
		t.Errorf("expose missing X-Nexus-Hook, got: %q", expose)
	}
	if !strings.Contains(expose, "X-Nexus-Via") {
		t.Errorf("expose missing X-Nexus-Via, got: %q", expose)
	}
	if !strings.Contains(expose, "X-Nexus-Request-Id") {
		t.Errorf("expose missing X-Nexus-Request-Id, got: %q", expose)
	}
}

// TestAgentMarkerInject_EmptyMarker verifies that a zero-value AgentMarker
// still stamps the mandatory headers (via, mode, hook=none) but omits the
// optional trace-id header.
func TestAgentMarkerInject_EmptyMarker(t *testing.T) {
	m := &AgentMarker{}
	h := http.Header{}
	m.injectInto(h)

	if got := h.Get("X-Nexus-Via"); got != "agent" {
		t.Errorf("X-Nexus-Via: got %q want %q", got, "agent")
	}
	if got := h.Get("X-Nexus-Hook"); got != "none" {
		t.Errorf("X-Nexus-Hook: got %q want %q", got, "none")
	}
	if got := h.Get("X-Nexus-Request-Id"); got != "" {
		t.Errorf("X-Nexus-Request-Id should be absent, got %q", got)
	}
}

// TestAgentMarkerInject_RejectHookOutcome verifies that a reject hook outcome
// is formatted correctly as "rejected:<hook>:<reason>".
func TestAgentMarkerInject_RejectHookOutcome(t *testing.T) {
	m := &AgentMarker{
		FlowID: "flow-2",
		HookOutcome: traffic.HookOutcomeInput{
			Rejected:     "prompt-injection",
			RejectReason: "sql-fragment",
		},
	}
	h := http.Header{}
	m.injectInto(h)

	want := "rejected:prompt-injection:sql-fragment"
	if got := h.Get("X-Nexus-Hook"); got != want {
		t.Errorf("X-Nexus-Hook: got %q want %q", got, want)
	}
}

// TestAgentMarkerInject_NoDomainRule verifies that when FlowID is empty the
// trace-id header is omitted and other headers are still present.
func TestAgentMarkerInject_NoDomainRule(t *testing.T) {
	m := &AgentMarker{
		HookOutcome: traffic.HookOutcomeInput{
			Passed:      []string{"pii-redact", "jwt-strip"},
			Transformed: true,
		},
	}
	h := http.Header{}
	m.injectInto(h)

	if got := h.Get("X-Nexus-Hook"); got != "transformed:pii-redact,jwt-strip" {
		t.Errorf("X-Nexus-Hook: got %q want %q", got, "transformed:pii-redact,jwt-strip")
	}
	if got := h.Get("X-Nexus-Request-Id"); got != "" {
		t.Errorf("X-Nexus-Request-Id should be absent, got %q", got)
	}
	// via and mode must still be set
	if got := h.Get("X-Nexus-Via"); got != "agent" {
		t.Errorf("X-Nexus-Via: got %q want %q", got, "agent")
	}
}

// TestAgentMarkerInject_PreservesUpstreamVia verifies that PrependVia
// preserves an existing X-Nexus-Via chain from the upstream response.
func TestAgentMarkerInject_PreservesUpstreamVia(t *testing.T) {
	m := &AgentMarker{FlowID: "flow-3"}
	h := http.Header{}
	h.Set("X-Nexus-Via", "ai-gateway, compliance-proxy")
	m.injectInto(h)

	// agent should be prepended before the existing chain
	got := h.Get("X-Nexus-Via")
	if !strings.HasPrefix(got, "agent") {
		t.Errorf("X-Nexus-Via should start with 'agent', got %q", got)
	}
	if !strings.Contains(got, "ai-gateway") {
		t.Errorf("X-Nexus-Via should preserve ai-gateway, got %q", got)
	}
	if !strings.Contains(got, "compliance-proxy") {
		t.Errorf("X-Nexus-Via should preserve compliance-proxy, got %q", got)
	}
}

// TestInspectRequest_RejectPath_NewHeaders verifies that when the request
// inspector returns reject_hard, the synthetic 403 response written to the
// client carries the canonical X-Nexus-* marker headers and no legacy
// X-Nexus-Hook-Decision / X-Nexus-Hook-Reason headers.
//
// net.Pipe() semantics: data written on A is read on B and vice versa.
// - serverSide: the end passed to inspectRequest as clientTLS (reads request, writes 403 back)
// - clientEnd:  the end the test controls (writes request bytes in, reads 403 response back)
func TestInspectRequest_RejectPath_NewHeaders(t *testing.T) {
	// serverSide ←→ clientEnd: serverSide is the "TLS connection to client" seen by the proxy.
	serverSide, clientEnd := net.Pipe()
	t.Cleanup(func() {
		_ = serverSide.Close()
		_ = clientEnd.Close()
	})

	// Craft a minimal HTTP/1.1 request to feed into inspectRequest.
	rawReq := "GET /secret HTTP/1.1\r\nHost: example.com\r\n\r\n"

	// Inspector that always rejects with a named hook + reason code.
	rejectInspector := RequestInspector(func(_ context.Context, _, _, _ string, _ http.Header, _ []byte) InspectionResult {
		return InspectionResult{
			Decision:   "reject_hard",
			Reason:     "prompt injection detected",
			ReasonCode: "prompt-injection",
			HookOutcome: traffic.HookOutcomeInput{
				Rejected:     "prompt-injection-hook",
				RejectReason: "sql-fragment",
			},
		}
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		// inspectRequest reads from serverSide (via bufio.Reader) and writes the
		// 403 back to serverSide. Data written on serverSide is readable on clientEnd.
		_, _, _ = inspectRequest(context.Background(), bufio.NewReader(serverSide), serverSide, "example.com", rejectInspector, "flow-99", 1<<20)
	}()

	// Write the request bytes into clientEnd — inspectRequest reads them from serverSide.
	_, _ = clientEnd.Write([]byte(rawReq))

	// Read the 403 response that inspectRequest wrote back to serverSide — it arrives on clientEnd.
	resp, err := http.ReadResponse(bufio.NewReader(clientEnd), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status: got %d want 403", resp.StatusCode)
	}

	// Canonical markers must be present.
	for _, want := range []struct{ k, v string }{
		{"X-Nexus-Via", "agent"},
		{"X-Nexus-Hook", "rejected:prompt-injection-hook:sql-fragment"},
		{"X-Nexus-Request-Id", "flow-99"},
	} {
		if got := resp.Header.Get(want.k); got != want.v {
			t.Errorf("%s: got %q want %q", want.k, got, want.v)
		}
	}

	expose := resp.Header.Get("Access-Control-Expose-Headers")
	for _, name := range []string{"X-Nexus-Hook", "X-Nexus-Request-Id", "X-Nexus-Via"} {
		if !strings.Contains(expose, name) {
			t.Errorf("Access-Control-Expose-Headers missing %q, got: %q", name, expose)
		}
	}

	// Legacy headers must be absent.
	for _, legacy := range []string{"X-Nexus-Hook-Decision", "X-Nexus-Hook-Reason"} {
		if got := resp.Header.Get(legacy); got != "" {
			t.Errorf("legacy header %q must be absent, got %q", legacy, got)
		}
	}

	_ = clientEnd.Close()
	<-done
}

// TestInspectRequest_RejectPath_NoFlowID verifies that when flowID is empty
// the X-Nexus-Request-Id header is omitted entirely from the 403 response.
func TestInspectRequest_RejectPath_NoFlowID(t *testing.T) {
	serverSide, clientEnd := net.Pipe()
	t.Cleanup(func() {
		_ = serverSide.Close()
		_ = clientEnd.Close()
	})

	rawReq := "GET /blocked HTTP/1.1\r\nHost: example.com\r\n\r\n"

	rejectInspector := RequestInspector(func(_ context.Context, _, _, _ string, _ http.Header, _ []byte) InspectionResult {
		return InspectionResult{
			Decision:   "reject_hard",
			Reason:     "blocked by compliance policy",
			ReasonCode: "policy-block",
			HookOutcome: traffic.HookOutcomeInput{
				Rejected:     "policy-hook",
				RejectReason: "policy-block",
			},
		}
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		// Empty flowID — trace-id header must be omitted.
		_, _, _ = inspectRequest(context.Background(), bufio.NewReader(serverSide), serverSide, "example.com", rejectInspector, "", 1<<20)
	}()

	_, _ = clientEnd.Write([]byte(rawReq))

	resp, err := http.ReadResponse(bufio.NewReader(clientEnd), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status: got %d want 403", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Nexus-Request-Id"); got != "" {
		t.Errorf("X-Nexus-Request-Id should be absent when flowID empty, got %q", got)
	}
	// Other markers still present.
	if got := resp.Header.Get("X-Nexus-Via"); got != "agent" {
		t.Errorf("X-Nexus-Via: got %q want %q", got, "agent")
	}

	_ = clientEnd.Close()
	<-done
}

// TestAgentMarkerInject_PreservesUpstreamExposeHeaders verifies that
// MergeExposeHeaders extends (rather than replaces) an existing
// Access-Control-Expose-Headers from the upstream.
func TestAgentMarkerInject_PreservesUpstreamExposeHeaders(t *testing.T) {
	m := &AgentMarker{FlowID: "flow-4"}
	h := http.Header{}
	h.Set("Access-Control-Expose-Headers", "x-custom-upstream")
	m.injectInto(h)

	expose := h.Get("Access-Control-Expose-Headers")
	if !strings.Contains(expose, "x-custom-upstream") {
		t.Errorf("expose should preserve upstream x-custom-upstream, got: %q", expose)
	}
	if !strings.Contains(expose, "X-Nexus-Via") {
		t.Errorf("expose should contain X-Nexus-Via, got: %q", expose)
	}
}

// TestTunnelRelay_NoMarkers asserts that when the agent takes the pure
// CONNECT-tunnel / passthrough code path (proxy.Relay), no X-Nexus-*
// marker headers are present in the bytes that reach the client.
//
// Per spec §3 non-goals: stamping markers on forwarding/CONNECT-tunnel
// flows is physically impossible without TLS termination, and the system
// must NOT inject them via side channels. The tunnel path uses proxy.Relay,
// which is a dumb bidirectional byte copier with no HTTP awareness —
// AgentMarker.injectInto is unreachable from that code path.
//
// The test verifies this at the wire level: an upstream mock writes a
// plain HTTP/1.1 response (including some custom headers but zero
// x-nexus-* headers) through proxy.Relay, and the client-side bytes
// contain no x-nexus-* headers whatsoever.
func TestTunnelRelay_NoMarkers(t *testing.T) {
	// upstream ←→ serverSide pipe: simulates the raw TCP stream from the
	// real upstream (which the agent tunnels without TLS termination).
	serverSide, upstream := net.Pipe()
	// clientSide ←→ clientEnd pipe: simulates the client-side connection.
	clientSide, clientEnd := net.Pipe()

	t.Cleanup(func() {
		_ = upstream.Close()
		_ = clientEnd.Close()
	})

	// Run proxy.Relay between the two pipe halves.
	// Relay blocks until both directions are done, so run it in a goroutine.
	relayDone := make(chan struct{})
	go func() {
		defer close(relayDone)
		Relay(clientSide, serverSide)
	}()

	// The upstream writes a raw HTTP/1.1 response. It carries only
	// vendor headers — zero x-nexus-* markers. The tunnel must not add any.
	upstreamResponse := "HTTP/1.1 200 OK\r\n" +
		"Content-Type: application/json\r\n" +
		"X-Vendor-Custom: some-value\r\n" +
		"Content-Length: 2\r\n" +
		"\r\n" +
		"{}"
	if _, err := upstream.Write([]byte(upstreamResponse)); err != nil {
		t.Fatalf("upstream write: %v", err)
	}
	// Close the upstream write side so Relay knows the stream is done.
	_ = upstream.Close()

	// Read the response on the client end and parse HTTP headers.
	resp, err := http.ReadResponse(bufio.NewReader(clientEnd), nil)
	if err != nil {
		t.Fatalf("parse tunneled response: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	// Close clientEnd so proxy.Relay's client→server copy direction
	// receives EOF and the goroutine can exit. (net.Pipe does not
	// support half-close, so the direction must be unblocked via Close.)
	_ = clientEnd.Close()

	// Wait for Relay to finish.
	<-relayDone

	// Assert: no x-nexus-* headers were injected by the tunnel path.
	for k := range resp.Header {
		if strings.HasPrefix(strings.ToLower(k), "x-nexus-") {
			t.Errorf("tunnel response unexpectedly carried marker header %q (tunnel must not inject markers; see spec §3 non-goals)", k)
		}
	}

	// Sanity check: the vendor header from upstream survived unmolested.
	if got := resp.Header.Get("X-Vendor-Custom"); got != "some-value" {
		t.Errorf("upstream X-Vendor-Custom: got %q want %q (relay must not corrupt headers it does pass)", got, "some-value")
	}
}

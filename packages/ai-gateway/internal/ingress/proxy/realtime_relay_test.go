package proxy

// realtime_relay_test.go — the live relay core: AC-1 (byte-verbatim relay in
// both directions; binary → 1003; oversize → 1009), close propagation in
// both directions, and AC-6's handshake-hygiene assertions (only
// gateway-built headers reach the provider; no subprotocol echo; no audio in
// audit envelopes).

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// TestRealtimeRelay_VerbatimBothDirections drives a frame client→provider
// and its echo provider→client through the relay and asserts BYTE equality
// in both directions, plus the handshake hygiene contract on the recorded
// provider-side upgrade request.
func TestRealtimeRelay_VerbatimBothDirections(t *testing.T) {
	provider := newRTProvider(t, nil) // echo
	deps, _ := realtimeDeps(t, provider.srv.URL)
	srv, done := rtServer(t, deps)

	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer nvk_client_token_should_never_cross")
	hdr.Set("X-Client-Junk", "must-not-cross")
	c, _, err := rtDial(t, srv.URL, "gpt-realtime", hdr)
	if err != nil {
		t.Fatalf("dial through relay: %v", err)
	}

	frame := `{"type":"session.update","session":{"instructions":"hi é"}}`
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Write(ctx, websocket.MessageText, []byte(frame)); err != nil {
		t.Fatalf("client write: %v", err)
	}
	typ, echo, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("client read echo: %v", err)
	}
	if typ != websocket.MessageText || string(echo) != frame {
		t.Fatalf("echo = (%v, %q), want byte-verbatim %q", typ, echo, frame)
	}

	_ = c.Close(websocket.StatusNormalClosure, "done")
	waitHandlerDone(t, done, 1)

	// AC-6: the provider handshake carries ONLY gateway-built headers.
	hs := provider.handshakes()
	if len(hs) != 1 {
		t.Fatalf("provider saw %d handshakes, want 1", len(hs))
	}
	got := hs[0]
	if auth := got.Get("Authorization"); auth != "Bearer sk-upstream" {
		t.Errorf("upstream Authorization = %q, want the resolved provider key", auth)
	}
	if v := got.Get("X-Client-Junk"); v != "" {
		t.Errorf("client header crossed the relay: X-Client-Junk=%q", v)
	}
	if v := got.Get("Sec-Websocket-Protocol"); v != "" {
		t.Errorf("subprotocol echoed upstream: %q", v)
	}
	for k, vals := range got {
		for _, v := range vals {
			if strings.Contains(v, "nvk_client_token_should_never_cross") {
				t.Errorf("client credential reached the provider via %s", k)
			}
		}
	}
	// The dial query carries the RESOLVED provider model id (alias→provider
	// rewrite), not the client's requested label.
	if m := provider.query(0).Get("model"); m != "gpt-realtime-x" {
		t.Errorf("upstream model query = %q, want gpt-realtime-x", m)
	}
}

// TestRealtimeRelay_BinaryFrameTerminates1003: the protocol is
// text-frames-only; a binary frame from the client terminates the session
// with 1003 on both sides and stamps the session row.
func TestRealtimeRelay_BinaryFrameTerminates1003(t *testing.T) {
	provider := newRTProvider(t, rtHoldScript)
	deps, prod := realtimeDeps(t, provider.srv.URL)
	srv, done := rtServer(t, deps)

	c, _, err := rtDial(t, srv.URL, "gpt-realtime", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Write(ctx, websocket.MessageBinary, []byte{0x01, 0x02}); err != nil {
		t.Fatalf("client binary write: %v", err)
	}
	_, closeStatus := readUntilClose(t, c, 5*time.Second)
	if closeStatus != websocket.StatusUnsupportedData {
		t.Errorf("client close status = %v, want 1003", closeStatus)
	}
	if st := provider.waitClosed(t, 5*time.Second); st != websocket.StatusUnsupportedData {
		t.Errorf("provider close status = %v, want 1003 propagated", st)
	}
	waitHandlerDone(t, done, 1)

	_, sessions := splitRealtimeRows(drainAudit(t, deps.AuditWriter, prod))
	if len(sessions) != 1 {
		t.Fatalf("session rows = %d, want 1", len(sessions))
	}
	if sessions[0].ErrorCode == nil || *sessions[0].ErrorCode != "REALTIME_BINARY_FRAME" {
		t.Errorf("session row error code = %v, want REALTIME_BINARY_FRAME", sessions[0].ErrorCode)
	}
}

// TestRealtimeRelay_OversizeFrame1009: a frame beyond the per-frame ceiling
// fails that conn's read; the session terminates with 1009 and the session
// row records the reason. (The 16 MiB default ceiling is exercised for real:
// the client sends 16 MiB + 1.)
func TestRealtimeRelay_OversizeFrame1009(t *testing.T) {
	provider := newRTProvider(t, rtHoldScript)
	deps, prod := realtimeDeps(t, provider.srv.URL)
	srv, done := rtServer(t, deps)

	c, _, err := rtDial(t, srv.URL, "gpt-realtime", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	big := make([]byte, 16<<20+1)
	for i := range big {
		big[i] = 'a'
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// The write may itself error mid-flight once the relay kills the conn;
	// either way the session must end 1009-classified.
	_ = c.Write(ctx, websocket.MessageText, big)
	_, closeStatus := readUntilClose(t, c, 10*time.Second)
	// The client observes 1009 when the close frame won the race against the
	// TCP teardown; a raw reset surfaces as -1. The authoritative assertion
	// is the session row below.
	if closeStatus != websocket.StatusMessageTooBig && closeStatus != -1 {
		t.Errorf("client close status = %v, want 1009 (or a raw teardown)", closeStatus)
	}
	waitHandlerDone(t, done, 1)

	_, sessions := splitRealtimeRows(drainAudit(t, deps.AuditWriter, prod))
	if len(sessions) != 1 {
		t.Fatalf("session rows = %d, want 1", len(sessions))
	}
	if sessions[0].ErrorCode == nil || *sessions[0].ErrorCode != "REALTIME_FRAME_TOO_BIG" {
		t.Errorf("session row error code = %v, want REALTIME_FRAME_TOO_BIG", sessions[0].ErrorCode)
	}
}

// TestRealtimeRelay_UpstreamCloseProagatesToClient: the provider closes
// mid-session → the client receives a terminal gateway error event and the
// provider's close status; the session row records NO error code (a clean
// upstream close is a benign session end — only abnormal terminations stamp
// error_code). There is no mid-session re-dial.
func TestRealtimeRelay_UpstreamCloseProagates(t *testing.T) {
	provider := newRTProvider(t, func(ctx context.Context, c *websocket.Conn, _ *rtProvider) {
		_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"session.created"}`))
		_ = c.Close(websocket.StatusNormalClosure, "provider done")
	})
	deps, prod := realtimeDeps(t, provider.srv.URL)
	srv, done := rtServer(t, deps)

	c, _, err := rtDial(t, srv.URL, "gpt-realtime", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	frames, closeStatus := readUntilClose(t, c, 5*time.Second)
	if closeStatus != websocket.StatusNormalClosure {
		t.Errorf("client close status = %v, want the provider's 1000 propagated", closeStatus)
	}
	var sawEvent, sawErrorEvent bool
	for _, f := range frames {
		s := string(f)
		if strings.Contains(s, "session.created") {
			sawEvent = true
		}
		if strings.Contains(s, "REALTIME_UPSTREAM_CLOSED") {
			sawErrorEvent = true
		}
	}
	if !sawEvent {
		t.Error("provider frame was not relayed before the close")
	}
	if !sawErrorEvent {
		t.Error("client did not receive the terminal gateway error event on upstream close")
	}
	waitHandlerDone(t, done, 1)

	_, sessions := splitRealtimeRows(drainAudit(t, deps.AuditWriter, prod))
	// A clean upstream close is a BENIGN session end: the session row carries
	// no error code (the terminal client EVENT above still fired). Only
	// abnormal terminations stamp error_code.
	if len(sessions) != 1 || !benignRealtimeErrorCode(sessions[0].ErrorCode) {
		t.Fatalf("session rows = %+v, want one with no error code (benign upstream close)", sessions)
	}
}

// TestRealtimeRelay_ClientClosePropagatesToProvider: a client close frees
// the provider side promptly with the client's status code.
func TestRealtimeRelay_ClientClosePropagates(t *testing.T) {
	provider := newRTProvider(t, rtHoldScript)
	deps, prod := realtimeDeps(t, provider.srv.URL)
	srv, done := rtServer(t, deps)

	c, _, err := rtDial(t, srv.URL, "gpt-realtime", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = c.Close(websocket.StatusNormalClosure, "client done")
	if st := provider.waitClosed(t, 5*time.Second); st != websocket.StatusNormalClosure {
		t.Errorf("provider close status = %v, want the client's 1000 propagated", st)
	}
	waitHandlerDone(t, done, 1)

	msgs := drainAudit(t, deps.AuditWriter, prod)
	_, sessions := splitRealtimeRows(msgs)
	// A client close is the ordinary end of a voice call — benign, no error
	// code on the session row.
	if len(sessions) != 1 || !benignRealtimeErrorCode(sessions[0].ErrorCode) {
		t.Fatalf("session rows = %+v, want one with no error code (benign client close)", sessions)
	}
	if sessions[0].StatusCode != http.StatusSwitchingProtocols {
		t.Errorf("session row status = %d, want 101", sessions[0].StatusCode)
	}
	if sessions[0].EndpointType != string(typology.EndpointKindRealtime) {
		t.Errorf("session row endpoint type = %q", sessions[0].EndpointType)
	}
}

// TestRealtimeRelay_NoAudioBytesInAudit: audio/base64 frames must never
// enter any Record body field — the envelopes carry usage numbers only.
func TestRealtimeRelay_NoAudioBytesInAudit(t *testing.T) {
	const audioB64 = "UklGRiSampleAudioPayloadThatMustNotPersist=="
	provider := newRTProvider(t, nil) // echo
	deps, prod := realtimeDeps(t, provider.srv.URL)
	srv, done := rtServer(t, deps)

	c, _, err := rtDial(t, srv.URL, "gpt-realtime", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	frame := `{"type":"input_audio_buffer.append","audio":"` + audioB64 + `"}`
	if err := c.Write(ctx, websocket.MessageText, []byte(frame)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, _, err := c.Read(ctx); err != nil { // wait for the echo round-trip
		t.Fatalf("read echo: %v", err)
	}
	_ = c.Close(websocket.StatusNormalClosure, "done")
	waitHandlerDone(t, done, 1)

	deps.AuditWriter.Close()
	for _, raw := range rawAudit(prod) {
		if strings.Contains(string(raw), audioB64) {
			t.Fatalf("audio payload leaked into an audit envelope: %s", raw)
		}
	}
}

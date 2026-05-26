package thingclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// TestSendBreakGlassShadowReport_WSPath — in WS-connected mode the client
// must enqueue a `shadow_report_break_glass` message carrying Reported,
// ReportedVer, and the break-glass audit context.
func TestSendBreakGlassShadowReport_WSPath(t *testing.T) {
	t.Parallel()

	hub := newHubServer(connectedMsg(1))
	defer hub.Close()

	reg := prometheus.NewRegistry()
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
	defer writeCancel()

	c.mu.RLock()
	conn := c.wsConn
	c.mu.RUnlock()
	if conn == nil {
		t.Fatalf("wsConn is nil after connectWS")
	}

	go c.writePump(writeCtx, conn)

	// Drain the initial plain shadow_report emitted by applyConfig during
	// connectWS (desiredVer=1 from connectedMsg). The break-glass message
	// lands on recvCh after it.
	select {
	case raw := <-hub.recvCh:
		var msg thingMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			t.Fatalf("unmarshal initial shadow_report: %v", err)
		}
		if msg.Type != "shadow_report" {
			t.Fatalf("initial msg.Type = %q, want shadow_report", msg.Type)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for initial shadow_report")
	}

	if err := c.SendBreakGlassShadowReport(
		ctx,
		"killswitch",
		json.RawMessage(`{"enabled":false}`),
		42,
		"break-glass reason",
		"10.0.0.1",
		"tok-abc",
	); err != nil {
		t.Fatalf("SendBreakGlassShadowReport: %v", err)
	}

	select {
	case raw := <-hub.recvCh:
		var msg thingMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			t.Fatalf("unmarshal received message: %v", err)
		}
		if msg.Type != "shadow_report_break_glass" {
			t.Errorf("msg.Type = %q, want shadow_report_break_glass", msg.Type)
		}
		if msg.ReportedVer != 42 {
			t.Errorf("msg.ReportedVer = %d, want 42", msg.ReportedVer)
		}
		if msg.Reason != "break-glass reason" {
			t.Errorf("msg.Reason = %q", msg.Reason)
		}
		if msg.SourceIP != "10.0.0.1" {
			t.Errorf("msg.SourceIP = %q", msg.SourceIP)
		}
		if msg.ActorTokenID != "tok-abc" {
			t.Errorf("msg.ActorTokenID = %q", msg.ActorTokenID)
		}
		if got, ok := msg.KeyVersions["killswitch"]; !ok || got != 42 {
			t.Errorf("msg.KeyVersions[killswitch] = %v, want 42", msg.KeyVersions)
		}
		if _, ok := msg.Reported["killswitch"]; !ok {
			t.Errorf("msg.Reported missing killswitch entry: %+v", msg.Reported)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for break-glass message on server")
	}
}

// TestSendBreakGlassShadowReport_HTTPPath — in HTTP fallback mode the client
// must POST to /api/internal/things/shadow/break-glass with the audit context.
func TestSendBreakGlassShadowReport_HTTPPath(t *testing.T) {
	t.Parallel()

	var gotBody []byte
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b := make([]byte, r.ContentLength)
		if _, err := r.Body.Read(b); err != nil && err.Error() != "EOF" {
			t.Logf("read body: %v", err)
		}
		gotBody = b
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	reg := prometheus.NewRegistry()
	cfg := testConfig("ws://unused/ws", reg)
	cfg.HubHTTPURL = srv.URL
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.setMode(ModeHTTPFallback)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := c.SendBreakGlassShadowReport(
		ctx,
		"killswitch",
		json.RawMessage(`{"enabled":false}`),
		7,
		"",
		"",
		"tok-http",
	); err != nil {
		t.Fatalf("SendBreakGlassShadowReport: %v", err)
	}

	if gotPath != "/api/internal/things/shadow/break-glass" {
		t.Errorf("POST path = %q, want /api/internal/things/shadow/break-glass", gotPath)
	}
	var req breakGlassShadowRequest
	if err := json.Unmarshal(gotBody, &req); err != nil {
		t.Fatalf("unmarshal POST body: %v", err)
	}
	if req.ReportedVer != 7 {
		t.Errorf("req.ReportedVer = %d, want 7", req.ReportedVer)
	}
	if req.ActorTokenID != "tok-http" {
		t.Errorf("req.ActorTokenID = %q", req.ActorTokenID)
	}
	if _, ok := req.Reported["killswitch"]; !ok {
		t.Errorf("req.Reported missing killswitch entry")
	}
}

// TestSendBreakGlassShadowReport_DisconnectedReturnsError — when the client
// is neither on WS nor HTTP fallback the call must surface an error so the
// PUT handler knows to spool the request to the pending-buffer on disk.
func TestSendBreakGlassShadowReport_DisconnectedReturnsError(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	cfg := testConfig("ws://unused/ws", reg)
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Leave mode at ModeDisconnected (zero value).

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	err = c.SendBreakGlassShadowReport(ctx, "killswitch",
		json.RawMessage(`{"enabled":false}`), 1, "", "", "")
	if err == nil {
		t.Fatal("expected error when disconnected, got nil")
	}
}

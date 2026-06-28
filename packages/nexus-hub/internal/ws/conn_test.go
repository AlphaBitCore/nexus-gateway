package ws

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// TestConn_OnLivenessFiresAfterPing verifies that writePump invokes the
// onLiveness callback after a successful Hub→Thing ping. This is the
// guarantee that keeps WS-connected Things marked online: one ping per
// pingInterval refreshes last_seen_at via Manager.TouchLiveness.
func TestConn_OnLivenessFiresAfterPing(t *testing.T) {
	// Shorten ping cadence for the test; restore on exit.
	orig := pingInterval
	pingInterval = 20 * time.Millisecond
	t.Cleanup(func() { pingInterval = orig })

	const thingID = "agent-liveness-test"
	var hits int64
	done := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wsConn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			OriginPatterns: []string{"*"},
		})
		if err != nil {
			t.Errorf("server accept: %v", err)
			return
		}
		c := newConn(wsConn, thingID, "agent", nil, func(id string) {
			if id != thingID {
				t.Errorf("onLiveness got id=%q want=%q", id, thingID)
			}
			if atomic.AddInt64(&hits, 1) == 2 {
				close(done)
			}
		}, slog.Default())
		c.Run(r.Context())
	}))
	t.Cleanup(srv.Close)

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	client, dialResp, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("client dial: %v", err)
	}
	if dialResp != nil && dialResp.Body != nil {
		_ = dialResp.Body.Close()
	}
	t.Cleanup(func() { _ = client.Close(websocket.StatusNormalClosure, "test done") })

	// Drive the client's read loop so server pings complete (pongs must
	// come back from a reading peer).
	readErr := make(chan error, 1)
	go func() {
		for {
			if _, _, err := client.Read(ctx); err != nil {
				readErr <- err
				return
			}
		}
	}()

	select {
	case <-done:
		// Two pings observed — callback wiring works.
	case <-time.After(1 * time.Second):
		t.Fatalf("onLiveness never fired twice; hits=%d", atomic.LoadInt64(&hits))
	case err := <-readErr:
		t.Fatalf("client read error before livelines hits: %v", err)
	}
}

// TestConn_WritePump_RecoversOnLivenessPanic covers the case where a panic inside the
// onLiveness callback (which in production makes a TouchLiveness DB call) must
// be recovered inside writePump and tear down only this connection — it must
// NOT crash the process. The test asserts Run returns (recover fired + Close
// was called) rather than the goroutine panicking the test binary.
func TestConn_WritePump_RecoversOnLivenessPanic(t *testing.T) {
	orig := pingInterval
	pingInterval = 20 * time.Millisecond
	t.Cleanup(func() { pingInterval = orig })

	srvDone := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wsConn, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"*"}})
		if err != nil {
			t.Errorf("server accept: %v", err)
			return
		}
		c := newConn(wsConn, "agent-panic", "agent", nil, func(string) {
			panic("simulated TouchLiveness DB panic")
		}, slog.Default())
		c.Run(r.Context()) // blocks until both pumps exit
		close(srvDone)
	}))
	t.Cleanup(srv.Close)

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	client, dialResp, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("client dial: %v", err)
	}
	if dialResp != nil && dialResp.Body != nil {
		_ = dialResp.Body.Close()
	}
	t.Cleanup(func() { _ = client.Close(websocket.StatusNormalClosure, "done") })

	// Drive the client read loop so the server ping completes and onLiveness
	// (which panics) fires.
	go func() {
		for {
			if _, _, err := client.Read(ctx); err != nil {
				return
			}
		}
	}()

	select {
	case <-srvDone:
		// Run returned: the writePump panic was recovered and the conn closed.
	case <-time.After(2 * time.Second):
		t.Fatal("Run never returned — writePump panic was not recovered (F-0252)")
	}
}

// TestConn_ReadLimit_AcceptsLargeShadowReport covers the case where a message larger
// than the old 64 KiB limit (here ~256 KiB, a realistic large shadow_report
// with a full outcomes ledger) must be delivered to the message handler rather
// than hard-closing the connection. The previous limit would have severed the
// connection and triggered a reconnect loop.
func TestConn_ReadLimit_AcceptsLargeShadowReport(t *testing.T) {
	const payloadSize = 256 * 1024 // > old 64 KiB limit, < new 1 MiB limit
	payload := bytes.Repeat([]byte("a"), payloadSize)

	got := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wsConn, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"*"}})
		if err != nil {
			t.Errorf("server accept: %v", err)
			return
		}
		c := newConn(wsConn, "agent-big", "agent", func(_, _ string, data []byte) {
			cp := make([]byte, len(data))
			copy(cp, data)
			got <- cp
		}, nil, slog.Default())
		c.Run(r.Context())
	}))
	t.Cleanup(srv.Close)

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	client, dialResp, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("client dial: %v", err)
	}
	if dialResp != nil && dialResp.Body != nil {
		_ = dialResp.Body.Close()
	}
	// Client must also raise its own read limit if it ever reads; we only write.
	t.Cleanup(func() { _ = client.Close(websocket.StatusNormalClosure, "done") })

	if err := client.Write(ctx, websocket.MessageText, payload); err != nil {
		t.Fatalf("client write large payload: %v", err)
	}

	select {
	case data := <-got:
		if len(data) != payloadSize {
			t.Errorf("handler got %d bytes, want %d", len(data), payloadSize)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("large shadow_report was not delivered — read limit too low (F-0255)")
	}
}

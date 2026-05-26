package ws

import (
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

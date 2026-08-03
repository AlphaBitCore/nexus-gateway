package wiring

// routes_realtime_test.go — the realtime route registration and the
// load-bearing hijack pin: websocket.Accept discovers http.Hijacker by
// walking ResponseWriter Unwrap() chains, so EVERY writer wrapper in
// applyMiddleware must keep unwrapping to the raw connection. If any
// middleware breaks the chain, Accept fails for every realtime session —
// this test fails first.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// TestApplyMiddleware_PreservesHijackForWebSocketAccept upgrades a real
// client connection through the FULL applyMiddleware chain (connection
// stage, logger, recovery, CORS, tracing, request id) and round-trips a
// frame — proving the statusWriter Unwrap() chain still reaches the
// hijackable connection.
func TestApplyMiddleware_PreservesHijackForWebSocketAccept(t *testing.T) {
	deps := buildMinimalRouteDeps(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /wspin", func(w http.ResponseWriter, r *http.Request) {
		// Empty AcceptOptions — the exact production posture ServeRealtime
		// uses (no OriginPatterns, no subprotocols).
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{})
		if err != nil {
			t.Errorf("websocket.Accept through the middleware chain failed: %v", err)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		typ, data, err := c.Read(ctx)
		if err != nil {
			t.Errorf("server read: %v", err)
			return
		}
		if err := c.Write(ctx, typ, data); err != nil {
			t.Errorf("server write: %v", err)
			return
		}
		_ = c.Close(websocket.StatusNormalClosure, "pin done")
	})

	srv := httptest.NewServer(applyMiddleware(mux, deps))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/wspin", nil)
	if err != nil {
		t.Fatalf("dial through the middleware chain: %v (a broken Unwrap chain surfaces here)", err)
	}
	if err := c.Write(ctx, websocket.MessageText, []byte("ping-through-chain")); err != nil {
		t.Fatalf("client write: %v", err)
	}
	_, echo, err := c.Read(ctx)
	if err != nil || string(echo) != "ping-through-chain" {
		t.Fatalf("echo = (%q, %v), want the frame back through the hijacked conn", echo, err)
	}
	_ = c.Close(websocket.StatusNormalClosure, "done")
}

// TestMountCoreRoutes_realtimeRegisteredAndGated: the route is mounted and
// gated — a non-WebSocket request gets the endpoint's own 400 (never 404),
// and an unauthenticated upgrade-shaped request cannot open a session.
func TestMountCoreRoutes_realtimeRegisteredAndGated(t *testing.T) {
	h := getSharedCoreHandler(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/realtime?model=gpt-realtime", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code == http.StatusNotFound {
		t.Fatal("/v1/realtime: route not registered (404)")
	}
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "REALTIME_UPGRADE_REQUIRED") {
		t.Fatalf("non-upgrade request = %d %s, want the 400 REALTIME_UPGRADE_REQUIRED envelope",
			rr.Code, rr.Body.String())
	}

	req2 := httptest.NewRequest(http.MethodGet, "/v1/realtime?model=gpt-realtime", nil)
	req2.Header.Set("Upgrade", "websocket")
	req2.Header.Set("Connection", "Upgrade")
	rr2 := httptest.NewRecorder()
	h.ServeHTTP(rr2, req2)
	if rr2.Code == http.StatusOK || rr2.Code == http.StatusSwitchingProtocols {
		t.Fatalf("unauthenticated realtime upgrade returned %d; must be refused", rr2.Code)
	}
}

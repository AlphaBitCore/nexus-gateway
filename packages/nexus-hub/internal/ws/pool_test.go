package ws

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/prometheus/client_golang/prometheus"

	opsmetrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
)

// droppedCount scrapes the supplied prometheus registry for the
// ws_send_dropped_total metric and returns the value for the given type label.
// Reads via Gather (the same path /metrics uses) rather than the registry's
// unexported Counter.vec field, and avoids the prometheus/testutil dependency.
func droppedCount(t *testing.T, reg *prometheus.Registry, thingType string) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range families {
		if !strings.HasSuffix(mf.GetName(), "send_dropped_total") {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == "type" && lp.GetValue() == thingType {
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}

// newPooledConn stands up a real coder/websocket connection (so Close has a
// live socket to tear down) and returns the server-side *Conn WITHOUT calling
// Run — so its outbound buffer is never drained, letting tests fill it to force
// the buffer-full drop path. The client side and HTTP server are cleaned up via
// t.Cleanup.
func newPooledConn(t *testing.T, thingID, thingType string) *Conn {
	t.Helper()
	accepted := make(chan *websocket.Conn, 1)
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"*"}})
		if err != nil {
			return
		}
		accepted <- ws
		<-r.Context().Done()
	}))
	t.Cleanup(httpSrv.Close)

	wsURL := "ws" + strings.TrimPrefix(httpSrv.URL, "http")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	client, resp, err := websocket.Dial(ctx, wsURL, nil)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close(websocket.StatusInternalError, "") })

	server := <-accepted
	return newConn(server, thingID, thingType, nil, nil, slog.Default())
}

// fillBuffer enqueues until the conn's outbound buffer is full so the next
// Write is guaranteed to drop. Writes until the first full-buffer error,
// regardless of how many slots were already occupied.
func fillBuffer(t *testing.T, c *Conn) {
	t.Helper()
	// cap+1 attempts guarantee we hit the full state even from an empty buffer.
	for i := 0; i <= cap(c.outCh); i++ {
		if err := c.Write([]byte("x")); err != nil {
			return // buffer is now full
		}
	}
	t.Fatalf("buffer never reported full after %d writes (cap=%d)", cap(c.outCh)+1, cap(c.outCh))
}

// TestPool_SendDropOnFullBufferClosesConnAndCounts covers the full-buffer drop for Send: when
// a Thing's outbound buffer is full, the dropped push must (a) make Send return
// false, (b) increment ws.send_dropped_total{type}, and (c) close the
// connection so the Thing reconnects and rebuilds full state.
func TestPool_SendDropOnFullBufferClosesConnAndCounts(t *testing.T) {
	prom := prometheus.NewRegistry()
	pool := NewPool(opsmetrics.NewRegistry(prom), slog.Default())
	c := newPooledConn(t, "agent-1", "agent")
	pool.Add(c)

	// Healthy send first: buffer has room, returns true, no drop.
	if !pool.Send("agent-1", []byte("ok")) {
		t.Fatal("Send into a connection with buffer room should succeed")
	}

	fillBuffer(t, c)

	if pool.Send("agent-1", []byte("overflow")) {
		t.Fatal("Send into a full buffer must return false")
	}
	if got := droppedCount(t, prom, "agent"); got != 1 {
		t.Errorf("ws.send_dropped_total{type=agent} = %v, want 1", got)
	}
	select {
	case <-c.done:
		// connection was closed by the drop path — correct.
	default:
		t.Error("dropped Send must close the connection so the Thing reconnects")
	}
}

// TestPool_BroadcastDropOnFullBufferCounts covers the full-buffer drop for Broadcast: a Thing
// whose buffer is full must be skipped (not counted as sent), increment the
// drop counter, and be closed; a healthy sibling of the same type still
// receives the message.
func TestPool_BroadcastDropOnFullBufferCounts(t *testing.T) {
	prom := prometheus.NewRegistry()
	pool := NewPool(opsmetrics.NewRegistry(prom), slog.Default())

	full := newPooledConn(t, "agent-full", "agent")
	healthy := newPooledConn(t, "agent-ok", "agent")
	pool.Add(full)
	pool.Add(healthy)
	fillBuffer(t, full)

	sent := pool.Broadcast("agent", []byte("config_changed"))
	if sent != 1 {
		t.Errorf("Broadcast should report 1 healthy delivery, got %d", sent)
	}
	if got := droppedCount(t, prom, "agent"); got != 1 {
		t.Errorf("ws.send_dropped_total{type=agent} = %v, want 1", got)
	}
	select {
	case <-full.done:
	default:
		t.Error("dropped Broadcast target must be closed")
	}
}

func TestPool(t *testing.T) {
	pool := NewPool(opsmetrics.NewRegistry(prometheus.NewRegistry()), slog.Default())

	t.Run("SendNotConnected", func(t *testing.T) {
		ok := pool.Send("nonexistent", []byte("hello"))
		if ok {
			t.Error("Send to nonexistent should return false")
		}
	})

	t.Run("IsConnectedEmpty", func(t *testing.T) {
		if pool.IsConnected("anything") {
			t.Error("IsConnected on empty pool should return false")
		}
	})

	t.Run("BroadcastEmpty", func(t *testing.T) {
		n := pool.Broadcast("some-type", []byte("hello"))
		if n != 0 {
			t.Errorf("Broadcast on empty pool should return 0, got %d", n)
		}
	})

	t.Run("CountEmpty", func(t *testing.T) {
		if pool.Count() != 0 {
			t.Errorf("empty pool count should be 0, got %d", pool.Count())
		}
	})
}

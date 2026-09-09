package tlsbump

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"
)

// serveHTTP1 used to never return. http.Server calls Accept in a loop; the
// second call blocked on a `done` channel nothing ever closed, so Serve never
// returned — one goroutine stranded per bumped HTTP/1.1 tunnel, for the life of
// the process. A long-running compliance proxy accumulated one per connection.
//
// These arms assert the observable outcome: the serve call RETURNS once the
// connection is done, and the goroutine count comes back down.

// serveOverPipe runs http.Server on a singleConnListener over one half of a
// net.Pipe and returns a channel that receives when Serve returns.
func serveOverPipe(t *testing.T, srvSide net.Conn, h http.Handler) <-chan error {
	t.Helper()
	l := newSingleConnListener(srvSide)
	server := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
	done := make(chan error, 1)
	go func() { done <- server.Serve(l) }()
	return done
}

func TestSingleConnListener_ServeReturnsWhenTheConnectionIsDone(t *testing.T) {
	client, srv := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })

	served := make(chan struct{}, 1)
	done := serveOverPipe(t, srv, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served <- struct{}{}
		w.Header().Set("Content-Length", "2")
		_, _ = io.WriteString(w, "ok")
	}))

	// One request, then the client hangs up — the ordinary tunnel lifecycle.
	go func() {
		_, _ = io.WriteString(client, "GET / HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")
	}()

	select {
	case <-served:
	case <-time.After(5 * time.Second):
		t.Fatal("handler never ran")
	}

	// Drain the response so the server can finish the connection.
	go func() {
		br := bufio.NewReader(client)
		for {
			if _, err := br.ReadString('\n'); err != nil {
				return
			}
		}
	}()

	select {
	case err := <-done:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Logf("Serve returned %v (acceptable — the point is that it RETURNED)", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after the connection finished — the goroutine is stranded for the life of the process, one per bumped tunnel")
	}
}

// The consequence arm, measured rather than argued: serve many tunnels and
// check the goroutine count does not grow with them.
func TestSingleConnListener_DoesNotStrandAGoroutinePerTunnel(t *testing.T) {
	const tunnels = 40

	settle := func() {
		for range 10 {
			runtime.GC()
			time.Sleep(20 * time.Millisecond)
		}
	}
	settle()
	before := runtime.NumGoroutine()

	for i := range tunnels {
		client, srv := net.Pipe()
		done := serveOverPipe(t, srv, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", "2")
			_, _ = io.WriteString(w, "ok")
		}))
		go func() {
			_, _ = io.WriteString(client, "GET / HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")
			br := bufio.NewReader(client)
			for {
				if _, err := br.ReadString('\n'); err != nil {
					return
				}
			}
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("tunnel %d: Serve did not return", i)
		}
		_ = client.Close()
	}

	settle()
	after := runtime.NumGoroutine()
	// A stranded goroutine per tunnel would put `after` at roughly before+40.
	// The allowance covers scheduler noise and the pipes' own readers, without
	// being wide enough to hide a per-tunnel leak.
	if after > before+tunnels/2 {
		buf := make([]byte, 1<<16)
		n := runtime.Stack(buf, true)
		t.Errorf("goroutines went from %d to %d across %d tunnels — that is a leak per tunnel.\n%s",
			before, after, tunnels, summariseStacks(string(buf[:n])))
	}
}

// summariseStacks keeps the failure message readable: one line per distinct
// top frame, with counts.
func summariseStacks(dump string) string {
	counts := map[string]int{}
	for _, g := range strings.Split(dump, "\n\n") {
		lines := strings.Split(g, "\n")
		if len(lines) < 2 {
			continue
		}
		counts[strings.TrimSpace(lines[1])]++
	}
	var b strings.Builder
	for frame, n := range counts {
		if n > 1 {
			fmt.Fprintf(&b, "  %d× %s\n", n, frame)
		}
	}
	return b.String()
}

// The listener must still hand out exactly one connection, and the second
// Accept must not succeed. Without this, "return an error immediately" would
// satisfy the arms above while breaking the single-connection contract.
func TestSingleConnListener_YieldsExactlyOneConnection(t *testing.T) {
	client, srv := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })
	l := newSingleConnListener(srv)

	first, err := l.Accept()
	if err != nil {
		t.Fatalf("first Accept: %v", err)
	}
	if first == nil {
		t.Fatal("first Accept returned nil")
	}

	// Closing the served connection is what releases the second Accept.
	go func() {
		time.Sleep(50 * time.Millisecond)
		_ = first.Close()
	}()

	type res struct {
		c   net.Conn
		err error
	}
	ch := make(chan res, 1)
	go func() {
		c, err := l.Accept()
		ch <- res{c, err}
	}()

	select {
	case r := <-ch:
		if r.err == nil {
			t.Error("the second Accept returned a connection — the listener must yield exactly one")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the second Accept never returned after the connection closed")
	}
}

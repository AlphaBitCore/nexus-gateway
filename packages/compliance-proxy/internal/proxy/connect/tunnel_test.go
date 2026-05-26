package connect

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// nonHijackingResponseWriter is a plain ResponseWriter that does NOT implement
// http.Hijacker. EstablishTunnel must detect this and return an error without
// panicking.
type nonHijackingResponseWriter struct {
	code int
	body strings.Builder
}

func (w *nonHijackingResponseWriter) Header() http.Header         { return http.Header{} }
func (w *nonHijackingResponseWriter) WriteHeader(code int)        { w.code = code }
func (w *nonHijackingResponseWriter) Write(b []byte) (int, error) { return w.body.Write(b) }

// TestEstablishTunnel_NonHijacker_ReturnsError asserts the failure mode where
// the HTTP server does not support hijacking (e.g. HTTP/2 paths). The error
// message must be non-nil and the returned conn must be nil so callers do not
// try to use an invalid conn.
func TestEstablishTunnel_NonHijacker_ReturnsError(t *testing.T) {
	w := &nonHijackingResponseWriter{}
	req := httptest.NewRequest(http.MethodConnect, "example.com:443", nil)

	conn, err := EstablishTunnel(w, req)
	if err == nil {
		t.Fatal("expected error when ResponseWriter does not support hijacking")
	}
	if conn != nil {
		conn.Close()
		t.Error("connection should be nil on error")
	}
	if w.code != http.StatusInternalServerError {
		t.Errorf("expected 500 for non-hijacker, got %d", w.code)
	}
	if !strings.Contains(err.Error(), "ResponseWriter does not implement http.Hijacker") {
		t.Errorf("unexpected error message: %q", err.Error())
	}
}

// failHijacker implements http.Hijacker but returns an error from Hijack().
// Named failure mode: the OS or HTTP/2 layer rejects the hijack after Upgrade.
type failHijacker struct {
	code int
}

func (w *failHijacker) Header() http.Header         { return http.Header{} }
func (w *failHijacker) WriteHeader(code int)        { w.code = code }
func (w *failHijacker) Write(b []byte) (int, error) { return len(b), nil }
func (w *failHijacker) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return nil, nil, fmt.Errorf("simulated hijack failure")
}

// TestEstablishTunnel_HijackError_ReturnsError — named failure mode: Hijack()
// itself fails (can happen when the multiplexer claims to support Hijacker but
// the underlying conn is already closed). EstablishTunnel must propagate the
// error and return nil conn.
func TestEstablishTunnel_HijackError_ReturnsError(t *testing.T) {
	w := &failHijacker{}
	req := httptest.NewRequest(http.MethodConnect, "example.com:443", nil)

	conn, err := EstablishTunnel(w, req)
	if err == nil {
		t.Fatal("expected error when Hijack() fails")
	}
	if conn != nil {
		conn.Close()
		t.Error("conn must be nil on Hijack error")
	}
	if !strings.Contains(err.Error(), "hijack connection") {
		t.Errorf("error must wrap 'hijack connection'; got %q", err.Error())
	}
}

// newPipeWithClosedPeer creates a net.Pipe and closes the client end. Writes
// to the server side will fail once the bufio buffer drains. Unlike the
// large-buffer approach, using a tiny buffer means WriteString immediately
// attempts to flush to the closed pipe.
func newPipeWithClosedPeer(t *testing.T) net.Conn {
	t.Helper()
	server, client := net.Pipe()
	client.Close()
	t.Cleanup(func() { server.Close() })
	return server
}

// flushFailHijacker injects a bufio.ReadWriter whose Writer holds a buffer
// large enough to swallow the CONNECT response line without hitting the
// underlying closed pipe — Flush is what actually fails. This exercises the
// `if err := bufrw.Flush(); err != nil` branch.
type flushFailHijacker struct {
	rawConn net.Conn
	rw      *bufio.ReadWriter
}

func newFlushFailHijacker(t *testing.T) *flushFailHijacker {
	t.Helper()
	rawConn := newPipeWithClosedPeer(t)
	rw := bufio.NewReadWriter(
		bufio.NewReader(rawConn),
		bufio.NewWriterSize(rawConn, 4096), // large enough to buffer the 200 line
	)
	return &flushFailHijacker{rawConn: rawConn, rw: rw}
}

// writeFailHijacker injects a bufio.Writer wrapping an immediately-failing
// writer so even the initial WriteString call fails (not deferred to Flush).
// This exercises the `if _, err := bufrw.WriteString(connectResponse); err != nil`
// branch specifically.
type writeFailHijacker struct {
	rawConn net.Conn
	rw      *bufio.ReadWriter
}

// alwaysFailWriter rejects every write immediately.
type alwaysFailWriter struct{}

func (alwaysFailWriter) Write(_ []byte) (int, error) {
	return 0, fmt.Errorf("write: simulated write failure")
}

func newWriteFailHijacker(t *testing.T) *writeFailHijacker {
	t.Helper()
	rawConn := newPipeWithClosedPeer(t)
	// bufio.NewWriterSize with size=1 will call the underlying writer on
	// every WriteString call when the string exceeds 1 byte. Using an
	// alwaysFailWriter as the underlying sink guarantees WriteString fails.
	rw := bufio.NewReadWriter(
		bufio.NewReader(rawConn),
		bufio.NewWriterSize(alwaysFailWriter{}, 1),
	)
	return &writeFailHijacker{rawConn: rawConn, rw: rw}
}

func (w *writeFailHijacker) Header() http.Header         { return http.Header{} }
func (w *writeFailHijacker) WriteHeader(int)             {}
func (w *writeFailHijacker) Write(b []byte) (int, error) { return len(b), nil }
func (w *writeFailHijacker) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return w.rawConn, w.rw, nil
}

func (w *flushFailHijacker) Header() http.Header         { return http.Header{} }
func (w *flushFailHijacker) WriteHeader(int)             {}
func (w *flushFailHijacker) Write(b []byte) (int, error) { return len(b), nil }
func (w *flushFailHijacker) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return w.rawConn, w.rw, nil
}

// TestEstablishTunnel_WriteError_ClosesConnAndReturnsError — named failure
// mode: the WriteString of "200 Connection Established" fails immediately
// (not deferred to Flush). EstablishTunnel must close rawConn and return a
// wrapped "write 200 Connection Established" error.
func TestEstablishTunnel_WriteError_ClosesConnAndReturnsError(t *testing.T) {
	w := newWriteFailHijacker(t)
	req := httptest.NewRequest(http.MethodConnect, "example.com:443", nil)

	conn, err := EstablishTunnel(w, req)
	if err == nil {
		if conn != nil {
			conn.Close()
		}
		t.Fatal("expected write error for always-fail-writer hijacker")
	}
	if conn != nil {
		conn.Close()
		t.Error("conn must be nil on write error")
	}
	if !strings.Contains(err.Error(), "write 200 Connection Established") {
		t.Errorf("unexpected error message: %q", err.Error())
	}
}

// TestEstablishTunnel_FlushError_ClosesConnAndReturnsError — named failure
// mode: the write of "200 Connection Established" is buffered but Flush hits
// a broken pipe. EstablishTunnel must close rawConn and return a wrapped
// "flush 200 Connection Established" error.
func TestEstablishTunnel_FlushError_ClosesConnAndReturnsError(t *testing.T) {
	w := newFlushFailHijacker(t)
	req := httptest.NewRequest(http.MethodConnect, "example.com:443", nil)

	conn, err := EstablishTunnel(w, req)
	if err == nil {
		if conn != nil {
			conn.Close()
		}
		t.Fatal("expected flush error for closed-pipe hijacker")
	}
	if conn != nil {
		conn.Close()
		t.Error("conn must be nil on flush error")
	}
	if !strings.Contains(err.Error(), "flush 200 Connection Established") {
		t.Errorf("unexpected error message: %q", err.Error())
	}
}

// hijackingHandler is an http.Handler that calls EstablishTunnel and sends
// the result back on channels so the test goroutine can inspect it.
type hijackingHandler struct {
	result chan net.Conn
	errCh  chan error
}

func newHijackingHandler() *hijackingHandler {
	return &hijackingHandler{
		result: make(chan net.Conn, 1),
		errCh:  make(chan error, 1),
	}
}

func (h *hijackingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c, err := EstablishTunnel(w, r)
	if err != nil {
		h.errCh <- err
		return
	}
	h.result <- c
}

// TestEstablishTunnel_HappyPath_Sends200AndReturnsConn verifies the primary
// observable behaviours via a real httptest.Server that supports Hijacking:
//   - the client receives "HTTP/1.1 200 Connection Established"
//   - EstablishTunnel returns a non-nil conn
func TestEstablishTunnel_HappyPath_Sends200AndReturnsConn(t *testing.T) {
	handler := newHijackingHandler()
	ts := httptest.NewServer(handler)
	defer ts.Close()

	rawConn, err := net.Dial("tcp", ts.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer rawConn.Close()

	if _, err := fmt.Fprint(rawConn, "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n"); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}

	br := bufio.NewReader(rawConn)
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read response line: %v", err)
	}
	if !strings.Contains(line, "200") {
		t.Errorf("expected 200 status, got: %q", line)
	}
	// Drain blank line.
	for {
		l, err := br.ReadString('\n')
		if err != nil {
			break
		}
		if l == "\r\n" {
			break
		}
	}

	select {
	case c := <-handler.result:
		if c == nil {
			t.Error("EstablishTunnel returned nil conn on success")
		}
		c.Close()
	case e := <-handler.errCh:
		t.Fatalf("EstablishTunnel error: %v", e)
	}
}

// devNullConn is a net.Conn that discards all writes and returns EOF on reads.
// It is safe to use as the rawConn in hijacking tests where we only need the
// write of "200 Connection Established" to succeed (not produce data for the
// test to read back), so Flush() completes immediately.
type devNullConn struct{}

func (devNullConn) Read(b []byte) (int, error)         { return 0, io.EOF }
func (devNullConn) Write(b []byte) (int, error)        { return len(b), nil }
func (devNullConn) Close() error                       { return nil }
func (devNullConn) LocalAddr() net.Addr                { return &net.TCPAddr{} }
func (devNullConn) RemoteAddr() net.Addr               { return &net.TCPAddr{} }
func (devNullConn) SetDeadline(_ time.Time) error      { return nil }
func (devNullConn) SetReadDeadline(_ time.Time) error  { return nil }
func (devNullConn) SetWriteDeadline(_ time.Time) error { return nil }

// bufferedHijacker is an http.Hijacker whose bufio.Reader already has bytes
// buffered from the preamble, and whose bufio.Writer writes to a devNullConn
// so Flush() succeeds immediately. This exercises the `Buffered() > 0` branch.
type bufferedHijacker struct {
	rawConn net.Conn
	rw      *bufio.ReadWriter
}

func (w *bufferedHijacker) Header() http.Header         { return http.Header{} }
func (w *bufferedHijacker) WriteHeader(int)             {}
func (w *bufferedHijacker) Write(b []byte) (int, error) { return len(b), nil }
func (w *bufferedHijacker) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return w.rawConn, w.rw, nil
}

// TestEstablishTunnel_BufferedPreamble_WrapsInBufConn — named correctness
// invariant: a pipelining client that sends TLS ClientHello bytes immediately
// after the CONNECT line will have those bytes buffered in the bufio.Reader
// when Hijack() is called. Without BufConn wrapping, those bytes would be
// silently discarded and the TLS handshake would fail.
//
// Design: rawConn is a devNullConn so Flush() of "200 Connection Established"
// completes immediately without blocking. The Reader is backed by a
// bytes.Reader pre-loaded with preamble bytes — Peek() pushes them into
// bufio's internal buffer so Buffered() > 0 when EstablishTunnel inspects it.
func TestEstablishTunnel_BufferedPreamble_WrapsInBufConn(t *testing.T) {
	preamble := []byte("TLS-ClientHello-preamble")

	// Pre-fill the bufio.Reader with preamble bytes.
	br := bufio.NewReaderSize(bytes.NewReader(preamble), len(preamble)+64)
	if _, err := br.Peek(len(preamble)); err != nil {
		t.Fatalf("peek preamble into bufio: %v", err)
	}
	if br.Buffered() != len(preamble) {
		t.Fatalf("Buffered() = %d, want %d", br.Buffered(), len(preamble))
	}

	h := &bufferedHijacker{
		rawConn: devNullConn{},
		rw:      bufio.NewReadWriter(br, bufio.NewWriter(devNullConn{})),
	}

	req := httptest.NewRequest(http.MethodConnect, "example.com:443", nil)
	tunnelConn, err := EstablishTunnel(h, req)
	if err != nil {
		t.Fatalf("EstablishTunnel: %v", err)
	}
	if tunnelConn == nil {
		t.Fatal("expected non-nil conn when bufio has buffered bytes")
	}
	defer tunnelConn.Close()

	// The preamble bytes must be the first bytes readable from the BufConn.
	buf := make([]byte, len(preamble))
	if _, err := io.ReadFull(tunnelConn, buf); err != nil {
		t.Fatalf("read preamble from tunnelConn: %v", err)
	}
	if string(buf) != string(preamble) {
		t.Errorf("preamble mismatch: got %q, want %q", buf, preamble)
	}
}

package responseio

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestCopy_BasicResponse(t *testing.T) {
	raw := "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 13\r\n\r\n{\"ok\":true}\r\n"
	src := bufio.NewReader(strings.NewReader(raw))

	var dst bytes.Buffer
	hookCalled := false
	err := Copy(&dst, src, func(resp *http.Response) {
		hookCalled = true
		resp.Header.Set("x-nexus-test", "hello")
	})
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("Copy error: %v", err)
	}
	if !hookCalled {
		t.Fatal("expected header hook to be invoked")
	}
	out := dst.String()
	if !strings.Contains(out, "X-Nexus-Test: hello") {
		t.Errorf("expected injected header in output; got:\n%s", out)
	}
	if !strings.Contains(out, "{\"ok\":true}") {
		t.Errorf("expected body bytes preserved; got:\n%s", out)
	}
}

// TestCopy_StripsConnectionListed verifies that header names listed in the
// Connection header (dynamic hop-by-hop headers per RFC 7230 §6.1) are removed
// from the forwarded response along with Connection itself.
func TestCopy_StripsConnectionListed(t *testing.T) {
	raw := "HTTP/1.1 200 OK\r\n" +
		"Content-Length: 0\r\n" +
		"Connection: X-Forwarded-For\r\n" +
		"X-Forwarded-For: 1.2.3.4\r\n" +
		"\r\n"
	src := bufio.NewReader(strings.NewReader(raw))

	var dst bytes.Buffer
	if err := Copy(&dst, src, nil); err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("Copy error: %v", err)
	}
	out := dst.String()
	if strings.Contains(out, "Connection") {
		t.Errorf("Connection header must not appear in output; got:\n%s", out)
	}
	if strings.Contains(out, "X-Forwarded-For") {
		t.Errorf("X-Forwarded-For (listed in Connection) must not appear in output; got:\n%s", out)
	}
}

// TestCopy_PreservesUpstreamStatusReason verifies that non-standard status
// codes retain the upstream's reason phrase verbatim (http.StatusText returns
// "" for codes like 499, so manual status-line construction would corrupt it).
func TestCopy_PreservesUpstreamStatusReason(t *testing.T) {
	raw := "HTTP/1.1 499 Client Closed Request\r\nContent-Length: 0\r\n\r\n"
	src := bufio.NewReader(strings.NewReader(raw))

	var dst bytes.Buffer
	if err := Copy(&dst, src, nil); err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("Copy error: %v", err)
	}
	out := dst.String()
	want := "HTTP/1.1 499 Client Closed Request\r\n"
	if !strings.HasPrefix(out, want) {
		t.Errorf("expected output to start with %q; got:\n%s", want, out)
	}
}

// TestCopy_StripsMultiLineConnection verifies that header names listed across
// multiple Connection header lines (per RFC 7230 §6.1) are all stripped. When an
// upstream response has Connection: X-Foo\r\nConnection: X-Bar, both X-Foo and
// X-Bar must be removed from the forwarded response.
func TestCopy_StripsMultiLineConnection(t *testing.T) {
	raw := "HTTP/1.1 200 OK\r\n" +
		"Connection: X-Foo\r\n" +
		"Connection: X-Bar\r\n" +
		"X-Foo: a\r\n" +
		"X-Bar: b\r\n" +
		"Content-Length: 0\r\n" +
		"\r\n"
	src := bufio.NewReader(strings.NewReader(raw))

	var dst bytes.Buffer
	if err := Copy(&dst, src, nil); err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("Copy error: %v", err)
	}
	out := dst.String()
	if strings.Contains(out, "X-Foo") {
		t.Errorf("X-Foo (from first Connection line) must not appear in output; got:\n%s", out)
	}
	if strings.Contains(out, "X-Bar") {
		t.Errorf("X-Bar (from second Connection line) must not appear in output; got:\n%s", out)
	}
	if strings.Contains(out, "Connection") {
		t.Errorf("Connection header itself must not appear in output; got:\n%s", out)
	}
}

func TestCopy_ChunkedBody(t *testing.T) {
	raw := "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\nContent-Type: text/plain\r\n\r\n" +
		"5\r\nhello\r\n6\r\n world\r\n0\r\n\r\n"
	src := bufio.NewReader(strings.NewReader(raw))

	var dst bytes.Buffer
	if err := Copy(&dst, src, nil); err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("Copy: %v", err)
	}
	out := dst.String()
	// Body must be reassembled (re-chunked or with content-length); the body
	// content "hello world" must reach the client either way.
	if !strings.Contains(out, "hello") || !strings.Contains(out, "world") {
		t.Errorf("expected body 'hello' + 'world' to reach client; got:\n%s", out)
	}
	// Per Task 0.1 fix C1: response must remain self-framing on a keep-alive
	// connection. With ContentLength<0 from the dechunked source, resp.Write
	// re-emits Transfer-Encoding: chunked. Confirm one form of framing exists.
	hasChunkedTE := strings.Contains(out, "Transfer-Encoding: chunked")
	hasContentLength := strings.Contains(out, "Content-Length:")
	if !hasChunkedTE && !hasContentLength {
		t.Errorf("output lacks both Transfer-Encoding and Content-Length — keep-alive corruption hazard:\n%s", out)
	}
}

func TestCopy_MalformedHTTPResponseReturnsErr(t *testing.T) {
	// http.ReadResponse failure: missing status line → fail-fast with
	// "responseio: read response" wrapper, not silent garbage.
	src := bufio.NewReader(strings.NewReader("not an http response"))
	var dst bytes.Buffer
	err := Copy(&dst, src, nil)
	if err == nil {
		t.Fatal("expected error on malformed response")
	}
	if !strings.Contains(err.Error(), "read response") {
		t.Errorf("error should be wrapped: %v", err)
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, fmt.Errorf("write fail") }

func TestCopy_DstWriteErrorReturnsErr(t *testing.T) {
	// resp.Write to dst failing must surface as wrapped error — without
	// it, callers would see only partial data and no signal.
	raw := "HTTP/1.1 200 OK\r\nContent-Length: 5\r\n\r\nhello"
	src := bufio.NewReader(strings.NewReader(raw))
	err := Copy(failWriter{}, src, nil)
	if err == nil {
		t.Fatal("expected error when dst.Write fails")
	}
	if !strings.Contains(err.Error(), "write response") {
		t.Errorf("error should mention write response: %v", err)
	}
}

func TestCopy_HookCanMutateBeforeWrite(t *testing.T) {
	raw := "HTTP/1.1 404 Not Found\r\nContent-Length: 0\r\n\r\n"
	src := bufio.NewReader(strings.NewReader(raw))

	var dst bytes.Buffer
	err := Copy(&dst, src, func(resp *http.Response) {
		resp.Header.Add("X-Nexus-Via", "compliance-proxy")
		resp.Header.Add("X-Nexus-Via", "ai-gateway") // multi-value to test
	})
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("Copy: %v", err)
	}
	out := dst.String()
	if !strings.Contains(out, "404 Not Found") {
		t.Errorf("status preserved; got:\n%s", out)
	}
	if !strings.Contains(out, "X-Nexus-Via") {
		t.Errorf("via header injected; got:\n%s", out)
	}
}

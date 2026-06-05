package local

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// errOnceTransport returns a connection-level error on the first call and
// delegates to base afterwards. Lets us assert the retry without a real socket.
type errOnceTransport struct {
	calls int32
	err   error
	base  http.RoundTripper
}

func (e *errOnceTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if atomic.AddInt32(&e.calls, 1) == 1 {
		return nil, e.err
	}
	return e.base.RoundTrip(r)
}

func newOKServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestRetryTransport_RetriesIdempotentGetOnConnLost(t *testing.T) {
	srv := newOKServer(t)
	inner := &errOnceTransport{err: errors.New("http2: client connection lost"), base: http.DefaultTransport}
	rt := &RetryTransport{Next: inner, Idle: http.DefaultTransport.(*http.Transport)}

	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("expected transparent recovery, got: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if n := atomic.LoadInt32(&inner.calls); n != 2 {
		t.Fatalf("inner calls = %d, want 2 (fail + one retry)", n)
	}
}

func TestRetryTransport_DoesNotRetryPOST(t *testing.T) {
	inner := &errOnceTransport{err: errors.New("http2: client connection lost"), base: http.DefaultTransport}
	rt := &RetryTransport{Next: inner}

	req, _ := http.NewRequest(http.MethodPost, "http://example.invalid/x", strings.NewReader("{}"))
	_, err := rt.RoundTrip(req)
	if err == nil {
		t.Fatal("POST must surface the error (a mutation must not be silently re-sent)")
	}
	if n := atomic.LoadInt32(&inner.calls); n != 1 {
		t.Fatalf("inner calls = %d, want 1 (no retry for POST)", n)
	}
}

func TestRetryTransport_DoesNotRetryNonConnError(t *testing.T) {
	// A 500 is a real response (err==nil) — RetryTransport must pass it through
	// untouched; only a connection-level *error* triggers a retry.
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	rt := &RetryTransport{Next: http.DefaultTransport}

	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 500 || atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("a 500 response must not be retried: status=%d calls=%d", resp.StatusCode, calls)
	}
}

func TestRetryTransport_DoesNotRetryUserCancelOrDeadline(t *testing.T) {
	for _, msg := range []string{"context canceled", "context deadline exceeded"} {
		inner := &errOnceTransport{err: errors.New(msg), base: http.DefaultTransport}
		rt := &RetryTransport{Next: inner}
		req, _ := http.NewRequest(http.MethodGet, "http://example.invalid/x", nil)
		if _, err := rt.RoundTrip(req); err == nil {
			t.Fatalf("%q should surface, not retry", msg)
		}
		if n := atomic.LoadInt32(&inner.calls); n != 1 {
			t.Fatalf("%q: inner calls = %d, want 1", msg, n)
		}
	}
}

func TestRetryableIdempotent_Matrix(t *testing.T) {
	get, _ := http.NewRequest(http.MethodGet, "http://x/y", nil)
	head, _ := http.NewRequest(http.MethodHead, "http://x/y", nil)
	getBody, _ := http.NewRequest(http.MethodGet, "http://x/y", strings.NewReader("b"))
	cases := []struct {
		name string
		req  *http.Request
		err  error
		want bool
	}{
		{"GET conn lost", get, errors.New("http2: client connection lost"), true},
		{"GET conn reset", get, errors.New("read: connection reset by peer"), true},
		{"GET goaway", get, errors.New("http2: server sent GOAWAY"), true},
		{"HEAD conn lost", head, errors.New("connection lost"), true},
		{"GET deadline", get, errors.New("context deadline exceeded"), false},
		{"GET canceled", get, errors.New("context canceled"), false},
		{"GET unrelated", get, errors.New("tls: handshake failure"), false},
		{"GET with body", getBody, errors.New("connection lost"), false},
	}
	for _, c := range cases {
		if got := retryableIdempotent(c.req, c.err); got != c.want {
			t.Errorf("%s: retryableIdempotent = %v, want %v", c.name, got, c.want)
		}
	}
}

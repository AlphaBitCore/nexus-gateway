package local

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// captureHandler is a minimal slog.Handler that records every emitted record's
// message + attributes so a test can assert what was (and was not) logged.
type captureHandler struct {
	mu      sync.Mutex
	records []captured
}

type captured struct {
	msg   string
	level slog.Level
	attrs map[string]slog.Value
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h *captureHandler) WithGroup(string) slog.Handler            { return h }
func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	c := captured{msg: r.Message, level: r.Level, attrs: map[string]slog.Value{}}
	r.Attrs(func(a slog.Attr) bool {
		c.attrs[a.Key] = a.Value
		return true
	})
	h.mu.Lock()
	h.records = append(h.records, c)
	h.mu.Unlock()
	return nil
}

func (h *captureHandler) only() captured {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.records) != 1 {
		panic("expected exactly one record")
	}
	return h.records[0]
}

// TestLoggingTransport_LogsTimingAndStatus drives a real round-trip through an
// httptest server and asserts the one emitted line carries the method, sanitised
// URL, status, and timing fields — and that the secret bearer token is absent.
func TestLoggingTransport_LogsTimingAndStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot) // 418 — a status nothing else would produce
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()

	cap := &captureHandler{}
	lt := &LoggingTransport{Base: http.DefaultTransport, Log: slog.New(cap)}
	client := &http.Client{Transport: lt}

	const secret = "super-secret-bearer-token-value"
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/admin/things?key=topsecretquery", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("x-admin-key", secret)

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	_ = resp.Body.Close()

	rec := cap.only()
	if rec.msg != "http request" {
		t.Errorf("msg = %q, want %q", rec.msg, "http request")
	}
	if rec.level != slog.LevelInfo {
		t.Errorf("level = %v, want Info (visible at any configured level)", rec.level)
	}
	if got := rec.attrs["method"].String(); got != http.MethodGet {
		t.Errorf("method = %q, want GET", got)
	}
	if got := rec.attrs["status"].String(); got != "418 I'm a teapot" {
		t.Errorf("status = %q, want 418 status text", got)
	}
	// The sanitised URL keeps scheme+host+path but DROPS the query string.
	gotURL := rec.attrs["url"].String()
	if !strings.HasSuffix(gotURL, "/admin/things") {
		t.Errorf("url = %q, want it to end in %q (path only)", gotURL, "/admin/things")
	}
	if strings.Contains(gotURL, "topsecretquery") {
		t.Errorf("url %q leaked the query string", gotURL)
	}
	// total_ms is present and non-negative; ttfb_ms is recorded for a real response.
	if rec.attrs["total_ms"].Int64() < 0 {
		t.Errorf("total_ms = %d, want >= 0", rec.attrs["total_ms"].Int64())
	}
	if _, ok := rec.attrs["ttfb_ms"]; !ok {
		t.Error("ttfb_ms attribute missing")
	}
	if _, ok := rec.attrs["reused"]; !ok {
		t.Error("reused attribute missing")
	}

	// The secret must not appear in ANY attribute value (no header logging).
	for k, v := range rec.attrs {
		if strings.Contains(v.String(), secret) {
			t.Errorf("secret leaked in attr %q = %q", k, v.String())
		}
	}
}

// TestLoggingTransport_RecordsConnectAndTLSPhases drives a request over a fresh
// (non-pooled) TLS connection so the DNS / connect / TLS-handshake httptrace
// callbacks all fire, and asserts their phase durations are recorded (>= 0)
// rather than the -1 "phase did not run" sentinel. DNS resolution is forced by
// dialing the server by name (localhost) instead of its 127.0.0.1 literal.
func TestLoggingTransport_RecordsConnectAndTLSPhases(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// A dedicated transport (not the shared DefaultTransport) with the server's
	// CA trusted and keep-alives disabled, so this request always opens a brand
	// new connection — guaranteeing the connect/TLS callbacks run.
	base := srv.Client().Transport.(*http.Transport).Clone()
	base.DisableKeepAlives = true

	cap := &captureHandler{}
	lt := &LoggingTransport{Base: base, Log: slog.New(cap)}
	client := &http.Client{Transport: lt}

	// Rewrite 127.0.0.1 → localhost so the dialer performs a DNS lookup, but keep
	// the TLS ServerName as the cert's 127.0.0.1 (the cert is for the IP, not the
	// name) via the base transport's TLSClientConfig.ServerName.
	url := strings.Replace(srv.URL, "127.0.0.1", "localhost", 1)
	base.TLSClientConfig.ServerName = "127.0.0.1"

	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	_ = resp.Body.Close()

	rec := cap.only()
	// connect + TLS always run on a brand-new keep-alive-disabled connection, so
	// their phase durations must be recorded (not the -1 "did not run" sentinel).
	if got := rec.attrs["connect_ms"].Int64(); got < 0 {
		t.Errorf("connect_ms = %d, want >= 0 (TCP connect should have run on a fresh conn)", got)
	}
	if got := rec.attrs["tls_ms"].Int64(); got < 0 {
		t.Errorf("tls_ms = %d, want >= 0 (TLS handshake should have run on a fresh conn)", got)
	}
	// dns_ms is present as an attribute; its value is environment-dependent
	// (a hosts-file short-circuit can skip the resolver and leave it -1), so we
	// only assert the field is emitted, not its sign.
	if _, ok := rec.attrs["dns_ms"]; !ok {
		t.Error("dns_ms attribute missing")
	}
}

// TestLoggingTransport_LogsError asserts a transport error (unreachable host) is
// recorded in the err attribute with an empty status, and the call still returns
// the error to the caller.
func TestLoggingTransport_LogsError(t *testing.T) {
	cap := &captureHandler{}
	// A base that always fails, so we exercise the error branch deterministically
	// without depending on network timing.
	lt := &LoggingTransport{Base: errRoundTripper{}, Log: slog.New(cap)}

	req, _ := http.NewRequest(http.MethodGet, "https://gateway.invalid/v1/chat", nil)
	_, err := lt.RoundTrip(req)
	if err == nil {
		t.Fatal("expected error from failing base, got nil")
	}

	rec := cap.only()
	if rec.attrs["status"].String() != "" {
		t.Errorf("status = %q, want empty on error", rec.attrs["status"].String())
	}
	if rec.attrs["err"].String() == "" {
		t.Error("err attribute empty; want the transport error message")
	}
}

// TestLoggingTransport_NilLogPassthrough asserts a nil Log makes RoundTrip a pure
// pass-through (no panic, base still invoked).
func TestLoggingTransport_NilLogPassthrough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	lt := &LoggingTransport{Base: http.DefaultTransport, Log: nil}
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	resp, err := lt.RoundTrip(req)
	if err != nil {
		t.Fatalf("nil-log round trip: %v", err)
	}
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("status = %d, want 204", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

// TestLoggingTransport_NilBaseDefaults asserts a nil Base falls back to
// http.DefaultTransport (so a misconstructed transport still works rather than
// panicking on a nil RoundTripper).
func TestLoggingTransport_NilBaseDefaults(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cap := &captureHandler{}
	lt := &LoggingTransport{Base: nil, Log: slog.New(cap)}
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	resp, err := lt.RoundTrip(req)
	if err != nil {
		t.Fatalf("nil-base round trip: %v", err)
	}
	_ = resp.Body.Close()
	if rec := cap.only(); rec.attrs["status"].String() != "200 OK" {
		t.Errorf("status = %q, want 200 OK", rec.attrs["status"].String())
	}
}

// TestPhaseMS covers the duration helper: a measured interval, an unset phase
// (-1), and an inverted pair (-1, never a negative duration).
func TestPhaseMS(t *testing.T) {
	base := time.Now()
	if got := phaseMS(base, base.Add(5*time.Millisecond)); got < 5 {
		t.Errorf("phaseMS(5ms) = %d, want >= 5", got)
	}
	if got := phaseMS(time.Time{}, base); got != -1 {
		t.Errorf("phaseMS(unset start) = %d, want -1", got)
	}
	if got := phaseMS(base, time.Time{}); got != -1 {
		t.Errorf("phaseMS(unset end) = %d, want -1", got)
	}
	if got := phaseMS(base.Add(time.Second), base); got != -1 {
		t.Errorf("phaseMS(end before start) = %d, want -1", got)
	}
}

// errRoundTripper always fails, to exercise LoggingTransport's error branch.
type errRoundTripper struct{}

func (errRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("dial tcp: connection refused")
}

package local

import (
	"crypto/tls"
	"log/slog"
	"net/http"
	"net/http/httptrace"
	"sync"
	"time"
)

// LoggingTransport wraps an http.RoundTripper and records one structured log
// line per request with httptrace-derived connection timings. It is the CLI's
// primary diagnostic for transport hangs: when a turn dies with "context
// deadline exceeded" or "Client.Timeout exceeded while awaiting headers", the
// matching line here shows exactly which phase stalled — DNS, TCP connect, TLS
// handshake, or time-to-first-byte — and whether the connection was reused.
//
// It logs the request URL WITHOUT its query string (queries can carry secrets)
// and never logs the Authorization / x-admin-key header values.
type LoggingTransport struct {
	// Base is the underlying RoundTripper that performs the request. The CLI
	// sets it to core.NewHTTPTransport() to preserve the widened 30s TLS
	// handshake budget.
	Base http.RoundTripper
	// Log receives the per-request line. A nil Log makes RoundTrip a pure
	// pass-through (no logging) so the transport is safe to construct before a
	// logger exists.
	Log *slog.Logger
}

// reqTrace collects wall-clock timestamps for each connection phase as the
// httptrace callbacks fire. The callbacks run on the transport's dial
// goroutine, which can OUTLIVE RoundTrip: a cancelled/timed-out request
// returns while its abandoned dial keeps resolving DNS and firing callbacks,
// concurrent with the post-return log read. mu makes both sides safe.
type reqTrace struct {
	mu                        sync.Mutex
	start                     time.Time
	dnsStart, dnsDone         time.Time
	connectStart, connectDone time.Time
	tlsStart, tlsDone         time.Time
	gotFirstByte              time.Time
	reused                    bool
}

// set runs fn under the trace lock (the httptrace callback write side).
func (tr *reqTrace) set(fn func()) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	fn()
}

// RoundTrip performs req through Base, timing each connection phase via
// httptrace, and logs one Info-level line (so it is visible at any configured
// level) with the method, sanitised URL, status, error, and per-phase
// durations in milliseconds.
func (t *LoggingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}
	if t.Log == nil {
		return base.RoundTrip(req)
	}

	tr := &reqTrace{start: time.Now()}
	trace := &httptrace.ClientTrace{
		GetConn:              func(string) {},
		GotConn:              func(info httptrace.GotConnInfo) { tr.set(func() { tr.reused = info.Reused }) },
		DNSStart:             func(httptrace.DNSStartInfo) { tr.set(func() { tr.dnsStart = time.Now() }) },
		DNSDone:              func(httptrace.DNSDoneInfo) { tr.set(func() { tr.dnsDone = time.Now() }) },
		ConnectStart:         func(string, string) { tr.set(func() { tr.connectStart = time.Now() }) },
		ConnectDone:          func(string, string, error) { tr.set(func() { tr.connectDone = time.Now() }) },
		TLSHandshakeStart:    func() { tr.set(func() { tr.tlsStart = time.Now() }) },
		TLSHandshakeDone:     func(tls.ConnectionState, error) { tr.set(func() { tr.tlsDone = time.Now() }) },
		GotFirstResponseByte: func() { tr.set(func() { tr.gotFirstByte = time.Now() }) },
	}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))

	resp, err := base.RoundTrip(req)
	total := time.Since(tr.start)
	// Snapshot under the lock: the abandoned dial of a cancelled request can
	// still be firing callbacks on its own goroutine at this point.
	tr.mu.Lock()
	reused := tr.reused
	dnsMS := phaseMS(tr.dnsStart, tr.dnsDone)
	connectMS := phaseMS(tr.connectStart, tr.connectDone)
	tlsMS := phaseMS(tr.tlsStart, tr.tlsDone)
	ttfbMS := phaseMS(tr.start, tr.gotFirstByte)
	tr.mu.Unlock()

	status := ""
	if resp != nil {
		status = resp.Status
	}
	errStr := ""
	if err != nil {
		errStr = err.Error()
	}

	// URL without query: scheme + host + path only, so a secret carried in a
	// query parameter never lands in the log. The Authorization / x-admin-key
	// header value is never read here, so it cannot leak either.
	u := req.URL
	safeURL := u.Scheme + "://" + u.Host + u.Path

	t.Log.Info("http request",
		"method", req.Method,
		"url", safeURL,
		"status", status,
		"err", errStr,
		"reused", reused,
		"dns_ms", dnsMS,
		"connect_ms", connectMS,
		"tls_ms", tlsMS,
		"ttfb_ms", ttfbMS,
		"total_ms", total.Milliseconds(),
	)
	return resp, err
}

// phaseMS returns the milliseconds between start and end, or -1 when either
// timestamp is unset (the phase did not occur — e.g. DNS is skipped on a reused
// connection, TLS is skipped for plain HTTP). -1 is unambiguous in the log:
// "this phase did not run" versus a real 0ms.
func phaseMS(start, end time.Time) int64 {
	if start.IsZero() || end.IsZero() || end.Before(start) {
		return -1
	}
	return end.Sub(start).Milliseconds()
}

package local

import (
	"net/http"
	"strings"
)

// RetryTransport transparently retries an idempotent request once on a fresh
// connection when the first attempt fails with a connection-level error.
//
// Why this is needed on top of the h2 PING health-check (EnableH2Health):
// prod closes each HTTP/2 connection at a max-connection-age (~30 min observed).
// The CLI's dashboard polls a host every few seconds, so that one shared h2
// connection is never idle long enough for the ReadIdleTimeout PING to evict
// it — it is killed by the server while "active", and the client only discovers
// it as an "http2: client connection lost" failure mid-request. The PING
// health-check cures the IDLE-then-dropped case; this retry cures the
// ACTIVE-then-max-age case. Without it every ~30 min a request surfaces a
// ~15-20s hang (the spinner the operator sees as "working" forever); with it
// the retry dials a fresh connection and the failure is invisible.
//
// Only GET/HEAD with no body are retried — idempotent and replayable, so a
// retry can never double-execute a mutation. POST (chat completions, admin
// writes) is never retried here; the streaming chat path has its own
// transparent retry in the core client (ChatStream/ChatToolStream).
type RetryTransport struct {
	// Next performs the request. The CLI sets it to the LoggingTransport so each
	// attempt — including the failed one — is recorded for diagnostics.
	Next http.RoundTripper
	// Idle, when non-nil, has CloseIdleConnections called before the retry so
	// the dead pooled connection cannot be handed back. Set to the underlying
	// *http.Transport.
	Idle interface{ CloseIdleConnections() }
}

// RoundTrip runs req through Next and, on a retryable connection-level failure
// of an idempotent request, evicts idle connections and retries exactly once.
func (t *RetryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.Next.RoundTrip(req)
	if err == nil || !retryableIdempotent(req, err) {
		return resp, err
	}
	if t.Idle != nil {
		t.Idle.CloseIdleConnections()
	}
	return t.Next.RoundTrip(req)
}

// retryableIdempotent reports whether req may be safely re-sent after err: a
// body-less idempotent method (GET/HEAD) that failed with a connection-level
// drop — NOT a user cancel or a turn deadline, which are not transient and must
// not silently re-run.
func retryableIdempotent(req *http.Request, err error) bool {
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		return false
	}
	if req.Body != nil && req.Body != http.NoBody {
		return false
	}
	msg := err.Error()
	if strings.Contains(msg, "context canceled") || strings.Contains(msg, "deadline exceeded") {
		return false
	}
	for _, sig := range []string{
		"connection lost",                  // http2: client connection lost (max-age kill)
		"connection reset",                 // peer/middlebox RST
		"broken pipe",                      // write to a half-closed conn
		"unexpected EOF",                   // truncated response on a dying conn
		"use of closed network connection", // pool handed back a closed conn
		"GOAWAY",                           // server graceful close mid-flight
		"server closed",                    // idle conn closed under us
	} {
		if strings.Contains(msg, sig) {
			return true
		}
	}
	return false
}

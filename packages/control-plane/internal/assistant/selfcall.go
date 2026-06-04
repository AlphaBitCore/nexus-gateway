package assistant

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
)

// selfcall.go implements the P2b in-process self-call (R3). The web agent's read +
// mitigate tools reach the admin API through a core.Client; before P2b that client
// made a real loopback HTTP round-trip to this same CP (http://localhost:<port>),
// which (a) cost a TCP/TLS hop + JSON re-serialize per tool call, (b) carried the
// AI-initiated channel marker as a wire header an admin could forge, and (c) blurred
// the audit SourceIp (the call appeared to originate from localhost).
//
// inProcessTransport replaces that hop: CP-bound requests are dispatched straight
// into the CP echo router in-process (no socket), while inference calls to the AI
// Gateway (a different host) still go over the real network. It is installed as the
// core.Client's http transport, so all 28 Gateway methods stay byte-unchanged.

// inProcessTransport is the core.Client round-tripper for the web assistant. It
// routes by host: requests to the CP itself dispatch into the in-process handler
// (the echo router, with its full AdminAuth → IAM → audit middleware stack intact);
// every other host (the AI Gateway inference endpoint) delegates to base.
type inProcessTransport struct {
	handler   http.Handler      // the CP echo router (in-process dispatch target)
	cpHost    string            // host:port of CPBaseURL — requests to it dispatch in-process
	sourceIP  string            // the originating web user's IP, stamped for the audit actor
	requestID string            // the originating chat turn's X-Nexus-Request-Id, for audit correlation
	base      http.RoundTripper // network transport for inference / external hosts
}

// newInProcessTransport builds the transport. cpBaseURL is parsed for its host; a
// blank/invalid host means nothing is dispatched in-process (everything → base),
// which keeps tests that point CPBaseURL at an httptest server on the network path.
func newInProcessTransport(handler http.Handler, cpBaseURL, sourceIP, requestID string, base http.RoundTripper) *inProcessTransport {
	if base == nil {
		base = newAssistantBaseTransport()
	}
	host := ""
	if u, err := url.Parse(cpBaseURL); err == nil {
		host = u.Host
	}
	return &inProcessTransport{handler: handler, cpHost: host, sourceIP: sourceIP, requestID: requestID, base: base}
}

// newAssistantBaseTransport clones the default transport and widens the TLS handshake
// budget — mirroring core.NewHTTPTransport, so inference calls
// to a slow-TLS upstream behave the same as a CLI core.Client would.
func newAssistantBaseTransport() *http.Transport {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSHandshakeTimeout = 30 * time.Second
	return tr
}

// RoundTrip dispatches CP-host requests in-process and everything else over base.
func (t *inProcessTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.handler == nil || t.cpHost == "" || req.URL.Host != t.cpHost {
		return t.base.RoundTrip(req) // inference / external host → real network
	}

	// In-process CP self-call. Clone first (the RoundTripper contract forbids
	// mutating the caller's *http.Request) onto a context that carries the
	// UNFORGEABLE assistant initiator signal — a network client cannot set a Go
	// context value, so this is the tamper-evident replacement for the old wire
	// header (E90 I5 / #18b H1). Stamp the originating user's IP so EntryFor's
	// c.RealIP() records the human actor, not the loopback.
	r := req.Clone(audit.WithInitiator(req.Context(), audit.ViaAssistant))
	if t.sourceIP != "" {
		r.Header.Set("X-Real-IP", t.sourceIP)
		r.Header.Set("X-Forwarded-For", t.sourceIP)
		r.RemoteAddr = net.JoinHostPort(t.sourceIP, "0")
	}
	// Propagate the originating chat turn's request id so the audit row for an
	// AI-initiated write correlates to the conversation that triggered it. The CP's
	// NexusRequestID middleware honors an inbound X-Nexus-Request-Id; without this the
	// self-call would mint a fresh, uncorrelated id.
	if t.requestID != "" {
		r.Header.Set("X-Nexus-Request-Id", t.requestID)
	}
	// net/http guarantees a server-side request has a non-nil Body; a client-built
	// request (what core.Client hands us) leaves Body nil for a bodyless GET. Mimic
	// the server contract so a handler that reads the body does not panic.
	if r.Body == nil {
		r.Body = http.NoBody
	}

	// Dispatch through the full CP middleware chain (AdminAuth → IAM → audit). NOTE:
	// this also re-runs the global access-log + request-metrics middleware, so each
	// in-process tool call counts as one CP HTTP request in those observability
	// surfaces — the CP request count is inflated by the agent's self-calls and does
	// not reflect external ingress alone.
	rec := &inProcRecorder{}
	t.handler.ServeHTTP(rec, r)

	code := rec.code
	if code == 0 {
		code = http.StatusOK // a handler that wrote a body without an explicit code
	}
	body := rec.buf.Bytes()
	return &http.Response{
		StatusCode:    code,
		Status:        http.StatusText(code),
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        rec.Header(),
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}, nil
}

// inProcRecorder is a minimal http.ResponseWriter capturing one in-process response.
// The admin handlers the assistant calls write a single JSON (or empty) body and do
// not stream, so a buffered recorder is sufficient; Flush is a harmless no-op so the
// recorder satisfies http.Flusher for any middleware that probes for it.
type inProcRecorder struct {
	header http.Header
	buf    bytes.Buffer
	code   int
}

func (r *inProcRecorder) Header() http.Header {
	if r.header == nil {
		r.header = make(http.Header)
	}
	return r.header
}

func (r *inProcRecorder) Write(b []byte) (int, error) {
	if r.code == 0 {
		r.code = http.StatusOK
	}
	return r.buf.Write(b)
}

func (r *inProcRecorder) WriteHeader(code int) {
	if r.code == 0 {
		r.code = code
	}
}

func (r *inProcRecorder) Flush() {}

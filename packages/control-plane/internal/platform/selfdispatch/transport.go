// Package selfdispatch is the in-process self-call transport: an
// http.RoundTripper that dispatches CP-bound requests straight into the CP echo
// router (no socket) while delegating every other host to a base network
// transport. It replaces a loopback HTTP hop, which (a) cost a TCP/TLS round-trip
// + JSON re-serialize per call, (b) carried the initiator marker as a wire header
// an admin could forge, and (c) blurred the audit SourceIp (calls appeared to
// originate from localhost).
//
// Its consumer is the web assistant: initiator=ViaAssistant, source IP = the
// live caller's IP, request id = the chat turn's request id, no extra
// credential (the caller's bearer rides on the core.Client token source).
package selfdispatch

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/initiator"
)

// Config parameterizes a Transport. Handler and CPBaseURL determine what is
// dispatched in-process; Initiator/SourceIP/RequestID shape the in-process
// request so it authenticates and audits as the right principal.
type Config struct {
	// Handler is the CP echo router (the in-process dispatch target). A nil
	// Handler (or a blank CPBaseURL host) means nothing is dispatched in-process
	// and every request goes to Base — which keeps tests pointing CPBaseURL at an
	// httptest server on the network path.
	Handler http.Handler
	// CPBaseURL is parsed for its host:port; requests to that host dispatch
	// in-process, all others go to Base.
	CPBaseURL string
	// Initiator is the unforgeable channel marker stamped on in-process requests
	// (initiator.ViaAssistant). Required for in-process dispatch — a blank
	// initiator would make the call indistinguishable from an external human
	// request.
	Initiator string
	// SourceIP is stamped as X-Real-IP / X-Forwarded-For / RemoteAddr so the
	// audit actor records the originating human (assistant) or run starter
	// not the loopback. Blank → not stamped.
	SourceIP string
	// RequestID is propagated as X-Nexus-Request-Id so the audit row correlates
	// to the originating chat turn. Blank → not stamped;
	// the CP then mints a fresh uncorrelated id.
	RequestID string
	// Base is the network transport for inference / external hosts. nil → a clone
	// of the default transport with a widened TLS handshake budget (mirroring
	// core.NewHTTPTransport, so a slow-TLS upstream behaves as a CLI client would).
	Base http.RoundTripper
}

// Transport routes by host: requests to the CP itself dispatch into the
// in-process handler (the echo router, with its full AdminAuth → IAM → audit
// middleware stack intact); every other host (e.g. the AI Gateway inference
// endpoint) delegates to base.
type Transport struct {
	handler   http.Handler
	cpHost    string
	initiator string
	sourceIP  string
	requestID string
	base      http.RoundTripper
}

// New builds the transport from cfg. cfg.CPBaseURL is parsed for its host; a
// blank/invalid host means nothing is dispatched in-process (everything → base).
func New(cfg Config) *Transport {
	base := cfg.Base
	if base == nil {
		base = DefaultBaseTransport()
	}
	host := ""
	if u, err := url.Parse(cfg.CPBaseURL); err == nil {
		host = u.Host
	}
	return &Transport{
		handler:   cfg.Handler,
		cpHost:    host,
		initiator: cfg.Initiator,
		sourceIP:  cfg.SourceIP,
		requestID: cfg.RequestID,
		base:      base,
	}
}

// DefaultBaseTransport clones the default transport and widens the TLS handshake
// budget — mirroring core.NewHTTPTransport, so inference calls to a slow-TLS
// upstream behave the same as a CLI core.Client would.
func DefaultBaseTransport() *http.Transport {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSHandshakeTimeout = 30 * time.Second
	return tr
}

// RoundTrip dispatches CP-host requests in-process and everything else over base.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.handler == nil || t.cpHost == "" || req.URL.Host != t.cpHost {
		return t.base.RoundTrip(req) // inference / external host → real network
	}

	// In-process CP self-call. Clone first (the RoundTripper contract forbids
	// mutating the caller's *http.Request) onto a context that carries the
	// UNFORGEABLE initiator signal — a network client cannot set a Go context
	// value, so this is the tamper-evident replacement for the old wire header
	// Stamp the originating IP so EntryFor's c.RealIP()
	// records the human/run-starter actor, not the loopback.
	r := req.Clone(initiator.With(req.Context(), t.initiator))
	if t.sourceIP != "" {
		r.Header.Set("X-Real-IP", t.sourceIP)
		r.Header.Set("X-Forwarded-For", t.sourceIP)
		r.RemoteAddr = net.JoinHostPort(t.sourceIP, "0")
	}
	// Propagate the originating turn/run request id so the audit row for an
	// AI/workflow-initiated write correlates to what triggered it. The CP's
	// NexusRequestID middleware honors an inbound X-Nexus-Request-Id; without this
	// the self-call would mint a fresh, uncorrelated id.
	if t.requestID != "" {
		r.Header.Set("X-Nexus-Request-Id", t.requestID)
	}
	// net/http guarantees a server-side request has a non-nil Body; a client-built
	// request (what core.Client hands us) leaves Body nil for a bodyless GET. Mimic
	// the server contract so a handler that reads the body does not panic.
	if r.Body == nil {
		r.Body = http.NoBody
	}

	// Dispatch through the full CP middleware chain (AdminAuth → IAM → audit).
	// NOTE: this also re-runs the global access-log + request-metrics middleware,
	// so each in-process call counts as one CP HTTP request in those observability
	// surfaces — the CP request count is inflated by self-calls and does not
	// reflect external ingress alone.
	rec := &recorder{}
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

// recorder is a minimal http.ResponseWriter capturing one in-process response.
// The admin handlers these consumers call write a single JSON (or empty) body and
// do not stream, so a buffered recorder is sufficient; Flush is a harmless no-op
// so the recorder satisfies http.Flusher for any middleware that probes for it.
type recorder struct {
	header http.Header
	buf    bytes.Buffer
	code   int
}

func (r *recorder) Header() http.Header {
	if r.header == nil {
		r.header = make(http.Header)
	}
	return r.header
}

func (r *recorder) Write(b []byte) (int, error) {
	if r.code == 0 {
		r.code = http.StatusOK
	}
	return r.buf.Write(b)
}

func (r *recorder) WriteHeader(code int) {
	if r.code == 0 {
		r.code = code
	}
}

func (r *recorder) Flush() {}

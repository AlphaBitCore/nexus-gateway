// Package proxy implements the core proxy pipeline: CONNECT tunnel establishment,
// TLS interception (bump), and upstream request forwarding.
package tlsbump

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	nexushttp "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/http"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/responseio"
)

// clientHelloKey is the per-request context key under which the raw TLS
// ClientHello bytes are stored. Injected by buildForwardHandler so that
// DialTLSContext below can replay the client's fingerprint to the upstream.
type clientHelloKey struct{}

// hopByHopHeaders lists HTTP/1.1 hop-by-hop headers that must not be forwarded
// to upstream servers per RFC 2616 Section 13.5.1 and RFC 7230 Section 6.1.
var hopByHopHeaders = []string{
	"Connection",
	"Proxy-Connection",
	"Keep-Alive",
	"Transfer-Encoding",
	"TE",
	"Trailer",
	"Upgrade",
	"Proxy-Authorization",
	"Proxy-Authenticate",
}

// hopByHopHeaderSet is a pre-built lookup set keyed by http.CanonicalHeaderKey
// for O(1) membership checks in isHopByHopHeader. Built at init time from the
// canonical hopByHopHeaders slice.
var hopByHopHeaderSet = func() map[string]bool {
	m := make(map[string]bool, len(hopByHopHeaders))
	for _, h := range hopByHopHeaders {
		m[http.CanonicalHeaderKey(h)] = true
	}
	return m
}()

// UpstreamTransport manages a shared HTTP transport with connection pooling
// and HTTP/2 support for forwarding requests to upstream servers.
type UpstreamTransport struct {
	transport http.RoundTripper
	// requestInjector is invoked per-request inside ForwardRequest
	// before the wire send. Used by the agent to stamp the
	// X-Nexus-Attestation header on every outbound request. Nil
	// (production CP) is the no-op default.
	requestInjector func(req *http.Request) error
}

// UpstreamOptions tunes optional behaviour of NewUpstreamTransportWith.
// Zero value matches the legacy NewUpstreamTransport behaviour byte-for-
// byte so existing callers (compliance-proxy, agent legacy path) need
// no migration.
type UpstreamOptions struct {
	// Proxy, when non-nil, routes outbound requests through this HTTP
	// proxy. Reserved for callers that want explicit HTTP_PROXY
	// routing (not used by the agent — see RequestInjector below for
	// the agent's attestation path).
	Proxy *url.URL
	// GetProxyConnectHeader is invoked by Go's stdlib on every
	// proxy CONNECT emission when Proxy is set. Reserved alongside
	// Proxy; not used by the agent attestation path.
	//
	// IMPORTANT — fail-open contract: callbacks MUST return
	// `(nil, nil)` on any signing failure. A non-nil error aborts
	// the request at the stdlib layer and turns signing problems
	// into customer-visible 502s — explicitly forbidden by the
	// attestation architecture's fail-open rule.
	GetProxyConnectHeader func(ctx context.Context, proxyURL *url.URL, target string) (http.Header, error)
	// RequestInjector, when non-nil, is invoked once per outbound
	// request inside ForwardRequest BEFORE the request is sent. It
	// receives the (already-cloned) outbound *http.Request so it
	// can mutate headers and/or rewrap the body. Used by the agent
	// to inject the `X-Nexus-Attestation` header (and the body-hash
	// commitment) without any HTTP_PROXY plumbing — agent stays
	// unaware of where its traffic ends up; compliance-proxy detects
	// the header after its own transparent TLS bump.
	//
	// Fail-open contract: an injector error MUST translate to "no
	// header added"; ForwardRequest swallows the error and forwards
	// the request unmodified. Returning the error to the caller
	// would turn every signing problem into a customer-visible failure.
	RequestInjector func(req *http.Request) error
	// UpstreamProxy, when non-nil, routes the MITM upstream's uTLS dial
	// through an egress proxy (socks5:// or http://) at the TCP-dial level —
	// distinct from Proxy above, which is the stdlib HTTP-CONNECT path used by
	// the attestation chain. The proxy only sees the CONNECT target host:port;
	// the inner uTLS handshake rides on top, so MITM inspection is preserved.
	// Used by the agent to forward intercepted AI traffic through a local
	// SOCKS/HTTP proxy in environments where direct egress to the provider is
	// blocked. The SO_MARK dial control still applies to the proxy hop.
	UpstreamProxy *url.URL
}

// NewUpstreamTransport creates a transport with uTLS fingerprint passthrough
// and configurable connection pooling. Equivalent to
// `NewUpstreamTransportWith(..., UpstreamOptions{})`.
//
// maxConnsPerHost controls the maximum number of connections per upstream host.
// idleConnTimeout controls how long idle connections remain in the pool.
// dialTimeout controls the timeout for establishing new TCP connections.
//
// TLS to the upstream is handled via uTLS (DialTLSContext). The client's raw
// ClientHello bytes — captured by BumpConnection and threaded through the
// request context under clientHelloKey — are replayed verbatim so the
// upstream sees the client's real JA3 fingerprint. ALPN is offered faithfully
// as [h2, http/1.1]: a protocolDispatchRoundTripper (upstream_h2.go) routes
// h2-negotiating upstreams through golang.org/x/net/http2 — which owns the h2
// framing + PING the stdlib cannot apply to a *utls.UConn — and http/1.1
// upstreams through this stdlib transport unchanged. The proxy speaks H2 to the
// client (bump.go / serveHTTP2) so end-to-end H2 is preserved for clients that
// require it (connect-RPC bidi/streaming agents).
//
// The returned RoundTripper is wrapped via nexushttp.WrapTransport so every
// upstream MITM call emits the platform-wide outbound HTTP debug log
// (caller=cp-upstream). PropagateReqID is false because the user's request
// already carries x-nexus-request-id from the agent / ai-gateway upstream of
// compliance-proxy; re-adding it here would be redundant.
func NewUpstreamTransport(maxConnsPerHost int, idleConnTimeout, dialTimeout time.Duration) (*UpstreamTransport, error) {
	return NewUpstreamTransportWith(maxConnsPerHost, idleConnTimeout, dialTimeout, UpstreamOptions{})
}

// upstreamDialControl applies the process-wide outbound dial control to the
// MITM upstream socket. The Linux agent installs an SO_MARK setter (via
// nexushttp.SetGlobalDialControl) so its own egress escapes the NEXUS_AGENT
// iptables REDIRECT; without it the upstream forward self-loops back into the
// agent's listener. Looked up at dial time so it is robust to construction
// order, and a no-op (nil hook) in the compliance-proxy, ai-gateway, and on
// macOS/Windows, which never install the hook.
func upstreamDialControl(network, address string, c syscall.RawConn) error {
	if ctl := nexushttp.GlobalDialControl(); ctl != nil {
		return ctl(network, address, c)
	}
	return nil
}

// NewUpstreamTransportWith is the option-aware constructor. Production
// callers that need the legacy behaviour use NewUpstreamTransport (no
// options); the agent's attestation wire-up calls this directly with
// UpstreamOptions{Proxy: ..., GetProxyConnectHeader: ...}.
func NewUpstreamTransportWith(maxConnsPerHost int, idleConnTimeout, dialTimeout time.Duration, opts UpstreamOptions) (*UpstreamTransport, error) {
	dialer := &net.Dialer{
		Timeout:   dialTimeout,
		KeepAlive: 30 * time.Second,
		// SO_MARK the MITM upstream connection so the Linux agent's own
		// iptables REDIRECT (the NEXUS_AGENT chain) RETURNs it instead of
		// looping it back into the proxy listener. Without this the agent's
		// upstream forward is re-intercepted by its own rule — a self-loop
		// that cascades into hundreds of flows and a ~20s timeout / 502.
		// Resolved at dial time via the process-wide hook the agent installs
		// at startup; nil / no-op in the compliance-proxy, the ai-gateway,
		// and on macOS/Windows (which never install it).
		Control: upstreamDialControl,
	}

	fd := &fingerprintDialer{dialer: dialer, upstreamProxy: opts.UpstreamProxy}

	base := &http.Transport{
		MaxConnsPerHost:     maxConnsPerHost,
		MaxIdleConnsPerHost: maxConnsPerHost,
		IdleConnTimeout:     idleConnTimeout,
		DialContext:         dialer.DialContext,
		// Irrelevant here: this stdlib transport only serves the http/1.1
		// authorities the dispatcher routes to it (Go cannot upgrade a
		// custom-dialed *utls.UConn to h2). h2 authorities use the http2.Transport.
		ForceAttemptHTTP2: false,
		// Bounds the wait for response headers, not the body. Long streaming
		// agent runs can take minutes before the first byte; 300s leaves the
		// overall limit to the request ctx while still catching a wedged upstream.
		ResponseHeaderTimeout: 300 * time.Second,
		// fd.dial takes full ownership of the TLS handshake via uTLS; Go's
		// transport will not wrap the returned conn with crypto/tls.
		DialTLSContext: fd.dial,
	}
	// With a proxy configured, Go's stdlib emits CONNECT then TLS;
	// GetProxyConnectHeader lets the agent's signer add X-Nexus-Attestation.
	if opts.Proxy != nil {
		base.Proxy = http.ProxyURL(opts.Proxy)
		if opts.GetProxyConnectHeader != nil {
			base.GetProxyConnectHeader = opts.GetProxyConnectHeader
		}
	}

	// Dispatch h2/h1 per upstream ALPN; tracing + debug-log wraps sit OUTSIDE so
	// both protocol paths are instrumented. See buildUpstreamRoundTripper.
	upstream := buildUpstreamRoundTripper(base, fd, opts.Proxy != nil)

	return &UpstreamTransport{
		// Wrap the WrapTransport result with the shared tracing transport
		// so any forward request whose context carries a PhaseSink captures
		// upstream TTFB + upstream-total. Calls without a sink pass through
		// unchanged.
		transport: traffic.NewTracingTransport(nexushttp.WrapTransport(upstream, nexushttp.WrapOpts{
			Caller:         "cp-upstream",
			PropagateReqID: false,
		})),
		requestInjector: opts.RequestInjector,
	}, nil
}

// ForwardRequest sends the request to the upstream and returns the response.
// It removes hop-by-hop headers, clears RequestURI (required for client requests),
// and records upstream request duration metrics.
func (u *UpstreamTransport) ForwardRequest(ctx context.Context, req *http.Request) (*http.Response, error) {
	// Clone the request so we do not mutate the original.
	outReq := req.Clone(ctx)

	// RequestURI must be empty for client requests (net/http requirement).
	outReq.RequestURI = ""

	// Remove hop-by-hop headers that must not be forwarded.
	for _, h := range hopByHopHeaders {
		outReq.Header.Del(h)
	}

	// Strip the client's Accept-Encoding so Go's transport owns compression
	// negotiation end-to-end. Without this, the client's "Accept-Encoding: gzip"
	// passes through to the upstream, the upstream responds with
	// Content-Encoding: gzip, but Go's transport does NOT auto-decompress
	// (it only does that when it injected the header itself). The result is that
	// the compliance pipeline — including the SSE path — sees raw compressed
	// bytes it cannot parse or store as readable text. By removing the header
	// here, Go adds its own Accept-Encoding, auto-decompresses on receipt, and
	// sets resp.Uncompressed = true, so every downstream path gets plain bytes.
	// Clients always accept uncompressed responses even when they sent
	// Accept-Encoding, so this never breaks the client-facing flow.
	outReq.Header.Del("Accept-Encoding")

	// Invoke the per-request injector AFTER hop-by-hop scrubbing +
	// Accept-Encoding strip, but BEFORE the wire send. The injector may
	// read + replace req.Body (it does so to compute the body-hash
	// commitment), so it runs late enough that all CP/agent hook
	// mutations are already settled. Failure is swallowed (fail-open
	// contract) — the request still forwards, just without the
	// X-Nexus-Attestation header. CP that receives a request without a
	// valid header runs its normal MITM pipeline.
	if u.requestInjector != nil {
		_ = u.requestInjector(outReq)
	}

	host := outReq.URL.Host

	start := time.Now()
	resp, err := u.transport.RoundTrip(outReq)
	durationMs := float64(time.Since(start).Milliseconds())

	if err != nil {
		if UpstreamRequestMs != nil {
			UpstreamRequestMs.With(host, "error").Observe(durationMs)
		}
		return nil, fmt.Errorf("upstream round-trip to %s: %w", host, err)
	}

	if UpstreamRequestMs != nil {
		UpstreamRequestMs.With(host, strconv.Itoa(resp.StatusCode)).Observe(durationMs)
	}
	return resp, nil
}

// isHopByHopHeader returns true if the header name is a hop-by-hop header
// that must not be forwarded in proxy responses per RFC 7230 §6.1.
func isHopByHopHeader(name string) bool {
	return hopByHopHeaderSet[http.CanonicalHeaderKey(name)]
}

// copyResponse writes the upstream response back to the client response writer.
// It strips hop-by-hop headers (static list and any names in Connection per
// RFC 7230 §6.1), invokes hook (if non-nil) to let the caller mutate headers
// before they are sent, writes the status code, and streams the body.
//
// The hook parameter uses responseio.HeaderHook so Phase 3 can inject
// x-nexus-* response markers without changing this function's signature.
func copyResponse(w http.ResponseWriter, resp *http.Response, hook responseio.HeaderHook) error {
	defer func() {
		_ = resp.Body.Close()
	}()

	// RFC 7230 §6.1: strip dynamic hop-by-hop headers listed in Connection
	// before deleting Connection itself. Values iterates every Connection line.
	for _, line := range resp.Header.Values("Connection") {
		for _, name := range strings.Split(line, ",") {
			if n := strings.TrimSpace(name); n != "" {
				resp.Header.Del(n)
			}
		}
	}

	// Strip the static set of hop-by-hop headers (includes Connection itself).
	for _, h := range hopByHopHeaders {
		resp.Header.Del(h)
	}

	// Allow the caller to mutate headers (e.g. inject x-nexus-* markers).
	if hook != nil {
		hook(resp)
	}

	// Copy surviving response headers to the client.
	for key, values := range resp.Header {
		for _, v := range values {
			w.Header().Add(key, v)
		}
	}

	w.WriteHeader(resp.StatusCode)

	if _, err := io.Copy(w, resp.Body); err != nil {
		return fmt.Errorf("copy response body: %w", err)
	}

	// Flush if the writer supports it (important for SSE / streaming).
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}

	return nil
}

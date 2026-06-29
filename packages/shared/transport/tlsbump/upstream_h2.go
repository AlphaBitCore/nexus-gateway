package tlsbump

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
)

// ALPN protocol identifiers negotiated over the replayed uTLS handshake.
const (
	protoH2 = "h2"
	protoH1 = "http/1.1"
)

// errUpstreamNotH2 is returned by the h2 transport's dial hook when the upstream
// negotiated something other than HTTP/2 over the offered [h2, http/1.1] ALPN
// list. It is never surfaced to a caller: protocolDispatchRoundTripper detects
// it (errors.Is), records the authority as http/1.1, and retries the request on
// the HTTP/1.1 transport. The upstream sees a single replayed ClientHello; the
// extra probe-and-close only happens once per authority (then the cache routes
// straight to the right transport).
var errUpstreamNotH2 = errors.New("tlsbump: upstream did not negotiate h2")

// fingerprintDialFunc dials addr and completes a TLS handshake, returning the
// (typically *utls.UConn) connection. newH2DispatchTransport takes this as a
// seam so the protocol-dispatch behavior can be driven end-to-end in tests with
// real h2/h1 handshakes against loopback servers, without the production uTLS
// fingerprint + cert-verification path.
type fingerprintDialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// fingerprintDialer dials addr and completes the uTLS handshake mirroring the
// client's fingerprint. ALPN is offered as [h2, http/1.1] (see utls_dialer.go)
// so the upstream can negotiate HTTP/2. The raw ClientHello is pulled from the
// context (clientHelloKey), injected by buildForwardHandler.
type fingerprintDialer struct {
	dialer        *net.Dialer
	upstreamProxy *url.URL
}

func (d *fingerprintDialer) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	rawHello, _ := ctx.Value(clientHelloKey{}).([]byte)
	return dialWithFingerprint(ctx, network, addr, rawHello, d.dialer, d.upstreamProxy)
}

// protocolDispatchRoundTripper routes each upstream MITM request to an HTTP/2 or
// HTTP/1.1 transport based on the ALPN protocol the upstream negotiates over the
// replayed uTLS fingerprint.
//
// Why this exists: the MITM upstream dials with a custom uTLS DialTLSContext so
// the upstream sees the client's real JA3. Go's standard http.Transport cannot
// upgrade a custom-dialed *utls.UConn to HTTP/2 — it only auto-detects h2 on a
// *tls.Conn it dialed itself — so forcing http/1.1 ALPN would be the only
// stdlib option. That downgrade breaks clients whose protocol needs h2 end-to-end:
// connect-RPC bidi/streaming plus h2 PING keepalive (e.g. an IDE agent service)
// cancel ~30s in when their stream is forced onto HTTP/1.1.
//
// This dispatcher offers ALPN [h2, http/1.1] faithfully and, per upstream
// authority, routes h2-negotiating hosts through golang.org/x/net/http2 (which
// owns the h2 framing + PING the stdlib cannot apply to a *utls.UConn) and
// http/1.1 hosts through the existing stdlib transport. Hosts that negotiate
// http/1.1 keep byte-for-byte the prior behavior.
type protocolDispatchRoundTripper struct {
	h1 http.RoundTripper // stdlib transport for http/1.1 authorities
	h2 *http2.Transport  // h2 framing for h2 authorities

	// protos caches the negotiated protocol per authority (host:port) so a
	// known-http/1.1 host skips the h2 probe-and-fallback on every request.
	// Correctness does not depend on the cache (the errUpstreamNotH2 fallback
	// always fires); the cache only removes the repeated probe.
	protos sync.Map // authority(host:port) -> protoH2 | protoH1
}

// RoundTrip dispatches req to the h2 or h1 transport. Unknown and known-h2
// authorities attempt h2 first; the h2 dial hook records the negotiated
// protocol and, when the upstream chose http/1.1, returns errUpstreamNotH2
// BEFORE the request body is touched (the failure is at GetClientConn / dial,
// ahead of cc.RoundTrip), so the retry on the h1 transport replays an untouched
// body.
func (rt *protocolDispatchRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	// Only https authorities have an ALPN to negotiate. Plain http:// (and any
	// non-TLS scheme) has none — the h2 transport rejects it outright
	// ("unencrypted HTTP/2 not enabled") and the MITM path never speaks h2c —
	// so route it straight to the stdlib transport.
	if req.URL.Scheme != "https" {
		return rt.h1.RoundTrip(req)
	}

	authority := canonicalAuthority(req.URL)

	// Known http/1.1 authority: straight to the stdlib transport, no h2 probe.
	if v, ok := rt.protos.Load(authority); ok && v.(string) == protoH1 {
		return rt.h1.RoundTrip(req)
	}

	resp, err := rt.h2.RoundTrip(req)
	if err != nil && errors.Is(err, errUpstreamNotH2) {
		// Upstream negotiated http/1.1 over the [h2, http/1.1] offer. The dial
		// hook already cached the authority and closed the probe conn; retry on
		// the stdlib transport, which dials its own http/1.1 connection.
		return rt.h1.RoundTrip(req)
	}
	return resp, err
}

// buildUpstreamRoundTripper assembles the upstream transport around the stdlib
// base. With no HTTP-CONNECT proxy it returns the h2/h1 dispatcher.
//
// When an HTTP-CONNECT proxy is configured (the agent's attestation forwarding
// chain), the dispatcher is bypassed and base is returned unchanged: the h2
// dispatcher's custom uTLS DialTLSContext connects directly and would silently
// skip the proxy (and its GetProxyConnectHeader signer). That leg forwards to
// the compliance-proxy, which re-bumps and applies its own upstream h2, so the
// agent leg needs no end-to-end h2 here. opts.UpstreamProxy (TCP-level SOCKS/
// HTTP egress) is unaffected — it rides inside dialWithFingerprint, so h2
// dispatch still applies over it.
func buildUpstreamRoundTripper(base *http.Transport, fd *fingerprintDialer, hasConnectProxy bool) http.RoundTripper {
	if hasConnectProxy {
		return base
	}
	return newH2DispatchTransport(base, fd.dial)
}

// newH2DispatchTransport builds the protocol-dispatching RoundTripper around the
// already-constructed stdlib transport (h1) and a fingerprint dialer shared by
// both protocol paths. The h2 transport's dial hook validates the negotiated
// ALPN and feeds protocol decisions back into the dispatcher's cache.
func newH2DispatchTransport(h1 http.RoundTripper, dial fingerprintDialFunc) *protocolDispatchRoundTripper {
	rt := &protocolDispatchRoundTripper{h1: h1}
	rt.h2 = &http2.Transport{
		// Mirror the client's own h2 PING keepalive: if no frame is seen for
		// ReadIdleTimeout, send a PING; close the conn if no pong within
		// PingTimeout. This detects a dead upstream on a long streaming agent
		// run instead of hanging the stream — the same keepalive an h2 client
		// (e.g. Cursor) runs on its own side.
		ReadIdleTimeout: 30 * time.Second,
		PingTimeout:     15 * time.Second,
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			conn, err := dial(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			uconn, ok := conn.(*utls.UConn)
			if !ok {
				// dialWithFingerprint always returns *utls.UConn; treat any
				// other type as h2 (http2.Transport asked for an h2 conn) and
				// let the h2 handshake surface a real error if it is not.
				rt.protos.Store(addr, protoH2)
				return conn, nil
			}
			if uconn.ConnectionState().NegotiatedProtocol != protoH2 {
				rt.protos.Store(addr, protoH1)
				_ = uconn.Close()
				return nil, errUpstreamNotH2
			}
			rt.protos.Store(addr, protoH2)
			return uconn, nil
		},
	}
	return rt
}

// canonicalAuthority renders u as the host:port authority key used by the
// dispatcher cache, mirroring how golang.org/x/net/http2 derives the dial addr
// (default port 443 for https). Exact equality with the http2 addr is not
// required for correctness — the errUpstreamNotH2 fallback fires regardless —
// only for the fast-path cache hit.
func canonicalAuthority(u *url.URL) string {
	// Lower-case the host so this key matches the addr golang.org/x/net/http2
	// passes to the dial hook (it idna-ASCII-lowercases the authority). A
	// mismatch would only cost the fast-path cache hit (the errUpstreamNotH2
	// fallback still fires), but matching keeps a mixed-case host from
	// re-probing h2 on every request.
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if port == "" {
		port = "443"
		if u.Scheme == "http" {
			port = "80"
		}
	}
	return net.JoinHostPort(host, port)
}

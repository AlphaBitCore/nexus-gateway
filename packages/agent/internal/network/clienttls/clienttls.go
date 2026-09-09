// Package clienttls installs a TLS configuration onto an existing
// *http.Client whose transport may be wrapped in RoundTripper decorators.
// The agent's outbound HTTP clients (Hub enrollment, the hub sync client)
// build a base client via shared/transport/http and then pin their mTLS
// cert + CA pool through these helpers.
package clienttls

import (
	"crypto/tls"
	"errors"
	"net/http"
)

// underlyingHTTPTransport walks any RoundTripper-wrapper chain (anything
// exposing Unwrap() http.RoundTripper) to find the inner *http.Transport.
// Returns nil if the chain bottoms out at a non-*http.Transport.
func underlyingHTTPTransport(rt http.RoundTripper) *http.Transport {
	for {
		if tr, ok := rt.(*http.Transport); ok {
			return tr
		}
		type unwrapper interface{ Unwrap() http.RoundTripper }
		u, ok := rt.(unwrapper)
		if !ok {
			return nil
		}
		rt = u.Unwrap()
		if rt == nil {
			return nil
		}
	}
}

// WithTLSConfig installs cfg as the *tls.Config on c's transport. Use this
// when the call site needs to pin a CA pool, set MinVersion, or install
// both client cert and CA in one shot — for example, the Hub client which
// validates the Hub's CA and presents the agent's mTLS cert.
//
// The transport's existing ALPN list survives the install unless cfg states
// one of its own — see the note inside. Without that, pinning a CA pool
// silently downgraded the connection to HTTP/1.1.
//
// Must be called once during construction, before concurrent outbound
// requests are in flight. CloseIdleConnections forces the next dial to read
// the updated config but does not interrupt in-flight requests.
//
// Returns an error if c is nil or c's transport chain does not bottom out at
// *http.Transport. shared/transport/http wraps the transport in a logging
// RoundTripper, so the chain is walked via Unwrap() to find the underlying
// *http.Transport.
func WithTLSConfig(c *http.Client, cfg *tls.Config) error {
	if c == nil {
		return errors.New("clienttls: nil http.Client")
	}
	tr := underlyingHTTPTransport(c.Transport)
	if tr == nil {
		return errors.New("clienttls: client transport chain does not contain *http.Transport")
	}
	if cfg == nil {
		tr.TLSClientConfig = nil
	} else {
		next := cfg.Clone()
		// Carry the transport's ALPN list across the replacement unless the
		// caller stated one of their own.
		//
		// The base client from shared/transport/http calls
		// http2.ConfigureTransports, and that writes "h2" into
		// TLSClientConfig.NextProtos. Replacing the whole config therefore
		// dropped the ALPN offer, so both agent Hub clients — which call this
		// only to pin a CA pool and an mTLS cert, saying nothing about ALPN —
		// silently negotiated HTTP/1.1. TLSNextProto still held the h2
		// registration, but it can only fire on a handshake that NEGOTIATED
		// h2, and there was no longer an offer to negotiate from. The
		// ReadIdleTimeout keep-alive probe configured beside it became dead
		// configuration, and nothing logged the downgrade.
		//
		// A caller that DOES set NextProtos is choosing deliberately and wins.
		if len(next.NextProtos) == 0 && tr.TLSClientConfig != nil && len(tr.TLSClientConfig.NextProtos) > 0 {
			next.NextProtos = append([]string(nil), tr.TLSClientConfig.NextProtos...)
		}
		tr.TLSClientConfig = next
	}
	tr.CloseIdleConnections()
	return nil
}

// UnderlyingTransport returns the *http.Transport on c. Used by call sites
// that need to wrap the transport in a RoundTripper decorator (e.g.
// otelhttp.NewTransport) while keeping the per-host pool and H2
// configuration. The returned pointer must not be mutated; use WithTLSConfig
// for TLS adjustments.
//
// Returns an error if c is nil or c's transport chain does not bottom out at
// *http.Transport.
func UnderlyingTransport(c *http.Client) (*http.Transport, error) {
	if c == nil {
		return nil, errors.New("clienttls: nil http.Client")
	}
	tr := underlyingHTTPTransport(c.Transport)
	if tr == nil {
		return nil, errors.New("clienttls: client transport chain does not contain *http.Transport")
	}
	return tr, nil
}

package relay

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

// WithClientCert installs cert as the client certificate on c's
// transport. Sugar for WithTLSConfig with a config that carries only
// the cert.
//
// Returns an error if c is nil or c's transport chain does not bottom
// out at *http.Transport.
func WithClientCert(c *http.Client, cert tls.Certificate) error {
	if c == nil {
		return errors.New("relay: nil http.Client")
	}
	tr := underlyingHTTPTransport(c.Transport)
	if tr == nil {
		return errors.New("relay: client transport chain does not contain *http.Transport")
	}
	if tr.TLSClientConfig == nil {
		tr.TLSClientConfig = &tls.Config{}
	}
	tr.TLSClientConfig.Certificates = []tls.Certificate{cert}
	tr.CloseIdleConnections()
	return nil
}

// WithTLSConfig installs cfg as the *tls.Config on c's transport. Use
// this when the call site needs to pin a CA pool, set MinVersion, or
// install both client cert and CA in one shot — for example, the Hub
// client which validates the Hub's CA and presents the agent's mTLS
// cert.
//
// Must be called once during construction, before concurrent outbound
// requests are in flight. CloseIdleConnections forces the next dial to
// read the updated config but does not interrupt in-flight requests.
//
// Returns an error if c is nil or c's transport chain does not bottom
// out at *http.Transport.
//
// Together with WithClientCert this is the only place in the agent
// codebase allowed to reach for *http.Transport. shared/httpclient now
// wraps the transport in a logging RoundTripper, so we walk Unwrap()
// to find the underlying *http.Transport (see
// agent-transport-rewrite-design §5.5).
func WithTLSConfig(c *http.Client, cfg *tls.Config) error {
	if c == nil {
		return errors.New("relay: nil http.Client")
	}
	tr := underlyingHTTPTransport(c.Transport)
	if tr == nil {
		return errors.New("relay: client transport chain does not contain *http.Transport")
	}
	if cfg == nil {
		tr.TLSClientConfig = nil
	} else {
		tr.TLSClientConfig = cfg.Clone()
	}
	tr.CloseIdleConnections()
	return nil
}

// UnderlyingTransport returns the *http.Transport on c. Used by call
// sites that need to wrap the transport in a RoundTripper decorator
// (e.g. otelhttp.NewTransport) while keeping the per-host pool and H2
// configuration. The returned pointer must not be mutated; use
// WithClientCert / WithTLSConfig for TLS adjustments.
//
// Returns an error if c is nil or c's transport chain does not bottom
// out at *http.Transport.
func UnderlyingTransport(c *http.Client) (*http.Transport, error) {
	if c == nil {
		return nil, errors.New("relay: nil http.Client")
	}
	tr := underlyingHTTPTransport(c.Transport)
	if tr == nil {
		return nil, errors.New("relay: client transport chain does not contain *http.Transport")
	}
	return tr, nil
}

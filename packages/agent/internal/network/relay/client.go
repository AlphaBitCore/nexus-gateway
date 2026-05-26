package relay

import (
	"crypto/tls"
	"errors"
	"net/http"
	"net/http/httptrace"
	"syscall"
	"time"

	nexushttp "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/http"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

// Config controls Client construction. All fields except OpsRegistry
// have sensible defaults; passing the zero value is valid for tests.
type Config struct {
	// UserAgent is stamped on every outbound request that does not
	// already carry a User-Agent header. Empty leaves the header
	// untouched (stdlib default applies).
	UserAgent string

	// OpsRegistry receives relay.dial_total{host,mode} and
	// relay.handshake_total. Required (the relay is a hot-path component
	// and a nil registry would silently drop the signals). Tests pass a
	// fresh registry.NewRegistry(prometheus.NewRegistry()) so each
	// client gets isolated counters.
	OpsRegistry *registry.Registry

	// TLSClientConfig is applied to the underlying http.Transport. Tests
	// use this to install InsecureSkipVerify against httptest servers.
	// Production callers leave this nil; defaults apply.
	TLSClientConfig *tls.Config

	// MaxIdleConnsPerHost overrides the default (8). Zero means default.
	MaxIdleConnsPerHost int

	// IdleConnTimeout overrides the default (90s). Zero means default.
	IdleConnTimeout time.Duration

	// H2ReadIdleTimeout overrides the default (30s). Zero means default.
	H2ReadIdleTimeout time.Duration

	// DialTimeout overrides the default (10s). Zero means default.
	DialTimeout time.Duration

	// TLSHandshakeTimeout overrides the default (10s). Zero means default.
	TLSHandshakeTimeout time.Duration

	// DialControl, when non-nil, is forwarded to the underlying
	// nexushttp.Config.DialControl. The agent's Linux build uses
	// this to set SO_MARK on relay sockets so the agent's own
	// egress is excluded from the NEXUS_AGENT REDIRECT chain.
	// Other platforms leave this nil.
	DialControl func(network, address string, c syscall.RawConn) error
}

// Client wraps a *http.Client tuned for the agent's MITM relay outbound
// path. It is safe for concurrent use; the underlying transport handles
// per-host pooling and HTTP/2 multiplex.
type Client struct {
	httpClient *http.Client
	metrics    *metricsBundle
	userAgent  string
}

// New constructs a Client. It returns an error only when applying the
// caller-supplied TLS config requires a transport type that is not
// *http.Transport (which would only happen if shared/httpclient swaps
// transports in a future refactor).
func New(cfg Config) (*Client, error) {
	maxIdle := cfg.MaxIdleConnsPerHost
	if maxIdle == 0 {
		maxIdle = 8
	}
	idleTO := cfg.IdleConnTimeout
	if idleTO == 0 {
		idleTO = 90 * time.Second
	}
	h2Idle := cfg.H2ReadIdleTimeout
	if h2Idle == 0 {
		h2Idle = 30 * time.Second
	}
	dialTO := cfg.DialTimeout
	if dialTO == 0 {
		dialTO = 10 * time.Second
	}
	tlsTO := cfg.TLSHandshakeTimeout
	if tlsTO == 0 {
		tlsTO = 10 * time.Second
	}

	hc := nexushttp.New(nexushttp.Config{
		Timeout:             0, // Per-request via Request.Context(); SSE streams may be long-lived.
		Caller:              "agent-relay",
		DialTimeout:         dialTO,
		MaxIdleConnsPerHost: maxIdle,
		MaxConnsPerHost:     -1, // Uncapped — let the transport open more under stream-concurrency pressure.
		IdleConnTimeout:     idleTO,
		TLSHandshakeTimeout: tlsTO,
		ForceHTTP2:          nexushttp.On(),
		H2ReadIdleTimeout:   h2Idle,
		DialControl:         cfg.DialControl,
	})

	if cfg.TLSClientConfig != nil {
		tr := underlyingHTTPTransport(hc.Transport)
		if tr == nil {
			return nil, errors.New("relay: client transport chain does not contain *http.Transport")
		}
		tr.TLSClientConfig = cfg.TLSClientConfig.Clone()
	}

	// Wrap the transport with the shared tracing roundtripper so any outbound
	// request whose context carries a traffic.PhaseSink captures upstream
	// TTFB + upstream-total. Calls without a sink pass through unchanged.
	// Done AFTER the TLS config surgery so underlyingHTTPTransport still
	// bottoms out at the same *http.Transport.
	hc.Transport = traffic.NewTracingTransport(hc.Transport)

	if cfg.OpsRegistry == nil {
		return nil, errors.New("relay: OpsRegistry is required")
	}
	return &Client{
		httpClient: hc,
		metrics:    newMetrics(cfg.OpsRegistry),
		userAgent:  cfg.UserAgent,
	}, nil
}

// Do sends req and returns the response. The User-Agent header is set
// (when configured) only if the caller has not set it already. A
// httptrace.ClientTrace is attached so the dial counter labels each
// request as reused or new.
func (c *Client) Do(req *http.Request) (*http.Response, error) {
	if c.userAgent != "" && req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", c.userAgent)
	}
	host := req.URL.Hostname()
	trace := &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			mode := "reused"
			if !info.Reused {
				mode = "new"
			}
			c.metrics.dials.With(host, mode).Inc()
		},
		// TLSHandshakeStart fires once per fresh TLS handshake (HTTP/2
		// multiplex avoids re-handshakes; cached H2 connections do not
		// re-trigger this hook). The counter is unlabeled — host
		// dimensionality already lives on dial_total{host, mode=new}.
		TLSHandshakeStart: func() {
			c.metrics.handshakes.With().Inc()
		},
	}
	ctx := httptrace.WithClientTrace(req.Context(), trace)
	return c.httpClient.Do(req.WithContext(ctx))
}

// HTTPClient returns the underlying *http.Client. Callers should
// generally use Do; HTTPClient exists for tests and for the
// WithClientCert helper that mutates the transport's TLS config.
func (c *Client) HTTPClient() *http.Client {
	return c.httpClient
}

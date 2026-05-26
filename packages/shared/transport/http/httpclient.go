// Package http is the single source of *http.Client construction
// for outbound calls across all data-plane services.
//
// Bare http.Client literals and http.DefaultClient are forbidden in
// production code paths (enforced by forbidigo). Every outbound HTTP
// caller — provider adapters, webhook hooks, alerting, SIEM sinks,
// thingclient HTTP fallback — instantiates a tuned *http.Client through
// New or NewProbe at startup and reuses it for the lifetime of the
// process.
package http

import (
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/net/http2"
)

// Config controls *http.Client behaviour. All fields are optional; zero
// values are filled with sensible defaults appropriate for upstream API
// calls (LLM providers, webhooks, agent → gateway forwarding).
type Config struct {
	// Timeout is the absolute upper bound on a single Do call. Streaming
	// callers should use Request.Context() deadlines on top of this.
	// Zero means use 30s.
	Timeout time.Duration

	// DialTimeout caps the TCP connect step. Zero means use 10s.
	DialTimeout time.Duration

	// KeepAlive sets the TCP keep-alive interval. Zero means use 30s.
	KeepAlive time.Duration

	// MaxIdleConns is the global pool size across all hosts. Zero means
	// use 200.
	MaxIdleConns int

	// MaxIdleConnsPerHost is the per-host idle pool size. Zero means
	// use 50.
	MaxIdleConnsPerHost int

	// MaxConnsPerHost caps total connections per host. Zero means use
	// 100; a negative value means no cap (translated to 0 for net/http).
	MaxConnsPerHost int

	// IdleConnTimeout is the idle-pool eviction window. Zero means use
	// 90s.
	IdleConnTimeout time.Duration

	// TLSHandshakeTimeout caps the TLS handshake. Zero means use 10s.
	TLSHandshakeTimeout time.Duration

	// ResponseHeaderTimeout caps how long the server may take to send
	// response headers after the request is fully written. Zero means
	// no deadline (use Timeout instead).
	ResponseHeaderTimeout time.Duration

	// ForceHTTP2 enables HTTP/2 negotiation. Defaults to true. Set
	// false only for debugging or for callers known to require HTTP/1
	// (rare).
	ForceHTTP2 *bool

	// H2ReadIdleTimeout enables HTTP/2 PING-frame health checking.
	// Connections idle for longer than this trigger a PING; if it fails
	// the transport tears the connection down so the next request opens
	// a fresh one. Zero means no PING (stdlib default). Recommended:
	// 30s for long-lived gateway connections.
	H2ReadIdleTimeout time.Duration

	// Logger receives the per-request log record (debug on success,
	// warn on transport error). nil → slog.Default().
	Logger *slog.Logger

	// Caller is the static identifier written to every log line as caller=…
	// Empty string → "unknown".
	Caller string

	// PropagateReqID, when true, copies the context request id into the
	// outbound x-nexus-request-id header (only if the request does not
	// already set one). Off by default — opt in for calls to peer services
	// in this platform; leave off for calls to opaque third-party APIs.
	PropagateReqID bool

	// DialControl, when non-nil, is invoked on every freshly-created socket
	// before connect() so callers may set socket options like SO_MARK
	// (Linux transparent-proxy SELF-exclusion) or SO_REUSEADDR. The
	// signature matches [net.Dialer.Control]. Nil leaves the default
	// behaviour (no extra setsockopt calls).
	DialControl func(network, address string, c syscall.RawConn) error
}

// globalDialControl is a process-wide hook callers may install via
// [SetGlobalDialControl] so every freshly-built transport inherits
// it. The Linux agent uses this to apply SO_MARK to all of its
// outbound sockets without having to plumb a Config.DialControl
// through every constructor. Per-call Config.DialControl always
// wins when non-nil.
var globalDialControl atomic.Pointer[func(network, address string, c syscall.RawConn) error]

// SetGlobalDialControl installs a process-wide [net.Dialer.Control]
// callback consulted by every [New]-built transport whose
// Config.DialControl is nil. Pass nil to clear. Safe for concurrent
// use; calls are last-writer-wins.
//
// The Linux agent calls this once at startup with a function that
// sets SO_MARK = 0x4E58 on every outbound socket, so the agent's
// own egress (Hub WebSocket, audit upload, enrollment, MITM
// upstream, updater) is excluded from the NEXUS_AGENT REDIRECT
// chain.
func SetGlobalDialControl(fn func(network, address string, c syscall.RawConn) error) {
	if fn == nil {
		globalDialControl.Store(nil)
		return
	}
	globalDialControl.Store(&fn)
}

// GlobalDialControl returns the currently-installed process-wide
// control callback, or nil if none. Exported so packages that build
// dialers outside [New] (e.g. thingclient's coder/websocket dialer)
// can pick it up too.
func GlobalDialControl() func(network, address string, c syscall.RawConn) error {
	if p := globalDialControl.Load(); p != nil {
		return *p
	}
	return nil
}

// resolveDialControl returns the per-call Config.DialControl when
// non-nil, otherwise falls back to the process-wide global.
func resolveDialControl(cfg Config) func(network, address string, c syscall.RawConn) error {
	if cfg.DialControl != nil {
		return cfg.DialControl
	}
	return GlobalDialControl()
}

// New returns a *http.Client whose Transport applies the given Config.
// The transport is freshly constructed; do not share Configs across
// calls if you intend distinct pools.
func New(cfg Config) *http.Client {
	cfg = applyDefaults(cfg)

	tr := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   cfg.DialTimeout,
			KeepAlive: cfg.KeepAlive,
			Control:   resolveDialControl(cfg),
		}).DialContext,
		MaxIdleConns:          cfg.MaxIdleConns,
		MaxIdleConnsPerHost:   cfg.MaxIdleConnsPerHost,
		MaxConnsPerHost:       maxConnsPerHostValue(cfg.MaxConnsPerHost),
		IdleConnTimeout:       cfg.IdleConnTimeout,
		TLSHandshakeTimeout:   cfg.TLSHandshakeTimeout,
		ResponseHeaderTimeout: cfg.ResponseHeaderTimeout,
		ForceAttemptHTTP2:     boolValue(cfg.ForceHTTP2, true),
	}

	if boolValue(cfg.ForceHTTP2, true) {
		// Explicit ConfigureTransport gives access to ReadIdleTimeout,
		// which ForceAttemptHTTP2 alone does not expose.
		if h2tr, err := http2.ConfigureTransports(tr); err == nil && h2tr != nil {
			h2tr.ReadIdleTimeout = cfg.H2ReadIdleTimeout
		}
	}

	return &http.Client{
		Timeout: cfg.Timeout,
		Transport: WrapTransport(tr, WrapOpts{
			Logger:         cfg.Logger,
			Caller:         cfg.Caller,
			PropagateReqID: cfg.PropagateReqID,
		}),
	}
}

// NewProbe returns a short-timeout *http.Client suitable for health
// checks and reachability probes. The pool is intentionally small.
func NewProbe() *http.Client {
	return New(Config{
		Timeout:             5 * time.Second,
		DialTimeout:         3 * time.Second,
		KeepAlive:           30 * time.Second,
		MaxIdleConns:        20,
		MaxIdleConnsPerHost: 5,
		IdleConnTimeout:     30 * time.Second,
		TLSHandshakeTimeout: 3 * time.Second,
	})
}

func applyDefaults(c Config) Config {
	if c.Timeout == 0 {
		c.Timeout = 30 * time.Second
	}
	if c.DialTimeout == 0 {
		c.DialTimeout = 10 * time.Second
	}
	if c.KeepAlive == 0 {
		c.KeepAlive = 30 * time.Second
	}
	if c.MaxIdleConns == 0 {
		c.MaxIdleConns = 200
	}
	if c.MaxIdleConnsPerHost == 0 {
		c.MaxIdleConnsPerHost = 50
	}
	if c.MaxConnsPerHost == 0 {
		c.MaxConnsPerHost = 100
	}
	if c.IdleConnTimeout == 0 {
		c.IdleConnTimeout = 90 * time.Second
	}
	if c.TLSHandshakeTimeout == 0 {
		c.TLSHandshakeTimeout = 10 * time.Second
	}
	return c
}

func boolValue(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

func maxConnsPerHostValue(v int) int {
	if v < 0 {
		return 0 // net/http treats 0 as "no cap"
	}
	return v
}

// Off returns a *bool false suitable for ForceHTTP2 when callers want
// to opt out of HTTP/2 explicitly.
func Off() *bool {
	v := false
	return &v
}

// On returns a *bool true. Equivalent to leaving ForceHTTP2 nil but
// makes intent explicit at call sites.
func On() *bool {
	v := true
	return &v
}

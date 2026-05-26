// Package specutil provides shared helpers used across the
// per-provider AdapterSpec subpackages — HTTP client construction,
// OpenAI-compatible SSE decoding, and error envelope parsing.
//
// HTTP client construction delegates to packages/shared/transport/http.
// Provider adapters call NewHTTPClient/NewProbeClient at construction
// time and reuse the returned client for the lifetime of the adapter.
//
// The upstream client tunables (timeout, dial timeout, idle pool size,
// ...) are seeded from the ai-gateway config at startup via
// [Configure]. Until Configure is called the package-level defaults
// match the values that used to be hardcoded here.
package specutil

import (
	"net/http"
	"sync/atomic"
	"time"

	nexushttp "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/http"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

// HTTPConfig tunes the upstream client every provider adapter shares.
type HTTPConfig struct {
	// Timeout is the per-request budget. The underlying http.Client.Timeout
	// is set slightly higher (Timeout+5s) so the per-Do context deadline
	// usually fires first and we surface a clean timeout error path.
	Timeout time.Duration
	// DialTimeout caps the TCP connect phase.
	DialTimeout time.Duration
	// KeepAlive is the TCP keep-alive interval on dialed connections.
	KeepAlive time.Duration
	// TLSHandshakeTimeout caps the TLS handshake.
	TLSHandshakeTimeout time.Duration
	// IdleConnTimeout is how long an idle pooled conn survives.
	IdleConnTimeout time.Duration
	// MaxIdleConns is the global pool cap.
	MaxIdleConns int
	// MaxIdleConnsPerHost is the per-host pool cap.
	MaxIdleConnsPerHost int
}

// DefaultHTTPConfig returns the baseline upstream HTTP client tunables.
// Used as the initial config and as a source of fill-in-the-blanks defaults
// inside [Configure].
func DefaultHTTPConfig() HTTPConfig {
	return HTTPConfig{
		Timeout:             120 * time.Second,
		DialTimeout:         15 * time.Second,
		KeepAlive:           30 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
		IdleConnTimeout:     90 * time.Second,
		MaxIdleConns:        200,
		MaxIdleConnsPerHost: 50,
	}
}

// ProbeTimeout is the maximum time a provider probe call may take.
// Health checks must not block the gateway — kept small and fixed,
// independent of the upstream call budget.
const ProbeTimeout = 5 * time.Second

// activeConfig holds the live HTTPConfig snapshot. atomic.Pointer is
// used so tests that call [Configure] mid-suite don't race with adapter
// constructors built in other goroutines.
var activeConfig atomic.Pointer[HTTPConfig]

// activeRT is the swappable RoundTripper backing the upstream singleton
// client. Configure rebuilds it with the new config and atomic-swaps so
// the next outbound request uses the new transport-level tunables
// (dial timeout, idle pool, TLS handshake, keep-alive). The previous
// transport's idle pool is closed in a background goroutine.
var activeRT atomic.Pointer[http.RoundTripper]

// liveTransport is the http.Transport implementation embedded in the
// upstream singleton client. Each RoundTrip delegates to activeRT so a
// Configure call takes effect on the next request — no client rebuild,
// no adapter refresh.
type liveTransport struct{}

// RoundTrip delegates to the currently active RoundTripper. activeRT is
// always populated by init() before any request can route through.
func (liveTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rt := activeRT.Load()
	return (*rt).RoundTrip(req)
}

// upstreamSingleton is the shared *http.Client every provider adapter
// receives from NewHTTPClient. The pointer is stable for the lifetime
// of the process so adapters that cache it at construction continue to
// see live transport updates. Timeout=0 means the per-request budget is
// enforced via the caller's context deadline (gateway handlers read it
// from ActiveConfig().Timeout); the http.Client level has no fallback,
// which prevents stale baked-in timeouts from masking the live value.
var upstreamSingleton = &http.Client{
	Transport: liveTransport{},
	// Intentionally 0 — see comment above.
}

// probeSingleton has fixed tunables (probes must stay cheap and snappy
// regardless of upstream policy) so it does not participate in the
// hot-swap. Constructed once at init time.
var probeSingleton = nexushttp.New(nexushttp.Config{
	Timeout:             5 * time.Second,
	DialTimeout:         3 * time.Second,
	KeepAlive:           30 * time.Second,
	MaxIdleConns:        20,
	MaxIdleConnsPerHost: 5,
	IdleConnTimeout:     30 * time.Second,
	TLSHandshakeTimeout: 3 * time.Second,
	Caller:              "provider-probe",
})

func init() {
	d := DefaultHTTPConfig()
	activeConfig.Store(&d)
	rt := buildUpstreamTransport(d)
	activeRT.Store(&rt)
}

// buildUpstreamTransport produces a new RoundTripper for the upstream
// singleton's swappable Transport. Delegates to shared/httpclient so
// outbound calls retain the standard logging wrapper, then wraps with
// shared/traffic tracing so any request whose context carries a PhaseSink
// populates upstream TTFB + upstream-total. Requests without a sink pass
// through with no observable cost.
func buildUpstreamTransport(cfg HTTPConfig) http.RoundTripper {
	base := nexushttp.New(nexushttp.Config{
		Timeout:             cfg.Timeout + 5*time.Second,
		DialTimeout:         cfg.DialTimeout,
		KeepAlive:           cfg.KeepAlive,
		MaxIdleConns:        cfg.MaxIdleConns,
		MaxIdleConnsPerHost: cfg.MaxIdleConnsPerHost,
		IdleConnTimeout:     cfg.IdleConnTimeout,
		TLSHandshakeTimeout: cfg.TLSHandshakeTimeout,
		Caller:              "provider-upstream",
	}).Transport
	return traffic.NewTracingTransport(base)
}

// Configure replaces the upstream config snapshot and rebuilds the
// shared RoundTripper used by every adapter. Zero-valued fields fall
// back to the matching [DefaultHTTPConfig] entry so partial YAML blocks
// (or partial shadow payloads) behave intuitively. The old transport's
// idle conn pool is best-effort closed in a background goroutine; in-
// flight requests on the old transport complete normally before GC
// reclaims the object.
//
// Called once at startup with the YAML `upstream:` block — the
// upstream transport tunables are yaml-only (SRE operator concern, not
// admin business policy) and require a redeploy to change.
func Configure(cfg HTTPConfig) {
	def := DefaultHTTPConfig()
	if cfg.Timeout <= 0 {
		cfg.Timeout = def.Timeout
	}
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = def.DialTimeout
	}
	if cfg.KeepAlive <= 0 {
		cfg.KeepAlive = def.KeepAlive
	}
	if cfg.TLSHandshakeTimeout <= 0 {
		cfg.TLSHandshakeTimeout = def.TLSHandshakeTimeout
	}
	if cfg.IdleConnTimeout <= 0 {
		cfg.IdleConnTimeout = def.IdleConnTimeout
	}
	if cfg.MaxIdleConns <= 0 {
		cfg.MaxIdleConns = def.MaxIdleConns
	}
	if cfg.MaxIdleConnsPerHost <= 0 {
		cfg.MaxIdleConnsPerHost = def.MaxIdleConnsPerHost
	}
	activeConfig.Store(&cfg)

	newRT := buildUpstreamTransport(cfg)
	old := activeRT.Swap(&newRT)
	if old != nil {
		// Drain idle conns on the old transport. The swappable wrapper
		// already routes new requests to newRT; closing idles on old
		// frees the socket pool without disrupting in-flight requests.
		go closeIdleConns(*old)
	}
}

// closeIdleConns invokes CloseIdleConnections on the underlying
// transport, defensively handling the case where the wrapped logging
// RoundTripper does not expose the method directly.
func closeIdleConns(rt http.RoundTripper) {
	type idleCloser interface{ CloseIdleConnections() }
	if c, ok := rt.(idleCloser); ok {
		c.CloseIdleConnections()
	}
}

// ActiveConfig returns the current snapshot. Useful for places that
// need the per-request budget (e.g. streaming response WriteDeadline,
// per-request context deadlines applied by the gateway handler) without
// taking a separate config dependency.
func ActiveConfig() HTTPConfig {
	return *activeConfig.Load()
}

// NewHTTPClient returns the shared upstream singleton. The returned
// pointer is stable for the lifetime of the process; the underlying
// Transport is swapped atomically via [Configure], so adapters that
// cache this value at construction continue to route through the live
// transport on every request.
func NewHTTPClient() *http.Client {
	return upstreamSingleton
}

// NewProbeClient returns the shared probe singleton. Probe tunables are
// fixed (see [ProbeTimeout]) so this client is not affected by
// Configure.
func NewProbeClient() *http.Client {
	return probeSingleton
}

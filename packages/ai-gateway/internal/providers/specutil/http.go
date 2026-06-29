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

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	nexushttp "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/http"
)

// HTTPConfig tunes the upstream client every provider adapter shares.
type HTTPConfig struct {
	// Timeout is the per-request budget. For a non-streaming call it caps the
	// whole exchange; for a streaming call it caps time-to-first-byte (the
	// model's think time before the first token) — once tokens flow,
	// StreamIdleTimeout governs instead. It also feeds the Transport's
	// ResponseHeaderTimeout. Raise it for deployments serving long-reasoning
	// models that take many minutes before the first byte.
	Timeout time.Duration
	// StreamIdleTimeout is the silence budget for a streaming response: the
	// downstream write deadline is reset to now+StreamIdleTimeout on every
	// chunk, so an actively-producing stream runs unbounded (any inference
	// length) while a stalled upstream is cut after this much quiet. Only
	// applies after the first byte; the initial wait is bounded by Timeout.
	StreamIdleTimeout time.Duration
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
	// MaxIdleConnsPerHost is the per-host idle pool cap.
	MaxIdleConnsPerHost int
	// MaxConnsPerHost caps TOTAL (active + idle) connections to one upstream host.
	// CRITICAL for a proxy: when in-flight requests to a host exceed this, the
	// transport BLOCKS them waiting for a connection to free — capping upstream
	// concurrency regardless of how much the gateway can otherwise serve. The shared
	// httpclient defaults this to 100 (right for fan-out clients hitting many hosts,
	// WRONG for the upstream proxy which drives high concurrency to a FEW provider
	// hosts), so the upstream transport sets it explicitly high. 0 here would inherit
	// that 100 default — so DefaultHTTPConfig pins a real value; mirrors Bifrost's
	// fasthttp MaxConnsPerHost (default 5000).
	MaxConnsPerHost int
}

// DefaultHTTPConfig returns the baseline upstream HTTP client tunables.
// Used as the initial config and as a source of fill-in-the-blanks defaults
// inside [Configure].
func DefaultHTTPConfig() HTTPConfig {
	return HTTPConfig{
		// 600s budget: long enough for most reasoning models' time-to-first
		// token; deployments serving multi-minute think times raise it via
		// upstream.timeoutSec. Active streaming is governed by StreamIdleTimeout,
		// so a longer Timeout here does not cap a producing stream.
		Timeout:             600 * time.Second,
		StreamIdleTimeout:   120 * time.Second,
		DialTimeout:         15 * time.Second,
		KeepAlive:           30 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
		// Pool sized for high-concurrency bursts (warm conns survive longer →
		// fewer fresh TLS handshakes); per-host pool sized for the dominant
		// target (OpenAI).
		IdleConnTimeout:     300 * time.Second,
		MaxIdleConns:        2000,
		MaxIdleConnsPerHost: 500,
		// Total per-host connection cap. 5000 mirrors Bifrost's fasthttp default and
		// is well above any realistic single-box concurrency, so the upstream call is
		// never connection-starved (the prior implicit 100 default capped upstream
		// concurrency at 100 → throughput collapsed under load). Tunable via
		// AI_GATEWAY_UPSTREAM_MAX_CONNS_PER_HOST.
		MaxConnsPerHost: 5000,
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
// enforced via the caller's context deadline: the dispatcher wraps each
// non-streaming upstream call with context.WithTimeout(ActiveConfig().Timeout)
// (spec_adapter.go), and the Transport's ResponseHeaderTimeout (set from the
// same budget in buildUpstreamTransport) bounds time-to-headers on both
// stream and non-stream paths. The http.Client level has no fallback, which
// prevents stale baked-in timeouts from masking the live value.
var upstreamSingleton = &http.Client{
	Transport: liveTransport{},
	// Intentionally 0 — see comment above.
}

// probeSingleton has fixed tunables (probes must stay cheap and snappy
// regardless of upstream policy) so it does not participate in the
// hot-swap. Constructed once at init time.
//
// The probe dials an admin-supplied provider base URL (a fresh,
// not-yet-saved URL on the test-connection path), making it an SSRF primitive.
// It is guarded with the AdminEgressAllowPrivate policy: the cloud-metadata /
// link-local range (169.254.169.254 et al.) is refused at dial time, but
// RFC-1918 / loopback is permitted so on-prem self-hosted providers
// (vLLM / Ollama on a private address) stay reachable. The guard runs on the
// resolved address, so it also defeats a hostname that DNS-rebinds to the
// metadata endpoint at dial time.
var probeSingleton = nexushttp.New(nexushttp.Config{
	Timeout:             5 * time.Second,
	DialTimeout:         3 * time.Second,
	KeepAlive:           30 * time.Second,
	MaxIdleConns:        20,
	MaxIdleConnsPerHost: 5,
	IdleConnTimeout:     30 * time.Second,
	TLSHandshakeTimeout: 3 * time.Second,
	Caller:              "provider-probe",
	DialControl:         nexushttp.AdminEgressDialControl(nexushttp.AdminEgressAllowPrivate),
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
		MaxConnsPerHost:     cfg.MaxConnsPerHost,
		IdleConnTimeout:     cfg.IdleConnTimeout,
		TLSHandshakeTimeout: cfg.TLSHandshakeTimeout,
		// ResponseHeaderTimeout bounds the time from finishing the request
		// write to receiving the upstream response headers. This is the only
		// budget that survives in the singleton's Transport (the wrapping
		// http.Client.Timeout is discarded below — we extract only
		// .Transport), so without it a connected-but-silent upstream that
		// never sends headers would be bounded only by TCP keep-alive, not
		// the configured budget. Safe for streaming: SSE
		// headers arrive before the first event, so this caps time-to-headers,
		// not time-to-first-event. The non-stream body-stall case is bounded
		// separately by the per-request context deadline the dispatcher sets.
		ResponseHeaderTimeout: cfg.Timeout,
		Caller:                "provider-upstream",
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
	if cfg.StreamIdleTimeout <= 0 {
		cfg.StreamIdleTimeout = def.StreamIdleTimeout
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
	if cfg.MaxConnsPerHost <= 0 {
		cfg.MaxConnsPerHost = def.MaxConnsPerHost
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

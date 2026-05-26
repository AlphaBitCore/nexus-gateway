// Package metrics owns the compliance-proxy's L3 business instruments.
//
// Instruments are registered against a shared *registry.Registry which
// both:
//
//  1. registers the underlying Prometheus instruments on
//     prometheus.DefaultRegisterer (so /metrics scrapes keep working), and
//  2. records bindings so the per-tick Sampler.Collect() includes them in
//     metrics_sample messages pushed to Hub via thingclient.
//
// Names follow the dotted opsmetrics convention (spec §6.3 catalog for
// Compliance Proxy). Pre-GA: the old `nexus_compliance_proxy_*` namespace
// prefix is dropped per CLAUDE.md "no backcompat" rule.
package metrics

import (
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
)

var (
	// ConnectionsActive — gauge: tunnels.active
	ConnectionsActive *registry.Gauge
	// ConnectionsTotal — counter: tunnels.total{result}
	ConnectionsTotal *registry.Counter
	// TLSHandshakeMs lives in shared/transport/tlsbump (registered by tlsbump.RegisterMetrics
	// so the agent shares the same instrument).
	// CertCacheHits — counter: cert_cache.hits_total{layer}
	CertCacheHits *registry.Counter
	// CertCacheMisses — counter: cert_cache.misses_total
	CertCacheMisses *registry.Counter
	// CertCacheSize — gauge: cert_cache.size
	CertCacheSize *registry.Gauge
	// CertSignMs — histogram: cert_sign_ms (AG-specific, not in spec catalog)
	CertSignMs *registry.Histogram
	// CertPrewarmMs — gauge: cert_prewarm.duration_ms (AG-specific)
	CertPrewarmMs *registry.Gauge
	// UpstreamRequestMs lives in shared/transport/tlsbump (registered by tlsbump.RegisterMetrics).
	// RedisAvailable — gauge: redis.available (1=yes, 0=no)
	RedisAvailable *registry.Gauge
	// PinningPassthroughTotal — counter: pinning.passthrough_total{status}
	PinningPassthroughTotal *registry.Counter
	// KillSwitchActive — gauge: killswitch.active (0|1) — set by runtime API
	KillSwitchActive *registry.Gauge
	// AttestationVerifyTotal — counter: attestation.verify_total{outcome}
	// where outcome ∈ {valid, missing, invalid_sig, expired, replayed,
	// unknown_agent, disabled}. Operators alert on the sustained ratio of
	// non-valid + non-missing outcomes per architecture § 6 — a sustained
	// 1% invalid rate over 5 minutes indicates either a bad agent rollout
	// or a forgery attempt.
	AttestationVerifyTotal *registry.Counter
)

// Register binds the package-level instruments to the supplied opsmetrics
// registry. Must be called once at process startup before any data-plane
// traffic is served. Safe to call again with the same registry (registry
// re-registration is idempotent).
func Register(reg *registry.Registry) {
	if reg == nil {
		return
	}
	ConnectionsActive = reg.NewGauge("tunnels.active", nil)
	ConnectionsTotal = reg.NewCounter("tunnels.total", []string{"result"})
	CertCacheHits = reg.NewCounter("cert_cache.hits_total", []string{"layer"})
	CertCacheMisses = reg.NewCounter("cert_cache.misses_total", nil)
	CertCacheSize = reg.NewGauge("cert_cache.size", nil)
	CertSignMs = reg.NewHistogram("cert_sign_ms", nil)
	CertPrewarmMs = reg.NewGauge("cert_prewarm.duration_ms", nil)
	RedisAvailable = reg.NewGauge("redis.available", nil)
	PinningPassthroughTotal = reg.NewCounter("pinning.passthrough_total", []string{"status"})
	KillSwitchActive = reg.NewGauge("killswitch.active", nil)
	AttestationVerifyTotal = reg.NewCounter("attestation.verify_total", []string{"outcome"})
}

// Package cache provides in-memory config caching for the compliance-proxy.
//
// Metrics are registered against a shared *registry.Registry which both
// registers underlying Prometheus instruments (so /metrics scrapes keep
// working) and records bindings so the per-tick Sampler.Collect() includes
// them in metrics_sample messages pushed to Hub via thingclient.
//
// Names follow the dotted opsmetrics convention. Pre-GA: the old
// `nexus_compliance_proxy_config_*` namespace prefix is dropped per CLAUDE.md
// "no backcompat" rule.
package cache

import (
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
)

var (
	// CacheHits — counter: config_cache.hit_total{category}
	CacheHits *registry.Counter
	// CacheMisses — counter: config_cache.miss_total{category}
	CacheMisses *registry.Counter
	// LastRefresh — gauge: config.last_refresh_timestamp{category}
	LastRefresh *registry.Gauge
	// Staleness — gauge: config.staleness_seconds{category}
	Staleness *registry.Gauge
)

// Register binds the package-level instruments to the supplied opsmetrics
// registry. Must be called once at process startup before any cache traffic
// is served. Safe to call again with the same registry (registry
// re-registration is idempotent).
func Register(reg *registry.Registry) {
	if reg == nil {
		return
	}
	CacheHits = reg.NewCounter("config_cache.hit_total", []string{"category"})
	CacheMisses = reg.NewCounter("config_cache.miss_total", []string{"category"})
	LastRefresh = reg.NewGauge("config.last_refresh_timestamp", []string{"category"})
	Staleness = reg.NewGauge("config.staleness_seconds", []string{"category"})
}

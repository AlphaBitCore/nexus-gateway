package cachelayer

import (
	opsmetrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
)

// Metrics owns the opsmetrics instruments that observe the data-plane
// configuration cache. Construct once at startup with a shared
// *opsmetrics.Registry (which also registers the underlying Prometheus
// instruments) and pass via Config.Metrics.
//
// Names follow the ops-metrics spec catalog (§6.3) for AI Gateway:
//
//	cache.hits_total{cache}
//	cache.misses_total{cache}
//	cache.size{cache}
//	cache.invalidations_total{cache, source}
//
// `source` distinguishes who initiated the invalidation — today every drop
// originates from a Hub-pushed config_changed delta, so we pin the value to
// `"hub"`. Future invalidation sources (e.g. local TTL eviction, admin force
// reload) should pass their own value via the dedicated With() label.
type Metrics struct {
	hits        *opsmetrics.Counter
	misses      *opsmetrics.Counter
	size        *opsmetrics.Gauge
	invalidates *opsmetrics.Counter
}

// NewMetrics registers the cachelayer instruments on the shared opsmetrics
// registry. Re-registration of the same name is idempotent on the registry
// side; in practice it is called once per process at startup.
func NewMetrics(reg *opsmetrics.Registry) *Metrics {
	if reg == nil {
		return nil
	}
	return &Metrics{
		hits:        reg.NewCounter("cache.hits_total", []string{"cache"}),
		misses:      reg.NewCounter("cache.misses_total", []string{"cache"}),
		size:        reg.NewGauge("cache.size", []string{"cache"}),
		invalidates: reg.NewCounter("cache.invalidations_total", []string{"cache", "source"}),
	}
}

// invalidationSource is the only invalidation initiator wired today. See the
// type doc on Metrics for the rationale.
const invalidationSource = "hub"

// bindLayer installs opsmetrics instrumentation hooks into the Layer.
// Called from New when Config.Metrics is set.
func (m *Metrics) bindLayer(l *Layer) {
	if m == nil || l == nil {
		return
	}
	l.vkOnHit = func() { m.hits.With("key_virtual_keys").Inc() }
	l.vkOnMiss = func() { m.misses.With("key_virtual_keys").Inc() }
	l.vkOnInvalidate = func(_ int) {
		m.invalidates.With("key_virtual_keys", invalidationSource).Inc()
		m.size.With("key_virtual_keys").Set(float64(l.vkeys.Size()))
	}
	l.snapshotOnReload = func(name string) {
		dim := "snapshot_" + name
		m.invalidates.With(dim, invalidationSource).Inc()
		switch name {
		case "providers":
			m.size.With("snapshot_providers").Set(float64(l.providers.Size()))
		case "models":
			m.size.With("snapshot_models").Set(float64(l.models.Size()))
		case "credentials":
			m.size.With("snapshot_credentials").Set(float64(l.credentials.Size()))
		}
	}
}

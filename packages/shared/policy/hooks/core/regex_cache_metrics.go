package core

import (
	"errors"

	"github.com/prometheus/client_golang/prometheus"
)

// RegisterRegexCacheMetrics registers the regex-cache counters with the
// provided Prometheus registerer. Call once per service during bootstrap.
// Uses Register (not MustRegister) + AlreadyRegisteredError handling so
// repeated registration on the same registerer is a no-op rather than a
// panic — services that re-init for hot reload won't crash.
func RegisterRegexCacheMetrics(reg prometheus.Registerer) {
	for _, c := range []prometheus.Collector{regexCacheHits, regexCacheMisses} {
		if err := reg.Register(c); err != nil {
			var already prometheus.AlreadyRegisteredError
			if !errors.As(err, &already) {
				panic(err)
			}
		}
	}
}

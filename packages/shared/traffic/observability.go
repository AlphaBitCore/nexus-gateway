package traffic

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Metrics for the traffic interception layer.
// Registered once at startup; namespace comes from the calling service.
var (
	trafficMetricsOnce sync.Once
	unmatchedTotal     *prometheus.CounterVec
)

// RegisterMetrics initialises Prometheus metrics for the traffic layer.
// Must be called once at startup with the service's namespace
// (e.g. "nexus_compliance_proxy"). Subsequent calls are no-ops; the first
// namespace wins. Uses sync.Once to prevent duplicate registration panics
// and to guard against accidental overwrites.
func RegisterMetrics(namespace string) {
	trafficMetricsOnce.Do(func() {
		unmatchedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "traffic_unmatched_total",
			Help:      "Requests to known AI domains that had no matching filter rule or adapter",
		}, []string{"host", "reason"})
	})
}

// RecordUnmatched increments the unmatched traffic counter.
// reason ∈ { "no_rule", "no_adapter", "parse_error", "unknown_schema" }.
func RecordUnmatched(host, reason string) {
	if unmatchedTotal != nil {
		unmatchedTotal.WithLabelValues(host, reason).Inc()
	}
}

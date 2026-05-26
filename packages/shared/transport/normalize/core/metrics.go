package core

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Metrics is the registered Prometheus metric set for the normalize
// package. Constructed once per data-plane service with the service's
// own namespace (e.g. "nexus_ai_gateway"). Subsequent calls with the
// same namespace return the cached instance to avoid duplicate
// registration panics in shared-package tests.
type Metrics struct {
	Total         *prometheus.CounterVec
	LatencyMs     *prometheus.HistogramVec
	PayloadBytes  *prometheus.HistogramVec
	FallbackTotal *prometheus.CounterVec
}

var (
	metricsMu    sync.Mutex
	metricsCache = map[string]*Metrics{}
)

// NewMetrics registers and returns the metric set under the given
// namespace. Safe to call multiple times per namespace per process.
//
// Labels:
//   - adapter: normalizer ID (e.g. "openai-chat", "anthropic-messages",
//     "generic-http", "unsupported").
//   - kind: NormalizedPayload.Kind value.
//   - direction: "request" | "response".
//   - status: "ok" | "partial" | "failed".
//
// FallbackTotal carries a separate `reason` label describing why the
// fallback fired (e.g. "no-normalizer-match", "parse-error").
func NewMetrics(reg prometheus.Registerer, namespace string) *Metrics {
	metricsMu.Lock()
	defer metricsMu.Unlock()
	if m, ok := metricsCache[namespace]; ok {
		return m
	}
	factory := promauto.With(reg)
	m := &Metrics{
		Total: factory.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: "normalize",
			Name:      "total",
			Help:      "Number of normalize calls partitioned by adapter, kind, direction, status.",
		}, []string{"adapter", "kind", "direction", "status"}),

		LatencyMs: factory.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace,
			Subsystem: "normalize",
			Name:      "latency_ms",
			Help:      "Normalize call latency in milliseconds, partitioned by adapter and direction.",
			Buckets:   []float64{0.5, 1, 2, 5, 10, 25, 50, 100, 250, 500, 1000},
		}, []string{"adapter", "direction"}),

		PayloadBytes: factory.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace,
			Subsystem: "normalize",
			Name:      "payload_bytes",
			Help:      "Size of the produced NormalizedPayload JSON in bytes.",
			Buckets:   prometheus.ExponentialBuckets(256, 4, 8), // 256, 1K, 4K, 16K, 64K, 256K, 1M, 4M
		}, []string{"adapter", "direction"}),

		FallbackTotal: factory.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: "normalize",
			Name:      "fallback_total",
			Help:      "Number of times the registry fell through to a fallback or recorded an unsupported normalize, partitioned by reason.",
		}, []string{"reason"}),
	}
	metricsCache[namespace] = m
	return m
}

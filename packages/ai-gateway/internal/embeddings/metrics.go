package embeddings

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Metrics holds the Prometheus counters and histograms for embedding
// client calls. Constructed once per Client and registered against the
// default registerer via promauto.
type Metrics struct {
	// callsTotal counts embedding HTTP calls by provider, model, and
	// outcome ("success" | "timeout" | "provider_error" | "dim_mismatch").
	callsTotal *prometheus.CounterVec
	// latencySeconds tracks the wall-clock duration of each embedding call.
	latencySeconds prometheus.Histogram
	// requestTokensTotal accumulates prompt_tokens from successful calls.
	requestTokensTotal *prometheus.CounterVec
}

// NewMetrics registers the embedding metrics under the given namespace.
// The namespace is typically the ai-gateway service namespace (e.g.
// "nexus") so all counters appear in the same Prometheus job as the
// rest of the gateway metrics.
func NewMetrics(namespace string) *Metrics {
	return &Metrics{
		callsTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "embeddings_calls_total",
			Help:      "Total embedding provider HTTP calls partitioned by provider, model, and outcome.",
		}, []string{"provider", "model", "outcome"}),

		latencySeconds: promauto.NewHistogram(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "embeddings_latency_seconds",
			Help:      "Wall-clock latency of embedding provider calls in seconds.",
			Buckets:   prometheus.DefBuckets,
		}),

		requestTokensTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "embeddings_request_tokens_total",
			Help:      "Accumulated prompt tokens consumed by embedding calls.",
		}, []string{"provider", "model"}),
	}
}

// observeCall records a completed embedding call (success or error).
func (m *Metrics) observeCall(provider, model, outcome string, latency float64) {
	if m == nil {
		return
	}
	m.callsTotal.WithLabelValues(provider, model, outcome).Inc()
	m.latencySeconds.Observe(latency)
}

// observeTokens records prompt_tokens on a successful embedding call.
func (m *Metrics) observeTokens(provider, model string, tokens int) {
	if m == nil {
		return
	}
	m.requestTokensTotal.WithLabelValues(provider, model).Add(float64(tokens))
}

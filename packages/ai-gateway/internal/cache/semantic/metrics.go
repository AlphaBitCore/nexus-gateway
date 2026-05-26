package semantic

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Metrics holds all Prometheus instruments for the L2 semantic cache package.
// A nil *Metrics is safe: every method is a no-op on a nil receiver.
type Metrics struct {
	// l2WritesTotal counts L2 write attempts by outcome.
	// outcome labels: ok, skip_disabled, skip_oversize, skip_time_sensitive,
	//   skip_embedding_timeout, skip_embedding_circuit, skip_embedding_dim_mismatch,
	//   skip_embedding_error, skip_valkey_unavailable, skip_search_error.
	l2WritesTotal *prometheus.CounterVec

	// l2EntrySizeBytes is a histogram of the serialised response_body size
	// on successful HSET writes.
	l2EntrySizeBytes prometheus.Histogram

	// l2WriteLatencySeconds is a histogram of total L2 write latency
	// (embed + HSET) on the write path.
	l2WriteLatencySeconds prometheus.Histogram

	// embeddingCallsTotal counts embedding calls partitioned by provider,
	// model, and outcome. Outcomes: ok, error, coalesced, circuit_open.
	embeddingCallsTotal *prometheus.CounterVec

	// embeddingLatencySeconds tracks the wall-clock latency of leader
	// embedding calls (joiners are not counted — they share the leader's
	// result with no HTTP cost).
	embeddingLatencySeconds prometheus.Histogram

	// embeddingCostUSDTotal accumulates the embedding cost in USD for
	// leader calls. Joiners stamp 0.0 per the cost-accounting principle
	// (only the leader actually paid).
	embeddingCostUSDTotal *prometheus.CounterVec

	// l2LookupsTotal counts L2 read (FT.SEARCH) outcomes partitioned by
	// outcome: hit, threshold_miss, miss, skip_disabled, skip_<reason>.
	l2LookupsTotal *prometheus.CounterVec

	// l2SimilarityHistogram records the distribution of best-neighbour
	// cosine similarity values on every Lookup call (0 on skip/miss).
	// Buckets: 0.5, 0.7, 0.8, 0.85, 0.9, 0.92, 0.94, 0.96, 0.98, 1.0.
	l2SimilarityHistogram prometheus.Histogram

	// l2LookupLatencySeconds tracks the FT.SEARCH query latency on the
	// read path.
	l2LookupLatencySeconds prometheus.Histogram

	// l2FeedbackTotal counts admin negative-feedback submissions by reason.
	// The reason is the admin-provided string from the feedback body.
	l2FeedbackTotal *prometheus.CounterVec

	// l2PoisonHitsTotal counts the number of times Reader.Read rejected a
	// candidate entry because it appeared in the poison list.
	l2PoisonHitsTotal prometheus.Counter
}

// NewMetrics constructs and registers the metric set under namespace.
// namespace is typically "nexus". Registered against the default
// Prometheus registerer via promauto.
func NewMetrics(namespace string) *Metrics {
	return &Metrics{
		l2WritesTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "cache_l2_writes_total",
			Help:      "Total L2 semantic cache write attempts partitioned by outcome.",
		}, []string{"outcome"}),

		l2EntrySizeBytes: promauto.NewHistogram(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "cache_l2_entry_size_bytes",
			Help:      "Serialised response_body size in bytes on successful L2 HSET writes.",
			Buckets:   prometheus.ExponentialBuckets(1024, 4, 8), // 1KiB → 64KiB+
		}),

		l2WriteLatencySeconds: promauto.NewHistogram(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "cache_l2_write_latency_seconds",
			Help:      "End-to-end L2 write latency in seconds (embedding call + HSET).",
			Buckets:   prometheus.DefBuckets,
		}),

		embeddingCallsTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "cache_embedding_calls_total",
			Help:      "Total embedding provider calls partitioned by provider, model, and outcome (ok, error, coalesced, circuit_open).",
		}, []string{"provider", "model", "outcome"}),

		embeddingLatencySeconds: promauto.NewHistogram(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "cache_embedding_latency_seconds",
			Help:      "Wall-clock latency of leader embedding HTTP calls in seconds.",
			Buckets:   prometheus.DefBuckets,
		}),

		embeddingCostUSDTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "cache_embedding_cost_usd_total",
			Help:      "Accumulated embedding cost in USD for leader calls (joiners stamp 0).",
		}, []string{"provider", "model"}),

		l2LookupsTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "cache_l2_lookups_total",
			Help:      "Total L2 semantic cache lookup (FT.SEARCH) outcomes partitioned by outcome label.",
		}, []string{"outcome"}),

		l2SimilarityHistogram: promauto.NewHistogram(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "cache_l2_similarity",
			Help:      "Distribution of best-neighbour cosine similarity values on L2 lookups (0 when skip/miss).",
			Buckets:   []float64{0.5, 0.7, 0.8, 0.85, 0.9, 0.92, 0.94, 0.96, 0.98, 1.0},
		}),

		l2LookupLatencySeconds: promauto.NewHistogram(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "cache_l2_lookup_latency_seconds",
			Help:      "End-to-end L2 read path latency in seconds (embedding call + FT.SEARCH).",
			Buckets:   prometheus.DefBuckets,
		}),

		l2FeedbackTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "cache_l2_feedback_total",
			Help:      "Total admin negative-feedback submissions partitioned by admin-provided reason.",
		}, []string{"reason"}),

		l2PoisonHitsTotal: promauto.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "cache_l2_poison_hits_total",
			Help:      "Total L2 hits rejected because the entry was in the poison list.",
		}),
	}
}

// IncWrite increments the l2WritesTotal counter for the given outcome label.
func (m *Metrics) IncWrite(outcome string) {
	if m == nil {
		return
	}
	m.l2WritesTotal.WithLabelValues(outcome).Inc()
}

// ObserveEntrySize records a successful write's response_body size.
func (m *Metrics) ObserveEntrySize(bytes int) {
	if m == nil {
		return
	}
	m.l2EntrySizeBytes.Observe(float64(bytes))
}

// ObserveWriteLatency records the end-to-end write latency in seconds.
func (m *Metrics) ObserveWriteLatency(seconds float64) {
	if m == nil {
		return
	}
	m.l2WriteLatencySeconds.Observe(seconds)
}

// IncEmbeddingCall increments the embeddingCallsTotal counter.
func (m *Metrics) IncEmbeddingCall(provider, model, outcome string) {
	if m == nil {
		return
	}
	m.embeddingCallsTotal.WithLabelValues(provider, model, outcome).Inc()
}

// ObserveEmbeddingLatency records leader embedding call latency.
func (m *Metrics) ObserveEmbeddingLatency(seconds float64) {
	if m == nil {
		return
	}
	m.embeddingLatencySeconds.Observe(seconds)
}

// AddEmbeddingCost accumulates cost in USD for a leader embedding call.
func (m *Metrics) AddEmbeddingCost(provider, model string, costUSD float64) {
	if m == nil {
		return
	}
	m.embeddingCostUSDTotal.WithLabelValues(provider, model).Add(costUSD)
}

// IncLookup increments the l2LookupsTotal counter for the given outcome label.
// Called on every Reader.Read invocation — one call per outcome.
func (m *Metrics) IncLookup(outcome string) {
	if m == nil {
		return
	}
	m.l2LookupsTotal.WithLabelValues(outcome).Inc()
}

// ObserveLookupSimilarity records a cosine similarity value in the histogram.
// Pass 0 when the lookup did not produce a similarity score (skip / miss).
func (m *Metrics) ObserveLookupSimilarity(sim float32) {
	if m == nil {
		return
	}
	m.l2SimilarityHistogram.Observe(float64(sim))
}

// ObserveLookupLatency records the end-to-end L2 read path latency in seconds.
func (m *Metrics) ObserveLookupLatency(seconds float64) {
	if m == nil {
		return
	}
	m.l2LookupLatencySeconds.Observe(seconds)
}

// IncLookupSimilarity is an alias for ObserveLookupSimilarity kept for
// backward-compat with call sites that used the old name.
func (m *Metrics) IncLookupSimilarity(sim float32) {
	m.ObserveLookupSimilarity(sim)
}

// IncFeedback increments the l2FeedbackTotal counter for the given admin-provided reason.
func (m *Metrics) IncFeedback(reason string) {
	if m == nil {
		return
	}
	m.l2FeedbackTotal.WithLabelValues(reason).Inc()
}

// IncPoisonHits increments the l2PoisonHitsTotal counter when Reader.Read rejects a poisoned candidate.
func (m *Metrics) IncPoisonHits() {
	if m == nil {
		return
	}
	m.l2PoisonHitsTotal.Inc()
}

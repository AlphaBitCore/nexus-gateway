// Package streamcache — metrics.go owns the Prometheus instruments for
// the SSE cache + broker subsystem.
//
// A nil *Metrics is safe: every method is a no-op on a nil receiver so
// callers do not need nil-guards.
package streamcache

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Metrics holds the six Prometheus instruments for the SSE cache +
// broker subsystem. Construct with NewMetrics; use the helper methods
// for thread-safe, nil-safe recording.
type Metrics struct {
	// LookupsTotal counts cache lookups by outcome.
	// Label: result ∈ {hit, hit_live, miss, skip_no_cache, disabled}
	LookupsTotal *prometheus.CounterVec

	// WritesTotal counts cache writes by entry kind and write outcome.
	// Labels: kind ∈ {stream, response}, reason ∈ {ok, too_large, encode_error}
	WritesTotal *prometheus.CounterVec

	// BrokerSubscribers is the current sum of subscribers across all
	// in-flight brokers. Increments on subscribe, decrements on Close.
	BrokerSubscribers prometheus.Gauge

	// BrokerActive is the number of in-flight brokers. Increments when a
	// new broker is created; decrements when its pump exits.
	BrokerActive prometheus.Gauge

	// ReplayChunks counts chunks replayed from a Redis cache HIT. One
	// increment per non-EOF Next() call on a replaySub.
	ReplayChunks prometheus.Counter

	// EntryBytes is a histogram of persisted cache-entry sizes in bytes.
	// Observed on successful writes (reason == "ok") only.
	EntryBytes prometheus.Histogram
}

// NewMetrics constructs the metric set under the nexus_aigw_cache
// namespace/subsystem. reg may be nil to use prometheus.DefaultRegisterer.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	if reg == nil {
		reg = prometheus.DefaultRegisterer
	}
	f := promauto.With(reg)
	return &Metrics{
		LookupsTotal: f.NewCounterVec(prometheus.CounterOpts{
			Namespace: "nexus_aigw",
			Subsystem: "cache",
			Name:      "lookups_total",
			Help:      "Cache lookups by outcome (hit, hit_live, miss, skip_no_cache, disabled).",
		}, []string{"result"}),
		WritesTotal: f.NewCounterVec(prometheus.CounterOpts{
			Namespace: "nexus_aigw",
			Subsystem: "cache",
			Name:      "writes_total",
			Help:      "Cache writes by entry kind (stream, response) and outcome (ok, too_large, encode_error).",
		}, []string{"kind", "reason"}),
		BrokerSubscribers: f.NewGauge(prometheus.GaugeOpts{
			Namespace: "nexus_aigw",
			Subsystem: "cache",
			Name:      "broker_subscribers",
			Help:      "Current sum of subscribers across all in-flight brokers.",
		}),
		BrokerActive: f.NewGauge(prometheus.GaugeOpts{
			Namespace: "nexus_aigw",
			Subsystem: "cache",
			Name:      "broker_active",
			Help:      "Current number of in-flight brokers.",
		}),
		ReplayChunks: f.NewCounter(prometheus.CounterOpts{
			Namespace: "nexus_aigw",
			Subsystem: "cache",
			Name:      "replay_chunks_total",
			Help:      "Chunks replayed from a Redis cache HIT.",
		}),
		EntryBytes: f.NewHistogram(prometheus.HistogramOpts{
			Namespace: "nexus_aigw",
			Subsystem: "cache",
			Name:      "entry_bytes",
			Help:      "Persisted cache-entry size in bytes (observed on successful writes only).",
			Buckets:   prometheus.ExponentialBuckets(1024, 2, 12), // 1KiB → 4MiB
		}),
	}
}

// RecordLookup increments the lookups counter for the given result label.
// result must be one of: hit, hit_live, miss, skip_no_cache, disabled.
func (m *Metrics) RecordLookup(result string) {
	if m == nil {
		return
	}
	m.LookupsTotal.WithLabelValues(result).Inc()
}

// RecordWrite increments the writes counter and, on a successful write
// (reason == "ok"), observes the serialised size in the EntryBytes
// histogram. kind must be "stream" or "response"; reason must be one
// of "ok", "too_large", "encode_error".
func (m *Metrics) RecordWrite(kind, reason string, bytes int) {
	if m == nil {
		return
	}
	m.WritesTotal.WithLabelValues(kind, reason).Inc()
	if reason == "ok" && bytes > 0 {
		m.EntryBytes.Observe(float64(bytes))
	}
}

// IncBrokerActive increments the broker-active gauge.
func (m *Metrics) IncBrokerActive() {
	if m != nil {
		m.BrokerActive.Inc()
	}
}

// DecBrokerActive decrements the broker-active gauge.
func (m *Metrics) DecBrokerActive() {
	if m != nil {
		m.BrokerActive.Dec()
	}
}

// IncSubscribers increments the broker-subscribers gauge.
func (m *Metrics) IncSubscribers() {
	if m != nil {
		m.BrokerSubscribers.Inc()
	}
}

// DecSubscribers decrements the broker-subscribers gauge.
func (m *Metrics) DecSubscribers() {
	if m != nil {
		m.BrokerSubscribers.Dec()
	}
}

// IncReplayChunks increments the replay-chunks counter by one.
func (m *Metrics) IncReplayChunks() {
	if m != nil {
		m.ReplayChunks.Inc()
	}
}

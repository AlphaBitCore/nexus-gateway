package credstats

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Metrics owns the Buffer's Prometheus collectors. Names follow the
// nexus_ai_gateway namespace convention. All collectors are no-op
// callable when Metrics is nil so the package stays usable in tests
// without a registry.
type Metrics struct {
	attemptsTotal      *prometheus.CounterVec
	circuitTransitions *prometheus.CounterVec
	authFailIncrements prometheus.Counter
	redisWriteFailures *prometheus.CounterVec
	redisWriteLatencyS prometheus.Histogram
}

// NewMetrics registers the Buffer's collectors on reg. Pass nil to
// disable metrics collection — callers without a registry (tests,
// short-lived tools) can still construct a fully-functional Buffer.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	if reg == nil {
		return nil
	}
	f := promauto.With(reg)
	return &Metrics{
		attemptsTotal: f.NewCounterVec(prometheus.CounterOpts{
			Namespace: "nexus",
			Subsystem: "credstats",
			Name:      "attempts_total",
			Help:      "Number of upstream attempts recorded by the credential stats buffer, labelled by HTTP class.",
		}, []string{"class"}),
		circuitTransitions: f.NewCounterVec(prometheus.CounterOpts{
			Namespace: "nexus",
			Subsystem: "credstats",
			Name:      "circuit_transitions_total",
			Help:      "Circuit-breaker state transitions observed by the buffer.",
		}, []string{"to", "reason"}),
		authFailIncrements: f.NewCounter(prometheus.CounterOpts{
			Namespace: "nexus",
			Subsystem: "credstats",
			Name:      "auth_fail_increments_total",
			Help:      "Number of 401/403 responses that incremented the live auth_fails counter without crossing the open threshold.",
		}),
		redisWriteFailures: f.NewCounterVec(prometheus.CounterOpts{
			Namespace: "nexus",
			Subsystem: "credstats",
			Name:      "redis_write_failures_total",
			Help:      "Buffer Redis writes that returned an error, labelled by stage.",
		}, []string{"stage"}),
		redisWriteLatencyS: f.NewHistogram(prometheus.HistogramOpts{
			Namespace: "nexus",
			Subsystem: "credstats",
			Name:      "redis_write_seconds",
			Help:      "End-to-end time spent in the Buffer's Redis pipelines.",
			Buckets:   []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.2, 0.5},
		}),
	}
}

func (m *Metrics) incAttempt(class string) {
	if m != nil {
		m.attemptsTotal.WithLabelValues(class).Inc()
	}
}

func (m *Metrics) incCircuit(to, reason string) {
	if m != nil {
		m.circuitTransitions.WithLabelValues(to, reason).Inc()
	}
}

func (m *Metrics) incAuthFailIncrement() {
	if m != nil {
		m.authFailIncrements.Inc()
	}
}

func (m *Metrics) incRedisFailure(stage string) {
	if m != nil {
		m.redisWriteFailures.WithLabelValues(stage).Inc()
	}
}

func (m *Metrics) observeWrite(d time.Duration) {
	if m != nil {
		m.redisWriteLatencyS.Observe(d.Seconds())
	}
}

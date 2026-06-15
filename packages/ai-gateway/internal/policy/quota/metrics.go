package quota

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Metrics holds the Prometheus instruments for the quota enforcement engine.
// A nil *Metrics is safe: every method is a no-op on a nil receiver, so an
// Engine constructed without metrics (e.g. a sibling-package test) still runs.
//
// These counters make the three otherwise-invisible quota behaviours
// observable to monitoring:
//   - decisionTotal: which action each Check resolved to (allow / downgrade /
//     reject / notify-and-proceed / track-only) — lets ops see how often the
//     gateway is throttling traffic.
//   - checkFailOpenTotal: how often Check fell through a level because the
//     usage-cache (Redis) read failed — i.e. unmetered, un-throttled spend
//     during a cache outage.
//   - reconcileFailedTotal: how often a post-success Reconcile failed to
//     advance the usage counter — that increment is permanently lost until the
//     next boot Backfill, so an alert on this counter is the only timely
//     signal of counter drift.
type Metrics struct {
	decisionTotal        *prometheus.CounterVec
	checkFailOpenTotal   *prometheus.CounterVec
	reconcileFailedTotal prometheus.Counter
}

// NewMetrics registers the quota metrics against the given registerer under the
// supplied namespace. Pass prometheus.DefaultRegisterer for production wiring;
// pass a fresh prometheus.NewRegistry() in tests so each Engine gets isolated
// counters and repeated construction never panics on duplicate registration.
func NewMetrics(namespace string, reg prometheus.Registerer) *Metrics {
	factory := promauto.With(reg)
	return &Metrics{
		decisionTotal: factory.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "quota_decision_total",
			Help:      "Total quota enforcement decisions partitioned by resolved action.",
		}, []string{"action"}),

		checkFailOpenTotal: factory.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "quota_check_failopen_total",
			Help:      "Total quota pre-checks that failed open (skipped a level) partitioned by reason.",
		}, []string{"reason"}),

		reconcileFailedTotal: factory.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "quota_reconcile_failed_total",
			Help:      "Total post-success quota reconcile increments that failed to persist to the usage cache.",
		}),
	}
}

// observeDecision records the resolved action of one Check call.
func (m *Metrics) observeDecision(action string) {
	if m == nil {
		return
	}
	m.decisionTotal.WithLabelValues(action).Inc()
}

// observeCheckFailOpen records a single fail-open level skip during Check.
func (m *Metrics) observeCheckFailOpen(reason string) {
	if m == nil {
		return
	}
	m.checkFailOpenTotal.WithLabelValues(reason).Inc()
}

// observeReconcileFailed records a single failed Reconcile increment.
func (m *Metrics) observeReconcileFailed() {
	if m == nil {
		return
	}
	m.reconcileFailedTotal.Inc()
}

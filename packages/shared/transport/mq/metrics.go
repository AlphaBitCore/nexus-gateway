package mq

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Metrics holds Prometheus counters for MQ operations.
// Create one per service via GetOrCreateMetrics and pass to producer/consumer constructors.
type Metrics struct {
	PublishedTotal prometheus.Counter
	EnqueuedTotal  prometheus.Counter
	ConsumedTotal  prometheus.Counter
	AckedTotal     prometheus.Counter
	NakedTotal     prometheus.Counter
	DeferredTotal  prometheus.Counter
	ErrorsTotal    prometheus.Counter
}

var (
	metricsCacheMu sync.RWMutex
	metricsCache   = map[string]*Metrics{}
)

// GetOrCreateMetrics returns the Metrics for namespace, creating and
// registering them on first call. Subsequent calls with the same namespace
// return the cached instance without re-registering (avoids duplicate
// Prometheus registration panics when both Producer and Consumer are created
// in the same process under the same namespace).
func GetOrCreateMetrics(namespace string) *Metrics {
	metricsCacheMu.RLock()
	if m, ok := metricsCache[namespace]; ok {
		metricsCacheMu.RUnlock()
		return m
	}
	metricsCacheMu.RUnlock()

	metricsCacheMu.Lock()
	defer metricsCacheMu.Unlock()
	// Double-check after acquiring write lock.
	if m, ok := metricsCache[namespace]; ok {
		return m
	}
	m := newMetrics(namespace)
	metricsCache[namespace] = m
	return m
}

// NewMetrics registers a fresh set of MQ Prometheus metrics under namespace.
// Callers that create both a Producer and Consumer in the same process must
// use GetOrCreateMetrics instead to avoid duplicate registration panics.
func NewMetrics(namespace string) *Metrics {
	return newMetrics(namespace)
}

func newMetrics(namespace string) *Metrics {
	return &Metrics{
		PublishedTotal: promauto.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: "mq",
			Name:      "published_total",
			Help:      "Total messages published to topics.",
		}),
		EnqueuedTotal: promauto.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: "mq",
			Name:      "enqueued_total",
			Help:      "Total messages enqueued to queues.",
		}),
		ConsumedTotal: promauto.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: "mq",
			Name:      "consumed_total",
			Help:      "Total messages consumed from queues or topics.",
		}),
		AckedTotal: promauto.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: "mq",
			Name:      "acked_total",
			Help:      "Total messages acknowledged.",
		}),
		NakedTotal: promauto.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: "mq",
			Name:      "naked_total",
			Help:      "Total messages negatively acknowledged (requeued for redelivery).",
		}),
		DeferredTotal: promauto.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: "mq",
			Name:      "deferred_total",
			Help:      "Messages deferred to handler for manual ack/nak (ErrDeferAck).",
		}),
		ErrorsTotal: promauto.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: "mq",
			Name:      "errors_total",
			Help:      "Total MQ operation errors.",
		}),
	}
}

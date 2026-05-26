package thingclient

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

type clientMetrics struct {
	wsConnections    *prometheus.CounterVec
	wsConnected      prometheus.Gauge
	mode             *prometheus.GaugeVec
	configApplies    *prometheus.CounterVec
	shadowReports    *prometheus.CounterVec
	mqPublished      *prometheus.CounterVec
	mqBufferSize     prometheus.Gauge
	mqDropped        prometheus.Counter
	reconnectTotal   prometheus.Counter
	httpFallbackReqs *prometheus.CounterVec
	outboxDropped    *prometheus.CounterVec
}

func newClientMetrics(reg prometheus.Registerer, namespace string) *clientMetrics {
	factory := promauto.With(reg)

	return &clientMetrics{
		wsConnections: factory.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: "thingclient",
			Name:      "ws_connections_total",
			Help:      "Total WebSocket connection attempts by status.",
		}, []string{"status"}),

		wsConnected: factory.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: "thingclient",
			Name:      "ws_connected",
			Help:      "1 if WebSocket is connected, 0 otherwise.",
		}),

		mode: factory.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: "thingclient",
			Name:      "mode",
			Help:      "Current operating mode (ws_connected, http_fallback, disconnected).",
		}, []string{"mode"}),

		configApplies: factory.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: "thingclient",
			Name:      "config_applies_total",
			Help:      "Total config apply callback invocations by status.",
		}, []string{"status"}),

		shadowReports: factory.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: "thingclient",
			Name:      "shadow_reports_total",
			Help:      "Total shadow reports sent by status.",
		}, []string{"status"}),

		mqPublished: factory.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: "thingclient",
			Name:      "mq_published_total",
			Help:      "Total events published to MQ by queue.",
		}, []string{"queue"}),

		mqBufferSize: factory.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: "thingclient",
			Name:      "mq_buffer_size",
			Help:      "Current number of events in the local MQ buffer.",
		}),

		mqDropped: factory.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: "thingclient",
			Name:      "mq_dropped_total",
			Help:      "Total events dropped due to buffer overflow.",
		}),

		reconnectTotal: factory.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: "thingclient",
			Name:      "reconnect_total",
			Help:      "Total reconnection attempts.",
		}),

		httpFallbackReqs: factory.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: "thingclient",
			Name:      "http_fallback_requests_total",
			Help:      "Total HTTP fallback requests by endpoint.",
		}, []string{"endpoint"}),

		outboxDropped: factory.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: "thingclient",
			Name:      "outbox_dropped_total",
			Help:      "Outgoing messages dropped from the outbox by message type.",
		}, []string{"msg_type"}),
	}
}

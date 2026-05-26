package alerteval

import (
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/observability/consumer"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
)

// EventKind enumerates the MQ source category. Aggregators inspect Kind to
// decide whether to process or skip an event.
type EventKind string

const (
	EventTraffic EventKind = "traffic"
	EventAudit   EventKind = "audit"
)

// Event is the decoded payload an Aggregator's OnEvent receives. Traffic
// events use the Hub-side consumer.TrafficEventMessage shape (pointer types
// for nullable columns + SourceProcess + JSONB hooks_pipeline) which is
// what the existing Hub TrafficEventWriter consumes from the same MQ
// subjects. Audit events use the shared mq.AdminAuditMessage (CP publishes
// that exact shape).
type Event struct {
	Kind      EventKind
	Source    EventSource
	Timestamp time.Time

	Traffic *consumer.TrafficEventMessage
	Audit   *mq.AdminAuditMessage
}

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

// Event is the decoded payload an Aggregator's OnEvent receives. Traffic events
// use consumer.AlertView — the narrow, compiler-enforced projection of the
// producer message holding only the fields the aggregators read (pointer types
// for nullable columns + SourceProcess + JSONB hooks_pipeline). An aggregator
// that needs a field absent from AlertView fails to compile, which is the guard
// against the narrowed alert decode silently dropping a field a rule depends on.
// Audit events use the shared mq.AdminAuditMessage (CP publishes that exact shape).
type Event struct {
	Kind      EventKind
	Source    EventSource
	Timestamp time.Time

	Traffic *consumer.AlertView
	Audit   *mq.AdminAuditMessage
}

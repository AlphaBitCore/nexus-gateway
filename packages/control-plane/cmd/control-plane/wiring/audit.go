package wiring

import (
	"log/slog"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	cpmetrics "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/metrics"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
)

// InitAuditWriter creates the audit writer that publishes admin-audit events
// to the MQ topic "nexus.event.admin-audit". The failure observer bumps the
// admin.audit_log_failed_total{action} counter so on-call sees MQ gaps.
//
// mqProducer may be nil; the audit package handles nil gracefully (events are
// dropped and the failure observer is called).
func InitAuditWriter(mqProducer mq.Producer, logger *slog.Logger) *audit.Writer {
	return audit.NewWriter(mqProducer, "nexus.event.admin-audit", logger).
		WithFailureObserver(func(action string) {
			if cpmetrics.AdminAuditLogFailedTotal != nil {
				cpmetrics.AdminAuditLogFailedTotal.With(action).Inc()
			}
		})
}

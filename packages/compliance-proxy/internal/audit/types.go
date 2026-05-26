// Package audit holds compliance-proxy's audit pipeline producer (MQ writer
// + NDJSON fallback). The canonical event shape and Writer / QueueInspector
// interfaces live in packages/shared/audit so other data-plane services
// (agent today, ai-gateway later) can implement the same Writer contract
// against their own persistence backend.
//
// All database writes live in the Hub's db-writer; the proxy never inserts
// into traffic_event directly.
package audit

import (
	sharedaudit "github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
)

// AuditEvent is the canonical event shape producers populate. Re-exported
// from shared/audit so existing cp callers keep `audit.AuditEvent` working.
type AuditEvent = sharedaudit.AuditEvent

// Writer is the audit-event sink interface implemented by MQBatchWriter
// (and NDJSONWriter in fallback). Re-exported from shared/audit.
type Writer = sharedaudit.Writer

// QueueInspector is the queue-depth introspection interface implemented by
// MQBatchWriter for health/alerting checks. Re-exported from shared/audit.
type QueueInspector = sharedaudit.QueueInspector

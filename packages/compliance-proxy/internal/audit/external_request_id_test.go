package audit

import (
	"testing"

	sharedaudit "github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
)

// The caller's own x-request-id has to survive the compliance-proxy leg.
//
// Three ids reach traffic_event and they answer three questions: the row's id
// says which row, trace_id says which unit of work, and external_request_id is
// the caller's own — recorded as given so an external system can join Nexus
// rows to its own logs. Only the AI Gateway ever wrote the third. A caller
// whose traffic reached Nexus through the compliance proxy had the header
// sitting in the intercepted request and nothing read it, so the join they
// were promised did not exist on that path.
func TestToMessage_CarriesTheCallersOwnRequestID(t *testing.T) {
	m := toMessage(sharedaudit.AuditEvent{
		ID:                "evt-1",
		TraceID:           "trace-1",
		ExternalRequestID: "caller-own-id",
	}, "thing-1", "Thing One")

	if m.ExternalRequestID != "caller-own-id" {
		t.Errorf("ExternalRequestID = %q, want the caller's own id — nothing else carries it on this path", m.ExternalRequestID)
	}
	// The three stay distinct: carrying one into another's column is the
	// failure this contract exists to prevent.
	if m.TraceID != "trace-1" {
		t.Errorf("TraceID = %q, want trace-1", m.TraceID)
	}
	if m.ExternalRequestID == m.TraceID || m.ExternalRequestID == m.ID {
		t.Errorf("the caller's id collided with another: id=%q trace=%q external=%q", m.ID, m.TraceID, m.ExternalRequestID)
	}
}

// A caller that sent no id leaves the column empty rather than borrowing the
// trace, which would silently claim they had supplied one.
func TestToMessage_NoCallerIDLeavesTheColumnEmpty(t *testing.T) {
	m := toMessage(sharedaudit.AuditEvent{ID: "evt-2", TraceID: "trace-2"}, "thing-1", "Thing One")
	if m.ExternalRequestID != "" {
		t.Errorf("ExternalRequestID = %q, want empty", m.ExternalRequestID)
	}
}

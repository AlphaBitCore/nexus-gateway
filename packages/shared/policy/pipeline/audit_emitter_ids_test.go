package pipeline

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

// Three ids reach traffic_event and each answers a different question: the
// row's own id says WHICH ROW, trace_id says which unit of work, and
// external_request_id is the CALLER'S own — recorded as given so an external
// system can join Nexus rows to its own logs.
//
// The emitter is where the third one enters an audit event on the proxy and
// agent paths. Before this it entered nowhere: only the AI Gateway recorded it,
// so a caller whose traffic reached Nexus through tlsbump had the header
// sitting in the intercepted request with nothing reading it, and the join they
// were promised did not exist on that path.
func TestEmitDual_CarriesTheThreeIdsApart(t *testing.T) {
	w := &captureWriter{}
	e := NewAuditEmitter(w, testEmitterLogger())

	e.EmitDual(
		&core.HookInput{IngressType: "COMPLIANCE_PROXY", TargetHost: "api.openai.com", Path: "/v1/chat/completions", Method: "POST"},
		AuditInfo{
			TransactionID:     "txn-1",
			TraceID:           "trace-1",
			ExternalRequestID: "caller-own-id",
		},
		&core.CompliancePipelineResult{Decision: core.Approve, Action: core.ActionApprove}, nil,
		"BUMP_SUCCESS", 200, 5, nil, nil, traffic.UsageMeta{},
	)

	if got := w.count(); got != 1 {
		t.Fatalf("expected 1 event, got %d", got)
	}
	ev := w.events[0]
	if ev.ExternalRequestID != "caller-own-id" {
		t.Errorf("ExternalRequestID = %q, want the caller's own id — the header was read off the "+
			"intercepted request and dropped here, leaving every proxy row unjoinable", ev.ExternalRequestID)
	}
	if ev.TraceID != "trace-1" {
		t.Errorf("TraceID = %q, want trace-1", ev.TraceID)
	}
	if ev.ID == "" || ev.ID == ev.TraceID || ev.ID == ev.ExternalRequestID {
		t.Errorf("the row id must be its own value: id=%q trace=%q external=%q", ev.ID, ev.TraceID, ev.ExternalRequestID)
	}
}

// A caller that sent no id leaves the column empty rather than borrowing the
// trace, which would silently claim they had supplied one.
func TestEmitDual_NoCallerIDLeavesTheColumnEmpty(t *testing.T) {
	w := &captureWriter{}
	NewAuditEmitter(w, testEmitterLogger()).EmitDual(
		&core.HookInput{IngressType: "COMPLIANCE_PROXY", TargetHost: "h", Path: "/p", Method: "POST"},
		AuditInfo{TransactionID: "txn-2", TraceID: "trace-2"},
		&core.CompliancePipelineResult{Decision: core.Approve, Action: core.ActionApprove}, nil,
		"BUMP_SUCCESS", 200, 5, nil, nil, traffic.UsageMeta{},
	)
	if got := w.events[0].ExternalRequestID; got != "" {
		t.Errorf("ExternalRequestID = %q, want empty", got)
	}
}

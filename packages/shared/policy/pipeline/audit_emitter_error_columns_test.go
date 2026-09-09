package pipeline

import (
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

// buildEvent's error columns are covered here and nowhere else. A
// classifyComplianceError running before gateStorageBody reads the RAW
// response: under a redact policy those bytes are correctly kept out of the
// body column and land in error_reason beside it — the gate honoured for one
// column and bypassed for its neighbour. Every other test calls the
// classifier directly, so moving the call back before the gate leaves the
// whole suite green.
//
// These two tests are a pair on purpose. The first alone would pass against a
// classifier that always answered with the status line and never quoted
// anything; the second is what makes the first mean "the gate decided".
func emitWithStatus(t *testing.T, info AuditInfo, respResult *CompliancePipelineResult,
	status int, respBody []byte) audit.AuditEvent {
	t.Helper()
	w := &captureWriter{}
	e := NewAuditEmitter(w, testEmitterLogger())
	e.EmitDual(storageTestInput(), info, nil, respResult, "BUMP_SUCCESS", status, 12,
		nil, respBody, traffic.UsageMeta{})
	if w.count() != 1 {
		t.Fatalf("want 1 event, got %d", w.count())
	}
	return w.events[0]
}

func TestBuildEvent_ErrorReasonObeysTheStorageGate(t *testing.T) {
	// A provider 4xx that quotes the caller's input back — OpenAI and
	// Anthropic validation errors routinely do exactly this.
	quoted := "contact " + emailMarker + " now"
	raw := []byte(`{"error":{"message":"invalid input: ` + quoted + `"}}`)

	// Redact with NO rewritten wire copy, so the gate withholds the body
	// entirely rather than substituting a masked one.
	respResult := &CompliancePipelineResult{Decision: Modify, Action: core.ActionRedact}
	evt := emitWithStatus(t, AuditInfo{TransactionID: "tx-error-gated"}, respResult, 400, raw)

	// Precondition: without this the test proves nothing about the gate.
	if evt.ResponseBody.Kind != audit.BodyAbsent {
		t.Fatalf("the gate did not withhold the body, so error_reason has nothing to be "+
			"compared against: kind=%q bytes=%q", evt.ResponseBody.Kind, evt.ResponseBody.InlineBytes)
	}

	if strings.Contains(evt.ErrorReason, emailMarker) {
		t.Errorf("error_reason quotes the bytes the storage gate just withheld from the "+
			"column beside it: %q", evt.ErrorReason)
	}
	if evt.ErrorReason != "provider returned HTTP 400" {
		t.Errorf("with the body withheld there is nothing to quote, so the reason must "+
			"degrade to the status line; got %q", evt.ErrorReason)
	}
	if evt.ErrorCode != "PROVIDER_ERROR" {
		t.Errorf("error_code = %q, want PROVIDER_ERROR — the test is on the wrong arm", evt.ErrorCode)
	}
}

func TestBuildEvent_ErrorReasonStillQuotesAnApprovedBody(t *testing.T) {
	raw := []byte(`{"error":{"message":"model 'gpt-4o' does not exist"}}`)
	respResult := &CompliancePipelineResult{Decision: Approve, Action: core.ActionApprove}
	evt := emitWithStatus(t, AuditInfo{TransactionID: "tx-error-approved"}, respResult, 404, raw)

	if evt.ErrorReason != "model 'gpt-4o' does not exist" {
		t.Errorf("an approved body must still be quoted — degrading every provider error "+
			"to a status line would make the column useless; got %q", evt.ErrorReason)
	}
}

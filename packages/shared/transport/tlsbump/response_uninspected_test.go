package tlsbump

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/decision"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/pipeline"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

// The audit trail must not report a compliance verdict for a response no hook
// examined. Observed live on 2026-07-28: with every hook disabled, bumped
// traffic wrote response_hook_decision=APPROVE with response_hooks_pipeline
// NULL, indistinguishable from the same column on traffic a hook really did
// approve.
func TestUninspectedResponse_EmitsNoDecisionButKeepsBodyStored(t *testing.T) {
	w := &recordingAuditWriter{}
	e := pipeline.NewAuditEmitter(w, discardSlog())

	respBody := []byte(`{"choices":[{"message":{"content":"hello"}}]}`)
	e.EmitDual(
		&core.HookInput{IngressType: "COMPLIANCE_PROXY"},
		pipeline.AuditInfo{},
		nil, // no request hook ran either
		uninspectedResponse(),
		"BUMP_SUCCESS", 200, 10, nil, respBody, traffic.UsageMeta{},
	)

	events := w.snapshot()
	if len(events) != 1 {
		t.Fatalf("expected exactly one audit event, got %d", len(events))
	}
	ev := events[0]

	// The claim under test: no hook ran, so the column carries nothing. A
	// consumer filtering for APPROVE must not find this row.
	if ev.ResponseHookDecision != nil {
		t.Errorf("uninspected response reported decision %q — a verdict for traffic nothing looked at",
			*ev.ResponseHookDecision)
	}

	// The other half, and the reason the result is not simply nil: the emitter's
	// storage gate reads the stage ACTION to decide whether the captured body may
	// be persisted verbatim. Telling the truth in the decision column must not
	// silently stop bodies being stored on every deployment that runs no response
	// hooks.
	if ev.ResponseBody.Kind != audit.BodyInline {
		t.Errorf("response body kind = %s, want inline — the honesty fix cost body capture",
			ev.ResponseBody.Kind)
	}
}

// The control. Without it the test above passes on a build where the emitter
// never reports a response decision at all, which would be a different bug
// wearing the same green.
func TestGenuineResponseApproval_StillReportsApprove(t *testing.T) {
	w := &recordingAuditWriter{}
	e := pipeline.NewAuditEmitter(w, discardSlog())

	e.EmitDual(
		&core.HookInput{IngressType: "COMPLIANCE_PROXY"},
		pipeline.AuditInfo{},
		nil,
		&core.CompliancePipelineResult{Decision: core.Approve},
		"BUMP_SUCCESS", 200, 10, nil, []byte(`{"ok":true}`), traffic.UsageMeta{},
	)

	events := w.snapshot()
	if len(events) != 1 {
		t.Fatalf("expected exactly one audit event, got %d", len(events))
	}
	if events[0].ResponseHookDecision == nil {
		t.Fatal("a response the pipeline approved reported no decision — the fix went too far")
	}
	if got := *events[0].ResponseHookDecision; got != string(core.Approve) {
		t.Errorf("approved response reported %q, want %q", got, core.Approve)
	}
}

// uninspectedResponse carries an explicit approve ACTION even though it carries
// no decision; the two fields answer different questions and the storage gate
// reads the second one.
func TestUninspectedResponse_CarriesApproveActionWithoutDecision(t *testing.T) {
	r := uninspectedResponse()
	if r.Decision != "" {
		t.Errorf("decision = %q, want empty so the audit column persists NULL", r.Decision)
	}
	if r.Action != decision.ActionApprove {
		t.Errorf("action = %q, want %q so the body-storage gate is unchanged", r.Action, decision.ActionApprove)
	}
}

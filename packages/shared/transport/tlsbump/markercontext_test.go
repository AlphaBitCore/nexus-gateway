package tlsbump

import (
	"context"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

// CPMarker context round-trip

func TestCPMarkerContext_Roundtrip(t *testing.T) {
	m := &CPMarker{
		RequestID:    "req-1",
		DomainRuleID: "rule-uuid",
		HookOutcome:  traffic.HookOutcomeInput{Passed: []string{"pii-redact"}},
	}
	ctx := contextWithCPMarker(context.Background(), m)
	got := CPMarkerFromContext(ctx)
	if got == nil {
		t.Fatal("expected non-nil marker from context")
		return
	}
	if got.RequestID != "req-1" {
		t.Errorf("RequestID: want %q got %q", "req-1", got.RequestID)
	}
	if got.DomainRuleID != "rule-uuid" {
		t.Errorf("DomainRuleID: want %q got %q", "rule-uuid", got.DomainRuleID)
	}
	if len(got.HookOutcome.Passed) != 1 || got.HookOutcome.Passed[0] != "pii-redact" {
		t.Errorf("HookOutcome.Passed: want [pii-redact] got %v", got.HookOutcome.Passed)
	}
}

func TestCPMarkerContext_NilWhenAbsent(t *testing.T) {
	if got := CPMarkerFromContext(context.Background()); got != nil {
		t.Errorf("expected nil from empty context, got %+v", got)
	}
}

// cpHookOutcomeFromResult — mirrors AI Gateway's test cases (5 cases)

func TestCPHookOutcomeFromResult_Nil(t *testing.T) {
	got := cpHookOutcomeFromResult(nil)
	if got.Rejected != "" || got.Transformed || len(got.Passed) != 0 {
		t.Errorf("nil input: expected zero HookOutcomeInput, got %+v", got)
	}
}

func TestCPHookOutcomeFromResult_Empty(t *testing.T) {
	got := cpHookOutcomeFromResult(&core.CompliancePipelineResult{})
	if got.Rejected != "" || got.Transformed || len(got.Passed) != 0 {
		t.Errorf("empty result: expected zero HookOutcomeInput, got %+v", got)
	}
}

func TestCPHookOutcomeFromResult_TwoPassed(t *testing.T) {
	r := &core.CompliancePipelineResult{
		Decision: core.Approve,
		HookResults: []core.HookResult{
			{HookName: "pii-redact", Decision: core.Approve},
			{HookName: "jwt-strip", Decision: core.Abstain},
		},
	}
	got := cpHookOutcomeFromResult(r)
	if got.Rejected != "" {
		t.Errorf("expected no reject, got Rejected=%q", got.Rejected)
	}
	if got.Transformed {
		t.Errorf("expected Transformed=false, got true")
	}
	if len(got.Passed) != 2 || got.Passed[0] != "pii-redact" || got.Passed[1] != "jwt-strip" {
		t.Errorf("Passed: want [pii-redact jwt-strip] got %v", got.Passed)
	}
}

func TestCPHookOutcomeFromResult_Transformed(t *testing.T) {
	r := &core.CompliancePipelineResult{
		Decision: core.Approve,
		HookResults: []core.HookResult{
			{HookName: "pii-redact", Decision: core.Modify},
		},
	}
	got := cpHookOutcomeFromResult(r)
	if !got.Transformed {
		t.Errorf("expected Transformed=true")
	}
	if len(got.Passed) != 1 || got.Passed[0] != "pii-redact" {
		t.Errorf("Passed: want [pii-redact] got %v", got.Passed)
	}
	if got.Rejected != "" {
		t.Errorf("expected no reject")
	}
}

func TestCPHookOutcomeFromResult_RejectHaltsIteration(t *testing.T) {
	// A RejectHard before another hook: only the rejecting hook is reported;
	// earlier accumulated Passed hooks are discarded (spec §4.5).
	r := &core.CompliancePipelineResult{
		Decision: core.RejectHard,
		HookResults: []core.HookResult{
			{HookName: "pii-redact", Decision: core.Approve},
			{HookName: "prompt-injection", Decision: core.RejectHard, ReasonCode: "sql-fragment"},
			{HookName: "keyword-filter", Decision: core.Approve}, // must NOT appear
		},
	}
	got := cpHookOutcomeFromResult(r)
	if got.Rejected != "prompt-injection" {
		t.Errorf("Rejected: want %q got %q", "prompt-injection", got.Rejected)
	}
	if got.RejectReason != "sql-fragment" {
		t.Errorf("RejectReason: want %q got %q", "sql-fragment", got.RejectReason)
	}
	// Passed must be empty because the reject path returns a fresh HookOutcomeInput.
	if len(got.Passed) != 0 {
		t.Errorf("Passed should be empty after reject halt, got %v", got.Passed)
	}
}

func TestCPHookOutcomeFromResult_RejectFallsBackToReason(t *testing.T) {
	// When ReasonCode is empty the Reason field is used instead.
	r := &core.CompliancePipelineResult{
		Decision: core.BlockSoft,
		HookResults: []core.HookResult{
			{HookName: "pii-detector", Decision: core.BlockSoft, Reason: "Contains SSN"},
		},
	}
	got := cpHookOutcomeFromResult(r)
	if got.Rejected != "pii-detector" {
		t.Errorf("Rejected: want %q got %q", "pii-detector", got.Rejected)
	}
	if got.RejectReason != "Contains SSN" {
		t.Errorf("RejectReason: want %q got %q", "Contains SSN", got.RejectReason)
	}
}

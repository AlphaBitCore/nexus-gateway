package pipeline

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
)

// stubHook is a test hook with configurable behavior.
type stubHook struct {
	core.AnyEndpointAnyModality
	decision   core.Decision
	reason     string
	reasonCode string
	delay      time.Duration
	err        error
	executed   atomic.Int32
}

func (h *stubHook) Execute(ctx context.Context, _ *core.HookInput) (*core.HookResult, error) {
	h.executed.Add(1)
	if h.delay > 0 {
		select {
		case <-time.After(h.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if h.err != nil {
		return nil, h.err
	}
	return &core.HookResult{
		Decision:   h.decision,
		Reason:     h.reason,
		ReasonCode: h.reasonCode,
	}, nil
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

func TestPipeline_AllApprove(t *testing.T) {
	hks := []boundHook{
		{hook: &stubHook{decision: core.Approve, reason: "ok1"}, config: &core.HookConfig{ID: "h1", Name: "hook1", Priority: 1, FailBehavior: "fail-open"}},
		{hook: &stubHook{decision: core.Approve, reason: "ok2"}, config: &core.HookConfig{ID: "h2", Name: "hook2", Priority: 2, FailBehavior: "fail-open"}},
		{hook: &stubHook{decision: core.Approve, reason: "ok3"}, config: &core.HookConfig{ID: "h3", Name: "hook3", Priority: 3, FailBehavior: "fail-open"}},
	}

	p := NewPipeline(hks, 5*time.Second, 30*time.Second, false, testLogger())
	result := p.Execute(context.Background(), &core.HookInput{})

	if result.Decision != core.Approve {
		t.Fatalf("expected APPROVE, got %s", result.Decision)
	}
	if len(result.HookResults) != 3 {
		t.Fatalf("expected 3 hook results, got %d", len(result.HookResults))
	}
}

func TestPipeline_RejectHardShortCircuits(t *testing.T) {
	hook3 := &stubHook{decision: core.Approve, reason: "ok3"}
	hks := []boundHook{
		{hook: &stubHook{decision: core.Approve, reason: "ok1"}, config: &core.HookConfig{ID: "h1", Name: "hook1", Priority: 1, FailBehavior: "fail-open"}},
		{hook: &stubHook{decision: core.RejectHard, reason: "blocked"}, config: &core.HookConfig{ID: "h2", Name: "hook2", Priority: 2, FailBehavior: "fail-open"}},
		{hook: hook3, config: &core.HookConfig{ID: "h3", Name: "hook3", Priority: 3, FailBehavior: "fail-open"}},
	}

	p := NewPipeline(hks, 5*time.Second, 30*time.Second, false, testLogger())
	result := p.Execute(context.Background(), &core.HookInput{})

	if result.Decision != core.RejectHard {
		t.Fatalf("expected REJECT_HARD, got %s", result.Decision)
	}
	if result.Reason != "blocked" {
		t.Fatalf("expected reason 'blocked', got %q", result.Reason)
	}
	// Hook3 should not have been executed due to short-circuit.
	if hook3.executed.Load() != 0 {
		t.Fatal("hook3 should not have been executed after REJECT_HARD short-circuit")
	}
	if len(result.HookResults) != 2 {
		t.Fatalf("expected 2 hook results (short-circuit), got %d", len(result.HookResults))
	}
}

func TestPipeline_SoftReject(t *testing.T) {
	hks := []boundHook{
		{hook: &stubHook{decision: core.Approve}, config: &core.HookConfig{ID: "h1", Name: "hook1", Priority: 1, FailBehavior: "fail-open"}},
		{hook: &stubHook{decision: core.BlockSoft, reason: "soft block"}, config: &core.HookConfig{ID: "h2", Name: "hook2", Priority: 2, FailBehavior: "fail-open"}},
		{hook: &stubHook{decision: core.Approve}, config: &core.HookConfig{ID: "h3", Name: "hook3", Priority: 3, FailBehavior: "fail-open"}},
	}

	p := NewPipeline(hks, 5*time.Second, 30*time.Second, false, testLogger())
	result := p.Execute(context.Background(), &core.HookInput{})

	if result.Decision != core.BlockSoft {
		t.Fatalf("expected BLOCK_SOFT, got %s", result.Decision)
	}
	if result.Reason != "soft block" {
		t.Fatalf("expected reason 'soft block', got %q", result.Reason)
	}
	// All hooks should have been executed (no short-circuit on soft reject).
	if len(result.HookResults) != 3 {
		t.Fatalf("expected 3 hook results, got %d", len(result.HookResults))
	}
}

func TestPipeline_ParallelExecution(t *testing.T) {
	delay := 50 * time.Millisecond
	hks := []boundHook{
		{hook: &stubHook{decision: core.Approve, delay: delay}, config: &core.HookConfig{ID: "h1", Name: "hook1", Priority: 1, FailBehavior: "fail-open"}},
		{hook: &stubHook{decision: core.Approve, delay: delay}, config: &core.HookConfig{ID: "h2", Name: "hook2", Priority: 2, FailBehavior: "fail-open"}},
		{hook: &stubHook{decision: core.Approve, delay: delay}, config: &core.HookConfig{ID: "h3", Name: "hook3", Priority: 3, FailBehavior: "fail-open"}},
	}

	p := NewPipeline(hks, 5*time.Second, 30*time.Second, true, testLogger())

	start := time.Now()
	result := p.Execute(context.Background(), &core.HookInput{})
	elapsed := time.Since(start)

	if result.Decision != core.Approve {
		t.Fatalf("expected APPROVE, got %s", result.Decision)
	}

	// If run in parallel, total time should be roughly 1x delay, not 3x.
	// Allow generous margin for CI slowness but must be less than 3x.
	maxAllowed := 3 * delay
	if elapsed >= maxAllowed {
		t.Fatalf("parallel execution took %v, expected less than %v (sequential would be ~%v)",
			elapsed, maxAllowed, 3*delay)
	}
}

func TestPipeline_PerHookTimeout(t *testing.T) {
	hks := []boundHook{
		{hook: &stubHook{decision: core.Approve, delay: 5 * time.Second}, config: &core.HookConfig{
			ID: "h1", Name: "slow-hook", Priority: 1,
			FailBehavior: "fail-open", TimeoutMs: 50,
		}},
	}

	p := NewPipeline(hks, 100*time.Millisecond, 30*time.Second, false, testLogger())

	start := time.Now()
	result := p.Execute(context.Background(), &core.HookInput{})
	elapsed := time.Since(start)

	// Should complete quickly due to timeout, not wait 5 seconds.
	if elapsed > 1*time.Second {
		t.Fatalf("expected quick timeout, took %v", elapsed)
	}

	// fail-open: should approve despite timeout error.
	if result.Decision != core.Approve {
		t.Fatalf("expected APPROVE (fail-open on timeout), got %s", result.Decision)
	}
	if len(result.HookResults) != 1 {
		t.Fatalf("expected 1 hook result, got %d", len(result.HookResults))
	}
	if result.HookResults[0].Error == "" {
		t.Fatal("expected non-empty error on timed-out hook")
	}
}

func TestPipeline_FailOpen(t *testing.T) {
	hks := []boundHook{
		{hook: &stubHook{err: errors.New("database unavailable")}, config: &core.HookConfig{
			ID: "h1", Name: "erroring-hook", Priority: 1, FailBehavior: "fail-open",
		}},
	}

	p := NewPipeline(hks, 5*time.Second, 30*time.Second, false, testLogger())
	result := p.Execute(context.Background(), &core.HookInput{})

	if result.Decision != core.Approve {
		t.Fatalf("expected APPROVE (fail-open), got %s", result.Decision)
	}
	if result.HookResults[0].Error == "" {
		t.Fatal("expected error to be recorded")
	}
	if result.HookResults[0].ReasonCode != "HOOK_ERROR_FAIL_OPEN" {
		t.Fatalf("expected reason code HOOK_ERROR_FAIL_OPEN, got %q", result.HookResults[0].ReasonCode)
	}
}

func TestPipeline_FailClosed(t *testing.T) {
	hks := []boundHook{
		{hook: &stubHook{err: errors.New("service down")}, config: &core.HookConfig{
			ID: "h1", Name: "erroring-hook", Priority: 1, FailBehavior: "fail-closed",
		}},
	}

	p := NewPipeline(hks, 5*time.Second, 30*time.Second, false, testLogger())
	result := p.Execute(context.Background(), &core.HookInput{})

	if result.Decision != core.RejectHard {
		t.Fatalf("expected REJECT_HARD (fail-closed), got %s", result.Decision)
	}
	if result.HookResults[0].Error == "" {
		t.Fatal("expected error to be recorded")
	}
	if result.HookResults[0].ReasonCode != "HOOK_ERROR_FAIL_CLOSED" {
		t.Fatalf("expected reason code HOOK_ERROR_FAIL_CLOSED, got %q", result.HookResults[0].ReasonCode)
	}
}

// MODIFY no longer downgrades to REJECT_HARD when allowModify
// is false. The pipeline always preserves the MODIFY decision; the
// downstream caller (data-plane service) decides via TrafficAdapter
// whether inflight rewrite is possible, mapping ErrRewriteUnsupported
// to ReasonRedactInflightUnsupported instead. This test asserts the new
// contract.
func TestPipeline_ModifyPreservedWithoutAllowModify(t *testing.T) {
	hks := []boundHook{
		{hook: &stubHook{decision: core.Modify, reason: "want to modify"}, config: &core.HookConfig{
			ID: "h1", Name: "modify-hook", Priority: 1, FailBehavior: "fail-open",
		}},
	}

	p := NewPipeline(hks, 5*time.Second, 30*time.Second, false, testLogger())
	result := p.Execute(context.Background(), &core.HookInput{})

	if result.Decision != core.Modify {
		t.Fatalf("expected MODIFY preserved, got %s", result.Decision)
	}
	if result.HookResults[0].Decision != core.Modify {
		t.Fatalf("expected hook result decision MODIFY, got %s", result.HookResults[0].Decision)
	}
}

func TestPipeline_AllowModify_Preserved(t *testing.T) {
	hks := []boundHook{
		{hook: &stubHook{decision: core.Modify, reason: "rewrite body"}, config: &core.HookConfig{
			ID: "h1", Name: "modify-hook", Priority: 1, FailBehavior: "fail-open",
		}},
	}

	p := NewPipeline(hks, 5*time.Second, 30*time.Second, false, testLogger())
	p.SetAllowModify(true)
	result := p.Execute(context.Background(), &core.HookInput{})

	// With allowModify, the MODIFY decision should pass through to merge.
	// mergeResults promotes MODIFY to the pipeline-level decision.
	if result.Decision != core.Modify {
		t.Fatalf("expected pipeline decision MODIFY, got %s", result.Decision)
	}
	if result.HookResults[0].Decision != core.Modify {
		t.Fatalf("expected hook result decision MODIFY (preserved), got %s", result.HookResults[0].Decision)
	}
	// Hook supplied no ReasonCode → fall back to the generic default.
	if result.ReasonCode != "CONTENT_MODIFIED" {
		t.Fatalf("expected reason code CONTENT_MODIFIED, got %q", result.ReasonCode)
	}
}

// TestPipeline_Modify_PreservesHookReasonCode pins the Q5 fix: a Modify
// hook that supplied its own ReasonCode (e.g. ReasonAIGuardSuggestedVsPolicy
// stamped at the webhook-forward reconcile) propagates through
// mergeResults' Modify branch instead of being clobbered by the generic
// "CONTENT_MODIFIED" default. Without this, the UI chip + i18n that
// consume request_hook_reason_code never light up for the redact-ceiling
// reconcile path.
func TestPipeline_Modify_PreservesHookReasonCode(t *testing.T) {
	hks := []boundHook{
		{hook: &stubHook{
			decision:   core.Modify,
			reason:     "webhook suggested approve; policy ceiling: redact",
			reasonCode: core.ReasonAIGuardSuggestedVsPolicy,
		}, config: &core.HookConfig{
			ID: "h1", Name: "ai-guard-webhook", Priority: 1, FailBehavior: "fail-open",
		}},
	}

	p := NewPipeline(hks, 5*time.Second, 30*time.Second, false, testLogger())
	p.SetAllowModify(true)
	result := p.Execute(context.Background(), &core.HookInput{})

	if result.Decision != core.Modify {
		t.Fatalf("expected pipeline decision MODIFY, got %s", result.Decision)
	}
	if result.ReasonCode != core.ReasonAIGuardSuggestedVsPolicy {
		t.Errorf("expected ReasonCode %q (hook-supplied), got %q (Modify branch should not clobber)",
			core.ReasonAIGuardSuggestedVsPolicy, result.ReasonCode)
	}
	if result.Reason != "webhook suggested approve; policy ceiling: redact" {
		t.Errorf("expected hook-supplied Reason, got %q", result.Reason)
	}
}

// TestPipeline_Modify_FirstHookByPriorityWinsInParallelMode pins the
// fix for parallel non-determinism: when two Modify hooks run in
// parallel, mergeResults sorts results by Order before applying the
// "first wins" rule — so the priority-first hook wins regardless of
// which goroutine finished first. Without the sort, the slower hook
// could land at results[0] and silently steal the tie-break.
func TestPipeline_Modify_FirstHookByPriorityWinsInParallelMode(t *testing.T) {
	hks := []boundHook{
		// Priority 1 hook is slow — completes second in the goroutine race.
		{hook: &stubHook{
			decision:   core.Modify,
			reason:     "priority-1 reason",
			reasonCode: "PRIORITY_1_CODE",
			delay:      50 * time.Millisecond,
		}, config: &core.HookConfig{
			ID: "h1", Name: "modify-priority-1", Priority: 1, FailBehavior: "fail-open",
		}},
		// Priority 2 hook is fast — completes first but should NOT win the tie.
		{hook: &stubHook{
			decision:   core.Modify,
			reason:     "priority-2 reason",
			reasonCode: "PRIORITY_2_CODE",
		}, config: &core.HookConfig{
			ID: "h2", Name: "modify-priority-2", Priority: 2, FailBehavior: "fail-open",
		}},
	}

	p := NewPipeline(hks, 5*time.Second, 30*time.Second, true, testLogger()) // parallel = true
	p.SetAllowModify(true)
	result := p.Execute(context.Background(), &core.HookInput{})

	if result.ReasonCode != "PRIORITY_1_CODE" {
		t.Errorf("first-by-priority Modify ReasonCode should win in parallel; got %q (slowest-completes-last bug)",
			result.ReasonCode)
	}
	if result.Reason != "priority-1 reason" {
		t.Errorf("first-by-priority Modify Reason should win; got %q", result.Reason)
	}
}

// TestPipeline_Reconcile_AIGuardReasonCodeSurvivesAlongsideApproveHook
// exercises the integration: a Modify hook (simulating a webhook-forward
// reconcile) stamps ReasonAIGuardSuggestedVsPolicy; a sibling Approve
// hook runs in the same pipeline. The reconcile reason code must reach
// CompliancePipelineResult.ReasonCode so the audit row + UI chip can
// surface the override.
func TestPipeline_Reconcile_AIGuardReasonCodeSurvivesAlongsideApproveHook(t *testing.T) {
	hks := []boundHook{
		{hook: &stubHook{
			decision:   core.Modify,
			reason:     "webhook suggested approve; policy ceiling: redact",
			reasonCode: core.ReasonAIGuardSuggestedVsPolicy,
		}, config: &core.HookConfig{
			ID: "h1", Name: "ai-guard-webhook", Priority: 1, FailBehavior: "fail-open",
		}},
		{hook: &stubHook{decision: core.Approve, reason: "benign passthrough"}, config: &core.HookConfig{
			ID: "h2", Name: "benign-hook", Priority: 2, FailBehavior: "fail-open",
		}},
	}

	p := NewPipeline(hks, 5*time.Second, 30*time.Second, false, testLogger())
	p.SetAllowModify(true)
	result := p.Execute(context.Background(), &core.HookInput{})

	if result.Decision != core.Modify {
		t.Fatalf("expected MODIFY (only the reconcile hook drives an opinion), got %s", result.Decision)
	}
	if result.ReasonCode != core.ReasonAIGuardSuggestedVsPolicy {
		t.Errorf("reconcile ReasonCode lost through aggregation; got %q want %q",
			result.ReasonCode, core.ReasonAIGuardSuggestedVsPolicy)
	}
}

// TestPipeline_Modify_FirstHookReasonCodeWins documents the tie-break
// rule when multiple Modify hooks each supply a ReasonCode: the first
// hook in priority order wins, matching the existing softReject tie-break
// pattern in the same merge function.
func TestPipeline_Modify_FirstHookReasonCodeWins(t *testing.T) {
	hks := []boundHook{
		{hook: &stubHook{
			decision:   core.Modify,
			reason:     "first hook reason",
			reasonCode: "FIRST_CODE",
		}, config: &core.HookConfig{
			ID: "h1", Name: "modify-1", Priority: 1, FailBehavior: "fail-open",
		}},
		{hook: &stubHook{
			decision:   core.Modify,
			reason:     "second hook reason",
			reasonCode: "SECOND_CODE",
		}, config: &core.HookConfig{
			ID: "h2", Name: "modify-2", Priority: 2, FailBehavior: "fail-open",
		}},
	}

	p := NewPipeline(hks, 5*time.Second, 30*time.Second, false, testLogger())
	p.SetAllowModify(true)
	result := p.Execute(context.Background(), &core.HookInput{})

	if result.ReasonCode != "FIRST_CODE" {
		t.Errorf("first Modify hook's ReasonCode should win; got %q", result.ReasonCode)
	}
	if result.Reason != "first hook reason" {
		t.Errorf("first Modify hook's Reason should win; got %q", result.Reason)
	}
}

func TestPipeline_ClearSoftOnApprove_Default(t *testing.T) {
	// Default: SoftReject is sticky — a subsequent Approve does NOT clear it.
	hks := []boundHook{
		{hook: &stubHook{decision: core.BlockSoft, reason: "flagged"}, config: &core.HookConfig{
			ID: "h1", Name: "soft-hook", Priority: 1, FailBehavior: "fail-open",
		}},
		{hook: &stubHook{decision: core.Approve, reason: "ok"}, config: &core.HookConfig{
			ID: "h2", Name: "approve-hook", Priority: 2, FailBehavior: "fail-open",
		}},
	}

	p := NewPipeline(hks, 5*time.Second, 30*time.Second, false, testLogger())
	result := p.Execute(context.Background(), &core.HookInput{})

	if result.Decision != core.BlockSoft {
		t.Fatalf("expected BLOCK_SOFT (sticky by default), got %s", result.Decision)
	}
	if result.Reason != "flagged" {
		t.Fatalf("expected reason 'flagged', got %q", result.Reason)
	}
}

func TestPipeline_ClearSoftOnApprove_Enabled(t *testing.T) {
	// With clearSoftOnApprove: Approve clears a preceding SoftReject.
	hks := []boundHook{
		{hook: &stubHook{decision: core.BlockSoft, reason: "flagged"}, config: &core.HookConfig{
			ID: "h1", Name: "soft-hook", Priority: 1, FailBehavior: "fail-open",
		}},
		{hook: &stubHook{decision: core.Approve, reason: "ok"}, config: &core.HookConfig{
			ID: "h2", Name: "approve-hook", Priority: 2, FailBehavior: "fail-open",
		}},
	}

	p := NewPipeline(hks, 5*time.Second, 30*time.Second, false, testLogger())
	p.SetClearSoftOnApprove(true)
	result := p.Execute(context.Background(), &core.HookInput{})

	if result.Decision != core.Approve {
		t.Fatalf("expected APPROVE (soft reject cleared), got %s", result.Decision)
	}
}

// tagEmittingHook returns an Approve decision with a preset tag set.
type tagEmittingHook struct {
	core.AnyEndpointAnyModality
	tags []string
}

func (h *tagEmittingHook) Execute(_ context.Context, _ *core.HookInput) (*core.HookResult, error) {
	return &core.HookResult{Decision: core.Approve, Tags: h.tags}, nil
}

// upstreamTagsRecorder captures the value of input.UpstreamTags the pipeline
// provided at the moment this hook was invoked.
type upstreamTagsRecorder struct {
	core.AnyEndpointAnyModality
	observedUpstream []string
}

func (h *upstreamTagsRecorder) Execute(_ context.Context, in *core.HookInput) (*core.HookResult, error) {
	// Capture a stable copy so later mutations don't affect the assertion.
	h.observedUpstream = append([]string(nil), in.UpstreamTags...)
	return &core.HookResult{Decision: core.Approve}, nil
}

func equalStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestPipeline_MergesTagsAsSetUnion(t *testing.T) {
	// Two hooks emit overlapping tag sets. Pipeline must return the
	// sorted, deduplicated union on CompliancePipelineResult.Tags.
	hks := []boundHook{
		{hook: &tagEmittingHook{tags: []string{"compliance:pii", "severity:confidential"}},
			config: &core.HookConfig{ID: "h1", Name: "hook1", Priority: 1, FailBehavior: "fail-open"}},
		{hook: &tagEmittingHook{tags: []string{"severity:confidential", "region:eu-only"}},
			config: &core.HookConfig{ID: "h2", Name: "hook2", Priority: 2, FailBehavior: "fail-open"}},
	}

	p := NewPipeline(hks, 5*time.Second, 30*time.Second, false, testLogger())
	result := p.Execute(context.Background(), &core.HookInput{})

	want := []string{"compliance:pii", "region:eu-only", "severity:confidential"}
	if !equalStringSlice(result.Tags, want) {
		t.Fatalf("merged tags = %v, want %v", result.Tags, want)
	}
}

func TestPipeline_Sequential_AccumulatesUpstreamTags(t *testing.T) {
	// Sequential executor must populate input.UpstreamTags before each
	// subsequent hook, so downstream hooks can observe upstream tag context.
	recorder := &upstreamTagsRecorder{}
	hks := []boundHook{
		{hook: &tagEmittingHook{tags: []string{"compliance:pii"}},
			config: &core.HookConfig{ID: "h1", Name: "hook1", Priority: 1, FailBehavior: "fail-open"}},
		{hook: recorder,
			config: &core.HookConfig{ID: "h2", Name: "hook2", Priority: 2, FailBehavior: "fail-open"}},
	}

	p := NewPipeline(hks, 5*time.Second, 30*time.Second, false /* sequential */, testLogger())
	_ = p.Execute(context.Background(), &core.HookInput{})

	want := []string{"compliance:pii"}
	if !equalStringSlice(recorder.observedUpstream, want) {
		t.Fatalf("hook2 observed UpstreamTags = %v, want %v", recorder.observedUpstream, want)
	}
}

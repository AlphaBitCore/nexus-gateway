package pipeline

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
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

// TestPipeline_HookLatencyMicroseconds verifies executeOneHook stamps precise
// microsecond latency (LatencyUs) alongside the truncated millisecond value, and
// that LatencyMs is the exact integer-ms floor of LatencyUs — i.e. LatencyMs is
// NOT clamped. A clamp-to-≥1 would break the floor invariant for sub-millisecond
// hooks and N×-inflate the summed _ms aggregate.
func TestPipeline_HookLatencyMicroseconds(t *testing.T) {
	hks := []boundHook{
		// Real delay: LatencyUs must reflect it.
		{hook: &stubHook{decision: core.Approve, delay: 3 * time.Millisecond}, config: &core.HookConfig{ID: "slow", Name: "slow", Priority: 1, FailBehavior: "fail-open"}},
		// Near-instant: on a fast host LatencyUs < 1000, so the floor invariant
		// forces LatencyMs == 0 (proves no clamp).
		{hook: &stubHook{decision: core.Approve, delay: 0}, config: &core.HookConfig{ID: "fast", Name: "fast", Priority: 2, FailBehavior: "fail-open"}},
	}
	p := NewPipeline(hks, 5*time.Second, 30*time.Second, false, testLogger())
	result := p.Execute(context.Background(), &core.HookInput{})
	if len(result.HookResults) != 2 {
		t.Fatalf("expected 2 hook results, got %d", len(result.HookResults))
	}
	var slow *core.HookResult
	for i := range result.HookResults {
		hr := &result.HookResults[i]
		// Floor invariant: both fields derive from the same elapsed, so ms is the
		// exact integer-ms floor of us. A clamp would violate this for us<1000.
		if hr.LatencyMs != hr.LatencyUs/1000 {
			t.Fatalf("hook %s: LatencyMs=%d is not the floor of LatencyUs=%d (us/1000=%d) — clamp or skew?",
				hr.HookID, hr.LatencyMs, hr.LatencyUs, hr.LatencyUs/1000)
		}
		if result.HookResults[i].HookID == "slow" {
			slow = hr
		}
	}
	if slow == nil {
		t.Fatal("missing 'slow' hook result")
	}
	if slow.LatencyUs < 2000 {
		t.Fatalf("slow hook LatencyUs=%d, expected >= 2000 (3ms delay)", slow.LatencyUs)
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

// TestPipeline_FailOpen_IncrementsCounter verifies that a hook error resolved
// as fail-open bumps compliance_hook_fail_open_total{hook=<name>}, and that a
// fail-closed error does NOT bump it (the counter tracks silently-degraded
// hooks specifically). Uses an isolated registry so the global metric set is
// untouched.
func TestPipeline_FailOpen_IncrementsCounter(t *testing.T) {
	reg := prometheus.NewRegistry()
	c := promauto.With(reg).NewCounterVec(prometheus.CounterOpts{
		Name: "test_hook_fail_open_total",
		Help: "test",
	}, []string{"hook"})

	// Swap the package-level convenience var for the duration of the test so
	// pipeline.go increments our isolated counter, then restore it.
	prev := HookFailOpenTotal
	HookFailOpenTotal = c
	defer func() { HookFailOpenTotal = prev }()

	// fail-open hook: error must increment the counter by 1.
	openHooks := []boundHook{
		{hook: &stubHook{err: errors.New("guard backend down")}, config: &core.HookConfig{
			ID: "h1", Name: "pii-detector", Priority: 1, FailBehavior: "fail-open",
		}},
	}
	p := NewPipeline(openHooks, 5*time.Second, 30*time.Second, false, testLogger())
	if got := p.Execute(context.Background(), &core.HookInput{}); got.Decision != core.Approve {
		t.Fatalf("expected APPROVE (fail-open), got %s", got.Decision)
	}
	if v := testutil.ToFloat64(c.WithLabelValues("pii-detector")); v != 1 {
		t.Fatalf("expected fail_open_total{pii-detector}=1, got %v", v)
	}

	// fail-closed hook erroring must NOT increment the fail-open counter.
	closedHooks := []boundHook{
		{hook: &stubHook{err: errors.New("guard backend down")}, config: &core.HookConfig{
			ID: "h2", Name: "block-on-secret", Priority: 1, FailBehavior: "fail-closed",
		}},
	}
	pc := NewPipeline(closedHooks, 5*time.Second, 30*time.Second, false, testLogger())
	if got := pc.Execute(context.Background(), &core.HookInput{}); got.Decision != core.RejectHard {
		t.Fatalf("expected REJECT_HARD (fail-closed), got %s", got.Decision)
	}
	if v := testutil.ToFloat64(c.WithLabelValues("block-on-secret")); v != 0 {
		t.Fatalf("expected fail_open_total{block-on-secret}=0 on fail-closed, got %v", v)
	}
	// And the original hook's count is unchanged by the second pipeline.
	if v := testutil.ToFloat64(c.WithLabelValues("pii-detector")); v != 1 {
		t.Fatalf("expected fail_open_total{pii-detector} still 1, got %v", v)
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

// enforcingConfig returns a HookConfig whose onMatch action makes the hook
// ENFORCING (redact or block) and leaves FailBehavior unset, so the strict
// fail-posture is what decides the error outcome.
func enforcingConfig(id, name, action string) *core.HookConfig {
	return &core.HookConfig{
		ID: id, Name: name, Priority: 1,
		Config: map[string]any{"onMatch": map[string]any{"action": action}},
	}
}

// strictPipeline builds a NewPipeline and flips the strict posture so the test
// exercises the non-packet-path (ai-gateway reverse proxy) behavior.
func strictPipeline(hks []boundHook, strict bool) *Pipeline {
	p := NewPipeline(hks, 5*time.Second, 30*time.Second, false, testLogger())
	p.SetStrictFailClosed(strict)
	return p
}

// TestPipeline_StrictEnforcing_BlockHookError_FailsClosed pins BUG-failopen:
// an enforcing (block) hook that ERRORS in a STRICT pipeline must REJECT_HARD
// even though FailBehavior was never seeded "fail-closed" — the guaranteed-
// execution contract for block is broken, so a strict caller must not leak.
func TestPipeline_StrictEnforcing_BlockHookError_FailsClosed(t *testing.T) {
	hks := []boundHook{
		{hook: &stubHook{err: errors.New("guard backend down")},
			config: enforcingConfig("h1", "block-on-secret", "block")},
	}
	result := strictPipeline(hks, true).Execute(context.Background(), &core.HookInput{})

	if result.Decision != core.RejectHard {
		t.Fatalf("expected REJECT_HARD (strict enforcing fail-closed), got %s", result.Decision)
	}
	if result.HookResults[0].ReasonCode != "HOOK_ERROR_FAIL_CLOSED" {
		t.Fatalf("expected HOOK_ERROR_FAIL_CLOSED, got %q", result.HookResults[0].ReasonCode)
	}
}

// TestPipeline_StrictEnforcing_RedactHookError_FailsClosed: a redact-scope hook
// is also enforcing (guaranteed inflight rewrite), so its error in strict mode
// fails closed too.
func TestPipeline_StrictEnforcing_RedactHookError_FailsClosed(t *testing.T) {
	hks := []boundHook{
		{hook: &stubHook{err: errors.New("redactor down")},
			config: enforcingConfig("h1", "pii-redactor", "redact")},
	}
	result := strictPipeline(hks, true).Execute(context.Background(), &core.HookInput{})

	if result.Decision != core.RejectHard {
		t.Fatalf("expected REJECT_HARD (strict redact fail-closed), got %s", result.Decision)
	}
	if result.HookResults[0].ReasonCode != "HOOK_ERROR_FAIL_CLOSED" {
		t.Fatalf("expected HOOK_ERROR_FAIL_CLOSED, got %q", result.HookResults[0].ReasonCode)
	}
}

// TestPipeline_NonStrictEnforcing_HookError_FailsOpen: the SAME enforcing hook
// error in a NON-strict pipeline (packet-path callers) stays fail-open and
// increments hook_fail_open_total — host-network safety mandates fail-open.
func TestPipeline_NonStrictEnforcing_HookError_FailsOpen(t *testing.T) {
	reg := prometheus.NewRegistry()
	c := promauto.With(reg).NewCounterVec(prometheus.CounterOpts{
		Name: "test_strict_fail_open_total", Help: "test",
	}, []string{"hook"})
	prev := HookFailOpenTotal
	HookFailOpenTotal = c
	defer func() { HookFailOpenTotal = prev }()

	hks := []boundHook{
		{hook: &stubHook{err: errors.New("guard backend down")},
			config: enforcingConfig("h1", "block-on-secret", "block")},
	}
	result := strictPipeline(hks, false).Execute(context.Background(), &core.HookInput{})

	if result.Decision != core.Approve {
		t.Fatalf("expected APPROVE (non-strict fail-open), got %s", result.Decision)
	}
	if result.HookResults[0].ReasonCode != "HOOK_ERROR_FAIL_OPEN" {
		t.Fatalf("expected HOOK_ERROR_FAIL_OPEN, got %q", result.HookResults[0].ReasonCode)
	}
	if v := testutil.ToFloat64(c.WithLabelValues("block-on-secret")); v != 1 {
		t.Fatalf("expected fail_open_total{block-on-secret}=1, got %v", v)
	}
}

// TestPipeline_StrictEnforcing_ExplicitFailOpenOverridesStrict: an admin who
// sets FailBehavior=="fail-open" on a known-flaky enforcing hook MUST keep
// fail-open even in strict mode — the explicit override always wins.
func TestPipeline_StrictEnforcing_ExplicitFailOpenOverridesStrict(t *testing.T) {
	cfg := enforcingConfig("h1", "flaky-block", "block")
	cfg.FailBehavior = "fail-open"
	hks := []boundHook{{hook: &stubHook{err: errors.New("flaky guard")}, config: cfg}}

	result := strictPipeline(hks, true).Execute(context.Background(), &core.HookInput{})

	if result.Decision != core.Approve {
		t.Fatalf("expected APPROVE (explicit fail-open override wins in strict), got %s", result.Decision)
	}
	if result.HookResults[0].ReasonCode != "HOOK_ERROR_FAIL_OPEN" {
		t.Fatalf("expected HOOK_ERROR_FAIL_OPEN, got %q", result.HookResults[0].ReasonCode)
	}
}

// TestPipeline_StrictNonEnforcing_HookError_FailsOpen: an approve-scope (non-
// enforcing) hook error in strict mode stays fail-open — only enforcing hooks
// fail closed under the new posture.
func TestPipeline_StrictNonEnforcing_HookError_FailsOpen(t *testing.T) {
	hks := []boundHook{
		{hook: &stubHook{err: errors.New("advisory backend down")},
			config: enforcingConfig("h1", "advisory-scan", "approve")},
	}
	result := strictPipeline(hks, true).Execute(context.Background(), &core.HookInput{})

	if result.Decision != core.Approve {
		t.Fatalf("expected APPROVE (non-enforcing stays fail-open), got %s", result.Decision)
	}
	if result.HookResults[0].ReasonCode != "HOOK_ERROR_FAIL_OPEN" {
		t.Fatalf("expected HOOK_ERROR_FAIL_OPEN, got %q", result.HookResults[0].ReasonCode)
	}
}

// TestPipeline_StrictExplicitFailClosed_StillRejects: explicit fail-closed is
// unchanged by the strict posture — it rejects regardless of enforcing scope.
func TestPipeline_StrictExplicitFailClosed_StillRejects(t *testing.T) {
	hks := []boundHook{
		{hook: &stubHook{err: errors.New("svc down")}, config: &core.HookConfig{
			ID: "h1", Name: "seeded-fail-closed", Priority: 1, FailBehavior: "fail-closed",
		}},
	}
	result := strictPipeline(hks, true).Execute(context.Background(), &core.HookInput{})
	if result.Decision != core.RejectHard {
		t.Fatalf("expected REJECT_HARD (explicit fail-closed), got %s", result.Decision)
	}
}

// TestPipeline_StrictEnforcing_DefaultBlockOnMatch_FailsClosed: a match-only
// hook with NO onMatch block defaults to block (core.ParseOnMatch security
// default), so it counts as enforcing and fails closed in strict mode.
func TestPipeline_StrictEnforcing_DefaultBlockOnMatch_FailsClosed(t *testing.T) {
	hks := []boundHook{
		{hook: &stubHook{err: errors.New("detector down")}, config: &core.HookConfig{
			ID: "h1", Name: "pii-detector", Priority: 1, // no onMatch => defaults to block
		}},
	}
	result := strictPipeline(hks, true).Execute(context.Background(), &core.HookInput{})
	if result.Decision != core.RejectHard {
		t.Fatalf("expected REJECT_HARD (default block-on-match is enforcing), got %s", result.Decision)
	}
	if result.HookResults[0].ReasonCode != "HOOK_ERROR_FAIL_CLOSED" {
		t.Fatalf("expected HOOK_ERROR_FAIL_CLOSED, got %q", result.HookResults[0].ReasonCode)
	}
}

// TestPipeline_StrictEnforcing_UnparseableOnMatch_FailsClosed: a malformed
// onMatch makes core.ParseOnMatch error; the conservative posture treats it as
// enforcing so a strict caller fails closed rather than leaking.
func TestPipeline_StrictEnforcing_UnparseableOnMatch_FailsClosed(t *testing.T) {
	hks := []boundHook{
		{hook: &stubHook{err: errors.New("hook down")}, config: &core.HookConfig{
			ID: "h1", Name: "bad-config", Priority: 1,
			Config: map[string]any{"onMatch": "not-an-object"},
		}},
	}
	result := strictPipeline(hks, true).Execute(context.Background(), &core.HookInput{})
	if result.Decision != core.RejectHard {
		t.Fatalf("expected REJECT_HARD (unparseable onMatch treated as enforcing), got %s", result.Decision)
	}
}

// TestBuildPipeline_PlumbsStrictPosture verifies BuildPipeline threads the
// strictFailClosed flag onto the runtime Pipeline so an enforcing hook error
// fails closed end-to-end (not just when a test flips the field directly).
func TestBuildPipeline_PlumbsStrictPosture(t *testing.T) {
	cfgs := []core.HookConfig{{
		ID: "h1", ImplementationID: "test-erroring", Name: "block-on-secret",
		Enabled: true, Stage: "request", ApplicableIngress: []string{"ALL"},
		Config: map[string]any{"onMatch": map[string]any{"action": "block"}},
	}}
	reg := core.NewHookRegistry()
	reg.Register("test-erroring", func(_ *core.HookConfig) (core.Hook, error) {
		return &stubHook{err: errors.New("backend down")}, nil
	})
	r := NewPolicyResolver(cfgs, reg, testLogger())

	// strict=true → enforcing hook error rejects.
	pipe, err := r.BuildPipeline("request", "AI_GATEWAY", "", nil,
		5*time.Second, 30*time.Second, false, true, testLogger())
	if err != nil || pipe == nil {
		t.Fatalf("BuildPipeline strict: err=%v pipe=%v", err, pipe)
	}
	if got := pipe.Execute(context.Background(), &core.HookInput{}); got.Decision != core.RejectHard {
		t.Fatalf("strict BuildPipeline: expected REJECT_HARD, got %s", got.Decision)
	}

	// strict=false → same hook error stays fail-open.
	pipeOpen, err := r.BuildPipeline("request", "AGENT", "", nil,
		5*time.Second, 30*time.Second, false, false, testLogger())
	if err != nil || pipeOpen == nil {
		t.Fatalf("BuildPipeline non-strict: err=%v pipe=%v", err, pipeOpen)
	}
	if got := pipeOpen.Execute(context.Background(), &core.HookInput{}); got.Decision != core.Approve {
		t.Fatalf("non-strict BuildPipeline: expected APPROVE, got %s", got.Decision)
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

// TestPipeline_Modify_PreservesHookReasonCode pins the Modify reason-code
// fix: a Modify
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

// redactStubHook returns a Modify result carrying replacement content (a redact hook).
type redactStubHook struct {
	core.AnyEndpointAnyModality
	content []core.ContentBlock
}

func (h *redactStubHook) Execute(context.Context, *core.HookInput) (*core.HookResult, error) {
	return &core.HookResult{Decision: core.Modify, ModifiedContent: h.content}, nil
}

// TestPipeline_CoFiringRedactSoftBlock_CarriesRedaction pins the #13 core invariant: when
// a redact hook (Modify + ModifiedContent) co-fires with a soft-block hook, mergeResults
// promotes the aggregate Decision to BlockSoft (the strictest) but MUST carry the redact's
// ModifiedContent — previously it was dropped, leaving consumers unable to apply the
// redaction (fail-closed or leak). CarriesRedaction() must report true so every consumer
// applies the mask instead of keying on Decision==Modify and forwarding the original raw.
func TestPipeline_CoFiringRedactSoftBlock_CarriesRedaction(t *testing.T) {
	hks := []boundHook{
		{hook: &redactStubHook{content: []core.ContentBlock{{Type: "text", Text: "[REDACTED]"}}}, config: &core.HookConfig{ID: "redact", Name: "redact", Priority: 1, FailBehavior: "fail-open"}},
		{hook: &stubHook{decision: core.BlockSoft, reason: "soft flag"}, config: &core.HookConfig{ID: "soft", Name: "soft", Priority: 2, FailBehavior: "fail-open"}},
	}
	p := NewPipeline(hks, 5*time.Second, 30*time.Second, false, testLogger())
	result := p.Execute(context.Background(), &core.HookInput{})

	if result.Decision != core.BlockSoft {
		t.Fatalf("co-firing redact+soft-block must aggregate to BlockSoft (the ceiling), got %s", result.Decision)
	}
	if len(result.ModifiedContent) == 0 {
		t.Fatal("the redact ModifiedContent must be CARRIED under BlockSoft (not dropped) — the #13 core invariant")
	}
	if !result.CarriesRedaction() {
		t.Fatal("CarriesRedaction() must be true for a BlockSoft masking a co-firing redact")
	}
}

// TestPipeline_StandaloneSoftBlock_NoRedaction is the negative: a soft-block with no
// co-firing redact carries no redaction → CarriesRedaction() false → consumers
// deliver-with-warning (the original), not fail-closed/masked.
func TestPipeline_StandaloneSoftBlock_NoRedaction(t *testing.T) {
	hks := []boundHook{
		{hook: &stubHook{decision: core.BlockSoft, reason: "soft flag"}, config: &core.HookConfig{ID: "soft", Name: "soft", Priority: 1, FailBehavior: "fail-open"}},
	}
	p := NewPipeline(hks, 5*time.Second, 30*time.Second, false, testLogger())
	result := p.Execute(context.Background(), &core.HookInput{})
	if result.Decision != core.BlockSoft {
		t.Fatalf("expected BlockSoft, got %s", result.Decision)
	}
	if result.CarriesRedaction() {
		t.Fatal("a standalone soft-block carries no redaction")
	}
}

// spanStubHook returns a configurable decision with optional ModifiedContent and
// TransformSpans — exercises the applicable-artifact discriminator (#14).
type spanStubHook struct {
	core.AnyEndpointAnyModality
	decision core.Decision
	content  []core.ContentBlock
	spans    []normalize.TransformSpan
}

func (h *spanStubHook) Execute(context.Context, *core.HookInput) (*core.HookResult, error) {
	return &core.HookResult{Decision: h.decision, ModifiedContent: h.content, TransformSpans: h.spans}, nil
}

func auditOnlySpan() normalize.TransformSpan {
	return normalize.TransformSpan{Source: normalize.SourceHook, Action: normalize.ActionRedact, ContentAddress: normalize.AddressAuditOnlySentinel, Start: 0, End: 4}
}

func applicableSpan() normalize.TransformSpan {
	return normalize.TransformSpan{Source: normalize.SourceHook, Action: normalize.ActionRedact, ContentAddress: "messages.0.content.0.toolUse.input.0", Start: 0, End: 4}
}

// TestPipeline_ApproveWebhookAuditSpans_CoFiringSoftBlock_NoRedaction is the #14 fix:
// an Approve hook emitting AUDIT-ONLY sentinel spans (the approve-webhook+redactions
// shape) co-firing with a soft-block promotes the aggregate to BlockSoft, but those
// advisory spans are NOT an applicable redaction — CarriesRedaction() must be false so
// the appliance soft-delivers instead of failing closed (over-block).
func TestPipeline_ApproveWebhookAuditSpans_CoFiringSoftBlock_NoRedaction(t *testing.T) {
	hks := []boundHook{
		{hook: &spanStubHook{decision: core.Approve, spans: []normalize.TransformSpan{auditOnlySpan()}}, config: &core.HookConfig{ID: "wh", Name: "webhook", Priority: 1, FailBehavior: "fail-open"}},
		{hook: &stubHook{decision: core.BlockSoft, reason: "soft flag"}, config: &core.HookConfig{ID: "soft", Name: "soft", Priority: 2, FailBehavior: "fail-open"}},
	}
	p := NewPipeline(hks, 5*time.Second, 30*time.Second, false, testLogger())
	result := p.Execute(context.Background(), &core.HookInput{})
	if result.Decision != core.BlockSoft {
		t.Fatalf("expected BlockSoft aggregate, got %s", result.Decision)
	}
	if result.RedactionApplicable {
		t.Fatal("an Approve hook's audit-only spans must NOT set RedactionApplicable")
	}
	if result.CarriesRedaction() {
		t.Fatal("approve-webhook audit-only spans masked by soft-block must NOT CarryRedaction (the #14 over-block fix)")
	}
	// Audit union still records the advisory spans — the fix changes only the inflight predicate.
	if len(result.TransformSpans) != 1 {
		t.Fatalf("advisory span must still be unioned into the audit record, got %d", len(result.TransformSpans))
	}
}

// TestPipeline_ModifyWebhookFlatOnly_CoFiringSoftBlock_NoRedaction closes the sibling
// hole: a Modify hook (e.g. a webhook reconciled under an approve ceiling) carrying ONLY
// audit-only sentinel spans and NO ModifiedContent is not an applicable inflight
// redaction, so under a co-firing soft-block CarriesRedaction() must be false.
func TestPipeline_ModifyWebhookFlatOnly_CoFiringSoftBlock_NoRedaction(t *testing.T) {
	hks := []boundHook{
		{hook: &spanStubHook{decision: core.Modify, spans: []normalize.TransformSpan{auditOnlySpan()}}, config: &core.HookConfig{ID: "wh", Name: "webhook", Priority: 1, FailBehavior: "fail-open"}},
		{hook: &stubHook{decision: core.BlockSoft, reason: "soft flag"}, config: &core.HookConfig{ID: "soft", Name: "soft", Priority: 2, FailBehavior: "fail-open"}},
	}
	p := NewPipeline(hks, 5*time.Second, 30*time.Second, false, testLogger())
	result := p.Execute(context.Background(), &core.HookInput{})
	if result.Decision != core.BlockSoft {
		t.Fatalf("expected BlockSoft aggregate, got %s", result.Decision)
	}
	if result.CarriesRedaction() {
		t.Fatal("modify-webhook flat-only spans masked by soft-block must NOT CarryRedaction (sibling of the #14 fix)")
	}
}

// TestPipeline_ToolArgOnlyModify_CoFiringSoftBlock_CarriesRedaction is the leak-guard:
// a real redaction carried by an APPLICABLE span with NO ModifiedContent (the tool-call-
// argument redaction shape) masked by a co-firing soft-block MUST still CarryRedaction —
// dropping the span clause would leak it.
func TestPipeline_ToolArgOnlyModify_CoFiringSoftBlock_CarriesRedaction(t *testing.T) {
	hks := []boundHook{
		{hook: &spanStubHook{decision: core.Modify, spans: []normalize.TransformSpan{applicableSpan()}}, config: &core.HookConfig{ID: "redact", Name: "redact", Priority: 1, FailBehavior: "fail-open"}},
		{hook: &stubHook{decision: core.BlockSoft, reason: "soft flag"}, config: &core.HookConfig{ID: "soft", Name: "soft", Priority: 2, FailBehavior: "fail-open"}},
	}
	p := NewPipeline(hks, 5*time.Second, 30*time.Second, false, testLogger())
	result := p.Execute(context.Background(), &core.HookInput{})
	if result.Decision != core.BlockSoft {
		t.Fatalf("expected BlockSoft aggregate, got %s", result.Decision)
	}
	if !result.RedactionApplicable || !result.CarriesRedaction() {
		t.Fatal("a tool-arg-only Modify (applicable span, no ModifiedContent) masked by soft-block MUST CarryRedaction — else it leaks")
	}
}

// TestPipeline_SelfRedactingBlockSoft_ApplicableSpan_CarriesRedaction: a single hook
// emitting BlockSoft WITH an applicable span carries that redaction (the aggregate unions
// every hook's spans), so CarriesRedaction() is true.
func TestPipeline_SelfRedactingBlockSoft_ApplicableSpan_CarriesRedaction(t *testing.T) {
	hks := []boundHook{
		{hook: &spanStubHook{decision: core.BlockSoft, spans: []normalize.TransformSpan{applicableSpan()}}, config: &core.HookConfig{ID: "soft", Name: "soft", Priority: 1, FailBehavior: "fail-open"}},
	}
	p := NewPipeline(hks, 5*time.Second, 30*time.Second, false, testLogger())
	result := p.Execute(context.Background(), &core.HookInput{})
	if result.Decision != core.BlockSoft {
		t.Fatalf("expected BlockSoft, got %s", result.Decision)
	}
	if !result.CarriesRedaction() {
		t.Fatal("a self-redacting BlockSoft with an applicable span must CarryRedaction (leak-guard)")
	}
}

// TestPipeline_SelfRedactingBlockSoft_ModifiedContentOnly_NoRedaction pins the merge
// asymmetry: mergeResults carries ModifiedContent ONLY from Modify hooks, so a BlockSoft
// hook's ModifiedContent is dropped from the aggregate. CarriesRedaction() must therefore
// be false (the aggregate cannot apply it) — fold-to-block is the fail-safe disposition,
// not a claimed-but-unappliable redaction.
func TestPipeline_SelfRedactingBlockSoft_ModifiedContentOnly_NoRedaction(t *testing.T) {
	hks := []boundHook{
		{hook: &spanStubHook{decision: core.BlockSoft, content: []core.ContentBlock{{Type: "text", Text: "[REDACTED]"}}}, config: &core.HookConfig{ID: "soft", Name: "soft", Priority: 1, FailBehavior: "fail-open"}},
	}
	p := NewPipeline(hks, 5*time.Second, 30*time.Second, false, testLogger())
	result := p.Execute(context.Background(), &core.HookInput{})
	if result.Decision != core.BlockSoft {
		t.Fatalf("expected BlockSoft, got %s", result.Decision)
	}
	if len(result.ModifiedContent) != 0 {
		t.Fatal("the merge must NOT carry a BlockSoft hook's ModifiedContent into the aggregate")
	}
	if result.CarriesRedaction() {
		t.Fatal("a BlockSoft hook's ModifiedContent is not carried, so CarriesRedaction must be false (fold-to-block fail-safe)")
	}
}

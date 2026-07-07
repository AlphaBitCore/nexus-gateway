package pipeline

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
)

// onMatchCfg builds a HookConfig.Config map in the shape the seed stores it
// (config.onMatch.action), e.g. the seeded pii-scanner / content-safety hooks
// carry {"onMatch": {"action": "redact"}}. Passing action="" omits the onMatch
// block entirely, exercising ParseOnMatch's block-by-default branch.
func onMatchCfg(action string) map[string]any {
	if action == "" {
		return map[string]any{}
	}
	return map[string]any{"onMatch": map[string]any{"action": action}}
}

func boundWithAction(id, action string) boundHook {
	return boundHook{
		hook:   &stubHook{decision: core.Approve},
		config: &core.HookConfig{ID: id, Name: id, Config: onMatchCfg(action)},
	}
}

func newTestPipeline(hks []boundHook) *Pipeline {
	return NewPipeline(hks, 5*time.Second, 30*time.Second, false, testLogger())
}

// TestPipeline_MayRedact_RoutesSeedRedactHooksToBuffer asserts the streaming
// routing predicate: a resolved response pipeline carrying a redact-action hook
// (the seeded default — every compliance hook is onMatch.action="redact") reports
// MayRedact()==true so the relay forces buffered execution, while MayBlock() stays
// false (no block-action hook present).
func TestPipeline_MayRedact_RoutesSeedRedactHooksToBuffer(t *testing.T) {
	p := newTestPipeline([]boundHook{boundWithAction("pii-scanner", "redact")})

	if !p.MayRedact() {
		t.Fatal("MayRedact()=false for a redact-action hook; request would stream unredacted instead of routing to buffer")
	}
	if p.MayBlock() {
		t.Error("MayBlock()=true with no block-action hook present")
	}
}

func TestPipeline_MayRedact_BlockAndApprove(t *testing.T) {
	cases := []struct {
		name       string
		hooks      []boundHook
		wantRedact bool
		wantBlock  bool
	}{
		{
			name:       "block action only",
			hooks:      []boundHook{boundWithAction("h", "block")},
			wantRedact: false,
			wantBlock:  true,
		},
		{
			name:       "approve action only does not enforce",
			hooks:      []boundHook{boundWithAction("h", "approve")},
			wantRedact: false,
			wantBlock:  false,
		},
		{
			// ParseOnMatch defaults an absent onMatch to block; a content hook with
			// no explicit action enforces as block at execution, so MayBlock must
			// reflect that (safe buffer-routing direction).
			name:       "absent onMatch defaults to block",
			hooks:      []boundHook{boundWithAction("h", "")},
			wantRedact: false,
			wantBlock:  true,
		},
		{
			// Mixed pipeline: one redact hook among approves still routes to buffer.
			name: "redact present among approves",
			hooks: []boundHook{
				boundWithAction("a1", "approve"),
				boundWithAction("pii", "redact"),
				boundWithAction("a2", "approve"),
			},
			wantRedact: true,
			wantBlock:  false,
		},
		{
			name:       "empty pipeline enforces nothing",
			hooks:      []boundHook{},
			wantRedact: false,
			wantBlock:  false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newTestPipeline(tc.hooks)
			if got := p.MayRedact(); got != tc.wantRedact {
				t.Errorf("MayRedact()=%v, want %v", got, tc.wantRedact)
			}
			if got := p.MayBlock(); got != tc.wantBlock {
				t.Errorf("MayBlock()=%v, want %v", got, tc.wantBlock)
			}
		})
	}
}

// TestPipeline_MayRedact_NilConfigSkipped guards the nil-config branch: a bound
// hook with no config must not panic and must not be read as enforcing.
func TestPipeline_MayRedact_NilConfigSkipped(t *testing.T) {
	p := newTestPipeline([]boundHook{{hook: &stubHook{decision: core.Approve}, config: nil}})
	if p.MayRedact() || p.MayBlock() {
		t.Error("nil-config hook must report neither redact nor block")
	}
}

// escalatableHook is a test double for a hook whose runtime decision can exceed
// its declarative onMatch ceiling (the webhook-forward shape). exceed controls
// whether it advertises that capability.
type escalatableHook struct {
	core.AnyEndpointAnyModality
	exceed  bool
	execErr error
}

func (h *escalatableHook) Execute(context.Context, *core.HookInput) (*core.HookResult, error) {
	if h.execErr != nil {
		return nil, h.execErr
	}
	return &core.HookResult{Decision: core.Approve}, nil
}

func (h *escalatableHook) MayExceedOnMatch() bool { return h.exceed }

// malformedOnMatchCfg returns a config whose onMatch.action is an unknown value,
// so core.ParseOnMatch returns an error. Mirrors a misconfigured hook row.
func malformedOnMatchCfg() map[string]any {
	return map[string]any{"onMatch": map[string]any{"action": "bogus-not-an-action"}}
}

// TestPipeline_MayRedact_MalformedOnMatchEnforces covers MEDIUM-2: a hook whose
// onMatch cannot be parsed must be treated as enforcing (both block- and
// redact-capable) so it routes to buffer rather than leaking onto the live path.
// This mirrors hookIsEnforcing, which already over-routes the same parse error.
func TestPipeline_MayRedact_MalformedOnMatchEnforces(t *testing.T) {
	p := newTestPipeline([]boundHook{{
		hook:   &stubHook{decision: core.Approve},
		config: &core.HookConfig{ID: "bad", Name: "bad", Config: malformedOnMatchCfg()},
	}})
	if !p.MayBlock() {
		t.Error("MayBlock()=false for a malformed-onMatch hook; it would stream instead of routing to buffer")
	}
	if !p.MayRedact() {
		t.Error("MayRedact()=false for a malformed-onMatch hook; it would stream instead of routing to buffer")
	}
}

// TestPipeline_MayRedact_RuntimeEscalatable covers HIGH-1: a hook that can exceed
// its declared onMatch ceiling at runtime (webhook-forward, which reconciles a
// remote reply against the ceiling via the strictest of the two) must be treated
// as both block- and redact-capable even when its declared action is approve, so
// its runtime enforcement is never under-routed onto the audit-only live path.
func TestPipeline_MayRedact_RuntimeEscalatable(t *testing.T) {
	// Declared action is approve (non-enforcing), but the hook advertises that it
	// can exceed that ceiling at runtime.
	p := newTestPipeline([]boundHook{{
		hook:   &escalatableHook{exceed: true},
		config: &core.HookConfig{ID: "webhook", Name: "webhook", Config: onMatchCfg("approve")},
	}})
	if !p.MayBlock() {
		t.Error("MayBlock()=false for a runtime-escalatable approve-ceiling hook; runtime block would be dropped onto the live path")
	}
	if !p.MayRedact() {
		t.Error("MayRedact()=false for a runtime-escalatable approve-ceiling hook; runtime redact would be dropped onto the live path")
	}
}

// TestPipeline_MayRedact_EscalatableOptOut guards the negative side of HIGH-1: a
// hook that implements core.RuntimeEscalatable but reports false (cannot exceed
// its ceiling) is governed by its declared action like any ordinary hook — the
// over-route fires only for genuinely escalatable hooks, not for every
// implementer of the interface.
func TestPipeline_MayRedact_EscalatableOptOut(t *testing.T) {
	p := newTestPipeline([]boundHook{{
		hook:   &escalatableHook{exceed: false},
		config: &core.HookConfig{ID: "h", Name: "h", Config: onMatchCfg("approve")},
	}})
	if p.MayBlock() || p.MayRedact() {
		t.Error("a non-escalating (exceed=false) approve-ceiling hook must enforce neither block nor redact")
	}
}

// TestPipeline_FailClosedOnError_RuntimeEscalatable covers the error-posture variant
// (review 17-LOW-1): a runtime-escalatable hook with an approve ceiling that ERRORS
// under strict posture must fail closed (RejectHard), because its runtime decision
// could have exceeded the declared ceiling — mirroring the routing predicate so the
// strict appliance does not fail open on the very hook whose enforcement it cannot see.
func TestPipeline_FailClosedOnError_RuntimeEscalatable(t *testing.T) {
	p := newTestPipeline([]boundHook{{
		hook:   &escalatableHook{exceed: true, execErr: errors.New("transient webhook failure")},
		config: &core.HookConfig{ID: "webhook", Name: "webhook", Config: onMatchCfg("approve")},
	}})
	p.SetStrictFailClosed(true)
	res := p.Execute(context.Background(), &core.HookInput{Stage: "response"})
	if res.Decision != core.RejectHard {
		t.Fatalf("a strict escalatable hook's transient error must fail closed (RejectHard), got %s", res.Decision)
	}
}

// TestPipeline_FailClosedOnError_NonEscalatable_StaysOpen is the negative side: a
// non-escalatable approve-ceiling hook erroring under strict stays fail-open — its
// declared ceiling means a transient error cannot have leaked any enforcement.
func TestPipeline_FailClosedOnError_NonEscalatable_StaysOpen(t *testing.T) {
	p := newTestPipeline([]boundHook{{
		hook:   &escalatableHook{exceed: false, execErr: errors.New("transient failure")},
		config: &core.HookConfig{ID: "h", Name: "h", Config: onMatchCfg("approve")},
	}})
	p.SetStrictFailClosed(true)
	res := p.Execute(context.Background(), &core.HookInput{Stage: "response"})
	if res.Decision == core.RejectHard {
		t.Fatalf("a non-escalatable approve-ceiling hook's error must NOT fail closed, got %s", res.Decision)
	}
}

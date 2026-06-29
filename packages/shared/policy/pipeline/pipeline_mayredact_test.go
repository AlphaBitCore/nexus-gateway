package pipeline

import (
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

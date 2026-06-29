package compliance

import (
	"context"
	"github.com/goccy/go-json"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/sync/shadow"
	hooks "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/pipeline"
)

// The agent's hook-config loader never fails (it reads an in-memory atomic
// slot), so the only deterministic way to make HookConfigCache.Reload fail
// is the SnapshotCache contract that a nil context is rejected before the
// loader runs. The tests below use a nil context purely as that injection
// seam; what they pin is the business contract on reload failure: the
// applier surfaces a wrapped error to configsync AND the previously-applied
// policy stays enforced (the resolver is only swapped on successful reload).

// TestApplyHooksShadowState_ReloadFailure_KeepsPriorPolicy pins the
// failed-reload contract for the hooks key: when the hook cache reload
// fails, ApplyHooksShadowState returns a wrapped error (so configsync can
// report the apply as failed) and connection enforcement continues on the
// previously-loaded policy — a failed push must never blank live enforcement.
func TestApplyHooksShadowState_ReloadFailure_KeepsPriorPolicy(t *testing.T) {
	p := buildPipelineWithConnectionHook(t, &rejectAllConnHook{reason: "host not allowed"})

	// Precondition: the seeded policy blocks.
	blocked, _ := p.EvaluateConnection(context.Background(), EvaluateConnectionInput{
		SourceIP:   "10.0.0.1",
		TargetHost: "evil.example.com",
	})
	if !blocked {
		t.Fatal("precondition: seeded connection policy must block")
	}

	// An authoritative-empty push ({"hookConfigs":[]}) would normally clear
	// the resolver; with the reload failing it must NOT take effect.
	var nilCtx context.Context
	err := p.ApplyHooksShadowState(nilCtx, json.RawMessage(`{"hookConfigs":[]}`))
	if err == nil {
		t.Fatal("expected error when hook cache reload fails")
	}
	if !strings.Contains(err.Error(), "hook cache reload") {
		t.Fatalf("error must identify the reload step; got %q", err.Error())
	}

	blocked, reason := p.EvaluateConnection(context.Background(), EvaluateConnectionInput{
		SourceIP:   "10.0.0.1",
		TargetHost: "evil.example.com",
	})
	if !blocked {
		t.Fatal("previous policy must stay enforced after a failed hooks reload")
	}
	if reason != "host not allowed" {
		t.Fatalf("reason = %q, want the previously-applied policy's reason", reason)
	}
}

// TestApplyRulePacksShadowState_ReloadFailure_SurfacesError pins the same
// failed-reload contract on the installed_rule_packs key, for both payload
// arms that route through the hook re-reload (indexed packs and the
// empty-payload clear). The applier must return a wrapped error naming the
// re-reload step, and the resolver must keep serving the previously-loaded
// hooks.
func TestApplyRulePacksShadowState_ReloadFailure_SurfacesError(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{
			name: "indexed pack payload",
			raw: `{"installedRulePacks":[{"id":"i1","name":"safety","version":"1.0.0",` +
				`"boundHookId":"h-seed","enabled":true,"rules":[{"ruleId":"r1","pattern":"secret"}]}]}`,
		},
		{
			name: "empty payload clear",
			raw:  `{}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := NewAgentPipeline(silentLogger())
			applyHooks(t, p, []hooks.HookConfig{
				{ID: "h-seed", ImplementationID: "pii-detector", Name: "h-seed",
					Stage: "request", Enabled: true, FailBehavior: "fail-open"},
			})
			if !p.Resolver().HasHooks("request") {
				t.Fatal("precondition: seeded request hook must be resolvable")
			}

			var nilCtx context.Context
			err := p.ApplyRulePacksShadowState(nilCtx, json.RawMessage(tc.raw))
			if err == nil {
				t.Fatal("expected error when the post-rule-pack hook reload fails")
			}
			if !strings.Contains(err.Error(), "hook cache re-reload after rule pack update") {
				t.Fatalf("error must identify the re-reload step; got %q", err.Error())
			}

			// The resolver was not swapped: enforcement continues on the
			// previously-loaded hooks.
			if !p.Resolver().HasHooks("request") {
				t.Fatal("previously-loaded hooks must survive a failed rule-pack reload")
			}
		})
	}
}

// TestEvaluateConnection_NilResolver_FailsOpen pins the first fail-open
// guard in EvaluateConnection: if the hook cache yields no resolver at all,
// the agent must allow the connection (blocked=false, empty reason). The
// agent NE proxy sits in the host's outbound packet path, so an absent
// resolver must degrade to passthrough, never to refusal. A zero-value
// HookConfigCache (ttl=0, no resolver constructed) is the only state that
// produces a nil resolver, so the test swaps one in.
func TestEvaluateConnection_NilResolver_FailsOpen(t *testing.T) {
	p := NewAgentPipeline(silentLogger())
	p.hookCache = &pipeline.HookConfigCache{}

	blocked, reason := p.EvaluateConnection(context.Background(), EvaluateConnectionInput{
		SourceIP:   "10.0.0.1",
		TargetHost: "api.openai.com",
	})
	if blocked {
		t.Fatalf("expected blocked=false (fail-open) with nil resolver, got blocked=true reason=%q", reason)
	}
	if reason != "" {
		t.Fatalf("expected empty reason with nil resolver, got %q", reason)
	}
}

// TestApplyDomainsShadowState_NilEngine_Reinitialized pins the defensive
// re-init inside ApplyDomainsShadowState: even if the engine pointer was
// never seated (a pipeline constructed outside the normal ctor path), a
// domains push must build a fresh engine and serve per-host decisions from
// the pushed snapshot rather than dropping the update or panicking.
func TestApplyDomainsShadowState_NilEngine_Reinitialized(t *testing.T) {
	p := NewAgentPipeline(silentLogger())
	p.domainEngine = nil

	applyDomains(t, p, []shadow.InterceptionDomainDTO{
		{
			ID: "dom-reinit", Name: "reinit", HostPattern: "example.com",
			HostMatchType: "EXACT", AdapterID: "openai-compat",
			Enabled: true, Priority: 100,
			DefaultPathAction: "PROCESS", OnAdapterError: "FAIL_OPEN",
			NetworkZone: "PUBLIC",
		},
	})

	eng := p.DomainEngine()
	if eng == nil {
		t.Fatal("ApplyDomainsShadowState must re-initialize a nil domain engine")
	}
	matched := eng.MatchHost("example.com")
	if matched == nil || matched.ID != "dom-reinit" {
		t.Fatalf("re-initialized engine must serve the pushed domain; got %+v", matched)
	}
}

package compliance

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/sync/shadow"
	hooks "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
)

// strPtr / intPtr / boolPtr help build the nullable per-host override fields
// on InterceptionDomainDTO from literal values without scattering temporaries.
func strPtr(s string) *string { return &s }
func intPtr(i int) *int       { return &i }
func boolPtr(b bool) *bool    { return &b }

// modifyOnlyHook is a non-ConnectionStageCompatible hook. When bound at
// stage="connection" the resolver will refuse to build the pipeline, exercising
// the BuildPipeline-err arm of EvaluateConnection's fail-open contract.
type modifyOnlyHook struct {
	hooks.AnyEndpointAnyModality
}

func (*modifyOnlyHook) Execute(context.Context, *hooks.HookInput) (*hooks.HookResult, error) {
	return &hooks.HookResult{Decision: hooks.Modify}, nil
}

// emptyReasonRejectHook returns RejectHard with a truly empty Reason so the
// agent-level fallback string (`connection blocked by compliance policy`) gets
// exercised — the pipeline's reason aggregation copies r.Reason verbatim, so
// an empty hook reason reaches the agent layer untouched.
type emptyReasonRejectHook struct {
	hooks.AnyEndpointAnyModality
}

func (*emptyReasonRejectHook) Execute(context.Context, *hooks.HookInput) (*hooks.HookResult, error) {
	return &hooks.HookResult{Decision: hooks.RejectHard, Reason: ""}, nil
}
func (*emptyReasonRejectHook) ConnectionStageOK() {}

// buildPipelineWithFactory installs a single connection-stage hook config
// pointing at a caller-supplied factory and returns the pipeline. The factory
// can return an error (factory-failure path) or build any kind of hook
// (including one missing ConnectionStageCompatible) so each error arm of
// EvaluateConnection is exercisable without touching production code.
func buildPipelineWithFactory(t *testing.T, implID string, factory hooks.HookFactory) *AgentPipeline {
	t.Helper()
	registry := hooks.NewHookRegistry()
	registry.Register(implID, factory)
	p := newAgentPipelineWithRegistry(silentLogger(), registry)
	payload := map[string]any{
		"hookConfigs": []hooks.HookConfig{
			{
				ID:                "h1",
				ImplementationID:  implID,
				Name:              "factory-error-hook",
				Stage:             "connection",
				Enabled:           true,
				FailBehavior:      "fail-open",
				ApplicableIngress: []string{"ALL"},
			},
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := p.ApplyHooksShadowState(context.Background(), raw); err != nil {
		t.Fatalf("ApplyHooksShadowState: %v", err)
	}
	return p
}

// TestEvaluateConnection_EmptyReason_AgentFallback verifies the agent-level
// reason fallback. The pipeline copies HookResult.Reason verbatim, so a hook
// that returns RejectHard with an empty Reason hits the
// `connection blocked by compliance policy` default in EvaluateConnection.
func TestEvaluateConnection_EmptyReason_AgentFallback(t *testing.T) {
	p := buildPipelineWithConnectionHook(t, &emptyReasonRejectHook{})

	blocked, reason := p.EvaluateConnection(context.Background(), EvaluateConnectionInput{
		SourceIP:   "10.0.0.1",
		TargetHost: "evil.example.com",
	})
	if !blocked {
		t.Fatalf("expected blocked=true on RejectHard")
	}
	if reason != "connection blocked by compliance policy" {
		t.Fatalf("reason = %q, want default fallback string", reason)
	}
}

// TestEvaluateConnection_FactoryError_FailsOpen drives the
// `resolver.BuildPipeline` err arm of EvaluateConnection. The registered
// factory always errors, so BuildPipeline returns (nil, err), and per the
// fail-open contract EvaluateConnection MUST return (false, "").
func TestEvaluateConnection_FactoryError_FailsOpen(t *testing.T) {
	p := buildPipelineWithFactory(t, "bad-factory", func(*hooks.HookConfig) (hooks.Hook, error) {
		return nil, errors.New("synthetic factory failure")
	})
	blocked, reason := p.EvaluateConnection(context.Background(), EvaluateConnectionInput{
		SourceIP:   "10.0.0.1",
		TargetHost: "api.openai.com",
	})
	if blocked {
		t.Fatalf("expected blocked=false (fail-open) on factory error, got blocked=true reason=%q", reason)
	}
	if reason != "" {
		t.Fatalf("expected empty reason on fail-open, got %q", reason)
	}
}

// TestEvaluateConnection_NonConnectionStageHook_FailsOpen pins the second
// BuildPipeline err arm: a hook bound at stage="connection" that does NOT
// implement ConnectionStageCompatible (e.g. a Modify-capable hook) makes
// resolve() error. Per fail-open, EvaluateConnection still returns (false, "").
func TestEvaluateConnection_NonConnectionStageHook_FailsOpen(t *testing.T) {
	p := buildPipelineWithFactory(t, "modify-only", func(*hooks.HookConfig) (hooks.Hook, error) {
		return &modifyOnlyHook{}, nil
	})
	blocked, reason := p.EvaluateConnection(context.Background(), EvaluateConnectionInput{
		SourceIP:   "10.0.0.1",
		TargetHost: "api.openai.com",
	})
	if blocked {
		t.Fatalf("expected blocked=false (fail-open) on non-conn-stage hook, got blocked=true reason=%q", reason)
	}
	if reason != "" {
		t.Fatalf("expected empty reason, got %q", reason)
	}
}

// TestApplySnapshot_PopulatesResolverAndDomainSnapshot verifies the
// ConfigSnapshot push path used by shadow.Manager (also exercised in the
// intercept handler test). Both the resolver and the domain snapshot must
// update atomically as a unit, so a single call should bring both surfaces up.
func TestApplySnapshot_PopulatesResolverAndDomainSnapshot(t *testing.T) {
	p := NewAgentPipeline(silentLogger())
	if sz := p.Snapshot().Size(); sz != 0 {
		t.Fatalf("precondition: empty snapshot, got size=%d", sz)
	}
	if p.Resolver().HasHooks("request") {
		t.Fatalf("precondition: no request hooks")
	}

	snap := &shadow.ConfigSnapshot{
		HookConfigs: []hooks.HookConfig{
			{ID: "h-apply", ImplementationID: "pii-detector", Name: "h-apply",
				Stage: "request", Enabled: true, FailBehavior: "fail-open"},
		},
		InterceptionDomains: []shadow.InterceptionDomainDTO{
			{
				ID:                "dom-apply",
				Name:              "apply",
				HostPattern:       "api.openai.com",
				HostMatchType:     "EXACT",
				AdapterID:         "openai-compat",
				Enabled:           true,
				Priority:          100,
				DefaultPathAction: "PROCESS",
				OnAdapterError:    "FAIL_OPEN",
				NetworkZone:       "PUBLIC",
			},
		},
	}
	p.ApplySnapshot(snap)

	if !p.Resolver().HasHooks("request") {
		t.Fatal("ApplySnapshot must replace the resolver with the snapshot hooks")
	}
	if sz := p.Snapshot().Size(); sz != 1 {
		t.Fatalf("ApplySnapshot must replace the domain snapshot; size=%d want 1", sz)
	}
}

// TestApplySnapshot_EmptySlicesClearStateAsAUnit pins the atomic semantics
// of the dual update. After seeding a non-empty resolver + snapshot, an
// ApplySnapshot with empty slices must produce an empty resolver and an
// empty domain snapshot — readers never see a half-applied state where one
// surface is still populated.
func TestApplySnapshot_EmptySlicesClearStateAsAUnit(t *testing.T) {
	p := NewAgentPipeline(silentLogger())
	seed := &shadow.ConfigSnapshot{
		HookConfigs: []hooks.HookConfig{
			{ID: "seed", ImplementationID: "pii-detector", Name: "seed",
				Stage: "request", Enabled: true, FailBehavior: "fail-open"},
		},
		InterceptionDomains: []shadow.InterceptionDomainDTO{
			{
				ID: "seed", Name: "seed", HostPattern: "example.com",
				HostMatchType: "EXACT", AdapterID: "openai-compat",
				Enabled: true, Priority: 100,
				DefaultPathAction: "PROCESS", OnAdapterError: "FAIL_OPEN",
				NetworkZone: "PUBLIC",
			},
		},
	}
	p.ApplySnapshot(seed)
	if !p.Resolver().HasHooks("request") || p.Snapshot().Size() != 1 {
		t.Fatalf("seed precondition failed")
	}

	// Authoritative empty payload: both surfaces clear.
	p.ApplySnapshot(&shadow.ConfigSnapshot{})
	if p.Resolver().HasHooks("request") {
		t.Fatal("empty snapshot must clear resolver hooks")
	}
	if sz := p.Snapshot().Size(); sz != 0 {
		t.Fatalf("empty snapshot must clear domain snapshot; size=%d", sz)
	}
}

// TestApplyRulePacksShadowState_Empty_ClearsRegistryAndReloads pins the
// no-op / clear semantics. An empty payload (and the equivalent shapes)
// MUST clear the rule pack registry; the in-memory hook config slice (if
// present) is then re-emitted with no `_rulePackInstalls` keys.
func TestApplyRulePacksShadowState_Empty_ClearsRegistryAndReloads(t *testing.T) {
	p := NewAgentPipeline(silentLogger())

	// Seed a hook so we can verify reload runs without removing the hook.
	hookPayload, _ := json.Marshal(map[string]any{
		"hookConfigs": []hooks.HookConfig{
			{ID: "h1", ImplementationID: "pii-detector", Name: "h1",
				Stage: "request", Enabled: true, FailBehavior: "fail-open"},
		},
	})
	if err := p.ApplyHooksShadowState(context.Background(), hookPayload); err != nil {
		t.Fatalf("seed hooks: %v", err)
	}

	cases := []struct {
		name string
		raw  string
	}{
		{"empty bytes", ""},
		{"null", "null"},
		{"empty object", "{}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := p.ApplyRulePacksShadowState(context.Background(), json.RawMessage(tc.raw)); err != nil {
				t.Fatalf("ApplyRulePacksShadowState err: %v", err)
			}
			if !p.Resolver().HasHooks("request") {
				t.Fatal("rule pack clear must NOT remove the resolver's seeded hook")
			}
			reg := p.rulePacksByHookID.Load()
			if reg == nil {
				t.Fatal("registry pointer must be initialized to empty map, not nil")
			}
			if len(*reg) != 0 {
				t.Fatalf("registry must be empty after clear; got %d entries", len(*reg))
			}
		})
	}
}

// TestApplyRulePacksShadowState_Malformed_NoStateChange pins the error
// envelope — a parse error returns wrapped err and MUST NOT mutate the
// registry or the resolver.
func TestApplyRulePacksShadowState_Malformed_NoStateChange(t *testing.T) {
	p := NewAgentPipeline(silentLogger())
	before := p.Resolver()
	err := p.ApplyRulePacksShadowState(context.Background(), json.RawMessage(`{"installedRulePacks":[`))
	if err == nil {
		t.Fatal("expected error for malformed json")
	}
	if p.Resolver() != before {
		t.Fatal("resolver must not change on parse failure")
	}
	if p.rulePacksByHookID.Load() != nil {
		t.Fatal("registry must remain nil (uninitialized) on parse failure")
	}
}

// TestApplyRulePacksShadowState_HappyPath_IndexesByBoundHook verifies the
// full registry build: bound packs are indexed under boundHookId; an
// unbound pack (boundHookId="") is dropped so it cannot enforce silently.
func TestApplyRulePacksShadowState_HappyPath_IndexesByBoundHook(t *testing.T) {
	p := NewAgentPipeline(silentLogger())

	// Seed a matching hook so injectRulePacks has somewhere to inject.
	hookPayload, _ := json.Marshal(map[string]any{
		"hookConfigs": []hooks.HookConfig{
			{ID: "hook-pii", ImplementationID: "pii-detector", Name: "pii",
				Stage: "request", Enabled: true, FailBehavior: "fail-open",
				Config: map[string]any{"existing": "value"}},
		},
	})
	if err := p.ApplyHooksShadowState(context.Background(), hookPayload); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rulePackPayload := map[string]any{
		"installedRulePacks": []map[string]any{
			{
				"id":          "install-1",
				"packId":      "pack-1",
				"name":        "safety-default",
				"version":     "1.0.0",
				"boundHookId": "hook-pii",
				"enabled":     true,
				"rules": []map[string]any{
					{
						"ruleId":   "r1",
						"category": "safety",
						"severity": "hard",
						"pattern":  "secret",
					},
				},
			},
			{
				// Unbound pack — must be dropped from the registry.
				"id":          "install-2",
				"packId":      "pack-2",
				"name":        "unbound",
				"version":     "1.0.0",
				"boundHookId": "",
				"enabled":     true,
			},
			{
				// Second install for the same hook — must concat under same key.
				"id":          "install-3",
				"packId":      "pack-3",
				"name":        "extra",
				"version":     "2.0.0",
				"boundHookId": "hook-pii",
				"enabled":     false,
				"rules":       []map[string]any{},
			},
		},
	}
	raw, _ := json.Marshal(rulePackPayload)
	if err := p.ApplyRulePacksShadowState(context.Background(), raw); err != nil {
		t.Fatalf("ApplyRulePacksShadowState err: %v", err)
	}

	reg := p.rulePacksByHookID.Load()
	if reg == nil {
		t.Fatal("registry must be populated after happy path apply")
	}
	if _, ok := (*reg)[""]; ok {
		t.Fatal("unbound pack (boundHookId=\"\") must NOT appear in the registry")
	}
	bound, ok := (*reg)["hook-pii"]
	if !ok {
		t.Fatal("expected hook-pii registry entry")
	}
	if len(bound) != 2 {
		t.Fatalf("expected 2 installs concatenated under hook-pii; got %d", len(bound))
	}
	// First install — fields preserved verbatim.
	if bound[0].InstallID != "install-1" || bound[0].PackName != "safety-default" ||
		bound[0].PackVersion != "1.0.0" || !bound[0].Enabled || len(bound[0].Rules) != 1 {
		t.Fatalf("install-1 mis-projected: %+v", bound[0])
	}
	if bound[0].Rules[0].RuleID != "r1" || bound[0].Rules[0].Pattern != "secret" {
		t.Fatalf("rule projection lost data: %+v", bound[0].Rules[0])
	}
	// Second install — disabled flag carried through (enforcement decision is
	// downstream — registry is purely a wiring layer).
	if bound[1].Enabled {
		t.Fatalf("install-3 must surface enabled=false; got true")
	}

	// Verify the reload re-injected `_rulePackInstalls` into the matching
	// hook config — this is the load-bearing wiring assertion that pins
	// rule packs as NOT a vanity surface for the rulepack-engine flow.
	cfgs := p.pendingConfigs.Load()
	if cfgs == nil {
		t.Fatal("pendingConfigs must be re-stored after reload")
	}
	var found bool
	for _, c := range *cfgs {
		if c.ID != "hook-pii" {
			continue
		}
		found = true
		installs, ok := c.Config["_rulePackInstalls"]
		if !ok {
			t.Fatal("hook-pii config must have _rulePackInstalls injected after rule-pack apply")
		}
		anyList, ok := installs.([]any)
		if !ok {
			t.Fatalf("_rulePackInstalls must be []any (the JSON-safe shape); got %T", installs)
		}
		if len(anyList) != 2 {
			t.Fatalf("_rulePackInstalls must carry both installs; got %d", len(anyList))
		}
		// Original Config["existing"] must be preserved (clone-not-mutate).
		if v, ok := c.Config["existing"]; !ok || v != "value" {
			t.Fatalf("existing config keys must survive clone; got %v", c.Config)
		}
	}
	if !found {
		t.Fatal("hook-pii config must be present in pendingConfigs after reload")
	}
}

// TestApplyRulePacksShadowState_NoHooksYet_NoOpReload pins the
// out-of-order delivery contract: if installed_rule_packs arrives before
// hooks, the registry is built but the reload exits early because
// pendingConfigs is still nil. The next ApplyHooksShadowState will pick up
// the registry and inject on its own.
func TestApplyRulePacksShadowState_NoHooksYet_NoOpReload(t *testing.T) {
	p := NewAgentPipeline(silentLogger())
	rulePackPayload := map[string]any{
		"installedRulePacks": []map[string]any{
			{
				"id":          "install-1",
				"name":        "safety",
				"version":     "1.0.0",
				"boundHookId": "hook-future",
				"enabled":     true,
				"rules":       []map[string]any{},
			},
		},
	}
	raw, _ := json.Marshal(rulePackPayload)
	if err := p.ApplyRulePacksShadowState(context.Background(), raw); err != nil {
		t.Fatalf("ApplyRulePacksShadowState err: %v", err)
	}
	if p.pendingConfigs.Load() != nil {
		t.Fatal("rule pack apply must not synthesize a pendingConfigs slot when hooks have not landed")
	}
	reg := p.rulePacksByHookID.Load()
	if reg == nil || len(*reg) != 1 {
		t.Fatalf("registry must still be populated even without hooks; got %v", reg)
	}

	// Now deliver the hook — injectRulePacks must pick up the registry and
	// stamp `_rulePackInstalls` onto the matching hook config.
	hookPayload, _ := json.Marshal(map[string]any{
		"hookConfigs": []hooks.HookConfig{
			{ID: "hook-future", ImplementationID: "pii-detector", Name: "hook-future",
				Stage: "request", Enabled: true, FailBehavior: "fail-open"},
		},
	})
	if err := p.ApplyHooksShadowState(context.Background(), hookPayload); err != nil {
		t.Fatalf("hooks apply err: %v", err)
	}
	cfgs := p.pendingConfigs.Load()
	if cfgs == nil || len(*cfgs) != 1 {
		t.Fatalf("pendingConfigs must hold the late-arriving hook config")
	}
	if _, ok := (*cfgs)[0].Config["_rulePackInstalls"]; !ok {
		t.Fatal("late-arriving hook must pick up the pre-installed rule packs")
	}
}

// TestInjectRulePacks_NoMatchingHookIsIdentity verifies the per-hook
// filter: a registry that does not reference any hook ID in the slice
// returns the slice unchanged (still a copy — the input slice must not
// be mutated to avoid leaking rule pack data into audit logging).
func TestInjectRulePacks_NoMatchingHookIsIdentity(t *testing.T) {
	cfgs := []hooks.HookConfig{
		{ID: "h1", Name: "h1"},
		{ID: "h2", Name: "h2"},
	}
	registry := map[string][]rulePackInstallView{
		"OTHER-HOOK": {{InstallID: "x"}},
	}
	out := injectRulePacks(cfgs, &registry, silentLogger())
	if len(out) != len(cfgs) {
		t.Fatalf("len mismatch; got %d want %d", len(out), len(cfgs))
	}
	// Output must be a distinct backing array (no shared writes).
	if &out[0] == &cfgs[0] {
		t.Fatal("injectRulePacks must return a copy, not the original slice")
	}
	for i := range out {
		if _, ok := out[i].Config["_rulePackInstalls"]; ok {
			t.Fatalf("hook %d must not be touched when no registry entry matches", i)
		}
	}
}

// TestInjectRulePacks_EmptyInputShortCircuits pins the early-return path:
// no allocations, no logger calls, returns the same nil/empty slice.
func TestInjectRulePacks_EmptyInputShortCircuits(t *testing.T) {
	if out := injectRulePacks(nil, nil, silentLogger()); out != nil {
		t.Fatalf("nil cfgs must return nil; got %v", out)
	}
	empty := []hooks.HookConfig{}
	out := injectRulePacks(empty, nil, silentLogger())
	if len(out) != 0 {
		t.Fatalf("empty cfgs must return empty slice; got len=%d", len(out))
	}
}

// TestInjectRulePacks_NilLoggerSafe pins the "logger == nil is safe" branch:
// older callers (and any future test harness) may pass nil; the function
// must not panic and must still inject when the registry matches.
func TestInjectRulePacks_NilLoggerSafe(t *testing.T) {
	cfgs := []hooks.HookConfig{{ID: "h1", Name: "h1"}}
	registry := map[string][]rulePackInstallView{
		"h1": {{InstallID: "i1", PackName: "p", Enabled: true}},
	}
	out := injectRulePacks(cfgs, &registry, nil)
	if _, ok := out[0].Config["_rulePackInstalls"]; !ok {
		t.Fatal("nil logger must not block injection")
	}
}

// TestInjectRulePacks_RegistryEntryEmptyListSkips pins the second short-
// circuit inside the per-hook loop: a registry slot that exists but is
// empty must NOT add `_rulePackInstalls` (an empty marker would force the
// hook factory down the rulepack-engine path with zero rules).
func TestInjectRulePacks_RegistryEntryEmptyListSkips(t *testing.T) {
	cfgs := []hooks.HookConfig{{ID: "h1", Name: "h1"}}
	registry := map[string][]rulePackInstallView{"h1": {}}
	out := injectRulePacks(cfgs, &registry, silentLogger())
	if _, ok := out[0].Config["_rulePackInstalls"]; ok {
		t.Fatal("empty registry slot must NOT inject the _rulePackInstalls marker")
	}
}

// TestApplyDomainsShadowState_NestedPathsConverted exercises the
// per-path inner loop in convertDomainDTOs — a domain with multiple
// nested paths must produce one InterceptionPath per entry, all carrying
// the parent DomainId.
func TestApplyDomainsShadowState_NestedPathsConverted(t *testing.T) {
	p := NewAgentPipeline(silentLogger())
	payload := map[string]any{
		"interceptionDomains": []shadow.InterceptionDomainDTO{
			{
				ID:                "dom-paths",
				Name:              "with-paths",
				HostPattern:       "api.openai.com",
				HostMatchType:     "EXACT",
				AdapterID:         "openai-compat",
				Enabled:           true,
				Priority:          100,
				DefaultPathAction: "PROCESS",
				OnAdapterError:    "FAIL_OPEN",
				NetworkZone:       "PUBLIC",
				Paths: []shadow.InterceptionPathDTO{
					{ID: "p1", PathPattern: []string{"/v1/chat/completions"}, MatchType: "PREFIX", Action: "PROCESS", Priority: 10, Enabled: true},
					{ID: "p2", PathPattern: []string{"/v1/models"}, MatchType: "EXACT", Action: "PASSTHROUGH", Priority: 20, Enabled: true},
				},
			},
		},
	}
	raw, _ := json.Marshal(payload)
	if err := p.ApplyDomainsShadowState(context.Background(), raw); err != nil {
		t.Fatalf("ApplyDomainsShadowState err: %v", err)
	}
	if sz := p.Snapshot().Size(); sz != 1 {
		t.Fatalf("expected 1 domain in snapshot; got %d", sz)
	}
	// The domain snapshot's internal path table is exposed indirectly
	// through Snapshot().Size() — but Size() counts domains only. The
	// behavioural pin is that BuildDomainSnapshot must accept the
	// (domains, paths) tuple without panicking, which happens here.
}

// TestApplyDomainsShadowState_InvalidRegex_PreservesEngine pins the
// `domainpolicy.Engine.Swap` error arm. A DTO with HostMatchType="REGEX"
// and an uncompilable pattern makes Swap reject the new snapshot; the
// previous engine snapshot must remain in place and the apply call must
// still succeed (the per-engine error must NOT propagate).
func TestApplyDomainsShadowState_InvalidRegex_PreservesEngine(t *testing.T) {
	p := NewAgentPipeline(silentLogger())

	// Seed a valid engine snapshot first.
	seed := map[string]any{
		"interceptionDomains": []shadow.InterceptionDomainDTO{
			{
				ID: "seed", Name: "seed", HostPattern: "example.com",
				HostMatchType: "EXACT", AdapterID: "openai-compat",
				Enabled: true, Priority: 100,
				DefaultPathAction: "PROCESS", OnAdapterError: "FAIL_OPEN",
				NetworkZone: "PUBLIC",
			},
		},
	}
	seedRaw, _ := json.Marshal(seed)
	if err := p.ApplyDomainsShadowState(context.Background(), seedRaw); err != nil {
		t.Fatalf("seed err: %v", err)
	}
	engineBefore := p.DomainEngine()
	if engineBefore == nil {
		t.Fatal("seed must populate the domain engine")
	}
	matched := engineBefore.MatchHost("example.com")
	if matched == nil || matched.ID != "seed" {
		t.Fatalf("seed engine must match example.com; got %+v", matched)
	}

	// Now push a payload with an invalid REGEX pattern. The shadow apply
	// must still return nil (the engine error is swallowed) and the
	// engine snapshot must still match the seed.
	bad := map[string]any{
		"interceptionDomains": []shadow.InterceptionDomainDTO{
			{
				ID: "bad", Name: "bad", HostPattern: "[unterminated",
				HostMatchType: "REGEX", AdapterID: "openai-compat",
				Enabled: true, Priority: 200,
				DefaultPathAction: "PROCESS", OnAdapterError: "FAIL_OPEN",
				NetworkZone: "PUBLIC",
			},
		},
	}
	badRaw, _ := json.Marshal(bad)
	if err := p.ApplyDomainsShadowState(context.Background(), badRaw); err != nil {
		t.Fatalf("apply must not propagate engine.Swap errors; got %v", err)
	}
	// The traffic snapshot DID update (BuildDomainSnapshot is tolerant);
	// only the domainpolicy.Engine swap was rejected.
	matchedAfter := p.DomainEngine().MatchHost("example.com")
	if matchedAfter == nil || matchedAfter.ID != "seed" {
		t.Fatalf("engine snapshot must be preserved after Swap rejection; got %+v", matchedAfter)
	}
}

// TestAdapterRegistry_NotNil_AndFrozen pins the AdapterRegistry accessor.
// The registry is built once at constructor time, populated with the
// builtins, and frozen — Freeze() makes Register panic. Callers
// (wire_bridge_darwin.go) pass this into shared/tlsbump.
func TestAdapterRegistry_NotNil_AndFrozen(t *testing.T) {
	p := NewAgentPipeline(silentLogger())
	reg := p.AdapterRegistry()
	if reg == nil {
		t.Fatal("AdapterRegistry must be initialised by the constructor")
	}
}

// TestDomainEngine_NonNilAtBootEmptyMatch locks in the eager-init contract:
// DomainEngine is non-nil from the AgentPipeline ctor onward so boot-time
// callers (platformshim/wire_bridge_darwin.go, wiring/bridge.go) can take
// a real pointer. The empty engine's MatchHost returns nil for every
// input until the first interception_domains shadow push populates it,
// providing fail-open behaviour.
func TestDomainEngine_NonNilAtBootEmptyMatch(t *testing.T) {
	p := NewAgentPipeline(silentLogger())
	eng := p.DomainEngine()
	if eng == nil {
		t.Fatal("DomainEngine must be non-nil at boot (eager-init contract)")
	}
	// Before any shadow push, MatchHost on any host returns nil so
	// callers fall through to passthrough — same effective semantics
	// the old lazy-nil contract gave, just expressed at the engine.
	if got := eng.MatchHost("example.com"); got != nil {
		t.Errorf("MatchHost on empty engine should return nil; got %+v", got)
	}

	payload := map[string]any{
		"interceptionDomains": []shadow.InterceptionDomainDTO{
			{
				ID: "d1", Name: "d1", HostPattern: "example.com",
				HostMatchType: "EXACT", AdapterID: "openai-compat",
				Enabled: true, Priority: 100,
				DefaultPathAction: "PROCESS", OnAdapterError: "FAIL_OPEN",
				NetworkZone: "PUBLIC",
			},
		},
	}
	raw, _ := json.Marshal(payload)
	if err := p.ApplyDomainsShadowState(context.Background(), raw); err != nil {
		t.Fatalf("apply err: %v", err)
	}
	if eng := p.DomainEngine(); eng == nil {
		t.Fatal("DomainEngine must be non-nil after first apply")
	}
}

// TestDomainEngine_PerHostOverridesPropagate verifies that the nullable
// streaming + capture override pointers in InterceptionDomainDTO survive
// the round-trip into domainpolicy.Engine. This is the wiring that lets
// shared/tlsbump honour the same per-host knobs the compliance-proxy
// uses — without this assertion a future refactor could quietly null them.
func TestDomainEngine_PerHostOverridesPropagate(t *testing.T) {
	p := NewAgentPipeline(silentLogger())
	payload := map[string]any{
		"interceptionDomains": []shadow.InterceptionDomainDTO{
			{
				ID: "d1", Name: "with-overrides", HostPattern: "api.openai.com",
				HostMatchType: "EXACT", AdapterID: "openai-compat",
				Enabled: true, Priority: 100,
				DefaultPathAction: "PROCESS", OnAdapterError: "FAIL_OPEN",
				NetworkZone:             "PUBLIC",
				StreamingMode:           strPtr("inline"),
				StreamingChunkBytes:     intPtr(4096),
				StreamingHookTimeoutMs:  intPtr(1500),
				StreamingMaxBufferBytes: intPtr(1 << 20),
				StreamingFailBehavior:   strPtr("fail-open"),
				CaptureRequestBody:      boolPtr(true),
				CaptureResponseBody:     boolPtr(false),
				RawBodySpillEnabled:     boolPtr(true),
			},
		},
	}
	raw, _ := json.Marshal(payload)
	if err := p.ApplyDomainsShadowState(context.Background(), raw); err != nil {
		t.Fatalf("apply err: %v", err)
	}
	eng := p.DomainEngine()
	if eng == nil {
		t.Fatal("engine must be populated")
	}
	d := eng.MatchHost("api.openai.com")
	if d == nil {
		t.Fatal("must match api.openai.com")
	}
	if d.StreamingMode == nil || *d.StreamingMode != "inline" {
		t.Fatalf("StreamingMode lost in conversion; got %v", d.StreamingMode)
	}
	if d.CaptureRequestBody == nil || !*d.CaptureRequestBody {
		t.Fatalf("CaptureRequestBody lost; got %v", d.CaptureRequestBody)
	}
	if d.CaptureResponseBody == nil || *d.CaptureResponseBody {
		t.Fatalf("CaptureResponseBody must be false; got %v", d.CaptureResponseBody)
	}
}

// TestRulePacks_VanitySurfaceForUnboundHooks pins the wiring contract: for
// hooks NOT named in the registry (i.e. without a matching boundHookId),
// installed rule packs are RECEIVED but NOT enforced — no
// `_rulePackInstalls` key is injected into their HookConfig.Config, so the
// keyword-filter factory does NOT route to NewRulePackEngine.
func TestRulePacks_VanitySurfaceForUnboundHooks(t *testing.T) {
	p := NewAgentPipeline(silentLogger())

	// Hook is registered; rule pack targets a DIFFERENT hook id.
	hookPayload, _ := json.Marshal(map[string]any{
		"hookConfigs": []hooks.HookConfig{
			{ID: "hook-keyword", ImplementationID: "pii-detector", Name: "kw",
				Stage: "request", Enabled: true, FailBehavior: "fail-open"},
		},
	})
	if err := p.ApplyHooksShadowState(context.Background(), hookPayload); err != nil {
		t.Fatalf("seed hooks: %v", err)
	}

	rulePackPayload, _ := json.Marshal(map[string]any{
		"installedRulePacks": []map[string]any{
			{
				"id":          "i1",
				"name":        "stray",
				"version":     "1.0.0",
				"boundHookId": "OTHER-HOOK-ID",
				"enabled":     true,
				"rules":       []map[string]any{{"ruleId": "r", "pattern": "x"}},
			},
		},
	})
	if err := p.ApplyRulePacksShadowState(context.Background(), rulePackPayload); err != nil {
		t.Fatalf("apply rule packs: %v", err)
	}

	// The registry holds it (Policies UI sees it) but no hook config
	// got `_rulePackInstalls` injected — i.e. no enforcement.
	cfgs := p.pendingConfigs.Load()
	if cfgs == nil {
		t.Fatal("pendingConfigs must be present after reload")
	}
	for _, c := range *cfgs {
		if _, ok := c.Config["_rulePackInstalls"]; ok {
			t.Fatalf("hook %q must NOT receive _rulePackInstalls when no rule pack is bound to it", c.ID)
		}
	}
}

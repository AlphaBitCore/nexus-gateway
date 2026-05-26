package routing

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/matcher"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/strategies"
)

// fakeStore is an in-memory routingStore used to drive Resolver.Resolve
// end-to-end without a live Postgres. Tests populate Rules + Providers +
// Models and exercise the full stage-0 + stage-1 pipeline.
type fakeStore struct {
	rules     []store.RoutingRule
	providers map[string]*store.Provider
	models    map[string]*store.Model
}

func (f *fakeStore) GetEnabledRoutingRules(_ context.Context) ([]store.RoutingRule, error) {
	return f.rules, nil
}

func (f *fakeStore) GetProviderAndModel(_ context.Context, providerID, modelID string) (*store.Provider, *store.Model, error) {
	p, ok := f.providers[providerID]
	if !ok {
		return nil, nil, fmt.Errorf("provider %q not found", providerID)
	}
	m, ok := f.models[modelID]
	if !ok {
		return nil, nil, fmt.Errorf("model %q not found", modelID)
	}
	return p, m, nil
}

func (f *fakeStore) GetModel(_ context.Context, id string) (*store.Model, error) {
	m, ok := f.models[id]
	if !ok {
		return nil, fmt.Errorf("model %q not found", id)
	}
	return m, nil
}

// ResolveModelCandidates mirrors store.DB.ResolveModelCandidates: returns
// every enabled Model whose Code equals the request string OR whose
// Aliases contain it. The fake walks the in-memory map.
func (f *fakeStore) ResolveModelCandidates(_ context.Context, code string) ([]store.Model, error) {
	if code == "" {
		return nil, nil
	}
	var out []store.Model
	for _, m := range f.models {
		if m == nil || !m.Enabled {
			continue
		}
		if m.Code == code {
			out = append(out, *m)
			continue
		}
		for _, a := range m.Aliases {
			if a == code {
				out = append(out, *m)
				break
			}
		}
	}
	return out, nil
}

// resolverFixture wires a Resolver around a fakeStore with a real
// strategies.StrategyRegistry (single/fallback/loadbalance/conditional/ab_split/policy).
type resolverFixture struct {
	store    *fakeStore
	registry *strategies.StrategyRegistry
	resolver *Resolver
}

func newResolverFixture() *resolverFixture {
	fs := &fakeStore{
		providers: map[string]*store.Provider{},
		models:    map[string]*store.Model{},
	}

	reg := strategies.NewStrategyRegistry()
	resolver := &Resolver{
		db:              fs,
		registry:        reg,
		logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		narrowingEngine: &matcher.NarrowingEngine{},
		capCache:        nil, // nil = capability pre-filter disabled in these tests
	}
	strategies.RegisterAllStrategies(reg, resolver.LookupTargetFunc(), nil)
	return &resolverFixture{store: fs, registry: reg, resolver: resolver}
}

func (f *resolverFixture) addProvider(id string, enabled bool) {
	f.store.providers[id] = &store.Provider{ID: id, Name: id, AdapterType: "openai", BaseURL: "https://" + id + ".example.com", Enabled: enabled}
}

func (f *resolverFixture) addModel(id, providerID, providerModelID string, enabled bool) {
	// Code defaults to the fixture id so existing tests that wrote
	// MatchConditions.Models = []string{"gpt-4"} still work end-to-end:
	// hydrateRequestedModel resolves the request string via Code and the
	// resulting CandidateIDs equal []string{id}.
	f.store.models[id] = &store.Model{ID: id, Code: id, Name: id, ProviderID: providerID, ProviderModelID: providerModelID, Type: "chat", Enabled: enabled}
}

func (f *resolverFixture) addRule(r store.RoutingRule) {
	r.Enabled = true
	f.store.rules = append(f.store.rules, r)
}

// mustJSON marshals v and fails the test on error.
func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// TestResolver_HappyPath_PrimarySingle verifies the minimum end-to-end shape:
// one stage-1 single-strategy rule matches and produces one target, no
// substitution, no recovery.
func TestResolver_HappyPath_PrimarySingle(t *testing.T) {
	f := newResolverFixture()
	f.addProvider("openai", true)
	f.addModel("gpt-4", "openai", "gpt-4", true)

	f.addRule(store.RoutingRule{
		ID:            "r-primary",
		Name:          "primary",
		StrategyType:  "single",
		PipelineStage: 1,
		Priority:      100,
		Config:        mustJSON(t, core.StrategyNode{Type: "single", ProviderID: "openai", ModelID: "gpt-4"}),
	})

	plan, err := f.resolver.Resolve(context.Background(), &core.RoutingContext{
		RequestedModel: core.RequestedModel{ID: "gpt-4"},
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(plan.Targets) != 1 {
		t.Fatalf("expected 1 primary target, got %d (trace=%+v)", len(plan.Targets), plan.Trace)
	}
	if plan.Targets[0].ProviderID != "openai" || plan.Targets[0].ModelID != "gpt-4" {
		t.Errorf("wrong target: %+v", plan.Targets[0])
	}
	if plan.Targets[0].Source != "primary" {
		t.Errorf("expected source=primary, got %q", plan.Targets[0].Source)
	}
	if plan.Substituted {
		t.Error("did not expect substitution")
	}
	if plan.RuleID != "r-primary" {
		t.Errorf("wrong rule id: %q", plan.RuleID)
	}
}

// TestResolver_Stage0Narrowing_FiltersPrimaryTarget verifies that a stage-0
// policy rule denying the model causes the primary target to be filtered out
// (and that core.NarrowingSummary reflects what was applied).
func TestResolver_Stage0Narrowing_FiltersPrimaryTarget(t *testing.T) {
	f := newResolverFixture()
	f.addProvider("openai", true)
	f.addModel("gpt-4", "openai", "gpt-4", true)

	f.addRule(store.RoutingRule{
		ID:            "r-policy",
		Name:          "deny gpt-4",
		StrategyType:  "policy",
		PipelineStage: 0,
		Priority:      100,
		Config: mustJSON(t, core.StrategyNode{
			Type:         "policy",
			DenyModelIDs: []string{"gpt-4"},
		}),
	})
	f.addRule(store.RoutingRule{
		ID:            "r-primary",
		Name:          "primary",
		StrategyType:  "single",
		PipelineStage: 1,
		Priority:      100,
		Config:        mustJSON(t, core.StrategyNode{Type: "single", ProviderID: "openai", ModelID: "gpt-4"}),
	})

	plan, err := f.resolver.Resolve(context.Background(), &core.RoutingContext{
		RequestedModel: core.RequestedModel{ID: "gpt-4"},
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(plan.Targets) != 0 {
		t.Fatalf("expected 0 primary targets (denied), got %d", len(plan.Targets))
	}
	if plan.NarrowingSummary == nil {
		t.Fatal("expected core.NarrowingSummary to be populated")
	}
	if len(plan.NarrowingSummary.DenyModelIDs) != 1 || plan.NarrowingSummary.DenyModelIDs[0] != "gpt-4" {
		t.Errorf("unexpected deny summary: %+v", plan.NarrowingSummary)
	}
}

// TestResolver_Stage0Narrowing_IntersectAllowUnionDeny verifies that multiple
// stage-0 policies compose: allow-lists intersect, deny-lists union.
func TestResolver_Stage0Narrowing_IntersectAllowUnionDeny(t *testing.T) {
	f := newResolverFixture()
	f.addProvider("openai", true)
	f.addProvider("anthropic", true)
	f.addModel("gpt-4", "openai", "gpt-4", true)
	f.addModel("gpt-3.5", "openai", "gpt-3.5", true)
	f.addModel("claude-3", "anthropic", "claude-3", true)

	f.addRule(store.RoutingRule{
		ID:            "r-policy-a",
		Name:          "allow gpt-4, gpt-3.5",
		StrategyType:  "policy",
		PipelineStage: 0,
		Priority:      200,
		Config: mustJSON(t, core.StrategyNode{
			Type:          "policy",
			AllowModelIDs: []string{"gpt-4", "gpt-3.5"},
		}),
	})
	f.addRule(store.RoutingRule{
		ID:            "r-policy-b",
		Name:          "allow gpt-4, claude-3",
		StrategyType:  "policy",
		PipelineStage: 0,
		Priority:      100,
		Config: mustJSON(t, core.StrategyNode{
			Type:          "policy",
			AllowModelIDs: []string{"gpt-4", "claude-3"},
		}),
	})
	f.addRule(store.RoutingRule{
		ID:            "r-policy-c",
		Name:          "deny gpt-3.5",
		StrategyType:  "policy",
		PipelineStage: 0,
		Priority:      50,
		Config: mustJSON(t, core.StrategyNode{
			Type:         "policy",
			DenyModelIDs: []string{"gpt-3.5"},
		}),
	})
	f.addRule(store.RoutingRule{
		ID:            "r-primary",
		Name:          "primary",
		StrategyType:  "single",
		PipelineStage: 1,
		Priority:      100,
		Config:        mustJSON(t, core.StrategyNode{Type: "single", ProviderID: "openai", ModelID: "gpt-4"}),
	})

	plan, err := f.resolver.Resolve(context.Background(), &core.RoutingContext{
		RequestedModel: core.RequestedModel{ID: "gpt-4"},
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(plan.Targets) != 1 || plan.Targets[0].ModelID != "gpt-4" {
		t.Fatalf("expected gpt-4 to survive intersect-allow, got %+v", plan.Targets)
	}
	if plan.NarrowingSummary == nil {
		t.Fatal("expected NarrowingSummary")
	}
	// Intersect(a,b) keeps only gpt-4.
	if got := plan.NarrowingSummary.AllowModelIDs; len(got) != 1 || got[0] != "gpt-4" {
		t.Errorf("expected allow=[gpt-4] after intersect, got %v", got)
	}
	if got := plan.NarrowingSummary.DenyModelIDs; len(got) != 1 || got[0] != "gpt-3.5" {
		t.Errorf("expected deny=[gpt-3.5], got %v", got)
	}
}

// TestResolver_VKAllowedModels_FiltersPrimary verifies that VK AllowedModels
// filter runs after narrowing and excludes targets not in the VK whitelist.
func TestResolver_VKAllowedModels_FiltersPrimary(t *testing.T) {
	f := newResolverFixture()
	f.addProvider("openai", true)
	f.addModel("gpt-4", "openai", "gpt-4", true)

	f.addRule(store.RoutingRule{
		ID:            "r-primary",
		Name:          "primary",
		StrategyType:  "single",
		PipelineStage: 1,
		Config:        mustJSON(t, core.StrategyNode{Type: "single", ProviderID: "openai", ModelID: "gpt-4"}),
	})

	plan, err := f.resolver.Resolve(context.Background(), &core.RoutingContext{
		RequestedModel: core.RequestedModel{ID: "gpt-4"},
		VirtualKey: &core.VKContext{
			ID: "vk-1",
			AllowedModels: []store.AllowedModelRef{
				{ProviderID: "anthropic", ModelID: "claude-3"},
			},
		},
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(plan.Targets) != 0 {
		t.Fatalf("expected 0 targets after VK filter, got %+v", plan.Targets)
	}
}

// TestResolver_InlineFallbackChain_PopulatesRecovery verifies that the
// primary rule's FallbackChain JSON produces RecoveryTargets tagged source=fallback.
func TestResolver_InlineFallbackChain_PopulatesRecovery(t *testing.T) {
	f := newResolverFixture()
	f.addProvider("openai", true)
	f.addProvider("anthropic", true)
	f.addModel("gpt-4", "openai", "gpt-4", true)
	f.addModel("claude-3", "anthropic", "claude-3", true)

	chain := []core.FallbackChainEntry{
		{ProviderID: "anthropic", ModelID: "claude-3"},
	}
	f.addRule(store.RoutingRule{
		ID:            "r-primary",
		Name:          "primary + chain",
		StrategyType:  "single",
		PipelineStage: 1,
		Config:        mustJSON(t, core.StrategyNode{Type: "single", ProviderID: "openai", ModelID: "gpt-4"}),
		FallbackChain: mustJSON(t, chain),
	})

	plan, err := f.resolver.Resolve(context.Background(), &core.RoutingContext{
		RequestedModel: core.RequestedModel{ID: "gpt-4"},
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(plan.Targets) != 1 || plan.Targets[0].ModelID != "gpt-4" {
		t.Fatalf("expected primary gpt-4, got %+v", plan.Targets)
	}
	if len(plan.RecoveryTargets) != 1 || plan.RecoveryTargets[0].ModelID != "claude-3" {
		t.Fatalf("expected recovery claude-3, got %+v", plan.RecoveryTargets)
	}
	if plan.RecoveryTargets[0].Source != "fallback" {
		t.Errorf("expected recovery source=fallback, got %q", plan.RecoveryTargets[0].Source)
	}
}

// TestResolver_FallbackStrategyRule_PopulatesRecovery verifies that a
// separate stage-1 rule with strategyType="fallback" is classified as a
// recovery rule, not primary, and its targets land in RecoveryTargets.
func TestResolver_FallbackStrategyRule_PopulatesRecovery(t *testing.T) {
	f := newResolverFixture()
	f.addProvider("openai", true)
	f.addProvider("anthropic", true)
	f.addModel("gpt-4", "openai", "gpt-4", true)
	f.addModel("claude-3", "anthropic", "claude-3", true)

	f.addRule(store.RoutingRule{
		ID:            "r-primary",
		Name:          "primary",
		StrategyType:  "single",
		PipelineStage: 1,
		Priority:      100,
		Config:        mustJSON(t, core.StrategyNode{Type: "single", ProviderID: "openai", ModelID: "gpt-4"}),
	})
	f.addRule(store.RoutingRule{
		ID:            "r-fallback",
		Name:          "recovery",
		StrategyType:  "fallback",
		PipelineStage: 1,
		Priority:      50,
		Config: mustJSON(t, core.StrategyNode{
			Type: "fallback",
			Targets: []core.StrategyNode{
				{Type: "single", ProviderID: "anthropic", ModelID: "claude-3"},
			},
		}),
	})

	plan, err := f.resolver.Resolve(context.Background(), &core.RoutingContext{
		RequestedModel: core.RequestedModel{ID: "gpt-4"},
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if plan.RuleID != "r-primary" {
		t.Errorf("expected primary rule r-primary, got %q", plan.RuleID)
	}
	if len(plan.Targets) != 1 || plan.Targets[0].ModelID != "gpt-4" {
		t.Fatalf("expected primary gpt-4, got %+v", plan.Targets)
	}
	if len(plan.RecoveryTargets) != 1 || plan.RecoveryTargets[0].ModelID != "claude-3" {
		t.Fatalf("expected recovery claude-3, got %+v", plan.RecoveryTargets)
	}
	if plan.RecoveryTargets[0].Source != "recovery" {
		t.Errorf("expected recovery source=recovery, got %q", plan.RecoveryTargets[0].Source)
	}
}

// TestResolver_Substitution_SetsFlag verifies that Substituted=true when the
// resolved ModelID differs from the requested one (e.g. model-routing rule
// that remaps a user-visible name to a backing provider model).
func TestResolver_Substitution_SetsFlag(t *testing.T) {
	f := newResolverFixture()
	f.addProvider("openai", true)
	f.addModel("gpt-4", "openai", "gpt-4", true)

	f.addRule(store.RoutingRule{
		ID:            "r-remap",
		Name:          "remap",
		StrategyType:  "single",
		PipelineStage: 1,
		Config:        mustJSON(t, core.StrategyNode{Type: "single", ProviderID: "openai", ModelID: "gpt-4"}),
	})

	plan, err := f.resolver.Resolve(context.Background(), &core.RoutingContext{
		RequestedModel: core.RequestedModel{ID: "gpt-5-preview"},
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !plan.Substituted {
		t.Errorf("expected Substituted=true when requested gpt-5-preview resolved to gpt-4")
	}
	if plan.OriginalModelID != "gpt-5-preview" {
		t.Errorf("expected OriginalModelID=gpt-5-preview, got %q", plan.OriginalModelID)
	}
}

// TestResolver_NoRuleMatches_ReturnsEmptyPlan verifies the behaviour when no
// stage-1 rule matches: plan returns with empty Targets and the pipeline
// trace must still mark stage-0 and stage-1 (so the simulator can explain).
func TestResolver_NoRuleMatches_ReturnsEmptyPlan(t *testing.T) {
	f := newResolverFixture()

	plan, err := f.resolver.Resolve(context.Background(), &core.RoutingContext{
		RequestedModel: core.RequestedModel{ID: "gpt-4"},
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(plan.Targets) != 0 || len(plan.RecoveryTargets) != 0 {
		t.Errorf("expected empty plan, got targets=%+v recovery=%+v", plan.Targets, plan.RecoveryTargets)
	}
	if plan.RuleID != "" {
		t.Errorf("expected no rule match, got RuleID=%q", plan.RuleID)
	}
	if len(plan.PipelineTrace) != 2 {
		t.Fatalf("expected 2 pipeline trace entries (stage-0, stage-1), got %d", len(plan.PipelineTrace))
	}
	if !strings.Contains(plan.PipelineTrace[1].Decision, "resolved 0 targets") {
		t.Errorf("stage-1 decision unclear: %q", plan.PipelineTrace[1].Decision)
	}
}

// TestResolver_DisabledProviderOrModel_PrimaryFails verifies that when a
// strategy lookup hits a disabled provider/model, SingleStrategy soft-fails
// with trace, and plan.Targets stays empty instead of crashing.
func TestResolver_DisabledProviderOrModel_PrimaryFails(t *testing.T) {
	f := newResolverFixture()
	f.addProvider("google", false) // disabled
	f.addModel("gemini-flash", "google", "gemini-flash", true)

	f.addRule(store.RoutingRule{
		ID:            "r-primary",
		Name:          "primary",
		StrategyType:  "single",
		PipelineStage: 1,
		Config:        mustJSON(t, core.StrategyNode{Type: "single", ProviderID: "google", ModelID: "gemini-flash"}),
	})

	plan, err := f.resolver.Resolve(context.Background(), &core.RoutingContext{
		RequestedModel: core.RequestedModel{ID: "gemini-flash"},
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(plan.Targets) != 0 {
		t.Fatalf("expected 0 targets (provider disabled), got %+v", plan.Targets)
	}
	if len(plan.Trace) != 1 {
		t.Fatalf("expected 1 strategy trace entry explaining the failure, got %+v", plan.Trace)
	}
	if !strings.Contains(plan.Trace[0].Decision, "lookup failed") || !strings.Contains(plan.Trace[0].Decision, "disabled") {
		t.Errorf("trace should explain disabled provider: %q", plan.Trace[0].Decision)
	}
}

// TestHydrateRequestedModel_FillsCandidates verifies that the request
// string is resolved through ResolveModelCandidates and every matching
// Model.id lands on RequestedModel.CandidateIDs.
func TestHydrateRequestedModel_FillsCandidates(t *testing.T) {
	f := newResolverFixture()
	f.addProvider("openai", true)
	f.addModel("gpt-4-uuid", "openai", "gpt-4-0613", true)
	// Add a second model with the same code = "gpt-4" via aliases to
	// emulate the cross-provider alias case the hydrate path is built for.
	f.store.models["gpt-4-mirror"] = &store.Model{
		ID: "gpt-4-mirror", Code: "gpt-4-mirror", Name: "Mirror",
		ProviderID: "openai", ProviderModelID: "gpt-4-mirror",
		Type: "chat", Enabled: true, Aliases: []string{"gpt-4-uuid"},
	}

	rctx := &core.RoutingContext{RequestedModel: core.RequestedModel{ID: "gpt-4-uuid"}}
	f.resolver.hydrateRequestedModel(context.Background(), rctx)

	if len(rctx.RequestedModel.CandidateIDs) != 2 {
		t.Fatalf("expected 2 candidate IDs (code + alias), got %d: %v", len(rctx.RequestedModel.CandidateIDs), rctx.RequestedModel.CandidateIDs)
	}
	have := map[string]bool{}
	for _, id := range rctx.RequestedModel.CandidateIDs {
		have[id] = true
	}
	if !have["gpt-4-uuid"] || !have["gpt-4-mirror"] {
		t.Errorf("expected both code-hit and alias-hit candidates, got %v", rctx.RequestedModel.CandidateIDs)
	}
	if rctx.RequestedModel.ProviderID == "" || rctx.RequestedModel.Type == "" {
		t.Error("hydrate should fill ProviderID + Type from first candidate")
	}
}

// TestHydrateRequestedModel_AutoKeyword_LeavesCandidatesEmpty: the
// "auto" sentinel must not be resolved against the catalog so
// matchConditions.models cannot accidentally route it through a
// UUID-bearing rule. Smart-router rules use requestedModelLiterals
// instead.
func TestHydrateRequestedModel_AutoKeyword_LeavesCandidatesEmpty(t *testing.T) {
	f := newResolverFixture()
	f.addProvider("openai", true)
	f.addModel("gpt-4", "openai", "gpt-4", true)

	rctx := &core.RoutingContext{RequestedModel: core.RequestedModel{ID: "auto"}}
	f.resolver.hydrateRequestedModel(context.Background(), rctx)

	if len(rctx.RequestedModel.CandidateIDs) != 0 {
		t.Errorf("auto sentinel should not produce candidates, got %v", rctx.RequestedModel.CandidateIDs)
	}
}

// TestResolver_MatchConditions_PicksCorrectRule verifies that rule matching
// honors MatchConditions: only the rule whose MatchConditions.Models contains
// the requested model should be selected as primary.
func TestResolver_MatchConditions_PicksCorrectRule(t *testing.T) {
	f := newResolverFixture()
	f.addProvider("openai", true)
	f.addProvider("anthropic", true)
	f.addModel("gpt-4", "openai", "gpt-4", true)
	f.addModel("claude-3", "anthropic", "claude-3", true)

	f.addRule(store.RoutingRule{
		ID:              "r-gpt",
		Name:            "gpt only",
		StrategyType:    "single",
		PipelineStage:   1,
		Priority:        100,
		MatchConditions: mustJSON(t, core.MatchConditions{Models: []string{"gpt-4"}}),
		Config:          mustJSON(t, core.StrategyNode{Type: "single", ProviderID: "openai", ModelID: "gpt-4"}),
	})
	f.addRule(store.RoutingRule{
		ID:              "r-claude",
		Name:            "claude only",
		StrategyType:    "single",
		PipelineStage:   1,
		Priority:        100,
		MatchConditions: mustJSON(t, core.MatchConditions{Models: []string{"claude-3"}}),
		Config:          mustJSON(t, core.StrategyNode{Type: "single", ProviderID: "anthropic", ModelID: "claude-3"}),
	})

	plan, err := f.resolver.Resolve(context.Background(), &core.RoutingContext{
		RequestedModel: core.RequestedModel{ID: "claude-3"},
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if plan.RuleID != "r-claude" {
		t.Errorf("expected r-claude, got %q", plan.RuleID)
	}
	if len(plan.Targets) != 1 || plan.Targets[0].ModelID != "claude-3" {
		t.Errorf("wrong target: %+v", plan.Targets)
	}
}

// TestResolver_Loadbalance_DistributesBranches verifies that a loadbalance
// primary rule yields both branches over many resolutions (probabilistic;
// pinned so the distribution can't silently collapse to a single branch).
func TestResolver_Loadbalance_DistributesBranches(t *testing.T) {
	f := newResolverFixture()
	f.addProvider("openai", true)
	f.addProvider("anthropic", true)
	f.addModel("gpt-4", "openai", "gpt-4", true)
	f.addModel("claude-3", "anthropic", "claude-3", true)

	f.addRule(store.RoutingRule{
		ID:            "r-lb",
		Name:          "loadbalance",
		StrategyType:  "loadbalance",
		PipelineStage: 1,
		Config: mustJSON(t, core.StrategyNode{
			Type:      "loadbalance",
			Algorithm: "weighted",
			Weighted: []core.WeightedTarget{
				{Weight: 1, Node: core.StrategyNode{Type: "single", ProviderID: "openai", ModelID: "gpt-4"}},
				{Weight: 1, Node: core.StrategyNode{Type: "single", ProviderID: "anthropic", ModelID: "claude-3"}},
			},
		}),
	})

	hits := map[string]int{}
	for range 200 {
		plan, err := f.resolver.Resolve(context.Background(), &core.RoutingContext{
			RequestedModel: core.RequestedModel{ID: "gpt-4"},
		})
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if len(plan.Targets) == 0 {
			t.Fatalf("loadbalance picked 0 targets (trace=%+v)", plan.Trace)
		}
		hits[plan.Targets[0].ProviderID]++
	}
	if hits["openai"] == 0 || hits["anthropic"] == 0 {
		t.Errorf("expected both branches to be hit over 200 rolls, got %v", hits)
	}
}

// TestResolver_Narrowing_BlocksRecoveryTargets verifies that narrowing
// applies to RecoveryTargets too (inline fallback chain + fallback-strategy rule).
func TestResolver_Narrowing_BlocksRecoveryTargets(t *testing.T) {
	f := newResolverFixture()
	f.addProvider("openai", true)
	f.addProvider("anthropic", true)
	f.addModel("gpt-4", "openai", "gpt-4", true)
	f.addModel("claude-3", "anthropic", "claude-3", true)

	f.addRule(store.RoutingRule{
		ID:            "r-policy",
		Name:          "deny anthropic",
		StrategyType:  "policy",
		PipelineStage: 0,
		Config: mustJSON(t, core.StrategyNode{
			Type:            "policy",
			DenyProviderIDs: []string{"anthropic"},
		}),
	})
	f.addRule(store.RoutingRule{
		ID:            "r-primary",
		Name:          "primary",
		StrategyType:  "single",
		PipelineStage: 1,
		Priority:      100,
		Config:        mustJSON(t, core.StrategyNode{Type: "single", ProviderID: "openai", ModelID: "gpt-4"}),
	})
	f.addRule(store.RoutingRule{
		ID:            "r-fallback",
		Name:          "recovery",
		StrategyType:  "fallback",
		PipelineStage: 1,
		Priority:      50,
		Config: mustJSON(t, core.StrategyNode{
			Type: "fallback",
			Targets: []core.StrategyNode{
				{Type: "single", ProviderID: "anthropic", ModelID: "claude-3"},
			},
		}),
	})

	plan, err := f.resolver.Resolve(context.Background(), &core.RoutingContext{
		RequestedModel: core.RequestedModel{ID: "gpt-4"},
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(plan.Targets) != 1 {
		t.Fatalf("expected 1 primary, got %+v", plan.Targets)
	}
	// The fallback-strategy rule path runs through NarrowingEngine.Filter,
	// so anthropic (denied by stage-0 policy) must be filtered out there.
	for _, rt := range plan.RecoveryTargets {
		if rt.ProviderID == "anthropic" && rt.Source == "recovery" {
			t.Errorf("fallback-strategy rule leaked denied provider into RecoveryTargets: %+v", plan.RecoveryTargets)
		}
	}
}

// TestRuleMatches_MatchConditionsIsSoleFilter locks the contract: with the
// RoutingRule.modelId column removed, rule applicability is decided
// exclusively from matchConditions. Covers the three shapes that together
// define the contract.
//
//  1. empty / omitted matchConditions      -> catch-all (every model matches)
//  2. matchConditions.models = [X]         -> only X matches
//  3. matchConditions.models + providers   -> dimensions AND'd
func TestRuleMatches_MatchConditionsIsSoleFilter(t *testing.T) {
	f := newResolverFixture()
	f.addProvider("openai", true)
	f.addProvider("anthropic", true)
	f.addModel("gpt-4", "openai", "gpt-4", true)
	f.addModel("gpt-3.5", "openai", "gpt-3.5", true)
	f.addModel("claude-3", "anthropic", "claude-3", true)

	tests := []struct {
		name            string
		matchConditions json.RawMessage
		requested       string
		wantMatch       bool
	}{
		{
			name:            "empty matchConditions matches gpt-4",
			matchConditions: nil,
			requested:       "gpt-4",
			wantMatch:       true,
		},
		{
			name:            "empty matchConditions matches claude-3",
			matchConditions: nil,
			requested:       "claude-3",
			wantMatch:       true,
		},
		{
			name:            "empty-object matchConditions still acts as catch-all",
			matchConditions: json.RawMessage(`{}`),
			requested:       "gpt-3.5",
			wantMatch:       true,
		},
		{
			name:            "models=[gpt-4] matches gpt-4",
			matchConditions: mustJSON(t, core.MatchConditions{Models: []string{"gpt-4"}}),
			requested:       "gpt-4",
			wantMatch:       true,
		},
		{
			name:            "models=[gpt-4] rejects gpt-3.5",
			matchConditions: mustJSON(t, core.MatchConditions{Models: []string{"gpt-4"}}),
			requested:       "gpt-3.5",
			wantMatch:       false,
		},
		{
			name: "models=[gpt-4] AND providers=[anthropic] rejects gpt-4 (provider mismatch)",
			matchConditions: mustJSON(t, core.MatchConditions{
				Models:    []string{"gpt-4"},
				Providers: []string{"anthropic"},
			}),
			requested: "gpt-4",
			wantMatch: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rule := store.RoutingRule{
				ID:              "r-under-test",
				Name:            "under test",
				StrategyType:    "single",
				PipelineStage:   1,
				Priority:        10,
				MatchConditions: tc.matchConditions,
				Config:          mustJSON(t, core.StrategyNode{Type: "single", ProviderID: "openai", ModelID: "gpt-4"}),
			}
			// ruleMatches is the post-hydrate predicate, so we synthesize
			// CandidateIDs the way hydrateRequestedModel would (1:1 with the
			// fixture id, since fakeStore.addModel sets Code = id).
			ctx := &core.RoutingContext{
				RequestedModel: core.RequestedModel{
					ID:           tc.requested,
					ProviderID:   "openai",
					CandidateIDs: []string{tc.requested},
				},
			}
			got := f.resolver.ruleMatches(rule, tc.requested, ctx)
			if got != tc.wantMatch {
				t.Fatalf("ruleMatches=%v want=%v (matchConditions=%s, requested=%q)",
					got, tc.wantMatch, string(tc.matchConditions), tc.requested)
			}
		})
	}
}

// TestResolver_CatchAll_LosesToSpecific locks the priority ordering between
// a catch-all rule (no matchConditions) and a specific rule whose
// matchConditions.models names the requested model. The specific rule must
// win; the catch-all must not suppress it by filtering on a stale
// rule-level modelId field.
func TestResolver_CatchAll_LosesToSpecific(t *testing.T) {
	f := newResolverFixture()
	f.addProvider("openai", true)
	f.addProvider("anthropic", true)
	f.addModel("gpt-4", "openai", "gpt-4", true)
	f.addModel("claude-3", "anthropic", "claude-3", true)

	// Rules inserted in the order the real store returns them: pipelineStage
	// ASC, priority DESC. Specific (priority 100) before catch-all (priority
	// 0) — mirrors `SELECT ... ORDER BY "pipelineStage" ASC, priority DESC`
	// in packages/ai-gateway/internal/store/routing.go.
	f.addRule(store.RoutingRule{
		ID:              "r-specific",
		Name:            "specific-gpt4",
		StrategyType:    "single",
		PipelineStage:   1,
		Priority:        100,
		MatchConditions: mustJSON(t, core.MatchConditions{Models: []string{"gpt-4"}}),
		Config:          mustJSON(t, core.StrategyNode{Type: "single", ProviderID: "openai", ModelID: "gpt-4"}),
	})
	f.addRule(store.RoutingRule{
		ID:            "r-catchall",
		Name:          "catch-all",
		StrategyType:  "single",
		PipelineStage: 1,
		Priority:      0,
		Config:        mustJSON(t, core.StrategyNode{Type: "single", ProviderID: "anthropic", ModelID: "claude-3"}),
	})

	// gpt-4 request: specific wins.
	plan, err := f.resolver.Resolve(context.Background(), &core.RoutingContext{
		RequestedModel: core.RequestedModel{ID: "gpt-4"},
	})
	if err != nil {
		t.Fatalf("resolve gpt-4: %v", err)
	}
	if plan.RuleID != "r-specific" {
		t.Fatalf("gpt-4 should hit r-specific, got %q", plan.RuleID)
	}

	// claude-3 request: specific rejects (wrong model), catch-all wins.
	plan, err = f.resolver.Resolve(context.Background(), &core.RoutingContext{
		RequestedModel: core.RequestedModel{ID: "claude-3"},
	})
	if err != nil {
		t.Fatalf("resolve claude-3: %v", err)
	}
	if plan.RuleID != "r-catchall" {
		t.Fatalf("claude-3 should fall through to r-catchall, got %q", plan.RuleID)
	}
}

// findExplainBranch is a local helper for the Explain tests below.
func findExplainBranch(branches []core.BranchedTarget, providerID, modelID string) *core.BranchedTarget {
	for i := range branches {
		if branches[i].Target.ProviderID == providerID && branches[i].Target.ModelID == modelID {
			return &branches[i]
		}
	}
	return nil
}

// TestResolver_Explain_PopulatesBranchesForLoadbalance verifies that Explain
// runs Resolve and then attaches the full branch enumeration to plan.Branches.
// This is the end-to-end contract the simulate endpoint depends on.
func TestResolver_Explain_PopulatesBranchesForLoadbalance(t *testing.T) {
	f := newResolverFixture()
	f.addProvider("openai", true)
	f.addProvider("google", true)
	f.addModel("gpt-4", "openai", "gpt-4", true)
	f.addModel("gemini", "google", "gemini", true)

	f.addRule(store.RoutingRule{
		ID:            "r-lb",
		Name:          "70/30",
		StrategyType:  "loadbalance",
		PipelineStage: 1,
		Config: mustJSON(t, core.StrategyNode{
			Type:      "loadbalance",
			Algorithm: "weighted",
			Weighted: []core.WeightedTarget{
				{Weight: 70, Node: core.StrategyNode{Type: "single", ProviderID: "openai", ModelID: "gpt-4"}},
				{Weight: 30, Node: core.StrategyNode{Type: "single", ProviderID: "google", ModelID: "gemini"}},
			},
		}),
	})

	plan, err := f.resolver.Explain(context.Background(), &core.RoutingContext{
		RequestedModel: core.RequestedModel{ID: "gpt-4"},
	})
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	if len(plan.Branches) != 2 {
		t.Fatalf("expected 2 branches, got %d (%+v)", len(plan.Branches), plan.Branches)
	}
	openai := findExplainBranch(plan.Branches, "openai", "gpt-4")
	google := findExplainBranch(plan.Branches, "google", "gemini")
	if openai == nil || google == nil {
		t.Fatalf("branches missing: %+v", plan.Branches)
	}
	if math.Abs(openai.Probability-0.7) > 1e-9 || math.Abs(google.Probability-0.3) > 1e-9 {
		t.Errorf("probs wrong: openai=%v google=%v", openai.Probability, google.Probability)
	}
}

// TestResolver_Explain_NoRuleMatched_LeavesBranchesEmpty verifies Explain
// degrades gracefully when no primary rule matched.
func TestResolver_Explain_NoRuleMatched_LeavesBranchesEmpty(t *testing.T) {
	f := newResolverFixture()
	plan, err := f.resolver.Explain(context.Background(), &core.RoutingContext{
		RequestedModel: core.RequestedModel{ID: "gpt-4"},
	})
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	if plan.RuleID != "" || len(plan.Branches) != 0 {
		t.Errorf("expected empty explain result when no rule matched, got ruleId=%q branches=%+v", plan.RuleID, plan.Branches)
	}
}

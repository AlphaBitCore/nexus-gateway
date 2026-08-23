package routing

import (
	"context"
	"errors"
	"fmt"
	"github.com/goccy/go-json"
	"io"
	"log/slog"
	"math"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/matcher"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/strategies"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// fakeStore is an in-memory routingStore used to drive Resolver.Resolve
// end-to-end without a live Postgres. Tests populate Rules + Providers +
// Models and exercise the full stage-0 + stage-1 pipeline.
type fakeStore struct {
	rules     []store.RoutingRule
	providers map[string]*store.Provider
	models    map[string]*store.Model
	// candidatesErr makes ResolveModelCandidates fail, so a test can drive the
	// catalogue-blip path rather than assert against a hand-built context.
	candidatesErr error
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
	if f.candidatesErr != nil {
		return nil, f.candidatesErr
	}
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
		db:       fs,
		registry: reg,
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		vkAccess: matcher.VKAccessFilter{},
		capCache: nil, // nil = capability pre-filter disabled in these tests
	}
	strategies.RegisterAllStrategies(reg, resolver.LookupTargetFunc(), nil, nil)
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
	f.store.models[id] = &store.Model{ID: id, Code: id, Name: id, ProviderID: providerID, ProviderName: providerID, ProviderModelID: providerModelID, Type: "chat", Enabled: enabled}
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

// TestResolver_ANamedModelTheKeyCannotUseIsRefused.
//
// The caller pinned a model. Their key does not permit it. Serving the request
// anyway — because a rule would redirect it to something the key CAN use — is
// how a client hard-coded to one model got 200s from another: the client's own
// configuration silently overridden, every response attributed to a model the
// key cannot use, and nothing in the exchange saying so.
//
// This is a deliberate reversal. The access filter's own comment argued against
// it, and correctly, for the UNCONDITIONAL version — see the sibling test.
func TestResolver_ANamedModelTheKeyCannotUseIsRefused(t *testing.T) {
	f := newResolverFixture()
	f.addProvider("openai", true)
	f.addModel("gpt-4", "openai", "gpt-4", true)

	f.addRule(store.RoutingRule{
		ID: "r-primary", Name: "primary", StrategyType: "single", PipelineStage: 1,
		Config: mustJSON(t, core.StrategyNode{Type: "single", ProviderID: "openai", ModelID: "gpt-4"}),
	})

	_, err := f.resolver.Resolve(context.Background(), &core.RoutingContext{
		RequestedModel: core.RequestedModel{ID: "gpt-4", CandidateIDs: []string{"gpt-4"}},
		VirtualKey: &core.VKContext{
			ID: "vk-1",
			AllowedModels: []store.AllowedModelRef{
				{ProviderID: "anthropic", ModelID: "claude-3"},
			},
		},
	})
	var notAllowed *core.ModelNotAllowedError
	if !errors.As(err, &notAllowed) {
		t.Fatalf("err = %v, want a model-not-allowed refusal — the caller named a model this "+
			"key may not use and the request was served regardless", err)
	}
	if notAllowed.RequestedModel != "gpt-4" {
		t.Errorf("the refusal names %q, want the model the caller actually sent — an error "+
			"naming the redirect target tells them to fix something they never asked for",
			notAllowed.RequestedModel)
	}
}

// lookupSpy records the model ids the resolver asked the catalogue about.
type lookupSpy struct {
	*fakeStore
	asked []string
}

func (l *lookupSpy) GetModel(ctx context.Context, id string) (*store.Model, error) {
	l.asked = append(l.asked, id)
	return l.fakeStore.GetModel(ctx, id)
}

// TestResolver_AnAutoRequestFromARestrictedKeyStillRoutes.
//
// The other half, and the reason the rule above is conditional. `auto` is not a
// catalog model and can never appear on an allow list; a code that fans out to
// several providers has no single reference to match. Requiring either to be
// allowed refuses every routed request from every restricted key — it deletes
// routing rather than tightening it.
//
// The target side still holds: the rule's answer is filtered to what the key
// may use, so a restricted key routing `auto` reaches only its own models.
func TestResolver_AnAutoRequestFromARestrictedKeyStillRoutes(t *testing.T) {
	f := newResolverFixture()
	f.addProvider("openai", true)
	f.addProvider("anthropic", true)
	f.addModel("gpt-4", "openai", "gpt-4", true)
	f.addModel("claude-3", "anthropic", "claude-3", true)

	f.addRule(store.RoutingRule{
		ID: "r-allowed", Name: "to the allowed model", StrategyType: "single", PipelineStage: 1,
		Priority: 100,
		Config:   mustJSON(t, core.StrategyNode{Type: "single", ProviderID: "anthropic", ModelID: "claude-3"}),
	})

	spy := &lookupSpy{fakeStore: f.store}
	f.resolver.db = spy

	plan, err := f.resolver.Resolve(context.Background(), &core.RoutingContext{
		RequestedModel: core.RequestedModel{ID: "auto"},
		VirtualKey: &core.VKContext{
			ID:            "vk-1",
			AllowedModels: []store.AllowedModelRef{{ProviderID: "anthropic", ModelID: "claude-3"}},
		},
	})
	if err != nil {
		t.Fatalf("an auto request from a restricted key was refused: %v — `auto` can never be "+
			"on an allow list, so this refuses every routed request the key makes", err)
	}
	if len(plan.Targets) != 1 || plan.Targets[0].ModelID != "claude-3" {
		t.Errorf("targets = %+v, want the one model this key may use", plan.Targets)
	}
	// The guard must not even ASK about "auto".
	//
	// Asserting only that the request succeeded would pass for the wrong
	// reason: looking up a model id that does not exist fails, and the guard
	// keeps the request on a lookup failure — correctly, since an unreadable
	// catalogue is not evidence a key lacks permission. So an unconditional
	// guard would ask about "auto", get nothing, and let it through anyway,
	// leaving this test green while the condition it exists for was gone.
	for _, id := range spy.asked {
		if id == "auto" {
			t.Errorf("the access guard asked the catalogue about %q — it is running for a "+
				"request that named no model, and it survives here only because the lookup "+
				"happens to fail. A code that fans out to several providers resolves, and "+
				"that one would be refused.", id)
		}
	}
}

// TestResolver_AutoRoutedToAModelTheKeyLacksYieldsNothing.
//
// The target-side filter, unchanged by the tightening above. When WE choose the
// model, a choice the key cannot use is removed from the plan rather than
// served — the rule yields, and the rules below it get their turn.
func TestResolver_AutoRoutedToAModelTheKeyLacksYieldsNothing(t *testing.T) {
	f := newResolverFixture()
	f.addProvider("openai", true)
	f.addModel("gpt-4", "openai", "gpt-4", true)

	f.addRule(store.RoutingRule{
		ID: "r-primary", Name: "to a model this key lacks", StrategyType: "single", PipelineStage: 1,
		Config: mustJSON(t, core.StrategyNode{Type: "single", ProviderID: "openai", ModelID: "gpt-4"}),
	})

	plan, err := f.resolver.Resolve(context.Background(), &core.RoutingContext{
		RequestedModel: core.RequestedModel{ID: "auto"},
		VirtualKey: &core.VKContext{
			ID:            "vk-1",
			AllowedModels: []store.AllowedModelRef{{ProviderID: "anthropic", ModelID: "claude-3"}},
		},
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(plan.Targets) != 0 {
		t.Fatalf("targets = %+v, want none — the rule chose a model this key cannot use, and "+
			"dispatching it would spend the key's own credential on a model it is denied",
			plan.Targets)
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

// TestResolver_InlineFallbackChain_RespectsVKAllowedModels is the
// regression: an inline FallbackChain entry pointing at a (provider, model)
// OUTSIDE the VK's allowedModels must be filtered out of RecoveryTargets — the
// same allowlist gate the primary path enforces — so a primary failure cannot
// dispatch (and bill / consume the upstream credential of) a model the VK is not
// permitted to use. Before the fix the inline FallbackChain skipped the filter.
func TestResolver_InlineFallbackChain_RespectsVKAllowedModels(t *testing.T) {
	f := newResolverFixture()
	f.addProvider("openai", true)
	f.addProvider("anthropic", true)
	f.addModel("gpt-4", "openai", "gpt-4", true)
	f.addModel("claude-3", "anthropic", "claude-3", true)

	chain := []core.FallbackChainEntry{
		{ProviderID: "anthropic", ModelID: "claude-3"}, // NOT in the VK allowlist
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
		VirtualKey: &core.VKContext{
			ID: "vk-restricted",
			AllowedModels: []store.AllowedModelRef{
				{ProviderID: "openai", ModelID: "gpt-4"}, // only gpt-4 permitted
			},
		},
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(plan.Targets) != 1 || plan.Targets[0].ModelID != "gpt-4" {
		t.Fatalf("expected primary gpt-4 (in allowlist), got %+v", plan.Targets)
	}
	for _, rt := range plan.RecoveryTargets {
		if rt.ModelID == "claude-3" {
			t.Errorf("SEC-C1-01: inline FallbackChain leaked a non-allowlisted model into RecoveryTargets: %+v", plan.RecoveryTargets)
		}
	}
}

// TestResolver_InlineFallbackChain_KeepsAllowlistedEntry is the positive control
// for the inline-fallback-chain allowlist case: a FallbackChain entry that IS within the VK allowlist must still
// survive the new filter (no over-blocking).
func TestResolver_InlineFallbackChain_KeepsAllowlistedEntry(t *testing.T) {
	f := newResolverFixture()
	f.addProvider("openai", true)
	f.addProvider("anthropic", true)
	f.addModel("gpt-4", "openai", "gpt-4", true)
	f.addModel("claude-3", "anthropic", "claude-3", true)

	chain := []core.FallbackChainEntry{{ProviderID: "anthropic", ModelID: "claude-3"}}
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
		VirtualKey: &core.VKContext{
			ID: "vk-broad",
			AllowedModels: []store.AllowedModelRef{
				{ProviderID: "openai", ModelID: "gpt-4"},
				{ProviderID: "anthropic", ModelID: "claude-3"}, // fallback permitted
			},
		},
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(plan.RecoveryTargets) != 1 || plan.RecoveryTargets[0].ModelID != "claude-3" {
		t.Fatalf("allowlisted fallback claude-3 must be kept, got %+v", plan.RecoveryTargets)
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
	// One entry, not two: stage 0 was policy narrowing and is gone with it.
	if len(plan.PipelineTrace) != 1 {
		t.Fatalf("expected 1 pipeline trace entry (stage-1), got %d", len(plan.PipelineTrace))
	}
	if !strings.Contains(plan.PipelineTrace[0].Decision, "resolved 0 targets") {
		t.Errorf("stage-1 decision unclear: %q", plan.PipelineTrace[0].Decision)
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

// TestResolveTargets_RequestedSideIdentity locks the traffic_event REQUESTED-side
// write decision: model_id/provider_id/provider_name are populated only when the
// client requested a specific model that resolves to exactly one catalog model;
// "auto" and multi-provider codes leave them empty.
func TestResolveTargets_RequestedSideIdentity(t *testing.T) {
	ctx := context.Background()

	t.Run("specific single-candidate model → requested set", func(t *testing.T) {
		f := newResolverFixture()
		f.addProvider("openai", true)
		f.addModel("gpt-4", "openai", "gpt-4", true)
		f.addRule(store.RoutingRule{
			ID: "r", Name: "r", StrategyType: "single", PipelineStage: 1, Priority: 100,
			Config: mustJSON(t, core.StrategyNode{Type: "single", ProviderID: "openai", ModelID: "gpt-4"}),
		})
		rr, err := f.resolver.ResolveTargets(ctx, &core.RoutingContext{RequestedModel: core.RequestedModel{ID: "gpt-4"}})
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if rr.RequestedModelID != "gpt-4" || rr.RequestedProviderID != "openai" || rr.RequestedProviderName != "openai" {
			t.Errorf("requested side = %q/%q/%q, want gpt-4/openai/openai", rr.RequestedModelID, rr.RequestedProviderID, rr.RequestedProviderName)
		}
	})

	t.Run("auto → requested empty", func(t *testing.T) {
		f := newResolverFixture()
		f.addProvider("openai", true)
		f.addModel("gpt-4", "openai", "gpt-4", true)
		rr, err := f.resolver.ResolveTargets(ctx, &core.RoutingContext{RequestedModel: core.RequestedModel{ID: "auto"}})
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if rr.RequestedModelID != "" || rr.RequestedProviderID != "" || rr.RequestedProviderName != "" {
			t.Errorf("auto requested side = %q/%q/%q, want all empty", rr.RequestedModelID, rr.RequestedProviderID, rr.RequestedProviderName)
		}
	})

	t.Run("multi-provider code → requested empty (ambiguous)", func(t *testing.T) {
		f := newResolverFixture()
		f.addProvider("openai", true)
		f.addProvider("anthropic", true)
		// Same Code under two providers → ResolveModelCandidates returns 2.
		f.store.models["m1"] = &store.Model{ID: "m1", Code: "shared", Name: "shared", ProviderID: "openai", ProviderName: "openai", Type: "chat", Enabled: true}
		f.store.models["m2"] = &store.Model{ID: "m2", Code: "shared", Name: "shared", ProviderID: "anthropic", ProviderName: "anthropic", Type: "chat", Enabled: true}
		rr, err := f.resolver.ResolveTargets(ctx, &core.RoutingContext{RequestedModel: core.RequestedModel{ID: "shared"}})
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if rr.RequestedModelID != "" {
			t.Errorf("multi-candidate requested model id = %q, want empty", rr.RequestedModelID)
		}
	})
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

// TestResolve_APolicyRuleDoesNotSwallowTheRulesBelowIt.
//
// The `policy` strategy returns no targets and no error. It was written as a
// placeholder for a stage-0 narrowing engine that was never built, and left
// inert it would be harmless — but it is not inert.
//
// It is not a `fallback` rule, so it competes for the primary slot like any
// other. Winning it, it produces nothing and reports nothing: the resolver's
// warning fires only on an error, and it returned none. Every lower-priority
// rule is then locked out, and the request falls through to the requested-model
// passthrough — one rule silently disabling all the routing beneath it, with
// nothing shown to the operator.
func TestResolve_APolicyRuleDoesNotSwallowTheRulesBelowIt(t *testing.T) {
	f := newResolverFixture()
	f.addProvider("p1", true)
	f.addModel("m1", "p1", "gpt-4o", true)

	// Ordered as the resolver receives them: highest priority first.
	f.addRule(store.RoutingRule{
		ID: "r-policy", Name: "narrow", StrategyType: "policy",
		Priority: 100, PipelineStage: 1,
		Config: json.RawMessage(`{"type":"policy","denyProviderIds":["p-bad"]}`),
	})
	f.addRule(store.RoutingRule{
		ID: "r-real", Name: "the actual routing", StrategyType: "single",
		Priority: 50, PipelineStage: 1,
		Config: json.RawMessage(`{"type":"single","providerId":"p1","modelId":"m1"}`),
	})

	plan, err := f.resolver.Resolve(context.Background(), &core.RoutingContext{
		RequestedModel: core.RequestedModel{ID: "auto"},
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(plan.Targets) == 0 {
		t.Fatalf("the policy rule took the primary slot and produced nothing, so the rule "+
			"below it never ran — routing quietly stops working with no error anywhere "+
			"(matched rule: %q)", plan.RuleName)
	}
	if plan.RuleID != "r-real" {
		t.Errorf("served by rule %q, want the rule that actually resolves a target", plan.RuleID)
	}
}

// TestResolve_ARuleThatResolvesNothingYieldsTheSlot.
//
// The dangerous shape is not the rule that errors — that one is loud. It is the
// rule that evaluates perfectly and produces nothing: a `single` whose model was
// disabled last week, or one whose every target this virtual key cannot reach.
// Nothing is wrong from the strategy's point of view, so nothing is reported,
// and holding the primary slot on that basis disables every rule beneath it.
//
// The virtual-key case is the sharper one, because the rule works for most
// callers and silently removes routing for exactly the keys that were narrowed.
func TestResolve_ARuleThatResolvesNothingYieldsTheSlot(t *testing.T) {
	t.Run("a rule pointing at a disabled model", func(t *testing.T) {
		f := newResolverFixture()
		f.addProvider("p1", true)
		f.addModel("gone", "p1", "retired", false) // disabled
		f.addModel("live", "p1", "gpt-4o", true)

		f.addRule(store.RoutingRule{
			ID: "r-stale", Name: "points at a retired model", StrategyType: "single",
			Priority: 100, PipelineStage: 1,
			Config: json.RawMessage(`{"type":"single","providerId":"p1","modelId":"gone"}`),
		})
		f.addRule(store.RoutingRule{
			ID: "r-live", Name: "still works", StrategyType: "single",
			Priority: 50, PipelineStage: 1,
			Config: json.RawMessage(`{"type":"single","providerId":"p1","modelId":"live"}`),
		})

		plan, err := f.resolver.Resolve(context.Background(), &core.RoutingContext{
			RequestedModel: core.RequestedModel{ID: "auto"},
		})
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if plan.RuleID != "r-live" {
			t.Fatalf("served by rule %q with %d targets — a rule that resolves nothing held "+
				"the slot and disabled the one below it, with no error to read",
				plan.RuleID, len(plan.Targets))
		}
	})

	t.Run("a rule whose targets this key cannot reach", func(t *testing.T) {
		f := newResolverFixture()
		f.addProvider("p1", true)
		f.addModel("restricted", "p1", "gpt-4o", true)
		f.addModel("allowed", "p1", "gpt-4o-mini", true)

		f.addRule(store.RoutingRule{
			ID: "r-restricted", Name: "the key cannot reach this", StrategyType: "single",
			Priority: 100, PipelineStage: 1,
			Config: json.RawMessage(`{"type":"single","providerId":"p1","modelId":"restricted"}`),
		})
		f.addRule(store.RoutingRule{
			ID: "r-allowed", Name: "the key can reach this", StrategyType: "single",
			Priority: 50, PipelineStage: 1,
			Config: json.RawMessage(`{"type":"single","providerId":"p1","modelId":"allowed"}`),
		})

		plan, err := f.resolver.Resolve(context.Background(), &core.RoutingContext{
			RequestedModel: core.RequestedModel{ID: "auto"},
			VirtualKey: &core.VKContext{
				AllowedModels: []store.AllowedModelRef{{ProviderID: "p1", ModelID: "allowed"}},
			},
		})
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if plan.RuleID != "r-allowed" {
			t.Fatalf("served by rule %q with %d targets — the rule resolved a target this key "+
				"cannot be served, then held the slot; routing disappears for exactly the keys "+
				"that were narrowed, and works for everyone else", plan.RuleID, len(plan.Targets))
		}
	})
}

// TestResolve_TheModelPoolIsReadOnce.
//
// "Which models could serve this request" is a fact about the request, so it is
// established where the request context is prepared and every strategy that
// needs it reads that one answer.
//
// It used to be asked twice. The smart strategy fetched the catalogue itself
// and applied the virtual key's allowlist in its own loop, alongside the
// resolver's. Two readers of one snapshot is how the defects this program keeps
// finding begin — not because a second read is slow, but because the two can
// answer differently, and the one that disagrees is invisible until a request
// lands on the difference.
func TestResolve_TheModelPoolIsReadOnce(t *testing.T) {
	f := newResolverFixture()
	f.addProvider("p1", true)
	f.addModel("m1", "p1", "gpt-4o", true)
	f.addRule(store.RoutingRule{
		ID: "r1", Name: "smart", StrategyType: "single", PipelineStage: 1,
		Config: json.RawMessage(`{"type":"single","providerId":"p1","modelId":"m1"}`),
	})

	counting := &countingPool{rows: []core.SmartModelRow{
		{ModelID: "m1", ProviderID: "p1", ProviderModelID: "gpt-4o"},
	}}
	r := f.resolver.WithModelPool(counting)

	rctx := &core.RoutingContext{RequestedModel: core.RequestedModel{ID: "auto"}}
	if _, err := r.Resolve(context.Background(), rctx); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if counting.calls != 1 {
		t.Errorf("the catalogue was read %d times for one request — every extra read is a "+
			"second answer to a settled question", counting.calls)
	}
	if len(rctx.ModelPool) != 1 {
		t.Errorf("the prepared context carries %d models; a strategy reading it would fall "+
			"back to its own fetch and narrow it a second time", len(rctx.ModelPool))
	}
}

// countingPool records how many times the catalogue is asked, and for which
// endpoint kinds — a pool prepared for chat and consumed by an image request
// reads as one call but answers a different question.
type countingPool struct {
	rows  []core.SmartModelRow
	calls int
	kinds []typology.EndpointKind
}

func (c *countingPool) ListEnabledChatModels(context.Context) ([]core.SmartModelRow, error) {
	c.calls++
	c.kinds = append(c.kinds, typology.EndpointKindChat)
	return c.rows, nil
}

func (c *countingPool) ListEnabledCandidates(_ context.Context, kind typology.EndpointKind) ([]core.SmartModelRow, error) {
	c.calls++
	c.kinds = append(c.kinds, kind)
	return c.rows, nil
}

// brokenPool stands for a catalogue read that fails — a dropped connection, a
// snapshot mid-reload.
type brokenPool struct{ err error }

func (b *brokenPool) ListEnabledChatModels(context.Context) ([]core.SmartModelRow, error) {
	return nil, b.err
}

func (b *brokenPool) ListEnabledCandidates(context.Context, typology.EndpointKind) ([]core.SmartModelRow, error) {
	return nil, b.err
}

// TestResolve_APoolFailureDoesNotSinkRulesThatNeverWantedAPool.
//
// The pool is prepared for every request because Stage A assembles facts once,
// but most rules name their target outright and never look at it. Treating a
// catalogue read failure as fatal would convert an outage in one optional
// input into a total routing outage — the rules that could still be served
// perfectly are the majority, and they would go down with it.
//
// The pool is left nil rather than empty. Those are different statements: nil
// says "not available", empty says "nothing qualifies", and a strategy that
// reads the difference falls back to its own fetch instead of concluding no
// model can serve the request.
func TestResolve_APoolFailureDoesNotSinkRulesThatNeverWantedAPool(t *testing.T) {
	f := newResolverFixture()
	f.addProvider("p1", true)
	f.addModel("m1", "p1", "gpt-4o", true)
	f.addRule(store.RoutingRule{
		ID: "r1", Name: "direct", StrategyType: "single", PipelineStage: 1,
		Config: json.RawMessage(`{"type":"single","providerId":"p1","modelId":"m1"}`),
	})

	r := f.resolver.WithModelPool(&brokenPool{err: errors.New("catalogue unavailable")})
	rctx := &core.RoutingContext{RequestedModel: core.RequestedModel{ID: "gpt-4o"}}

	plan, err := r.Resolve(context.Background(), rctx)
	if err != nil {
		t.Fatalf("a rule naming its own target was refused because an input it never reads "+
			"was unavailable: %v", err)
	}
	if len(plan.Targets) != 1 || plan.Targets[0].ModelID != "m1" {
		t.Errorf("targets = %+v, want the rule's own target — the routing decision does not "+
			"depend on the pool here", plan.Targets)
	}
	if rctx.ModelPool != nil {
		t.Error("the pool was left non-nil after a failed read; a strategy cannot then tell " +
			"'unavailable' from 'nothing qualifies' and will refuse instead of falling back")
	}
}

// TestResolveTargets_ACrossModalityDropIsVisibleInTheTrace.
//
// The modality guard exists because a rule can name a model that cannot serve
// the endpoint it is routing — an image model on a chat rule, most often after
// a model is re-tagged in the catalogue and the rule that names it is not
// revisited. Dropping the target is right; dropping it quietly is not.
//
// Silent, the operator sees a rule that matched, produced nothing, and left
// nothing to read — the failure looks like a rule that "just doesn't work". The
// trace entry is the difference between that and a one-line answer, and it is
// the half of this guard nothing was asserting: the drop itself is exercised by
// the recovery tests, the explanation was not.
func TestResolveTargets_ACrossModalityDropIsVisibleInTheTrace(t *testing.T) {
	f := newResolverFixture()
	f.addProvider("p1", true)
	f.addModel("m-img", "p1", "dall-e-3", true)
	f.store.models["m-img"].Type = "image"
	f.addRule(store.RoutingRule{
		ID: "r1", Name: "names an image model", StrategyType: "single", PipelineStage: 1,
		Config: json.RawMessage(`{"type":"single","providerId":"p1","modelId":"m-img"}`),
	})

	res, err := f.resolver.ResolveTargets(context.Background(), &core.RoutingContext{
		RequestedModel: core.RequestedModel{ID: "m-img"},
		EndpointType:   typology.EndpointKindChat,
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(res.AllTargets()) != 0 {
		t.Fatalf("targets = %+v, want none — an image model cannot serve a chat request",
			res.AllTargets())
	}

	var explained bool
	for _, e := range res.PipelineTrace {
		if strings.Contains(e.Decision, "modality guard") {
			explained = true
		}
	}
	if !explained {
		t.Errorf("the drop left no trace entry: %+v — the operator sees a rule that matched "+
			"and produced nothing, with no way to learn the model's type was the reason",
			res.PipelineTrace)
	}
}

// TestResolve_TheModelPoolIsReadOnce_WhenAStrategyActuallyUsesIt.
//
// The sibling test above uses a `single` rule, so no strategy ever reaches for
// a pool: it counts the pipeline's own read and would stay green if every
// strategy fetched its own. This one puts the strategy that DOES need a pool on
// the other end, and does it on a non-chat endpoint — the shape where the two
// readers disagreed. The pipeline was reading the chat set for every request
// while `model=auto` on an image endpoint fetched the image set for itself: two
// catalogue reads on the routing hot path, one of them thrown away, and a
// prepared field describing a different endpoint than the request.
//
// Two reads are not merely wasteful. They are two snapshots: a model enabled
// between them is routable by one and not the other, and which one wins depends
// on timing.
func TestResolve_TheModelPoolIsReadOnce_WhenAStrategyActuallyUsesIt(t *testing.T) {
	f := newResolverFixture()
	f.addProvider("p1", true)
	f.addModel("m-img", "p1", "dall-e-3", true)
	f.store.models["m-img"].Type = "image"
	f.addRule(store.RoutingRule{
		ID: "r1", Name: "auto", StrategyType: "smart", PipelineStage: 1,
		Config: json.RawMessage(`{"type":"smart"}`),
	})

	counting := &countingPool{rows: []core.SmartModelRow{
		{ModelID: "m-img", ProviderID: "p1", ProviderModelID: "dall-e-3"},
	}}
	// Re-register the strategies with smart wired to the SAME catalogue the
	// pipeline reads, so a second read anywhere is visible on one counter.
	reg := strategies.NewStrategyRegistry()
	f.resolver.registry = reg
	strategies.RegisterAllStrategies(reg, f.resolver.LookupTargetFunc(), nil, &strategies.SmartDeps{
		Store:  counting,
		Lookup: f.resolver.LookupTargetFunc(),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	r := f.resolver.WithModelPool(counting)

	rctx := &core.RoutingContext{
		RequestedModel: core.RequestedModel{ID: "auto"},
		EndpointType:   typology.EndpointKindImageGeneration,
	}
	plan, err := r.Resolve(context.Background(), rctx)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(plan.Targets) != 1 {
		t.Fatalf("targets = %+v — the strategy never consumed a pool, so this test is not "+
			"looking at the path it claims to guard", plan.Targets)
	}
	if counting.calls != 1 {
		t.Errorf("the catalogue was read %d times (%v) for one request — the pipeline and the "+
			"strategy each answered 'which models could serve this', from two snapshots that "+
			"need not agree", counting.calls, counting.kinds)
	}
	for _, k := range counting.kinds {
		if k != typology.EndpointKindImageGeneration {
			t.Errorf("the catalogue was read for %s on an image request — the prepared pool "+
				"describes a different endpoint than the one being routed, so the strategy "+
				"either discards it or routes from chat models", k)
		}
	}
}

// TestResolve_ARuleOwnChainComesBeforeAnotherRulesAnswer.
//
// Two kinds of backup end up in one plan: the chain an admin wrote ON the rule
// that won, and the rules they wrote below it. The first is that rule's own
// statement about what to do when its answer fails; the second is a different
// rule's answer. A request should exhaust the first before reaching the second.
//
// Collecting them in evaluation order inverts that — the lower rules are seen
// while the loop is still running and the winner's chain is assembled after it,
// so a rule the admin ranked last got tried before the backup attached to the
// rule that actually matched.
func TestResolve_ARuleOwnChainComesBeforeAnotherRulesAnswer(t *testing.T) {
	f := newResolverFixture()
	f.addProvider("p1", true)
	f.addProvider("p2", true)
	f.addProvider("p3", true)
	f.addModel("m-primary", "p1", "primary", true)
	f.addModel("m-chain", "p2", "chain", true)
	f.addModel("m-lower", "p3", "lower", true)

	f.addRule(store.RoutingRule{
		ID: "r-a-primary", Name: "the rule that wins", StrategyType: "single", PipelineStage: 1,
		Priority: 100,
		Config:   json.RawMessage(`{"type":"single","providerId":"p1","modelId":"m-primary"}`),
		FallbackChain: mustJSON(t, []core.FallbackChainEntry{
			{ProviderID: "p2", ModelID: "m-chain"},
		}),
	})
	f.addRule(store.RoutingRule{
		ID: "r-b-lower", Name: "the rule underneath", StrategyType: "single", PipelineStage: 1,
		Priority: 1,
		Config:   json.RawMessage(`{"type":"single","providerId":"p3","modelId":"m-lower"}`),
	})

	plan, err := f.resolver.Resolve(context.Background(), &core.RoutingContext{
		RequestedModel: core.RequestedModel{ID: "primary"},
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	var order []string
	for _, tgt := range plan.Targets {
		order = append(order, tgt.ModelID)
	}
	for _, tgt := range plan.RecoveryTargets {
		order = append(order, tgt.ModelID)
	}
	want := []string{"m-primary", "m-chain", "m-lower"}
	for i := range want {
		if i >= len(order) || order[i] != want[i] {
			t.Fatalf("order = %v, want %v — the backup an admin attached to the rule that "+
				"matched must be tried before a rule they ranked below it", order, want)
		}
	}
}

// TestResolveTargets_ReportsThatARuleMatchedEvenWhenItResolvedNothing is the
// resolver half of the passthrough precondition. Its twin below holds the
// other boundary: a filter emptying the plan is NOT this case.
//
// The handler decides between "no rule applies — serve the model they asked
// for" and "a rule applies and yielded — refuse" from this count alone. Without
// it the two cases are one empty target list, and the request a rule redirected
// gets served by the model the rule redirects AWAY from. The count is about the
// MATCH, not the outcome: a rule pointing at a row that is gone still applied.
func TestResolveTargets_ReportsThatARuleMatchedEvenWhenItResolvedNothing(t *testing.T) {
	f := newResolverFixture()
	f.addProvider("openai", true)
	f.addModel("gpt-4", "openai", "gpt-4", true)

	f.addRule(store.RoutingRule{
		ID: "r-redirect", Name: "compliance redirect", StrategyType: "single",
		PipelineStage: 1, Priority: 100,
		Config: mustJSON(t, core.StrategyNode{Type: "single", ProviderID: "openai", ModelID: "m-decommissioned"}),
	})

	res, err := f.resolver.ResolveTargets(context.Background(), &core.RoutingContext{
		RequestedModel: core.RequestedModel{ID: "gpt-4", CandidateIDs: []string{"gpt-4"}},
		EndpointType:   typology.EndpointKindChat,
	})
	if err != nil {
		t.Fatalf("resolve targets: %v", err)
	}
	if len(res.AllTargets()) != 0 {
		t.Fatalf("fixture is wrong: the rule resolved %d target(s), so this test cannot see the "+
			"case it exists for", len(res.AllTargets()))
	}
	if !res.RuleMatchedAndResolvedNothing {
		t.Fatal("the handler reads this to tell a redirected request from an unrouted one; " +
			"false sends the redirected one to the model it was redirected away from")
	}
}

// TestHydrateRequestedModel_TwoProvidersLeaveTheProviderFieldsEmpty.
//
// The catalogue query states no order, so "the first candidate's provider" is
// not a fact about the request — it is a fact about which row came back. Naming
// it anyway put a provider on the routing context that a second identical
// request could disagree with, and `matchConditions.providers` compared against
// exactly that. The set is recorded instead; the singular field stays empty,
// which every consumer already treats as "not known".
func TestHydrateRequestedModel_TwoProvidersLeaveTheProviderFieldsEmpty(t *testing.T) {
	f := newResolverFixture()
	f.addProvider("groq", true)
	f.addProvider("together", true)
	f.addModel("m-groq", "groq", "llama-3.3-70b", true)
	f.addModel("m-together", "together", "llama-3.3-70b", true)
	// Same code on both rows: one model, two hosts.
	f.store.models["m-groq"].Code = "llama-3.3-70b"
	f.store.models["m-together"].Code = "llama-3.3-70b"

	rctx := &core.RoutingContext{RequestedModel: core.RequestedModel{ID: "llama-3.3-70b"}}
	f.resolver.hydrateRequestedModel(context.Background(), rctx)

	rm := rctx.RequestedModel
	if len(rm.CandidateProviderIDs) != 2 {
		t.Fatalf("CandidateProviderIDs = %v, want both providers — without the set there is "+
			"nothing for a provider condition to compare against", rm.CandidateProviderIDs)
	}
	if rm.ProviderID != "" || rm.ProviderName != "" || rm.ProviderModelID != "" {
		t.Errorf("the singular fields name %q/%q/%q for a code two providers serve; whichever "+
			"row happened to be first is not a fact about the request",
			rm.ProviderID, rm.ProviderName, rm.ProviderModelID)
	}
	// The type is shared by every host of one code, so it stays available.
	if rm.Type != "chat" {
		t.Errorf("Type = %q, want chat — every candidate agrees, so there is nothing to guess", rm.Type)
	}
}

// TestResolveTargets_AFilterEmptyingThePlanIsNotARuleRefusing is the boundary
// the first version of this signal got wrong, and the mistake took out a whole
// class of endpoint.
//
// The handler refuses rather than passing through when a rule applied and
// resolved nothing. Counting that AFTER the filters made a catch-all rule — an
// empty matchConditions matches every request — take down every non-chat
// endpoint in the deployment: the rule matched, resolved its chat target, the
// modality filter dropped it because the request was for images, and an image
// request that passthrough serves correctly got a 503.
//
// A filter emptying the plan is OUR fact about the targets, not the rule
// refusing anything. The rule resolved something; we are the ones who could
// not use it.
func TestResolveTargets_AFilterEmptyingThePlanIsNotARuleRefusing(t *testing.T) {
	f := newResolverFixture()
	f.addProvider("openai", true)
	f.addModel("m-chat", "openai", "gpt-4o", true)
	f.addModel("m-image", "openai", "dall-e-3", true)
	f.store.models["m-image"].Type = "image"

	// The catch-all: no match conditions, so it applies to every request.
	f.addRule(store.RoutingRule{
		ID: "r-catchall", Name: "catch-all", StrategyType: "single",
		PipelineStage: 1, Priority: 100,
		Config: mustJSON(t, core.StrategyNode{Type: "single", ProviderID: "openai", ModelID: "m-chat"}),
	})

	res, err := f.resolver.ResolveTargets(context.Background(), &core.RoutingContext{
		RequestedModel: core.RequestedModel{ID: "dall-e-3", CandidateIDs: []string{"m-image"}},
		EndpointType:   typology.EndpointKindImageGeneration,
	})
	if err != nil {
		t.Fatalf("resolve targets: %v", err)
	}
	if len(res.AllTargets()) != 0 {
		t.Fatalf("fixture is wrong: the modality filter did not empty the plan (%d target(s)), "+
			"so this test cannot see the case it exists for", len(res.AllTargets()))
	}
	if res.RuleMatchedAndResolvedNothing {
		t.Fatal("a chat rule matched an image request, resolved its chat target, and the " +
			"modality filter dropped it — reporting that as the RULE resolving nothing makes " +
			"the handler refuse a request passthrough serves correctly, and one catch-all rule " +
			"then takes down every non-chat endpoint in the deployment")
	}
}

// TestHydrateRequestedModel_RecordsThatTheCatalogueCouldNotAnswer.
//
// The candidate fields end up empty for two very different reasons: the caller
// named nothing, or the caller named something and the lookup failed. Only the
// first makes a provider condition inapplicable. Without a flag distinguishing
// them, a catalogue read failure widened every provider-scoped rule to every
// provider — and the failure was logged at Debug, so nothing said so either.
func TestHydrateRequestedModel_RecordsThatTheCatalogueCouldNotAnswer(t *testing.T) {
	f := newResolverFixture()
	f.store.candidatesErr = errors.New("store: resolve model candidates: connection reset")

	rctx := &core.RoutingContext{RequestedModel: core.RequestedModel{ID: "gpt-4o"}}
	f.resolver.hydrateRequestedModel(context.Background(), rctx)

	if !rctx.RequestedModel.HydrationFailed {
		t.Fatal("a failed catalogue lookup left no trace on the request, so downstream it is " +
			"indistinguishable from a caller who named nothing — and a provider-scoped rule " +
			"then matches every provider")
	}
	if !rctx.RequestedModel.ProviderIsUnknowable() {
		t.Error("ProviderIsUnknowable() is false after a failed lookup of a NAMED model")
	}

	// The other direction: a caller who named nothing has not "failed" anything.
	auto := &core.RoutingContext{RequestedModel: core.RequestedModel{ID: "auto"}}
	f.resolver.hydrateRequestedModel(context.Background(), auto)
	if auto.RequestedModel.ProviderIsUnknowable() {
		t.Error("`auto` reported as unknowable; it names no model, which is an ANSWER, and " +
			"reporting it as a failure makes provider-scoped rules refuse the requests they exist for")
	}
}

// TestResolveTargets_ThePlanNeverHoldsOneTargetTwice.
//
// The strategy's answer and the rule's own fallback chain are assembled
// independently, so a chain naming a model the strategy also picked put that
// model in the plan twice. Not cosmetic: the walk gives each entry its own
// state and dispatches to it again after a transient failure, and
// EffectiveCallBudget is derived from the plan's LENGTH — so a duplicate
// silently buys the request another pair of attempts against a provider the
// admin bounded.
func TestResolveTargets_ThePlanNeverHoldsOneTargetTwice(t *testing.T) {
	f := newResolverFixture()
	f.addProvider("openai", true)
	f.addProvider("anthropic", true)
	f.addModel("m-primary", "openai", "gpt-4o", true)
	f.addModel("m-backup", "anthropic", "claude", true)

	f.addRule(store.RoutingRule{
		ID: "r-dupe", Name: "chain repeats the pick", StrategyType: "single",
		PipelineStage: 1, Priority: 100,
		Config: mustJSON(t, core.StrategyNode{Type: "single", ProviderID: "openai", ModelID: "m-primary"}),
		FallbackChain: mustJSON(t, []core.FallbackChainEntry{
			// The admin listed the primary again, plus a real alternative.
			{ProviderID: "openai", ModelID: "m-primary"},
			{ProviderID: "anthropic", ModelID: "m-backup"},
		}),
	})

	res, err := f.resolver.ResolveTargets(context.Background(), &core.RoutingContext{
		RequestedModel: core.RequestedModel{ID: "gpt-4o", CandidateIDs: []string{"m-primary"}},
		EndpointType:   typology.EndpointKindChat,
	})
	if err != nil {
		t.Fatalf("resolve targets: %v", err)
	}

	seen := map[string]int{}
	for _, tgt := range res.AllTargets() {
		seen[tgt.ProviderID+"/"+tgt.ModelID]++
	}
	if n := seen["openai/m-primary"]; n != 1 {
		t.Fatalf("openai/m-primary appears %d times in a %d-target plan: %+v — the budget is "+
			"derived from that length, so the duplicate raises what one request may spend",
			n, len(res.AllTargets()), res.AllTargets())
	}
	// The real alternative must survive the deduplication.
	if seen["anthropic/m-backup"] != 1 {
		t.Errorf("the chain's genuine alternative was dropped: %+v", res.AllTargets())
	}
	// And the pick keeps position zero — first occurrence wins, which is the
	// position the strategy and the health reorder chose for it.
	if res.Primary().ModelID != "m-primary" {
		t.Errorf("Primary() = %q, want m-primary", res.Primary().ModelID)
	}
}

// TestResolveTargets_TheReasoningFlagReachesTheTarget.
//
// An egress codec's whole per-model payload is the target it is handed. Until
// this landed, `features` was not part of it — so a codec could not ask whether
// the model it was about to call reasons at all, and every adapter either sent
// a reasoning parameter to all of its models or to none of them. `openai`'s
// identity codec does the first: `reasoning_effort` rides through to gpt-4o
// as readily as to o3.
//
// Asserted end-to-end from a catalogue row, because the failure this replaces
// was precisely a fact that existed in the catalogue and stopped existing three
// hops later.
func TestResolveTargets_TheReasoningFlagReachesTheTarget(t *testing.T) {
	f := newResolverFixture()
	f.addProvider("openai", true)
	f.addModel("m-reasoner", "openai", "o3", true)
	f.addModel("m-plain", "openai", "gpt-4o", true)
	f.store.models["m-reasoner"].Features = []string{"streaming", core.FeatureReasoning}
	f.store.models["m-plain"].Features = []string{"streaming", "function_calling"}

	for _, tc := range []struct {
		model string
		want  bool
	}{
		{"m-reasoner", true},
		{"m-plain", false},
	} {
		t.Run(tc.model, func(t *testing.T) {
			f2 := newResolverFixture()
			f2.addProvider("openai", true)
			f2.store.models[tc.model] = f.store.models[tc.model]
			f2.addRule(store.RoutingRule{
				ID: "r-" + tc.model, Name: tc.model, StrategyType: "single",
				PipelineStage: 1, Priority: 100,
				Config: mustJSON(t, core.StrategyNode{Type: "single", ProviderID: "openai", ModelID: tc.model}),
			})
			res, err := f2.resolver.ResolveTargets(context.Background(), &core.RoutingContext{
				RequestedModel: core.RequestedModel{ID: "auto"},
				EndpointType:   typology.EndpointKindChat,
			})
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if len(res.AllTargets()) == 0 {
				t.Fatalf("fixture resolved nothing, so this asserts about no target")
			}
			if got := res.Primary().Reasons; got != tc.want {
				t.Fatalf("Reasons = %v, want %v — the catalogue says so and the codec that will "+
					"call this model reads it from here", got, tc.want)
			}
		})
	}
}

package routing

import (
	"context"
	"errors"
	"fmt"
	"github.com/goccy/go-json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/matcher"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/strategies"
)

// awaitStatus polls until a provider reaches the expected health status. The
// HealthTracker applies records asynchronously (single writer), so a read
// immediately after recording must wait for the samples to be published.
func awaitStatus(t *testing.T, tracker *store.HealthTracker, providerID string, want store.HealthStatus) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if tracker.GetHealth(providerID).Status == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("provider %s did not reach %s in time; got %+v", providerID, want, tracker.GetHealth(providerID))
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// Test helpers for routing package coverage tests.
// These mirror the helpers in resolver_test.go to avoid the import cycle that
// would occur if this file referenced the routing_test fakeStore directly.

// coverageFakeStore is an in-memory routingStore for coverage-lift tests.
type coverageFakeStore struct {
	rules     []store.RoutingRule
	providers map[string]*store.Provider
	models    map[string]*store.Model
}

func (f *coverageFakeStore) GetEnabledRoutingRules(_ context.Context) ([]store.RoutingRule, error) {
	return f.rules, nil
}

func (f *coverageFakeStore) GetProviderAndModel(_ context.Context, providerID, modelID string) (*store.Provider, *store.Model, error) {
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

func (f *coverageFakeStore) GetModel(_ context.Context, id string) (*store.Model, error) {
	m, ok := f.models[id]
	if !ok {
		return nil, fmt.Errorf("model %q not found", id)
	}
	return m, nil
}

func (f *coverageFakeStore) ResolveModelCandidates(_ context.Context, code string) ([]store.Model, error) {
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

// coverageResolverFixture wires a Resolver for coverage-lift tests.
type coverageResolverFixture struct {
	store    *coverageFakeStore
	registry *strategies.StrategyRegistry
	resolver *Resolver
}

func newCoverageResolverFixture() *coverageResolverFixture {
	fs := &coverageFakeStore{
		providers: map[string]*store.Provider{},
		models:    map[string]*store.Model{},
	}
	reg := strategies.NewStrategyRegistry()
	resolver := NewResolver(fs, reg, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	strategies.RegisterAllStrategies(reg, resolver.LookupTargetFunc(), nil)
	return &coverageResolverFixture{store: fs, registry: reg, resolver: resolver}
}

func (f *coverageResolverFixture) addProvider(id string, enabled bool) {
	f.store.providers[id] = &store.Provider{ID: id, Name: id, AdapterType: "openai", BaseURL: "https://" + id + ".example.com", Enabled: enabled}
}

func (f *coverageResolverFixture) addModel(id, providerID, providerModelID string, enabled bool) {
	f.store.models[id] = &store.Model{ID: id, Code: id, Name: id, ProviderID: providerID, ProviderModelID: providerModelID, Type: "chat", Enabled: enabled}
}

func (f *coverageResolverFixture) addRule(r store.RoutingRule) {
	r.Enabled = true
	f.store.rules = append(f.store.rules, r)
}

// coverageMockLookup is a no-op TargetLookup for coverage-lift tests.
func coverageMockLookup(_ context.Context, providerID, modelID string) (*core.RoutingTarget, error) {
	return &core.RoutingTarget{ProviderID: providerID, ModelID: modelID, ModelCode: modelID}, nil
}

// coverageFakeSmartStore returns an empty model list.
type coverageFakeSmartStore struct{}

func (s *coverageFakeSmartStore) ListEnabledChatModels(_ context.Context) ([]core.SmartModelRow, error) {
	return nil, nil
}

func TestMatchGlob_LiteralPath(t *testing.T) {
	// No `*` -> early-return exact compare.
	if core.MatchGlob("exact", "exact") != true {
		t.Error("literal glob must match exact value")
	}
	if core.MatchGlob("exact", "different") != false {
		t.Error("literal glob must not match different value")
	}
}

// format.go — nil core.RoutingTarget paths.

// TestFormatTarget_NilTarget covers the nil-guard early-returns in both
// formatters. Operators rely on a stable "?/? (?)" placeholder so the
// strategy trace string remains parseable when a target row is missing.

func TestFormatTarget_NilTarget(t *testing.T) {
	if got := core.FormatTargetFriendly(nil); got != `?/? ("?")` {
		t.Errorf("core.FormatTargetFriendly(nil) = %q, want \"?/? (\\\"?\\\")\"", got)
	}
	if got := core.FormatTargetPath(nil); got != `?/?` {
		t.Errorf("core.FormatTargetPath(nil) = %q, want ?/?", got)
	}
}

// TestFormatTarget_PartialFields locks the orQuestion fallback: any
// empty field is substituted with `?` so the structure stays parseable.

func TestFormatTarget_PartialFields(t *testing.T) {
	t1 := &core.RoutingTarget{ProviderName: "openai", ModelCode: "", ModelName: ""}
	if got := core.FormatTargetFriendly(t1); got != `openai/? ("?")` {
		t.Errorf("partial formatTargetFriendly = %q", got)
	}
	if got := core.FormatTargetPath(t1); got != `openai/?` {
		t.Errorf("partial formatTargetPath = %q", got)
	}
}

// health_ranker.go — degraded + unavailable ordering.

// TestHealthRanker_DegradedAndUnavailable_RankedLast verifies the full
// 3-tier ordering: healthy first, degraded next, unavailable last; and
// within each band the original index order is preserved (stable sort).

func TestHealthRanker_DegradedAndUnavailable_RankedLast(t *testing.T) {
	tracker := store.NewHealthTracker()
	t.Cleanup(tracker.Stop)
	// degraded: ~10% failure rate (just above 0.05 threshold).
	// thresholdDegraded = 0.05, thresholdUnavailable = 0.25.
	// One failure out of 11 samples = ~0.09 error rate => degraded.
	for range 10 {
		tracker.RecordSuccess("provDegraded", "deg", 50)
	}
	tracker.RecordFailure("provDegraded", "deg", 100)

	// unavailable: 100% failure (>0.25).
	for range 5 {
		tracker.RecordFailure("provUnavail", "una", 200)
	}
	awaitStatus(t, tracker, "provDegraded", store.HealthStatusDegraded)
	awaitStatus(t, tracker, "provUnavail", store.HealthStatusUnavailable)

	ranker := core.NewHealthRanker(tracker)
	in := []core.RoutingTarget{
		{ProviderID: "provUnavail", ProviderName: "u"},
		{ProviderID: "provDegraded", ProviderName: "d"},
		{ProviderID: "provHealthy", ProviderName: "h"},
	}
	out := ranker.Reorder(in)
	wantOrder := []string{"provHealthy", "provDegraded", "provUnavail"}
	for i, w := range wantOrder {
		if out[i].ProviderID != w {
			t.Errorf("Reorder[%d]=%q, want %q (full=%v)", i, out[i].ProviderID, w, out)
		}
	}
}

// narrowing.go — Apply: invalid JSON, non-matching stage, non-policy node.

// stubWarnLogger captures Warn calls so we can assert Apply logs (without
// pulling slog).
type stubWarnLogger struct{ warns []string }

func (l *stubWarnLogger) Warn(msg string, _ ...any) { l.warns = append(l.warns, msg) }

// TestNarrowingEngine_Apply_BadJSON_Skipped: a stage-0 rule with malformed
// Config JSON is logged via Warn and skipped — state remains empty.

func TestNarrowingEngine_Apply_BadJSON_Skipped(t *testing.T) {
	eng := &matcher.NarrowingEngine{}
	rules := []store.RoutingRule{
		{
			ID:            "r-bad",
			PipelineStage: 0,
			Config:        json.RawMessage(`{ this is not json`),
		},
	}
	logger := &stubWarnLogger{}
	match := func(_ store.RoutingRule, _ string, _ *core.RoutingContext) bool { return true }
	state, _ := eng.Apply(context.Background(), rules, &core.RoutingContext{}, logger, match)
	if !matcher.IsNarrowingEmpty(state) {
		t.Error("bad JSON rule should leave narrowing empty")
	}
	if len(logger.warns) != 1 {
		t.Errorf("expected 1 Warn for bad JSON, got %d (%v)", len(logger.warns), logger.warns)
	}
}

// TestNarrowingEngine_Apply_SkipsNonStage0AndNonPolicy: rules whose
// PipelineStage != 0 are skipped; non-policy stage-0 rules contribute a
// trace entry but no state change.

func TestNarrowingEngine_Apply_SkipsNonStage0AndNonPolicy(t *testing.T) {
	eng := &matcher.NarrowingEngine{}
	rules := []store.RoutingRule{
		{
			ID:            "r-stage1",
			PipelineStage: 1, // skipped
			Config:        mustJSON(t, core.StrategyNode{Type: "policy", DenyModelIDs: []string{"x"}}),
		},
		{
			ID:            "r-stage0-nonmatch",
			PipelineStage: 0,
			Config:        mustJSON(t, core.StrategyNode{Type: "policy", DenyModelIDs: []string{"y"}}),
		},
		{
			ID:            "r-stage0-nonpolicy",
			PipelineStage: 0,
			Config:        mustJSON(t, core.StrategyNode{Type: "single", ProviderID: "p", ModelID: "m"}),
		},
	}
	logger := &stubWarnLogger{}
	match := func(rule store.RoutingRule, _ string, _ *core.RoutingContext) bool {
		return rule.ID != "r-stage0-nonmatch"
	}
	state, trace := eng.Apply(context.Background(), rules, &core.RoutingContext{}, logger, match)
	if !matcher.IsNarrowingEmpty(state) {
		t.Errorf("non-policy stage-0 rule should not mutate state, got %+v", state)
	}
	// r-stage0-nonpolicy contributes one trace entry.
	if len(trace) != 1 {
		t.Errorf("expected 1 trace entry (non-policy stage-0), got %d", len(trace))
	}
}

// resolver.go — constructor + error branches in Resolve / Explain /
// ResolveTargets / hydrateRequestedModel / lookupTarget / ruleMatches.

// TestNewResolver_WiresAllFields covers the exported constructor that the
// production CMD uses (kept as a separate path from the test fixture's
// hand-rolled struct literal).

func TestNewResolver_WiresAllFields(t *testing.T) {
	fs := &coverageFakeStore{providers: map[string]*store.Provider{}, models: map[string]*store.Model{}}
	reg := strategies.NewStrategyRegistry()
	ht := store.NewHealthTracker()
	t.Cleanup(ht.Stop)
	ranker := core.NewHealthRanker(ht)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := NewResolver(fs, reg, ranker, logger, nil)
	if r == nil {
		t.Fatal("NewResolver returned nil")
	}
	// Verify the resolver is functional by running a trivial Resolve call.
	_, err := r.Resolve(context.Background(), &core.RoutingContext{
		RequestedModel: core.RequestedModel{ID: "x"},
	})
	if err != nil {
		t.Fatalf("NewResolver: Resolve failed unexpectedly: %v", err)
	}
}

// errRulesStore returns an error from GetEnabledRoutingRules to exercise
// the Resolve fetch-rules error path.
type errRulesStore struct{ coverageFakeStore }

func (e *errRulesStore) GetEnabledRoutingRules(_ context.Context) ([]store.RoutingRule, error) {
	return nil, errors.New("rules query exploded")
}

func TestResolver_Resolve_FetchRulesError(t *testing.T) {
	f := newCoverageResolverFixture()
	f.resolver.db = &errRulesStore{coverageFakeStore: *f.store}
	_, err := f.resolver.Resolve(context.Background(), &core.RoutingContext{
		RequestedModel: core.RequestedModel{ID: "gpt-4"},
	})
	if err == nil || !strings.Contains(err.Error(), "rules query exploded") {
		t.Fatalf("expected fetch-rules error to surface, got %v", err)
	}
}

// TestResolver_Resolve_BadStrategyConfigJSON: primary rule's Config is
// malformed JSON. Resolve must return a wrapped parse error.

func TestResolver_Resolve_BadStrategyConfigJSON(t *testing.T) {
	f := newCoverageResolverFixture()
	f.addRule(store.RoutingRule{
		ID:            "r-bad",
		Name:          "bad",
		StrategyType:  "single",
		PipelineStage: 1,
		Config:        json.RawMessage(`{not json`),
	})
	_, err := f.resolver.Resolve(context.Background(), &core.RoutingContext{
		RequestedModel: core.RequestedModel{ID: "gpt-4"},
	})
	if err == nil || !strings.Contains(err.Error(), "parse strategy config") {
		t.Fatalf("expected parse error, got %v", err)
	}
}

// TestResolver_Resolve_EvaluateError_LeavesEmptyTargetsWithTrace: when the
// strategy evaluation returns an error (e.g. unknown strategy type), the
// resolver logs a Warn but still completes with empty targets (no crash).

func TestResolver_Resolve_EvaluateError_LeavesEmptyTargetsWithTrace(t *testing.T) {
	f := newCoverageResolverFixture()
	f.addProvider("openai", true)
	f.addModel("gpt-4", "openai", "gpt-4", true)
	f.addRule(store.RoutingRule{
		ID:            "r-bad-type",
		Name:          "unknown-type",
		StrategyType:  "single", // strategyType column is just metadata, eval uses node.Type
		PipelineStage: 1,
		Config:        mustJSON(t, core.StrategyNode{Type: "no-such-strategy"}),
	})
	plan, err := f.resolver.Resolve(context.Background(), &core.RoutingContext{
		RequestedModel: core.RequestedModel{ID: "gpt-4"},
	})
	if err != nil {
		t.Fatalf("Resolve should swallow strategy-eval errors, got %v", err)
	}
	if len(plan.Targets) != 0 {
		t.Errorf("expected empty Targets when strategy eval errored, got %+v", plan.Targets)
	}
}

// TestResolver_Resolve_FallbackChainBadJSON_NoRecovery: a primary rule's
// FallbackChain JSON is malformed — best-effort unmarshal yields no entries,
// the primary still produces its target, and RecoveryTargets stays empty.

func TestResolver_Resolve_FallbackChainBadJSON_NoRecovery(t *testing.T) {
	f := newCoverageResolverFixture()
	f.addProvider("openai", true)
	f.addModel("gpt-4", "openai", "gpt-4", true)
	f.addRule(store.RoutingRule{
		ID:            "r-primary",
		StrategyType:  "single",
		PipelineStage: 1,
		Config:        mustJSON(t, core.StrategyNode{Type: "single", ProviderID: "openai", ModelID: "gpt-4"}),
		FallbackChain: json.RawMessage(`{not-json`),
	})
	plan, err := f.resolver.Resolve(context.Background(), &core.RoutingContext{
		RequestedModel: core.RequestedModel{ID: "gpt-4"},
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(plan.RecoveryTargets) != 0 {
		t.Errorf("malformed fallback chain must yield no recovery, got %+v", plan.RecoveryTargets)
	}
	if len(plan.Targets) != 1 {
		t.Errorf("primary should still produce a target, got %+v", plan.Targets)
	}
}

// TestResolver_Resolve_FallbackChain_LookupErrorSkipped: a fallback chain
// entry that points at a missing model is skipped without crashing.

func TestResolver_Resolve_FallbackChain_LookupErrorSkipped(t *testing.T) {
	f := newCoverageResolverFixture()
	f.addProvider("openai", true)
	f.addModel("gpt-4", "openai", "gpt-4", true)
	chain := []core.FallbackChainEntry{
		{ProviderID: "openai", ModelID: "gpt-4"},       // valid
		{ProviderID: "doesnotexist", ModelID: "ghost"}, // skipped
	}
	f.addRule(store.RoutingRule{
		ID:            "r-primary",
		StrategyType:  "single",
		PipelineStage: 1,
		Config:        mustJSON(t, core.StrategyNode{Type: "single", ProviderID: "openai", ModelID: "gpt-4"}),
		FallbackChain: mustJSON(t, chain),
	})
	plan, err := f.resolver.Resolve(context.Background(), &core.RoutingContext{
		RequestedModel: core.RequestedModel{ID: "gpt-4"},
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(plan.RecoveryTargets) != 1 || plan.RecoveryTargets[0].ProviderID != "openai" {
		t.Errorf("expected only valid recovery entry, got %+v", plan.RecoveryTargets)
	}
}

// TestResolver_Resolve_FallbackRule_BadJSON_Skipped: fallback-strategy
// rules with malformed JSON skip silently without aborting the resolve.

func TestResolver_Resolve_FallbackRule_BadJSON_Skipped(t *testing.T) {
	f := newCoverageResolverFixture()
	f.addProvider("openai", true)
	f.addModel("gpt-4", "openai", "gpt-4", true)
	f.addRule(store.RoutingRule{
		ID:            "r-primary",
		StrategyType:  "single",
		PipelineStage: 1,
		Priority:      100,
		Config:        mustJSON(t, core.StrategyNode{Type: "single", ProviderID: "openai", ModelID: "gpt-4"}),
	})
	f.addRule(store.RoutingRule{
		ID:            "r-fallback-bad",
		StrategyType:  "fallback",
		PipelineStage: 1,
		Priority:      50,
		Config:        json.RawMessage(`{not-json`),
	})
	plan, err := f.resolver.Resolve(context.Background(), &core.RoutingContext{
		RequestedModel: core.RequestedModel{ID: "gpt-4"},
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(plan.Targets) != 1 || plan.Targets[0].ModelID != "gpt-4" {
		t.Errorf("primary should survive bad fallback rule, got %+v", plan.Targets)
	}
}

// TestResolver_Resolve_FallbackRule_EvaluateError_Skipped: fallback rule
// whose node.Type is unknown — registry.Evaluate errors and the rule is
// skipped without affecting the primary.

func TestResolver_Resolve_FallbackRule_EvaluateError_Skipped(t *testing.T) {
	f := newCoverageResolverFixture()
	f.addProvider("openai", true)
	f.addModel("gpt-4", "openai", "gpt-4", true)
	f.addRule(store.RoutingRule{
		ID:            "r-primary",
		StrategyType:  "single",
		PipelineStage: 1,
		Priority:      100,
		Config:        mustJSON(t, core.StrategyNode{Type: "single", ProviderID: "openai", ModelID: "gpt-4"}),
	})
	f.addRule(store.RoutingRule{
		ID:            "r-fallback",
		StrategyType:  "fallback",
		PipelineStage: 1,
		Priority:      50,
		Config:        mustJSON(t, core.StrategyNode{Type: "ghost-strategy"}), // evaluate -> err
	})
	plan, err := f.resolver.Resolve(context.Background(), &core.RoutingContext{
		RequestedModel: core.RequestedModel{ID: "gpt-4"},
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(plan.RecoveryTargets) != 0 {
		t.Errorf("fallback eval error should yield no recovery, got %+v", plan.RecoveryTargets)
	}
}

// TestResolver_Resolve_CarriesRuleRetryPolicy verifies the per-rule
// RetryPolicy JSON rides forward onto the core.RoutingPlan exactly as stored.

func TestResolver_Resolve_CarriesRuleRetryPolicy(t *testing.T) {
	f := newCoverageResolverFixture()
	f.addProvider("openai", true)
	f.addModel("gpt-4", "openai", "gpt-4", true)
	retry := json.RawMessage(`{"maxAttempts":3}`)
	f.addRule(store.RoutingRule{
		ID:            "r-primary",
		StrategyType:  "single",
		PipelineStage: 1,
		Config:        mustJSON(t, core.StrategyNode{Type: "single", ProviderID: "openai", ModelID: "gpt-4"}),
		RetryPolicy:   retry,
	})
	plan, err := f.resolver.Resolve(context.Background(), &core.RoutingContext{
		RequestedModel: core.RequestedModel{ID: "gpt-4"},
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(plan.RuleRetryPolicyJSON) != string(retry) {
		t.Errorf("RuleRetryPolicyJSON not carried: %q vs %q", plan.RuleRetryPolicyJSON, retry)
	}
}

// TestHydrateRequestedModel_NilContext is a defensive guard: hydrate must
// not panic on a nil core.RoutingContext (used by the simulate endpoint when
// the request has no body).

func TestHydrateRequestedModel_NilContext(t *testing.T) {
	f := newCoverageResolverFixture()
	f.resolver.hydrateRequestedModel(context.Background(), nil)
}

// TestHydrateRequestedModel_EmptyID_NoLookup: empty request string skips
// the catalog lookup. Verified by leaving the store empty and checking no
// candidates get added.

func TestHydrateRequestedModel_EmptyID_NoLookup(t *testing.T) {
	f := newCoverageResolverFixture()
	rctx := &core.RoutingContext{RequestedModel: core.RequestedModel{ID: ""}}
	f.resolver.hydrateRequestedModel(context.Background(), rctx)
	if len(rctx.RequestedModel.CandidateIDs) != 0 {
		t.Error("empty request ID must not trigger lookup")
	}
}

// errCandidateStore returns an error from ResolveModelCandidates to
// exercise the swallow-with-Debug path in hydrateRequestedModel.
type errCandidateStore struct{ coverageFakeStore }

func (e *errCandidateStore) ResolveModelCandidates(_ context.Context, _ string) ([]store.Model, error) {
	return nil, errors.New("catalog offline")
}

func TestHydrateRequestedModel_CandidateError_SwallowedAndEmpty(t *testing.T) {
	f := newCoverageResolverFixture()
	f.resolver.db = &errCandidateStore{coverageFakeStore: *f.store}
	rctx := &core.RoutingContext{RequestedModel: core.RequestedModel{ID: "gpt-4"}}
	f.resolver.hydrateRequestedModel(context.Background(), rctx)
	if len(rctx.RequestedModel.CandidateIDs) != 0 {
		t.Errorf("expected empty candidates on lookup error, got %v", rctx.RequestedModel.CandidateIDs)
	}
}

// TestHydrateRequestedModel_NoCandidates_NoFill: an unknown request
// string yields an empty candidates list — no fields get filled.

func TestHydrateRequestedModel_NoCandidates_NoFill(t *testing.T) {
	f := newCoverageResolverFixture()
	rctx := &core.RoutingContext{RequestedModel: core.RequestedModel{ID: "never-seen"}}
	f.resolver.hydrateRequestedModel(context.Background(), rctx)
	if len(rctx.RequestedModel.CandidateIDs) != 0 {
		t.Error("unknown model must produce no candidates")
	}
	if rctx.RequestedModel.ProviderID != "" {
		t.Error("ProviderID must not be auto-filled when no candidates")
	}
}

// TestRuleMatches_InvalidJSON_NoMatch: a malformed matchConditions JSON
// causes ruleMatches to fail-closed (return false) so a corrupted rule
// row can't accidentally widen its scope.

func TestRuleMatches_InvalidJSON_NoMatch(t *testing.T) {
	f := newCoverageResolverFixture()
	rule := store.RoutingRule{
		ID:              "r-bad-conds",
		MatchConditions: json.RawMessage(`{not-json`),
	}
	if f.resolver.ruleMatches(rule, "gpt-4", &core.RoutingContext{}) {
		t.Error("malformed matchConditions should fail-closed")
	}
}

// TestLookupTarget_CarriesRegionPointer verifies the *Region dereference
// path: when Provider.Region is non-nil, the resulting core.RoutingTarget
// carries the dereferenced value verbatim.

func TestLookupTarget_CarriesRegionPointer(t *testing.T) {
	f := newCoverageResolverFixture()
	region := "eu-west-1"
	f.store.providers["openai"] = &store.Provider{
		ID: "openai", Name: "openai", AdapterType: "openai",
		BaseURL: "https://api.openai.com", Enabled: true, Region: &region,
	}
	f.addModel("gpt-4", "openai", "gpt-4", true)
	tg, err := f.resolver.lookupTarget(context.Background(), "openai", "gpt-4")
	if err != nil {
		t.Fatalf("lookupTarget: %v", err)
	}
	if tg.Region != "eu-west-1" {
		t.Errorf("Region not propagated: %q", tg.Region)
	}
}

// TestLookupTarget_NotFound: missing provider/model returns an error.

func TestLookupTarget_NotFound(t *testing.T) {
	f := newCoverageResolverFixture()
	_, err := f.resolver.lookupTarget(context.Background(), "ghost", "ghost")
	if err == nil {
		t.Error("expected error for missing provider")
	}
}

// TestExplain_RuleConfigUnmarshalFails_NoBranches: when the matched
// primary rule's Config JSON cannot be re-parsed in Explain's second
// pass (after Resolve succeeded), Explain returns the plan without
// branches rather than failing.

func TestExplain_GracefullyDegrades(t *testing.T) {
	// We cannot easily make a single rule's JSON unparseable AFTER Resolve
	// succeeded (Resolve does the parse first). Instead we cover the
	// "rule not found in second fetch" branch via a swap-during-Explain
	// store.
	f := newCoverageResolverFixture()
	f.addProvider("openai", true)
	f.addModel("gpt-4", "openai", "gpt-4", true)
	f.addRule(store.RoutingRule{
		ID:            "r-primary",
		Name:          "p",
		StrategyType:  "single",
		PipelineStage: 1,
		Config:        mustJSON(t, core.StrategyNode{Type: "single", ProviderID: "openai", ModelID: "gpt-4"}),
	})

	// First call to Resolve via Explain populates plan.RuleID.
	// Then swap the store so the second GetEnabledRoutingRules returns
	// a rules list missing the primary rule (simulates cache eviction).
	plan, err := f.resolver.Explain(context.Background(), &core.RoutingContext{
		RequestedModel: core.RequestedModel{ID: "gpt-4"},
	})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if plan.RuleID != "r-primary" {
		t.Fatalf("expected primary rule, got %q", plan.RuleID)
	}
	// Now exercise the not-found branch with a dedicated swap.
	swap := &swapRulesStore{
		base:      f.store,
		swapRules: []store.RoutingRule{}, // no rule with that ID
	}
	f.resolver.db = swap
	plan2, err := f.resolver.Explain(context.Background(), &core.RoutingContext{
		RequestedModel: core.RequestedModel{ID: "gpt-4"},
	})
	if err != nil {
		t.Fatalf("Explain after swap: %v", err)
	}
	// Without a matched rule on the swap store, plan2.RuleID is "".
	if plan2.RuleID != "" {
		t.Errorf("expected no rule on swap store, got %q", plan2.RuleID)
	}
	if len(plan2.Branches) != 0 {
		t.Errorf("expected empty branches, got %+v", plan2.Branches)
	}
}

// swapRulesStore lets a test return one set of rules from one call and a
// different set from a subsequent call (used to simulate cache eviction).
type swapRulesStore struct {
	base      *coverageFakeStore
	swapRules []store.RoutingRule
}

func (s *swapRulesStore) GetEnabledRoutingRules(_ context.Context) ([]store.RoutingRule, error) {
	return s.swapRules, nil
}
func (s *swapRulesStore) GetProviderAndModel(ctx context.Context, p, m string) (*store.Provider, *store.Model, error) {
	return s.base.GetProviderAndModel(ctx, p, m)
}
func (s *swapRulesStore) GetModel(ctx context.Context, id string) (*store.Model, error) {
	return s.base.GetModel(ctx, id)
}
func (s *swapRulesStore) ResolveModelCandidates(ctx context.Context, c string) ([]store.Model, error) {
	return s.base.ResolveModelCandidates(ctx, c)
}

// TestExplain_ResolveError_PropagatesError: when Resolve fails (e.g.
// fetch-rules error), Explain surfaces the same error.

func TestExplain_ResolveError_PropagatesError(t *testing.T) {
	f := newCoverageResolverFixture()
	f.resolver.db = &errRulesStore{coverageFakeStore: *f.store}
	_, err := f.resolver.Explain(context.Background(), &core.RoutingContext{
		RequestedModel: core.RequestedModel{ID: "gpt-4"},
	})
	if err == nil {
		t.Error("Explain must surface Resolve error")
	}
}

// TestExplain_RefetchRulesError_FallsBackToPlanOnly: when the second
// GetEnabledRoutingRules call inside Explain errors, Explain returns the
// plan from Resolve unchanged (nilerr path).

func TestExplain_RefetchRulesError_FallsBackToPlanOnly(t *testing.T) {
	f := newCoverageResolverFixture()
	f.addProvider("openai", true)
	f.addModel("gpt-4", "openai", "gpt-4", true)
	f.addRule(store.RoutingRule{
		ID:            "r-primary",
		StrategyType:  "single",
		PipelineStage: 1,
		Config:        mustJSON(t, core.StrategyNode{Type: "single", ProviderID: "openai", ModelID: "gpt-4"}),
	})
	// Swap to a store that succeeds for Resolve but fails on subsequent
	// rules calls. We cheat by sharing call-count state.
	tw := &twoCallStore{base: f.store, rulesOnFirstCall: f.store.rules}
	f.resolver.db = tw
	plan, err := f.resolver.Explain(context.Background(), &core.RoutingContext{
		RequestedModel: core.RequestedModel{ID: "gpt-4"},
	})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if len(plan.Branches) != 0 {
		t.Errorf("refetch-rules error path must skip Branches population, got %+v", plan.Branches)
	}
	if plan.RuleID != "r-primary" {
		t.Errorf("primary rule id should still be present, got %q", plan.RuleID)
	}
}

type twoCallStore struct {
	base             *coverageFakeStore
	rulesOnFirstCall []store.RoutingRule
	called           int
}

func (s *twoCallStore) GetEnabledRoutingRules(_ context.Context) ([]store.RoutingRule, error) {
	s.called++
	if s.called == 1 {
		return s.rulesOnFirstCall, nil
	}
	return nil, errors.New("cache evicted")
}
func (s *twoCallStore) GetProviderAndModel(ctx context.Context, p, m string) (*store.Provider, *store.Model, error) {
	return s.base.GetProviderAndModel(ctx, p, m)
}
func (s *twoCallStore) GetModel(ctx context.Context, id string) (*store.Model, error) {
	return s.base.GetModel(ctx, id)
}
func (s *twoCallStore) ResolveModelCandidates(ctx context.Context, c string) ([]store.Model, error) {
	return s.base.ResolveModelCandidates(ctx, c)
}

// TestExplain_RuleConfigBadOnRefetch_FallsBackToPlanOnly: second-pass JSON
// of the matched rule fails to unmarshal — Explain returns plan unchanged
// (nilerr "stale rule config" path).

func TestExplain_RuleConfigBadOnRefetch_FallsBackToPlanOnly(t *testing.T) {
	f := newCoverageResolverFixture()
	f.addProvider("openai", true)
	f.addModel("gpt-4", "openai", "gpt-4", true)
	// First pass: valid config so Resolve succeeds.
	good := store.RoutingRule{
		ID:            "r-primary",
		StrategyType:  "single",
		PipelineStage: 1,
		Config:        mustJSON(t, core.StrategyNode{Type: "single", ProviderID: "openai", ModelID: "gpt-4"}),
	}
	bad := good
	bad.Config = json.RawMessage(`{not-json`)
	tw := &twoCallStoreSwap{base: f.store, first: []store.RoutingRule{good}, second: []store.RoutingRule{bad}}
	f.resolver.db = tw
	plan, err := f.resolver.Explain(context.Background(), &core.RoutingContext{
		RequestedModel: core.RequestedModel{ID: "gpt-4"},
	})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if len(plan.Branches) != 0 {
		t.Errorf("expected no Branches on stale-config path, got %+v", plan.Branches)
	}
}

type twoCallStoreSwap struct {
	base   *coverageFakeStore
	first  []store.RoutingRule
	second []store.RoutingRule
	called int
}

func (s *twoCallStoreSwap) GetEnabledRoutingRules(_ context.Context) ([]store.RoutingRule, error) {
	s.called++
	if s.called == 1 {
		return s.first, nil
	}
	return s.second, nil
}
func (s *twoCallStoreSwap) GetProviderAndModel(ctx context.Context, p, m string) (*store.Provider, *store.Model, error) {
	return s.base.GetProviderAndModel(ctx, p, m)
}
func (s *twoCallStoreSwap) GetModel(ctx context.Context, id string) (*store.Model, error) {
	return s.base.GetModel(ctx, id)
}
func (s *twoCallStoreSwap) ResolveModelCandidates(ctx context.Context, c string) ([]store.Model, error) {
	return s.base.ResolveModelCandidates(ctx, c)
}

// TestResolveTargets_FlattensAndHealthRanks runs the full ResolveTargets
// pipeline end-to-end: primary + recovery flattened, then health-ranker
// sinks the unhealthy provider to the end.

func TestResolveTargets_FlattensAndHealthRanks(t *testing.T) {
	f := newCoverageResolverFixture()
	f.addProvider("openai", true)
	f.addProvider("anthropic", true)
	f.addModel("gpt-4", "openai", "gpt-4", true)
	f.addModel("claude-3", "anthropic", "claude-3", true)

	chain := []core.FallbackChainEntry{{ProviderID: "anthropic", ModelID: "claude-3"}}
	f.addRule(store.RoutingRule{
		ID:            "r-primary",
		StrategyType:  "single",
		PipelineStage: 1,
		Config:        mustJSON(t, core.StrategyNode{Type: "single", ProviderID: "openai", ModelID: "gpt-4"}),
		FallbackChain: mustJSON(t, chain),
	})

	// Mark openai unhealthy so ranker should sink it to the end.
	tracker := store.NewHealthTracker()
	t.Cleanup(tracker.Stop)
	for range 20 {
		tracker.RecordFailure("openai", "openai", 100)
	}
	awaitStatus(t, tracker, "openai", store.HealthStatusUnavailable)
	f.resolver.healthRanker = core.NewHealthRanker(tracker)

	res, err := f.resolver.ResolveTargets(context.Background(), &core.RoutingContext{
		RequestedModel: core.RequestedModel{ID: "gpt-4"},
	})
	if err != nil {
		t.Fatalf("ResolveTargets: %v", err)
	}
	if len(res.Targets) != 2 {
		t.Fatalf("expected 2 flattened targets, got %d", len(res.Targets))
	}
	if res.Targets[0].ProviderID != "anthropic" {
		t.Errorf("health-ranker should sink unhealthy openai; got order %s -> %s",
			res.Targets[0].ProviderID, res.Targets[1].ProviderID)
	}
	if res.RuleID != "r-primary" {
		t.Errorf("RuleID not propagated: %q", res.RuleID)
	}
}

// TestResolveTargets_ResolveError_Propagates verifies the error path.

func TestResolveTargets_ResolveError_Propagates(t *testing.T) {
	f := newCoverageResolverFixture()
	f.resolver.db = &errRulesStore{coverageFakeStore: *f.store}
	_, err := f.resolver.ResolveTargets(context.Background(), &core.RoutingContext{
		RequestedModel: core.RequestedModel{ID: "gpt-4"},
	})
	if err == nil {
		t.Error("ResolveTargets must surface Resolve error")
	}
}

// TestResolveTargets_NoHealthRanker_PassesThrough verifies the nil-ranker
// branch in ResolveTargets keeps the order Resolve produced.

func TestResolveTargets_NoHealthRanker_PassesThrough(t *testing.T) {
	f := newCoverageResolverFixture()
	f.addProvider("openai", true)
	f.addModel("gpt-4", "openai", "gpt-4", true)
	f.addRule(store.RoutingRule{
		ID:            "r-primary",
		StrategyType:  "single",
		PipelineStage: 1,
		Config:        mustJSON(t, core.StrategyNode{Type: "single", ProviderID: "openai", ModelID: "gpt-4"}),
	})
	// healthRanker stays nil (default from newResolverFixture).
	res, err := f.resolver.ResolveTargets(context.Background(), &core.RoutingContext{
		RequestedModel: core.RequestedModel{ID: "gpt-4"},
	})
	if err != nil {
		t.Fatalf("ResolveTargets: %v", err)
	}
	if len(res.Targets) != 1 || res.Targets[0].ProviderID != "openai" {
		t.Errorf("nil-ranker passthrough wrong: %+v", res.Targets)
	}
}

// smart_store.go — full coverage of the SmartStore adapter.

// fakeSmartCatalog implements SmartCatalog with scripted models and providers.
type fakeSmartCatalog struct {
	models    []store.Model
	providers map[string]*store.Provider
	modelsErr error
}

func (f *fakeSmartCatalog) ListEnabledModels(_ context.Context) ([]store.Model, error) {
	if f.modelsErr != nil {
		return nil, f.modelsErr
	}
	return f.models, nil
}
func (f *fakeSmartCatalog) GetProvider(_ context.Context, id string) (*store.Provider, error) {
	p, ok := f.providers[id]
	if !ok {
		return nil, errors.New("provider not found")
	}
	return p, nil
}

// TestSmartStore_ListEnabledChatModels_FiltersTypeAndDisabledProviders
// covers the chat-only filter, the providerCache hit on second iteration,
// the disabled-provider skip, and the GetProvider error skip.

func TestSmartStore_ListEnabledChatModels_FiltersTypeAndDisabledProviders(t *testing.T) {
	cat := &fakeSmartCatalog{
		models: []store.Model{
			// chat + enabled provider — should appear.
			{ID: "m-chat-1", Code: "chat-1", Name: "Chat One", ProviderID: "p-enabled", ProviderModelID: "chat-1", Type: "chat"},
			// chat + same provider (exercises cache hit).
			{ID: "m-chat-2", Code: "chat-2", Name: "Chat Two", ProviderID: "p-enabled", ProviderModelID: "chat-2", Type: "chat"},
			// embedding type — filtered.
			{ID: "m-emb", Code: "emb", Name: "Emb", ProviderID: "p-enabled", ProviderModelID: "emb", Type: "embedding"},
			// chat + disabled provider — filtered.
			{ID: "m-chat-3", Code: "chat-3", Name: "Chat Three", ProviderID: "p-disabled", ProviderModelID: "chat-3", Type: "chat"},
			// chat + missing provider — GetProvider errors, model skipped.
			{ID: "m-chat-4", Code: "chat-4", Name: "Chat Four", ProviderID: "p-missing", ProviderModelID: "chat-4", Type: "chat"},
		},
		providers: map[string]*store.Provider{
			"p-enabled":  {ID: "p-enabled", Name: "EnabledCo", Enabled: true},
			"p-disabled": {ID: "p-disabled", Name: "DisabledCo", Enabled: false},
		},
	}
	store := core.NewSmartStoreDB(cat)
	rows, err := store.ListEnabledChatModels(context.Background())
	if err != nil {
		t.Fatalf("ListEnabledChatModels: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 chat models with enabled providers, got %d (%+v)", len(rows), rows)
	}
	for _, r := range rows {
		if r.ProviderID != "p-enabled" {
			t.Errorf("only enabled-provider rows expected, got %+v", r)
		}
		if r.ModelCode == "" || r.ModelID == "" {
			t.Errorf("expected ModelCode + ModelID populated: %+v", r)
		}
	}
}

// TestSmartStore_ListEnabledChatModels_ModelsErrorWrapped: an error from
// ListEnabledModels is wrapped, not silently swallowed.

func TestSmartStore_ListEnabledChatModels_ModelsErrorWrapped(t *testing.T) {
	cat := &fakeSmartCatalog{modelsErr: errors.New("db down")}
	s := core.NewSmartStoreDB(cat)
	_, err := s.ListEnabledChatModels(context.Background())
	if err == nil || !strings.Contains(err.Error(), "list models") {
		t.Errorf("expected wrapped list-models error, got %v", err)
	}
}

// strategy.go + variants — error / edge paths.

// TestRegisterAllStrategies_WithSmartDeps registers the smart strategy
// when smartDeps is non-nil and exercises a lookup through the registry.

func TestRegisterAllStrategies_WithSmartDeps(t *testing.T) {
	reg := strategies.NewStrategyRegistry()
	// Verify smart strategy is registered by evaluating a "smart" node and
	// expecting a non-"unknown strategy" error (the eval may fail for other
	// reasons without a real LLM, but the type must be found).
	strategies.RegisterAllStrategies(reg, coverageMockLookup, &strategies.SmartDeps{
		Store:  &coverageFakeSmartStore{},
		Lookup: coverageMockLookup,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	// Verify by running with nil smartDeps → no smart strategy → evaluate returns "unknown strategy".
	reg2 := strategies.NewStrategyRegistry()
	strategies.RegisterAllStrategies(reg2, coverageMockLookup, nil)
	var trace []core.TraceEntry
	_, err := reg2.Evaluate(context.Background(), core.StrategyNode{Type: "smart"}, &core.RoutingContext{}, &trace, 0)
	if err == nil || !strings.Contains(err.Error(), "unknown strategy") {
		t.Errorf("without smartDeps, smart strategy should be unknown; got: %v", err)
	}
	// With smartDeps, the smart strategy IS registered (even if it can't fully run without decider).
	var trace2 []core.TraceEntry
	_, err2 := reg.Evaluate(context.Background(), core.StrategyNode{Type: "smart"}, &core.RoutingContext{}, &trace2, 0)
	if err2 != nil && strings.Contains(err2.Error(), "unknown strategy") {
		t.Error("smart strategy was not registered when smartDeps != nil")
	}
}

// TestFallbackStrategy_RecurseErrorPropagates: an inner strategy that
// returns an error bubbles up through the fallback iterator.

func TestFallbackStrategy_RecurseErrorPropagates(t *testing.T) {
	reg := strategies.NewStrategyRegistry()
	strategies.RegisterAllStrategies(reg, coverageMockLookup, nil)
	var trace []core.TraceEntry
	_, err := reg.Evaluate(
		context.Background(),
		core.StrategyNode{Type: "fallback", Targets: []core.StrategyNode{
			{Type: "ghost"}, // unknown -> err
		}},
		&core.RoutingContext{},
		&trace,
		0,
	)
	if err == nil {
		t.Error("fallback should surface inner strategy errors")
	}
}

// TestLoadbalanceStrategy_RecurseErrorPropagates: an inner strategy
// erroring inside the chosen weighted branch returns the error verbatim.

func TestLoadbalanceStrategy_RecurseErrorPropagates(t *testing.T) {
	reg := strategies.NewStrategyRegistry()
	strategies.RegisterAllStrategies(reg, coverageMockLookup, nil)
	var trace []core.TraceEntry
	_, err := reg.Evaluate(
		context.Background(),
		core.StrategyNode{Type: "loadbalance", Weighted: []core.WeightedTarget{
			{Weight: 1, Node: core.StrategyNode{Type: "ghost"}},
		}},
		&core.RoutingContext{},
		&trace,
		0,
	)
	if err == nil {
		t.Error("loadbalance should surface inner errors")
	}
}

// TestConditionalStrategy_NoBranchNoDefault_EmitsTrace: pin the
// terminal-trace path when no branch matches AND no default is set.

func TestConditionalStrategy_NoBranchNoDefault_EmitsTrace(t *testing.T) {
	reg := strategies.NewStrategyRegistry()
	strategies.RegisterAllStrategies(reg, coverageMockLookup, nil)
	var trace []core.TraceEntry
	targets, err := reg.Evaluate(
		context.Background(),
		core.StrategyNode{
			Type: "conditional",
			Conditions: []core.ConditionalBranch{
				{When: map[string]any{"requestedModel.type": "image"}, Then: core.StrategyNode{Type: "single", ProviderID: "p", ModelID: "m"}},
			},
		},
		&core.RoutingContext{RequestedModel: core.RequestedModel{Type: "chat"}},
		&trace,
		0,
	)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if len(targets) != 0 {
		t.Errorf("expected 0 targets, got %d", len(targets))
	}
	if len(trace) != 1 || !strings.Contains(trace[0].Decision, "no default") {
		t.Errorf("expected 'no default' trace, got %+v", trace)
	}
}

// strategy_smart.go — full negative-case + helper coverage.

// TestSmartConfig_DefaultsFromZeroValues: temperature defaults to 0
// when nil, maxTokens defaults to 1024 when 0, timeoutMs defaults to
// 10000 when 0.

// enumLookup is a no-op TargetLookup for enumerate tests.
func enumLookup(_ context.Context, providerID, modelID string) (*core.RoutingTarget, error) {
	return &core.RoutingTarget{ProviderID: providerID, ModelID: modelID, ModelCode: modelID}, nil
}

func TestEnumerate_ABSplit_LookupNilNoError(t *testing.T) {
	nilLookup := func(_ context.Context, _, _ string) (*core.RoutingTarget, error) {
		return nil, nil
	}
	node := core.StrategyNode{Type: "ab_split", ABTargets: []core.ABTarget{
		{ProviderID: "p", ModelID: "m", Weight: 1},
	}}
	branches := matcher.EnumerateTerminalTargets(context.Background(), node, &core.RoutingContext{}, nilLookup)
	if len(branches) != 1 {
		t.Fatalf("expected 1 ab_split branch, got %d", len(branches))
	}
	if !strings.Contains(branches[0].Note, "nil target") {
		t.Errorf("expected nil-target note, got %q", branches[0].Note)
	}
}

// TestEnumerate_ABSplit_Empty returns nil cleanly.

func TestEnumerate_ABSplit_Empty(t *testing.T) {
	node := core.StrategyNode{Type: "ab_split"}
	if branches := matcher.EnumerateTerminalTargets(context.Background(), node, &core.RoutingContext{}, enumLookup); branches != nil {
		t.Errorf("empty ab_split should return nil, got %+v", branches)
	}
}

// TestEnumerate_ABSplit_ZeroWeight_ReturnsNil

func TestEnumerate_LoadbalanceEmpty_ReturnsNil(t *testing.T) {
	node := core.StrategyNode{Type: "loadbalance"}
	if branches := matcher.EnumerateTerminalTargets(context.Background(), node, &core.RoutingContext{}, enumLookup); branches != nil {
		t.Errorf("empty loadbalance should return nil, got %+v", branches)
	}
}

// TestEvaluateExpression_ImplicitEqViaNonMap walks the default arm of the
// outer switch in EvaluateExpression — when the condition value is not a
// map, $eq semantics apply.

func TestEvaluateExpression_ImplicitEqViaNonMap(t *testing.T) {
	ctx := &core.RoutingContext{RequestedModel: core.RequestedModel{Type: "chat"}}
	if !matcher.EvaluateExpression(map[string]any{"requestedModel.type": "chat"}, ctx) {
		t.Error("implicit $eq should match")
	}
	if matcher.EvaluateExpression(map[string]any{"requestedModel.type": "image"}, ctx) {
		t.Error("implicit $eq should fail")
	}
}

// TestNarrowingEngine_Filter_DropsByVKAllowed: the Filter method on
// NarrowingEngine handles the VK whitelist branch directly (parallel to
// the resolver-level test which exercises it end-to-end).

func TestMergePolicy_EmptyNodeNoOp(t *testing.T) {
	state := matcher.EmptyNarrowingState()
	got := matcher.MergePolicyIntoState(state, core.StrategyNode{Type: "policy"})
	if !matcher.IsNarrowingEmpty(got) {
		t.Errorf("empty policy node should be a no-op: %+v", got)
	}
}

// TestSafeHeaders_BlocksAllVariants exercises the all-three-blocked-list
// path; existing tests only check Authorization.

func TestSafeHeaders_BlocksAllVariants(t *testing.T) {
	in := http.Header{}
	in.Set("Cookie", "c")
	in.Set("X-Api-Key", "k")
	in.Set("Authorization", "a")
	in.Set("User-Agent", "u")
	s := core.NewSafeHeaders(in)
	for _, name := range []string{"Cookie", "X-Api-Key", "Authorization"} {
		if got := s.Get(name); got != "" {
			t.Errorf("blocked header %q leaked: %q", name, got)
		}
	}
	if s.Get("User-Agent") != "u" {
		t.Error("non-blocked header should pass through")
	}
}

// Ensure tests don't share namespace pollution; verify ExtractCleanup.
var _ = strings.Contains // keep strings import referenced even after edits

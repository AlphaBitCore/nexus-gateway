package strategies

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/llm"
)

func TestSmartConfig_DefaultsFromZeroValues(t *testing.T) {
	zero := &SmartConfig{}
	if zero.temperature() != 0 {
		t.Errorf("default temperature = %v, want 0", zero.temperature())
	}
	if zero.maxTokens() != 1024 {
		t.Errorf("default maxTokens = %d, want 1024", zero.maxTokens())
	}
	if zero.timeoutMs() != 10000 {
		t.Errorf("default timeoutMs = %d, want 10000", zero.timeoutMs())
	}

	// Non-zero overrides.
	temp := 0.7
	set := &SmartConfig{Temperature: &temp, MaxTokens: 256, TimeoutMs: 500}
	if set.temperature() != 0.7 || set.maxTokens() != 256 || set.timeoutMs() != 500 {
		t.Errorf("SmartConfig overrides not honored: temp=%v max=%d timeout=%d",
			set.temperature(), set.maxTokens(), set.timeoutMs())
	}
}

// TestSmartStrategy_TypeAndMissingConfig pin the Type() string + the
// missing-router-config short-circuit branch.

func TestSmartStrategy_TypeAndMissingConfig(t *testing.T) {
	strat := &SmartStrategy{deps: SmartDeps{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}}
	if strat.Type() != "smart" {
		t.Errorf("Type=%q want smart", strat.Type())
	}
	var trace []core.TraceEntry
	out, err := strat.Evaluate(context.Background(), core.StrategyNode{Type: "smart"}, aiChatRctx(), &trace, 0, nil)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if out != nil {
		t.Errorf("missing router config must return nil targets, got %+v", out)
	}
	if len(trace) != 1 || !strings.Contains(trace[0].Decision, "missing smart config") {
		t.Errorf("expected missing-config trace, got %+v", trace)
	}
}

// TestSmartStrategy_StoreError_FallsBackToDefault: when ListEnabledChatModels
// errors, the strategy uses smartFallback to return the configured default.

func TestSmartStrategy_StoreError_FallsBackToDefault(t *testing.T) {
	candidates := []core.SmartModelRow{
		{ModelID: "m-def", ProviderID: "p-def", ProviderName: "def"},
	}
	fx := newSmartFixture(t, &fakeDecider{}, candidates)
	fx.store.err = errors.New("store down")
	node := core.StrategyNode{
		RouterProviderID:  "p-r",
		RouterModelID:     "m-r",
		DefaultProviderID: "p-def",
		DefaultModelID:    "m-def",
	}
	var trace []core.TraceEntry
	strat := &SmartStrategy{deps: fx.deps()}
	out, err := strat.Evaluate(context.Background(), node, aiChatRctx(), &trace, 0, nil)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(out) != 1 || out[0].ModelID != "m-def" {
		t.Errorf("expected default fallback, got %+v", out)
	}
}

// TestSmartStrategy_VKAllowedModelsFilter_LeavesEmptyCandidates: when the
// VK's AllowedModels list excludes every catalog row, candidates becomes
// empty and the strategy falls back.

func TestSmartStrategy_VKAllowedModelsFilter_LeavesEmptyCandidates(t *testing.T) {
	candidates := []core.SmartModelRow{
		{ModelID: "m-1", ProviderID: "p-1", ProviderModelID: "code-1"},
		{ModelID: "m-def", ProviderID: "p-def", ProviderName: "def", ProviderModelID: "default"},
	}
	fx := newSmartFixture(t, &fakeDecider{}, candidates)
	// Decider must not be invoked when no candidates survive the filter.
	node := core.StrategyNode{
		RouterProviderID:  "p-r",
		RouterModelID:     "m-r",
		DefaultProviderID: "p-def",
		DefaultModelID:    "m-def",
	}
	rctx := &core.RoutingContext{
		Request: aiChatRctx().Request,
		VirtualKey: &core.VKContext{
			AllowedModels: []store.AllowedModelRef{
				{ProviderID: "ghost-provider", ModelID: "ghost"},
			},
		},
	}
	var trace []core.TraceEntry
	strat := &SmartStrategy{deps: fx.deps()}
	out, err := strat.Evaluate(context.Background(), node, rctx, &trace, 0, nil)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if fx.decider.calls != 0 {
		t.Errorf("decider must not be called when candidates list is empty")
	}
	if len(out) != 1 || out[0].ModelID != "m-def" {
		t.Errorf("expected default fallback, got %+v", out)
	}
	if !traceContains(trace, "no candidate models available") {
		t.Errorf("expected 'no candidate' trace, got %+v", trace)
	}
}

// TestSmartStrategy_NilDecider_FallsBack: a nil RouterLLM short-circuits
// the strategy to the fallback path with an explicit trace.

func TestSmartStrategy_NilDecider_FallsBack(t *testing.T) {
	candidates := []core.SmartModelRow{
		{ModelID: "m-def", ProviderID: "p-def", ProviderName: "def", ProviderModelID: "default"},
	}
	fx := newSmartFixture(t, &fakeDecider{}, candidates)
	fx.decider = nil
	deps := SmartDeps{
		Store:  fx.store,
		Lookup: fx.lookup,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	node := core.StrategyNode{
		RouterProviderID:  "p-r",
		RouterModelID:     "m-r",
		DefaultProviderID: "p-def",
		DefaultModelID:    "m-def",
	}
	var trace []core.TraceEntry
	strat := &SmartStrategy{deps: deps}
	out, err := strat.Evaluate(context.Background(), node, aiChatRctx(), &trace, 0, nil)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(out) != 1 || out[0].ModelID != "m-def" {
		t.Errorf("expected default fallback, got %+v", out)
	}
	if !traceContains(trace, "router LLM client not wired") {
		t.Errorf("expected 'not wired' trace, got %+v", trace)
	}
}

// TestSmartStrategy_UnknownModelFromDecider_FallsBack: the router LLM
// returns a token that resolveSelectedModelID cannot map back. Strategy
// falls back to the default with a "unknown model" trace.

func TestSmartStrategy_UnknownModelFromDecider_FallsBack(t *testing.T) {
	decider := &fakeDecider{decision: llm.Decision{ModelID: "ghost-token", Reason: "no idea"}}
	candidates := []core.SmartModelRow{
		{ModelID: "m-1", ProviderID: "p-1", ProviderModelID: "code-1", ModelCode: "code-1"},
		{ModelID: "m-def", ProviderID: "p-def", ProviderName: "def", ProviderModelID: "default", ModelCode: "default"},
	}
	fx := newSmartFixture(t, decider, candidates)
	node := core.StrategyNode{
		RouterProviderID:  "p-r",
		RouterModelID:     "m-r",
		DefaultProviderID: "p-def",
		DefaultModelID:    "m-def",
	}
	var trace []core.TraceEntry
	strat := &SmartStrategy{deps: fx.deps()}
	out, err := strat.Evaluate(context.Background(), node, aiChatRctx(), &trace, 0, nil)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(out) != 1 || out[0].ModelID != "m-def" {
		t.Errorf("expected fallback to default, got %+v", out)
	}
	if !traceContains(trace, "unknown model") {
		t.Errorf("expected 'unknown model' trace, got %+v", trace)
	}
}

// TestSmartStrategy_LookupFailureOnSelected_FallsBack: the router returns
// a valid candidate token, but the Lookup call for that target errors.
// Strategy falls back with a "target lookup failed" trace.

func TestSmartStrategy_LookupFailureOnSelected_FallsBack(t *testing.T) {
	decider := &fakeDecider{decision: llm.Decision{ModelID: "code-x", Reason: "ok"}}
	candidates := []core.SmartModelRow{
		{ModelID: "m-x", ProviderID: "p-x", ProviderModelID: "code-x", ModelCode: "code-x"},
		{ModelID: "m-def", ProviderID: "p-def", ProviderName: "def", ProviderModelID: "default", ModelCode: "default"},
	}
	failingLookup := func(_ context.Context, _, mid string) (*core.RoutingTarget, error) {
		if mid == "m-x" {
			return nil, errors.New("lookup boom")
		}
		// Allow default lookup to succeed so smartFallback returns a target.
		return &core.RoutingTarget{ProviderID: "p-def", ModelID: mid}, nil
	}
	deps := SmartDeps{
		Store:     &fakeSmartStore{rows: candidates},
		Lookup:    failingLookup,
		RouterLLM: decider,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	node := core.StrategyNode{
		RouterProviderID:  "p-r",
		RouterModelID:     "m-r",
		DefaultProviderID: "p-def",
		DefaultModelID:    "m-def",
	}
	var trace []core.TraceEntry
	strat := &SmartStrategy{deps: deps}
	out, err := strat.Evaluate(context.Background(), node, aiChatRctx(), &trace, 0, nil)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(out) != 1 || out[0].ModelID != "m-def" {
		t.Errorf("expected default fallback, got %+v", out)
	}
	if !traceContains(trace, "target lookup failed") {
		t.Errorf("expected 'target lookup failed' trace, got %+v", trace)
	}
}

// TestSmartFallback_NoDefaultConfigured_NoTargets: smartFallback returns
// no targets when DefaultProviderID/DefaultModelID are blank.

func TestSmartFallback_NoDefaultConfigured_NoTargets(t *testing.T) {
	decider := &fakeDecider{err: errors.New("router unreachable")}
	candidates := []core.SmartModelRow{
		{ModelID: "m-x", ProviderID: "p-x", ProviderModelID: "code-x", ModelCode: "code-x"},
	}
	fx := newSmartFixture(t, decider, candidates)
	node := core.StrategyNode{RouterProviderID: "p-r", RouterModelID: "m-r"}
	var trace []core.TraceEntry
	strat := &SmartStrategy{deps: fx.deps()}
	out, err := strat.Evaluate(context.Background(), node, aiChatRctx(), &trace, 0, nil)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("no default configured -> no targets, got %+v", out)
	}
}

// TestSmartFallback_DefaultLookupError_NoTargets: smartFallback whose
// default lookup itself errors returns nil (best-effort).

func TestSmartFallback_DefaultLookupError_NoTargets(t *testing.T) {
	decider := &fakeDecider{err: errors.New("router boom")}
	candidates := []core.SmartModelRow{
		{ModelID: "m-x", ProviderID: "p-x", ProviderModelID: "code-x", ModelCode: "code-x"},
	}
	failingLookup := func(_ context.Context, _, _ string) (*core.RoutingTarget, error) {
		return nil, errors.New("everything broken")
	}
	deps := SmartDeps{
		Store:     &fakeSmartStore{rows: candidates},
		Lookup:    failingLookup,
		RouterLLM: decider,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	node := core.StrategyNode{
		RouterProviderID:  "p-r",
		RouterModelID:     "m-r",
		DefaultProviderID: "p-def",
		DefaultModelID:    "m-def",
	}
	var trace []core.TraceEntry
	strat := &SmartStrategy{deps: deps}
	out, err := strat.Evaluate(context.Background(), node, aiChatRctx(), &trace, 0, nil)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("default lookup error -> no targets, got %+v", out)
	}
}

// TestResolveSelectedModelID_NoMatchInScopedNarrowing covers the "no
// candidates in scoped provider" branch.

func TestResolveSelectedModelID_NoMatchInScopedNarrowing(t *testing.T) {
	candidates := []core.SmartModelRow{
		{ModelID: "m-1", ProviderID: "p-other", ModelCode: "code-1"},
	}
	if _, ok := resolveSelectedModelID("code-1", "p-missing", candidates); ok {
		t.Error("scoped provider with no matching rows must return ok=false")
	}
	// Empty candidates entirely.
	if _, ok := resolveSelectedModelID("anything", "", nil); ok {
		t.Error("empty candidates must return ok=false")
	}
}

// TestBuildModelCatalog_EmptyInput verifies the helper handles an empty
// candidates list deterministically (returns "[]" rather than nil).

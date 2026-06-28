package strategies

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/llm"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// aiChatRctx builds the minimal core.RoutingContext that passes both S5
// negative-case guards: AI-kind payload with one role=user message. Used
// by happy-path and other-error tests where the negative-case branches
// must NOT trigger.
func aiChatRctx() *core.RoutingContext {
	return &core.RoutingContext{Request: &normalize.NormalizedPayload{
		Kind: normalize.KindAIChat,
		Messages: []normalize.Message{
			{Role: normalize.RoleUser, Content: []normalize.ContentBlock{{Type: normalize.ContentText, Text: "hello"}}},
		},
	}}
}

// fakeSmartStore implements SmartStore with scripted candidates.
type fakeSmartStore struct {
	rows []core.SmartModelRow
	err  error
}

func (f *fakeSmartStore) ListEnabledChatModels(_ context.Context) ([]core.SmartModelRow, error) {
	return f.rows, f.err
}

// fakeDecider implements llm.Decider with scripted return values
// and records the last Request seen so tests can assert on the inputs
// the strategy handed across the interface.
type fakeDecider struct {
	decision llm.Decision
	err      error
	calls    int
	lastReq  llm.Request
}

func (f *fakeDecider) Decide(_ context.Context, req llm.Request) (llm.Decision, error) {
	f.calls++
	f.lastReq = req
	if f.err != nil {
		return llm.Decision{}, f.err
	}
	return f.decision, nil
}

// smartFixture assembles the minimal dependencies for SmartStrategy.Evaluate.
type smartFixture struct {
	store   *fakeSmartStore
	decider *fakeDecider
	lookup  core.TargetLookup
}

func newSmartFixture(t *testing.T, decider *fakeDecider, candidates []core.SmartModelRow) *smartFixture {
	t.Helper()
	lookup := func(_ context.Context, _, mid string) (*core.RoutingTarget, error) {
		for _, c := range candidates {
			if c.ModelID == mid {
				return &core.RoutingTarget{
					ProviderID:      c.ProviderID,
					ProviderName:    c.ProviderName,
					ModelID:         c.ModelID,
					ProviderModelID: c.ProviderModelID,
				}, nil
			}
		}
		return nil, errors.New("not found")
	}
	return &smartFixture{
		store:   &fakeSmartStore{rows: candidates},
		decider: decider,
		lookup:  lookup,
	}
}

func (f *smartFixture) deps() SmartDeps {
	return SmartDeps{
		Store:     f.store,
		Lookup:    f.lookup,
		RouterLLM: f.decider,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// TestSmart_HappyPath_PicksRouterDecision exercises the strategy
// end-to-end against the fakeDecider: candidates list -> filter -> build
// catalog -> hand prompt + user messages to Decider -> resolve picked
// model code against candidates -> return core.RoutingTarget. The Decider
// receives a non-empty system prompt with the catalog inlined and the
// configured Router IDs.
func TestSmart_HappyPath_PicksRouterDecision(t *testing.T) {
	decider := &fakeDecider{
		decision: llm.Decision{ModelID: "m-claude", Reason: "best fit"},
	}
	candidates := []core.SmartModelRow{
		{ModelID: "m-claude", ModelName: "Claude", ProviderID: "p-anthropic", ProviderName: "fake-anthropic", ProviderModelID: "claude-3-opus"},
	}
	fx := newSmartFixture(t, decider, candidates)
	node := core.StrategyNode{RouterProviderID: "p-router", RouterModelID: "m-router"}
	trace := []core.TraceEntry{}
	strat := &SmartStrategy{deps: fx.deps()}

	out, err := strat.Evaluate(context.Background(), node, aiChatRctx(), &trace, 0, nil)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(out) != 1 || out[0].ModelID != "m-claude" {
		t.Fatalf("unexpected targets: %+v", out)
	}
	if decider.calls != 1 {
		t.Fatalf("expected 1 decider call, got %d", decider.calls)
	}
	if decider.lastReq.RouterProviderID != "p-router" || decider.lastReq.RouterModelID != "m-router" {
		t.Errorf("router IDs not propagated: got providerID=%q modelID=%q",
			decider.lastReq.RouterProviderID, decider.lastReq.RouterModelID)
	}
	if decider.lastReq.SystemPrompt == "" {
		t.Errorf("expected non-empty system prompt with catalog inlined")
	}
}

// TestSmart_DeciderError_FallsBack covers the smart-strategy contract
// that any error from the Decider is projected into the trace verbatim
// (via err.Error()) and triggers smartFallback to the configured
// default model. The error vocabulary that AdapterDecider produces
// matches the pre-S4 trace strings (e.g. "router target resolve failed",
// "router LLM timeout (N ms)") so audit consumers stay byte-identical.
func TestSmart_DeciderError_FallsBack(t *testing.T) {
	decider := &fakeDecider{err: errors.New("router target resolve failed: vault offline")}
	candidates := []core.SmartModelRow{
		{ModelID: "m-gpt", ModelName: "GPT", ProviderID: "p-openai", ProviderName: "fake-openai", ProviderModelID: "gpt-4o-mini"},
	}
	fx := newSmartFixture(t, decider, candidates)
	node := core.StrategyNode{
		RouterProviderID:  "p-router",
		RouterModelID:     "m-router",
		DefaultProviderID: "p-openai",
		DefaultModelID:    "m-gpt",
	}
	trace := []core.TraceEntry{}
	strat := &SmartStrategy{deps: fx.deps()}

	out, err := strat.Evaluate(context.Background(), node, aiChatRctx(), &trace, 0, nil)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(out) != 1 || out[0].ModelID != "m-gpt" {
		t.Fatalf("expected fallback to default model, got %+v", out)
	}
	if decider.calls != 1 {
		t.Fatalf("expected 1 decider call, got %d", decider.calls)
	}
	// Trace should include the decider's error string verbatim.
	found := false
	for _, e := range trace {
		if strings.Contains(e.Decision, "router target resolve failed") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("trace must surface the decider error verbatim: %+v", trace)
	}
}

func TestSmart_RouterReturnsProviderModelID_UniqueMatchAccepted(t *testing.T) {
	decider := &fakeDecider{
		decision: llm.Decision{ModelID: "gpt-4o-latest", Reason: "best fit"},
	}
	candidates := []core.SmartModelRow{
		{ModelID: "m-openai-latest", ModelName: "GPT-4o Latest", ProviderID: "p-openai", ProviderName: "openai", ProviderModelID: "gpt-4o-latest"},
		{ModelID: "m-openai-mini", ModelName: "GPT-4o Mini", ProviderID: "p-openai", ProviderName: "openai", ProviderModelID: "gpt-4o-mini"},
	}
	fx := newSmartFixture(t, decider, candidates)
	node := core.StrategyNode{RouterProviderID: "p-router", RouterModelID: "m-router"}
	trace := []core.TraceEntry{}
	strat := &SmartStrategy{deps: fx.deps()}

	out, err := strat.Evaluate(context.Background(), node, aiChatRctx(), &trace, 0, nil)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(out) != 1 || out[0].ModelID != "m-openai-latest" {
		t.Fatalf("expected providerModelId mapping to internal model, got %+v", out)
	}
}

// TestSmart_NoNormalizedPayload_FallsBackWithExplicitTrace pins the
// negative case: rctx.Request is nil (e.g. handler skipped normalize
// because the body was empty). The strategy must fall back to default
// WITHOUT calling RouterLLM.Decide, and the trace must carry the exact
// "not normalizable" string operators grep for.
func TestSmart_NoNormalizedPayload_FallsBackWithExplicitTrace(t *testing.T) {
	decider := &fakeDecider{}
	candidates := []core.SmartModelRow{
		{ModelID: "m-default", ModelName: "Default", ProviderID: "p-openai", ProviderName: "openai", ProviderModelID: "gpt-4o-mini"},
	}
	fx := newSmartFixture(t, decider, candidates)
	node := core.StrategyNode{
		RouterProviderID:  "p-router",
		RouterModelID:     "m-router",
		DefaultProviderID: "p-openai",
		DefaultModelID:    "m-default",
	}
	trace := []core.TraceEntry{}
	strat := &SmartStrategy{deps: fx.deps()}

	out, err := strat.Evaluate(context.Background(), node, &core.RoutingContext{Request: nil}, &trace, 0, nil)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(out) != 1 || out[0].ModelID != "m-default" {
		t.Fatalf("expected fallback to default, got %+v", out)
	}
	if decider.calls != 0 {
		t.Fatalf("decider must NOT be called when payload is not normalizable; got %d calls", decider.calls)
	}
	wantSub := "request payload not normalizable for smart routing; using default"
	if !traceContains(trace, wantSub) {
		t.Errorf("trace missing %q; got %+v", wantSub, trace)
	}
}

// TestSmart_NonAIKindPayload_FallsBackWithExplicitTrace pins the same
// negative case for a non-AI Kind (e.g. /v1/models hits a smart rule
// with broad matchConditions).
func TestSmart_NonAIKindPayload_FallsBackWithExplicitTrace(t *testing.T) {
	decider := &fakeDecider{}
	candidates := []core.SmartModelRow{
		{ModelID: "m-default", ModelName: "Default", ProviderID: "p-openai", ProviderName: "openai", ProviderModelID: "gpt-4o-mini"},
	}
	fx := newSmartFixture(t, decider, candidates)
	node := core.StrategyNode{
		RouterProviderID:  "p-router",
		RouterModelID:     "m-router",
		DefaultProviderID: "p-openai",
		DefaultModelID:    "m-default",
	}
	trace := []core.TraceEntry{}
	strat := &SmartStrategy{deps: fx.deps()}
	rctx := &core.RoutingContext{Request: &normalize.NormalizedPayload{Kind: normalize.KindHTTPJSON}}

	out, err := strat.Evaluate(context.Background(), node, rctx, &trace, 0, nil)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(out) != 1 || out[0].ModelID != "m-default" {
		t.Fatalf("expected fallback to default, got %+v", out)
	}
	if decider.calls != 0 {
		t.Fatalf("decider must NOT be called for non-AI Kind; got %d calls", decider.calls)
	}
	wantSub := "request payload not normalizable for smart routing; using default"
	if !traceContains(trace, wantSub) {
		t.Errorf("trace missing %q; got %+v", wantSub, trace)
	}
}

// TestSmart_NoUserContent_FallsBackWithExplicitTrace pins the
// negative case: AI-kind payload with no role=user messages
// (assistant-only, tool-only). The strategy falls back with a trace
// distinct from "not normalizable" so operators can tell client-side
// from operator-config-side root cause apart.
func TestSmart_NoUserContent_FallsBackWithExplicitTrace(t *testing.T) {
	decider := &fakeDecider{}
	candidates := []core.SmartModelRow{
		{ModelID: "m-default", ModelName: "Default", ProviderID: "p-openai", ProviderName: "openai", ProviderModelID: "gpt-4o-mini"},
	}
	fx := newSmartFixture(t, decider, candidates)
	node := core.StrategyNode{
		RouterProviderID:  "p-router",
		RouterModelID:     "m-router",
		DefaultProviderID: "p-openai",
		DefaultModelID:    "m-default",
	}
	trace := []core.TraceEntry{}
	strat := &SmartStrategy{deps: fx.deps()}
	rctx := &core.RoutingContext{Request: &normalize.NormalizedPayload{
		Kind: normalize.KindAIChat,
		Messages: []normalize.Message{
			{Role: normalize.RoleSystem, Content: []normalize.ContentBlock{{Type: normalize.ContentText, Text: "you are helpful"}}},
			{Role: normalize.RoleAssistant, Content: []normalize.ContentBlock{{Type: normalize.ContentText, Text: "hi"}}},
		},
	}}

	out, err := strat.Evaluate(context.Background(), node, rctx, &trace, 0, nil)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(out) != 1 || out[0].ModelID != "m-default" {
		t.Fatalf("expected fallback to default, got %+v", out)
	}
	if decider.calls != 0 {
		t.Fatalf("decider must NOT be called when no role=user content present; got %d calls", decider.calls)
	}
	wantSub := "smart routing: no user content in request; using default"
	if !traceContains(trace, wantSub) {
		t.Errorf("trace missing %q; got %+v", wantSub, trace)
	}
}

func traceContains(trace []core.TraceEntry, substr string) bool {
	for _, e := range trace {
		if strings.Contains(e.Decision, substr) {
			return true
		}
	}
	return false
}

func TestSmart_RouterReturnsProviderModelID_AmbiguousFallsBack(t *testing.T) {
	decider := &fakeDecider{
		decision: llm.Decision{ModelID: "gpt-4o-mini", Reason: "best fit"},
	}
	candidates := []core.SmartModelRow{
		{ModelID: "m-openai-mini", ModelName: "GPT-4o Mini", ProviderID: "p-openai", ProviderName: "openai", ProviderModelID: "gpt-4o-mini"},
		{ModelID: "m-moonshot-mini", ModelName: "Moonshot Mini", ProviderID: "p-moonshot", ProviderName: "moonshot", ProviderModelID: "gpt-4o-mini"},
	}
	fx := newSmartFixture(t, decider, candidates)
	node := core.StrategyNode{
		RouterProviderID:  "p-router",
		RouterModelID:     "m-router",
		DefaultProviderID: "p-openai",
		DefaultModelID:    "m-openai-mini",
	}
	trace := []core.TraceEntry{}
	strat := &SmartStrategy{deps: fx.deps()}

	out, err := strat.Evaluate(context.Background(), node, aiChatRctx(), &trace, 0, nil)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(out) != 1 || out[0].ModelID != "m-openai-mini" {
		t.Fatalf("expected fallback default due to ambiguous providerModelId, got %+v", out)
	}
}

// TestSmartConfig_TimeoutMsDefault — the built-in router-LLM timeout
// default is 3000ms (3s). Smart routing sits on the request hot path, so the
// default must be low; 10s would stall every smart-routed request behind a slow
// router provider.
func TestSmartConfig_TimeoutMsDefault(t *testing.T) {
	cfg := &SmartConfig{} // TimeoutMs unset
	if got := cfg.timeoutMs(); got != 3000 {
		t.Fatalf("default timeoutMs = %d, want 3000", got)
	}
}

// TestSmartConfig_TimeoutMsOverride — an operator-provided TimeoutMs
// overrides the built-in default.
func TestSmartConfig_TimeoutMsOverride(t *testing.T) {
	cfg := &SmartConfig{TimeoutMs: 7500}
	if got := cfg.timeoutMs(); got != 7500 {
		t.Fatalf("override timeoutMs = %d, want 7500", got)
	}
}

// TestSmart_DefaultTimeoutPropagatesToDecider — the 3s default flows
// through Evaluate into the router-LLM call budget when the node omits TimeoutMs.
func TestSmart_DefaultTimeoutPropagatesToDecider(t *testing.T) {
	decider := &fakeDecider{
		decision: llm.Decision{ModelID: "gpt-4o-mini", Reason: "best fit"},
	}
	candidates := []core.SmartModelRow{
		{ModelID: "m-openai-mini", ModelName: "GPT-4o Mini", ProviderID: "p-openai", ProviderName: "openai", ProviderModelID: "gpt-4o-mini", ModelCode: "gpt-4o-mini"},
	}
	fx := newSmartFixture(t, decider, candidates)
	node := core.StrategyNode{
		RouterProviderID: "p-router",
		RouterModelID:    "m-router",
		// TimeoutMs intentionally omitted → built-in default applies.
	}
	trace := []core.TraceEntry{}
	strat := &SmartStrategy{deps: fx.deps()}

	if _, err := strat.Evaluate(context.Background(), node, aiChatRctx(), &trace, 0, nil); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if got := decider.lastReq.Timeout; got != 3000*time.Millisecond {
		t.Fatalf("router-LLM Timeout = %v, want 3s", got)
	}
}

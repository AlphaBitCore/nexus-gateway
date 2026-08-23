package strategies

import (
	"context"
	"errors"
	"fmt"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/matcher"
	"io"
	"log/slog"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"

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
	// calls counts catalogue reads, so a test can assert the strategy used the
	// pool the routing context already prepared instead of fetching its own.
	calls int
}

func (f *fakeSmartStore) ListEnabledChatModels(_ context.Context) ([]core.SmartModelRow, error) {
	f.calls++
	return f.rows, f.err
}

func (f *fakeSmartStore) ListEnabledCandidates(_ context.Context, _ typology.EndpointKind) ([]core.SmartModelRow, error) {
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
	rows    []core.SmartModelRow
}

// pool stamps the fixture's candidates onto a routing context, which is what
// Stage A does for every production request.
//
// Tests used to leave it nil and let the strategy fetch its own. That fallback
// is gone — it was unreachable in production, because the wiring hands the same
// store to the resolver's pool preparation and to the strategy — so a context
// with no pool now means "the catalogue could not be read", which is a
// different test.
func (f *smartFixture) pool(rctx *core.RoutingContext) *core.RoutingContext {
	return poolOf(rctx, f.rows)
}

// poolOf is the same stamp for tests that build their SmartDeps by hand.
func poolOf(rctx *core.RoutingContext, rows []core.SmartModelRow) *core.RoutingContext {
	if rctx != nil {
		rctx.ModelPool = rows
	}
	return rctx
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
		rows:    candidates,
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

	out, err := strat.Evaluate(context.Background(), node, fx.pool(aiChatRctx()), &trace)
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

// TestSmart_NonChatEndpoint_ModalityAutoNoLLM pins modality-aware auto: on a
// non-chat endpoint (image generation), model=auto resolves to every modality
// candidate deterministically and MUST NOT invoke the chat LLM task-router.
func TestSmart_NonChatEndpoint_ModalityAutoNoLLM(t *testing.T) {
	decider := &fakeDecider{decision: llm.Decision{ModelID: "must-not-be-used"}}
	candidates := []core.SmartModelRow{
		{ModelID: "img-1", ProviderID: "p-openai", ProviderName: "openai", ProviderModelID: "gpt-image-1"},
		{ModelID: "img-2", ProviderID: "p-gemini", ProviderName: "gemini", ProviderModelID: "gemini-2.5-flash-image"},
	}
	fx := newSmartFixture(t, decider, candidates)
	node := core.StrategyNode{RouterProviderID: "p-router", RouterModelID: "m-router"}
	trace := []core.TraceEntry{}
	strat := &SmartStrategy{deps: fx.deps()}
	rctx := &core.RoutingContext{EndpointType: typology.EndpointKindImageGeneration}

	out, err := strat.Evaluate(context.Background(), node, fx.pool(rctx), &trace)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("modality-auto must return every image candidate, got %+v", out)
	}
	if decider.calls != 0 {
		t.Fatalf("LLM router must NOT be called on a non-chat endpoint; got %d calls", decider.calls)
	}
}

// TestSmart_NonChatEndpoint_RespectsVKAllowlist verifies modality-aware auto
// still honours the VK allowed-models allowlist.
func TestSmart_NonChatEndpoint_RespectsVKAllowlist(t *testing.T) {
	candidates := []core.SmartModelRow{
		{ModelID: "img-1", ProviderID: "p-openai", ProviderName: "openai", ProviderModelID: "gpt-image-1"},
		{ModelID: "img-2", ProviderID: "p-gemini", ProviderName: "gemini", ProviderModelID: "gemini-2.5-flash-image"},
	}
	fx := newSmartFixture(t, &fakeDecider{}, candidates)
	trace := []core.TraceEntry{}
	strat := &SmartStrategy{deps: fx.deps()}
	rctx := &core.RoutingContext{
		EndpointType: typology.EndpointKindImageGeneration,
		VirtualKey:   &core.VKContext{AllowedModels: []store.AllowedModelRef{{ProviderID: "p-openai", ModelID: "img-1"}}},
	}

	out, err := strat.Evaluate(context.Background(), core.StrategyNode{}, fx.pool(rctx), &trace)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(out) != 1 || out[0].ModelID != "img-1" {
		t.Fatalf("modality-auto must filter to the VK allowlist, got %+v", out)
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

	out, err := strat.Evaluate(context.Background(), node, fx.pool(aiChatRctx()), &trace)
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

	out, err := strat.Evaluate(context.Background(), node, fx.pool(aiChatRctx()), &trace)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(out) == 0 || out[0].ModelID != "m-openai-latest" {
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

	out, err := strat.Evaluate(context.Background(), node, fx.pool(&core.RoutingContext{Request: nil}), &trace)
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

	out, err := strat.Evaluate(context.Background(), node, fx.pool(rctx), &trace)
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

	out, err := strat.Evaluate(context.Background(), node, fx.pool(rctx), &trace)
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

	out, err := strat.Evaluate(context.Background(), node, fx.pool(aiChatRctx()), &trace)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(out) != 1 || out[0].ModelID != "m-openai-mini" {
		t.Fatalf("expected fallback default due to ambiguous providerModelId, got %+v", out)
	}
}

// TestSmartStrategy_RecordsRouterCostOnTrace pins the upward channel for the
// router call's own spend: the trace entry that records the router's pick
// carries the cost and the provider that SERVED the router call, so the proxy
// stage can drain both onto the request's audit record. The cost must appear
// on exactly ONE entry — the stage sums the trace, so a duplicate double-charges.
func TestSmartStrategy_RecordsRouterCostOnTrace(t *testing.T) {
	decider := &fakeDecider{decision: llm.Decision{
		ModelID:          "gpt-4o-mini",
		Reason:           "cheap",
		CostUsd:          0.0055,
		ServedProviderID: "prov-openai",
	}}
	// Two candidates with different context windows so armContextUpgrade also
	// appends its own entry: the assertion below then proves the cost rides on
	// exactly one of them.
	small, large := 128000, 1000000
	candidates := []core.SmartModelRow{
		{ModelID: "m-mini", ModelCode: "gpt-4o-mini", ModelName: "GPT-4o Mini", ProviderID: "p-openai", ProviderName: "openai", ProviderModelID: "gpt-4o-mini", MaxContextTokens: &small},
		{ModelID: "m-big", ModelCode: "gemini-2.5-pro", ModelName: "Gemini Pro", ProviderID: "p-gemini", ProviderName: "gemini", ProviderModelID: "gemini-2.5-pro", MaxContextTokens: &large},
	}
	fx := newSmartFixture(t, decider, candidates)
	node := core.StrategyNode{RouterProviderID: "p-router", RouterModelID: "m-router"}
	trace := []core.TraceEntry{}
	strat := &SmartStrategy{deps: fx.deps()}

	if _, err := strat.Evaluate(context.Background(), node, fx.pool(aiChatRctx()), &trace); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}

	// Two "smart" entries must exist (the pick + the armed context upgrade),
	// otherwise the exactly-one-stamped-entry assertion below is vacuous.
	if len(trace) < 2 {
		t.Fatalf("expected the pick entry AND the context-upgrade entry, got %d: %+v", len(trace), trace)
	}
	var total float64
	var provider string
	costEntries, providerEntries := 0, 0
	for _, e := range trace {
		total += e.RouterCostUsd
		if e.RouterCostUsd != 0 {
			costEntries++
		}
		if e.RouterProviderID != "" {
			provider = e.RouterProviderID
			providerEntries++
		}
	}
	if math.Abs(total-0.0055) > 1e-9 {
		t.Errorf("router cost on trace = %v, want 0.0055", total)
	}
	if provider != "prov-openai" {
		t.Errorf("router provider on trace = %q, want prov-openai", provider)
	}
	if costEntries != 1 {
		t.Errorf("router cost appears on %d trace entries, want exactly 1 (the stage sums; duplicates double-charge)", costEntries)
	}
	if providerEntries != 1 {
		t.Errorf("router provider appears on %d trace entries, want exactly 1", providerEntries)
	}
}

// TestSmartStrategy_FailedRouterCall_LeavesTraceCostZero pins the named
// failure mode "the router call itself failed": the smart strategy fails open
// to the default model, and because a failed Decide returns a zero Decision no
// cost is booked. A failed router call must never be billed.
func TestSmartStrategy_FailedRouterCall_LeavesTraceCostZero(t *testing.T) {
	decider := &fakeDecider{err: errors.New("router LLM timeout (3000ms)")}
	candidates := []core.SmartModelRow{
		{ModelID: "m-default", ModelCode: "gpt-4o-mini", ModelName: "Default", ProviderID: "p-openai", ProviderName: "openai", ProviderModelID: "gpt-4o-mini"},
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

	out, err := strat.Evaluate(context.Background(), node, fx.pool(aiChatRctx()), &trace)
	if err != nil {
		t.Fatalf("a failed router call must fail open, not fail the request: %v", err)
	}
	if len(out) != 1 || out[0].ModelID != "m-default" {
		t.Fatalf("expected fail-open to the default model, got %+v", out)
	}
	for _, e := range trace {
		if e.RouterCostUsd != 0 || e.RouterProviderID != "" {
			t.Errorf("failed router call must stay unbilled and unattributed; entry = %+v", e)
		}
	}
}

// TestSmartStrategy_RouterCostSurvivesPostDecideFallback pins the two
// fallback paths that run AFTER the router call succeeded — the router named a
// model the catalog does not have, and the picked target failed to resolve. The
// call cost money in both cases, so the spend must ride the trace even though
// the request ends up on the default model.
func TestSmartStrategy_RouterCostSurvivesPostDecideFallback(t *testing.T) {
	baseCandidates := func() []core.SmartModelRow {
		return []core.SmartModelRow{
			{ModelID: "m-default", ModelCode: "gpt-4o-mini", ProviderID: "p-openai", ProviderName: "openai", ProviderModelID: "gpt-4o-mini"},
		}
	}
	cases := []struct {
		name       string
		routerPick string
		candidates []core.SmartModelRow
		wantTrace  string
	}{
		{
			// Router returned a code no candidate carries.
			name:       "unknown model from router",
			routerPick: "gpt-9-turbo",
			candidates: baseCandidates(),
			wantTrace:  `router returned unknown model "gpt-9-turbo"`,
		},
		{
			// Router picked a real candidate whose target lookup fails: the
			// fixture's Lookup only resolves models present in candidates, and
			// "m-orphan" is a catalog row the lookup list does not cover.
			name:       "target lookup failure on the picked model",
			routerPick: "orphan-code",
			candidates: baseCandidates(),
			wantTrace:  `target lookup failed for "m-orphan"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			decider := &fakeDecider{decision: llm.Decision{
				ModelID:          tc.routerPick,
				Reason:           "picked",
				CostUsd:          0.0031,
				ServedProviderID: "prov-openai",
			}}
			fx := newSmartFixture(t, decider, tc.candidates)
			if tc.name == "target lookup failure on the picked model" {
				// Add a candidate the fixture's Lookup cannot resolve, so the
				// router's pick reaches Lookup and fails there.
				// Added to the POOL, which is where the strategy reads its
				// candidates from — the same place Stage A fills in production.
				// Appending only to the fixture's store would be adding it to a
				// source the strategy no longer consults.
				fx.rows = append(fx.rows, core.SmartModelRow{
					ModelID: "m-orphan", ModelCode: "orphan-code", ProviderID: "p-ghost", ProviderName: "ghost", ProviderModelID: "orphan",
				})
				fx.store.rows = fx.rows
			}
			node := core.StrategyNode{
				RouterProviderID:  "p-router",
				RouterModelID:     "m-router",
				DefaultProviderID: "p-openai",
				DefaultModelID:    "m-default",
			}
			trace := []core.TraceEntry{}
			strat := &SmartStrategy{deps: fx.deps()}

			out, err := strat.Evaluate(context.Background(), node, fx.pool(aiChatRctx()), &trace)
			if err != nil {
				t.Fatalf("Evaluate: %v", err)
			}
			if len(out) != 1 || out[0].ModelID != "m-default" {
				t.Fatalf("expected fallback to the default model, got %+v", out)
			}
			// Proves this subcase hit the intended post-Decide fallback arm.
			if !traceContains(trace, tc.wantTrace) {
				t.Fatalf("trace missing %q; got %+v", tc.wantTrace, trace)
			}

			var total float64
			var provider string
			costEntries := 0
			for _, e := range trace {
				total += e.RouterCostUsd
				if e.RouterCostUsd != 0 {
					costEntries++
				}
				if e.RouterProviderID != "" {
					provider = e.RouterProviderID
				}
			}
			if math.Abs(total-0.0031) > 1e-9 {
				t.Errorf("router cost on trace = %v, want 0.0031 — the call happened and must stay billed", total)
			}
			if costEntries != 1 {
				t.Errorf("router cost appears on %d entries, want exactly 1", costEntries)
			}
			if provider != "prov-openai" {
				t.Errorf("router provider on trace = %q, want prov-openai", provider)
			}
		})
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

	if _, err := strat.Evaluate(context.Background(), node, fx.pool(aiChatRctx()), &trace); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if got := decider.lastReq.Timeout; got != 3000*time.Millisecond {
		t.Fatalf("router-LLM Timeout = %v, want 3s", got)
	}
}

// TestSmart_TheRouterModelIsVisibleThroughAVKAllowlist.
//
// The router model does the choosing; it is not one of the choices. So when a
// virtual key narrows what may be SERVED, that narrowing must not also hide the
// model doing the narrowing's own bookkeeping — its declared context window,
// which sizes the catalogue prompt.
//
// Read from the wrong side of the filter, the window reads as zero, the prompt
// builder falls back to a conservative built-in, and the catalogue silently
// shrinks for exactly the restricted keys — those requests get a worse choice
// than an unrestricted key makes from the same models, with nothing to show
// for it.
//
// Pinned here because the pool is moving: it is to be fetched once by the
// pipeline and handed to the strategy, and a hoist that filters before handing
// over would break this without failing anything else.
func TestSmart_TheRouterModelIsVisibleThroughAVKAllowlist(t *testing.T) {
	routerWindow := 128_000
	decider := &fakeDecider{decision: llm.Decision{ModelID: "m-claude", Reason: "fits"}}

	candidates := []core.SmartModelRow{
		{ModelID: "m-claude", ModelName: "Claude", ProviderID: "p-anthropic",
			ProviderName: "fake-anthropic", ProviderModelID: "claude-3-opus"},
		// The router itself, which the allowlist below does NOT include.
		{ModelID: "m-router", ModelName: "Router", ProviderID: "p-router",
			ProviderName: "fake-router", ProviderModelID: "router-1",
			MaxContextTokens: &routerWindow},
	}
	fx := newSmartFixture(t, decider, candidates)

	rctx := aiChatRctx()
	rctx.VirtualKey = &core.VKContext{
		AllowedModels: []store.AllowedModelRef{{ProviderID: "p-anthropic", ModelID: "m-claude"}},
	}

	trace := []core.TraceEntry{}
	strat := &SmartStrategy{deps: fx.deps()}
	if _, err := strat.Evaluate(context.Background(),
		core.StrategyNode{RouterProviderID: "p-router", RouterModelID: "m-router"},
		fx.pool(rctx), &trace); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}

	if decider.lastReq.RouterContextLimit != routerWindow {
		t.Errorf("router context limit = %d, want %d — the allowlist hid the router's own "+
			"declared window, so the catalogue prompt was sized against a fallback and the "+
			"restricted key gets a worse choice from the same models",
			decider.lastReq.RouterContextLimit, routerWindow)
	}
}

// TestSmart_ReadsThePreparedPoolInsteadOfFetchingItsOwn.
//
// "Which models could serve this request" is settled when the routing context
// is prepared. This strategy used to ask again — its own fetch of the same
// snapshot, then its own pass applying the virtual key's allowlist — so one
// question had two answers derived independently.
//
// The cost is not the extra read. It is that the two can disagree, and the one
// that disagrees is invisible until a request lands on the difference: a model
// enabled between the two reads, an allowlist applied to one and not the other.
// Every defect this program has chased has that shape.
func TestSmart_ReadsThePreparedPoolInsteadOfFetchingItsOwn(t *testing.T) {
	decider := &fakeDecider{decision: llm.Decision{ModelID: "m-prepared", Reason: "from the pool"}}

	prepared := core.SmartModelRow{ModelID: "m-prepared", ModelName: "Prepared",
		ProviderID: "p1", ProviderName: "p1", ProviderModelID: "prepared"}
	itsOwn := core.SmartModelRow{ModelID: "m-its-own", ModelName: "Fetched",
		ProviderID: "p1", ProviderName: "p1", ProviderModelID: "own"}

	// The fixture knows both so target lookup can resolve either; what
	// distinguishes them is which list the strategy reasons over.
	fx := newSmartFixture(t, decider, []core.SmartModelRow{itsOwn, prepared})

	rctx := aiChatRctx()
	rctx.ModelPool = []core.SmartModelRow{prepared}

	trace := []core.TraceEntry{}
	strat := &SmartStrategy{deps: fx.deps()}
	out, err := strat.Evaluate(context.Background(),
		core.StrategyNode{RouterProviderID: "p-router", RouterModelID: "m-router"},
		rctx, &trace)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}

	if fx.store.calls != 0 {
		t.Errorf("the catalogue was fetched %d time(s) despite the context carrying a pool — "+
			"one question, two independently derived answers", fx.store.calls)
	}
	if len(out) != 1 || out[0].ModelID != "m-prepared" {
		t.Errorf("served from %+v, want the model the prepared pool offered", out)
	}
}

// TestSmart_TheFormShipsTheGatewaysDefaults.
//
// The rule form pre-fills the smart strategy's three numeric fields, and the
// gateway supplies the same numbers when a rule leaves them unset. Two places
// holding one default is fine only while they agree — when they disagree, an
// admin who never touched the field gets a different value depending on
// whether the rule was created through the form or through the API.
//
// It had already happened: the form shipped a 10s router timeout against the
// gateway's 3s, on a call that sits on the request hot path, so every rule
// created in the UI let a slow router stall requests three times longer than
// the gateway's own budget allows before the fallback takes over.
//
// Reading the TypeScript is the only way to compare them from here — there is
// no shared artifact, and a comment in each file asking the other to keep up is
// what produced the drift.
func TestSmart_TheFormShipsTheGatewaysDefaults(t *testing.T) {
	const form = "../../../../control-plane-ui/src/pages/ai-gateway/routing/_shared/routing-rule-config.ts"
	src, err := os.ReadFile(form)
	if err != nil {
		t.Fatalf("read %s: %v — the form is one of the two places this default lives; a test "+
			"that cannot find it silently stops comparing", form, err)
	}

	// The gateway's side, read through the accessors rather than copied, so a
	// change there moves this test's expectation with it.
	empty := &SmartConfig{}
	for _, tc := range []struct {
		field string
		want  int
	}{
		{"timeoutMs", empty.timeoutMs()},
		{"maxTokens", empty.maxTokens()},
	} {
		// `timeoutMs: '3000',` in the empty state and `?? '3000'` when parsing a
		// stored config; both must be the gateway's number.
		quoted := fmt.Sprintf("%s: '%d'", tc.field, tc.want)
		fallback := fmt.Sprintf("%s: String(cfg.%s ?? '%d')", tc.field, tc.field, tc.want)
		built := fmt.Sprintf("%s: Number(state.%s) || %d", tc.field, tc.field, tc.want)
		for _, want := range []string{quoted, fallback, built} {
			if !strings.Contains(string(src), want) {
				t.Errorf("the rule form does not carry %q: it ships its own %s default, so a "+
					"rule created through the UI gets a different value from one created "+
					"through the API with the field omitted", want, tc.field)
			}
		}
	}
}

// TestSmart_TheKeysAllowlistIsAppliedOnceEvenThoughItRunsTwice.
//
// The virtual key's allow list is applied inside this strategy and again by
// VKAccessFilter on the plan the resolver returns. Both are deliberate:
// filtering before the judge prompt is what stops the router LLM from ever
// picking a model the key lacks — filtering after would pay for a judge call
// and discard its answer — and the resolver's pass is what covers every other
// strategy.
//
// Two applications of one rule are safe only while they are the SAME rule. If
// this strategy's predicate ever drifts looser than the filter's, the judge
// picks a model the key cannot use, the resolver silently drops it, and the
// plan the caller gets is shorter than the one the router built — with nothing
// saying so.
//
// Asserted as a fixed point: running the filter over this strategy's own output
// must change nothing. It goes red the moment the two predicates disagree, in
// the direction that matters.
func TestSmart_TheKeysAllowlistIsAppliedOnceEvenThoughItRunsTwice(t *testing.T) {
	rows := []core.SmartModelRow{
		{ModelID: "m-allowed", ModelCode: "allowed", ProviderID: "p1",
			InputPricePM: priceOf(1), OutputPricePM: priceOf(1)},
		{ModelID: "m-also-allowed", ModelCode: "alsoallowed", ProviderID: "p1",
			InputPricePM: priceOf(2), OutputPricePM: priceOf(2)},
		{ModelID: "m-forbidden", ModelCode: "forbidden", ProviderID: "p2",
			InputPricePM: priceOf(0), OutputPricePM: priceOf(0)}, // cheapest, so it would lead the pool
		// A DIFFERENT row whose provider-side id is one the key was granted, on a
		// provider it was not. A predicate that compares ids and forgets the
		// provider admits this one, and the named check below cannot see it —
		// only the fixed point can.
		{ModelID: "m-elsewhere", ModelCode: "elsewhere", ProviderID: "p9",
			ProviderModelID: "m-allowed",
			InputPricePM:    priceOf(0), OutputPricePM: priceOf(0)},
	}
	rctx := aiChatRctx()
	rctx.VirtualKey = &core.VKContext{ID: "vk-1", AllowedModels: []store.AllowedModelRef{
		{ProviderID: "p1", ModelID: "m-allowed"},
		{ProviderID: "p1", ModelID: "m-also-allowed"},
	}}

	decider := &fakeDecider{decision: llm.Decision{ModelID: "allowed", Reason: "test"}}
	fx := newSmartFixture(t, decider, rows)
	node := core.StrategyNode{RouterProviderID: "p-router", RouterModelID: "m-router"}
	trace := []core.TraceEntry{}
	strat := &SmartStrategy{deps: fx.deps()}
	out, err := strat.Evaluate(context.Background(), node, fx.pool(rctx), &trace)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(out) == 0 {
		t.Fatalf("the strategy returned no targets; the scenario needs a plan to filter")
	}
	for _, tgt := range out {
		if tgt.ModelID == "m-forbidden" {
			t.Fatalf("the plan carries %s, which the key does not allow — the router was "+
				"offered it and the caller would have been served it", tgt.ModelID)
		}
	}

	// The fixed point: the resolver's pass over this plan removes nothing.
	kept := matcher.VKAccessFilter{}.Keep(out, rctx)
	if len(kept) != len(out) {
		t.Errorf("the resolver's allow-list pass dropped %d of %d targets this strategy had "+
			"already admitted — the two predicates have diverged, so the router built a plan "+
			"the caller does not get and nothing on the record says which entries went",
			len(out)-len(kept), len(out))
	}
}

// TestSmart_AContextWithNoPoolMeansTheCatalogueCouldNotBeRead.
//
// The strategy has no self-fetch, and this is what that costs and buys.
//
// It was there as "the path for a context that carries no pool". That context
// has no caller: `Evaluate` is reached from one place in production, on the
// pipeline path, and the wiring hands the SAME store to the resolver's pool
// preparation and to this strategy in the same branch. So a nil pool cannot
// mean "nobody prepared one" — it means the catalogue could not be read, and
// asking the same store again would have returned the same error.
//
// The strategy therefore falls back to the configured default rather than
// routing from a catalogue it does not have. Asserted because the whole test
// suite used to leave the pool nil and ride the self-fetch, which means it
// proved nothing about the path production actually takes.
func TestSmart_AContextWithNoPoolMeansTheCatalogueCouldNotBeRead(t *testing.T) {
	// m-default is in the rows so the fixture's Lookup can resolve it; the pool
	// is what the strategy reads, and the default is reached through Lookup.
	rows := []core.SmartModelRow{
		{ModelID: "m-a", ModelCode: "a", ProviderID: "p1"},
		{ModelID: "m-b", ModelCode: "b", ProviderID: "p1"},
		{ModelID: "m-default", ModelCode: "default", ProviderID: "p1"},
	}
	fx := newSmartFixture(t, &fakeDecider{decision: llm.Decision{ModelID: "a"}}, rows)
	node := core.StrategyNode{
		RouterProviderID: "p-router", RouterModelID: "m-router",
		DefaultProviderID: "p1", DefaultModelID: "m-default",
	}

	// With the pool, the router's pick is served.
	trace := []core.TraceEntry{}
	strat := &SmartStrategy{deps: fx.deps()}
	out, err := strat.Evaluate(context.Background(), node, fx.pool(aiChatRctx()), &trace)
	if err != nil {
		t.Fatalf("Evaluate with a pool: %v", err)
	}
	if len(out) == 0 || out[0].ModelID != "m-a" {
		t.Fatalf("with a prepared pool the router's pick must be served, got %+v", out)
	}

	// Without it, the default — NOT the same answer reached by a second query.
	trace = []core.TraceEntry{}
	out, err = strat.Evaluate(context.Background(), node, aiChatRctx(), &trace)
	if err != nil {
		t.Fatalf("Evaluate without a pool: %v", err)
	}
	if len(out) != 1 || out[0].ModelID != "m-default" {
		t.Errorf("a context with no pool produced %+v; the catalogue could not be read, so the "+
			"configured default is the only honest answer — routing from a pool fetched here "+
			"would be asking the same store that already failed", out)
	}
}

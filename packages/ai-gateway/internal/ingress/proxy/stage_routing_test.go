// stage_routing_test.go — characterization pins for the routing stage
// of the proxy pipeline: the capability-filter rejection envelope, the
// empty-target passthrough fallback, and the cross-format schema-
// mismatch recorder. Each test pins an observable contract (status
// code, error envelope, recorder invocation) of ServeProxy.
package proxy

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/requestcontext"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// errRouterStub returns a fixed error from ResolveTargets so tests can
// drive the handler's routing-error arms.
type errRouterStub struct{ err error }

func (s *errRouterStub) ResolveTargets(_ context.Context, _ *routingcore.RoutingContext) (*routingcore.RouteResult, error) {
	return nil, s.err
}

// schemaMismatchRecorderStub captures RecordSchemaMismatch tuples.
type schemaMismatchRecorderStub struct {
	mu    sync.Mutex
	calls [][2]string
}

func (r *schemaMismatchRecorderStub) RecordSchemaMismatch(ingress, provider string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, [2]string{ingress, provider})
}

func (r *schemaMismatchRecorderStub) snapshot() [][2]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([][2]string, len(r.calls))
	copy(out, r.calls)
	return out
}

// routingStageFixture drives routingStage.run() in isolation: a scripted
// RouteResult from the Router stub, an optional passthrough-fallback model, and
// the audit record the stage writes onto. Everything else on proxyState is the
// minimum the stage reads (ingress, request, logger); no upstream is involved
// because the stage never dispatches.
type routingStageFixture struct {
	stage routingStage
	rec   *audit.Record
	// logs captures everything the stage logged, so tests can assert on the
	// mis-attribution warning drainRouterCost emits.
	logs *bytes.Buffer
	// w is the response recorder the error arms write their envelope into.
	w *httptest.ResponseRecorder
}

func (f *routingStageFixture) run() bool { return f.stage.run() }

// newRoutingStageFixtureWith wires the routing stage with the given primary
// RouteResult and an optional Deps mutator for the passthrough-fallback model
// catalog. The stage's logger writes into f.logs.
func newRoutingStageFixtureWith(t *testing.T, result *routingcore.RouteResult, mutate func(*Deps)) *routingStageFixture {
	t.Helper()
	logs := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	deps := &Deps{
		Router: &fixedRouteResultStub{result: result},
		Logger: logger,
	}
	if mutate != nil {
		mutate(deps)
	}
	h := NewHandler(deps)
	ingress := Ingress{WireShape: typology.WireShapeOpenAIChat, BodyFormat: provcore.FormatOpenAI}
	r := freshChatRequest(t, `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	rec := &audit.Record{RequestID: "req-router-cost"}
	w := httptest.NewRecorder()
	s := &proxyState{
		h:            h,
		w:            w,
		r:            r,
		in:           ingress,
		resolved:     ingress,
		endpointType: string(typology.EndpointKindChat),
		logger:       logger,
		rec:          rec,
		modelID:      "gpt-4o",
		rctxFull:     requestcontext.NewBuilder().WithEndpoint(string(typology.EndpointKindChat)).WithHeaders(r.Header).Build(),
	}
	return &routingStageFixture{stage: routingStage{s: s}, rec: rec, logs: logs, w: w}
}

// newRoutingStageFixtureWithFallback wires the routing stage with the given
// primary RouteResult and, when fallbackModel is non-nil, a model catalog that
// lets the zero-target passthrough fallback resolve a replacement result.
func newRoutingStageFixtureWithFallback(t *testing.T, result *routingcore.RouteResult, fallbackModel *store.Model) *routingStageFixture {
	t.Helper()
	return newRoutingStageFixtureWith(t, result, func(d *Deps) {
		if fallbackModel != nil {
			d.Models = fallbackModelLookupStub{model: fallbackModel}
		}
	})
}

// newRoutingStageFixture is the no-fallback form: the scripted result already
// carries targets, so the passthrough fallback is never reached.
func newRoutingStageFixture(t *testing.T, result *routingcore.RouteResult) *routingStageFixture {
	t.Helper()
	return newRoutingStageFixtureWithFallback(t, result, nil)
}

// fixedRouteResultStub returns one scripted RouteResult from ResolveTargets.
type fixedRouteResultStub struct{ result *routingcore.RouteResult }

func (s *fixedRouteResultStub) ResolveTargets(_ context.Context, _ *routingcore.RoutingContext) (*routingcore.RouteResult, error) {
	return s.result, nil
}

// TestStageRouting_DrainsRouterCostOntoRecord pins the drain: the smart
// strategy's router-call cost and the vendor that SERVED that call, recorded on
// the routing trace, land on the request's own audit record.
func TestStageRouting_DrainsRouterCostOntoRecord(t *testing.T) {
	s := newRoutingStageFixture(t, &routingcore.RouteResult{
		Dispatch: []routingcore.RoutingTarget{{ProviderID: "prov-anthropic", ProviderName: "anthropic", AdapterType: "anthropic"}},
		Trace: []routingcore.TraceEntry{{
			StrategyType: "smart", Decision: "picked claude",
			RouterCostUsd: 0.0055, RouterProviderID: "prov-openai",
		}},
	})

	if ok := s.run(); !ok {
		t.Fatal("routing stage failed")
	}
	if math.Abs(s.rec.RouterCostUsd-0.0055) > 1e-9 {
		t.Errorf("rec.RouterCostUsd = %v, want 0.0055", s.rec.RouterCostUsd)
	}
	// The router ran on OpenAI while the request was served by Anthropic.
	// Booking this against the routed provider is the bug being fixed.
	if s.rec.RouterProviderID != "prov-openai" {
		t.Errorf("rec.RouterProviderID = %q, want prov-openai", s.rec.RouterProviderID)
	}
}

// TestStageRouting_KeepsRouterCostWhenPassthroughFallbackReplacesResult guards
// a real leak path: the router call already cost money, and a subsequent
// zero-target result that gets REPLACED by the passthrough fallback must not
// lose it (stage_routing swaps routeResult wholesale).
func TestStageRouting_KeepsRouterCostWhenPassthroughFallbackReplacesResult(t *testing.T) {
	s := newRoutingStageFixtureWithFallback(t,
		&routingcore.RouteResult{
			Dispatch: nil,
			Trace: []routingcore.TraceEntry{{
				StrategyType: "smart", Decision: "no candidates",
				RouterCostUsd: 0.0055, RouterProviderID: "prov-openai",
			}},
		},
		&store.Model{
			ID:                  "model-1",
			Code:                "gpt-4o",
			Enabled:             true,
			ProviderEnabled:     true,
			Status:              "active",
			Name:                "GPT-4o",
			ProviderID:          "p-openai",
			ProviderName:        "openai",
			ProviderModelID:     "gpt-4o",
			ProviderAdapterType: "openai",
		})

	if ok := s.run(); !ok {
		t.Fatal("routing stage failed")
	}
	if math.Abs(s.rec.RouterCostUsd-0.0055) > 1e-9 {
		t.Errorf("rec.RouterCostUsd = %v, want 0.0055 — cost lost across the fallback swap", s.rec.RouterCostUsd)
	}
	if s.rec.RouterProviderID != "prov-openai" {
		t.Errorf("rec.RouterProviderID = %q, want prov-openai — attribution lost across the fallback swap", s.rec.RouterProviderID)
	}
}

// TestStageRouting_KeepsRouterCostWhenPassthroughFallbackFails is the third
// exit from the zero-targets branch, and the one the earlier drain placement
// lost: the passthrough fallback errors and the stage returns after
// writeDetailedErr has already handed s.rec to the audit pipeline. The router
// call was paid for before any of that, so the row must still carry the cost
// and the vendor — a failed request is not a free router call.
func TestStageRouting_KeepsRouterCostWhenPassthroughFallbackFails(t *testing.T) {
	zeroTargetResult := func() *routingcore.RouteResult {
		return &routingcore.RouteResult{
			Dispatch: nil,
			Trace: []routingcore.TraceEntry{{
				StrategyType: "smart", Decision: "no candidates",
				RouterCostUsd: 0.0055, RouterProviderID: "prov-openai",
			}},
		}
	}
	cases := []struct {
		name       string
		mutate     func(*Deps)
		wantStatus int
	}{
		{
			// No model catalog wired: "passthrough fallback is unavailable".
			name:       "model lookup dependency missing",
			mutate:     func(d *Deps) { d.Models = nil },
			wantStatus: http.StatusInternalServerError,
		},
		{
			// Catalog present but the requested model is unknown.
			name: "requested model not in catalog",
			mutate: func(d *Deps) {
				d.Models = fallbackModelLookupStub{err: errors.New("no such model")}
			},
			wantStatus: http.StatusNotFound,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newRoutingStageFixtureWith(t, zeroTargetResult(), tc.mutate)

			if ok := s.run(); ok {
				t.Fatal("routing stage must fail when the passthrough fallback fails")
			}
			if s.w.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", s.w.Code, tc.wantStatus, s.w.Body.String())
			}
			if math.Abs(s.rec.RouterCostUsd-0.0055) > 1e-9 {
				t.Errorf("rec.RouterCostUsd = %v, want 0.0055 — router spend lost on the fallback error path", s.rec.RouterCostUsd)
			}
			if s.rec.RouterProviderID != "prov-openai" {
				t.Errorf("rec.RouterProviderID = %q, want prov-openai — attribution lost on the fallback error path", s.rec.RouterProviderID)
			}
		})
	}
}

// TestStageRouting_TwoRouterVendors_SumsCostKeepsFirstProviderAndWarns pins the
// documented limitation: traffic_event has one router_provider_id column, so a
// request whose two smart nodes were served by different vendors books the
// combined cost against the first. That is a real mis-attribution, so it must
// never be silent — the warning is the only signal, and this test is what keeps
// it wired.
func TestStageRouting_TwoRouterVendors_SumsCostKeepsFirstProviderAndWarns(t *testing.T) {
	s := newRoutingStageFixture(t, &routingcore.RouteResult{
		Dispatch: []routingcore.RoutingTarget{{ProviderID: "p-openai", ProviderName: "openai", AdapterType: "openai"}},
		Trace: []routingcore.TraceEntry{
			{StrategyType: "smart", Decision: "primary rule picked gpt-4o",
				RouterCostUsd: 0.0055, RouterProviderID: "prov-openai"},
			{StrategyType: "smart", Decision: "fallback rule picked claude",
				RouterCostUsd: 0.0031, RouterProviderID: "prov-anthropic"},
		},
	})

	if ok := s.run(); !ok {
		t.Fatal("routing stage failed")
	}
	if math.Abs(s.rec.RouterCostUsd-0.0086) > 1e-9 {
		t.Errorf("rec.RouterCostUsd = %v, want 0.0086 (both router calls)", s.rec.RouterCostUsd)
	}
	if s.rec.RouterProviderID != "prov-openai" {
		t.Errorf("rec.RouterProviderID = %q, want prov-openai (first-write-wins)", s.rec.RouterProviderID)
	}
	logged := s.logs.String()
	if !strings.Contains(logged, "a second vendor also served a router call") {
		t.Fatalf("mis-attribution warning missing; logs=%s", logged)
	}
	// The warning is only useful if it names BOTH vendors and the request.
	for _, want := range []string{"prov-openai", "prov-anthropic", "req-router-cost"} {
		if !strings.Contains(logged, want) {
			t.Errorf("warning must name %q; logs=%s", want, logged)
		}
	}
}

// TestStageRouting_RepeatedSameVendor_DoesNotWarn pins the negative: two router
// calls served by the SAME vendor are attributed correctly, so no warning fires
// — the signal must stay specific to genuine mis-attribution or operators will
// learn to ignore it.
func TestStageRouting_RepeatedSameVendor_DoesNotWarn(t *testing.T) {
	s := newRoutingStageFixture(t, &routingcore.RouteResult{
		Dispatch: []routingcore.RoutingTarget{{ProviderID: "p-openai", ProviderName: "openai", AdapterType: "openai"}},
		Trace: []routingcore.TraceEntry{
			{StrategyType: "smart", Decision: "primary", RouterCostUsd: 0.0055, RouterProviderID: "prov-openai"},
			{StrategyType: "smart", Decision: "fallback", RouterCostUsd: 0.0031, RouterProviderID: "prov-openai"},
		},
	})

	if ok := s.run(); !ok {
		t.Fatal("routing stage failed")
	}
	if math.Abs(s.rec.RouterCostUsd-0.0086) > 1e-9 {
		t.Errorf("rec.RouterCostUsd = %v, want 0.0086", s.rec.RouterCostUsd)
	}
	if strings.Contains(s.logs.String(), "a second vendor also served a router call") {
		t.Errorf("no warning expected when both router calls hit the same vendor; logs=%s", s.logs.String())
	}
}

// TestStageRouting_NonSmartRoute_LeavesRouterCostZero pins the no-op case:
// a route resolved without a router-LLM call leaves both new columns zero, so
// non-smart traffic never carries a phantom router charge.
func TestStageRouting_NonSmartRoute_LeavesRouterCostZero(t *testing.T) {
	s := newRoutingStageFixture(t, &routingcore.RouteResult{
		Dispatch: []routingcore.RoutingTarget{{ProviderID: "p-openai", ProviderName: "openai", AdapterType: "openai"}},
		Trace: []routingcore.TraceEntry{
			{StrategyType: "single", Decision: "picked gpt-4o"},
		},
	})

	if ok := s.run(); !ok {
		t.Fatal("routing stage failed")
	}
	if s.rec.RouterCostUsd != 0 {
		t.Errorf("rec.RouterCostUsd = %v, want 0 for a non-smart route", s.rec.RouterCostUsd)
	}
	if s.rec.RouterProviderID != "" {
		t.Errorf("rec.RouterProviderID = %q, want empty for a non-smart route", s.rec.RouterProviderID)
	}
}

// servableGPT4o is a catalog row the passthrough fallback resolves cleanly, so
// a test that reaches passthrough gets a SERVED request rather than an error —
// which is what makes the pair below able to tell refusal from service.
func servableGPT4o() *store.Model {
	return &store.Model{
		ID: "model-1", Code: "gpt-4o", Name: "GPT-4o",
		Enabled: true, ProviderEnabled: true, Status: "active",
		ProviderID: "p-openai", ProviderName: "openai",
		ProviderModelID: "gpt-4o", ProviderAdapterType: "openai",
	}
}

// TestStageRouting_MatchedRuleThatResolvedNothing_IsRefusedNotServed is the
// gate on the passthrough's precondition.
//
// Passthrough means "no rule applies to this request — serve the model they
// asked for". A rule that matched and then resolved no target is the opposite
// situation: the admin wrote a redirect away from gpt-4o, its targets were
// unavailable, and serving gpt-4o anyway delivers exactly what the rule exists
// to prevent — with a 200 and nothing in the exchange saying the redirect was
// dropped. The catalog here resolves gpt-4o happily, so nothing except the
// precondition stands between this request and the model it must not reach.
func TestStageRouting_MatchedRuleThatResolvedNothing_IsRefusedNotServed(t *testing.T) {
	s := newRoutingStageFixtureWithFallback(t,
		&routingcore.RouteResult{
			Dispatch: nil, RuleMatchedAndResolvedNothing: true,
			RuleID: "r-compliance", RuleName: "compliance redirect",
			Trace: []routingcore.TraceEntry{{
				StrategyType: "single", RuleID: "r-compliance",
				Decision: "it resolved no target",
			}},
		},
		servableGPT4o())

	if ok := s.run(); ok {
		t.Fatalf("a request a rule redirected was served by the model the rule redirects AWAY from; body=%s", s.w.Body.String())
	}
	if s.w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", s.w.Code, s.w.Body.String())
	}
	if !strings.Contains(s.w.Body.String(), "ROUTING_RULES_RESOLVED_NOTHING") {
		t.Fatalf("the envelope must name the reason so the operator can find the yielding rule; body=%s", s.w.Body.String())
	}

	// The hint sends the operator to the routing trace, so the trace has to be
	// on the record this refusal writes. The first version returned before the
	// assignment that puts it there, so the one request whose message said to
	// read the trace was the one request whose trace column was NULL.
	trace, ok := s.rec.RoutingTrace.(*routingAuditTrace)
	if !ok || trace == nil {
		t.Fatalf("no routing trace on the audit record (%T); the error tells the operator to "+
			"read one", s.rec.RoutingTrace)
	}
	if len(trace.Trace) == 0 {
		t.Error("the trace carries no entries, so it records no rule and no reason")
	}
	if s.rec.RoutingRuleID != "r-compliance" {
		t.Errorf("rec.RoutingRuleID = %q; without it the row cannot be joined to the rule that "+
			"refused", s.rec.RoutingRuleID)
	}
}

// TestStageRouting_NoRuleMatched_StillPassesThrough is the other half, and the
// reason the gate above cannot be satisfied by refusing every empty result: a
// default deployment has no rule matching most requests, and passthrough is its
// busiest path. Refusing here would take the gateway down for everyone.
//
// It covers the FILTERED-EMPTY case too: a rule that matched and resolved a
// target the modality filter then dropped arrives here with the flag false,
// because the flag is measured before any filter runs.
func TestStageRouting_NoRuleMatched_StillPassesThrough(t *testing.T) {
	s := newRoutingStageFixtureWithFallback(t,
		&routingcore.RouteResult{Dispatch: nil, RuleMatchedAndResolvedNothing: false},
		servableGPT4o())

	if ok := s.run(); !ok {
		t.Fatalf("no rule matched, so the requested model is the right answer; body=%s", s.w.Body.String())
	}
	if got := s.stage.s.routeResult; got == nil || len(got.AllTargets()) != 1 || got.AllTargets()[0].ModelID != "model-1" {
		t.Fatalf("passthrough did not install the requested model as the target: %+v", got)
	}
}

// TestServeProxy_Routing_CapabilityRejectAll_Returns400WithAvailableCapabilities
// pins the NoCompatibleProviderError arm: when the capability pre-filter
// rejected every candidate (Available non-empty), the handler answers 400
// with the no_compatible_capability envelope listing each candidate's
// supported parameters, and never proceeds to upstream.
func TestServeProxy_Routing_CapabilityRejectAll_Returns400WithAvailableCapabilities(t *testing.T) {
	deps := makeOpenAIDeps(t, "", emptyHookCache(t), func(d *Deps) {
		d.Router = &errRouterStub{err: &routingcore.NoCompatibleProviderError{
			Available: []routingcore.CandidateCapability{{
				Provider:            "openai",
				Model:               "text-embedding-3-small",
				SupportedDimensions: []int{1536},
			}},
		}}
	})

	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatOpenAI,
	})
	w := httptest.NewRecorder()
	h(w, freshChatRequest(t, `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400; body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "NO_COMPATIBLE_CAPABILITY") {
		t.Errorf("body=%s want NO_COMPATIBLE_CAPABILITY code", body)
	}
	if !strings.Contains(body, "available_capabilities") {
		t.Errorf("body=%s want available_capabilities array", body)
	}
	if !strings.Contains(body, "text-embedding-3-small") {
		t.Errorf("body=%s want the rejected candidate's model listed", body)
	}
}

// TestServeProxy_Routing_EmptyTargets_PassthroughFallbackServesRequest pins
// the no-targets fallback: when routing resolves zero targets but the model
// catalog knows the requested model, the request is served through the
// passthrough-fallback target instead of failing with ROUTING_NO_MATCH.
func TestServeProxy_Routing_EmptyTargets_PassthroughFallbackServesRequest(t *testing.T) {
	upstream := openAIChatUpstream(t, `{
		"id":"x","object":"chat.completion","model":"gpt-4o",
		"choices":[{"index":0,"message":{"role":"assistant","content":"fallback-served"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
	}`)
	defer upstream.Close()

	deps := makeOpenAIDeps(t, upstream.URL, emptyHookCache(t), func(d *Deps) {
		d.Router = &stubRouterCacheTest{targets: nil}
		d.Models = fallbackModelLookupStub{model: &store.Model{
			ID:                  "model-1",
			Code:                "gpt-4o",
			Enabled:             true,
			ProviderEnabled:     true,
			Status:              "active",
			Name:                "GPT-4o",
			ProviderID:          "p-openai",
			ProviderName:        "openai",
			ProviderModelID:     "gpt-4o",
			ProviderAdapterType: "openai",
		}}
	})

	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatOpenAI,
	})
	w := httptest.NewRecorder()
	h(w, freshChatRequest(t, `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200 (served via passthrough fallback); body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "fallback-served") {
		t.Errorf("body=%s want upstream response via fallback target", w.Body.String())
	}
}

// TestServeProxy_Routing_SchemaMismatch_RecorderReceivesRejectedTuple pins
// the cross-format filter's mismatch accounting: every incompatible target
// is reported to the SchemaMismatchRecorder as an (ingressFormat,
// providerFormat) tuple, and with zero compatible targets remaining the
// handler answers 400 without invoking the executor.
func TestServeProxy_Routing_SchemaMismatch_RecorderReceivesRejectedTuple(t *testing.T) {
	fexec := &fakeExecutor{} // must NOT be invoked
	fbridge := &fakeBridge{
		endpointRoutable: func(_ typology.WireShape, _, _ provcore.Format) bool { return false },
	}
	deps := makeFakeDeps(t, fexec, fbridge)
	rec := &schemaMismatchRecorderStub{}
	deps.SchemaMismatchRecorder = rec

	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatOpenAI,
	})
	w := httptest.NewRecorder()
	h(w, freshChatRequest(t, `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400; body=%s", w.Code, w.Body.String())
	}
	calls := rec.snapshot()
	if len(calls) != 1 {
		t.Fatalf("RecordSchemaMismatch calls=%d want 1 (one rejected target)", len(calls))
	}
	if calls[0][0] != "openai" || calls[0][1] != "openai" {
		t.Errorf("RecordSchemaMismatch tuple=%v want [openai openai]", calls[0])
	}
	if fexec.Calls != 0 || fexec.PreparedCalls != 0 {
		t.Errorf("executor must NOT be invoked; calls=%d prepared=%d", fexec.Calls, fexec.PreparedCalls)
	}
}

// TestStageRouting_ItemisesRouterCallIntoInternalOpsBreakdown pins the per-call
// record behind the amount. router_cost_usd alone is an amount with no basis:
// while cached router prompt tokens were billed at the full input rate, no
// stored row could show it, and none can be recomputed now. The breakdown entry
// is what makes the next such drift checkable from our own data.
func TestStageRouting_ItemisesRouterCallIntoInternalOpsBreakdown(t *testing.T) {
	s := newRoutingStageFixture(t, &routingcore.RouteResult{
		Dispatch: []routingcore.RoutingTarget{{ProviderID: "prov-anthropic", ProviderName: "anthropic", AdapterType: "anthropic"}},
		Trace: []routingcore.TraceEntry{{
			StrategyType: "smart", Decision: "picked claude",
			RouterCostUsd: 0.0031, RouterProviderID: "prov-openai",
			RouterModelID:          "model-gpt-4o",
			RouterPromptTokens:     5120,
			RouterCompletionTokens: 18,
			RouterCacheReadTokens:  4864,
		}},
	})

	if ok := s.run(); !ok {
		t.Fatal("routing stage failed")
	}
	if len(s.rec.InternalOpsBreakdown) != 1 {
		t.Fatalf("InternalOpsBreakdown has %d entries, want 1", len(s.rec.InternalOpsBreakdown))
	}
	got := s.rec.InternalOpsBreakdown[0]
	want := audit.InternalOpsEntry{
		Type: "smart-router", Model: "model-gpt-4o", ProviderID: "prov-openai",
		PromptTokens: 5120, CompletionTokens: 18, CacheReadTokens: 4864,
		CostUsd: 0.0031,
	}
	if got != want {
		t.Errorf("entry = %+v, want %+v", got, want)
	}
}

// TestStageRouting_ItemisesUnpricedRouterCall pins that the itemisation is
// gated on usage, not on cost. An unpriced router model produces real tokens
// and a zero amount — exactly the call whose spend the cost column cannot show,
// so it is the one that most needs a record.
func TestStageRouting_ItemisesUnpricedRouterCall(t *testing.T) {
	s := newRoutingStageFixture(t, &routingcore.RouteResult{
		Dispatch: []routingcore.RoutingTarget{{ProviderID: "prov-openai", ProviderName: "openai", AdapterType: "openai"}},
		Trace: []routingcore.TraceEntry{{
			StrategyType: "smart", Decision: "picked gpt-4o-mini",
			RouterCostUsd: 0, RouterProviderID: "prov-moonshot",
			RouterModelID: "model-kimi", RouterPromptTokens: 900, RouterCompletionTokens: 12,
		}},
	})

	if ok := s.run(); !ok {
		t.Fatal("routing stage failed")
	}
	if len(s.rec.InternalOpsBreakdown) != 1 {
		t.Fatalf("InternalOpsBreakdown has %d entries, want 1 — an unpriced call still happened",
			len(s.rec.InternalOpsBreakdown))
	}
	if got := s.rec.InternalOpsBreakdown[0]; got.PromptTokens != 900 || got.CostUsd != 0 {
		t.Errorf("entry = %+v, want 900 prompt tokens at zero cost", got)
	}
}

// TestStageRouting_NoInternalOpsEntryWithoutUsage pins the other side: a trace
// entry from a strategy that never called a router LLM (or a failed call, which
// stamps a zero Decision) must add no entry. A breakdown padded with empty rows
// would make "this request made an internal call" unreadable.
func TestStageRouting_NoInternalOpsEntryWithoutUsage(t *testing.T) {
	s := newRoutingStageFixture(t, &routingcore.RouteResult{
		Dispatch: []routingcore.RoutingTarget{{ProviderID: "prov-openai", ProviderName: "openai", AdapterType: "openai"}},
		Trace: []routingcore.TraceEntry{
			{StrategyType: "single", Decision: "picked gpt-4o"},
			{StrategyType: "smart", Decision: "router LLM timeout (3000ms)"},
		},
	})

	if ok := s.run(); !ok {
		t.Fatal("routing stage failed")
	}
	if len(s.rec.InternalOpsBreakdown) != 0 {
		t.Errorf("InternalOpsBreakdown = %+v, want empty when no router call produced usage",
			s.rec.InternalOpsBreakdown)
	}
}

// TestStageRouting_ItemisesEachRouterCallSeparately pins the multi-vendor case
// the flat columns cannot represent: two smart rules served by different
// vendors sum into one router_cost_usd attributed to the first vendor alone
// (drainRouterCost's KNOWN LIMITATION). The breakdown keeps the true split, so
// the mis-attribution stays recoverable rather than only being warned about.
func TestStageRouting_ItemisesEachRouterCallSeparately(t *testing.T) {
	s := newRoutingStageFixture(t, &routingcore.RouteResult{
		Dispatch: []routingcore.RoutingTarget{{ProviderID: "prov-openai", ProviderName: "openai", AdapterType: "openai"}},
		Trace: []routingcore.TraceEntry{
			{
				StrategyType: "smart", Decision: "primary rule",
				RouterCostUsd: 0.0031, RouterProviderID: "prov-openai",
				RouterModelID: "model-gpt-4o", RouterPromptTokens: 5120, RouterCompletionTokens: 18,
			},
			{
				StrategyType: "smart", Decision: "fallback rule",
				RouterCostUsd: 0.0009, RouterProviderID: "prov-anthropic",
				RouterModelID: "model-haiku", RouterPromptTokens: 800, RouterCompletionTokens: 10,
			},
		},
	})

	if ok := s.run(); !ok {
		t.Fatal("routing stage failed")
	}
	if len(s.rec.InternalOpsBreakdown) != 2 {
		t.Fatalf("InternalOpsBreakdown has %d entries, want 2 (one per router call)",
			len(s.rec.InternalOpsBreakdown))
	}
	if a, b := s.rec.InternalOpsBreakdown[0], s.rec.InternalOpsBreakdown[1]; a.ProviderID != "prov-openai" || b.ProviderID != "prov-anthropic" {
		t.Errorf("providers = (%q, %q), want (prov-openai, prov-anthropic); the per-vendor split must survive",
			a.ProviderID, b.ProviderID)
	}
	// The flat column still books everything against the first vendor — the
	// limitation the entries work around, pinned here so it stays visible.
	if s.rec.RouterProviderID != "prov-openai" {
		t.Errorf("rec.RouterProviderID = %q, want prov-openai (first-write-wins)", s.rec.RouterProviderID)
	}
	// Summing the entries must reproduce the flat total.
	sum := s.rec.InternalOpsBreakdown[0].CostUsd + s.rec.InternalOpsBreakdown[1].CostUsd
	if math.Abs(sum-s.rec.RouterCostUsd) > 1e-12 {
		t.Errorf("entries sum to %v but router_cost_usd is %v", sum, s.rec.RouterCostUsd)
	}
}

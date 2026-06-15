// stage_routing_test.go — characterization pins for the routing stage
// of the proxy pipeline: the capability-filter rejection envelope, the
// empty-target passthrough fallback, and the cross-format schema-
// mismatch recorder. Each test pins an observable contract (status
// code, error envelope, recorder invocation) of ServeProxy.
package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
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
	if !strings.Contains(body, "no_compatible_capability") {
		t.Errorf("body=%s want no_compatible_capability code", body)
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

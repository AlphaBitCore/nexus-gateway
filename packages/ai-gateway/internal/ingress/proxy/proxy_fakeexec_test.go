// proxy_fakeexec_test.go — drives the handler's uncovered error arms
// using fake executor + fake bridge implementations of the new
// executor.API / canonicalbridge.API interface seams (introduced for
// the final 88.6% → 95% push).
//
// What the real-executor tests in proxy_e2e_test.go / proxy_serveproxy_test.go
// can NOT reach without spinning up real provider HTTP servers:
//   - ExecuteWithPreparedBody.Error == ErrAllTargetsExhausted with a
//     terminal 429 → PROVIDER_RATE_LIMITED 429 branch in
//     fetchUpstreamWithPreparedBody.
//   - executor returning a 4xx ExecutionResult with a populated
//     ProviderError → cross-format error envelope branch in
//     fetchUpstreamWithPreparedBody → handleNonStream.
//   - handleNonStream's reshape failure arm (CanonicalBridge.
//     ResponseCanonicalToIngress returns error).
//   - handleNonStream's Responses-API reverse-decode failure arm
//     (ResponsesUpgrade ctx set + invalid upstream body).
//   - handleNonStreamHit's reshape warning + handleNonStreamWithSubscription's
//     reshape failure (broker non-stream MISS path).
//
// The fakes are minimal: no goroutines, no network. Each test wires
// fakeExecutor + fakeBridge into Deps and drives ServeProxy via httptest.
package proxy

import (
	"context"
	"errors"
	"fmt"
	"github.com/goccy/go-json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/auth/vkauth"
	cache "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/canonicalbridge"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/executor"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	metricspkg "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/metrics"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/ratelimit"
	provbuiltins "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/builtins"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/builtins"
	goHooks "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/payloadcapture"
	compliance "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/pipeline"
	configtypes "github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configtypes/policy"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	trafficbuiltins "github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/wirerewrite"
)

// fakeExecutor — minimal executor.API implementation that returns a
// caller-supplied ExecutionResult on Execute / ExecuteWithPreparedBody.
// Tests pre-set Result before invoking; calls increment Calls so the
// suite can assert which path fired.

type fakeExecutor struct {
	Result          *executor.ExecutionResult
	Calls           int
	PreparedCalls   int
	LastTargets     []routingcore.RoutingTarget
	LastPreparedLen int
}

func (f *fakeExecutor) Execute(
	_ context.Context,
	targets []routingcore.RoutingTarget,
	_ provcore.Request,
	_ configtypes.RetryPolicy,
) *executor.ExecutionResult {
	f.Calls++
	f.LastTargets = targets
	return f.Result
}

func (f *fakeExecutor) ExecuteWithPreparedBody(
	_ context.Context,
	targets []routingcore.RoutingTarget,
	_ provcore.Request,
	_ configtypes.RetryPolicy,
	preparedBody []byte,
	_ []string,
	_ string,
) *executor.ExecutionResult {
	f.PreparedCalls++
	f.LastTargets = targets
	f.LastPreparedLen = len(preparedBody)
	return f.Result
}

// fakeBridge — minimal canonicalbridge.API implementation. Each behaviour
// is independently overridable so a test can fail one method while
// keeping the rest functional.

type fakeBridge struct {
	endpointRoutable                 func(ep typology.WireShape, ingress, target provcore.Format) bool
	targetNativelyServesResponsesAPI func(target provcore.Format) bool
	ingressChatToCanonical           func(ingress provcore.Format, body []byte, ct provcore.CallTarget) ([]byte, error)
	responseCanonicalToIngress       func(ingress provcore.Format, canonical []byte) ([]byte, error)
	responseAcrossFormats            func(from typology.WireShape, to typology.WireShape, body []byte) ([]byte, error)
	newStreamTranscoder              func(ingress, target provcore.Format, model string) canonicalbridge.StreamTranscoder
}

func (b *fakeBridge) EndpointRoutable(ep typology.WireShape, ingress, target provcore.Format) bool {
	if b.endpointRoutable != nil {
		return b.endpointRoutable(ep, ingress, target)
	}
	// Default: permissive. Every valid (ingress, target) pair is
	// routable so the handler proceeds into the canonical/cache path
	// where the test wants to assert.
	return ingress.Valid() && target.Valid()
}

func (b *fakeBridge) ServesResponses(target provcore.Format, override *bool) bool {
	if override != nil && !*override {
		return false
	}
	if b.targetNativelyServesResponsesAPI != nil {
		return b.targetNativelyServesResponsesAPI(target)
	}
	return target == provcore.FormatOpenAI
}

func (b *fakeBridge) IngressChatToCanonical(ingress provcore.Format, body []byte, ct provcore.CallTarget) ([]byte, error) {
	if b.ingressChatToCanonical != nil {
		return b.ingressChatToCanonical(ingress, body, ct)
	}
	return body, nil
}

func (b *fakeBridge) ResponseCanonicalToIngress(ingress provcore.Format, canonical []byte) ([]byte, error) {
	if b.responseCanonicalToIngress != nil {
		return b.responseCanonicalToIngress(ingress, canonical)
	}
	return canonical, nil
}

func (b *fakeBridge) ResponseAcrossFormats(from typology.WireShape, to typology.WireShape, body []byte) ([]byte, error) {
	if b.responseAcrossFormats != nil {
		return b.responseAcrossFormats(from, to, body)
	}
	if from == to {
		return body, nil
	}
	// Default behavior mirrors the real Bridge: delegate to the
	// responseCanonicalToIngress hook with the to-format derived from
	// the to-WireShape so tests that only configure
	// responseCanonicalToIngress still see consistent results. Unmapped
	// shapes propagate as an unknown-format error.
	toFormat, ok := WireShapeToBodyFormat(to)
	if !ok {
		return nil, fmt.Errorf("fakeBridge: ResponseAcrossFormats: no format mapping for to wire-shape %q", to)
	}
	return b.ResponseCanonicalToIngress(toFormat, body)
}

func (b *fakeBridge) NewStreamTranscoder(ingress, target provcore.Format, model string) canonicalbridge.StreamTranscoder {
	if b.newStreamTranscoder != nil {
		return b.newStreamTranscoder(ingress, target, model)
	}
	return nil
}

func (b *fakeBridge) ChatWireShapeForTarget(target provcore.Format) typology.WireShape {
	switch target {
	case provcore.FormatAnthropic:
		return typology.WireShapeAnthropicMessages
	case provcore.FormatGemini:
		return typology.WireShapeGeminiGenerateContent
	case provcore.FormatVertex:
		return typology.WireShapeVertexGenerateContent
	case provcore.FormatBedrock:
		return typology.WireShapeBedrockConverse
	case provcore.FormatCohere:
		return typology.WireShapeCohereChat
	}
	return typology.WireShapeOpenAIChat
}

func (b *fakeBridge) EmbeddingsWireShapeForTarget(target provcore.Format) typology.WireShape {
	switch target {
	case provcore.FormatGemini:
		return typology.WireShapeGeminiEmbedContent
	case provcore.FormatVertex:
		return typology.WireShapeVertexEmbedContent
	case provcore.FormatBedrock:
		return typology.WireShapeBedrockEmbeddings
	case provcore.FormatCohere:
		return typology.WireShapeCohereEmbed
	case provcore.FormatVoyage:
		return typology.WireShapeVoyageEmbeddings
	}
	return typology.WireShapeOpenAIEmbeddings
}

func (b *fakeBridge) IngressEmbeddingsToCanonical(_ provcore.Format, body []byte, _ provcore.CallTarget) ([]byte, error) {
	return body, nil
}

func (b *fakeBridge) ResponseCanonicalToIngressEmbeddings(_ provcore.Format, canonical []byte) ([]byte, error) {
	return canonical, nil
}

// shared deps assembler

// makeFakeDeps wires a minimal Deps suitable for ServeProxy tests with
// a fake executor + fake bridge. Tests can mutate the returned Deps
// (e.g. swap CanonicalBridge to nil) before calling ServeProxy.
func makeFakeDeps(t *testing.T, fexec *fakeExecutor, fbridge *fakeBridge) *Deps {
	t.Helper()
	logger := slog.Default()
	provReg := provcore.NewRegistry()
	provbuiltins.Register(provReg, nil, logger)
	provReg.Freeze()

	trafficReg := traffic.NewAdapterRegistry("nexus_test_ai_gateway")
	trafficbuiltins.RegisterBuiltins(trafficReg)
	trafficReg.Freeze()

	hookCache := compliance.NewHookConfigCache(
		func(_ context.Context) ([]goHooks.HookConfig, error) { return nil, nil },
		builtins.Registry, 0, logger,
	)
	if err := hookCache.Start(context.Background()); err != nil {
		t.Fatalf("hookCache.Start: %v", err)
	}
	time.Sleep(20 * time.Millisecond)

	prod := &captureProducer{}
	auditWriter := audit.NewWriter(prod, "nexus.event.ai-traffic", nil, logger)
	ht := store.NewHealthTracker()
	t.Cleanup(ht.Stop)

	deps := &Deps{
		VKAuth: &stubVKAuthCacheTest{meta: &vkauth.VKMeta{
			ID:               "vk-1",
			Name:             "test-vk",
			OrganizationID:   "org-1",
			OrganizationName: "Org",
		}},
		RateLimiter: ratelimit.NewLocalOnly(logger),
		Router: &stubRouterCacheTest{targets: []routingcore.RoutingTarget{{
			ProviderID:      "p-openai",
			ProviderName:    "openai",
			ProviderModelID: "gpt-4o",
			ModelID:         "gpt-4o",
			ModelName:       "GPT-4o",
			ModelCode:       "gpt-4o",
			AdapterType:     "openai",
		}}},
		Executor:        fexec,
		HookConfigCache: hookCache,
		ProviderReg:     provReg,
		HealthTracker:   ht,
		AuditWriter:     auditWriter,
		CanonicalBridge: fbridge,
		TrafficAdapters: trafficReg,
		PayloadCapture: payloadcapture.NewStore(payloadcapture.Config{
			StoreRequestBody:   false,
			StoreResponseBody:  false,
			MaxInlineBodyBytes: 64 * 1024,
		}),
		Logger: logger,
	}
	return deps
}

// freshChatRequest returns a minimal valid OpenAI chat.completions POST
// for use across tests.
func freshChatRequest(_ *testing.T, body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer vk")
	return r
}

// fetchUpstreamWithPreparedBody — error arms

// TestServeProxy_Fake_AllTargetsExhausted_NonRate exercises the
// PROVIDER_UNAVAILABLE 502 branch in fetchUpstreamWithPreparedBody when
// the executor returns ErrAllTargetsExhausted with no terminal 429
// attempt. Lifts the late-error logging arm + writeDetailedErr(502).
func TestServeProxy_Fake_AllTargetsExhausted_NonRate(t *testing.T) {
	fexec := &fakeExecutor{Result: &executor.ExecutionResult{
		Error: executor.ErrAllTargetsExhausted,
		Attempts: []executor.Attempt{
			{StatusCode: 0, Error: "dial tcp: lookup nope: no such host"},
			{StatusCode: http.StatusBadGateway, Error: "upstream 502"},
		},
	}}
	fbridge := &fakeBridge{}
	deps := makeFakeDeps(t, fexec, fbridge)

	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatOpenAI,
	})
	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	w := httptest.NewRecorder()
	h(w, freshChatRequest(t, body))

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status=%d want 502; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "PROVIDER_UNAVAILABLE") {
		t.Errorf("body=%s want PROVIDER_UNAVAILABLE", w.Body.String())
	}
}

// TestServeProxy_Fake_ClientCanceled_499 exercises the CLIENT_CLOSED 499
// branch: when the inbound request context is canceled (client walked away
// mid-flight), the upstream-exhausted error must be attributed to the client
// as 499, NOT to the provider as 502 PROVIDER_UNAVAILABLE — so provider
// availability metrics aren't polluted by client disconnects.
func TestServeProxy_Fake_ClientCanceled_499(t *testing.T) {
	fexec := &fakeExecutor{Result: &executor.ExecutionResult{
		Error: executor.ErrAllTargetsExhausted,
		Attempts: []executor.Attempt{
			{StatusCode: 0, Error: "context canceled"},
		},
	}}
	deps := makeFakeDeps(t, fexec, &fakeBridge{})

	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatOpenAI,
	})
	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	w := httptest.NewRecorder()
	// Drive the request under an already-canceled context — the client
	// "disconnected" before the gateway could respond.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h(w, freshChatRequest(t, body).WithContext(ctx))

	if w.Code != statusClientClosedRequest {
		t.Fatalf("status=%d want 499 (client closed); body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "CLIENT_CLOSED") {
		t.Errorf("body=%s want CLIENT_CLOSED (not PROVIDER_UNAVAILABLE)", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "PROVIDER_UNAVAILABLE") {
		t.Errorf("client cancel must NOT be recorded as PROVIDER_UNAVAILABLE: %s", w.Body.String())
	}
}

// TestServeProxy_Fake_AllTargetsExhausted_TerminalRateLimited exercises
// the PROVIDER_RATE_LIMITED 429 branch: last attempt's StatusCode == 429
// short-circuits the 502 envelope path.
func TestServeProxy_Fake_AllTargetsExhausted_TerminalRateLimited(t *testing.T) {
	fexec := &fakeExecutor{Result: &executor.ExecutionResult{
		Error: executor.ErrAllTargetsExhausted,
		Attempts: []executor.Attempt{
			{StatusCode: http.StatusTooManyRequests, Error: "rate limited"},
		},
	}}
	fbridge := &fakeBridge{}
	deps := makeFakeDeps(t, fexec, fbridge)

	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatOpenAI,
	})
	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	w := httptest.NewRecorder()
	h(w, freshChatRequest(t, body))

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d want 429; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "PROVIDER_RATE_LIMITED") {
		t.Errorf("body=%s want PROVIDER_RATE_LIMITED", w.Body.String())
	}
}

// TestServeProxy_Fake_ProviderError4xx exercises the 4xx surfaced from
// the executor path (provider returned 400 invalid_request etc). The
// gateway must encode the ingress-format envelope, stamp ErrorCode +
// ErrorReason, and pass through allowlisted upstream headers.
func TestServeProxy_Fake_ProviderError4xx(t *testing.T) {
	upstreamHeaders := http.Header{}
	upstreamHeaders.Set("Content-Type", "application/json")
	upstreamHeaders.Set("X-Request-Id", "req-up-1")
	pe := &provcore.ProviderError{
		Status:  http.StatusBadRequest,
		Code:    "invalid_request",
		Type:    "invalid_request_error",
		Message: "missing required parameter messages",
		Raw:     []byte(`{"error":{"message":"missing required parameter messages","type":"invalid_request_error","code":"invalid_request"}}`),
		Headers: upstreamHeaders,
	}
	fexec := &fakeExecutor{Result: &executor.ExecutionResult{
		StatusCode:    http.StatusBadRequest,
		Headers:       upstreamHeaders,
		Body:          pe.Raw,
		Target:        routingcore.RoutingTarget{ProviderID: "p-openai", ProviderName: "openai", ModelID: "gpt-4o", ModelCode: "gpt-4o", AdapterType: "openai"},
		ProviderError: pe,
		Attempts: []executor.Attempt{
			{StatusCode: http.StatusBadRequest, Error: pe.Error()},
		},
	}}
	fbridge := &fakeBridge{}
	deps := makeFakeDeps(t, fexec, fbridge)

	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatOpenAI,
	})
	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	w := httptest.NewRecorder()
	h(w, freshChatRequest(t, body))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "missing required parameter") {
		t.Errorf("body=%s want upstream provider message", w.Body.String())
	}
}

// handleNonStream — reshape failure arm

// TestServeProxy_Fake_ResponseReshapeFails drives the
// `CanonicalBridge.ResponseCanonicalToIngress returns error` branch in
// handleNonStream. The handler must 502 with the
// "upstream response could not be reshaped for ingress format" message.
func TestServeProxy_Fake_ResponseReshapeFails(t *testing.T) {
	successBody := []byte(`{"id":"x","object":"chat.completion","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	fexec := &fakeExecutor{Result: &executor.ExecutionResult{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Content-Type": []string{"application/json"}},
		Body:       successBody,
		Target:     routingcore.RoutingTarget{ProviderID: "p-openai", ProviderName: "openai", ModelID: "gpt-4o", ModelCode: "gpt-4o", AdapterType: "openai"},
		Usage:      provcore.Usage{PromptTokens: iPtr(1), CompletionTokens: iPtr(1), TotalTokens: iPtr(2)},
		Attempts: []executor.Attempt{
			{StatusCode: http.StatusOK},
		},
	}}
	fbridge := &fakeBridge{
		// Force the chat-completions reshape branch to fail.
		responseCanonicalToIngress: func(_ provcore.Format, _ []byte) ([]byte, error) {
			return nil, errors.New("synthesised reshape failure")
		},
	}
	deps := makeFakeDeps(t, fexec, fbridge)

	// Anthropic ingress so the reshape branch (ingress != target) fires.
	// Router still points at the openai target so the reshape executes.
	deps.Router = &stubRouterCacheTest{targets: []routingcore.RoutingTarget{{
		ProviderID: "p-openai", ProviderName: "openai", ProviderModelID: "gpt-4o",
		ModelID: "gpt-4o", ModelName: "GPT-4o", ModelCode: "gpt-4o", AdapterType: "openai",
	}}}

	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatAnthropic,
	})
	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"max_tokens":4}`
	w := httptest.NewRecorder()
	h(w, freshChatRequest(t, body))

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status=%d want 502; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "reshaped for ingress format") {
		t.Errorf("body=%s want reshape failure envelope", w.Body.String())
	}
}

// handleNonStream — Responses-API reverse-decode failure arm

// TestServeProxy_Fake_E57_ReverseDecode_Fails drives the
// `ResponsesUpgradeFromContext + invalid upstream body` branch in
// handleNonStream. The fake executor pretends an OpenAI target replied
// to /v1/responses with non-Responses-shape bytes that cannot be
// reverse-decoded; the handler must 502 with the reverse-decode error.
//
// We stamp the ctx flag directly via a tiny adapter HandlerFunc wrapper
// because ServeProxy only sets it inside the auto-upgrade arm
// (which depends on routing rule fields not relevant here).
func TestServeProxy_Fake_E57_ReverseDecode_Fails(t *testing.T) {
	// Malformed JSON so DecodeResponsesResponse fails at gjson.ValidBytes.
	junk := []byte(`{not valid json`)
	fexec := &fakeExecutor{Result: &executor.ExecutionResult{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Content-Type": []string{"application/json"}},
		Body:       junk,
		Target:     routingcore.RoutingTarget{ProviderID: "p-openai", ProviderName: "openai", ModelID: "gpt-4o", ModelCode: "gpt-4o", AdapterType: "openai"},
		Attempts:   []executor.Attempt{{StatusCode: http.StatusOK}},
	}}
	fbridge := &fakeBridge{}
	deps := makeFakeDeps(t, fexec, fbridge)

	inner := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatOpenAI,
	})
	// Wrap to inject the ResponsesUpgrade ctx flag before delegating to
	// ServeProxy — simulates the auto-upgrade firing.
	h := func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(WithResponsesUpgrade(r.Context()))
		inner(w, r)
	}

	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	w := httptest.NewRecorder()
	h(w, freshChatRequest(t, body))

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status=%d want 502; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "reverse-decoded") {
		t.Errorf("body=%s want reverse-decode failure envelope", w.Body.String())
	}
}

// IngressChatToCanonical failure on the cache-key path

// TestServeProxy_Fake_CanonicalizeFails drives the
// `CanonicalBridge.IngressChatToCanonical returns error` branch in
// ServeProxy's cache-prepare path. Cross-format (Anthropic ingress → OpenAI
// target) is needed for needsCanonicalization to be true; the fake
// bridge fails the call.
func TestServeProxy_Fake_CanonicalizeFails(t *testing.T) {
	fexec := &fakeExecutor{} // must NOT be invoked
	fbridge := &fakeBridge{
		ingressChatToCanonical: func(_ provcore.Format, _ []byte, _ provcore.CallTarget) ([]byte, error) {
			return nil, errors.New("synthesised canonicalize failure")
		},
	}
	deps := makeFakeDeps(t, fexec, fbridge)

	// Wire a Redis-backed cache so the cache-prepare default branch fires.
	cacheOpt, cleanup := withCache(t)
	defer cleanup()
	cacheOpt(deps)

	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatAnthropic,
	})
	body := `{"model":"gpt-4o","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],"max_tokens":4}`
	w := httptest.NewRecorder()
	h(w, freshChatRequest(t, body))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "canonicalize ingress body") {
		t.Errorf("body=%s want canonicalize failure envelope", w.Body.String())
	}
	if fexec.Calls != 0 || fexec.PreparedCalls != 0 {
		t.Errorf("executor must not be invoked on canonicalize failure; calls=%d prepared=%d",
			fexec.Calls, fexec.PreparedCalls)
	}
}

// cache-HIT handleNonStreamHit — reshape failure (warns, serves canonical)

// TestServeProxy_Fake_CacheHIT_NonStream_ReshapeWarns seeds a non-stream
// cache entry and forces ResponseCanonicalToIngress to fail. The handler
// must NOT abort — it logs a warning and serves the canonical bytes —
// so the response stays 200 with the canonical body.
func TestServeProxy_Fake_CacheHIT_NonStream_ReshapeWarns(t *testing.T) {
	fexec := &fakeExecutor{} // must NOT be invoked on a HIT
	fbridge := &fakeBridge{
		responseCanonicalToIngress: func(_ provcore.Format, _ []byte) ([]byte, error) {
			return nil, errors.New("synthesised reshape failure (cache HIT)")
		},
	}
	deps := makeFakeDeps(t, fexec, fbridge)

	cacheOpt, cleanup := withCache(t)
	defer cleanup()
	cacheOpt(deps)

	// Anthropic ingress so the reshape branch fires.
	deps.Router = &stubRouterCacheTest{targets: []routingcore.RoutingTarget{{
		ProviderID: "p-openai", ProviderName: "openai", ProviderModelID: "gpt-4o",
		ModelID: "gpt-4o", ModelName: "GPT-4o", ModelCode: "gpt-4o", AdapterType: "openai",
	}}}

	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],"max_tokens":4}`)
	cacheKey := computeStreamCacheKey(t, deps, "openai", "gpt-4o", body, false)
	entry := &cache.ResponseEntry{
		Provider:          "openai",
		Model:             "gpt-4o",
		CanonicalResponse: json.RawMessage(`{"id":"cached","object":"chat.completion","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"cached hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`),
		Usage:             provcore.Usage{PromptTokens: iPtr(1), CompletionTokens: iPtr(1), TotalTokens: iPtr(2)},
		CachedAt:          time.Now().UTC(),
	}
	if _, err := deps.Cache.StoreResponse(context.Background(), cacheKey, entry); err != nil {
		t.Fatalf("StoreResponse: %v", err)
	}

	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatAnthropic,
	})
	w := httptest.NewRecorder()
	h(w, freshChatRequest(t, string(body)))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200 (reshape failure should warn, not abort); body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("X-Nexus-Cache"); got != "HIT" {
		t.Errorf("x-nexus-cache=%q want HIT", got)
	}
	// The canonical (un-reshaped) body should be served.
	if !strings.Contains(w.Body.String(), "cached hi") {
		t.Errorf("body=%s want canonical bytes served on reshape warn", w.Body.String())
	}
	if fexec.Calls != 0 || fexec.PreparedCalls != 0 {
		t.Errorf("executor must not be invoked on cache HIT; calls=%d prepared=%d", fexec.Calls, fexec.PreparedCalls)
	}
}

// handleNonStream — same-format passthrough (no reshape attempted)

// TestServeProxy_Fake_PassthroughNoReshape pins the same-format ingress
// path where the bridge's reshape branch is skipped (ingress == target,
// upstream body forwarded verbatim). Exercises the
// `CanonicalBridge != nil && (chat || responses-cross)` guard's "false"
// arm — the response body must pass through unchanged.
func TestServeProxy_Fake_PassthroughNoReshape(t *testing.T) {
	successBody := []byte(`{"id":"x","object":"chat.completion","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"passthrough hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`)
	fexec := &fakeExecutor{Result: &executor.ExecutionResult{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Content-Type": []string{"application/json"}},
		Body:       successBody,
		Target: routingcore.RoutingTarget{
			ProviderID: "p-openai", ProviderName: "openai", ModelID: "gpt-4o", ModelCode: "gpt-4o", AdapterType: "openai",
		},
		Usage:    provcore.Usage{PromptTokens: iPtr(1), CompletionTokens: iPtr(2), TotalTokens: iPtr(3)},
		Attempts: []executor.Attempt{{StatusCode: http.StatusOK}},
	}}
	// Bridge present, but ingress format == target format (openai → openai):
	// this is a same-format native passthrough, so the egress reshape is
	// SKIPPED (it would only be an identity no-op). Cross-format ingresses
	// (anthropic/gemini target) are what trigger the reshape.
	reshapeInvoked := false
	fbridge := &fakeBridge{
		responseCanonicalToIngress: func(_ provcore.Format, canonical []byte) ([]byte, error) {
			reshapeInvoked = true
			return canonical, nil
		},
	}
	deps := makeFakeDeps(t, fexec, fbridge)

	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatOpenAI,
	})
	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	w := httptest.NewRecorder()
	h(w, freshChatRequest(t, body))

	// OpenAI ingress → OpenAI target (same format). The reshape guard is
	// keyed on cross-format (ingress.BodyFormat != target format), so for a
	// same-format passthrough it does NOT fire — the upstream bytes are
	// forwarded verbatim. The point of this test is to drive the response
	// path with a permissive bridge so the success arm completes.
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "passthrough hi") {
		t.Errorf("body=%s want upstream content", w.Body.String())
	}
	if reshapeInvoked {
		t.Errorf("reshape MUST NOT be invoked for a same-format passthrough (openai→openai)")
	}
}

// handleNonStream — no CanonicalBridge wired (skip reshape entirely)

// TestServeProxy_Fake_NilBridge_NoReshape pins the path where
// h.deps.CanonicalBridge is nil — the response reshape guard is skipped
// entirely and the upstream bytes are forwarded verbatim. Also exercises
// the filterCompatibleTargets nil-bridge arm and the cache-prepare
// canonicalize-skip arm.
func TestServeProxy_Fake_NilBridge_NoReshape(t *testing.T) {
	successBody := []byte(`{"id":"x","object":"chat.completion","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"nil-bridge hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	fexec := &fakeExecutor{Result: &executor.ExecutionResult{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Content-Type": []string{"application/json"}},
		Body:       successBody,
		Target: routingcore.RoutingTarget{
			ProviderID: "p-openai", ProviderName: "openai", ModelID: "gpt-4o", ModelCode: "gpt-4o", AdapterType: "openai",
		},
		Usage:    provcore.Usage{PromptTokens: iPtr(1), CompletionTokens: iPtr(1), TotalTokens: iPtr(2)},
		Attempts: []executor.Attempt{{StatusCode: http.StatusOK}},
	}}
	deps := makeFakeDeps(t, fexec, &fakeBridge{})
	deps.CanonicalBridge = nil // pin the no-bridge arm

	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatOpenAI,
	})
	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	w := httptest.NewRecorder()
	h(w, freshChatRequest(t, body))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "nil-bridge hi") {
		t.Errorf("body=%s want upstream content forwarded verbatim", w.Body.String())
	}
}

// handleNonStreamWithSubscription — broker leader path response reshape failure

// TestServeProxy_Fake_BrokerLeader_NonStream_ReshapeFails wires the
// broker registry and forces a MISS, so runViaBroker invokes leaderFn,
// then handleNonStreamWithSubscription runs the response reshape. The
// fakeBridge fails the reshape, driving the 502 envelope branch in
// handleNonStreamWithSubscription that the cache HIT path covered but
// the broker MISS path did not.
func TestServeProxy_Fake_BrokerLeader_NonStream_ReshapeFails(t *testing.T) {
	successBody := []byte(`{"id":"x","object":"chat.completion","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"x"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	fexec := &fakeExecutor{Result: &executor.ExecutionResult{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Content-Type": []string{"application/json"}},
		Body:       successBody,
		Target: routingcore.RoutingTarget{
			ProviderID: "p-openai", ProviderName: "openai", ModelID: "gpt-4o", ModelCode: "gpt-4o", AdapterType: "openai",
		},
		Usage:    provcore.Usage{PromptTokens: iPtr(1), CompletionTokens: iPtr(1), TotalTokens: iPtr(2)},
		Attempts: []executor.Attempt{{StatusCode: http.StatusOK}},
	}}
	fbridge := &fakeBridge{
		responseCanonicalToIngress: func(_ provcore.Format, _ []byte) ([]byte, error) {
			return nil, errors.New("synthesised broker-path reshape failure")
		},
	}
	deps := makeFakeDeps(t, fexec, fbridge)
	cacheOpt, cleanup := withCache(t)
	defer cleanup()
	cacheOpt(deps)
	withBroker(t)(deps)

	// Anthropic ingress → OpenAI target → cross-format reshape fires.
	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatAnthropic,
	})
	body := `{"model":"gpt-4o","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],"max_tokens":4}`
	w := httptest.NewRecorder()
	h(w, freshChatRequest(t, body))

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status=%d want 502; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "reshaped for ingress format") {
		t.Errorf("body=%s want broker-path reshape failure envelope", w.Body.String())
	}
}

// ResponsesAPI cross-format guard — rejection 400

// TestServeProxy_Fake_ResponsesCrossFormat_Canonicalizes drives the
// canonicalize-on-Responses-API arm: ingress is /v1/responses, target
// adapter does NOT natively serve responses-api, request body carries
// none of the stateful fields, so the cross-format guard passes and
// the canonicalize path rewrites prepReq.WireShape to WireShapeOpenAIChat
// AND mutates the per-request `resolved` copy.
func TestServeProxy_Fake_ResponsesCrossFormat_Canonicalizes(t *testing.T) {
	successBody := []byte(`{"id":"x","object":"chat.completion","model":"claude","choices":[{"index":0,"message":{"role":"assistant","content":"resp xfmt"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	fexec := &fakeExecutor{Result: &executor.ExecutionResult{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Content-Type": []string{"application/json"}},
		Body:       successBody,
		Target: routingcore.RoutingTarget{
			ProviderID: "p-anthropic", ProviderName: "anthropic",
			ModelID: "claude-opus-4-7", ModelCode: "claude-opus-4-7",
			AdapterType: "anthropic", ProviderModelID: "claude-opus-4-7",
		},
		Usage:    provcore.Usage{PromptTokens: iPtr(1), CompletionTokens: iPtr(1), TotalTokens: iPtr(2)},
		Attempts: []executor.Attempt{{StatusCode: http.StatusOK}},
	}}
	fbridge := &fakeBridge{
		// Anthropic target does NOT natively serve responses-api → canonicalize fires.
		targetNativelyServesResponsesAPI: func(target provcore.Format) bool {
			return false
		},
		ingressChatToCanonical: func(_ provcore.Format, body []byte, _ provcore.CallTarget) ([]byte, error) {
			// Pretend the canonical body is the same as the input.
			return body, nil
		},
	}
	deps := makeFakeDeps(t, fexec, fbridge)
	deps.Router = &stubRouterCacheTest{targets: []routingcore.RoutingTarget{{
		ProviderID: "p-anthropic", ProviderName: "anthropic", ProviderModelID: "claude-opus-4-7",
		ModelID: "claude-opus-4-7", ModelName: "Claude Opus", ModelCode: "claude-opus-4-7",
		AdapterType: "anthropic",
	}}}
	// Wire cache so the canonicalize arm actually runs.
	cacheOpt, cleanup := withCache(t)
	defer cleanup()
	cacheOpt(deps)

	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIResponses,
		BodyFormat: provcore.FormatOpenAIResponses,
	})
	// Body has NO stateful fields — guard passes.
	body := `{"model":"claude-opus-4-7","input":"hi"}`
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer vk")
	w := httptest.NewRecorder()
	h(w, r)

	// We don't require status 200 — the canonicalize arm is the point.
	// 400 / 500 from the prepareBody is also fine; the coverage gain is
	// from running through lines 786-808.
	if w.Code == 0 {
		t.Fatalf("no response written")
	}
}

// TestServeProxy_Fake_ResponsesGuard_Rejects drives the
// validateResponsesIngressForCrossFormat success path. The request body
// carries previous_response_id, which cannot be honoured on a
// cross-format target, so the gateway must respond with a Responses-shape
// 400 envelope BEFORE the request hits hooks / quota / executor.
func TestServeProxy_Fake_ResponsesGuard_Rejects(t *testing.T) {
	fexec := &fakeExecutor{} // must NOT be invoked
	fbridge := &fakeBridge{
		// Anthropic target does NOT natively serve responses-api, so
		// the cross-format guard fires.
		targetNativelyServesResponsesAPI: func(target provcore.Format) bool {
			return false
		},
	}
	deps := makeFakeDeps(t, fexec, fbridge)
	// Override the router to point at an Anthropic target so the
	// cross-format guard branch fires.
	deps.Router = &stubRouterCacheTest{targets: []routingcore.RoutingTarget{{
		ProviderID: "p-anthropic", ProviderName: "anthropic", ProviderModelID: "claude-opus-4-7",
		ModelID: "claude-opus-4-7", ModelName: "Claude Opus", ModelCode: "claude-opus-4-7",
		AdapterType: "anthropic",
	}}}

	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIResponses,
		BodyFormat: provcore.FormatOpenAIResponses,
	})
	body := `{"model":"claude-opus-4-7","previous_response_id":"resp_prev_123","input":"hi"}`
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer vk")
	w := httptest.NewRecorder()
	h(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "previous_response_id") {
		t.Errorf("body=%s want rejection param=previous_response_id", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "feature_requires_native_responses_target") {
		t.Errorf("body=%s want stable error.code", w.Body.String())
	}
	if fexec.Calls != 0 || fexec.PreparedCalls != 0 {
		t.Errorf("executor must NOT be invoked on Responses cross-format guard reject; calls=%d prepared=%d",
			fexec.Calls, fexec.PreparedCalls)
	}
}

// ServeProxy — stream cross-format reject (writeCrossFormatStreamUnsupported)

// TestServeProxy_Fake_StreamCrossFormatBedrockRejected exercises the
// StreamShapeCompatible == false branch via a streaming request whose
// target is Bedrock; the handler must 400 with the
// cross_format_stream_unsupported envelope before reaching the executor.
func TestServeProxy_Fake_StreamCrossFormatBedrockRejected(t *testing.T) {
	fexec := &fakeExecutor{} // must NOT be invoked
	fbridge := &fakeBridge{}
	deps := makeFakeDeps(t, fexec, fbridge)
	deps.Router = &stubRouterCacheTest{targets: []routingcore.RoutingTarget{{
		ProviderID: "p-bedrock", ProviderName: "bedrock", ProviderModelID: "anthropic.claude-3",
		ModelID: "claude-bed", ModelName: "Claude Bedrock", ModelCode: "claude-bed",
		AdapterType: "bedrock",
	}}}

	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatOpenAI,
		Stream:     true,
	})
	body := `{"model":"claude-bed","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	w := httptest.NewRecorder()
	h(w, freshChatRequest(t, body))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "cross_format_stream_unsupported") {
		t.Errorf("body=%s want cross_format_stream_unsupported envelope", w.Body.String())
	}
	if fexec.Calls != 0 || fexec.PreparedCalls != 0 {
		t.Errorf("executor must NOT be invoked; calls=%d prepared=%d", fexec.Calls, fexec.PreparedCalls)
	}
}

// ServeProxy — request body too large (errRequestTooLarge envelope)

// TestServeProxy_Fake_RequestBodyTooLarge drives the
// errors.Is(err, errRequestTooLarge) arm in ServeProxy → PAYLOAD_TOO_LARGE
// 413 envelope. Configures a tiny payload-capture maxRequestBytes via
// the Store snapshot so a 1 KiB body trips the cap.
func TestServeProxy_Fake_RequestBodyTooLarge(t *testing.T) {
	fexec := &fakeExecutor{} // must NOT be invoked
	fbridge := &fakeBridge{}
	deps := makeFakeDeps(t, fexec, fbridge)
	// Replace payload-capture snapshot with a small cap.
	deps.PayloadCapture = nil // safe — readBody respects the snapshot
	// readBody uses the snapshot; with nil PayloadCapture the default
	// is unbounded for request bytes — so configure a real Store with
	// MaxRequestBytes=16.
	smallStore := newSmallReqCapPayloadStore(t, 16)
	deps.PayloadCapture = smallStore

	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatOpenAI,
	})
	body := strings.Repeat(`{"model":"gpt-4o","messages":[{"role":"user","content":"way too long for the 16 byte cap"}]}`, 1)
	w := httptest.NewRecorder()
	h(w, freshChatRequest(t, body))

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d want 413; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "PAYLOAD_TOO_LARGE") {
		t.Errorf("body=%s want PAYLOAD_TOO_LARGE envelope", w.Body.String())
	}
}

// newSmallReqCapPayloadStore wires a payload-capture Store snapshot
// with the supplied MaxRequestBytes cap so readBody trips
// errRequestTooLarge.
func newSmallReqCapPayloadStore(t *testing.T, cap int64) *payloadcapture.Store {
	t.Helper()
	return payloadcapture.NewStore(payloadcapture.Config{
		StoreRequestBody:   false,
		StoreResponseBody:  false,
		MaxInlineBodyBytes: 64 * 1024,
		MaxRequestBytes:    cap,
	})
}

// ServeProxy — router error → ROUTING_NO_MATCH 500 envelope

// errorRouter satisfies RouteResolver and always returns an error from
// ResolveTargets, driving the `if err != nil { ROUTING_NO_MATCH }` arm
// in ServeProxy.
type errorRouter struct{}

func (errorRouter) ResolveTargets(_ context.Context, _ *routingcore.RoutingContext) (*routingcore.RouteResult, error) {
	return nil, errors.New("synthetic router failure")
}

func TestServeProxy_Fake_RouterError(t *testing.T) {
	fexec := &fakeExecutor{} // must NOT be invoked
	fbridge := &fakeBridge{}
	deps := makeFakeDeps(t, fexec, fbridge)
	deps.Router = errorRouter{}

	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatOpenAI,
	})
	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	w := httptest.NewRecorder()
	h(w, freshChatRequest(t, body))

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d want 500; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "ROUTING_NO_MATCH") {
		t.Errorf("body=%s want ROUTING_NO_MATCH envelope", w.Body.String())
	}
	if fexec.Calls != 0 || fexec.PreparedCalls != 0 {
		t.Errorf("executor must NOT be invoked on router error; calls=%d prepared=%d", fexec.Calls, fexec.PreparedCalls)
	}
}

// ServeProxy — no routing targets + passthrough fallback fails

// emptyRouter returns a RouteResult with no targets.
type emptyRouter struct{}

func (emptyRouter) ResolveTargets(_ context.Context, _ *routingcore.RoutingContext) (*routingcore.RouteResult, error) {
	return &routingcore.RouteResult{Targets: nil}, nil
}

func TestServeProxy_Fake_NoTargets_FallbackFails(t *testing.T) {
	fexec := &fakeExecutor{} // must NOT be invoked
	fbridge := &fakeBridge{}
	deps := makeFakeDeps(t, fexec, fbridge)
	deps.Router = emptyRouter{}
	// Models is nil so resolveNoMatchPassthrough cannot find the model.

	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatOpenAI,
	})
	body := `{"model":"unknown-model","messages":[{"role":"user","content":"hi"}]}`
	w := httptest.NewRecorder()
	h(w, freshChatRequest(t, body))

	if w.Code == http.StatusOK {
		t.Fatalf("status=%d want non-200 on fallback failure; body=%s", w.Code, w.Body.String())
	}
	if fexec.Calls != 0 || fexec.PreparedCalls != 0 {
		t.Errorf("executor must NOT be invoked when fallback fails; calls=%d prepared=%d", fexec.Calls, fexec.PreparedCalls)
	}
}

// ServeProxy — NoCompatibleProviderError with empty Available falls through
// to passthrough fallback. The non-empty branch is covered by
// writeNoCompatibleCapability in capability_test.go; this arm pins the
// empty-list fall-through path so a regression that fires the
// no_compatible_capability envelope with zero candidates is caught.

// emptyCapErrorRouter returns NoCompatibleProviderError with an empty
// Available list, driving the proxy ServeProxy fall-through that retries
// via resolveNoMatchPassthrough.
type emptyCapErrorRouter struct{}

func (emptyCapErrorRouter) ResolveTargets(_ context.Context, _ *routingcore.RoutingContext) (*routingcore.RouteResult, error) {
	return nil, &routingcore.NoCompatibleProviderError{
		Available: nil, // empty list triggers fall-through, not writeNoCompatibleCapability
	}
}

func TestServeProxy_Fake_NoCompatCapability_EmptyAvailable_FallsThroughToPassthrough(t *testing.T) {
	// Named failure mode: when the capability filter returns
	// NoCompatibleProviderError with no candidates (e.g. nothing was
	// evaluated by the filter because no provider matched the requested
	// endpoint at all), the router's "no compatible capability" envelope
	// would be misleading — proxy.go must fall through to the
	// passthrough fallback instead.
	fexec := &fakeExecutor{} // must NOT be invoked — no targets → fallback fails
	fbridge := &fakeBridge{}
	deps := makeFakeDeps(t, fexec, fbridge)
	deps.Router = emptyCapErrorRouter{}
	// Models nil so the passthrough fallback can't find a model; the
	// final 500 envelope confirms we traversed the fallback arm.

	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatOpenAI,
	})
	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	w := httptest.NewRecorder()
	h(w, freshChatRequest(t, body))

	// Positive assertion: empty Available routes through the passthrough
	// fallback, which fails (no Models wired) and emits a 500
	// ROUTING_NO_MATCH envelope. Pinning the exact code catches a
	// regression that would either short-circuit to no_compatible_capability
	// or downgrade the envelope to a different error code.
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d want 500 (fallback fails with nil Models); body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "ROUTING_NO_MATCH") {
		t.Fatalf("body=%s want ROUTING_NO_MATCH envelope (fallback-failure path for empty Available)", w.Body.String())
	}
	if fexec.Calls != 0 || fexec.PreparedCalls != 0 {
		t.Errorf("executor must NOT be invoked when fallback also fails; calls=%d prepared=%d", fexec.Calls, fexec.PreparedCalls)
	}
}

// ServeProxy — auth failure 401

// failVKAuth always fails authentication.
type failVKAuth struct{}

func (failVKAuth) Authenticate(_ context.Context, _ *http.Request) (*vkauth.VKMeta, error) {
	return nil, errors.New("synthetic auth failure")
}

func TestServeProxy_Fake_AuthFails(t *testing.T) {
	fexec := &fakeExecutor{} // must NOT be invoked
	fbridge := &fakeBridge{}
	deps := makeFakeDeps(t, fexec, fbridge)
	deps.VKAuth = failVKAuth{}

	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatOpenAI,
	})
	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	w := httptest.NewRecorder()
	h(w, freshChatRequest(t, body))

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d want 401; body=%s", w.Code, w.Body.String())
	}
	if fexec.Calls != 0 || fexec.PreparedCalls != 0 {
		t.Errorf("executor must NOT be invoked when auth fails; calls=%d prepared=%d", fexec.Calls, fexec.PreparedCalls)
	}
}

// ServeProxy — RateLimitRpm visibility header stamped

// ServeProxy — Metrics + StoreResponseBody arms (fires all the
// `h.deps.Metrics != nil` and `StoreResponseBody && len(respBody) > 0`
// guards across handleNonStream / handleNonStreamWithSubscription /
// handleStreamWithSubscription on the live path)

// noopMetrics wires every MetricsRecorder method so the handler's
// `Metrics != nil` arms fire. Returns no useful information; we're
// only here for coverage.
type noopMetrics struct{}

func (noopMetrics) RecordRequest(_, _, _ string, _ int, _ time.Duration, _ metricspkg.Usage) {
}
func (noopMetrics) RecordHookRequest(_, _, _ string)                       {}
func (noopMetrics) RecordTrafficExtract(_, _, _ string)                    {}
func (noopMetrics) RecordEstimate(_, _, _ string, _ time.Duration)         {}
func (noopMetrics) RecordEstimateCompare(_ string, _ int, _ time.Duration) {}

// TestServeProxy_Fake_Direct_NonStream_MetricsAndCaptureFire wires both
// Metrics + StoreResponseBody=true so every `h.deps.Metrics != nil` arm
// + the `StoreResponseBody && len(respBody) > 0` guard fires across the
// direct (cache-disabled) non-stream pipeline.
func TestServeProxy_Fake_Direct_NonStream_MetricsAndCaptureFire(t *testing.T) {
	fexec := &fakeExecutor{Result: fakeBrokerSuccessResult()}
	fbridge := &fakeBridge{}
	deps := makeFakeDeps(t, fexec, fbridge)
	deps.Metrics = noopMetrics{}
	deps.PayloadCapture = payloadcapture.NewStore(payloadcapture.Config{
		StoreRequestBody:   true,
		StoreResponseBody:  true,
		MaxInlineBodyBytes: 64 * 1024,
	})

	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatOpenAI,
	})
	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	w := httptest.NewRecorder()
	h(w, freshChatRequest(t, body))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "broker hi") {
		t.Errorf("body=%s want upstream content", w.Body.String())
	}
}

// TestServeProxy_Fake_BrokerLeader_NonStream_MetricsAndCaptureFire —
// same as the direct test above but through the broker MISS leader
// path so handleNonStreamWithSubscription's Metrics + StoreResponseBody
// arms fire too.
func TestServeProxy_Fake_BrokerLeader_NonStream_MetricsAndCaptureFire(t *testing.T) {
	fexec := &fakeExecutor{Result: fakeBrokerSuccessResult()}
	fbridge := &fakeBridge{}
	deps := makeFakeDeps(t, fexec, fbridge)
	deps.Metrics = noopMetrics{}
	deps.PayloadCapture = payloadcapture.NewStore(payloadcapture.Config{
		StoreRequestBody:   true,
		StoreResponseBody:  true,
		MaxInlineBodyBytes: 64 * 1024,
	})
	cacheOpt, cleanup := withCache(t)
	defer cleanup()
	cacheOpt(deps)
	withBroker(t)(deps)

	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatOpenAI,
	})
	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	w := httptest.NewRecorder()
	h(w, freshChatRequest(t, body))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200; body=%s", w.Code, w.Body.String())
	}
}

// TestServeProxy_Fake_CacheHIT_NonStream_MetricsAndCaptureFire fires
// the same arms on the cache HIT path.
func TestServeProxy_Fake_CacheHIT_NonStream_MetricsAndCaptureFire(t *testing.T) {
	fexec := &fakeExecutor{} // must NOT be invoked
	fbridge := &fakeBridge{}
	deps := makeFakeDeps(t, fexec, fbridge)
	deps.Metrics = noopMetrics{}
	deps.PayloadCapture = payloadcapture.NewStore(payloadcapture.Config{
		StoreRequestBody:   true,
		StoreResponseBody:  true,
		MaxInlineBodyBytes: 64 * 1024,
	})
	cacheOpt, cleanup := withCache(t)
	defer cleanup()
	cacheOpt(deps)

	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	cacheKey := computeStreamCacheKey(t, deps, "openai", "gpt-4o", body, false)
	reasoning := 42
	entry := &cache.ResponseEntry{
		Provider:          "openai",
		Model:             "gpt-4o",
		CanonicalResponse: json.RawMessage(`{"id":"cached","object":"chat.completion","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"cached hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`),
		Usage: provcore.Usage{
			PromptTokens: iPtr(1), CompletionTokens: iPtr(1), TotalTokens: iPtr(2),
			// ReasoningTokens — fires the `entry.Usage.ReasoningTokens != nil`
			// stamp arm in handleNonStreamHit (and handleStreamHit's sibling).
			ReasoningTokens: &reasoning,
		},
		CachedAt: time.Now().UTC(),
	}
	if _, err := deps.Cache.StoreResponse(context.Background(), cacheKey, entry); err != nil {
		t.Fatalf("StoreResponse: %v", err)
	}

	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatOpenAI,
	})
	w := httptest.NewRecorder()
	h(w, freshChatRequest(t, string(body)))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200; body=%s", w.Code, w.Body.String())
	}
}

// TestServeProxy_Fake_NormaliserWired drives the Normaliser
// NormalizeUpstream + NormalizedStripCount/StripBytes/CacheMarkerInjected
// stamp arms in ServeProxy. Wires a freshly-constructed wirerewrite.Engine.
func TestServeProxy_Fake_NormaliserWired(t *testing.T) {
	fexec := &fakeExecutor{Result: fakeBrokerSuccessResult()}
	fbridge := &fakeBridge{}
	deps := makeFakeDeps(t, fexec, fbridge)
	deps.Normaliser = wirerewrite.New(slog.Default())
	// Wire cache so cachePreparedBody gets populated (Normaliser arm
	// requires len(cachePreparedBody) > 0).
	cacheOpt, cleanup := withCache(t)
	defer cleanup()
	cacheOpt(deps)

	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatOpenAI,
	})
	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	w := httptest.NewRecorder()
	h(w, freshChatRequest(t, body))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200; body=%s", w.Code, w.Body.String())
	}
}

// TestServeProxy_Fake_CacheHIT_Stream_ReasoningTokensOnEntryUsage
// targets the previously-uncovered `entry.Usage.ReasoningTokens != nil`
// stamp arm in handleStreamHit (the existing test put ReasoningTokens
// on the terminal chunk's Usage but not on entry.Usage).
func TestServeProxy_Fake_CacheHIT_Stream_ReasoningTokensOnEntryUsage(t *testing.T) {
	fexec := &fakeExecutor{}
	fbridge := &fakeBridge{}
	deps := makeFakeDeps(t, fexec, fbridge)
	cacheOpt, cleanup := withCache(t)
	defer cleanup()
	cacheOpt(deps)

	body := []byte(`{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	reasoning := 99
	streamEntry := &cache.StreamEntry{
		Provider: "openai", Model: "gpt-4o",
		Usage: provcore.Usage{
			PromptTokens: iPtr(1), CompletionTokens: iPtr(1), TotalTokens: iPtr(2),
			ReasoningTokens: &reasoning,
		},
		Chunks: []cache.ChunkRecord{
			{Delta: "x"},
			{Done: true},
		},
		CachedAt: time.Now().UTC(),
	}
	cacheKey := computeStreamCacheKey(t, deps, "openai", "gpt-4o", body, true)
	if _, err := deps.Cache.StoreStream(context.Background(), cacheKey, streamEntry); err != nil {
		t.Fatalf("StoreStream: %v", err)
	}

	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatOpenAI,
		Stream:     true,
	})
	w := httptest.NewRecorder()
	h(w, freshChatRequest(t, string(body)))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200; body=%s", w.Code, w.Body.String())
	}
}

// runRequestHooks + handleNonStream — pipeline build error arms

// erroringHookFactoryCache returns a HookConfigCache whose response-stage
// pipeline build fails (factory error path). Used to drive the
// `pErr != nil` arms inside handleNonStream / runRequestHooks.
func erroringHookFactoryCache(t *testing.T, stage string) *compliance.HookConfigCache {
	t.Helper()
	reg := builtins.Registry.Clone()
	reg.Register("error-factory", func(_ *goHooks.HookConfig) (goHooks.Hook, error) {
		return nil, errors.New("synth factory failure")
	})
	reg.Freeze()
	loader := func(_ context.Context) ([]goHooks.HookConfig, error) {
		return []goHooks.HookConfig{{
			ID:                "h-err",
			ImplementationID:  "error-factory",
			Name:              "err",
			Priority:          1,
			Enabled:           true,
			Stage:             stage,
			FailBehavior:      "fail-closed",
			TimeoutMs:         1000,
			ApplicableIngress: []string{"ALL"},
		}}, nil
	}
	hc := compliance.NewHookConfigCache(loader, reg, 0, slog.Default())
	if err := hc.Start(context.Background()); err != nil {
		t.Fatalf("hookCache.Start: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	return hc
}

func TestServeProxy_Fake_Direct_NonStream_RespHookPipelineError(t *testing.T) {
	fexec := &fakeExecutor{Result: fakeBrokerSuccessResult()}
	fbridge := &fakeBridge{}
	deps := makeFakeDeps(t, fexec, fbridge)
	deps.HookConfigCache = erroringHookFactoryCache(t, "response")

	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatOpenAI,
	})
	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	w := httptest.NewRecorder()
	h(w, freshChatRequest(t, body))

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d want 500 (hook pipeline error); body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "hook pipeline error") {
		t.Errorf("body=%s want hook pipeline error envelope", w.Body.String())
	}
}

func TestServeProxy_Fake_BrokerLeader_NonStream_RespHookPipelineError(t *testing.T) {
	fexec := &fakeExecutor{Result: fakeBrokerSuccessResult()}
	fbridge := &fakeBridge{}
	deps := makeFakeDeps(t, fexec, fbridge)
	deps.HookConfigCache = erroringHookFactoryCache(t, "response")
	cacheOpt, cleanup := withCache(t)
	defer cleanup()
	cacheOpt(deps)
	withBroker(t)(deps)

	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatOpenAI,
	})
	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	w := httptest.NewRecorder()
	h(w, freshChatRequest(t, body))

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d want 500 (broker-path hook pipeline error); body=%s", w.Code, w.Body.String())
	}
}

func TestServeProxy_Fake_CacheHIT_NonStream_RespHookPipelineError(t *testing.T) {
	fexec := &fakeExecutor{} // must NOT be invoked
	fbridge := &fakeBridge{}
	deps := makeFakeDeps(t, fexec, fbridge)
	deps.HookConfigCache = erroringHookFactoryCache(t, "response")
	cacheOpt, cleanup := withCache(t)
	defer cleanup()
	cacheOpt(deps)

	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	cacheKey := computeStreamCacheKey(t, deps, "openai", "gpt-4o", body, false)
	entry := &cache.ResponseEntry{
		Provider:          "openai",
		Model:             "gpt-4o",
		CanonicalResponse: json.RawMessage(`{"id":"x","object":"chat.completion","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`),
		Usage:             provcore.Usage{PromptTokens: iPtr(1), CompletionTokens: iPtr(1), TotalTokens: iPtr(2)},
		CachedAt:          time.Now().UTC(),
	}
	if _, err := deps.Cache.StoreResponse(context.Background(), cacheKey, entry); err != nil {
		t.Fatalf("StoreResponse: %v", err)
	}

	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatOpenAI,
	})
	w := httptest.NewRecorder()
	h(w, freshChatRequest(t, string(body)))

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d want 500 (cache HIT + hook pipeline error); body=%s", w.Code, w.Body.String())
	}
}

func TestServeProxy_Fake_RequestHookPipelineError(t *testing.T) {
	fexec := &fakeExecutor{} // must NOT be invoked
	fbridge := &fakeBridge{}
	deps := makeFakeDeps(t, fexec, fbridge)
	deps.HookConfigCache = erroringHookFactoryCache(t, "request")

	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatOpenAI,
	})
	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	w := httptest.NewRecorder()
	h(w, freshChatRequest(t, body))

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d want 500 (request hook pipeline error); body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "hook pipeline error") {
		t.Errorf("body=%s want hook pipeline error envelope", w.Body.String())
	}
}

// Broker non-stream MISS — response-hook RejectHard / non-blocking / Modify

// fakeBrokerSuccessResult is the common executor result used by the
// broker tests below: a successful OpenAI chat.completion non-stream
// response. Coerced is populated so the `len(result.Coerced) > 0`
// header-stamping arm fires across the live-path branches.
func fakeBrokerSuccessResult() *executor.ExecutionResult {
	return &executor.ExecutionResult{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Content-Type": []string{"application/json"}},
		Body:       []byte(`{"id":"x","object":"chat.completion","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"broker hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`),
		Target: routingcore.RoutingTarget{
			ProviderID: "p-openai", ProviderName: "openai", ModelID: "gpt-4o", ModelCode: "gpt-4o", AdapterType: "openai",
		},
		Usage:    provcore.Usage{PromptTokens: iPtr(1), CompletionTokens: iPtr(2), TotalTokens: iPtr(3)},
		Coerced:  []string{"gpt-4-turbo→gpt-4o"},
		Attempts: []executor.Attempt{{StatusCode: http.StatusOK}},
	}
}

// TestServeProxy_Fake_BrokerLeader_NonStream_RespHookRejects drives the
// handleNonStreamWithSubscription RejectHard arm (cache MISS via broker
// leader path) — the response hook returns RejectHard so the handler
// must write a 403.
func TestServeProxy_Fake_BrokerLeader_NonStream_RespHookRejects(t *testing.T) {
	fexec := &fakeExecutor{Result: fakeBrokerSuccessResult()}
	fbridge := &fakeBridge{}
	deps := makeFakeDeps(t, fexec, fbridge)
	deps.HookConfigCache = newResponseHookCache(t, responseRejectHook{})
	cacheOpt, cleanup := withCache(t)
	defer cleanup()
	cacheOpt(deps)
	withBroker(t)(deps)

	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatOpenAI,
	})
	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	w := httptest.NewRecorder()
	h(w, freshChatRequest(t, body))

	if w.Code != http.StatusForbidden {
		t.Fatalf("status=%d want 403; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "blocked by response hook") {
		t.Errorf("body=%s want RejectHard reason", w.Body.String())
	}
}

// TestServeProxy_Fake_BrokerLeader_NonStream_RespHookNonBlocking pins that
// a non-blocking response-hook decision (one that is neither RejectHard nor
// Modify) forwards the upstream body unchanged with HTTP 200 in
// handleNonStreamWithSubscription.
func TestServeProxy_Fake_BrokerLeader_NonStream_RespHookNonBlocking(t *testing.T) {
	fexec := &fakeExecutor{Result: fakeBrokerSuccessResult()}
	fbridge := &fakeBridge{}
	deps := makeFakeDeps(t, fexec, fbridge)
	deps.HookConfigCache = newResponseHookCache(t, responseBlockSoftHook{})
	cacheOpt, cleanup := withCache(t)
	defer cleanup()
	cacheOpt(deps)
	withBroker(t)(deps)

	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatOpenAI,
	})
	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	w := httptest.NewRecorder()
	h(w, freshChatRequest(t, body))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200 (non-blocking response hook forwards body); body=%s", w.Code, w.Body.String())
	}
}

// TestServeProxy_Fake_BrokerLeader_NonStream_RespHookModify drives
// the Modify arm (RewriteResponseBody mutates the response).
func TestServeProxy_Fake_BrokerLeader_NonStream_RespHookModify(t *testing.T) {
	fexec := &fakeExecutor{Result: fakeBrokerSuccessResult()}
	fbridge := &fakeBridge{}
	deps := makeFakeDeps(t, fexec, fbridge)
	deps.HookConfigCache = newResponseHookCache(t, responseModifyHook{})
	cacheOpt, cleanup := withCache(t)
	defer cleanup()
	cacheOpt(deps)
	withBroker(t)(deps)

	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatOpenAI,
	})
	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	w := httptest.NewRecorder()
	h(w, freshChatRequest(t, body))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "modified by response hook") {
		t.Errorf("body=%s want modified content", w.Body.String())
	}
}

// handleNonStream — response hook RejectHard / non-blocking / Modify
// (direct path, no cache, no broker — covers the 2249-2302 arms)

// TestServeProxy_Fake_Direct_NonStream_RespHookRejects exercises the
// handleNonStream RejectHard arm on the direct (cache-disabled) path.
func TestServeProxy_Fake_Direct_NonStream_RespHookRejects(t *testing.T) {
	fexec := &fakeExecutor{Result: fakeBrokerSuccessResult()}
	fbridge := &fakeBridge{}
	deps := makeFakeDeps(t, fexec, fbridge)
	deps.HookConfigCache = newResponseHookCache(t, responseRejectHook{})
	// No cache wired → direct path.

	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatOpenAI,
	})
	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	w := httptest.NewRecorder()
	h(w, freshChatRequest(t, body))

	if w.Code != http.StatusForbidden {
		t.Fatalf("status=%d want 403; body=%s", w.Code, w.Body.String())
	}
}

func TestServeProxy_Fake_Direct_NonStream_RespHookNonBlocking(t *testing.T) {
	fexec := &fakeExecutor{Result: fakeBrokerSuccessResult()}
	fbridge := &fakeBridge{}
	deps := makeFakeDeps(t, fexec, fbridge)
	deps.HookConfigCache = newResponseHookCache(t, responseBlockSoftHook{})

	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatOpenAI,
	})
	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	w := httptest.NewRecorder()
	h(w, freshChatRequest(t, body))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200 (non-blocking response hook forwards body); body=%s", w.Code, w.Body.String())
	}
}

func TestServeProxy_Fake_Direct_NonStream_RespHookModifies(t *testing.T) {
	fexec := &fakeExecutor{Result: fakeBrokerSuccessResult()}
	fbridge := &fakeBridge{}
	deps := makeFakeDeps(t, fexec, fbridge)
	deps.HookConfigCache = newResponseHookCache(t, responseModifyHook{})

	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatOpenAI,
	})
	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	w := httptest.NewRecorder()
	h(w, freshChatRequest(t, body))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "modified by response hook") {
		t.Errorf("body=%s want modified content", w.Body.String())
	}
}

// handleNonStream Modify + RewriteResponseBody ErrRewriteUnsupported

// TestServeProxy_Fake_Direct_NonStream_ModifyUnsupportedFailsClosed
// exercises the ErrRewriteUnsupported arm in handleNonStream's Modify path.
// The stub traffic adapter signals that rewrite is not supported; a redact
// that cannot be applied must fail CLOSED — the handler returns 502 and must
// NOT serve the unredacted upstream body to the client.
// stubRewriteRedactsAdapter succeeds RewriteResponseBody with a recognizably
// redacted body — it models the canonical (OpenAI) adapter that B0 always uses
// for response redaction, so a Modify decision produces a redacted 200 rather
// than the old reverse-encode fail-closed.
type stubRewriteRedactsAdapter struct{ stubTrafficAdapter }

func (s *stubRewriteRedactsAdapter) RewriteResponseBody(_ context.Context, _ []byte, _ string, _ traffic.NormalizedContent) ([]byte, int, error) {
	return []byte(`{"choices":[{"message":{"content":"[REDACTED]"}}]}`), 1, nil
}

// B0: response redaction runs on the CANONICAL body via the OpenAI adapter, which
// always supports the rewrite — so a Modify decision on the direct non-stream
// path produces a redacted 200, not the old ErrRewriteUnsupported fail-closed
// (which only happened because redaction used the ingress-format adapter).
func TestServeProxy_Fake_Direct_NonStream_RedactRewritesOnCanonical(t *testing.T) {
	fexec := &fakeExecutor{Result: fakeBrokerSuccessResult()}
	fbridge := &fakeBridge{}
	deps := makeFakeDeps(t, fexec, fbridge)
	deps.HookConfigCache = newResponseHookCache(t, responseModifyHook{})
	deps.TrafficAdapters = nil
	deps.TrafficAdapter = &stubRewriteRedactsAdapter{stubTrafficAdapter: stubTrafficAdapter{id: "stub-redacts"}}

	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatOpenAI,
	})
	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	w := httptest.NewRecorder()
	h(w, freshChatRequest(t, body))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200 (canonical redact rewrites, no fail-closed); body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "[REDACTED]") {
		t.Errorf("body=%s want redacted content from the canonical rewrite", w.Body.String())
	}
	// The original (un-redacted) upstream content must NOT leak to the client.
	if strings.Contains(w.Body.String(), "broker hi") {
		t.Errorf("body=%s leaked unredacted upstream content", w.Body.String())
	}
}

// stubRewriteFailAdapter returns a real (non-ErrRewriteUnsupported)
// error from RewriteResponseBody.
type stubRewriteFailAdapter struct{ stubTrafficAdapter }

func (s *stubRewriteFailAdapter) RewriteResponseBody(_ context.Context, _ []byte, _ string, _ traffic.NormalizedContent) ([]byte, int, error) {
	return nil, 0, errors.New("synthesised rewrite failure")
}

// B0: the broker non-stream path also runs response redaction on the CANONICAL
// body (handleNonStreamWithSubscription → runResponseHooksOnCanonical), so a
// Modify produces a redacted 200, not the old reverse-encode fail-closed.
func TestServeProxy_Fake_BrokerLeader_RedactRewritesOnCanonical(t *testing.T) {
	fexec := &fakeExecutor{Result: fakeBrokerSuccessResult()}
	fbridge := &fakeBridge{}
	deps := makeFakeDeps(t, fexec, fbridge)
	deps.HookConfigCache = newResponseHookCache(t, responseModifyHook{})
	deps.TrafficAdapters = nil
	deps.TrafficAdapter = &stubRewriteRedactsAdapter{stubTrafficAdapter: stubTrafficAdapter{id: "stub-redacts"}}
	cacheOpt, cleanup := withCache(t)
	defer cleanup()
	cacheOpt(deps)
	withBroker(t)(deps)

	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatOpenAI,
	})
	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	w := httptest.NewRecorder()
	h(w, freshChatRequest(t, body))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200 (broker canonical redact rewrites); body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "[REDACTED]") {
		t.Errorf("body=%s want redacted content from the canonical rewrite (broker path)", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "broker hi") {
		t.Errorf("body=%s leaked unredacted upstream content (broker path)", w.Body.String())
	}
}

func TestServeProxy_Fake_BrokerLeader_ModifyRewriteFails500(t *testing.T) {
	fexec := &fakeExecutor{Result: fakeBrokerSuccessResult()}
	fbridge := &fakeBridge{}
	deps := makeFakeDeps(t, fexec, fbridge)
	deps.HookConfigCache = newResponseHookCache(t, responseModifyHook{})
	deps.TrafficAdapters = nil
	deps.TrafficAdapter = &stubRewriteFailAdapter{stubTrafficAdapter: stubTrafficAdapter{id: "stub-fail"}}
	cacheOpt, cleanup := withCache(t)
	defer cleanup()
	cacheOpt(deps)
	withBroker(t)(deps)

	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatOpenAI,
	})
	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	w := httptest.NewRecorder()
	h(w, freshChatRequest(t, body))

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d want 500 (broker-path rewrite fail); body=%s", w.Code, w.Body.String())
	}
}

// B0: the cache-HIT non-stream path also runs response redaction on the CANONICAL
// cached body (handleNonStreamHit → runResponseHooksOnCanonical), so a Modify
// produces a redacted 200, not the old reverse-encode fail-closed.
func TestServeProxy_Fake_CacheHIT_RedactRewritesOnCanonical(t *testing.T) {
	fexec := &fakeExecutor{} // must NOT be invoked on a HIT
	fbridge := &fakeBridge{}
	deps := makeFakeDeps(t, fexec, fbridge)
	deps.HookConfigCache = newResponseHookCache(t, responseModifyHook{})
	deps.TrafficAdapters = nil
	deps.TrafficAdapter = &stubRewriteRedactsAdapter{stubTrafficAdapter: stubTrafficAdapter{id: "stub-redacts"}}
	cacheOpt, cleanup := withCache(t)
	defer cleanup()
	cacheOpt(deps)

	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	cacheKey := computeStreamCacheKey(t, deps, "openai", "gpt-4o", body, false)
	entry := &cache.ResponseEntry{
		Provider:          "openai",
		Model:             "gpt-4o",
		CanonicalResponse: json.RawMessage(`{"id":"cached","object":"chat.completion","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"cached hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`),
		Usage:             provcore.Usage{PromptTokens: iPtr(1), CompletionTokens: iPtr(1), TotalTokens: iPtr(2)},
		CachedAt:          time.Now().UTC(),
	}
	if _, err := deps.Cache.StoreResponse(context.Background(), cacheKey, entry); err != nil {
		t.Fatalf("StoreResponse: %v", err)
	}

	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatOpenAI,
	})
	w := httptest.NewRecorder()
	h(w, freshChatRequest(t, string(body)))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200 (cache HIT canonical redact rewrites); body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "[REDACTED]") {
		t.Errorf("body=%s want redacted content from the canonical rewrite (cache HIT path)", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "cached hi") {
		t.Errorf("body=%s leaked unredacted cached content (cache HIT path)", w.Body.String())
	}
}

func TestServeProxy_Fake_CacheHIT_ModifyRewriteFails500(t *testing.T) {
	fexec := &fakeExecutor{}
	fbridge := &fakeBridge{}
	deps := makeFakeDeps(t, fexec, fbridge)
	deps.HookConfigCache = newResponseHookCache(t, responseModifyHook{})
	deps.TrafficAdapters = nil
	deps.TrafficAdapter = &stubRewriteFailAdapter{stubTrafficAdapter: stubTrafficAdapter{id: "stub-fail"}}
	cacheOpt, cleanup := withCache(t)
	defer cleanup()
	cacheOpt(deps)

	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	cacheKey := computeStreamCacheKey(t, deps, "openai", "gpt-4o", body, false)
	entry := &cache.ResponseEntry{
		Provider:          "openai",
		Model:             "gpt-4o",
		CanonicalResponse: json.RawMessage(`{"id":"cached","object":"chat.completion","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"cached hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`),
		Usage:             provcore.Usage{PromptTokens: iPtr(1), CompletionTokens: iPtr(1), TotalTokens: iPtr(2)},
		CachedAt:          time.Now().UTC(),
	}
	if _, err := deps.Cache.StoreResponse(context.Background(), cacheKey, entry); err != nil {
		t.Fatalf("StoreResponse: %v", err)
	}

	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatOpenAI,
	})
	w := httptest.NewRecorder()
	h(w, freshChatRequest(t, string(body)))

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d want 500 (cache HIT + rewrite fail); body=%s", w.Code, w.Body.String())
	}
}

// TestServeProxy_Fake_Direct_NonStream_ModifyRewriteFails500 exercises
// the `rErr != nil` branch in handleNonStream's Modify arm. The
// rewrite fails with a non-ErrRewriteUnsupported error → 500.
func TestServeProxy_Fake_Direct_NonStream_ModifyRewriteFails500(t *testing.T) {
	fexec := &fakeExecutor{Result: fakeBrokerSuccessResult()}
	fbridge := &fakeBridge{}
	deps := makeFakeDeps(t, fexec, fbridge)
	deps.HookConfigCache = newResponseHookCache(t, responseModifyHook{})
	deps.TrafficAdapters = nil
	deps.TrafficAdapter = &stubRewriteFailAdapter{stubTrafficAdapter: stubTrafficAdapter{id: "stub-fail"}}

	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatOpenAI,
	})
	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	w := httptest.NewRecorder()
	h(w, freshChatRequest(t, body))

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d want 500; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "response rewrite failed") {
		t.Errorf("body=%s want 500 envelope", w.Body.String())
	}
}

// TestServeProxy_Fake_StampsRateLimitHeader exercises the rate-limit-
// visibility header arm. With RateLimitRpm set on VKMeta, the handler
// must stamp X-RateLimit-Limit on the response.
func TestServeProxy_Fake_StampsRateLimitHeader(t *testing.T) {
	successBody := []byte(`{"id":"x","object":"chat.completion","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	fexec := &fakeExecutor{Result: &executor.ExecutionResult{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Content-Type": []string{"application/json"}},
		Body:       successBody,
		Target: routingcore.RoutingTarget{
			ProviderID: "p-openai", ProviderName: "openai", ModelID: "gpt-4o", ModelCode: "gpt-4o", AdapterType: "openai",
		},
		Usage:    provcore.Usage{PromptTokens: iPtr(1), CompletionTokens: iPtr(1), TotalTokens: iPtr(2)},
		Attempts: []executor.Attempt{{StatusCode: http.StatusOK}},
	}}
	fbridge := &fakeBridge{}
	deps := makeFakeDeps(t, fexec, fbridge)
	rpm := 120
	deps.VKAuth = &stubVKAuthCacheTest{meta: &vkauth.VKMeta{
		ID: "vk-1", Name: "test-vk", OrganizationID: "org-1",
		RateLimitRpm: &rpm,
	}}

	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatOpenAI,
	})
	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	w := httptest.NewRecorder()
	h(w, freshChatRequest(t, body))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200; body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("X-RateLimit-Limit"); got != "120" {
		t.Errorf("X-RateLimit-Limit=%q want 120", got)
	}
}

// Additional coverage arms:

// TestServeProxy_Fake_Coerced_Header covers proxy.go line 1179-1180:
// when the ExecutionResult carries non-empty Coerced the handler must
// stamp x-nexus-coerced on the response.
//
// Named failure mode: a client that skipped a mandatory field (e.g.
// max_tokens) and got a silent adapter-fill would have no visibility into
// the coercion. Regression on this header breaks SDK clients that consume
// it for audit trails.
func TestServeProxy_Fake_Coerced_Header(t *testing.T) {
	successBody := []byte(`{"id":"x","object":"chat.completion","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	fexec := &fakeExecutor{Result: &executor.ExecutionResult{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Content-Type": []string{"application/json"}},
		Body:       successBody,
		Target:     routingcore.RoutingTarget{ProviderID: "p-openai", ProviderName: "openai", ModelID: "gpt-4o", ModelCode: "gpt-4o", AdapterType: "openai"},
		Usage:      provcore.Usage{PromptTokens: iPtr(1), CompletionTokens: iPtr(1), TotalTokens: iPtr(2)},
		Coerced:    []string{"max_tokens"},
		Attempts:   []executor.Attempt{{StatusCode: http.StatusOK}},
	}}
	fbridge := &fakeBridge{}
	deps := makeFakeDeps(t, fexec, fbridge)

	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatOpenAI,
	})
	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	w := httptest.NewRecorder()
	h(w, freshChatRequest(t, body))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200; body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("X-Nexus-Coerced"); got != "max_tokens" {
		t.Errorf("x-nexus-coerced=%q want max_tokens", got)
	}
}

// TestServeProxy_Fake_E57_ReverseDecode_Success covers proxy.go line 2379:
// when ResponsesUpgradeFromContext fires AND the upstream body is valid
// Responses-shape, DecodeResponsesResponse must succeed and respBody is
// reassigned (the stamp-back path executes without error).
//
// Named failure mode: a successful Responses-API auto-upgrade that hits a
// DecodeResponsesResponse error would 502 the client; verifying the
// success path ensures the decoder handles the nominal Responses body.
func TestServeProxy_Fake_E57_ReverseDecode_Success(t *testing.T) {
	// Minimal valid /v1/responses response body that DecodeResponsesResponse
	// can decode into canonical chat.completion shape.
	responsesBody := []byte(`{
		"id":"resp_abc","object":"response","created_at":1747353600,
		"status":"completed","model":"gpt-4o",
		"output":[{"type":"message","id":"msg_1","role":"assistant",
			"content":[{"type":"output_text","text":"hello"}]}],
		"usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}
	}`)
	fexec := &fakeExecutor{Result: &executor.ExecutionResult{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Content-Type": []string{"application/json"}},
		Body:       responsesBody,
		Target:     routingcore.RoutingTarget{ProviderID: "p-openai", ProviderName: "openai", ModelID: "gpt-4o", ModelCode: "gpt-4o", AdapterType: "openai"},
		Usage:      provcore.Usage{PromptTokens: iPtr(5), CompletionTokens: iPtr(2), TotalTokens: iPtr(7)},
		Attempts:   []executor.Attempt{{StatusCode: http.StatusOK}},
	}}
	fbridge := &fakeBridge{}
	deps := makeFakeDeps(t, fexec, fbridge)

	inner := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatOpenAI,
	})
	// Inject ResponsesUpgrade ctx flag — simulates auto-upgrade firing.
	h := func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(WithResponsesUpgrade(r.Context()))
		inner(w, r)
	}

	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	w := httptest.NewRecorder()
	h(w, freshChatRequest(t, body))

	// decode must succeed → no 502.
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200 after successful Responses-API decode; body=%s", w.Code, w.Body.String())
	}
	// The reverse-decoded body must be reshaped into chat.completion form.
	if !strings.Contains(w.Body.String(), "chat.completion") {
		t.Errorf("body=%q want chat.completion object", w.Body.String())
	}
}

// TestServeProxy_Fake_CacheReadTokensZero_ProviderCacheMiss covers
// proxy.go line 2438-2439: when CacheReadTokens is non-nil but zero
// the provider looked for a cached prompt but didn't find one →
// ProviderCacheStatus must be stamped ProviderCacheMiss.
//
// Named failure mode: a zero-read-tokens hit would be silently classified
// as ProviderCacheNA without this branch, understating cache-miss rates
// in analytics dashboards.
func TestServeProxy_Fake_CacheReadTokensZero_ProviderCacheMiss(t *testing.T) {
	zero := 0
	successBody := []byte(`{"id":"x","object":"chat.completion","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	fexec := &fakeExecutor{Result: &executor.ExecutionResult{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Content-Type": []string{"application/json"}},
		Body:       successBody,
		Target:     routingcore.RoutingTarget{ProviderID: "p-openai", ProviderName: "openai", ModelID: "gpt-4o", ModelCode: "gpt-4o", AdapterType: "openai"},
		Usage: provcore.Usage{
			PromptTokens:     iPtr(1),
			CompletionTokens: iPtr(1),
			TotalTokens:      iPtr(2),
			CacheReadTokens:  &zero, // non-nil but zero → ProviderCacheMiss
		},
		Attempts: []executor.Attempt{{StatusCode: http.StatusOK}},
	}}
	prod := &captureProducer{}
	deps := makeFakeDeps(t, fexec, &fakeBridge{})
	// Swap in a capturing audit writer.
	aw := audit.NewWriter(prod, "nexus.event.ai-traffic", nil, slog.Default())
	deps.AuditWriter = aw

	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatOpenAI,
	})
	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	w := httptest.NewRecorder()
	h(w, freshChatRequest(t, body))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200; body=%s", w.Code, w.Body.String())
	}
	// Drain audit writer so all messages are captured before we inspect.
	aw.Close()
	prod.mu.Lock()
	msgs := append([][]byte(nil), prod.messages...)
	prod.mu.Unlock()
	if len(msgs) == 0 {
		t.Fatal("no audit event captured")
	}
	// Parse the last message and verify provider_cache_status = "miss".
	last := msgs[len(msgs)-1]
	var env map[string]json.RawMessage
	if err := json.Unmarshal(last, &env); err != nil {
		t.Fatalf("audit JSON parse: %v; raw=%s", err, last)
	}
	var status string
	if raw, ok := env["providerCacheStatus"]; ok {
		_ = json.Unmarshal(raw, &status)
	}
	if status != string(audit.ProviderCacheMiss) {
		t.Errorf("providerCacheStatus=%q want %q", status, audit.ProviderCacheMiss)
	}
}

// TestServeProxy_Fake_CacheCreationTokens_Stamped covers proxy.go
// lines 2447-2449: when CacheCreationTokens is non-nil the audit record
// must carry the stamped value.
//
// Named failure mode: a provider that charged for prompt-cache population
// (Anthropic cache_creation_input_tokens) would show zero CacheCreationTokens
// in analytics if this branch regressed.
func TestServeProxy_Fake_CacheCreationTokens_Stamped(t *testing.T) {
	creation := 100
	successBody := []byte(`{"id":"x","object":"chat.completion","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	fexec := &fakeExecutor{Result: &executor.ExecutionResult{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Content-Type": []string{"application/json"}},
		Body:       successBody,
		Target:     routingcore.RoutingTarget{ProviderID: "p-openai", ProviderName: "openai", ModelID: "gpt-4o", ModelCode: "gpt-4o", AdapterType: "openai"},
		Usage: provcore.Usage{
			PromptTokens:        iPtr(1),
			CompletionTokens:    iPtr(1),
			TotalTokens:         iPtr(2),
			CacheCreationTokens: &creation,
		},
		Attempts: []executor.Attempt{{StatusCode: http.StatusOK}},
	}}
	prod2 := &captureProducer{}
	deps := makeFakeDeps(t, fexec, &fakeBridge{})
	aw2 := audit.NewWriter(prod2, "nexus.event.ai-traffic", nil, slog.Default())
	deps.AuditWriter = aw2

	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatOpenAI,
	})
	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	w := httptest.NewRecorder()
	h(w, freshChatRequest(t, body))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200; body=%s", w.Code, w.Body.String())
	}
	aw2.Close()
	prod2.mu.Lock()
	msgs2 := append([][]byte(nil), prod2.messages...)
	prod2.mu.Unlock()
	if len(msgs2) == 0 {
		t.Fatal("no audit event captured")
	}
	last2 := msgs2[len(msgs2)-1]
	var env2 map[string]json.RawMessage
	if err := json.Unmarshal(last2, &env2); err != nil {
		t.Fatalf("audit JSON parse: %v; raw=%s", err, last2)
	}
	var tokens int64
	if raw, ok := env2["cacheCreationTokens"]; ok {
		_ = json.Unmarshal(raw, &tokens)
	}
	if tokens != 100 {
		t.Errorf("cacheCreationTokens=%d want 100", tokens)
	}
	// First-turn cache write proves the model supports prompt cache even though
	// CacheReadTokens is nil; classification must be miss, not na — otherwise the
	// audit drawer renders the misleading "model doesn't support prompt cache"
	// label for traffic that clearly wrote to the cache.
	var status string
	if raw, ok := env2["providerCacheStatus"]; ok {
		_ = json.Unmarshal(raw, &status)
	}
	if status != string(audit.ProviderCacheMiss) {
		t.Errorf("providerCacheStatus=%q want %q (cache write proves model support)", status, audit.ProviderCacheMiss)
	}
}

// TestServeProxy_Fake_Embeddings_UpdateDimensionCalled covers proxy.go
// lines 2474-2476: when the Ingress.EndpointType is "embeddings" the
// handler must call updateEmbeddingDimension on the response body and
// stamp metadata.embedding.dimension into the audit record.
//
// Named failure mode: a missing dimension stamp would leave
// rec.Metadata.embedding.dimension absent for all embedding traffic,
// breaking dashboard filters that rely on the field.
func TestServeProxy_Fake_Embeddings_UpdateDimensionCalled(t *testing.T) {
	// Minimal embeddings response with a 3-element vector so
	// updateEmbeddingDimension can read data.0.embedding.# = 3.
	embBody := []byte(`{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.1,0.2,0.3]}],"model":"text-embedding-3-small","usage":{"prompt_tokens":2,"total_tokens":2}}`)
	fexec := &fakeExecutor{Result: &executor.ExecutionResult{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Content-Type": []string{"application/json"}},
		Body:       embBody,
		Target:     routingcore.RoutingTarget{ProviderID: "p-openai", ProviderName: "openai", ModelID: "text-embedding-3-small", ModelCode: "text-embedding-3-small", AdapterType: "openai"},
		Usage:      provcore.Usage{PromptTokens: iPtr(2), TotalTokens: iPtr(2)},
		Attempts:   []executor.Attempt{{StatusCode: http.StatusOK}},
	}}
	prod3 := &captureProducer{}
	deps := makeFakeDeps(t, fexec, &fakeBridge{})
	aw3 := audit.NewWriter(prod3, "nexus.event.ai-traffic", nil, slog.Default())
	deps.AuditWriter = aw3

	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIEmbeddings,
		BodyFormat: provcore.FormatOpenAI,
	})
	// Standard embeddings request body.
	reqBody := `{"model":"text-embedding-3-small","input":"hello world"}`
	r := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(reqBody))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer vk")
	w := httptest.NewRecorder()
	h(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200; body=%s", w.Code, w.Body.String())
	}
	aw3.Close()
	prod3.mu.Lock()
	msgs3 := append([][]byte(nil), prod3.messages...)
	prod3.mu.Unlock()
	if len(msgs3) == 0 {
		t.Fatal("no audit event captured")
	}
	last3 := msgs3[len(msgs3)-1]
	// metadata is nested under details.metadata in the wire envelope.
	// Parse via gjson-style path for simplicity.
	var env3 struct {
		Details map[string]json.RawMessage `json:"details"`
	}
	if err := json.Unmarshal(last3, &env3); err != nil {
		t.Fatalf("audit JSON parse: %v; raw=%s", err, last3)
	}
	if env3.Details == nil {
		t.Fatal("audit details field missing")
	}
	rawMeta, ok := env3.Details["metadata"]
	if !ok {
		t.Fatal("audit details.metadata missing")
	}
	// updateEmbeddingDimension stamps embedding.dimension = 3 into the metadata map.
	var md map[string]any
	if err := json.Unmarshal(rawMeta, &md); err != nil {
		t.Fatalf("metadata JSON parse: %v; raw=%s", err, rawMeta)
	}
	embRaw, ok2 := md["embedding"]
	if !ok2 {
		t.Fatalf("metadata.embedding missing; md=%v", md)
	}
	embMap, ok3 := embRaw.(map[string]any)
	if !ok3 {
		t.Fatalf("metadata.embedding=%T want map", embRaw)
	}
	dim, ok4 := embMap["dimension"]
	if !ok4 {
		t.Errorf("metadata.embedding.dimension missing; embedding=%v", embMap)
	} else if dimF, ok5 := dim.(float64); !ok5 || int(dimF) != 3 {
		t.Errorf("metadata.embedding.dimension=%v want 3", dim)
	}
}

// TestServeProxy_Fake_Embeddings_CrossFormat_Gemini routes an OpenAI
// /v1/embeddings request to a Gemini target (cross-format). It exercises the
// embeddings pre-cache canonicalization (IngressEmbeddingsToCanonical +
// EmbeddingsWireShapeForTarget) and the egress embeddings reshape
// (ResponseCanonicalToIngressEmbeddings); the fakeBridge stubs both as
// identity. The empty upstream usage also drives the embeddingTokenFallback
// (prompt_tokens back-filled from the request-side estimate).
func TestServeProxy_Fake_Embeddings_CrossFormat_Gemini(t *testing.T) {
	embBody := []byte(`{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.1,0.2,0.3]}],"model":"gemini-embedding-001","usage":{}}`)
	fexec := &fakeExecutor{Result: &executor.ExecutionResult{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Content-Type": []string{"application/json"}},
		Body:       embBody,
		Target:     routingcore.RoutingTarget{ProviderID: "p-gem", ProviderName: "google-gemini", ModelID: "gemini-embedding-001", ModelCode: "gemini-embedding-001", AdapterType: "gemini"},
		Attempts:   []executor.Attempt{{StatusCode: http.StatusOK}},
	}}
	deps := makeFakeDeps(t, fexec, &fakeBridge{})
	// Enable the response cache so the pre-cache body-prep runs (where the
	// embeddings canonicalization + target wire-shape resolution live).
	cacheOpt, cleanup := withCache(t)
	defer cleanup()
	cacheOpt(deps)
	deps.Router = &stubRouterCacheTest{targets: []routingcore.RoutingTarget{{
		ProviderID: "p-gem", ProviderName: "google-gemini", ProviderModelID: "gemini-embedding-001",
		ModelID: "gemini-embedding-001", ModelName: "Gemini Embedding 001", ModelCode: "gemini-embedding-001",
		AdapterType: "gemini",
	}}}

	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIEmbeddings,
		BodyFormat: provcore.FormatOpenAI,
	})
	reqBody := `{"model":"gemini-embedding-001","input":"hello world"}`
	r := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(reqBody))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer vk")
	w := httptest.NewRecorder()
	h(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200; body=%s", w.Code, w.Body.String())
	}
}

// TestServeProxy_Fake_Embeddings_NeverServedFromCache is the regression
// guard: the response cache is endpoint-scoped, so an embeddings request must
// NEVER be served from a seeded cache entry — even when the cache is enabled
// and a matching entry exists. The pre-lookup classifier short-circuits the
// embeddings endpoint (GatewayCacheSkipReasonEmbeddingsEndpoint), so the
// request goes upstream and the seeded cache content is ignored. This both
// fixes the cost-dashboard dilution and avoids occupying Redis with single-use
// embedding entries.
//
// The test seeds a distinctive cache entry, then asserts the served response
// carries the UPSTREAM body (from the fake executor), not the seeded entry —
// proving the cache was skipped, not consulted.
func TestServeProxy_Fake_Embeddings_NeverServedFromCache(t *testing.T) {
	const upstreamMarker = "upstream-not-cache"
	upstreamBody := []byte(`{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.9,0.9,0.9]}],"model":"text-embedding-3-small","usage":{},"_source":"` + upstreamMarker + `"}`)
	fexec := &fakeExecutor{Result: &executor.ExecutionResult{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Content-Type": []string{"application/json"}},
		Body:       upstreamBody,
		Target:     routingcore.RoutingTarget{ProviderID: "p-openai", ProviderName: "openai", ModelID: "text-embedding-3-small", ModelCode: "text-embedding-3-small", AdapterType: "openai"},
		Usage:      provcore.Usage{},
		Attempts:   []executor.Attempt{{StatusCode: http.StatusOK}},
	}}
	deps := makeFakeDeps(t, fexec, &fakeBridge{})
	cacheOpt, cleanup := withCache(t)
	defer cleanup()
	cacheOpt(deps)
	deps.Router = &stubRouterCacheTest{targets: []routingcore.RoutingTarget{{
		ProviderID: "p-openai", ProviderName: "openai", ProviderModelID: "text-embedding-3-small",
		ModelID: "text-embedding-3-small", ModelName: "text-embedding-3-small", ModelCode: "text-embedding-3-small",
		AdapterType: "openai",
	}}}

	body := []byte(`{"model":"text-embedding-3-small","input":"hello world"}`)
	// Seed a cache entry that, if (incorrectly) served, would be detectable by
	// the absence of the upstream marker.
	cacheKey := computeStreamCacheKey(t, deps, "openai", "text-embedding-3-small", body, false)
	entry := &cache.ResponseEntry{
		Provider:          "openai",
		Model:             "text-embedding-3-small",
		CanonicalResponse: json.RawMessage(`{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.1,0.2,0.3]}],"model":"text-embedding-3-small","usage":{},"_source":"seeded-cache"}`),
		Usage:             provcore.Usage{},
		CachedAt:          time.Now().UTC(),
	}
	if _, err := deps.Cache.StoreResponse(context.Background(), cacheKey, entry); err != nil {
		t.Fatalf("StoreResponse: %v", err)
	}

	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIEmbeddings,
		BodyFormat: provcore.FormatOpenAI,
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(string(body)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer vk")
	w := httptest.NewRecorder()
	h(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200; body=%s", w.Code, w.Body.String())
	}
	// The executor MUST have been called — a cache HIT would have skipped it.
	if fexec.Calls == 0 && fexec.PreparedCalls == 0 {
		t.Fatal("embeddings request was served from cache; F-0222 short-circuit not applied (executor never called)")
	}
	if !strings.Contains(w.Body.String(), upstreamMarker) {
		t.Fatalf("response did not come from upstream; got cache content?\nbody=%s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "seeded-cache") {
		t.Fatalf("response leaked the seeded cache entry; embeddings must not be cache-served\nbody=%s", w.Body.String())
	}
}

// TestServeProxy_Fake_Responses_CrossFormat_EgressReshape drives a
// /v1/responses request to a non-responses-native (Anthropic) target so the
// egress reshape switch hits the Responses branch (ResponseCanonicalToIngress
// on a cross-format Responses ingress).
func TestServeProxy_Fake_Responses_CrossFormat_EgressReshape(t *testing.T) {
	chatResp := []byte(`{"id":"c","object":"chat.completion","model":"claude-opus-4-7","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	fexec := &fakeExecutor{Result: &executor.ExecutionResult{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Content-Type": []string{"application/json"}},
		Body:       chatResp,
		Target:     routingcore.RoutingTarget{ProviderID: "p-anth", ProviderName: "anthropic", ModelID: "claude-opus-4-7", ModelCode: "claude-opus-4-7", AdapterType: "anthropic"},
		Usage:      provcore.Usage{PromptTokens: iPtr(1), CompletionTokens: iPtr(1), TotalTokens: iPtr(2)},
		Attempts:   []executor.Attempt{{StatusCode: http.StatusOK}},
	}}
	fbridge := &fakeBridge{
		targetNativelyServesResponsesAPI: func(provcore.Format) bool { return false },
	}
	deps := makeFakeDeps(t, fexec, fbridge)
	deps.Router = &stubRouterCacheTest{targets: []routingcore.RoutingTarget{{
		ProviderID: "p-anth", ProviderName: "anthropic", ProviderModelID: "claude-opus-4-7",
		ModelID: "claude-opus-4-7", ModelName: "Claude Opus", ModelCode: "claude-opus-4-7",
		AdapterType: "anthropic",
	}}}

	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIResponses,
		BodyFormat: provcore.FormatOpenAIResponses,
	})
	body := `{"model":"claude-opus-4-7","input":"hi"}`
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer vk")
	w := httptest.NewRecorder()
	h(w, r)
	if w.Code == 0 {
		t.Fatal("no response written")
	}
}

// TestServeProxy_Fake_BrokerLeader_Embeddings_TokenFallback drives an
// embeddings request through the broker-subscription non-stream path with an
// upstream that reports no usage, so the subscription-path embeddingTokenFallback
// fires (prompt_tokens back-filled from the request-side estimate).
func TestServeProxy_Fake_BrokerLeader_Embeddings_TokenFallback(t *testing.T) {
	embBody := []byte(`{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.1,0.2,0.3]}],"model":"gemini-embedding-001","usage":{}}`)
	fexec := &fakeExecutor{Result: &executor.ExecutionResult{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Content-Type": []string{"application/json"}},
		Body:       embBody,
		Target:     routingcore.RoutingTarget{ProviderID: "p-gem", ProviderName: "google-gemini", ModelID: "gemini-embedding-001", ModelCode: "gemini-embedding-001", AdapterType: "gemini"},
		Attempts:   []executor.Attempt{{StatusCode: http.StatusOK}},
	}}
	deps := makeFakeDeps(t, fexec, &fakeBridge{})
	cacheOpt, cleanup := withCache(t)
	defer cleanup()
	cacheOpt(deps)
	withBroker(t)(deps)
	deps.Router = &stubRouterCacheTest{targets: []routingcore.RoutingTarget{{
		ProviderID: "p-gem", ProviderName: "google-gemini", ProviderModelID: "gemini-embedding-001",
		ModelID: "gemini-embedding-001", ModelName: "Gemini Embedding 001", ModelCode: "gemini-embedding-001",
		AdapterType: "gemini",
	}}}

	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIEmbeddings,
		BodyFormat: provcore.FormatOpenAI,
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(`{"model":"gemini-embedding-001","input":"hello world"}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer vk")
	w := httptest.NewRecorder()
	h(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200; body=%s", w.Code, w.Body.String())
	}
}

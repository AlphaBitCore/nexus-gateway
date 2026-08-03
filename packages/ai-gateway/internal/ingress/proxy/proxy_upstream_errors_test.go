package proxy

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/auth/vkauth"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/canonicalbridge"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/executor"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	metricspkg "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/metrics"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/ratelimit"
	provbuiltins "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/builtins"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	provtarget "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/target"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/builtins"
	goHooks "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/payloadcapture"
	compliance "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/pipeline"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// dryingPoolResolver models a provider credential pool that goes dry between
// the first resolve and the retry's re-resolve — the production sequence a
// rate limit sets off: the 429 opens the credential's circuit, and the circuit
// is what makes the re-resolve fail. okCalls is how many resolves succeed
// before the pool reports itself exhausted.
type dryingPoolResolver struct {
	baseURL string
	mu      sync.Mutex
	calls   int
	okCalls int
	// deadProvider never resolves, modelling a target whose pool is empty
	// before anything is tried — the pre-dispatch failure that lands FIRST
	// rather than last.
	deadProvider string
}

func (r *dryingPoolResolver) Resolve(_ context.Context, providerID, modelID string, _ provtarget.ResolveHints) (provcore.CallTarget, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if providerID == r.deadProvider {
		return provcore.CallTarget{}, errAllCredentialsCircuitOpen
	}
	r.calls++
	if r.calls > r.okCalls {
		return provcore.CallTarget{}, errAllCredentialsCircuitOpen
	}
	return provcore.CallTarget{
		ProviderID:      providerID,
		ProviderName:    "openai",
		Format:          provcore.FormatOpenAI,
		BaseURL:         r.baseURL,
		APIKey:          "sk-test",
		CredentialID:    "cred-primary",
		CredentialName:  "primary key",
		ProviderModelID: modelID,
	}, nil
}

// errAllCredentialsCircuitOpen mirrors the resolver's real pool-exhausted
// error. Its text is irrelevant to the assertions; what matters is that the
// resolve fails, because that is what makes the executor append an entry
// carrying no upstream status.
var errAllCredentialsCircuitOpen = &poolError{}

type poolError struct{}

func (*poolError) Error() string { return `all credentials for provider "p-openai" are circuit-open` }

// capturingMetrics records the errors_total labels so a test can assert the
// counter is fed the terminal cause rather than never being fed at all.
type capturingMetrics struct {
	mu     sync.Mutex
	errors [][2]string // {provider, errorType}
}

func (m *capturingMetrics) RecordRequest(_, _, _ string, _ int, _ time.Duration, _ metricspkg.Usage) {
}
func (m *capturingMetrics) RecordHookRequest(_, _, _ string)                       {}
func (m *capturingMetrics) RecordTrafficExtract(_, _, _ string)                    {}
func (m *capturingMetrics) RecordEstimate(_, _, _ string, _ time.Duration)         {}
func (m *capturingMetrics) RecordEstimateCompare(_ string, _ int, _ time.Duration) {}
func (m *capturingMetrics) RecordError(provider, errorType string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.errors = append(m.errors, [2]string{provider, errorType})
}

func (m *capturingMetrics) snapshot() [][2]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([][2]string(nil), m.errors...)
}

// openaiTarget builds one routing target against the harness's fake openai
// provider. providerID is what the resolver keys on.
func openaiTarget(providerID string) routingcore.RoutingTarget {
	return routingcore.RoutingTarget{
		ProviderID:      providerID,
		ProviderName:    "openai",
		ProviderModelID: "gpt-4o",
		ModelID:         "gpt-4o",
		ModelName:       "GPT-4o",
		ModelCode:       "gpt-4o",
		AdapterType:     "openai",
	}
}

// upstreamErrorsHarness stands up ServeProxy over the REAL executor against a
// single routing target, so the executor's own retry/re-resolve sequence runs
// rather than being simulated. A single target is essential: with two, a
// failed re-resolve on the first target is followed by the second target's
// real attempt, so the entry carrying no upstream status is never last — which
// is exactly the shape that let this defect ship green.
type upstreamErrorsHarness struct {
	handler  http.HandlerFunc
	producer *captureProducer
	writer   *audit.Writer
	metrics  *capturingMetrics
	resolver *dryingPoolResolver
}

func newUpstreamErrorsHarness(t *testing.T, upstreamURL string, okResolves int, targets ...routingcore.RoutingTarget) *upstreamErrorsHarness {
	t.Helper()
	if len(targets) == 0 {
		targets = []routingcore.RoutingTarget{openaiTarget("p-openai")}
	}
	logger := slog.Default()

	provReg := provcore.NewRegistry()
	provbuiltins.Register(provReg, nil, logger)
	provReg.Freeze()

	bridge := canonicalbridge.New(provbuiltins.SchemaCodecs(logger))
	execHT := store.NewHealthTracker()
	t.Cleanup(execHT.Stop)

	res := &dryingPoolResolver{baseURL: upstreamURL, okCalls: okResolves}
	exec := executor.New(provReg, res, execHT, bridge)

	hookCache := compliance.NewHookConfigCache(
		func(_ context.Context) ([]goHooks.HookConfig, error) { return nil, nil },
		builtins.Registry, 0, logger,
	)
	if err := hookCache.Start(context.Background()); err != nil {
		t.Fatalf("hookCache.Start: %v", err)
	}

	prod := &captureProducer{}
	auditWriter := audit.NewWriter(prod, "nexus.event.ai-traffic", nil, logger)
	depsHT := store.NewHealthTracker()
	t.Cleanup(depsHT.Stop)
	m := &capturingMetrics{}

	deps := &Deps{
		VKAuth: &stubVKAuthCacheTest{meta: &vkauth.VKMeta{
			ID: "vk-1", Name: "test-vk", OrganizationID: "org-1", OrganizationName: "Org",
		}},
		RateLimiter:     ratelimit.NewLocalOnly(logger),
		Router:          &stubRouterCacheTest{targets: targets},
		Executor:        exec,
		HookConfigCache: hookCache,
		ProviderReg:     provReg,
		HealthTracker:   depsHT,
		AuditWriter:     auditWriter,
		CanonicalBridge: bridge,
		Metrics:         m,
		PayloadCapture: payloadcapture.NewStore(payloadcapture.Config{
			StoreRequestBody:   true,
			StoreResponseBody:  true,
			MaxInlineBodyBytes: 64 * 1024,
		}),
		Logger: logger,
	}

	return &upstreamErrorsHarness{
		handler: NewHandler(deps).ServeProxy(Ingress{
			WireShape:  typology.WireShapeOpenAIChat,
			BodyFormat: provcore.FormatOpenAI,
		}),
		producer: prod,
		writer:   auditWriter,
		metrics:  m,
		resolver: res,
	}
}

// trafficEvent drains the audit writer and returns the single traffic_event
// row the request produced — the persisted shape, not an in-memory struct.
func (h *upstreamErrorsHarness) trafficEvent(t *testing.T) mq.TrafficEventMessage {
	t.Helper()
	h.writer.Close()
	h.producer.mu.Lock()
	msgs := append([][]byte(nil), h.producer.messages...)
	h.producer.mu.Unlock()
	if len(msgs) != 1 {
		t.Fatalf("captured %d audit messages, want exactly 1", len(msgs))
	}
	var evt mq.TrafficEventMessage
	if err := json.Unmarshal(msgs[0], &evt); err != nil {
		t.Fatalf("unmarshal traffic event: %v", err)
	}
	return evt
}

// TestServeProxy_E2E_RateLimitedRetryFindsPoolDry_ClientGets429 drives the
// real executor through the production sequence: the upstream rate-limits us,
// the default retry policy retries, and the retry's re-resolve finds the pool
// dry. The executor then records a target it never called, and that entry is
// the LAST one in the attempt list.
//
// The client must still be told 429. A 502 would say the provider is down —
// false, the provider is fine and we are throttled — and a client told the
// provider is down does not back off, which is the one behaviour a rate limit
// needs from it.
//
// Everything this asserts rides on the same seam, so they are asserted
// together: the status, the credential attribution an operator needs to answer
// "which key got rate-limited?", the counter, and the attempt count.
func TestServeProxy_E2E_RateLimitedRetryFindsPoolDry_ClientGets429(t *testing.T) {
	var upstreamCalls int32
	var mu sync.Mutex
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		upstreamCalls++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"Rate limit reached","type":"rate_limit_error","code":"rate_limit_exceeded"}}`))
	}))
	defer upstream.Close()

	// One successful resolve: the first attempt gets a credential, the retry's
	// re-resolve finds the pool dry.
	h := newUpstreamErrorsHarness(t, upstream.URL, 1)

	w := httptest.NewRecorder()
	h.handler(w, freshChatRequest(t, `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("client status = %d, want 429: a rate limit reported as %d tells the client the provider is down and suppresses its backoff; body = %s",
			w.Code, w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "PROVIDER_RATE_LIMITED") {
		t.Errorf("error envelope = %s, want PROVIDER_RATE_LIMITED", w.Body.String())
	}

	// The scenario is only meaningful if the re-resolve really failed after a
	// real upstream 429 — otherwise the test would pass against the defect.
	mu.Lock()
	calls := upstreamCalls
	mu.Unlock()
	if calls != 1 {
		t.Fatalf("upstream called %d times, want 1 (the retry must have been stopped by the dry pool, not by the upstream)", calls)
	}
	if h.resolver.calls != 2 {
		t.Fatalf("resolver called %d times, want 2 (initial + the retry that found the pool dry)", h.resolver.calls)
	}

	evt := h.trafficEvent(t)
	if evt.CredentialID != "cred-primary" {
		t.Errorf("traffic_event.credential_id = %q, want %q: the failure must stay attributed to the credential that was rate-limited, not to the resolve that found no credential at all",
			evt.CredentialID, "cred-primary")
	}
	if got := h.metrics.snapshot(); len(got) != 1 || got[0] != [2]string{"openai", provcore.CodeRateLimited} {
		t.Errorf("errors_total = %v, want one {openai, %s}", got, provcore.CodeRateLimited)
	}
}

// TestServeProxy_E2E_TerminalAuthFailure_ErrorCodeCarriesTheCause pins that a
// terminal upstream 4xx persists WHY it failed. An operator filtering
// traffic_event needs to tell a rejected credential from a malformed request
// from an oversized prompt; a single blanket code answers none of those.
func TestServeProxy_E2E_TerminalAuthFailure_ErrorCodeCarriesTheCause(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"Incorrect API key provided","type":"invalid_request_error","code":"invalid_api_key"}}`))
	}))
	defer upstream.Close()

	h := newUpstreamErrorsHarness(t, upstream.URL, 1)

	w := httptest.NewRecorder()
	h.handler(w, freshChatRequest(t, `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("client status = %d, want 401; body = %s", w.Code, w.Body.String())
	}

	evt := h.trafficEvent(t)
	if evt.ErrorCode == nil {
		t.Fatalf("traffic_event.error_code is NULL, want %q", provcore.CodeAuthFailed)
	}
	if *evt.ErrorCode != provcore.CodeAuthFailed {
		t.Errorf("traffic_event.error_code = %q, want %q: a rejected credential must be distinguishable from every other terminal 4xx",
			*evt.ErrorCode, provcore.CodeAuthFailed)
	}
	if got := h.metrics.snapshot(); len(got) != 1 || got[0] != [2]string{"openai", provcore.CodeAuthFailed} {
		t.Errorf("errors_total = %v, want one {openai, %s}", got, provcore.CodeAuthFailed)
	}
}

// TestUpstreamFailureMessage_SplitsByCauseAndCarriesNoRequestData pins the two
// properties the operator errors page depends on. That page groups issues by a
// hash of the message text alone, so:
//
//   - two different causes must produce two different messages, or every
//     failure lands in one row and silencing one cause silences the others;
//   - the same cause must produce the same message whatever the request was,
//     or each provider/model combination mints its own single-event group and
//     buries the page.
func TestUpstreamFailureMessage_SplitsByCauseAndCarriesNoRequestData(t *testing.T) {
	target := func(provider, model string) routingcore.RoutingTarget {
		return routingcore.RoutingTarget{ProviderName: provider, ModelCode: model}
	}

	causes := []executor.Attempt{
		{Dispatched: true, Code: provcore.CodeRateLimited},
		{Dispatched: true, Code: provcore.CodeTimeout},
		{Dispatched: true, Code: provcore.CodeUpstreamError},
		{Dispatched: true, Code: provcore.CodeContextOverflow},
		{Dispatched: true, Code: provcore.CodeInvalidRequest},
		{Dispatched: true, Code: provcore.CodeAuthFailed},
		{Dispatched: true, Code: provcore.CodeEndpointUnsupported},
		{Dispatched: true, Code: provcore.CodeNotImplemented},
		{Dispatched: true, Code: provcore.CodeNoCompatibleProvider},
		{Dispatched: true, Code: ""},  // transport failure, no provider envelope
		{Dispatched: false, Code: ""}, // abandoned before dispatch
	}
	seen := map[string]string{}
	for _, a := range causes {
		msg := upstreamFailureMessage(a)
		label := a.Code
		if !a.Dispatched {
			label = "not-dispatched"
		}
		if prev, dup := seen[msg]; dup {
			t.Errorf("causes %q and %q share the message %q — they would collapse into one issue row and could not be silenced apart",
				prev, label, msg)
		}
		seen[msg] = label
	}

	// The same cause across different providers and models must group as one.
	a := executor.Attempt{Dispatched: true, Code: provcore.CodeAuthFailed, Target: target("openai", "gpt-4o")}
	b := executor.Attempt{Dispatched: true, Code: provcore.CodeAuthFailed, Target: target("anthropic", "claude-opus-4-8")}
	if upstreamFailureMessage(a) != upstreamFailureMessage(b) {
		t.Errorf("one cause produced two messages across providers (%q vs %q) — each provider/model pair would mint its own issue group",
			upstreamFailureMessage(a), upstreamFailureMessage(b))
	}
	for _, msg := range []string{upstreamFailureMessage(a), upstreamFailureMessage(b)} {
		for _, leaked := range []string{"openai", "anthropic", "gpt-4o", "claude-opus-4-8"} {
			if strings.Contains(msg, leaked) {
				t.Errorf("message %q interpolates the per-request value %q; it must ride in an attribute, which does not affect the group hash", msg, leaked)
			}
		}
	}
}

// TestServeProxy_E2E_FirstTargetNeverDispatched_AttemptCountExcludesIt pins
// that the attempt count the client is handed counts calls, not entries. The
// first target's pool is empty, so the gateway abandons it without a call and
// the second target serves the request on its first try.
//
// The client must be told 1. Telling it 2 claims an upstream call that never
// left the process, which is the number an operator reads when deciding
// whether a provider is flapping.
func TestServeProxy_E2E_FirstTargetNeverDispatched_AttemptCountExcludesIt(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","model":"gpt-4o",` +
			`"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],` +
			`"usage":{"prompt_tokens":5,"completion_tokens":4,"total_tokens":9}}`))
	}))
	defer upstream.Close()

	h := newUpstreamErrorsHarness(t, upstream.URL, 8,
		openaiTarget("p-dry"), openaiTarget("p-healthy"))
	h.resolver.deadProvider = "p-dry"

	w := httptest.NewRecorder()
	h.handler(w, freshChatRequest(t, `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (the second target must serve it); body = %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("X-Nexus-Attempts"); got != "1" {
		t.Errorf("X-Nexus-Attempts = %q, want \"1\": only one call reached a provider — the abandoned target never made one", got)
	}
}

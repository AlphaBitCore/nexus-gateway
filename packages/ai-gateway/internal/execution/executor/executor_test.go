package executor

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	provtarget "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/target"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	configtypes "github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configtypes/policy"
	nexushttp "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/http"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// scripted is the per-attempt outcome the mock adapter replays.
type scripted struct {
	resp *provcore.Response
	err  error
	// delay makes an attempt take measurable wall-clock, so a test can
	// exercise the walk deadline without a clock seam.
	delay time.Duration
	// coerced rides on the RESPONSE the adapter returns, which is how a real
	// adapter reports what it rewrote — including on a failed dispatch.
	coerced []string
}

type mockAdapter struct {
	format    provcore.Format
	responses []scripted
	callIndex int
	lastReq   provcore.Request
	// observedAttempts captures nexushttp.AttemptFromContext per call so
	// tests can assert the global attempt counter is monotonic across L2
	// + L3 within a single Execute invocation.
	observedAttempts []int
	// prepareBodyCalls / executeWithBodyCalls let
	// TestExecutor_ExecuteWithPreparedBody assert that the prepared-body
	// fast path skipped Adapter.PrepareBody on the primary first attempt.
	prepareBodyCalls     int
	executeWithBodyCalls int
	// lastBody / lastRewrites capture the prepared-body bytes the
	// executor passed through, so tests can assert byte-equivalence
	// with what Phase 5.5 produced.
	lastBody     []byte
	lastRewrites []string
	// lastURLOverride captures the urlOverride argument the executor passed
	// to ExecuteWithBody, so tests can assert a codec endpoint-selection
	// suffix (Gemini :batchEmbedContents) reaches the adapter.
	lastURLOverride string
}

func (m *mockAdapter) Format() provcore.Format { return m.format }
func (m *mockAdapter) SupportsShape(shape typology.WireShape) bool {
	return shape == typology.WireShapeOpenAIChat
}

func (m *mockAdapter) Execute(ctx context.Context, req provcore.Request) (*provcore.Response, error) {
	m.lastReq = req
	m.observedAttempts = append(m.observedAttempts, nexushttp.AttemptFromContext(ctx))
	if m.callIndex >= len(m.responses) {
		return nil, errors.New("no more responses")
	}
	r := m.responses[m.callIndex]
	m.callIndex++
	if r.delay > 0 {
		time.Sleep(r.delay)
	}
	if len(r.coerced) > 0 {
		if r.resp == nil {
			r.resp = &provcore.Response{}
		}
		r.resp.Coerced = r.coerced
	}
	return r.resp, r.err
}

func (m *mockAdapter) Probe(_ context.Context, _ provcore.CallTarget) (*provcore.ProbeResult, error) {
	return &provcore.ProbeResult{OK: true}, nil
}

func (m *mockAdapter) PrepareBody(req provcore.Request) ([]byte, []string, string, error) {
	m.prepareBodyCalls++
	return req.Body, nil, "", nil
}

func (m *mockAdapter) ExecuteWithBody(ctx context.Context, req provcore.Request, body []byte, rewrites []string, urlOverride string) (*provcore.Response, error) {
	m.executeWithBodyCalls++
	m.lastBody = append([]byte(nil), body...)
	m.lastRewrites = append([]string(nil), rewrites...)
	m.lastURLOverride = urlOverride
	req.Body = body
	m.lastReq = req
	m.observedAttempts = append(m.observedAttempts, nexushttp.AttemptFromContext(ctx))
	if m.callIndex >= len(m.responses) {
		return nil, errors.New("no more responses")
	}
	r := m.responses[m.callIndex]
	m.callIndex++
	if r.resp != nil {
		// Mirror the real specAdapter.executeWithBodyAndURL, which stamps the
		// coercion rewrites onto the response's Coerced. A mock that dropped
		// them would hide a double-merge of the x-nexus-coerced markers in the
		// executor (the merge is the executor's, not the adapter's, job).
		r.resp.Coerced = append([]string(nil), rewrites...)
	}
	return r.resp, r.err
}

// mockResolver returns a scripted CallTarget or error.
type mockResolver struct {
	target provcore.CallTarget
	err    error
	calls  int
	// delay makes resolution take real time. The walk deadline is read once
	// before a target is selected and again before its first dispatch, with
	// resolution in between — so a resolve that outlasts the deadline is how a
	// turn ends before anything has failed.
	delay time.Duration
}

func (m *mockResolver) Resolve(_ context.Context, providerID, modelID string, _ provtarget.ResolveHints) (provcore.CallTarget, error) {
	m.calls++
	if m.delay > 0 {
		time.Sleep(m.delay)
	}
	if m.err != nil {
		return provcore.CallTarget{}, m.err
	}
	t := m.target
	if t.ProviderID == "" {
		t.ProviderID = providerID
	}
	if t.ProviderModelID == "" {
		t.ProviderModelID = modelID
	}
	return t, nil
}

// newRegistry builds a Registry pre-populated with the supplied adapter.
func newRegistry(t *testing.T, a provcore.Adapter) *provcore.Registry {
	t.Helper()
	r := provcore.NewRegistry()
	if err := r.Register(a); err != nil {
		t.Fatalf("registry.Register: %v", err)
	}
	r.Freeze()
	return r
}

const mockFormat = provcore.FormatOpenAI

func target(providerName string) routingcore.RoutingTarget {
	return routingcore.RoutingTarget{
		ProviderID:      "prov-" + providerName,
		ProviderName:    providerName,
		ModelID:         "model-1",
		ProviderModelID: "gpt-4",
		BaseURL:         "https://api.example.com",
	}
}

func baseReq() provcore.Request {
	return provcore.Request{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatOpenAI,
		Body:       []byte(`{}`),
	}
}

const providerSlug = "openai"

// failForProvider wraps a resolver so one provider cannot be resolved at all —
// the shape of a target that fails before dispatching and therefore spends
// nothing from the budget.
type failForProvider struct {
	inner *mockResolver
	deny  string
}

// failAfterN resolves successfully N times, then stops — the shape a 429 leaves
// behind, since a rate limit opens the credential's circuit and the next
// resolve for that key fails.
type failAfterN struct {
	inner *mockResolver
	n     int
	calls int
}

func (f *failAfterN) Resolve(ctx context.Context, providerID, modelID string, h provtarget.ResolveHints) (provcore.CallTarget, error) {
	f.calls++
	if f.calls > f.n {
		return provcore.CallTarget{}, errors.New("credential circuit open")
	}
	return f.inner.Resolve(ctx, providerID, modelID, h)
}

func (f *failForProvider) Resolve(ctx context.Context, providerID, modelID string, h provtarget.ResolveHints) (provcore.CallTarget, error) {
	if providerID == f.deny {
		return provcore.CallTarget{}, errors.New("no credential configured")
	}
	return f.inner.Resolve(ctx, providerID, modelID, h)
}

func okResolver() *mockResolver {
	return &mockResolver{target: provcore.CallTarget{
		ProviderName: providerSlug,
		Format:       provcore.FormatOpenAI,
		BaseURL:      "https://api.example.com",
		APIKey:       "sk-123",
	}}
}

// fastBackoffPolicy is the default for tests that exercise multi-attempt
// paths but don't want to wait hundreds of milliseconds between tries.
// Caller passes the desired MaxAttemptsPerTarget.
func fastBackoffPolicy(max int, retryOn ...configtypes.ErrorClass) configtypes.RetryPolicy {
	if len(retryOn) == 0 {
		retryOn = []configtypes.ErrorClass{
			configtypes.ErrorClassNetwork,
			configtypes.ErrorClassTimeout,
			configtypes.ErrorClassRate429,
			configtypes.ErrorClass5xx,
		}
	}
	return configtypes.RetryPolicy{
		MaxAttemptsPerTarget: max,
		RetryOn:              retryOn,
		BackoffInitial:       1 * time.Millisecond,
		BackoffMax:           2 * time.Millisecond,
		BackoffJitter:        0,
	}
}

// metricsCapture is a thread-safe accumulator for the package-level
// metricsRecord hook so tests can assert what the executor emitted.
type metricsCapture struct {
	mu    sync.Mutex
	calls []metricsCall
}

type metricsCall struct {
	provider string
	class    string
	outcome  string
}

func (m *metricsCapture) record(provider, class, outcome string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, metricsCall{provider, class, outcome})
}

func (m *metricsCapture) snapshot() []metricsCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]metricsCall, len(m.calls))
	copy(out, m.calls)
	return out
}

// withCapturedMetrics swaps the package metricsRecord var for the test
// duration and restores the previous emitter on cleanup.
func withCapturedMetrics(t *testing.T) *metricsCapture {
	t.Helper()
	cap := &metricsCapture{}
	prev := metricsRecord
	metricsRecord = cap.record
	t.Cleanup(func() { metricsRecord = prev })
	return cap
}

func TestExecutor_SuccessFirstTarget(t *testing.T) {
	adapter := &mockAdapter{format: mockFormat, responses: []scripted{
		{resp: &provcore.Response{StatusCode: 200, Body: []byte(`ok`)}},
	}}
	reg := newRegistry(t, adapter)
	res := okResolver()
	exec := New(reg, res, nil, nil)

	result := exec.Execute(context.Background(), []routingcore.RoutingTarget{target(providerSlug)}, baseReq(), configtypes.DefaultRetryPolicy())

	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if result.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", result.StatusCode)
	}
	if len(result.Attempts) != 1 {
		t.Fatalf("expected 1 attempt, got %d", len(result.Attempts))
	}
	if res.calls != 1 {
		t.Fatalf("expected 1 resolver call, got %d", res.calls)
	}
	if adapter.lastReq.Target.APIKey != "sk-123" {
		t.Fatalf("expected APIKey from resolver, got %q", adapter.lastReq.Target.APIKey)
	}
}

// TestExecutor_ExecuteWithPreparedBody_SkipsRedundantPrepareBody locks
// the cache-MISS optimisation: when the cache layer has already
// computed the wire body via Adapter.PrepareBody for cache key
// computation, the executor must reuse those bytes for the primary
// target's first attempt instead of re-running PrepareBody. The
// adapter sees ExecuteWithBody once and never sees PrepareBody.
func TestExecutor_ExecuteWithPreparedBody_SkipsRedundantPrepareBody(t *testing.T) {
	adapter := &mockAdapter{format: mockFormat, responses: []scripted{
		{resp: &provcore.Response{StatusCode: 200, Body: []byte(`ok`)}},
	}}
	reg := newRegistry(t, adapter)
	res := okResolver()
	exec := New(reg, res, nil, nil)

	preparedBody := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`)
	preparedRewrites := []string{"reasoning_effort"}

	result := exec.ExecuteWithPreparedBody(
		context.Background(),
		[]routingcore.RoutingTarget{target(providerSlug)},
		baseReq(),
		configtypes.DefaultRetryPolicy(),
		preparedBody,
		preparedRewrites,
		"",
	)
	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if result.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", result.StatusCode)
	}

	if adapter.prepareBodyCalls != 0 {
		t.Fatalf("PrepareBody must NOT be called when prepared body is supplied; got %d calls", adapter.prepareBodyCalls)
	}
	if adapter.executeWithBodyCalls != 1 {
		t.Fatalf("ExecuteWithBody must be called exactly once on the primary first attempt; got %d", adapter.executeWithBodyCalls)
	}
	if string(adapter.lastBody) != string(preparedBody) {
		t.Fatalf("ExecuteWithBody received body %q; expected verbatim prepared body %q", adapter.lastBody, preparedBody)
	}
	if len(adapter.lastRewrites) != 1 || adapter.lastRewrites[0] != "reasoning_effort" {
		t.Fatalf("rewrites not propagated; got %v", adapter.lastRewrites)
	}
}

// TestExecutor_ExecuteWithPreparedBody_NilFallsBackToExecute confirms
// the convenience contract: passing nil prepared body produces exactly
// the same behaviour as Execute (PrepareBody runs, ExecuteWithBody is
// not called).
func TestExecutor_ExecuteWithPreparedBody_NilFallsBackToExecute(t *testing.T) {
	adapter := &mockAdapter{format: mockFormat, responses: []scripted{
		{resp: &provcore.Response{StatusCode: 200, Body: []byte(`ok`)}},
	}}
	reg := newRegistry(t, adapter)
	res := okResolver()
	exec := New(reg, res, nil, nil)

	result := exec.ExecuteWithPreparedBody(
		context.Background(),
		[]routingcore.RoutingTarget{target(providerSlug)},
		baseReq(),
		configtypes.DefaultRetryPolicy(),
		nil, nil, "",
	)
	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if adapter.executeWithBodyCalls != 0 {
		t.Fatalf("ExecuteWithBody must not be called on nil-prepared fallback; got %d", adapter.executeWithBodyCalls)
	}
	// adapter.Execute itself does not increment prepareBodyCalls in the
	// mock — that counter is a stand-in for the real adapter's PrepareBody
	// internal call. The fallback path goes through adapter.Execute which
	// in production calls PrepareBody internally; the mock's Execute does
	// the equivalent work directly. Either way, ExecuteWithBody = 0 is
	// the load-bearing assertion that the fast path was not taken.
}

// TestExecutor_ExecuteWithPreparedBody_FailoverFallsBackToExecute
// locks the trade-off: when the primary first attempt fails and the
// executor falls over to a secondary target, the secondary goes through
// the regular Execute path (no preparedBody for it). PrepareBody runs
// once for that secondary (mock counts it via its Execute method
// receiving the body it would have prepared), and ExecuteWithBody runs
// only for the primary first attempt.
func TestExecutor_ExecuteWithPreparedBody_FailoverFallsBackToExecute(t *testing.T) {
	adapter := &mockAdapter{format: mockFormat, responses: []scripted{
		{err: &provcore.ProviderError{Status: 500, Code: provcore.CodeUpstreamError, Message: "primary 5xx"}},
		{resp: &provcore.Response{StatusCode: 200, Body: []byte(`ok`)}},
	}}
	reg := newRegistry(t, adapter)
	res := okResolver()
	exec := New(reg, res, nil, nil)

	preparedBody := []byte(`{"model":"gpt-4"}`)

	result := exec.ExecuteWithPreparedBody(
		context.Background(),
		[]routingcore.RoutingTarget{target("primary"), target("secondary")},
		baseReq(),
		// max=1 so the primary 5xx fails over to the secondary (the path under
		// test) rather than retrying the primary in place under the new default.
		fastBackoffPolicy(1),
		preparedBody, nil, "",
	)
	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if result.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", result.StatusCode)
	}
	if adapter.executeWithBodyCalls != 1 {
		t.Fatalf("ExecuteWithBody should run exactly once (primary first attempt); got %d", adapter.executeWithBodyCalls)
	}
	if len(result.Attempts) != 2 {
		t.Fatalf("expected 2 attempts (primary fail + secondary success), got %d", len(result.Attempts))
	}
}

func TestExecutor_FallbackOn500(t *testing.T) {
	adapter := &mockAdapter{format: mockFormat, responses: []scripted{
		{err: &provcore.ProviderError{Status: 500, Code: provcore.CodeUpstreamError, Message: "boom"}},
		{resp: &provcore.Response{StatusCode: 200, Body: []byte(`ok`)}},
	}}
	reg := newRegistry(t, adapter)
	res := okResolver()
	exec := New(reg, res, nil, nil)

	targets := []routingcore.RoutingTarget{target(providerSlug), target(providerSlug)}
	// max=1 (no same-target retry) -> first target's 500 immediately L3
	// failovers to the second target which succeeds. Pinned to max=1 so this
	// stays a failover test independent of the platform default (which now
	// allows one same-target retry).
	result := exec.Execute(context.Background(), targets, baseReq(), fastBackoffPolicy(1))

	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if result.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", result.StatusCode)
	}
	if len(result.Attempts) != 2 {
		t.Fatalf("expected 2 attempts, got %d", len(result.Attempts))
	}
}

// Pre-spec executor implicitly retried 429 once on the same target. The new
// rewrite delegates that decision to the policy: with MaxAttemptsPerTarget>=2
// 429 retries on the same target, otherwise it L3-failovers immediately.
// This test pins the new behaviour: with maxPerTarget=2, a 429 followed by a
// 200 succeeds on the same target after one L2 retry.
func TestExecutor_RateLimited_L2Retry(t *testing.T) {
	adapter := &mockAdapter{format: mockFormat, responses: []scripted{
		{err: &provcore.ProviderError{Status: 429, Code: provcore.CodeRateLimited, Message: "rate limited"}},
		{resp: &provcore.Response{StatusCode: 200, Body: []byte(`ok`)}},
	}}
	reg := newRegistry(t, adapter)
	res := okResolver()
	exec := New(reg, res, nil, nil)

	result := exec.Execute(context.Background(),
		[]routingcore.RoutingTarget{target(providerSlug)},
		baseReq(),
		fastBackoffPolicy(2),
	)

	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if result.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 after retry, got %d", result.StatusCode)
	}
	if len(result.Attempts) != 2 {
		t.Fatalf("expected 2 attempts, got %d", len(result.Attempts))
	}
	// Two resolver calls: one up front, one for the retry. The 429 that drove
	// the retry also opened that credential's circuit, so the retry must ask
	// the pool again rather than re-send to the credential it just tripped.
	// With a sticky key the pool hands back the same credential when its
	// circuit is still closed, so this costs nothing when nothing is wrong.
	if res.calls != 2 {
		t.Fatalf("expected 2 resolver calls (initial + re-resolve for the retry), got %d", res.calls)
	}
}

// TestExecutor_DefaultPolicy_RetriesOnceOn5xx pins the platform default's
// user-visible behaviour: with the SHIPPED DefaultRetryPolicy() (no explicit
// override), a single target that returns a transient 5xx then a 200 succeeds
// via one same-target retry — not an immediate hard error. This is the
// behaviour change that makes flaky upstream endpoints self-heal.
func TestExecutor_DefaultPolicy_RetriesOnceOn5xx(t *testing.T) {
	adapter := &mockAdapter{format: mockFormat, responses: []scripted{
		{err: &provcore.ProviderError{Status: 503, Code: provcore.CodeUpstreamError, Message: "high demand"}},
		{resp: &provcore.Response{StatusCode: 200, Body: []byte(`ok`)}},
	}}
	reg := newRegistry(t, adapter)
	res := okResolver()
	exec := New(reg, res, nil, nil)

	// Single target: a same-target retry is the only way this can reach 200.
	result := exec.Execute(context.Background(),
		[]routingcore.RoutingTarget{target(providerSlug)},
		baseReq(),
		configtypes.DefaultRetryPolicy(),
	)

	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if result.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 after one same-target retry, got %d", result.StatusCode)
	}
	if len(result.Attempts) != 2 {
		t.Fatalf("expected 2 attempts on the single target (503 then 200), got %d", len(result.Attempts))
	}
	// The retry re-resolves (2 calls). A 503 does not open a credential
	// circuit — only 429 and three consecutive 401/403 do — so the pool hands
	// back the same credential here; the re-resolve exists for the cases where
	// it would not.
	if res.calls != 2 {
		t.Fatalf("expected 2 resolver calls (initial + re-resolve for the retry), got %d", res.calls)
	}
}

// A rejected credential is not retried in place, and no sibling target on the
// same provider is dispatched either — the key will not become valid inside one
// request, and every target behind it shares the key.
//
// The assertion is on UPSTREAM CALLS rather than on the length of the attempt
// list, because the walk now records the targets it deliberately skipped. That
// record is the point: an operator reading the trace can see the second target
// was passed over and why, instead of finding it silently absent.
func TestExecutor_4xxNoRetry(t *testing.T) {
	adapter := &mockAdapter{format: mockFormat, responses: []scripted{
		{err: &provcore.ProviderError{Status: 401, Code: provcore.CodeAuthFailed, Message: "unauthorized", Raw: []byte(`unauth`)}},
	}}
	reg := newRegistry(t, adapter)
	res := okResolver()
	exec := New(reg, res, nil, nil)

	targets := []routingcore.RoutingTarget{target(providerSlug), target(providerSlug)}
	result := exec.Execute(context.Background(), targets, baseReq(), configtypes.DefaultRetryPolicy())

	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if result.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", result.StatusCode)
	}
	if got := result.UpstreamAttempts(); got != 1 {
		t.Fatalf("upstream calls=%d, want 1 — the rejected credential must not be spent again", got)
	}
	if adapter.callIndex != 1 {
		t.Fatalf("adapter calls=%d, want 1", adapter.callIndex)
	}
	if len(result.Attempts) != 2 {
		t.Fatalf("attempts=%d, want 2: the failure, and the sibling target recorded as skipped", len(result.Attempts))
	}
	if result.Attempts[1].Dispatched {
		t.Error("the skipped target was recorded as dispatched, which would overcount upstream calls")
	}
}

func TestExecutor_AllExhausted(t *testing.T) {
	adapter := &mockAdapter{format: mockFormat, responses: []scripted{
		{err: errors.New("net")},
		{err: errors.New("net2")},
	}}
	reg := newRegistry(t, adapter)
	res := okResolver()
	exec := New(reg, res, nil, nil)

	targets := []routingcore.RoutingTarget{target(providerSlug), target(providerSlug)}
	// max=1 keeps this a clean per-target-exhaust test: one try per target, both
	// fail, ErrAllTargetsExhausted. (The retry-then-exhaust path is covered by the
	// explicit-policy multi-attempt tests.)
	result := exec.Execute(context.Background(), targets, baseReq(), fastBackoffPolicy(1))

	if !errors.Is(result.Error, ErrAllTargetsExhausted) {
		t.Fatalf("expected ErrAllTargetsExhausted, got %v", result.Error)
	}
	if len(result.Attempts) != 2 {
		t.Fatalf("expected 2 attempts, got %d", len(result.Attempts))
	}
}

func TestExecutor_ResolverFailure(t *testing.T) {
	adapter := &mockAdapter{format: mockFormat, responses: []scripted{
		{resp: &provcore.Response{StatusCode: 200, Body: []byte(`ok`)}},
	}}
	reg := newRegistry(t, adapter)
	res := &mockResolver{err: errors.New("vault unreachable")}
	exec := New(reg, res, nil, nil)

	targets := []routingcore.RoutingTarget{target(providerSlug), target(providerSlug)}
	result := exec.Execute(context.Background(), targets, baseReq(), configtypes.DefaultRetryPolicy())

	// Every target died while being prepared, so no provider was ever asked.
	// Reporting that as an exhausted upstream is the misattribution that made
	// a broken credential look like a provider outage in production.
	if !errors.Is(result.Error, ErrNoTargetDispatchable) {
		t.Fatalf("expected ErrNoTargetDispatchable, got %v", result.Error)
	}
	if errors.Is(result.Error, ErrAllTargetsExhausted) {
		t.Fatal("a resolver failure must not be reported as an exhausted upstream")
	}
	if len(result.Attempts) != 2 {
		t.Fatalf("expected 2 attempts, got %d", len(result.Attempts))
	}
	for _, a := range result.Attempts {
		if a.Error == "" {
			t.Fatal("expected resolver error in attempt")
		}
		if a.Dispatched {
			t.Fatal("a resolver failure never leaves the process, so no attempt may be marked dispatched")
		}
	}
	if result.Terminal() != nil {
		t.Fatal("no dispatched attempt means no terminal upstream outcome")
	}
}

// TestExecutor_DispatchedThenResolverFailureStaysExhausted — the counterpart.
// Once ANY attempt has reached a provider, the walk really did exhaust the
// upstream and must keep saying so; the new pre-dispatch classification must
// not swallow a genuine provider failure just because a later target failed
// to resolve.
func TestExecutor_DispatchedThenResolverFailureStaysExhausted(t *testing.T) {
	adapter := &mockAdapter{format: mockFormat, responses: []scripted{
		{err: &provcore.ProviderError{Status: 500, Message: "upstream boom"}},
	}}
	reg := newRegistry(t, adapter)
	res := &failAfterNResolver{ok: okResolver(), n: 1}
	exec := New(reg, res, nil, nil)

	targets := []routingcore.RoutingTarget{target(providerSlug), target(providerSlug)}
	result := exec.Execute(context.Background(), targets, baseReq(), configtypes.DefaultRetryPolicy())

	if errors.Is(result.Error, ErrNoTargetDispatchable) {
		t.Fatalf("a provider WAS called, so this is an exhausted upstream, not an undispatchable one; got %v", result.Error)
	}
	dispatched := 0
	for _, a := range result.Attempts {
		if a.Dispatched {
			dispatched++
		}
	}
	if dispatched == 0 {
		t.Fatal("test setup is wrong: expected at least one dispatched attempt")
	}
}

// failAfterNResolver resolves successfully n times, then fails — modelling a
// pool whose first credential works and whose next one cannot be decrypted.
type failAfterNResolver struct {
	ok    provtarget.Resolver
	n     int
	calls int
}

func (r *failAfterNResolver) Resolve(ctx context.Context, providerID, modelID string, hints provtarget.ResolveHints) (provcore.CallTarget, error) {
	r.calls++
	if r.calls > r.n {
		return provcore.CallTarget{}, errors.New("credentials: decryption failed: cipher: message authentication failed")
	}
	return r.ok.Resolve(ctx, providerID, modelID, hints)
}

func TestExecutor_NoCompatibleProviderTerminal(t *testing.T) {
	adapter := &mockAdapter{format: mockFormat, responses: []scripted{
		{err: &provcore.ProviderError{Status: 400, Code: provcore.CodeNoCompatibleProvider, Message: "no codec"}},
	}}
	reg := newRegistry(t, adapter)
	res := okResolver()
	exec := New(reg, res, nil, nil)

	targets := []routingcore.RoutingTarget{target(providerSlug), target(providerSlug)}
	result := exec.Execute(context.Background(), targets, baseReq(), configtypes.DefaultRetryPolicy())

	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if len(result.Attempts) != 1 {
		t.Fatalf("expected 1 attempt (terminal), got %d", len(result.Attempts))
	}
	if result.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", result.StatusCode)
	}
}

// New tests for L2/L3 RetryPolicy behavior

// The L2 retry budget on a single target keeps trying until it either
// succeeds (here: 4th attempt) or hits MaxAttemptsPerTarget. This pins
// the basic per-target retry loop and the global attempt counter that
// nexushttp.AttemptFromContext exposes for outbound debug logs.
func TestExecute_L2_Retries_429_UntilSuccess(t *testing.T) {
	adapter := &mockAdapter{format: mockFormat, responses: []scripted{
		{err: &provcore.ProviderError{Status: 429, Code: provcore.CodeRateLimited, Message: "rl"}},
		{err: &provcore.ProviderError{Status: 429, Code: provcore.CodeRateLimited, Message: "rl"}},
		{err: &provcore.ProviderError{Status: 429, Code: provcore.CodeRateLimited, Message: "rl"}},
		{resp: &provcore.Response{StatusCode: 200, Body: []byte(`ok`)}},
	}}
	reg := newRegistry(t, adapter)
	res := okResolver()
	exec := New(reg, res, nil, nil)

	cap := withCapturedMetrics(t)

	result := exec.Execute(context.Background(),
		[]routingcore.RoutingTarget{target(providerSlug)},
		baseReq(),
		fastBackoffPolicy(4, configtypes.ErrorClassRate429),
	)

	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if result.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", result.StatusCode)
	}
	if len(result.Attempts) != 4 {
		t.Fatalf("expected 4 attempts, got %d", len(result.Attempts))
	}
	wantSeq := []int{1, 2, 3, 4}
	if got := adapter.observedAttempts; len(got) != len(wantSeq) {
		t.Fatalf("attempt counter sequence length: want %v got %v", wantSeq, got)
	}
	for i, want := range wantSeq {
		if adapter.observedAttempts[i] != want {
			t.Fatalf("attempt[%d]: want %d got %d (full %v)", i, want, adapter.observedAttempts[i], adapter.observedAttempts)
		}
	}
	// Exactly one "retried_succeeded", naming the class it recovered from. A
	// rate limit is handed back to the walk rather than retried in place, so the
	// attempt that succeeds is a LATER TURN — the emission has to survive that,
	// and the class has to come from the target rather than from a variable
	// scoped to the turn that failed.
	succeeded := 0
	for _, c := range cap.snapshot() {
		if c.outcome != "retried_succeeded" {
			continue
		}
		succeeded++
		if c.class != string(configtypes.ErrorClassRate429) {
			t.Errorf("retried_succeeded carries class %q, want 429 — the class of the failure "+
				"this retry recovered from, which the walk left behind several turns ago", c.class)
		}
	}
	if succeeded != 1 {
		t.Fatalf("expected exactly one retried_succeeded emission; got %d in %+v",
			succeeded, cap.snapshot())
	}
}

// 4xx terminal codes (auth_failed, invalid_request, ...) MUST NOT trigger
// either L2 retry or L3 failover regardless of policy. The body is
// surfaced verbatim from the upstream.
func TestExecute_L2_NoRetry_On4xx_ReturnsImmediately(t *testing.T) {
	adapter := &mockAdapter{format: mockFormat, responses: []scripted{
		{err: &provcore.ProviderError{Status: 400, Code: provcore.CodeInvalidRequest, Message: "bad", Raw: []byte(`{"error":"bad"}`)}},
		{resp: &provcore.Response{StatusCode: 200, Body: []byte(`should not be reached`)}},
	}}
	reg := newRegistry(t, adapter)
	res := okResolver()
	exec := New(reg, res, nil, nil)

	cap := withCapturedMetrics(t)

	result := exec.Execute(context.Background(),
		[]routingcore.RoutingTarget{target(providerSlug), target("openai-secondary")},
		baseReq(),
		fastBackoffPolicy(3),
	)

	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if result.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", result.StatusCode)
	}
	if len(result.Attempts) != 1 {
		t.Fatalf("expected 1 attempt (no failover, no retry); got %d", len(result.Attempts))
	}
	// Second target was never touched.
	if adapter.callIndex != 1 {
		t.Fatalf("adapter should have been called exactly once; got callIndex=%d", adapter.callIndex)
	}
	if calls := cap.snapshot(); len(calls) != 0 {
		t.Fatalf("4xx terminal must not emit retry metrics; got %+v", calls)
	}
}

// When the failure class is retryable but the rule excluded it, the executor
// must skip L2 entirely and L3-failover to the next target — and emit
// "failover_class_excluded" on the metric.
func TestExecute_L3_FailoverWhen_ClassNotInRetryOn(t *testing.T) {
	adapter := &mockAdapter{format: mockFormat, responses: []scripted{
		{err: &provcore.ProviderError{Status: 429, Code: provcore.CodeRateLimited, Message: "rl"}},
		{resp: &provcore.Response{StatusCode: 200, Body: []byte(`ok`)}},
	}}
	reg := newRegistry(t, adapter)
	res := okResolver()
	exec := New(reg, res, nil, nil)

	cap := withCapturedMetrics(t)

	// Allow 3 retries per target, but exclude 429 from RetryOn — 429 must
	// not re-attempt on the same target; we go straight to target 2.
	result := exec.Execute(context.Background(),
		[]routingcore.RoutingTarget{target(providerSlug), target("openai-secondary")},
		baseReq(),
		fastBackoffPolicy(3, configtypes.ErrorClass5xx),
	)

	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if result.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 after failover, got %d", result.StatusCode)
	}
	if len(result.Attempts) != 2 {
		t.Fatalf("expected 2 attempts (one per target); got %d", len(result.Attempts))
	}
	calls := cap.snapshot()
	if len(calls) != 1 || calls[0].outcome != "failover_class_excluded" || calls[0].class != string(configtypes.ErrorClassRate429) {
		t.Fatalf("expected one failover_class_excluded(429); got %+v", calls)
	}
}

// The httpclient attempt counter is GLOBAL across L2 + L3 for one Execute,
// so the outbound HTTP debug log can correlate every upstream call against
// the request id even when retries cross targets.
func TestExecute_AttemptCounter_GlobalAcrossL2andL3(t *testing.T) {
	respFor5xx := func() scripted {
		return scripted{err: &provcore.ProviderError{Status: 500, Code: provcore.CodeUpstreamError, Message: "boom"}}
	}
	adapter := &mockAdapter{format: mockFormat, responses: []scripted{
		respFor5xx(), respFor5xx(), respFor5xx(), respFor5xx(),
	}}
	reg := newRegistry(t, adapter)
	res := okResolver()
	exec := New(reg, res, nil, nil)

	result := exec.Execute(context.Background(),
		[]routingcore.RoutingTarget{target("t1"), target("t2")},
		baseReq(),
		fastBackoffPolicy(2),
	)

	if !errors.Is(result.Error, ErrAllTargetsExhausted) {
		t.Fatalf("expected ErrAllTargetsExhausted, got %v", result.Error)
	}
	if len(result.Attempts) != 4 {
		t.Fatalf("expected 4 attempts (2 targets x 2 tries), got %d", len(result.Attempts))
	}
	wantSeq := []int{1, 2, 3, 4}
	if got := adapter.observedAttempts; len(got) != len(wantSeq) {
		t.Fatalf("attempt counter sequence length: want %v got %v", wantSeq, got)
	}
	for i, want := range wantSeq {
		if adapter.observedAttempts[i] != want {
			t.Fatalf("attempt[%d]: want %d got %d (full %v)", i, want, adapter.observedAttempts[i], adapter.observedAttempts)
		}
	}
}

// When the parent context deadline is so close that even one backoff cycle
// would blow it, the executor must NOT sleep — it bails to L3 immediately.
// Total wall time must stay well under the configured BackoffInitial.
func TestExecute_BackoffSkipped_WhenContextDeadlineImminent(t *testing.T) {
	adapter := &mockAdapter{format: mockFormat, responses: []scripted{
		{err: &provcore.ProviderError{Status: 500, Code: provcore.CodeUpstreamError, Message: "boom"}},
	}}
	reg := newRegistry(t, adapter)
	res := okResolver()
	exec := New(reg, res, nil, nil)

	policy := configtypes.RetryPolicy{
		MaxAttemptsPerTarget: 2,
		RetryOn:              []configtypes.ErrorClass{configtypes.ErrorClass5xx},
		BackoffInitial:       100 * time.Millisecond,
		BackoffMax:           500 * time.Millisecond,
		BackoffJitter:        0,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	start := time.Now()
	result := exec.Execute(ctx,
		[]routingcore.RoutingTarget{target(providerSlug)},
		baseReq(),
		policy,
	)
	elapsed := time.Since(start)

	if elapsed > 80*time.Millisecond {
		t.Fatalf("executor slept past deadline; elapsed=%v", elapsed)
	}
	if !errors.Is(result.Error, ErrAllTargetsExhausted) {
		t.Fatalf("expected ErrAllTargetsExhausted, got %v", result.Error)
	}
	dispatched := 0
	for _, a := range result.Attempts {
		if a.Dispatched {
			dispatched++
		}
	}
	if dispatched != 1 {
		t.Fatalf("expected 1 dispatch (no second try after deadline-skip); got %d in %d entries",
			dispatched, len(result.Attempts))
	}
}

// Attempt.ErrorClass is stamped per attempt, in the SAME vocabulary `code`
// uses, so one JSON object does not carry two spellings of one failure.
// Success leaves it empty.
func TestExecute_ErrorClassRecorded(t *testing.T) {
	adapter := &mockAdapter{format: mockFormat, responses: []scripted{
		{err: &provcore.ProviderError{Status: 500, Code: provcore.CodeUpstreamError, Message: "boom"}},
		{resp: &provcore.Response{StatusCode: 200, Body: []byte(`ok`)}},
	}}
	reg := newRegistry(t, adapter)
	res := okResolver()
	exec := New(reg, res, nil, nil)

	result := exec.Execute(context.Background(),
		[]routingcore.RoutingTarget{target(providerSlug)},
		baseReq(),
		fastBackoffPolicy(2),
	)

	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if len(result.Attempts) != 2 {
		t.Fatalf("expected 2 attempts, got %d", len(result.Attempts))
	}
	if got := result.Attempts[0].ErrorClass; got != provcore.CodeUpstreamError {
		t.Fatalf("Attempts[0].ErrorClass: want %q got %q — the row publishes `code` beside this, "+
			"and %q would be a second spelling of the same failure on one object",
			provcore.CodeUpstreamError, got, configtypes.ErrorClass5xx)
	}
	if got := result.Attempts[1].ErrorClass; got != "" {
		t.Fatalf("Attempts[1].ErrorClass on success: want empty got %q", got)
	}
}

// First-try success must not increment the router retry counter.
func TestExecute_Success_FirstTry_NoRetryMetric(t *testing.T) {
	adapter := &mockAdapter{format: mockFormat, responses: []scripted{
		{resp: &provcore.Response{StatusCode: 200, Body: []byte(`ok`)}},
	}}
	reg := newRegistry(t, adapter)
	res := okResolver()
	exec := New(reg, res, nil, nil)

	cap := withCapturedMetrics(t)

	result := exec.Execute(context.Background(),
		[]routingcore.RoutingTarget{target(providerSlug)},
		baseReq(),
		configtypes.DefaultRetryPolicy(),
	)

	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if result.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", result.StatusCode)
	}
	if calls := cap.snapshot(); len(calls) != 0 {
		t.Fatalf("first-try success must not emit retry metrics; got %+v", calls)
	}
}

// Context overflow fails over to the NEXT target without retrying the
// same one — the same model will always overflow, but a larger-context
// sibling (e.g. the smart strategy's upgrade target) can serve the
// request unchanged.
func TestExecute_ContextOverflow_FailsOverToNextTarget(t *testing.T) {
	adapter := &mockAdapter{format: mockFormat, responses: []scripted{
		{err: &provcore.ProviderError{Status: 400, Code: provcore.CodeContextOverflow, Message: "prompt is too long"}},
		{resp: &provcore.Response{StatusCode: 200, Body: []byte(`ok`)}},
	}}
	reg := newRegistry(t, adapter)
	exec := New(reg, okResolver(), nil, nil)

	result := exec.Execute(context.Background(),
		[]routingcore.RoutingTarget{target(providerSlug), target("bigger-window")},
		baseReq(),
		fastBackoffPolicy(3, configtypes.ErrorClass5xx),
	)

	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if result.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 after context-overflow failover, got %d", result.StatusCode)
	}
	if len(result.Attempts) != 2 {
		t.Fatalf("expected exactly 2 attempts (no same-target retry), got %d", len(result.Attempts))
	}
}

// Context overflow on the LAST (or only) target returns the provider's
// own error result — the client must see the upstream context error,
// not a generic all-targets-exhausted.
func TestExecute_ContextOverflow_LastTargetReturnsProviderError(t *testing.T) {
	adapter := &mockAdapter{format: mockFormat, responses: []scripted{
		{err: &provcore.ProviderError{Status: 400, Code: provcore.CodeContextOverflow, Message: "prompt is too long"}},
	}}
	reg := newRegistry(t, adapter)
	exec := New(reg, okResolver(), nil, nil)

	result := exec.Execute(context.Background(),
		[]routingcore.RoutingTarget{target(providerSlug)},
		baseReq(),
		fastBackoffPolicy(3, configtypes.ErrorClass5xx),
	)

	if errors.Is(result.Error, ErrAllTargetsExhausted) {
		t.Fatalf("single-target overflow must return the provider error, not ErrAllTargetsExhausted")
	}
	if result.StatusCode != http.StatusBadRequest || result.ProviderError == nil || result.ProviderError.Code != provcore.CodeContextOverflow {
		t.Fatalf("expected the provider's 400 context-overflow surfaced, got status=%d providerError=%+v err=%v",
			result.StatusCode, result.ProviderError, result.Error)
	}
	if len(result.Attempts) != 1 {
		t.Fatalf("expected exactly 1 attempt, got %d", len(result.Attempts))
	}
}

// A ContextUpgradeOnly target is reserved for context-overflow failover:
// transient failures (5xx/429/network) on the primary must NOT spill
// onto the larger — typically pricier — upgrade model.
func TestExecute_UpgradeOnlyTarget_SkippedForNonOverflowFailures(t *testing.T) {
	adapter := &mockAdapter{format: mockFormat, responses: []scripted{
		{err: &provcore.ProviderError{Status: 500, Code: provcore.CodeUpstreamError, Message: "boom"}},
	}}
	reg := newRegistry(t, adapter)
	exec := New(reg, okResolver(), nil, nil)

	upgrade := target("big-window")
	upgrade.ContextUpgradeOnly = true
	result := exec.Execute(context.Background(),
		[]routingcore.RoutingTarget{target(providerSlug), upgrade},
		baseReq(),
		fastBackoffPolicy(1),
	)

	if !errors.Is(result.Error, ErrAllTargetsExhausted) {
		t.Fatalf("expected exhausted (upgrade target skipped for 5xx), got %v", result.Error)
	}
	if len(result.Attempts) != 1 {
		t.Fatalf("expected 1 attempt (upgrade target must not run), got %d", len(result.Attempts))
	}
}

func TestExecute_UpgradeOnlyTarget_UsedForContextOverflow(t *testing.T) {
	adapter := &mockAdapter{format: mockFormat, responses: []scripted{
		{err: &provcore.ProviderError{Status: 400, Code: provcore.CodeContextOverflow, Message: "prompt is too long"}},
		{resp: &provcore.Response{StatusCode: 200, Body: []byte(`ok`)}},
	}}
	reg := newRegistry(t, adapter)
	exec := New(reg, okResolver(), nil, nil)

	upgrade := target("big-window")
	upgrade.ContextUpgradeOnly = true
	result := exec.Execute(context.Background(),
		[]routingcore.RoutingTarget{target(providerSlug), upgrade},
		baseReq(),
		fastBackoffPolicy(1),
	)

	if result.Error != nil || result.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 via upgrade target, got status=%d err=%v", result.StatusCode, result.Error)
	}
	if len(result.Attempts) != 2 {
		t.Fatalf("expected 2 attempts, got %d", len(result.Attempts))
	}
}

// F2 orphan case: a downstream filter (narrowing / cross-format) can
// leave a ContextUpgradeOnly target as the ONLY survivor. With nothing
// attempted before it, it must run as an ordinary target — not be
// skipped into a spurious zero-attempt all-targets-exhausted.
func TestExecute_UpgradeOnlyTarget_AloneRunsAsNormal(t *testing.T) {
	adapter := &mockAdapter{format: mockFormat, responses: []scripted{
		{resp: &provcore.Response{StatusCode: 200, Body: []byte(`ok`)}},
	}}
	reg := newRegistry(t, adapter)
	exec := New(reg, okResolver(), nil, nil)

	only := target("big-window")
	only.ContextUpgradeOnly = true
	result := exec.Execute(context.Background(),
		[]routingcore.RoutingTarget{only},
		baseReq(),
		fastBackoffPolicy(1),
	)

	if result.Error != nil || result.StatusCode != http.StatusOK {
		t.Fatalf("lone upgrade target must run as normal, got status=%d err=%v", result.StatusCode, result.Error)
	}
	if len(result.Attempts) != 1 {
		t.Fatalf("expected 1 attempt, got %d", len(result.Attempts))
	}
}

// F2 reorder case: health ranking can move a ContextUpgradeOnly target
// ahead of its pick. First in the list with nothing attempted before
// it, it must run as an ordinary target rather than being skipped.
func TestExecute_UpgradeOnlyTarget_FirstRunsAsNormal(t *testing.T) {
	adapter := &mockAdapter{format: mockFormat, responses: []scripted{
		{resp: &provcore.Response{StatusCode: 200, Body: []byte(`ok`)}},
	}}
	reg := newRegistry(t, adapter)
	exec := New(reg, okResolver(), nil, nil)

	upgrade := target("big-window")
	upgrade.ContextUpgradeOnly = true
	result := exec.Execute(context.Background(),
		[]routingcore.RoutingTarget{upgrade, target(providerSlug)},
		baseReq(),
		fastBackoffPolicy(1),
	)

	if result.Error != nil || result.StatusCode != http.StatusOK {
		t.Fatalf("front upgrade target must run as normal, got status=%d err=%v", result.StatusCode, result.Error)
	}
	if len(result.Attempts) != 1 {
		t.Fatalf("expected first target attempted, got %d attempts", len(result.Attempts))
	}
}

// TestExecutor_ABrokenCredentialDoesNotSinkTheRequest.
//
// Authentication failure is grouped with invalid-request today, and that group
// returns immediately without trying another target. But the two say opposite
// things about whose fault the failure is. An invalid request is the caller's,
// and every other model would reject it identically — stopping is right. A
// rejected credential is one PROVIDER's misconfiguration, and it tells us
// nothing about the healthy targets waiting behind it.
//
// So an org that rotates a key badly, or lets one provider's billing lapse,
// loses every request routed through a rule whose first target sits on that
// provider — while models it is entitled to, on providers that are fine, are
// never asked.
func TestExecutor_ABrokenCredentialDoesNotSinkTheRequest(t *testing.T) {
	adapter := &mockAdapter{format: mockFormat, responses: []scripted{
		{err: &provcore.ProviderError{Code: provcore.CodeAuthFailed, Message: "invalid api key"}},
		{resp: &provcore.Response{StatusCode: 200, Body: []byte(`served`)}},
	}}
	exec := New(newRegistry(t, adapter), okResolver(), nil, nil)

	result := exec.Execute(context.Background(),
		[]routingcore.RoutingTarget{target("expired-key"), target("healthy")},
		baseReq(), configtypes.DefaultRetryPolicy())

	if result.Error != nil {
		t.Fatalf("a healthy second target was available: %v", result.Error)
	}
	if result.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200 — the request never reached the healthy provider", result.StatusCode)
	}
	if len(result.Attempts) != 2 {
		t.Fatalf("attempts=%d, want 2: the failed provider recorded, then the one that served", len(result.Attempts))
	}

	// Eliminated, not merely deprioritised: the credential will not become
	// valid inside one request, so spending a second call on it is waste.
	if adapter.callIndex != 2 {
		t.Errorf("upstream calls=%d, want 2 — a rejected credential must not be retried in place", adapter.callIndex)
	}
}

// TestExecutor_AllProvidersRejected_SurfacesTheAuthError.
//
// When every provider's credential is rejected — an organisation-wide key
// rotation gone wrong, a lapsed bill — the walk eliminates each in turn and
// ends with nothing served. The question is what the caller is then told.
//
// A generic exhausted-targets error becomes a 502 "all upstream providers
// failed", which is a lie with consequences: SDKs retry 5xx and never retry
// 401, so every client starts re-running the whole walk against credentials
// that cannot work, and the operator is paged toward a provider outage that is
// not happening. The upstream's own 401 says what is wrong and stops the storm.
func TestExecutor_AllProvidersRejected_SurfacesTheAuthError(t *testing.T) {
	adapter := &mockAdapter{format: mockFormat, responses: []scripted{
		{err: &provcore.ProviderError{Code: provcore.CodeAuthFailed, Status: 401, Message: "key expired", Raw: []byte(`{"error":"key expired"}`)}},
		{err: &provcore.ProviderError{Code: provcore.CodeAuthFailed, Status: 401, Message: "key expired", Raw: []byte(`{"error":"key expired"}`)}},
	}}
	exec := New(newRegistry(t, adapter), okResolver(), nil, nil)

	result := exec.Execute(context.Background(),
		[]routingcore.RoutingTarget{target("openai"), target("anthropic")},
		baseReq(), configtypes.DefaultRetryPolicy())

	if result.Error != nil {
		t.Fatalf("the walk must surface the upstream refusal, not a transport error: %v", result.Error)
	}
	if result.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401 — a credential problem reported as an outage sends "+
			"every SDK into a retry loop that cannot succeed", result.StatusCode)
	}
	if result.ProviderError == nil || result.ProviderError.Code != provcore.CodeAuthFailed {
		t.Error("the provider's own error envelope must survive to the caller")
	}
	// Both were tried: they are different providers, so eliminating the first
	// says nothing about the second.
	if got := result.UpstreamAttempts(); got != 2 {
		t.Errorf("upstream calls=%d, want 2 — each provider is judged on its own credential", got)
	}
}

// TestExecutor_ClientGoneStopsTheWalk.
//
// When the caller's context is cancelled — a user pressing stop, a consumer
// redeploying — the transport error that follows says nothing about the
// provider. Treating it as a provider fault has two costs, and the walk pays
// both: it records a health failure against a provider that did nothing wrong,
// and it keeps dispatching to further targets on behalf of a client that is no
// longer listening. Under a cancellation storm the first cost compounds until
// every provider touched is marked unavailable for everyone else.
//
// Nothing about the request can succeed once the caller is gone, so the walk
// ends where the cancellation was noticed.
func TestExecutor_ClientGoneStopsTheWalk(t *testing.T) {
	adapter := &mockAdapter{format: mockFormat, responses: []scripted{
		{err: &provcore.ProviderError{Code: provcore.CodeClientGone, Status: 499, Message: "client went away: context canceled"}},
		{resp: &provcore.Response{StatusCode: 200, Body: []byte(`should never be reached`)}},
	}}
	exec := New(newRegistry(t, adapter), okResolver(), nil, nil)

	result := exec.Execute(context.Background(),
		[]routingcore.RoutingTarget{target("first"), target("second")},
		baseReq(), configtypes.DefaultRetryPolicy())

	if adapter.callIndex != 1 {
		t.Fatalf("upstream calls=%d, want 1 — the walk continued on behalf of a client that had gone", adapter.callIndex)
	}
	if result.StatusCode == http.StatusOK {
		t.Error("a request whose caller disconnected was reported as served")
	}
}

// TestExecutor_CallBudgetBoundsTotalDispatches.
//
// The budget's whole claim is that it bounds what a request can spend. That
// claim only holds if same-target attempts count: a design where they were
// exempt let a budget of 1 permit five billed generations, which is the
// opposite of what the number promises.
//
// Five targets, two attempts each, would reach ten dispatches unbounded. A
// budget of 3 must stop at 3 — and the targets it never reached must appear in
// the attempt record, so an operator can see they were passed over rather than
// finding them silently absent.
func TestExecutor_CallBudgetBoundsTotalDispatches(t *testing.T) {
	responses := make([]scripted, 12)
	for i := range responses {
		responses[i] = scripted{err: errors.New("upstream down")}
	}
	adapter := &mockAdapter{format: mockFormat, responses: responses}
	exec := New(newRegistry(t, adapter), okResolver(), nil, nil)

	policy := configtypes.DefaultRetryPolicy()
	policy.MaxUpstreamCalls = 3
	policy.BackoffInitial = time.Microsecond
	policy.BackoffMax = time.Microsecond

	result := exec.Execute(context.Background(), []routingcore.RoutingTarget{
		target("a"), target("b"), target("c"), target("d"), target("e"),
	}, baseReq(), policy)

	if adapter.callIndex != 3 {
		t.Fatalf("upstream calls=%d, want 3 — the budget did not bound the walk", adapter.callIndex)
	}
	if got := result.UpstreamAttempts(); got != 3 {
		t.Errorf("recorded upstream attempts=%d, want 3", got)
	}
	if len(result.Attempts) <= 3 {
		t.Errorf("attempts=%d — the targets skipped for want of budget left no trace",
			len(result.Attempts))
	}
}

// A walk deadline in the past stops the walk before it dispatches anything.
// The guard it replaces read the request context's deadline, which nothing on
// this path ever sets, so it never fired: with a ten-minute per-attempt
// timeout, a walk across several targets could keep dispatching for hours on
// behalf of a client long gone.
func TestExecutor_WalkDeadlineStopsTheWalk(t *testing.T) {
	// The first target burns more wall-clock than the whole walk is allowed,
	// which is the real shape of the problem: an unresponsive upstream, not a
	// misconfigured duration.
	adapter := &mockAdapter{format: mockFormat, responses: []scripted{
		{err: errors.New("upstream hung"), delay: 60 * time.Millisecond},
		{resp: &provcore.Response{StatusCode: 200, Body: []byte(`never reached`)}},
	}}
	exec := New(newRegistry(t, adapter), okResolver(), nil, nil)

	policy := configtypes.DefaultRetryPolicy()
	policy.MaxWalkDuration = 10 * time.Millisecond
	policy.BackoffInitial = time.Microsecond
	policy.BackoffMax = time.Microsecond

	result := exec.Execute(context.Background(),
		[]routingcore.RoutingTarget{target("slow"), target("fast")}, baseReq(), policy)

	if adapter.callIndex != 1 {
		t.Fatalf("upstream calls=%d, want 1 — the walk kept dispatching past its own deadline", adapter.callIndex)
	}
	if result.Error == nil {
		t.Error("a walk that ran out of time reported success")
	}
	if len(result.Attempts) < 2 {
		t.Errorf("attempts=%d — the target skipped for want of time left no trace", len(result.Attempts))
	}
}

// TestExecutor_LocalDecodeFailureIsNotRetried.
//
// An upstream that answers 2xx has produced the answer and charged for it. When
// the failure is ours afterwards — a read cap, a decompression fault, malformed
// bytes from a CDN in front of a custom endpoint — asking again does not
// recover the answer we already paid for. It buys a second one.
//
// Wrapped as an upstream error, this sat inside the default retry classes, so a
// single flaky decode became one billed generation per attempt and then one per
// remaining target. For image, video and speech, where every call generates new
// content, the walk stops entirely.
func TestExecutor_LocalDecodeFailureIsNotRetried(t *testing.T) {
	t.Run("image generation stops the walk", func(t *testing.T) {
		adapter := &mockAdapter{format: mockFormat, responses: []scripted{
			{err: &provcore.ProviderError{Code: provcore.CodeLocalProcessing, Status: 502, Message: "decode response: unexpected EOF"}},
			{resp: &provcore.Response{StatusCode: 200, Body: []byte(`a second image nobody asked for`)}},
		}}
		exec := New(newRegistry(t, adapter), okResolver(), nil, nil)

		req := baseReq()
		req.WireShape = typology.WireShapeOpenAIImages

		result := exec.Execute(context.Background(),
			[]routingcore.RoutingTarget{target("a"), target("b")}, req, configtypes.DefaultRetryPolicy())

		if adapter.callIndex != 1 {
			t.Fatalf("upstream calls=%d, want 1 — each extra call is another image generated and billed", adapter.callIndex)
		}
		if result.StatusCode != http.StatusBadGateway {
			t.Errorf("status=%d, want 502 naming the local failure", result.StatusCode)
		}
	})

	t.Run("chat may fail over, but never retries the same target", func(t *testing.T) {
		adapter := &mockAdapter{format: mockFormat, responses: []scripted{
			{err: &provcore.ProviderError{Code: provcore.CodeLocalProcessing, Status: 502, Message: "read body: unexpected EOF"}},
			{resp: &provcore.Response{StatusCode: 200, Body: []byte(`served`)}},
		}}
		exec := New(newRegistry(t, adapter), okResolver(), nil, nil)

		result := exec.Execute(context.Background(),
			[]routingcore.RoutingTarget{target("a"), target("b")}, baseReq(), configtypes.DefaultRetryPolicy())

		if result.StatusCode != http.StatusOK {
			t.Fatalf("status=%d, want 200 — a chat answer is a function of its input and can be re-asked elsewhere", result.StatusCode)
		}
		if adapter.callIndex != 2 {
			t.Errorf("upstream calls=%d, want 2 — one per target, never twice on the same one", adapter.callIndex)
		}
	})
}

// TestExecutor_SelectionFollowsTheFailure covers the rule the whole
// classification exists to serve: the reason an attempt failed decides which
// target is tried next.
//
// Position is the wrong answer for exactly two failures, and both are cases
// where the next entry in the list is the one least likely to help.
func TestExecutor_SelectionFollowsTheFailure(t *testing.T) {
	withWindow := func(name string, ctxTokens int) routingcore.RoutingTarget {
		tg := target(name)
		tg.MaxContextTokens = ctxTokens
		return tg
	}

	t.Run("overflow reaches for the largest window, not the next entry", func(t *testing.T) {
		// A price-ordered list: the next entry after the overflow is SMALLER.
		// Walking positionally would overflow again before finding the model
		// that could have served the request all along.
		adapter := &mockAdapter{format: mockFormat, responses: []scripted{
			{err: &provcore.ProviderError{Code: provcore.CodeContextOverflow, Status: 400, Message: "context length exceeded"}},
			{resp: &provcore.Response{StatusCode: 200, Body: []byte(`served`)}},
		}}
		exec := New(newRegistry(t, adapter), okResolver(), nil, nil)

		result := exec.Execute(context.Background(), []routingcore.RoutingTarget{
			withWindow("overflowed", 8_000),
			withWindow("even-smaller", 4_000),
			withWindow("roomy", 200_000),
		}, baseReq(), configtypes.DefaultRetryPolicy())

		if result.StatusCode != http.StatusOK {
			t.Fatalf("status=%d, want 200", result.StatusCode)
		}
		if adapter.callIndex != 2 {
			t.Fatalf("upstream calls=%d, want 2 — a positional walk overflows again on the way", adapter.callIndex)
		}
		served := result.Target.ProviderName
		if served != "roomy" {
			t.Errorf("served by %q, want \"roomy\" — the largest remaining window is the only "+
				"move that improves the odds on the dimension that just failed", served)
		}
		if term := result.Terminal(); term == nil || term.SelectionReason != "largest-window" {
			t.Errorf("selection reason not recorded; an operator cannot tell this jump from a bug")
		}
	})

	t.Run("a rate limit moves to a different provider", func(t *testing.T) {
		// Plans built from one provider's catalogue list that provider's
		// models together, so the next entry usually shares the quota that
		// was just exhausted.
		sameProvider := target("busy")
		sibling := target("busy")
		sibling.ModelID = "model-2"
		other := target("quiet")

		adapter := &mockAdapter{format: mockFormat, responses: []scripted{
			{err: &provcore.ProviderError{Code: provcore.CodeRateLimited, Status: 429, Message: "rate limit"}},
			{resp: &provcore.Response{StatusCode: 200, Body: []byte(`served`)}},
		}}
		exec := New(newRegistry(t, adapter), okResolver(), nil, nil)

		policy := configtypes.DefaultRetryPolicy()
		policy.MaxAttemptsPerTarget = 1 // isolate the across-target choice
		result := exec.Execute(context.Background(),
			[]routingcore.RoutingTarget{sameProvider, sibling, other}, baseReq(), policy)

		if result.StatusCode != http.StatusOK {
			t.Fatalf("status=%d, want 200", result.StatusCode)
		}
		if got := result.Target.ProviderName; got != "quiet" {
			t.Errorf("served by %q, want \"quiet\" — a quota is per-provider, and the sibling "+
				"model shares it", got)
		}
		if term := result.Terminal(); term == nil || term.SelectionReason != "different-provider" {
			t.Errorf("selection reason not recorded")
		}
	})
}

// TestExecutor_RestingRequiresAnExplicitBudget.
//
// A target that failed transiently goes to the back of the list rather than out
// of it, and comes round again if room remains. What that buys is elapsed time:
// an in-place retry happens in milliseconds, while coming back after trying
// another provider means several upstream round-trips have passed — which is
// what a rate limit actually needs.
//
// But it may only happen when the budget was set deliberately. The derived
// default exists to reproduce the reach the walk already had, so extra turns
// under it would break that promise — and the only way slack appears there is
// when a target failed BEFORE dispatching and therefore spent nothing, which is
// precisely when another turn is least deserved.
func TestExecutor_RestingCannotSpendWhatItWasNotAllotted(t *testing.T) {
	script := func() []scripted {
		s := make([]scripted, 8)
		for i := range s {
			s[i] = scripted{err: &provcore.ProviderError{Code: provcore.CodeRateLimited, Status: 429, Message: "slow down"}}
		}
		return s
	}

	// The case that actually distinguishes the gate. Under the derived default,
	// slack appears only when a target failed before dispatching and so spent
	// nothing from the budget — here the second target has no adapter. Without
	// the gate the walk would notice the leftover room and hand the first
	// target another turn, which is a retry nobody configured, bought with
	// budget a broken target failed to spend.
	t.Run("slack left by a pre-dispatch failure buys nobody a second turn", func(t *testing.T) {
		adapter := &mockAdapter{format: mockFormat, responses: script()}
		unroutable := target("no-credential")
		exec := New(newRegistry(t, adapter),
			&failForProvider{inner: okResolver(), deny: unroutable.ProviderID}, nil, nil)

		policy := fastBackoffPolicy(1) // one attempt per target, no explicit budget
		exec.Execute(context.Background(),
			[]routingcore.RoutingTarget{target("a"), unroutable}, baseReq(), policy)

		if adapter.callIndex != 1 {
			t.Errorf("upstream calls=%d, want 1 — the derived default must reproduce the "+
				"reach the walk already had, and a target that never dispatched has not "+
				"earned anyone a retry", adapter.callIndex)
		}
	})

	// The combination that makes "must have dispatched" load-bearing: budget
	// explicitly raised, and a target that fails before reaching an upstream.
	// Such a target spends nothing, so resting it never consumes the budget
	// that would eventually stop the walk — it just comes round forever. The
	// test times out rather than fails if that guard is removed, which is the
	// honest signal for a loop that cannot end.
	t.Run("a target that never dispatches is not rested, however much budget remains", func(t *testing.T) {
		adapter := &mockAdapter{format: mockFormat, responses: script()}
		broken := target("no-credential")
		exec := New(newRegistry(t, adapter),
			&failForProvider{inner: okResolver(), deny: broken.ProviderID}, nil, nil)

		policy := fastBackoffPolicy(6)
		policy.MaxUpstreamCalls = 6

		done := make(chan int, 1)
		go func() {
			exec.Execute(context.Background(),
				[]routingcore.RoutingTarget{broken, target("a")}, baseReq(), policy)
			done <- adapter.callIndex
		}()

		select {
		case calls := <-done:
			// The reachable target rests and comes back until IT runs out —
			// five, the clamp on a per-target allowance — and then the walk
			// ends. It does not go on to spend the sixth call the request
			// budget still holds, because that call was never its to spend.
			if calls != 5 {
				t.Errorf("upstream calls=%d, want 5 — a target rests against its OWN allowance, "+
					"and the request budget having room left over does not extend it", calls)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the walk never ended: a target that cannot dispatch was rested and re-selected " +
				"forever, spending no budget and so never reaching the ceiling that would stop it")
		}
	})

	// The knobs are ordered, and the per-target one comes first. A request
	// budget above what the targets can collectively absorb buys nothing: each
	// target's allowance is its own and no other target can spend it.
	t.Run("a budget above the targets' collective allowance buys nothing", func(t *testing.T) {
		adapter := &mockAdapter{format: mockFormat, responses: script()}
		exec := New(newRegistry(t, adapter), okResolver(), nil, nil)

		policy := fastBackoffPolicy(1)
		policy.MaxUpstreamCalls = 5 // the admin asked for more than the targets can take

		exec.Execute(context.Background(),
			[]routingcore.RoutingTarget{target("a"), target("b")}, baseReq(), policy)

		if adapter.callIndex != 2 {
			t.Errorf("upstream calls=%d, want 2 — two targets at one dispatch each is the "+
				"whole of what they may take, and raising the REQUEST ceiling must not "+
				"hand either of them an attempt the admin capped", adapter.callIndex)
		}
	})
}

// TestExecutor_CredentialStatsExcludeFailuresTheKeyDidNotCause.
//
// The credential circuit breaker counts failures per key and opens the circuit
// at a threshold, which stops that key being used at all. It reads raw status
// codes and derives its own vocabulary from them, so it cannot tell a client
// that hung up (499) from an ordinary client error, or our own decode failure
// (502) from an upstream fault. Filtering at the source is what keeps the two
// classifiers from disagreeing.
//
// Both withheld classes describe something the key did not do. A cancelled
// caller says nothing about the key that was about to be used; a local decode
// failure happened after the upstream had already accepted the request WITH
// that key.
func TestExecutor_CredentialStatsExcludeFailuresTheKeyDidNotCause(t *testing.T) {
	for _, tc := range []struct {
		name string
		code string
	}{
		{"a caller that hung up", provcore.CodeClientGone},
		{"our own decode failure", provcore.CodeLocalProcessing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			adapter := &mockAdapter{format: mockFormat, responses: []scripted{
				{err: &provcore.ProviderError{Code: tc.code, Status: 502, Message: "boom"}},
			}}
			rec := &recordingStats{}
			exec := New(newRegistry(t, adapter), resolverWithCredential(), nil, nil).WithStats(rec)

			req := baseReq()
			req.WireShape = typology.WireShapeOpenAIImages // stops the walk for local-processing
			exec.Execute(context.Background(),
				[]routingcore.RoutingTarget{target("a")}, req, configtypes.DefaultRetryPolicy())

			if got := rec.failuresCharged(); got != 0 {
				t.Errorf("the credential was charged %d failure(s) it did not cause — enough of "+
					"these open its circuit and the key stops being used at all", got)
			}
		})
	}

	// The breaker must still see what IS the key's doing, or withholding would
	// have disabled it rather than corrected it.
	t.Run("a rejected credential is still counted", func(t *testing.T) {
		adapter := &mockAdapter{format: mockFormat, responses: []scripted{
			{err: &provcore.ProviderError{Code: provcore.CodeAuthFailed, Status: 401, Message: "bad key"}},
		}}
		rec := &recordingStats{}
		exec := New(newRegistry(t, adapter), resolverWithCredential(), nil, nil).WithStats(rec)

		exec.Execute(context.Background(),
			[]routingcore.RoutingTarget{target("a")}, baseReq(), configtypes.DefaultRetryPolicy())

		if got := rec.failuresCharged(); got != 1 {
			t.Errorf("charged %d, want 1 — an authentication failure is exactly what this counter is for", got)
		}
	})
}

// resolverWithCredential names a credential, without which the stats recorder
// short-circuits and the test would pass for the wrong reason.
func resolverWithCredential() *mockResolver {
	r := okResolver()
	r.target.CredentialID = "cred-1"
	return r
}

// recordingStats captures what the credential circuit breaker is told. The
// status matters as well as the count: the breaker resets on success, so a
// recorded 200 is not a charge against the key.
type recordingStats struct {
	calls    int
	statuses []int
}

func (r *recordingStats) RecordAttempt(_ string, status int, _ string) {
	r.calls++
	r.statuses = append(r.statuses, status)
}

// failuresCharged counts what the breaker would hold against the key.
func (r *recordingStats) failuresCharged() int {
	n := 0
	for _, s := range r.statuses {
		if s < 200 || s >= 300 {
			n++
		}
	}
	return n
}

// TestExecutor_AModelPermissionDenialDoesNotCostTheProvider.
//
// Every normalizer folds 401 and 403 into one code, but they scope differently.
// A 401 means the key is not accepted, and every target on that provider shares
// it. A 403 means the key is fine and THIS MODEL is not licensed to it — an
// unbought tier, a preview nobody enabled.
//
// Treating the second as the first costs twice: the provider's working models
// are skipped for this request, and the failure feeds the counter that opens
// the key's circuit. Enough of them and every model on that key stops resolving
// for everyone using it — one misconfigured rule pointing at a model the org
// never licensed, taking down the models it did.
func TestExecutor_AModelPermissionDenialDoesNotCostTheProvider(t *testing.T) {
	// Two models on ONE provider: the first unlicensed, the second fine.
	unlicensed := target("acme")
	licensed := target("acme")
	licensed.ModelID = "model-2"

	adapter := &mockAdapter{format: mockFormat, responses: []scripted{
		{err: &provcore.ProviderError{Code: provcore.CodeAuthFailed, Status: 403, Message: "model not available on your plan"}},
		{resp: &provcore.Response{StatusCode: 200, Body: []byte(`served`)}},
	}}
	rec := &recordingStats{}
	exec := New(newRegistry(t, adapter), resolverWithCredential(), nil, nil).WithStats(rec)

	result := exec.Execute(context.Background(),
		[]routingcore.RoutingTarget{unlicensed, licensed}, baseReq(), configtypes.DefaultRetryPolicy())

	if result.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200 — the provider's licensed model was skipped over a "+
			"permission scoped to a different one", result.StatusCode)
	}
	if got := rec.failuresCharged(); got != 0 {
		t.Errorf("the credential was charged %d failure(s) for a model it is simply not "+
			"licensed for; enough of those open its circuit (recorded: %v)", got, rec.statuses)
	}

	// A real 401 must still cost the provider, or the split would have traded
	// one wrong answer for another.
	t.Run("a rejected key still eliminates the provider", func(t *testing.T) {
		adapter := &mockAdapter{format: mockFormat, responses: []scripted{
			{err: &provcore.ProviderError{Code: provcore.CodeAuthFailed, Status: 401, Message: "invalid key"}},
			{resp: &provcore.Response{StatusCode: 200, Body: []byte(`must not be reached`)}},
		}}
		exec := New(newRegistry(t, adapter), resolverWithCredential(), nil, nil)
		exec.Execute(context.Background(),
			[]routingcore.RoutingTarget{unlicensed, licensed}, baseReq(), configtypes.DefaultRetryPolicy())
		if adapter.callIndex != 1 {
			t.Errorf("upstream calls=%d, want 1 — a rejected key is shared by every target on "+
				"the provider, so trying the next one spends a call to be told the same thing", adapter.callIndex)
		}
	})
}

// TestExecutor_ExhaustionReportsTheLastAttempt.
//
// When nothing serves, WHICH failure gets to speak decides both whether the
// caller can recover and whether the operator looks in the right place.
//
// Keeping the last TERMINAL-class failure instead of the last attempt reads
// plausibly and is wrong twice over. An early prompt overflow would surface as
// a 400 when the walk actually ended on a rate limit: no SDK retries a 400, so
// a request that would have succeeded after a backoff fails permanently, and it
// fails with a false explanation — the prompt was fine, the second provider was
// merely busy. And the audit row takes its credential from the last dispatched
// attempt regardless, so the row would name one provider's error beside another
// provider's key.
func TestExecutor_ExhaustionReportsTheLastAttempt(t *testing.T) {
	t.Run("a later rate limit is not masked by an earlier overflow", func(t *testing.T) {
		small := target("small-window")
		small.MaxContextTokens = 8_000
		busy := target("busy")
		busy.MaxContextTokens = 200_000

		adapter := &mockAdapter{format: mockFormat, responses: []scripted{
			{err: &provcore.ProviderError{Code: provcore.CodeContextOverflow, Status: 400, Message: "context length exceeded"}},
			{err: &provcore.ProviderError{Code: provcore.CodeRateLimited, Status: 429, Message: "slow down"}},
		}}
		exec := New(newRegistry(t, adapter), okResolver(), nil, nil)

		result := exec.Execute(context.Background(),
			[]routingcore.RoutingTarget{small, busy}, baseReq(), fastBackoffPolicy(1))

		// A retryable failure carries no envelope, so exhaustion is reported as
		// such and the handler maps it from the terminal attempt — which is how
		// a rate limit reaches the caller as a 429 they can back off from.
		// Handing back the earlier 400 would end the request permanently, for a
		// reason that was never true.
		if result.StatusCode == http.StatusBadRequest {
			t.Fatalf("reported 400 context-overflow: the caller is told their prompt is too " +
				"long when the walk ended on a rate limit, and no SDK retries a 400")
		}
		if !errors.Is(result.Error, ErrAllTargetsExhausted) {
			t.Fatalf("error=%v, want all-targets-exhausted so the handler maps the terminal attempt", result.Error)
		}
		if term := result.Terminal(); term == nil || term.Code != provcore.CodeRateLimited {
			t.Fatalf("terminal attempt %+v, want the rate limit the walk actually ended on", term)
		}
	})

	t.Run("what is reported describes the attempt the row attributes", func(t *testing.T) {
		adapter := &mockAdapter{format: mockFormat, responses: []scripted{
			{err: &provcore.ProviderError{Code: provcore.CodeAuthFailed, Status: 401, Message: "expired"}},
			{err: &provcore.ProviderError{Code: provcore.CodeRateLimited, Status: 429, Message: "slow down"}},
		}}
		exec := New(newRegistry(t, adapter), okResolver(), nil, nil)

		result := exec.Execute(context.Background(),
			[]routingcore.RoutingTarget{target("expired-key"), target("busy")},
			baseReq(), fastBackoffPolicy(1))

		term := result.Terminal()
		if term == nil {
			t.Fatal("no dispatched attempt recorded")
		}
		if term.Target.ProviderName != "busy" {
			t.Fatalf("attributed to %q, want the target that ran last", term.Target.ProviderName)
		}
		if result.ProviderError != nil && result.ProviderError.Code == provcore.CodeAuthFailed {
			t.Error("the earlier expired key was reported while the walk ended on a rate limit " +
				"against a different provider — the row would pair one provider's error with " +
				"another provider's credential, and the wrong key gets rotated")
		}
	})
}

// TestExecutor_ATargetThatStopsResolvingDoesNotLoopForever.
//
// The sequence is ordinary: a target is rate-limited, which opens its
// credential's circuit, so the next resolve for that key fails. The target has
// dispatched once and its failure was retryable, so the rested pass wants it
// back — but a resolve failure costs no upstream call, so nothing advances the
// budget that is supposed to end the walk. It comes round forever.
//
// Every pre-dispatch failure therefore eliminates its target. That guard was
// written for this and had no test: deleting it left the suite green while the
// walk ran until the ten-minute deadline, and policies that zero that deadline
// out have no bound at all.
func TestExecutor_ATargetThatStopsResolvingDoesNotLoopForever(t *testing.T) {
	adapter := &mockAdapter{format: mockFormat, responses: []scripted{
		{err: &provcore.ProviderError{Code: provcore.CodeRateLimited, Status: 429, Message: "slow down"}},
	}}
	// Resolves once — the rate-limited dispatch — and never again.
	exec := New(newRegistry(t, adapter), &failAfterN{inner: okResolver(), n: 1}, nil, nil)

	policy := fastBackoffPolicy(1)
	policy.MaxUpstreamCalls = 8 // resting enabled, and room to loop in
	policy.MaxWalkDuration = 0  // no deadline to hide behind

	done := make(chan *ExecutionResult, 1)
	go func() {
		done <- exec.Execute(context.Background(),
			[]routingcore.RoutingTarget{target("rate-limited")}, baseReq(), policy)
	}()

	select {
	case result := <-done:
		if adapter.callIndex != 1 {
			t.Errorf("upstream calls=%d, want 1", adapter.callIndex)
		}
		if len(result.Attempts) > 3 {
			t.Errorf("recorded %d attempts for one target — it was re-selected repeatedly, "+
				"each round costing a backoff and spending no budget", len(result.Attempts))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the walk never ended: a target whose credential stopped resolving was rested " +
			"and re-selected forever, spending no budget and so never reaching the ceiling " +
			"that would have stopped it")
	}
}

// TestExecutor_RestingHonoursRetryOn.
//
// RetryOn is the admin saying which failures deserve another attempt. Resting a
// target and coming round to it IS another attempt, so a class excluded there
// must not reach one through this door — otherwise raising the budget quietly
// grants the retry the configuration refused.
//
// Mutation-verified: removing the check left every existing test green.
func TestExecutor_RestingHonoursRetryOn(t *testing.T) {
	responses := make([]scripted, 8)
	for i := range responses {
		responses[i] = scripted{err: &provcore.ProviderError{Code: provcore.CodeRateLimited, Status: 429, Message: "slow down"}}
	}
	adapter := &mockAdapter{format: mockFormat, responses: responses}
	exec := New(newRegistry(t, adapter), okResolver(), nil, nil)

	policy := fastBackoffPolicy(1)
	policy.MaxUpstreamCalls = 5                                          // resting enabled and funded
	policy.RetryOn = []configtypes.ErrorClass{configtypes.ErrorClass5xx} // …but not for rate limits

	exec.Execute(context.Background(),
		[]routingcore.RoutingTarget{target("a")}, baseReq(), policy)

	if adapter.callIndex != 1 {
		t.Errorf("upstream calls=%d, want 1 — the policy excludes 429 from retry, and resting "+
			"must not be a second way of granting it", adapter.callIndex)
	}
}

// TestExecutor_RestingActuallyRests.
//
// Coming round to a target that just failed is only worth doing because time
// has passed — that is the whole argument for resting rather than dropping it.
// Without a pause the walk ping-pongs between two struggling providers at full
// speed, adding load to exactly the upstreams that asked to be left alone.
//
// Mutation-verified: deleting the backoff left every existing test green, so
// "the rest has to be real" was a comment and nothing more.
func TestExecutor_RestingActuallyRests(t *testing.T) {
	responses := make([]scripted, 6)
	for i := range responses {
		responses[i] = scripted{err: &provcore.ProviderError{Code: provcore.CodeRateLimited, Status: 429, Message: "slow down"}}
	}
	adapter := &mockAdapter{format: mockFormat, responses: responses}
	exec := New(newRegistry(t, adapter), okResolver(), nil, nil)

	policy := fastBackoffPolicy(3) // one fresh dispatch, then two rested returns
	policy.MaxUpstreamCalls = 3
	policy.BackoffInitial = 40 * time.Millisecond
	policy.BackoffMax = 40 * time.Millisecond
	policy.BackoffJitter = 0

	start := time.Now()
	exec.Execute(context.Background(),
		[]routingcore.RoutingTarget{target("a")}, baseReq(), policy)
	elapsed := time.Since(start)

	if adapter.callIndex != 3 {
		t.Fatalf("upstream calls=%d, want 3", adapter.callIndex)
	}
	if elapsed < 60*time.Millisecond {
		t.Errorf("the walk took %s for two rested returns at 40ms each — the target was "+
			"re-dispatched with no pause, which is the ping-pong resting exists to avoid", elapsed)
	}
}

// TestExecutor_ARetryWithNoCredentialEliminatesTheTarget.
//
// The sequence closes on itself: a rate limit opens the credential's circuit,
// so when the retry asks the pool for a fresh credential for the SAME target,
// there is none. That target cannot be dispatched to any more — the same
// condition as a resolve failure before the walk began, and it gets the same
// treatment.
//
// Leaving it merely deprioritised is what made resting need a gate: the target
// stayed eligible, came round again, failed to resolve again, and spent nothing
// each time — so the budget that is supposed to end the walk never advanced.
func TestExecutor_ARetryWithNoCredentialEliminatesTheTarget(t *testing.T) {
	responses := make([]scripted, 6)
	for i := range responses {
		responses[i] = scripted{err: &provcore.ProviderError{Code: provcore.CodeRateLimited, Status: 429, Message: "slow down"}}
	}
	adapter := &mockAdapter{format: mockFormat, responses: responses}
	// Resolves for the first dispatch; the circuit the 429 opens closes it after.
	exec := New(newRegistry(t, adapter), &failAfterN{inner: okResolver(), n: 1}, nil, nil)

	policy := fastBackoffPolicy(2) // two attempts per target: the retry re-resolves
	policy.MaxUpstreamCalls = 6    // resting funded, so an un-eliminated target would return

	result := exec.Execute(context.Background(),
		[]routingcore.RoutingTarget{target("rate-limited")}, baseReq(), policy)

	if adapter.callIndex != 1 {
		t.Fatalf("upstream calls=%d, want 1 — a target with no usable credential was dispatched again", adapter.callIndex)
	}
	// Two records: the 429, and the retry that found no credential. A third
	// would mean the target came round despite being undispatchable.
	if len(result.Attempts) != 2 {
		t.Errorf("recorded %d attempts, want 2 — the target was re-selected after it had "+
			"already proved it cannot be reached, each round spending no budget", len(result.Attempts))
	}
}

// ruleTarget builds a target belonging to a named rule.
//
// It sets ModelID explicitly: the shared target() helper hard-codes it to a
// constant, so a test asserting on the model that served would compare two
// identical strings and pass whatever the walk did.
func ruleTarget(rule, model string) routingcore.RoutingTarget {
	t := target(model)
	t.RuleID = rule
	t.ModelID = model
	return t
}

// TestExecutor_ATransientFailureDoesNotCrossARuleBoundary.
//
// A rule an admin wrote at lower priority is the alternative for when the rule
// above it CANNOT serve the request — not for when it hiccups. The two look
// identical to a flat list of targets, which is why the boundary has to be
// carried: on a 503 the walk must exhaust the primary rule's own targets before
// anything below it is reachable.
//
// Getting this wrong is quiet. The request succeeds, the client sees nothing
// unusual, and the traffic row names the primary rule while the answer came
// from a model that rule never mentions — so a routing policy an admin
// configured is bypassed by the first blip and nobody is told.
func TestExecutor_ATransientFailureDoesNotCrossARuleBoundary(t *testing.T) {
	adapter := &mockAdapter{format: mockFormat, responses: []scripted{
		{err: &provcore.ProviderError{Code: provcore.CodeUpstreamError, Status: 503, Message: "overloaded"}},
		{resp: &provcore.Response{StatusCode: 200, Body: []byte(`served`)}},
	}}
	exec := New(newRegistry(t, adapter), okResolver(), nil, nil)

	// Both of rule-a's targets sit on ONE provider, and rule-b's is elsewhere.
	// That is the shape that tells the boundary apart from the rest of
	// selection: a 503 sends the walk looking for a DIFFERENT provider, and
	// without the boundary the only different provider is rule-b's. A test
	// whose three targets are on three providers passes either way, because
	// "different provider" happens to land on the right answer by accident.
	sameProvider := func(model string) routingcore.RoutingTarget {
		t := ruleTarget("rule-a", model)
		t.ProviderID, t.ProviderName = "prov-shared", "shared"
		return t
	}
	result := exec.Execute(context.Background(), []routingcore.RoutingTarget{
		sameProvider("a-first"),
		sameProvider("a-second"),
		ruleTarget("rule-b", "b-only"),
	}, baseReq(), fastBackoffPolicy(1))

	if result.Target.ModelID != "a-second" {
		t.Errorf("served by %q — a 503 on the primary rule's first target reached past that "+
			"rule's own remaining answer; the traffic row still names rule-a while a rule "+
			"the admin wrote for a different situation served the request",
			result.Target.ModelID)
	}
}

// TestExecutor_TheNextRuleIsReachedOnceThePrimaryRuleIsRuledOut.
//
// The other direction. Advancement has to actually happen, or a lower-priority
// rule is decoration: when every target of the rule above has been ELIMINATED —
// a permission the key lacks, a window that cannot hold the prompt, an endpoint
// the target does not serve — the request is not servable by that rule at all,
// and the rule below is exactly what an admin wrote it for.
//
// Elimination, never budget. A rule whose targets merely cost attempts has not
// been ruled out; advancing past it because the call budget ran low hands the
// request to a different rule while the intended one still had answers.
func TestExecutor_TheNextRuleIsReachedOnceThePrimaryRuleIsRuledOut(t *testing.T) {
	// 403 eliminates a target: the key is not permitted on it, and retrying
	// produces the same answer.
	denied := scripted{err: &provcore.ProviderError{
		Code: provcore.CodeAuthFailed, Status: 403, Message: "not permitted"}}
	adapter := &mockAdapter{format: mockFormat, responses: []scripted{
		denied, denied,
		{resp: &provcore.Response{StatusCode: 200, Body: []byte(`served by the next rule`)}},
	}}
	exec := New(newRegistry(t, adapter), okResolver(), nil, nil)

	result := exec.Execute(context.Background(), []routingcore.RoutingTarget{
		ruleTarget("rule-a", "a-first"),
		ruleTarget("rule-a", "a-second"),
		ruleTarget("rule-b", "b-only"),
	}, baseReq(), fastBackoffPolicy(1))

	if result.Target.ModelID != "b-only" {
		t.Errorf("served by %q — every target of the primary rule was ruled out and the rule "+
			"below it never ran, so the request failed on a plan that still had an answer "+
			"the admin wrote down", result.Target.ModelID)
	}
	if result.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", result.StatusCode)
	}
}

// TestExecutor_TheCallBudgetSpansEveryRuleNotEachOne.
//
// The budget is a ceiling on upstream calls for the REQUEST. A per-rule reading
// multiplies it by however many rules happen to match — an admin who wrote "at
// most three upstream calls" gets three per rule, and the bill and the latency
// both scale with a number they never configured and cannot see.
//
// The sibling budget test walks one rule, so it cannot tell the two readings
// apart: with a single rule, per-rule and per-request are the same number. This
// one puts a second rule below the first and gives the budget less room than
// the two rules hold.
func TestExecutor_TheCallBudgetSpansEveryRuleNotEachOne(t *testing.T) {
	// 403 ELIMINATES a target, so rule-a is genuinely ruled out and the walk
	// crosses into rule-b — which is the only way this test can see whether
	// the budget crossed with it. A transient failure would leave rule-a's
	// targets merely tired, the walk would never advance, and the test would
	// pass without the boundary ever being crossed.
	denied := scripted{err: &provcore.ProviderError{
		Code: provcore.CodeAuthFailed, Status: 403, Message: "not permitted"}}
	fail := scripted{err: &provcore.ProviderError{
		Code: provcore.CodeUpstreamError, Status: 503, Message: "overloaded"}}
	adapter := &mockAdapter{format: mockFormat, responses: []scripted{
		denied, denied, fail, fail, fail, fail}}
	exec := New(newRegistry(t, adapter), okResolver(), nil, nil)

	policy := fastBackoffPolicy(1)
	policy.MaxUpstreamCalls = 3

	exec.Execute(context.Background(), []routingcore.RoutingTarget{
		ruleTarget("rule-a", "a-first"),
		ruleTarget("rule-a", "a-second"),
		ruleTarget("rule-b", "b-first"),
		ruleTarget("rule-b", "b-second"),
	}, baseReq(), policy)

	// Two calls ruled rule-a out, the third reached rule-b and failed, and the
	// ceiling stopped the walk there. Read per rule, rule-b would start over
	// with three of its own.
	if adapter.callIndex != 3 {
		t.Errorf("upstream calls = %d, want 3 — the budget was read per rule, so an admin's "+
			"ceiling was multiplied by however many rules happened to match this request",
			adapter.callIndex)
	}
}

// TestExecute_AnUnrecognisedProviderErrorIsStampedAsUnclassified is where the
// class and the retry bucket come apart on a real walk.
//
// A 5xx cannot see this: the class and the bucket are both spelled "5xx", so
// an attempt stamped with either one passes. Only a failure whose class has no
// retry-policy spelling of its own distinguishes them, and stamping the bucket
// is what put "network" on rows where nothing touched the network.
func TestExecute_AnUnrecognisedProviderErrorIsNamedApartFromANetworkFault(t *testing.T) {
	adapter := &mockAdapter{format: mockFormat, responses: []scripted{
		{err: &provcore.ProviderError{Status: 418, Code: "teapot", Message: "short and stout"}},
		{resp: &provcore.Response{StatusCode: 200, Body: []byte(`ok`)}},
	}}
	exec := New(newRegistry(t, adapter), okResolver(), nil, nil)

	result := exec.Execute(context.Background(),
		[]routingcore.RoutingTarget{target(providerSlug)},
		baseReq(),
		fastBackoffPolicy(2),
	)

	if len(result.Attempts) < 1 {
		t.Fatalf("no attempts recorded")
	}
	if got := result.Attempts[0].ErrorClass; got != "unknown_provider_code" {
		t.Fatalf("ErrorClass = %q, want \"unknown_provider_code\" — this failure is RETRIED as "+
			"network, and stamping the retry bucket sends whoever reads the row to look at a "+
			"network that was never involved", got)
	}
}

// failingTargets scripts n identical 5xx replies so a walk runs until one of
// its ceilings stops it rather than until the script does.
func failingTargets(t *testing.T, n int) *mockAdapter {
	t.Helper()
	a := &mockAdapter{format: mockFormat}
	for range n {
		a.responses = append(a.responses, scripted{
			err: &provcore.ProviderError{Status: 500, Code: provcore.CodeUpstreamError, Message: "boom"},
		})
	}
	return a
}

// rateLimitedTargets scripts n rate limits. It is the counterpart to
// failingTargets: a 5xx is retried in place, a rate limit is handed back to the
// walk, and which mechanism runs is exactly what the class decides.
func rateLimitedTargets(t *testing.T, n int) *mockAdapter {
	t.Helper()
	a := &mockAdapter{format: mockFormat}
	for range n {
		a.responses = append(a.responses, scripted{
			err: &provcore.ProviderError{Status: 429, Code: provcore.CodeRateLimited, Message: "slow down"},
		})
	}
	return a
}

func selectionReasons(res *ExecutionResult) []string {
	out := make([]string, 0, len(res.Attempts))
	for _, a := range res.Attempts {
		out = append(out, a.SelectionReason)
	}
	return out
}

// TestExecute_TheFreshPassSpendsTheDerivedBudgetExactly is the second of the
// two independent reasons the rested pass never runs on a default deployment.
//
// The first is the flag — resting is gated on an explicitly configured budget,
// which `TestExecutor_RestingRequiresAnExplicitBudget` holds. This is the other
// one, and it holds on its own: the derived budget is targets ×
// attempts-per-target, which is precisely what one pass over every target
// spends, so there is no call left to come back with even if the flag said yes.
//
// Asserted as budget arithmetic rather than as "no target came round again",
// because that phrasing is true for two reasons at once and so cannot be made
// false by breaking either — a gate no single mistake can turn red is not
// watching either of them. Widen the derived budget and this fails; the rested
// pass silently arriving in the default deployment is what it is protecting
// against, since the pass costs a second full allowance per target.
func TestExecute_TheFreshPassSpendsTheDerivedBudgetExactly(t *testing.T) {
	targets := []routingcore.RoutingTarget{target("a"), target("b")}
	pol := fastBackoffPolicy(2)

	budget := configtypes.EffectiveCallBudget(pol, len(targets))
	want := len(targets) * configtypes.ClampMaxAttempts(pol.MaxAttemptsPerTarget)
	if budget != want {
		t.Fatalf("derived budget = %d, want %d (targets × attempts-per-target). Slack here is "+
			"what the rested pass spends, and it arrives without anyone configuring it", budget, want)
	}

	exec := New(newRegistry(t, failingTargets(t, 20)), okResolver(), nil, nil)
	res := exec.Execute(context.Background(), targets, baseReq(), pol)
	dispatched := 0
	for _, a := range res.Attempts {
		if a.Dispatched {
			dispatched++
		}
	}
	if dispatched != budget {
		t.Errorf("a walk over failing targets dispatched %d times against a budget of %d — the "+
			"arithmetic above only means something if the walk actually spends it", dispatched, budget)
	}
	for _, r := range selectionReasons(res) {
		if r == "deprioritised-retry" {
			t.Errorf("a target came round again on the shipped default: %v", selectionReasons(res))
		}
	}
}

// TestExecutor_ThePerTargetLimitBoundsTheREQUEST.
//
// Two mechanisms answer "try this target again" — the retry loop inside a turn
// and the walk coming back to a rested target — and they answer different
// questions, so both exist. What must not be duplicated is the BOUND. With the
// limit living inside the retry loop, every turn the walk granted handed the
// target a fresh allowance, so a rule capped at two attempts per target
// dispatched FOUR times to one provider and the request budget silently
// outranked the per-target knob.
//
// The allowance is now the target's for the whole request. Raising
// maxUpstreamCalls buys more of the walk, never more of one target — which also
// means a budget above targets x attempts-per-target is unreachable, and that
// is the honest reading of two knobs where one is a per-target cap.
func TestExecutor_ThePerTargetLimitBoundsTheREQUEST(t *testing.T) {
	pol := fastBackoffPolicy(2)
	pol.MaxUpstreamCalls = 8 // far above what two targets at two attempts can take
	// Rate limits, because they are the class the walk's half owns: a 5xx is
	// retried in place and never leaves the turn, so it would exercise only one
	// of the two mechanisms this test is about.
	exec := New(newRegistry(t, rateLimitedTargets(t, 20)), okResolver(), nil, nil)
	res := exec.Execute(context.Background(),
		[]routingcore.RoutingTarget{target("a"), target("b")}, baseReq(), pol)

	rested := 0
	perTarget := map[string]int{}
	dispatched := 0
	for _, a := range res.Attempts {
		if a.SelectionReason == "deprioritised-retry" {
			rested++
		}
		if a.Dispatched {
			dispatched++
			perTarget[a.Target.ProviderName]++
		}
	}
	// Both mechanisms are still alive: a rate limit is handed back to the walk,
	// so the second attempt on a target is a LATER TURN rather than an in-place
	// retry. A run with no return means the walk's half was deleted rather than
	// bounded.
	if rested == 0 {
		t.Fatalf("no target came round again — the walk's half of the retry answer is then "+
			"unreachable, which makes it dead code: %v", selectionReasons(res))
	}
	for name, got := range perTarget {
		if got > 2 {
			t.Errorf("provider %s was dispatched to %d times under maxAttemptsPerTarget=2 — "+
				"the per-target cap is what an admin writing that number is capping, and a "+
				"request-level budget must not be able to lift it", name, got)
		}
	}
	if dispatched != 4 {
		t.Errorf("the walk dispatched %d times; two targets at two attempts each is 4, and a "+
			"budget of 8 cannot buy a fifth because no target has one left to give", dispatched)
	}
}

// TestExecute_EveryEliminatingClassActuallyEliminates.
//
// `eliminatesTarget()` names the classes for which the target is out for this
// request. The walk must honour it for ALL of them, not for the ones an arm
// happens to handle: a class the predicate claims and the walk does not
// eliminate is selected again on the next turn, and again, until the call
// budget runs out — while `surfacesUpstreamEnvelope()`, which reads the same
// predicate, has already decided its envelope is the one the caller sees.
//
// Driven from the predicate and counted against `classCount`, so a class added
// later is covered here without anyone remembering to add it.
func TestExecute_EveryEliminatingClassActuallyEliminates(t *testing.T) {
	// One dispatchable failure per eliminating class. A class this table does
	// not name fails the count check below rather than being skipped.
	producers := map[errClass]*provcore.ProviderError{
		classAuthFailed:        {Status: 401, Code: provcore.CodeAuthFailed, Message: "bad key"},
		classPermissionDenied:  {Status: 403, Code: provcore.CodeAuthFailed, Message: "not licensed"},
		classTargetUnsupported: {Status: 404, Code: provcore.CodeEndpointUnsupported, Message: "no such endpoint"},
		classContextOverflow:   {Status: 400, Code: provcore.CodeContextOverflow, Message: "too long"},
		classLocalProcessing:   {Status: 502, Code: provcore.CodeLocalProcessing, Message: "decode"},
		classProviderQuotaExhausted: {Status: 400, Code: provcore.CodeProviderQuotaExhausted,
			Message: "You have reached your specified API usage limits."},
	}

	var eliminating []errClass
	for c := classSuccess; c < classCount; c++ {
		if c.eliminatesTarget() {
			eliminating = append(eliminating, c)
		}
	}
	if len(producers) != len(eliminating) {
		t.Fatalf("the table names %d eliminating classes but %d exist — a class was added to "+
			"eliminatesTarget() without anyone checking the walk honours it, and an unhonoured "+
			"one is selected forever", len(producers), len(eliminating))
	}

	for _, cls := range eliminating {
		t.Run(cls.name(), func(t *testing.T) {
			// Two RULES, one target each. Rule advancement is what makes
			// elimination observable: the walk may only leave a rule when
			// every one of ITS targets is eliminated, so a target that failed
			// with an eliminating class and was NOT marked keeps its rule
			// alive and the second rule is never reached.
			//
			// A single-rule plan cannot see this. An unmarked target has
			// attempts > 0, so it is not "fresh" either, and under the shipped
			// default nothing brings it back — the dispatch count is identical
			// whether or not the flag was set.
			first := target("a")
			first.RuleID = "rule-1"
			second := target("b")
			second.RuleID = "rule-2"

			adapter := &mockAdapter{format: mockFormat, responses: []scripted{
				{err: producers[cls]},
				{resp: &provcore.Response{StatusCode: 200, Body: []byte(`ok`)}},
			}}
			exec := New(newRegistry(t, adapter), okResolver(), nil, nil)
			res := exec.Execute(context.Background(),
				[]routingcore.RoutingTarget{first, second}, baseReq(), fastBackoffPolicy(2))

			reached := false
			for _, a := range res.Attempts {
				if a.Dispatched && a.Target.ProviderName == "b" {
					reached = true
				}
			}
			// classLocalProcessing on a fresh-output endpoint stops the walk by
			// design; this fixture is a chat request, so it must advance.
			if !reached {
				t.Fatalf("the walk never reached the second rule after a %s failure on the "+
					"first: the class eliminates the target, and a target left unmarked keeps "+
					"its rule alive forever. Attempts: %+v", cls.name(), res.Attempts)
			}
		})
	}
}

// TestExecute_OurOwnFailuresAreNotChargedToTheProvider.
//
// The health tracker marks a provider unavailable at an error rate, and the
// traffic then routed away belongs to everyone using it. So a failure that
// happened on OUR side of the call, or on the caller's, must not count: the
// upstream answered and billed before we failed to decode it, and a malformed
// body is the caller's.
//
// Observed through a real tracker's sample count, because the predicate alone
// proves nothing about whether the walk consults it — the arm that surfaces the
// upstream envelope used to call recordHealth unconditionally, and every class
// landing there was counted regardless of who had failed.
func TestExecute_OurOwnFailuresAreNotChargedToTheProvider(t *testing.T) {
	for _, tc := range []struct {
		name    string
		perr    *provcore.ProviderError
		counted bool
		why     string
	}{
		{"our decode fault", &provcore.ProviderError{Status: 502, Code: provcore.CodeLocalProcessing,
			Message: "decode"}, false,
			"the upstream answered and billed, so it worked; the failure is ours downstream of it"},
		{"the caller's malformed body", &provcore.ProviderError{Status: 400,
			Code: provcore.CodeInvalidRequest, Message: "bad"}, false,
			"one bad client would otherwise take a provider out for everyone"},
		{"the provider refusing this model", &provcore.ProviderError{Status: 403,
			Code: provcore.CodeAuthFailed, Message: "not licensed"}, true,
			"a 403 is the provider refusing, and it counts — even though the key is fine and " +
				"the credential is not charged"},
		{"an upstream fault", &provcore.ProviderError{Status: 500,
			Code: provcore.CodeUpstreamError, Message: "boom"}, true,
			"the provider failed, which is exactly what the tracker is for"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			health := store.NewHealthTracker()
			defer health.Stop()

			adapter := &mockAdapter{format: mockFormat, responses: []scripted{{err: tc.perr}}}
			exec := New(newRegistry(t, adapter), okResolver(), health, nil)
			exec.Execute(context.Background(),
				[]routingcore.RoutingTarget{target("a")}, baseReq(), fastBackoffPolicy(1))

			// Samples cross a channel to a single writer goroutine, so reading
			// straight after the dispatch is a race. A later sample for a
			// different provider is a BARRIER, not a sleep: one channel, one
			// consumer, FIFO — once the barrier is visible the earlier message
			// has necessarily been processed, whether or not it produced one.
			health.RecordSuccess("prov-barrier", "barrier", 1)
			deadline := time.Now().Add(2 * time.Second)
			for health.GetHealth("prov-barrier").SampleCount == 0 {
				if time.Now().After(deadline) {
					t.Fatal("the health writer never drained the barrier sample; this test " +
						"cannot tell a withheld sample from one still in flight")
				}
				time.Sleep(time.Millisecond)
			}

			got := health.GetHealth("prov-a").SampleCount > 0
			if got != tc.counted {
				t.Fatalf("provider health recorded=%v, want %v — %s", got, tc.counted, tc.why)
			}
		})
	}
}

// selectionReasonsByOutcome splits the recorded chain by what the entry says
// happened, so a gate can assert on the two kinds separately.
func selectionReasonsByOutcome(t *testing.T, attempts []Attempt) (stopped, skipped []Attempt) {
	t.Helper()
	for _, a := range attempts {
		switch {
		case strings.HasPrefix(a.Error, "stopped: "):
			stopped = append(stopped, a)
		case strings.HasPrefix(a.Error, "skipped: "):
			skipped = append(skipped, a)
		}
	}
	return stopped, skipped
}

// TestExecutor_ACeilingStopsTheWalk_OnlyTheSelectedTargetCarriesAReason.
//
// When a ceiling ends the walk, one target had just been selected and the rest
// never were. Both land in the chain, and the difference between them is the
// whole value of the record: an operator replaying it is asking which target
// the walk was reaching for when it ran out.
//
// Stamping the selected target's reason onto every target that never got a turn
// answers that question wrongly for all but one of them, and there is nothing in
// the row to tell which one it was right about.
func TestExecutor_ACeilingStopsTheWalk_OnlyTheSelectedTargetCarriesAReason(t *testing.T) {
	responses := make([]scripted, 12)
	for i := range responses {
		responses[i] = scripted{err: errors.New("upstream down")}
	}
	adapter := &mockAdapter{format: mockFormat, responses: responses}
	exec := New(newRegistry(t, adapter), okResolver(), nil, nil)

	policy := configtypes.DefaultRetryPolicy()
	policy.MaxUpstreamCalls = 3
	policy.BackoffInitial = time.Microsecond
	policy.BackoffMax = time.Microsecond

	result := exec.Execute(context.Background(), []routingcore.RoutingTarget{
		target("a"), target("b"), target("c"), target("d"), target("e"),
	}, baseReq(), policy)

	stopped, skipped := selectionReasonsByOutcome(t, result.Attempts)
	if len(stopped) == 0 || len(skipped) == 0 {
		t.Fatalf("chain has %d stopped and %d skipped entries; the scenario needs both to "+
			"compare them (%d attempts total)", len(stopped), len(skipped), len(result.Attempts))
	}
	for _, a := range stopped {
		if a.SelectionReason == "" {
			t.Errorf("%s was selected and then stopped by a ceiling, but records no selection "+
				"reason — the one entry that CAN say why the walk was there says nothing",
				a.Target.ModelID)
		}
	}
	for _, a := range skipped {
		if a.SelectionReason != "" {
			t.Errorf("%s never got a turn yet records selection reason %q: that reason was "+
				"computed for a different target, so the row asserts a decision nobody made "+
				"about this one", a.Target.ModelID, a.SelectionReason)
		}
	}
}

// TestExecutor_ACeilingStoppingARevisitLeavesTheLastSelectionOnTheRecord.
//
// A rested target selected for another turn, stopped by the budget before it
// dispatches, is the one case the "never got a turn" filter cannot see: its
// earlier turn left the attempt count non-zero. So the walk's LAST decision —
// this target, for this reason, and then the ceiling — left no entry at all,
// and the chain ends one selection short of what the walk actually did.
func TestExecutor_ACeilingStoppingARevisitLeavesTheLastSelectionOnTheRecord(t *testing.T) {
	responses := make([]scripted, 6)
	for i := range responses {
		responses[i] = scripted{err: &provcore.ProviderError{Code: provcore.CodeRateLimited, Status: 429, Message: "slow down"}}
	}
	adapter := &mockAdapter{format: mockFormat, responses: responses}
	exec := New(newRegistry(t, adapter), okResolver(), nil, nil)

	// One target with room for three dispatches, but a budget for two. The
	// third selection is a revisit the budget stops before it dispatches.
	policy := fastBackoffPolicy(3)
	policy.MaxUpstreamCalls = 2
	policy.BackoffInitial = time.Microsecond
	policy.BackoffMax = time.Microsecond
	policy.BackoffJitter = 0

	result := exec.Execute(context.Background(),
		[]routingcore.RoutingTarget{target("a")}, baseReq(), policy)

	if adapter.callIndex != 2 {
		t.Fatalf("upstream calls=%d, want 2 — the scenario needs the budget to stop a THIRD "+
			"selection, not a dispatch", adapter.callIndex)
	}
	stopped, _ := selectionReasonsByOutcome(t, result.Attempts)
	if len(stopped) != 1 {
		t.Fatalf("chain records %d stopped entries, want 1: the revisit the budget stopped is "+
			"absent, so the walk's last decision is unaccounted for (%d attempts total)",
			len(stopped), len(result.Attempts))
	}
	if stopped[0].SelectionReason != "deprioritised-retry" {
		t.Errorf("the stopped revisit records selection reason %q, want %q — the entry exists "+
			"but does not say the walk was coming BACK to this target",
			stopped[0].SelectionReason, "deprioritised-retry")
	}
	if last := result.Attempts[len(result.Attempts)-1]; !strings.HasPrefix(last.Error, "stopped: ") {
		t.Errorf("the chain ends on %q; the last thing the walk did was select this target "+
			"and hit the ceiling, and that has to be the last thing the chain says", last.Error)
	}
}

// TestExecutor_ACeilingBeforeAnyFailureLabelsTheMetricWithNoClass.
//
// The walk deadline is read before a target is selected and again before its
// first dispatch, with credential resolution in between. A resolve that
// outlasts the deadline ends the turn with nothing having failed — so the
// error class the metric carries is the zero value.
//
// An empty Prometheus label is legal and useless: it reads as a class the
// classifier forgot to set, and it groups with nothing.
func TestExecutor_ACeilingBeforeAnyFailureLabelsTheMetricWithNoClass(t *testing.T) {
	metrics := withCapturedMetrics(t)
	adapter := &mockAdapter{format: mockFormat, responses: []scripted{
		{resp: &provcore.Response{StatusCode: 200, Body: []byte(`never reached`)}},
	}}
	res := okResolver()
	res.delay = 30 * time.Millisecond
	exec := New(newRegistry(t, adapter), res, nil, nil)

	policy := configtypes.DefaultRetryPolicy()
	policy.MaxWalkDuration = 10 * time.Millisecond
	policy.BackoffInitial = time.Microsecond
	policy.BackoffMax = time.Microsecond

	exec.Execute(context.Background(),
		[]routingcore.RoutingTarget{target("a")}, baseReq(), policy)

	if adapter.callIndex != 0 {
		t.Fatalf("upstream calls=%d, want 0 — the deadline has to expire during resolution, "+
			"before anything is dispatched, or nothing exercises the empty-class path",
			adapter.callIndex)
	}
	exhausted := 0
	for _, c := range metrics.snapshot() {
		if c.outcome != "exhausted" {
			continue
		}
		exhausted++
		if c.class == "" {
			t.Errorf("the exhausted metric carries an empty class label: the turn ended before " +
				"anything failed, and the series says that by being unlabelled rather than by " +
				"naming it")
		}
	}
	if exhausted == 0 {
		t.Fatalf("no exhausted metric was emitted; the scenario did not reach the path it gates "+
			"(emitted: %v)", metrics.snapshot())
	}
}

// TestExecutor_ACoercedFieldIsOnTheAttemptThatWasCoerced.
//
// The adapter rewrites request fields before dispatching — max_tokens renamed
// for a reasoning model, a size coerced to an aspect ratio. Until now the only
// place that said so was a response header, which reaches a caller who
// discards it: the operator asking "what did we change" hours later has the
// traffic row and nothing else, while the adapter contract says a field we
// coerced is a field we own.
//
// Per-ATTEMPT, because a coercion is per-target: the same request translated
// for two wires is rewritten differently, and a walk that ends on the third
// target was not coerced the way the first one was. And recorded on a FAILED
// dispatch, because "we rewrote this field and then it 400ed" is the shape of
// the question.
func TestExecutor_ACoercedFieldIsOnTheAttemptThatWasCoerced(t *testing.T) {
	adapter := &mockAdapter{format: mockFormat, responses: []scripted{
		{err: &provcore.ProviderError{Status: 400, Code: provcore.CodeInvalidRequest, Message: "bad"},
			coerced: []string{"max_tokens→max_completion_tokens"}},
	}}
	exec := New(newRegistry(t, adapter), okResolver(), nil, nil)

	result := exec.Execute(context.Background(),
		[]routingcore.RoutingTarget{target("a")}, baseReq(), fastBackoffPolicy(1))

	var dispatched *Attempt
	for i := range result.Attempts {
		if result.Attempts[i].Dispatched {
			dispatched = &result.Attempts[i]
			break
		}
	}
	if dispatched == nil {
		t.Fatalf("no dispatched attempt was recorded: %+v", result.Attempts)
	}
	if len(dispatched.Coerced) != 1 || dispatched.Coerced[0] != "max_tokens→max_completion_tokens" {
		t.Errorf("the attempt records coercions %v — the rewrite that preceded this 400 is "+
			"visible only in a response header the caller has already discarded, so the "+
			"traffic row cannot answer what we changed", dispatched.Coerced)
	}
}

// TestExecutor_ATransportFailureHasNoResponseToReadCoercionsFrom.
//
// A connection reset produces (nil, err) — there is no response, so there is
// nowhere for a coercion to have been reported. Reading through it to fetch one
// panics, and it panics on the failure mode the gateway survives most often.
//
// Asserted as a walk that COMPLETES and reports the transport error, because a
// panic in the dispatch path is not a failed request, it is a dead process.
func TestExecutor_ATransportFailureHasNoResponseToReadCoercionsFrom(t *testing.T) {
	adapter := &mockAdapter{format: mockFormat, responses: []scripted{
		{err: errors.New("connection reset by peer")},
	}}
	exec := New(newRegistry(t, adapter), okResolver(), nil, nil)

	result := exec.Execute(context.Background(),
		[]routingcore.RoutingTarget{target("a")}, baseReq(), fastBackoffPolicy(1))

	if result.Error == nil {
		t.Fatalf("a transport failure produced no error: %+v", result)
	}
	for _, a := range result.Attempts {
		if len(a.Coerced) != 0 {
			t.Errorf("%s records coercions %v after a dispatch that never got a response — "+
				"there was nothing to have been rewritten by", a.Target.ModelID, a.Coerced)
		}
	}
}

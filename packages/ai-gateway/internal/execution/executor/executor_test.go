package executor

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

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
	return r.resp, r.err
}

func (m *mockAdapter) Probe(_ context.Context, _ provcore.CallTarget) (*provcore.ProbeResult, error) {
	return &provcore.ProbeResult{OK: true}, nil
}

func (m *mockAdapter) PrepareBody(req provcore.Request) ([]byte, []string, error) {
	m.prepareBodyCalls++
	return req.Body, nil, nil
}

func (m *mockAdapter) ExecuteWithBody(ctx context.Context, req provcore.Request, body []byte, rewrites []string) (*provcore.Response, error) {
	m.executeWithBodyCalls++
	m.lastBody = append([]byte(nil), body...)
	m.lastRewrites = append([]string(nil), rewrites...)
	req.Body = body
	m.lastReq = req
	m.observedAttempts = append(m.observedAttempts, nexushttp.AttemptFromContext(ctx))
	if m.callIndex >= len(m.responses) {
		return nil, errors.New("no more responses")
	}
	r := m.responses[m.callIndex]
	m.callIndex++
	return r.resp, r.err
}

// mockResolver returns a scripted CallTarget or error.
type mockResolver struct {
	target provcore.CallTarget
	err    error
	calls  int
}

func (m *mockResolver) Resolve(_ context.Context, providerID, modelID string, _ provtarget.ResolveHints) (provcore.CallTarget, error) {
	m.calls++
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
		nil, nil,
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
		configtypes.DefaultRetryPolicy(),
		preparedBody, nil,
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
	// Default policy: MaxAttemptsPerTarget=1 -> first target's 500 immediately
	// L3 failovers to the second target which succeeds.
	result := exec.Execute(context.Background(), targets, baseReq(), configtypes.DefaultRetryPolicy())

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
	// One resolver call per target — the L2 retry reuses the resolved CallTarget.
	if res.calls != 1 {
		t.Fatalf("expected 1 resolver call per target, got %d", res.calls)
	}
}

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
	if len(result.Attempts) != 1 {
		t.Fatalf("expected 1 attempt, got %d", len(result.Attempts))
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
	result := exec.Execute(context.Background(), targets, baseReq(), configtypes.DefaultRetryPolicy())

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

	if !errors.Is(result.Error, ErrAllTargetsExhausted) {
		t.Fatalf("expected ErrAllTargetsExhausted, got %v", result.Error)
	}
	if len(result.Attempts) != 2 {
		t.Fatalf("expected 2 attempts, got %d", len(result.Attempts))
	}
	for _, a := range result.Attempts {
		if a.Error == "" {
			t.Fatal("expected resolver error in attempt")
		}
	}
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
	// One "retried_succeeded" emission with the last error class.
	calls := cap.snapshot()
	if len(calls) != 1 || calls[0].outcome != "retried_succeeded" || calls[0].class != string(configtypes.ErrorClassRate429) {
		t.Fatalf("expected one retried_succeeded emission with class=429; got %+v", calls)
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
	if len(result.Attempts) != 1 {
		t.Fatalf("expected 1 attempt (no second try after deadline-skip); got %d", len(result.Attempts))
	}
}

// Attempt.RetryReason is stamped per attempt with the configtypes.ErrorClass
// string. Success and terminal 4xx leave it empty.
func TestExecute_RetryReasonRecorded(t *testing.T) {
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
	if got := result.Attempts[0].RetryReason; got != string(configtypes.ErrorClass5xx) {
		t.Fatalf("Attempts[0].RetryReason: want %q got %q", configtypes.ErrorClass5xx, got)
	}
	if got := result.Attempts[1].RetryReason; got != "" {
		t.Fatalf("Attempts[1].RetryReason on success: want empty got %q", got)
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

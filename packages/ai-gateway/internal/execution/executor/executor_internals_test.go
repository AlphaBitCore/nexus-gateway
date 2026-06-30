package executor

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/canonicalbridge"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	provbuiltins "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/builtins"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	provtarget "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/target"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	configtypes "github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configtypes/policy"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// SetMetricsRecorder + WithStats — package-level wiring helpers.

// statsRecorderStub is a thread-safe StatsRecorder that records every
// invocation so tests can assert the executor called it with the right
// credential id, status code, and error string.
type statsRecorderStub struct {
	mu    sync.Mutex
	calls []statsCall
}

type statsCall struct {
	credentialID string
	statusCode   int
	errMsg       string
}

func (s *statsRecorderStub) RecordAttempt(credentialID string, statusCode int, errMsg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, statsCall{credentialID, statusCode, errMsg})
}

func (s *statsRecorderStub) snapshot() []statsCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]statsCall, len(s.calls))
	copy(out, s.calls)
	return out
}

// TestSetMetricsRecorder_SwapsAndRestores asserts the package-level swap
// hook actually replaces metricsRecord with the supplied fn, and that
// passing nil restores a no-op (does NOT panic when invoked).
func TestSetMetricsRecorder_SwapsAndRestores(t *testing.T) {
	prev := metricsRecord
	t.Cleanup(func() { metricsRecord = prev })

	var observed [3]string
	SetMetricsRecorder(func(provider, class, outcome string) {
		observed[0], observed[1], observed[2] = provider, class, outcome
	})
	metricsRecord("p1", "5xx", "retried_succeeded")
	if observed != [3]string{"p1", "5xx", "retried_succeeded"} {
		t.Fatalf("custom recorder not wired; observed=%v", observed)
	}

	// nil must install the no-op variant, NOT preserve the previous fn.
	// Hitting the no-op must not crash and must not mutate observed.
	SetMetricsRecorder(nil)
	metricsRecord("p2", "429", "exhausted")
	if observed != [3]string{"p1", "5xx", "retried_succeeded"} {
		t.Fatalf("nil recorder should be silent; observed mutated to %v", observed)
	}
}

// TestWithStats_AttachesRecorderAndIsCalled covers the WithStats setter
// AND the recordCredentialStats happy-path (line 465 — stats.RecordAttempt
// invocation). It also locks the contract that stats is called once per
// attempt with the resolved CredentialID and the attempt's status code.
func TestWithStats_AttachesRecorderAndIsCalled(t *testing.T) {
	adapter := &mockAdapter{format: mockFormat, responses: []scripted{
		{resp: &provcore.Response{StatusCode: 200, Body: []byte(`ok`)}},
	}}
	reg := newRegistry(t, adapter)
	res := okResolver()
	res.target.CredentialID = "cred-abc"

	stats := &statsRecorderStub{}
	exec := New(reg, res, nil, nil).WithStats(stats)
	if exec.stats != stats {
		t.Fatalf("WithStats did not attach the recorder")
	}

	result := exec.Execute(context.Background(),
		[]routingcore.RoutingTarget{target(providerSlug)},
		baseReq(),
		configtypes.DefaultRetryPolicy(),
	)
	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}

	calls := stats.snapshot()
	if len(calls) != 1 {
		t.Fatalf("expected exactly 1 stats call; got %d (%v)", len(calls), calls)
	}
	if calls[0].credentialID != "cred-abc" || calls[0].statusCode != 200 || calls[0].errMsg != "" {
		t.Fatalf("stats call mismatch: %+v", calls[0])
	}
}

// TestRecordCredentialStats_SkippedWhenCredIDEmpty exercises the
// fast-path guard at recordCredentialStats: an empty CredentialID must
// NOT trigger the stats hook (avoid recording stats for keys sourced
// from provider config rather than the Credential table).
func TestRecordCredentialStats_SkippedWhenCredIDEmpty(t *testing.T) {
	adapter := &mockAdapter{format: mockFormat, responses: []scripted{
		{resp: &provcore.Response{StatusCode: 200, Body: []byte(`ok`)}},
	}}
	reg := newRegistry(t, adapter)
	res := okResolver() // CredentialID is "" by default

	stats := &statsRecorderStub{}
	exec := New(reg, res, nil, nil).WithStats(stats)

	result := exec.Execute(context.Background(),
		[]routingcore.RoutingTarget{target(providerSlug)},
		baseReq(),
		configtypes.DefaultRetryPolicy(),
	)
	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if calls := stats.snapshot(); len(calls) != 0 {
		t.Fatalf("stats must not be called when CredentialID is empty; got %v", calls)
	}
}

// retryOnSet / inRetryOn — RetryPolicy.RetryOn corner cases.

// TestRetryOnSet_NilTreatedAsRetryEverything covers retryOnSet's nil
// branch (lines 130-131) and inRetryOn's nil-set fall-through (line
// 151-153) — "if a caller forgot to merge DefaultRetryPolicy, retry
// everything" is the documented defensive fallback.
func TestRetryOnSet_NilTreatedAsRetryEverything(t *testing.T) {
	set, retryNothing := retryOnSet(configtypes.RetryPolicy{RetryOn: nil})
	if set != nil || retryNothing {
		t.Fatalf("nil RetryOn must yield (nil set, retryNothing=false); got (%v, %v)", set, retryNothing)
	}
	if !inRetryOn(set, retryNothing, configtypes.ErrorClass5xx) {
		t.Fatal("nil set must return true for any class (retry everything)")
	}
	if !inRetryOn(set, retryNothing, configtypes.ErrorClass("totally-made-up")) {
		t.Fatal("nil set must return true even for an unknown class")
	}
}

// TestRetryOnSet_EmptySliceMeansRetryNothing covers retryOnSet's
// length-0 branch (lines 133-135) and inRetryOn's retryNothing=true
// short-circuit (lines 148-150) — explicit RetryOn=[] means "retry
// NOTHING", not "retry everything".
func TestRetryOnSet_EmptySliceMeansRetryNothing(t *testing.T) {
	set, retryNothing := retryOnSet(configtypes.RetryPolicy{RetryOn: []configtypes.ErrorClass{}})
	if set != nil || !retryNothing {
		t.Fatalf("empty RetryOn must yield (nil set, retryNothing=true); got (%v, %v)", set, retryNothing)
	}
	// retryNothing=true must veto every class, even ones that would be
	// in a "retry everything" set.
	for _, cls := range []configtypes.ErrorClass{
		configtypes.ErrorClassNetwork,
		configtypes.ErrorClassTimeout,
		configtypes.ErrorClassRate429,
		configtypes.ErrorClass5xx,
	} {
		if inRetryOn(set, retryNothing, cls) {
			t.Fatalf("retryNothing=true must veto %q", cls)
		}
	}
}

// executeInner — invalid Format and missing-adapter target-skip paths.

// TestExecute_SkipsTarget_WhenResolverYieldsInvalidFormat covers
// executor.go:248-250 — when the resolver returns a CallTarget whose
// Format does not satisfy Format.Valid(), that target is skipped with an
// error attempt and execution falls through to the next target.
func TestExecute_SkipsTarget_WhenResolverYieldsInvalidFormat(t *testing.T) {
	adapter := &mockAdapter{format: mockFormat, responses: []scripted{
		{resp: &provcore.Response{StatusCode: 200, Body: []byte(`ok`)}},
	}}
	reg := newRegistry(t, adapter)

	// First target resolves to an invalid Format -> skipped.
	// Second target resolves normally -> succeeds.
	type formatResolver struct {
		seq []provcore.CallTarget
		idx int
	}
	res := &formatResolver{seq: []provcore.CallTarget{
		{ProviderName: "bogus", Format: provcore.Format("not-a-real-format"), BaseURL: "https://x", APIKey: "k"},
		{ProviderName: providerSlug, Format: provcore.FormatOpenAI, BaseURL: "https://api.example.com", APIKey: "sk-123"},
	}}
	// closure resolver to avoid adding yet another helper type.
	resolver := provtargetFunc(func(_ context.Context, providerID, modelID string, _ provtarget.ResolveHints) (provcore.CallTarget, error) {
		if res.idx >= len(res.seq) {
			return provcore.CallTarget{}, errors.New("out of scripts")
		}
		ct := res.seq[res.idx]
		res.idx++
		return ct, nil
	})

	exec := New(reg, resolver, nil, nil)
	result := exec.Execute(context.Background(),
		[]routingcore.RoutingTarget{target("first"), target("second")},
		baseReq(),
		configtypes.DefaultRetryPolicy(),
	)

	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if result.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from second target, got %d", result.StatusCode)
	}
	if len(result.Attempts) != 2 {
		t.Fatalf("expected 2 attempts (skipped + success), got %d", len(result.Attempts))
	}
	if result.Attempts[0].Error == "" {
		t.Fatalf("first attempt must carry the 'invalid adapter_type' error message")
	}
	wantSubstr := "invalid adapter_type"
	if got := result.Attempts[0].Error; !contains(got, wantSubstr) {
		t.Fatalf("first attempt error %q must mention %q", got, wantSubstr)
	}
}

// TestExecute_SkipsTarget_WhenNoAdapterRegistered covers
// executor.go:253-255 — when the resolver returns a Format that
// passes Valid() but is not present in the adapter Registry, the
// target is skipped with an explicit "no adapter registered" error.
func TestExecute_SkipsTarget_WhenNoAdapterRegistered(t *testing.T) {
	// Registry has only FormatOpenAI; first target asks for FormatAnthropic.
	adapter := &mockAdapter{format: mockFormat, responses: []scripted{
		{resp: &provcore.Response{StatusCode: 200, Body: []byte(`ok`)}},
	}}
	reg := newRegistry(t, adapter)

	type seq struct {
		cts []provcore.CallTarget
		idx int
	}
	s := &seq{cts: []provcore.CallTarget{
		// Anthropic is a valid Format but no adapter registered for it.
		{ProviderName: "anthropic", Format: provcore.FormatAnthropic, BaseURL: "https://x", APIKey: "k"},
		{ProviderName: providerSlug, Format: provcore.FormatOpenAI, BaseURL: "https://api.example.com", APIKey: "sk-123"},
	}}
	resolver := provtargetFunc(func(_ context.Context, providerID, modelID string, _ provtarget.ResolveHints) (provcore.CallTarget, error) {
		if s.idx >= len(s.cts) {
			return provcore.CallTarget{}, errors.New("out of scripts")
		}
		ct := s.cts[s.idx]
		s.idx++
		return ct, nil
	})

	exec := New(reg, resolver, nil, nil)
	result := exec.Execute(context.Background(),
		[]routingcore.RoutingTarget{target("anthropic-target"), target("openai-target")},
		baseReq(),
		configtypes.DefaultRetryPolicy(),
	)

	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if result.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from second target, got %d", result.StatusCode)
	}
	if len(result.Attempts) != 2 {
		t.Fatalf("expected 2 attempts, got %d", len(result.Attempts))
	}
	if !contains(result.Attempts[0].Error, "no adapter registered") {
		t.Fatalf("first attempt error must mention missing adapter; got %q", result.Attempts[0].Error)
	}
	// Confirm error names the format explicitly so an operator can grep.
	if !contains(result.Attempts[0].Error, string(provcore.FormatAnthropic)) {
		t.Fatalf("error must name the missing format %q; got %q", provcore.FormatAnthropic, result.Attempts[0].Error)
	}
}

// executeInner — bridge translation failure path.

// TestExecute_BridgeTranslateError_SkipsToNextTarget covers
// executor.go:270-277 — when bridge.IngressChatToWire returns an error
// (because target format has no codec in the bridge map), the target
// is skipped with a "hub translate" error attempt and execution falls
// through to the next target.
//
// To reach the bridge call we need:
//   - bridge != nil
//   - base.Endpoint == EndpointChatCompletions
//   - ingress != target (otherwise the executor short-circuits and the
//     bridge is not invoked)
//   - the FIRST target's adapter must be registered (otherwise the
//     no-adapter guard at line 253 fires before the bridge call)
//
// Wiring: register adapters for both FormatOpenAI and FormatAnthropic,
// install a bridge whose codecs map is EMPTY so the
// b.codecs[target] lookup inside IngressChatToWire fails on the
// FormatOpenAI->FormatAnthropic edge with "no codec for format".
func TestExecute_BridgeTranslateError_SkipsToNextTarget(t *testing.T) {
	// Anthropic-shaped adapter — registered so the registry lookup
	// succeeds; if the bridge translate failed we'd surface "hub translate"
	// before it ever sees adapter.Execute.
	anthAdapter := &mockAdapter{format: provcore.FormatAnthropic, responses: []scripted{
		{resp: &provcore.Response{StatusCode: 200, Body: []byte(`anth-unreached`)}},
	}}
	// OpenAI adapter for the second target — succeeds.
	oaiAdapter := &mockAdapter{format: provcore.FormatOpenAI, responses: []scripted{
		{resp: &provcore.Response{StatusCode: 200, Body: []byte(`ok`)}},
	}}
	reg := provcore.NewRegistry()
	if err := reg.Register(anthAdapter); err != nil {
		t.Fatalf("register anth: %v", err)
	}
	if err := reg.Register(oaiAdapter); err != nil {
		t.Fatalf("register oai: %v", err)
	}
	reg.Freeze()

	// Resolver returns:
	//   target[0] -> FormatAnthropic (codec missing -> translate fail)
	//   target[1] -> FormatOpenAI    (passthrough, ingress==target, succeeds)
	type seq struct {
		cts []provcore.CallTarget
		idx int
	}
	s := &seq{cts: []provcore.CallTarget{
		{ProviderName: "anthropic-target", Format: provcore.FormatAnthropic, BaseURL: "https://x", APIKey: "k"},
		{ProviderName: providerSlug, Format: provcore.FormatOpenAI, BaseURL: "https://api.example.com", APIKey: "sk-123"},
	}}
	resolver := provtargetFunc(func(_ context.Context, providerID, modelID string, _ provtarget.ResolveHints) (provcore.CallTarget, error) {
		if s.idx >= len(s.cts) {
			return provcore.CallTarget{}, errors.New("out of scripts")
		}
		ct := s.cts[s.idx]
		s.idx++
		return ct, nil
	})

	// Bridge with empty codecs — IngressChatToWire(FormatOpenAI -> FormatAnthropic)
	// will reach the codec lookup branch and fail with "no codec for format".
	bridge := canonicalbridge.New(map[provcore.Format]provcore.SchemaCodec{})

	exec := New(reg, resolver, nil, bridge)

	// Ingress = FormatOpenAI; first target resolves to FormatAnthropic
	// so ingress != target and the bridge path is entered.
	req := provcore.Request{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatOpenAI,
		Body:       []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`),
	}

	result := exec.Execute(context.Background(),
		[]routingcore.RoutingTarget{target("anth"), target("oai")},
		req,
		configtypes.DefaultRetryPolicy(),
	)

	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if result.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from second target after first failed translate, got %d", result.StatusCode)
	}
	if len(result.Attempts) != 2 {
		t.Fatalf("expected 2 attempts (translate-fail + success), got %d", len(result.Attempts))
	}
	if !contains(result.Attempts[0].Error, "hub translate") {
		t.Fatalf("first attempt must carry 'hub translate' error; got %q", result.Attempts[0].Error)
	}
	// First adapter must never have been called — translate failed before
	// dispatch.
	if anthAdapter.callIndex != 0 {
		t.Fatalf("anthropic adapter must NOT be called when translate fails first; got callIndex=%d", anthAdapter.callIndex)
	}
}

// executeInner — context-cancelled mid-backoff path.

// TestExecute_ContextCanceledDuringBackoff covers executor.go:338-339 —
// when ctx is cancelled while the executor is sleeping between L2
// retries, the function returns immediately with ctx.Err() (NOT
// ErrAllTargetsExhausted) and includes only the attempts taken so far.
func TestExecute_ContextCanceledDuringBackoff(t *testing.T) {
	// Single 5xx response — the executor will then enter backoff. We
	// cancel ctx during that sleep window so the <-ctx.Done() arm wins.
	adapter := &mockAdapter{format: mockFormat, responses: []scripted{
		{err: &provcore.ProviderError{Status: 500, Code: provcore.CodeUpstreamError, Message: "boom"}},
		{resp: &provcore.Response{StatusCode: 200, Body: []byte(`unreached`)}},
	}}
	reg := newRegistry(t, adapter)
	res := okResolver()
	exec := New(reg, res, nil, nil)

	// Long backoff so we have time to cancel during the sleep. No
	// ctx deadline -> the deadline-imminent guard at line 330-335 is
	// skipped and the executor enters time.After/ctx.Done select.
	policy := configtypes.RetryPolicy{
		MaxAttemptsPerTarget: 3,
		RetryOn:              []configtypes.ErrorClass{configtypes.ErrorClass5xx},
		BackoffInitial:       500 * time.Millisecond,
		BackoffMax:           500 * time.Millisecond,
		BackoffJitter:        0,
	}

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel after the first attempt is in flight and the executor is
	// sleeping in the backoff select. 50ms is well under the 500ms
	// backoff so we land squarely in <-ctx.Done().
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	result := exec.Execute(ctx,
		[]routingcore.RoutingTarget{target(providerSlug)},
		baseReq(),
		policy,
	)
	elapsed := time.Since(start)

	if !errors.Is(result.Error, context.Canceled) {
		t.Fatalf("expected ctx.Err()=context.Canceled, got %v", result.Error)
	}
	if elapsed >= 400*time.Millisecond {
		t.Fatalf("executor did not bail on ctx.Done; elapsed=%v >= 400ms", elapsed)
	}
	if len(result.Attempts) != 1 {
		t.Fatalf("expected exactly 1 attempt before cancellation, got %d", len(result.Attempts))
	}
	// Second response in the script must not have been consumed.
	if adapter.callIndex != 1 {
		t.Fatalf("adapter should have been called exactly once; got callIndex=%d", adapter.callIndex)
	}
}

// classifyAttempt — defensive fallback when classifier returns
// classNoFailoverNoRetry without a ProviderError envelope.

// classifyOverrideAdapter is an Adapter shim that delegates to a base
// mockAdapter but swaps the FIRST classifyAttempt path by returning a
// plain non-ProviderError error AFTER a 4xx-coded one. To hit the
// defensive fallback at executor.go:425-431 we need classify() to
// return classNoFailoverNoRetry while errors.As(err, &pe) is false. We
// can engineer that by stubbing the `classify` function via a test
// seam: see classifyForTest below.
//
// This isn't reachable through real adapter calls because every code
// path that produces classNoFailoverNoRetry inside classify() requires
// a *provcore.ProviderError. The block is documented as a defensive
// fallback. To exercise it we install a temporary test seam.
//
// We do this without a production code change by calling classifyAttempt
// directly with a hand-built (resp,err) pair that classifyAttempt's
// branches can both reach -- but classifyAttempt itself calls classify()
// which is a package-level function and can't be stubbed without a
// production seam. The minimally-invasive option is to invoke
// classifyAttempt with the pair classify() would route to
// classNoFailoverNoRetry, and accept that classifyAttempt's defensive
// fallback line is unreachable through any real input. We surface this
// in the final report.
//
// (No test added — the defensive fallback is correctly flagged
// unreachable by audit.)

// classifyAttempt — recordHealth happy paths (tracker non-nil).

// TestExecute_RecordsHealth_SuccessAndFailure covers executor.go:454-458
// — when a HealthTracker is wired, recordHealth invokes RecordSuccess
// (success path) AND RecordFailure (retryable failure + 4xx terminal).
// This is asserted by reading HealthTracker.GetHealth back.
func TestExecute_RecordsHealth_SuccessAndFailure(t *testing.T) {
	t.Run("success path records via RecordSuccess", func(t *testing.T) {
		adapter := &mockAdapter{format: mockFormat, responses: []scripted{
			{resp: &provcore.Response{StatusCode: 200, Body: []byte(`ok`)}},
		}}
		reg := newRegistry(t, adapter)
		res := okResolver()
		health := store.NewHealthTracker()
		exec := New(reg, res, health, nil)

		tgt := target(providerSlug)
		result := exec.Execute(context.Background(),
			[]routingcore.RoutingTarget{tgt},
			baseReq(),
			configtypes.DefaultRetryPolicy(),
		)
		if result.Error != nil {
			t.Fatalf("unexpected error: %v", result.Error)
		}
		hs := health.GetHealth(tgt.ProviderID)
		// SampleCount > 0 proves we reached RecordSuccess. The
		// HealthTracker contract guarantees a successful call appends
		// to the sample window.
		if hs.SampleCount == 0 {
			t.Fatalf("RecordSuccess was not called; GetHealth=%+v", hs)
		}
		if hs.ErrorRate != 0 {
			t.Fatalf("success-only window must have ErrorRate=0; got %v", hs.ErrorRate)
		}
	})

	t.Run("retryable failure records via RecordFailure", func(t *testing.T) {
		adapter := &mockAdapter{format: mockFormat, responses: []scripted{
			{err: &provcore.ProviderError{Status: 500, Code: provcore.CodeUpstreamError, Message: "boom"}},
		}}
		reg := newRegistry(t, adapter)
		res := okResolver()
		health := store.NewHealthTracker()
		exec := New(reg, res, health, nil)

		tgt := target(providerSlug)
		result := exec.Execute(context.Background(),
			[]routingcore.RoutingTarget{tgt},
			baseReq(),
			// max=1: a single 5xx on the only target exhausts immediately with one
			// failure sample. (The platform default now retries once, which would
			// record two samples — this subtest pins the single-sample case.)
			fastBackoffPolicy(1),
		)
		// 5xx + single target, no same-target retry -> ErrAllTargetsExhausted.
		if !errors.Is(result.Error, ErrAllTargetsExhausted) {
			t.Fatalf("expected ErrAllTargetsExhausted, got %v", result.Error)
		}
		hs := health.GetHealth(tgt.ProviderID)
		if hs.SampleCount == 0 {
			t.Fatalf("RecordFailure was not called; GetHealth=%+v", hs)
		}
		// A single failure sample yields ErrorRate=1.0 which falls into
		// the unavailable bucket — proves the failure (not success) was
		// recorded.
		if hs.ErrorRate != 1.0 {
			t.Fatalf("single failure must yield ErrorRate=1.0; got %v", hs.ErrorRate)
		}
	})

	t.Run("4xx terminal records via RecordFailure", func(t *testing.T) {
		adapter := &mockAdapter{format: mockFormat, responses: []scripted{
			{err: &provcore.ProviderError{Status: 401, Code: provcore.CodeAuthFailed, Message: "unauth", Raw: []byte(`{"e":1}`)}},
		}}
		reg := newRegistry(t, adapter)
		res := okResolver()
		health := store.NewHealthTracker()
		exec := New(reg, res, health, nil)

		tgt := target(providerSlug)
		result := exec.Execute(context.Background(),
			[]routingcore.RoutingTarget{tgt},
			baseReq(),
			configtypes.DefaultRetryPolicy(),
		)
		if result.Error != nil {
			t.Fatalf("unexpected error: %v", result.Error)
		}
		if result.StatusCode != http.StatusUnauthorized {
			t.Fatalf("expected 401, got %d", result.StatusCode)
		}
		hs := health.GetHealth(tgt.ProviderID)
		if hs.SampleCount == 0 {
			t.Fatalf("RecordFailure was not called on 4xx terminal; GetHealth=%+v", hs)
		}
		if hs.ErrorRate != 1.0 {
			t.Fatalf("4xx terminal must record as failure; got ErrorRate=%v", hs.ErrorRate)
		}
	})
}

// backoff.go — base-overshoots-Max clamp + negative-jitter clamp.

// TestComputeBackoff_BaseExceedsMaxAfterDoubling covers backoff.go:33-35
// — the post-loop clamp that triggers when BackoffInitial already
// exceeds BackoffMax (loop body never runs but base > max).
func TestComputeBackoff_BaseExceedsMaxAfterDoubling(t *testing.T) {
	p := configtypes.RetryPolicy{
		BackoffInitial: 5 * time.Second,
		BackoffMax:     1 * time.Second, // smaller than BackoffInitial
		BackoffJitter:  0,
	}
	got := computeBackoff(1, p)
	if got != 1*time.Second {
		t.Fatalf("expected base clamped to BackoffMax=1s; got %v", got)
	}
}

// TestComputeBackoff_NegativeJitterClampedToZero covers backoff.go:42-44
// — when extreme jitter (>1.0) pushes base below zero, the function
// must return 0, never a negative duration.
//
// The math: delta = base * jitter; result = base + Uniform[-delta,+delta).
// With jitter > 1.0, delta > base so base - delta < 0; ~half the
// uniform samples will trigger the clamp branch. We assert no negative
// values leak out and at least one sample lands on the clamped-to-zero
// branch.
func TestComputeBackoff_NegativeJitterClampedToZero(t *testing.T) {
	p := configtypes.RetryPolicy{
		BackoffInitial: 100 * time.Millisecond,
		BackoffMax:     100 * time.Millisecond,
		BackoffJitter:  2.0, // ± 2*base -> ~half of samples land negative pre-clamp
	}
	sawZero := false
	for i := range 10000 {
		got := computeBackoff(1, p)
		if got < 0 {
			t.Fatalf("iteration %d: negative backoff %v", i, got)
		}
		if got == 0 {
			sawZero = true
		}
	}
	if !sawZero {
		t.Fatalf("never observed clamped-to-zero in 10k iterations with jitter=2.0; the negative-clamp branch did not fire")
	}
}

// classify.go — explicit unreachable-line defensive coverage.

// TestClassify_NilRespNilErrFallsThroughToNetwork covers classify.go:53 —
// the final defensive `return classNetwork, ErrorClassNetwork`. This
// branch fires when err==nil AND (resp==nil OR resp.StatusCode is not
// 2xx). resp==nil + err==nil is a degenerate adapter contract violation,
// but the line exists to keep the function total.
func TestClassify_NilRespNilErrFallsThroughToNetwork(t *testing.T) {
	cls, errCl := classify(nil, nil)
	if cls != classNetwork {
		t.Fatalf("expected classNetwork for (nil,nil); got %v", cls)
	}
	if errCl != configtypes.ErrorClassNetwork {
		t.Fatalf("expected ErrorClassNetwork for (nil,nil); got %q", errCl)
	}
	// Also covers the path where resp is non-nil but the status is in
	// the 1xx / 3xx range — neither success nor a ProviderError. The
	// classifier must still surface classNetwork rather than crash.
	cls, errCl = classify(&provcore.Response{StatusCode: 100}, nil)
	if cls != classNetwork || errCl != configtypes.ErrorClassNetwork {
		t.Fatalf("expected classNetwork for 1xx with no err; got (%v, %q)", cls, errCl)
	}
}

// Sanity-check: classify already handles io.EOF correctly via
// classify_test.go's existing case — we don't duplicate that here.
var _ = io.EOF

// provtargetFunc is a function-typed Resolver shim so each table-test can
// pass its own closure without declaring a fresh struct. Satisfies the
// provtarget.Resolver interface via a single method.
type provtargetFunc func(ctx context.Context, providerID, modelID string, hints provtarget.ResolveHints) (provcore.CallTarget, error)

func (f provtargetFunc) Resolve(ctx context.Context, providerID, modelID string, hints provtarget.ResolveHints) (provcore.CallTarget, error) {
	return f(ctx, providerID, modelID, hints)
}

// contains is a tiny strings.Contains alias kept local so this test file
// does not need a strings import (the existing executor_test.go already
// keeps imports tight).
func contains(s, sub string) bool {
	if sub == "" {
		return true
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// TestExecute_EmbeddingsBridgeTranslate_SkipsToNextTarget mirrors the chat
// bridge-translate failover for the embeddings ingress kind: an embeddings
// request whose first target needs cross-format translation hits the
// EmbeddingsWireShapeForTarget call-shape + IngressEmbeddingsToWire branch in
// executeInner. With an empty-codec bridge the first translate fails ("hub
// translate") and the executor fails over to the same-format OpenAI target.
func TestExecute_EmbeddingsBridgeTranslate_SkipsToNextTarget(t *testing.T) {
	gemAdapter := &mockAdapter{format: provcore.FormatGemini, responses: []scripted{
		{resp: &provcore.Response{StatusCode: 200, Body: []byte(`gem-unreached`)}},
	}}
	oaiAdapter := &mockAdapter{format: provcore.FormatOpenAI, responses: []scripted{
		{resp: &provcore.Response{StatusCode: 200, Body: []byte(`ok`)}},
	}}
	reg := provcore.NewRegistry()
	if err := reg.Register(gemAdapter); err != nil {
		t.Fatalf("register gem: %v", err)
	}
	if err := reg.Register(oaiAdapter); err != nil {
		t.Fatalf("register oai: %v", err)
	}
	reg.Freeze()

	type embSeq struct {
		cts []provcore.CallTarget
		idx int
	}
	s := &embSeq{cts: []provcore.CallTarget{
		{ProviderName: "gemini-target", Format: provcore.FormatGemini, BaseURL: "https://x", APIKey: "k"},
		{ProviderName: providerSlug, Format: provcore.FormatOpenAI, BaseURL: "https://api.example.com", APIKey: "sk-123"},
	}}
	resolver := provtargetFunc(func(_ context.Context, _, _ string, _ provtarget.ResolveHints) (provcore.CallTarget, error) {
		if s.idx >= len(s.cts) {
			return provcore.CallTarget{}, errors.New("out of scripts")
		}
		ct := s.cts[s.idx]
		s.idx++
		return ct, nil
	})

	bridge := canonicalbridge.New(map[provcore.Format]provcore.SchemaCodec{})
	exec := New(reg, resolver, nil, bridge)

	req := provcore.Request{
		WireShape:  typology.WireShapeOpenAIEmbeddings,
		BodyFormat: provcore.FormatOpenAI,
		Body:       []byte(`{"model":"text-embedding-3-small","input":"hi"}`),
	}
	result := exec.Execute(context.Background(),
		[]routingcore.RoutingTarget{target("gem"), target("oai")},
		req,
		configtypes.DefaultRetryPolicy(),
	)
	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if result.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from second target after first failed translate, got %d", result.StatusCode)
	}
	if len(result.Attempts) != 2 {
		t.Fatalf("expected 2 attempts (translate-fail + success), got %d", len(result.Attempts))
	}
	if !contains(result.Attempts[0].Error, "hub translate") {
		t.Fatalf("first attempt must carry 'hub translate' error; got %q", result.Attempts[0].Error)
	}
}

// TestExecute_EmbeddingsBridgeURLOverride_ReachesAdapter is the executor-seam
// regression. A cross-format embeddings request (OpenAI ingress
// → Gemini target) is translated by the bridge, which emits the codec's
// endpoint-selection URLOverride (:embedContent vs :batchEmbedContents). The
// executor MUST hand that override to adapter.ExecuteWithBody — previously it
// called the plain attempt() (no override) and adapter.Execute's same-format
// passthrough re-derived nothing, so the batch body went to :embedContent and
// Gemini 400'd. This asserts the override actually arrives at the adapter for
// both single and batch inputs, and that single embeds carry no batch suffix.
func TestExecute_EmbeddingsBridgeURLOverride_ReachesAdapter(t *testing.T) {
	cases := []struct {
		name         string
		input        string
		wantOverride string
	}{
		{"batch_array", `["a","b","c","d","e","f","g","h"]`, ":batchEmbedContents"},
		{"single_string", `"hello"`, ":embedContent"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gem := &mockAdapter{format: provcore.FormatGemini, responses: []scripted{
				{resp: &provcore.Response{StatusCode: 200, Body: []byte(`{"object":"list","data":[]}`)}},
			}}
			reg := provcore.NewRegistry()
			if err := reg.Register(gem); err != nil {
				t.Fatalf("register gem: %v", err)
			}
			reg.Freeze()

			resolver := provtargetFunc(func(_ context.Context, _, modelID string, _ provtarget.ResolveHints) (provcore.CallTarget, error) {
				return provcore.CallTarget{
					ProviderName:    "gemini-target",
					Format:          provcore.FormatGemini,
					BaseURL:         "https://generativelanguage.googleapis.com",
					APIKey:          "k",
					ProviderModelID: "gemini-embedding-001",
				}, nil
			})

			// Real bridge with the production codecs so IngressEmbeddingsToWire
			// runs the actual Gemini embeddings codec and emits the override.
			bridge := canonicalbridge.New(provbuiltins.SchemaCodecs(slog.Default()))
			exec := New(reg, resolver, nil, bridge)

			base := provcore.Request{
				WireShape:  typology.WireShapeOpenAIEmbeddings,
				BodyFormat: provcore.FormatOpenAI,
				Body:       []byte(`{"model":"gemini-embedding-001","input":` + c.input + `}`),
			}
			result := exec.Execute(context.Background(),
				[]routingcore.RoutingTarget{target("gem")},
				base,
				configtypes.DefaultRetryPolicy(),
			)
			if result.Error != nil {
				t.Fatalf("unexpected error: %v (attempts=%+v)", result.Error, result.Attempts)
			}
			if result.StatusCode != http.StatusOK {
				t.Fatalf("expected 200, got %d", result.StatusCode)
			}
			if gem.executeWithBodyCalls != 1 {
				t.Fatalf("expected ExecuteWithBody to be called once (bridge-override path), got %d", gem.executeWithBodyCalls)
			}
			if gem.lastURLOverride != c.wantOverride {
				t.Fatalf("override reaching adapter = %q, want %q", gem.lastURLOverride, c.wantOverride)
			}
		})
	}
}

// TestExecute_ResponsesNative_KeepsResponsesWireShape covers the fix
// (executor.go executeInner): a /v1/responses request whose target NATIVELY
// serves the Responses API must keep WireShape=openai-responses (so BuildURL
// targets /v1/responses) and forward the verbatim Responses body — NOT be
// rewritten to openai-chat. The bug sent the input-only Responses body to
// /v1/chat/completions → upstream 400 "Missing required parameter: 'messages'".
func TestExecute_ResponsesNative_KeepsResponsesWireShape(t *testing.T) {
	adapter := &mockAdapter{format: provcore.FormatOpenAI, responses: []scripted{
		{resp: &provcore.Response{StatusCode: 200, Body: []byte(`{"object":"response","output":[]}`)}},
	}}
	reg := newRegistry(t, adapter)
	res := okResolver()
	// Real bridge: ServesResponses(FormatOpenAI, nil)==true (adapter
	// RequestShapes default, codec-independent), so the native-responses
	// passthrough branch fires.
	bridge := canonicalbridge.New(map[provcore.Format]provcore.SchemaCodec{})
	exec := New(reg, res, nil, bridge)

	base := provcore.Request{
		WireShape:  typology.WireShapeOpenAIResponses,
		BodyFormat: provcore.FormatOpenAIResponses,
		Body:       []byte(`{"model":"gpt-4o","input":"hi"}`),
	}
	result := exec.Execute(context.Background(), []routingcore.RoutingTarget{target(providerSlug)}, base, configtypes.DefaultRetryPolicy())
	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if adapter.lastReq.WireShape != typology.WireShapeOpenAIResponses {
		t.Fatalf("native /v1/responses must keep WireShape=openai-responses (BuildURL→/v1/responses); got %q → would hit /v1/chat/completions (E56)", adapter.lastReq.WireShape)
	}
	if string(adapter.lastReq.Body) != string(base.Body) {
		t.Fatalf("native responses body must be forwarded verbatim; got %s", adapter.lastReq.Body)
	}
}

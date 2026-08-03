package executor

import (
	"context"
	"net/http"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	configtypes "github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configtypes/policy"
)

// TestTerminal_IsTheRateLimitedCall_NotTheAbandonedRetry drives the real
// executor through the sequence a rate limit sets off against a single target:
// the call is rate-limited, the policy retries, and the retry's re-resolve finds
// the pool dry. The abandoned retry is recorded last.
//
// Terminal must still be the rate-limited call. It is what the handler reads to
// decide the client's status, and reading the abandoned entry instead reports a
// throttled provider as a dead one — on exactly the failure whose whole point is
// to make the client back off.
//
// A single target is what makes this reachable: with two, the next target's real
// call follows the abandoned one, so the abandoned entry is never last.
func TestTerminal_IsTheRateLimitedCall_NotTheAbandonedRetry(t *testing.T) {
	adapter := &mockAdapter{format: mockFormat, responses: []scripted{
		{err: &provcore.ProviderError{Status: http.StatusTooManyRequests, Code: provcore.CodeRateLimited, Message: "rl"}},
	}}
	res := &perProviderResolver{
		base:     provcore.CallTarget{Format: mockFormat, BaseURL: "https://up.test"},
		okBudget: map[string]int{"prov-" + providerSlug: 1},
	}
	exec := New(newRegistry(t, adapter), res, nil, nil)
	withCapturedMetrics(t)

	result := exec.Execute(context.Background(),
		[]routingcore.RoutingTarget{target(providerSlug)},
		baseReq(),
		fastBackoffPolicy(3, configtypes.ErrorClassRate429),
	)

	// Precondition: the scenario only means anything if the abandoned retry
	// really is the last entry. Assert the shape rather than trusting it.
	if len(result.Attempts) != 2 {
		t.Fatalf("recorded %d attempts, want 2 (the rate-limited call + the retry that found no credential)", len(result.Attempts))
	}
	if last := result.Attempts[len(result.Attempts)-1]; last.Dispatched {
		t.Fatalf("the last entry reached a provider; the test is not exercising the abandoned-retry case")
	}

	term := result.Terminal()
	if term == nil {
		t.Fatal("Terminal is nil, want the rate-limited call: a call did reach the provider")
	}
	if term.Code != provcore.CodeRateLimited {
		t.Errorf("Terminal.Code = %q, want %q: the retry abandoned before dispatch must not erase the cause of the failure it was reacting to",
			term.Code, provcore.CodeRateLimited)
	}
	if term.StatusCode != http.StatusTooManyRequests {
		t.Errorf("Terminal.StatusCode = %d, want 429", term.StatusCode)
	}
	if got := result.UpstreamAttempts(); got != 1 {
		t.Errorf("UpstreamAttempts = %d, want 1: only one call left the process", got)
	}
}

// TestTerminal_NoTargetEverDispatched reports the honest absence. Every target
// failed to resolve, so nothing reached a provider and there is no upstream
// outcome to attribute the failure to — the handler must be able to tell that
// apart from "a call failed".
func TestTerminal_NoTargetEverDispatched(t *testing.T) {
	adapter := &mockAdapter{format: mockFormat}
	res := &perProviderResolver{
		base:     provcore.CallTarget{Format: mockFormat, BaseURL: "https://up.test"},
		okBudget: map[string]int{"prov-" + providerSlug: 0},
	}
	exec := New(newRegistry(t, adapter), res, nil, nil)
	withCapturedMetrics(t)

	result := exec.Execute(context.Background(),
		[]routingcore.RoutingTarget{target(providerSlug)},
		baseReq(),
		fastBackoffPolicy(3, configtypes.ErrorClassRate429),
	)

	if len(result.Attempts) == 0 {
		t.Fatal("the resolve failure must still be recorded: it is the only account of why the request failed")
	}
	if term := result.Terminal(); term != nil {
		t.Errorf("Terminal = %+v, want nil: no call reached a provider, so claiming one would attribute the failure to a provider that was never asked", term)
	}
	if got := result.UpstreamAttempts(); got != 0 {
		t.Errorf("UpstreamAttempts = %d, want 0: no call left the process", got)
	}
}

// TestTerminal_SkipsBackPastAnAbandonedTarget pins that Terminal reaches back
// past a later target that never dispatched, rather than stopping at the most
// recent entry. The first target answers, the second is abandoned before any
// call — the first target's answer is still the only upstream outcome there is.
func TestTerminal_SkipsBackPastAnAbandonedTarget(t *testing.T) {
	adapter := &mockAdapter{format: mockFormat, responses: []scripted{
		{err: &provcore.ProviderError{Status: http.StatusInternalServerError, Code: provcore.CodeUpstreamError, Message: "boom"}},
	}}
	res := &perProviderResolver{
		base: provcore.CallTarget{Format: mockFormat, BaseURL: "https://up.test"},
		okBudget: map[string]int{
			"prov-" + providerSlug: 1, // first target: resolves once, then dry
			"prov-provider-b":      0, // second target: never resolves
		},
	}
	exec := New(newRegistry(t, adapter), res, nil, nil)
	withCapturedMetrics(t)

	result := exec.Execute(context.Background(),
		[]routingcore.RoutingTarget{target(providerSlug), target("provider-b")},
		baseReq(),
		fastBackoffPolicy(1, configtypes.ErrorClass5xx),
	)

	term := result.Terminal()
	if term == nil {
		t.Fatal("Terminal is nil, want the first target's call: it did reach a provider")
	}
	if term.Code != provcore.CodeUpstreamError {
		t.Errorf("Terminal.Code = %q, want %q: the abandoned second target must not mask the only real upstream outcome",
			term.Code, provcore.CodeUpstreamError)
	}
	if got := result.UpstreamAttempts(); got != 1 {
		t.Errorf("UpstreamAttempts = %d, want 1", got)
	}
}

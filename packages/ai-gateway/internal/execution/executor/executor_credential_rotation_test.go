package executor

import (
	"context"
	"errors"
	"net/http"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	provtarget "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/target"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	configtypes "github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configtypes/policy"
)

// rotatingResolver hands out a different credential on each Resolve call — the
// shape a real pool takes when the previous credential's circuit just opened
// and Select moves on to the next eligible one.
type rotatingResolver struct {
	base  provcore.CallTarget
	creds []string
	calls int
}

func (m *rotatingResolver) Resolve(_ context.Context, providerID, modelID string, _ provtarget.ResolveHints) (provcore.CallTarget, error) {
	t := m.base
	t.ProviderID = providerID
	t.ProviderModelID = modelID
	if m.calls < len(m.creds) {
		t.CredentialID = m.creds[m.calls]
		t.CredentialName = m.creds[m.calls]
	}
	m.calls++
	return t, nil
}

// errPoolExhausted mirrors what PgResolver.resolveCredential returns once every
// candidate's circuit is open.
var errPoolExhausted = errors.New(`provtarget: credential: all credentials for provider are circuit-open`)

// perProviderResolver reports one provider's pool as exhausted after its resolve
// budget is spent, leaving other providers healthy — so a test can dry up the
// first target without disarming the failover target.
type perProviderResolver struct {
	base     provcore.CallTarget
	okBudget map[string]int // providerID -> resolves that succeed before the pool reports exhausted
	calls    map[string]int
}

func (m *perProviderResolver) Resolve(_ context.Context, providerID, modelID string, _ provtarget.ResolveHints) (provcore.CallTarget, error) {
	if m.calls == nil {
		m.calls = map[string]int{}
	}
	m.calls[providerID]++
	if budget, capped := m.okBudget[providerID]; capped && m.calls[providerID] > budget {
		return provcore.CallTarget{}, errPoolExhausted
	}
	t := m.base
	t.ProviderID = providerID
	t.ProviderModelID = modelID
	t.CredentialID = "cred-" + providerID
	return t, nil
}

// A 429 opens the serving credential's circuit (credentials/stats/buffer.go
// opens on a single 429). The L2 retry must therefore ask the pool for a
// credential again rather than re-sending to the one it just tripped —
// otherwise every retry hammers a breaker we opened ourselves, and a
// multi-credential pool never gets used for the one thing it exists to do.
//
// Asserts the credential each attempt actually used, which is the observable
// the audit path records (Attempt.CredentialID → traffic_event.credential_id).
func TestExecute_L2_Retry_RotatesCredential(t *testing.T) {
	adapter := &mockAdapter{format: mockFormat, responses: []scripted{
		{err: &provcore.ProviderError{Status: 429, Code: provcore.CodeRateLimited, Message: "rl"}},
		{err: &provcore.ProviderError{Status: 429, Code: provcore.CodeRateLimited, Message: "rl"}},
		{resp: &provcore.Response{StatusCode: 200, Body: []byte(`ok`)}},
	}}
	reg := newRegistry(t, adapter)
	res := &rotatingResolver{
		base:  provcore.CallTarget{Format: mockFormat, BaseURL: "https://up.test"},
		creds: []string{"cred-1", "cred-2", "cred-3"},
	}
	exec := New(reg, res, nil, nil)
	withCapturedMetrics(t)

	result := exec.Execute(context.Background(),
		[]routingcore.RoutingTarget{target(providerSlug)},
		baseReq(),
		fastBackoffPolicy(3, configtypes.ErrorClassRate429),
	)

	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if len(result.Attempts) != 3 {
		t.Fatalf("expected 3 attempts, got %d", len(result.Attempts))
	}
	want := []string{"cred-1", "cred-2", "cred-3"}
	got := make([]string, len(result.Attempts))
	for i := range result.Attempts {
		got[i] = result.Attempts[i].CredentialID
	}
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("attempt %d used credential %q, want %q — the retry must re-resolve, not reuse the credential whose circuit it just tripped (full: %v)", i, got[i], w, got)
		}
	}
}

// When the pool has nothing eligible left, the retry's re-resolve fails. That
// must drop to L3 failover rather than surface a raw resolve error to the
// client or spin the remaining L2 budget against a pool that has nothing to
// give.
func TestExecute_L2_Retry_NoEligibleCredential_FallsToL3(t *testing.T) {
	adapter := &mockAdapter{format: mockFormat, responses: []scripted{
		{err: &provcore.ProviderError{Status: 429, Code: provcore.CodeRateLimited, Message: "rl"}},
		{resp: &provcore.Response{StatusCode: 200, Body: []byte(`second target ok`)}},
	}}
	reg := newRegistry(t, adapter)
	// target(x) sets ProviderID to "prov-"+x. Dry up the first target's pool
	// after its initial resolve; leave the failover target's pool healthy.
	res := &perProviderResolver{
		base:     provcore.CallTarget{Format: mockFormat, BaseURL: "https://up.test"},
		okBudget: map[string]int{"prov-" + providerSlug: 1},
	}
	exec := New(reg, res, nil, nil)
	withCapturedMetrics(t)

	result := exec.Execute(context.Background(),
		[]routingcore.RoutingTarget{target(providerSlug), target("provider-b")},
		baseReq(),
		fastBackoffPolicy(3, configtypes.ErrorClassRate429),
	)

	if result.Error != nil {
		t.Fatalf("must fail over to the second target, got error: %v", result.Error)
	}
	if result.StatusCode != http.StatusOK {
		t.Fatalf("expected the second target's 200, got %d", result.StatusCode)
	}
	if string(result.Body) != "second target ok" {
		t.Fatalf("body must come from the failover target, got %q", result.Body)
	}
	// The dry provider must have been asked exactly twice — once up front, once
	// for the retry that found the pool empty — and must not have burned the
	// rest of its L2 budget re-asking.
	if got := res.calls["prov-"+providerSlug]; got != 2 {
		t.Fatalf("first provider resolved %d times, want 2 (initial + one retry that found the pool dry)", got)
	}
}

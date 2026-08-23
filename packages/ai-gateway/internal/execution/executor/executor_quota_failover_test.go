package executor

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	provtarget "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/target"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	configtypes "github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configtypes/policy"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/canonicalbridge"
	provbuiltins "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/builtins"
)

// The classification is a means; this is the end. A provider whose ACCOUNT
// budget is spent must hand the request to a different provider, because the
// exhaustion is account-scoped — every model behind it is equally unusable —
// and the alternates are already carried in the plan.
//
// Measured on a live deployment before the fix: `model: auto` picked an
// Anthropic model, the routing trace carried deepseek, openai and
// google-gemini alternates, the account was out of budget, and the caller got
// a 400 with no second attempt. The upstream files this under
// invalid_request_error, which reads as the caller's fault, and a caller's
// fault is correctly non-retryable — so the misclassification, not the retry
// logic, is what stopped the failover.
func TestExecute_ProviderQuotaExhausted_FailsOverToAnotherProvider(t *testing.T) {
	exhausted := &mockAdapter{format: provcore.FormatAnthropic, responses: []scripted{
		{err: &provcore.ProviderError{
			Status:  http.StatusBadRequest,
			Code:    provcore.CodeProviderQuotaExhausted,
			Message: "You have reached your specified API usage limits. You will regain access on 2026-09-01 at 00:00 UTC.",
		}},
	}}
	healthy := &mockAdapter{format: provcore.FormatOpenAI, responses: []scripted{
		{resp: &provcore.Response{StatusCode: 200, Body: []byte(`served by the alternate`)}},
	}}
	reg := provcore.NewRegistry()
	if err := reg.Register(exhausted); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := reg.Register(healthy); err != nil {
		t.Fatalf("register: %v", err)
	}
	reg.Freeze()

	// Three targets, and the middle one is the point: a SECOND model behind
	// the same exhausted account. A budget is account-scoped, so that sibling
	// is just as dead, and dispatching it spends a real upstream call to
	// learn what the first attempt already told us. With only two targets on
	// two providers the walk reaches the alternate either way and the gate
	// proves nothing.
	//
	// The resolver keys on the target's provider rather than a call sequence:
	// an eliminated target never reaches the resolver, so a positional script
	// would hand the third target the second one's call target.
	byProvider := map[string]provcore.CallTarget{
		"prov-anth": {ProviderName: "anthropic-target", Format: provcore.FormatAnthropic, BaseURL: "https://x", APIKey: "k"},
		"prov-oai":  {ProviderName: providerSlug, Format: provcore.FormatOpenAI, BaseURL: "https://api.example.com", APIKey: "sk-123"},
	}
	resolver := provtargetFunc(func(_ context.Context, providerID, _ string, _ provtarget.ResolveHints) (provcore.CallTarget, error) {
		ct, ok := byProvider[providerID]
		if !ok {
			return provcore.CallTarget{}, errors.New("no call target for " + providerID)
		}
		return ct, nil
	})

	exec := New(reg, resolver, nil, canonicalbridge.New(provbuiltins.SchemaCodecs(nil)))
	result := exec.Execute(context.Background(),
		[]routingcore.RoutingTarget{target("anth"), target("anth"), target("oai")},
		provcore.Request{
			WireShape:  typology.WireShapeOpenAIChat,
			BodyFormat: provcore.FormatOpenAI,
			Body:       []byte(`{"model":"auto","messages":[{"role":"user","content":"hi"}]}`),
		},
		configtypes.DefaultRetryPolicy(),
	)

	if result.Error != nil {
		t.Fatalf("the alternate provider should have served the request: %v", result.Error)
	}
	if result.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 from the alternate provider — an exhausted account must not sink a request that another provider can serve", result.StatusCode)
	}
	if !result.Attempts[0].Dispatched {
		t.Fatalf("the first attempt never reached the provider (%q) — the walk moved on for a reason other than the quota classification, so this gate would pass whatever classify returns", result.Attempts[0].Error)
	}
	// The sibling still earns an attempt ROW — a skip is a fact worth
	// recording — but it must not reach the provider.
	dispatched := 0
	for _, a := range result.Attempts {
		if a.Dispatched {
			dispatched++
		}
	}
	if dispatched != 2 {
		t.Fatalf("dispatched %d attempts, want 2 — the exhausted provider's SECOND model must be skipped, not called: %+v", dispatched, result.Attempts)
	}
	if result.Attempts[1].Dispatched {
		t.Errorf("the sibling model on the exhausted account was dispatched; a budget is account-scoped, so that call can only be told the same thing")
	}
	// The two attempts must land on DIFFERENT providers. Retrying the
	// exhausted one in place spends a turn to learn what we already know: the
	// budget is account-scoped, so its other models are just as unusable.
	var hitProviders []string
	for _, a := range result.Attempts {
		if a.Dispatched {
			hitProviders = append(hitProviders, a.Target.ProviderID)
		}
	}
	if hitProviders[0] == hitProviders[1] {
		t.Errorf("both dispatched attempts went to provider %q — an account budget is spent for every model behind it, so the walk has to leave the provider entirely", hitProviders[0])
	}
	if result.Attempts[0].StatusCode == http.StatusOK {
		t.Errorf("the first attempt should carry the exhausted provider's failure, got %d", result.Attempts[0].StatusCode)
	}
}

// The commonest deployment has one provider. When every target sits behind the
// exhausted account there is nothing to fail over TO, and what the caller gets
// then is the whole value of the classification: the upstream's own message
// names the reset instant, which is the only actionable thing anyone will see.
//
// Eliminating a target without surfacing its envelope turns that into
// "all upstream providers failed" — a 502 that tells an operator nothing and
// leaves the traffic row's errorCode unstamped, because the stamp needs a
// status >= 400 to copy.
func TestExecute_ProviderQuotaExhausted_SingleProvider_KeepsTheUpstreamMessage(t *testing.T) {
	const upstreamMsg = "You have reached your specified API usage limits. You will regain access on 2026-09-01 at 00:00 UTC."
	exhausted := &mockAdapter{format: provcore.FormatOpenAI, responses: []scripted{
		{err: &provcore.ProviderError{
			Status:  http.StatusBadRequest,
			Code:    provcore.CodeProviderQuotaExhausted,
			Message: upstreamMsg,
		}},
	}}
	reg := provcore.NewRegistry()
	if err := reg.Register(exhausted); err != nil {
		t.Fatalf("register: %v", err)
	}
	reg.Freeze()

	resolver := provtargetFunc(func(_ context.Context, _, _ string, _ provtarget.ResolveHints) (provcore.CallTarget, error) {
		return provcore.CallTarget{ProviderName: providerSlug, Format: provcore.FormatOpenAI, BaseURL: "https://api.example.com", APIKey: "sk-123"}, nil
	})

	exec := New(reg, resolver, nil, canonicalbridge.New(provbuiltins.SchemaCodecs(nil)))
	result := exec.Execute(context.Background(),
		[]routingcore.RoutingTarget{target("oai"), target("oai")},
		provcore.Request{
			WireShape:  typology.WireShapeOpenAIChat,
			BodyFormat: provcore.FormatOpenAI,
			Body:       []byte(`{"model":"auto","messages":[{"role":"user","content":"hi"}]}`),
		},
		configtypes.DefaultRetryPolicy(),
	)

	if result.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want the upstream's 400 — a generic 502 loses the reset date, which is the only thing the caller can act on", result.StatusCode)
	}
	terminal := result.Terminal()
	if terminal == nil {
		t.Fatal("no terminal attempt — the caller has nothing to read")
	}
	if !strings.Contains(terminal.Error, upstreamMsg) {
		t.Errorf("terminal error = %q, want it to carry the upstream's own text naming the reset instant", terminal.Error)
	}
	if terminal.Code != provcore.CodeProviderQuotaExhausted {
		t.Errorf("terminal code = %q, want %q so the traffic row can be stamped", terminal.Code, provcore.CodeProviderQuotaExhausted)
	}
}

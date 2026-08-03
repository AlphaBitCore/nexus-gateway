package aggregators

import (
	"testing"
	"time"

	alerteval "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/eval"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/observability/consumer"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/errorcode"
)

// These three run the rules the way production runs them: the seeded parameters
// from builtin.go, the event shape the gateway actually writes, and the real
// Tick() -> Decision path — not just the predicate.
//
// They exist because the unit tests next door would have passed against a rule
// that never fires. Both of these DID never fire, for their entire deployed
// life, and every test around them was green the whole time. A rule is only
// worth what it raises.
//
// End-to-end against the SEEDED production parameters
// (builtin.go: thresholdPct=10, windowSec=300, minSamples=20) and the event
// shape production actually writes, replaying the real distribution observed in
// the production database on 2026-07-15:
//
//	400 PROVIDER_ERROR 71 | 404 PROVIDER_ERROR 15 | 502 PROVIDER_UNAVAILABLE 10
//
// The claim under test is not "the predicate works" — the unit tests cover that.
// It is "this rule, wired as production wires it, tuned as production tunes it,
// fed what production feeds it, produces a Fire decision" — because it never has.
func TestE2E_ProviderUpstreamError_FiresOnProductionShapedTraffic(t *testing.T) {
	a := NewProviderUpstreamError()
	rt := alerteval.NewRuntime("provider.upstream_error", time.Now().Add(-time.Hour))
	now := time.Now()
	prodParams := map[string]any{"thresholdPct": 10, "windowSec": 300, "minSamples": 20}

	// 50 successes.
	for range 50 {
		a.OnEvent(rt, trafficEvent(now, &consumer.AlertView{
			RoutedProviderID: strPtr("p-openai"), StatusCode: intPtr(200),
		}))
	}
	// 10 real upstream 5xx, carrying the canonical code an adapter stamps.
	// 10/60 = 16.7% > 10% threshold, and 60 >= minSamples 20.
	for range 10 {
		a.OnEvent(rt, trafficEvent(now, &consumer.AlertView{
			RoutedProviderID: strPtr("p-openai"), StatusCode: intPtr(503),
			ErrorCode: strPtr(errorcode.UpstreamError),
		}))
	}

	d := fireFromTick(a.Tick(rt, prodParams, now))
	if d == nil || d.Action != alerteval.Fire {
		t.Fatalf("the rule did not fire on 16.7%% upstream 5xx against a 10%% threshold — this is what production traffic looks like, and this rule has never fired in its life; got %+v", d)
	}
	if d.TargetKey != "provider:p-openai" {
		t.Fatalf("fired against the wrong target: %q", d.TargetKey)
	}
	t.Logf("FIRED: %s — %s", d.TargetKey, d.Message)
}

// The other half of the guarantee: a provider must not be paged for OUR
// rejects. Same volume, same status codes, but every failure is a gateway
// decision. If this fires, the rule is worse than useless — it is a false page.
func TestE2E_ProviderUpstreamError_SilentOnGatewayRejects(t *testing.T) {
	a := NewProviderUpstreamError()
	rt := alerteval.NewRuntime("provider.upstream_error", time.Now().Add(-time.Hour))
	now := time.Now()
	prodParams := map[string]any{"thresholdPct": 10, "windowSec": 300, "minSamples": 20}

	for range 50 {
		a.OnEvent(rt, trafficEvent(now, &consumer.AlertView{
			RoutedProviderID: strPtr("p-openai"), StatusCode: intPtr(200),
		}))
	}
	// 30 gateway-side 5xx — far past the threshold if they were counted.
	for range 30 {
		a.OnEvent(rt, trafficEvent(now, &consumer.AlertView{
			RoutedProviderID: strPtr("p-openai"), StatusCode: intPtr(502),
			ErrorCode: strPtr("PROVIDER_UNAVAILABLE"),
		}))
	}

	if d := fireFromTick(a.Tick(rt, prodParams, now)); d != nil && d.Action == alerteval.Fire {
		t.Fatalf("fired on 30 gateway-side rejects — that pages a provider for our own failure to route: %+v", d)
	}
}

// The credential cascade, same treatment: seeded params
// (thresholdPct=20, windowSec=600, minSamples=10), production event shape.
func TestE2E_CredentialAuthFailuresCascade_FiresOnDyingKey(t *testing.T) {
	a := NewCredentialAuthFailuresCascade()
	rt := alerteval.NewRuntime("credential.auth_failures_cascade", time.Now().Add(-time.Hour))
	now := time.Now()
	prodParams := map[string]any{"thresholdPct": 20, "windowSec": 600, "minSamples": 10}

	for range 10 {
		a.OnEvent(rt, trafficEvent(now, &consumer.AlertView{
			CredentialID: strPtr("c-openai-prod"), StatusCode: intPtr(200),
		}))
	}
	// 5 upstream 401s = the key is being rejected. 5/15 = 33% > 20%.
	for range 5 {
		a.OnEvent(rt, trafficEvent(now, &consumer.AlertView{
			CredentialID: strPtr("c-openai-prod"), StatusCode: intPtr(401),
			ErrorCode: strPtr(errorcode.AuthFailed),
		}))
	}

	d := fireFromTick(a.Tick(rt, prodParams, now))
	if d == nil || d.Action != alerteval.Fire {
		t.Fatalf("a key being rejected by the upstream 33%% of the time did not raise the cascade; got %+v", d)
	}
	t.Logf("FIRED: %s — %s", d.TargetKey, d.Message)
}

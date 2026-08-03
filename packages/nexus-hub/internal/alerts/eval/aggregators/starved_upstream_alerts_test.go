package aggregators

import (
	"testing"
	"time"

	alerteval "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/eval"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/observability/consumer"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/errorcode"
)

// ratio returns the fired/total counts the window accumulated for a target, so
// a test can assert what the rule actually counted rather than only whether it
// crossed a threshold.
func ratio(t *testing.T, rt *alerteval.Runtime, target string, windowSec int, now time.Time) (fired, total float64) {
	t.Helper()
	w := rt.Window(target, windowSec)
	return w.Sum(time.Duration(windowSec)*time.Second, now)
}

// A 502 carrying upstream_error is THE event provider.upstream_error exists to
// count. It is also exactly what the gateway writes today: every upstream
// failure is classified, so error_code is never empty on one.
//
// The rule selected on `error_code == ""` — a contract traffic.prisma documents
// but the gateway does not honour — so it counted nothing and had never fired
// in production. Verified against the live database: zero 5xx rows carry an
// empty error_code.
func TestProviderUpstreamError_CountsClassifiedUpstreamFailures(t *testing.T) {
	a := NewProviderUpstreamError()
	rt := alerteval.NewRuntime("provider.upstream_error", time.Now().Add(-time.Hour))
	now := time.Now()

	a.OnEvent(rt, trafficEvent(now, &consumer.AlertView{
		RoutedProviderID: strPtr("p-openai"),
		StatusCode:       intPtr(502),
		ErrorCode:        strPtr(errorcode.UpstreamError),
	}))

	fired, total := ratio(t, rt, "provider:p-openai", 600, now)
	if total != 1 {
		t.Fatalf("event was not observed at all: total=%v", total)
	}
	if fired != 1 {
		t.Fatalf("a 502 with error_code=%q was not counted as an upstream failure (fired=%v) — this is the exact event the rule exists for, and the shape the gateway actually writes",
			errorcode.UpstreamError, fired)
	}
}

// The rule's whole purpose is to blame providers only for provider faults. A
// gateway-side reject must be observed (it is traffic) but must not count
// against the provider — that is what the empty-string check was protecting,
// and the protection has to survive.
func TestProviderUpstreamError_DoesNotBlameProviderForGatewayRejects(t *testing.T) {
	for _, code := range []string{"ROUTING_NO_MATCH", "QUOTA_EXCEEDED", "MODEL_NOT_ALLOWED", "PROVIDER_UNAVAILABLE"} {
		t.Run(code, func(t *testing.T) {
			a := NewProviderUpstreamError()
			rt := alerteval.NewRuntime("provider.upstream_error", time.Now().Add(-time.Hour))
			now := time.Now()

			a.OnEvent(rt, trafficEvent(now, &consumer.AlertView{
				RoutedProviderID: strPtr("p-openai"),
				StatusCode:       intPtr(502),
				ErrorCode:        strPtr(code),
			}))

			fired, total := ratio(t, rt, "provider:p-openai", 600, now)
			if total != 1 {
				t.Fatalf("the event should still be observed as traffic: total=%v", total)
			}
			if fired != 0 {
				t.Fatalf("%s is a gateway decision — counting it would blame the provider for our own reject (fired=%v)", code, fired)
			}
		})
	}
}

// no_compatible_provider lives in the canonical set but describes a gateway
// decision: no target could serve the request, so no upstream was ever asked.
// It must not count against a provider.
func TestProviderUpstreamError_NoCompatibleProviderIsNotTheProvidersFault(t *testing.T) {
	a := NewProviderUpstreamError()
	rt := alerteval.NewRuntime("provider.upstream_error", time.Now().Add(-time.Hour))
	now := time.Now()

	a.OnEvent(rt, trafficEvent(now, &consumer.AlertView{
		RoutedProviderID: strPtr("p-openai"),
		StatusCode:       intPtr(502),
		ErrorCode:        strPtr(errorcode.NoCompatibleProvider),
	}))

	fired, _ := ratio(t, rt, "provider:p-openai", 600, now)
	if fired != 0 {
		t.Fatalf("no_compatible_provider means we never reached the upstream; counting it blames a provider we did not call (fired=%v)", fired)
	}
}

// A success must never count as a failure.
func TestProviderUpstreamError_SuccessDoesNotCount(t *testing.T) {
	a := NewProviderUpstreamError()
	rt := alerteval.NewRuntime("provider.upstream_error", time.Now().Add(-time.Hour))
	now := time.Now()

	a.OnEvent(rt, trafficEvent(now, &consumer.AlertView{
		RoutedProviderID: strPtr("p-openai"),
		StatusCode:       intPtr(200),
	}))

	fired, total := ratio(t, rt, "provider:p-openai", 600, now)
	if total != 1 || fired != 0 {
		t.Fatalf("a 200 must be observed but not counted: fired=%v total=%v", fired, total)
	}
}

// credential.auth_failures_cascade is the rule that tells an operator a key is
// dying. auth_failed is precisely that event, and precisely what the gateway
// writes for an upstream 401/403 — so the rule keyed on an empty error_code
// could not see the failures it exists to catch.
func TestCredentialAuthFailuresCascade_CountsAuthFailed(t *testing.T) {
	a := NewCredentialAuthFailuresCascade()
	rt := alerteval.NewRuntime("credential.auth_failures_cascade", time.Now().Add(-time.Hour))
	now := time.Now()

	a.OnEvent(rt, trafficEvent(now, &consumer.AlertView{
		CredentialID: strPtr("c-1"),
		StatusCode:   intPtr(401),
		ErrorCode:    strPtr(errorcode.AuthFailed),
	}))

	fired, total := ratio(t, rt, "cred:c-1", 1200, now)
	if total != 1 {
		t.Fatalf("event not observed: total=%v", total)
	}
	if fired != 1 {
		t.Fatalf("an upstream 401 with error_code=%q is the cascade this rule watches for, and it was not counted (fired=%v)",
			errorcode.AuthFailed, fired)
	}
}

// A 401 the GATEWAY produced — a bad virtual key — says nothing about the
// upstream credential. Counting it would open a credential's cascade alert
// because a caller typo'd their own key.
func TestCredentialAuthFailuresCascade_IgnoresGatewaySideAuthRejects(t *testing.T) {
	for _, code := range []string{"AUTH_INVALID_KEY", "AUTH_KEY_MISSING"} {
		t.Run(code, func(t *testing.T) {
			a := NewCredentialAuthFailuresCascade()
			rt := alerteval.NewRuntime("credential.auth_failures_cascade", time.Now().Add(-time.Hour))
			now := time.Now()

			a.OnEvent(rt, trafficEvent(now, &consumer.AlertView{
				CredentialID: strPtr("c-1"),
				StatusCode:   intPtr(401),
				ErrorCode:    strPtr(code),
			}))

			fired, _ := ratio(t, rt, "cred:c-1", 1200, now)
			if fired != 0 {
				t.Fatalf("%s is the caller's key being wrong, not ours — it must not count toward the credential's cascade (fired=%v)", code, fired)
			}
		})
	}
}

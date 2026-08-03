// Peer-URL wiring: the reported privateURL derivation (effectivePrivateURL)
// and the onboarding 407 CP-UI base provider (override → yaml, default →
// Hub-resolved Control Plane publicURL).
package wiring

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/cmd/compliance-proxy/config"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/peerurl"
)

// effectivePrivateURL: the yaml/env override wins verbatim (trailing slash
// trimmed); otherwise the URL is derived from the runtime-API listen port.
func TestEffectivePrivateURL_OverrideWins(t *testing.T) {
	cfg := &config.Config{}
	cfg.PrivateURL = "http://cp-proxy.internal:3040/"
	cfg.RuntimeAPI.ListenAddress = "127.0.0.1:9999" // must be ignored

	if got := effectivePrivateURL(cfg); got != "http://cp-proxy.internal:3040" {
		t.Fatalf("effectivePrivateURL = %q; want the trimmed override", got)
	}
}

// Without an override the runtime-API listen address is what peers dial —
// HOST included: every shipped config binds the runtime API to loopback, and
// a loopback-bound socket refuses dials to the machine's LAN IP, so a
// loopback bind must ADVERTISE loopback.
func TestEffectivePrivateURL_LoopbackBindAdvertisesLoopback(t *testing.T) {
	cfg := &config.Config{}
	cfg.RuntimeAPI.ListenAddress = "127.0.0.1:3040"

	if got := effectivePrivateURL(cfg); got != "http://127.0.0.1:3040" {
		t.Fatalf("effectivePrivateURL = %q; want the loopback bind advertised verbatim", got)
	}
}

// A wildcard bind (all interfaces) falls back to the primary-IP derivation.
// A sandbox without a route yields "" — also correct (not-yet-reported).
func TestEffectivePrivateURL_WildcardBindDerivesFromPrimaryIP(t *testing.T) {
	cfg := &config.Config{}
	cfg.RuntimeAPI.ListenAddress = "0.0.0.0:3040"

	got := effectivePrivateURL(cfg)
	if got == "" {
		t.Skip("no primary outbound IP in this environment; derivation correctly reports empty")
	}
	if !strings.HasPrefix(got, "http://") || !strings.HasSuffix(got, ":3040") {
		t.Fatalf("effectivePrivateURL = %q; want http://<primary-ip>:3040 (runtime-API port)", got)
	}
}

// A malformed listen address means no port to derive from → "" (the resolver
// side treats an absent privateUrl as not-yet-reported), never a bogus URL.
func TestEffectivePrivateURL_MalformedListenAddressYieldsEmpty(t *testing.T) {
	cfg := &config.Config{}
	cfg.RuntimeAPI.ListenAddress = "not-a-host-port"

	if got := effectivePrivateURL(cfg); got != "" {
		t.Fatalf("effectivePrivateURL = %q; want empty for an unparseable listen address", got)
	}
}

// cpHubStub stands up a fake Hub answering the control-plane service-url
// resolution with the given handler.
func cpHubStub(t *testing.T, handler http.HandlerFunc) *peerurl.Resolver {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return peerurl.New(srv.URL, "test-token")
}

// Override set → the override is used, resolver untouched (it would fail).
func TestOnboardingCPUIBaseURLProvider_OverrideWins(t *testing.T) {
	resolver := cpHubStub(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("resolver must not be consulted when the override is set")
		w.WriteHeader(http.StatusInternalServerError)
	})

	provider := onboardingCPUIBaseURLProvider("https://cp.vanity.example.com/", resolver)
	if got := provider(); got != "https://cp.vanity.example.com" {
		t.Fatalf("provider() = %q; want the trimmed override", got)
	}
}

// No override → the Hub-resolved Control Plane PUBLIC URL is used (display
// link targets end users; never the private URL).
func TestOnboardingCPUIBaseURLProvider_DefaultsToResolvedCPPublicURL(t *testing.T) {
	resolver := cpHubStub(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/internal/things/service-url/control-plane" {
			t.Errorf("resolver hit %q; want the control-plane service-url path", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"thingType":"control-plane","privateUrl":"http://10.0.0.2:3001","publicUrl":"https://cp.example.com"}`))
	})

	provider := onboardingCPUIBaseURLProvider("", resolver)
	if got := provider(); got != "https://cp.example.com" {
		t.Fatalf("provider() = %q; want the Hub-resolved CP public URL", got)
	}
}

// Resolver error (CP not reported yet / Hub down) → "" so the 407 page
// degrades the link exactly like an unset static value — and never panics.
func TestOnboardingCPUIBaseURLProvider_ResolverErrorYieldsEmpty(t *testing.T) {
	resolver := cpHubStub(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"code":"SERVICE_URL_NOT_REPORTED"}`))
	})

	provider := onboardingCPUIBaseURLProvider("", resolver)
	if got := provider(); got != "" {
		t.Fatalf("provider() = %q; want empty while the CP URL is unresolved", got)
	}
}

// Nil resolver (defensive wiring) behaves like unresolved: empty, no panic.
func TestOnboardingCPUIBaseURLProvider_NilResolverYieldsEmpty(t *testing.T) {
	provider := onboardingCPUIBaseURLProvider("", nil)
	if got := provider(); got != "" {
		t.Fatalf("provider() = %q; want empty with a nil resolver", got)
	}
}

// InitCompliance builds the process-wide peer resolver even when the
// compliance kernel is disabled — the onboarding 407 link default needs it.
func TestInitCompliance_Disabled_StillBuildsPeerResolver(t *testing.T) {
	cfg := &config.Config{}
	cfg.Compliance.Enabled = false
	cfg.Registry.NexusHubURL = "http://hub.example.com"
	cfg.Auth.InternalServiceToken = "tok"

	res, err := InitCompliance(cfg, nil, nil, testLogger())
	if err != nil {
		t.Fatalf("InitCompliance (disabled): %v", err)
	}
	if res.PeerResolver == nil {
		t.Fatal("PeerResolver must be built even with the compliance kernel disabled")
	}
}

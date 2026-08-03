package errorcode

import "testing"

// These strings are a cross-service contract carried in traffic_event.error_code:
// the AI Gateway writes them, the Hub's alert aggregators select on them. The
// coupling runs through a database column, so a drifted value cannot produce a
// compile error on the reading side — it produces an alert that silently stops
// firing. This pins the wire values so a rename has to be deliberate.
func TestWireValues(t *testing.T) {
	for _, tc := range []struct{ got, want string }{
		{InvalidRequest, "invalid_request"},
		{AuthFailed, "auth_failed"},
		{RateLimited, "rate_limited"},
		{Timeout, "timeout"},
		{UpstreamError, "upstream_error"},
		{EndpointUnsupported, "endpoint_unsupported"},
		{ContextOverflow, "context_overflow"},
		{NotImplemented, "not_implemented"},
		{NoCompatibleProvider, "no_compatible_provider"},
	} {
		if tc.got != tc.want {
			t.Errorf("wire value drifted: got %q, want %q", tc.got, tc.want)
		}
	}
}

// The whole point of this package: let a consumer ask "did the upstream reject
// this, or did we?" without hardcoding a list it will forget to update.
//
// provider_upstream_error and credential_auth_failures_cascade used to answer
// it with `error_code == ""` — a contract the gateway broke by always
// classifying, leaving both rules unable to fire at all.
func TestIsUpstream(t *testing.T) {
	for _, code := range []string{InvalidRequest, AuthFailed, RateLimited, Timeout,
		UpstreamError, EndpointUnsupported, ContextOverflow, NotImplemented} {
		if !IsUpstream(code) {
			t.Errorf("IsUpstream(%q) = false — an upstream rejection must be recognisable as one", code)
		}
	}
	// Gateway-side decisions must NOT read as upstream failures: attributing our
	// own reject to a provider is what the empty-string check existed to prevent.
	for _, code := range []string{"", "ROUTING_NO_MATCH", "QUOTA_EXCEEDED", "MODEL_NOT_ALLOWED",
		"CLIENT_CLOSED", "AUTH_INVALID_KEY", "PROVIDER_UNAVAILABLE", "PROVIDER_RATE_LIMITED"} {
		if IsUpstream(code) {
			t.Errorf("IsUpstream(%q) = true — this is a gateway-side decision, not an upstream failure", code)
		}
	}
}

// no_compatible_provider is the one canonical code that describes a GATEWAY
// decision: no target could serve the request, so no upstream was ever asked.
// Counting it as an upstream failure would blame a provider for our routing.
func TestIsUpstream_NoCompatibleProviderIsNotUpstream(t *testing.T) {
	if IsUpstream(NoCompatibleProvider) {
		t.Error("no_compatible_provider means we never reached an upstream — it must not count as an upstream failure")
	}
}

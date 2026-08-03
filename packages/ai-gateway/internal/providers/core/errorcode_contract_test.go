package core

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/errorcode"
)

// The canonical codes exist in two places on purpose: providers/core owns them
// for the adapters that produce them, and shared/schemas/errorcode owns them for
// the Hub, which reads them out of traffic_event.error_code and cannot import
// this package.
//
// Duplication is the price of that boundary; silent divergence is not. If these
// drift, the gateway keeps writing one value while the Hub's alert rules select
// on another — no compile error, no failing test, just an alert that quietly
// stops firing. That failure mode already cost provider_upstream_error its
// entire production life.
//
// This test is the seam that makes the drift loud.
func TestCanonicalCodes_MatchSharedVocabulary(t *testing.T) {
	for _, tc := range []struct {
		name   string
		local  string
		shared string
	}{
		{"invalid_request", CodeInvalidRequest, errorcode.InvalidRequest},
		{"auth_failed", CodeAuthFailed, errorcode.AuthFailed},
		{"rate_limited", CodeRateLimited, errorcode.RateLimited},
		{"timeout", CodeTimeout, errorcode.Timeout},
		{"upstream_error", CodeUpstreamError, errorcode.UpstreamError},
		{"endpoint_unsupported", CodeEndpointUnsupported, errorcode.EndpointUnsupported},
		{"context_overflow", CodeContextOverflow, errorcode.ContextOverflow},
		{"not_implemented", CodeNotImplemented, errorcode.NotImplemented},
		{"no_compatible_provider", CodeNoCompatibleProvider, errorcode.NoCompatibleProvider},
	} {
		if tc.local != tc.shared {
			t.Errorf("%s: providers/core has %q, shared/schemas/errorcode has %q — the Hub reads the shared value, so this divergence silently breaks its alert rules",
				tc.name, tc.local, tc.shared)
		}
	}
}

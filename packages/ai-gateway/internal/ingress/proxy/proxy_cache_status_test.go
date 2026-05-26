package proxy

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/freshness"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
)

// TestClassifyCachePreLookup covers the short-circuit branches of
// the response-cache phase. The function returns the
// (GatewayCacheStatus, GatewayCacheSkipReason) pair: ("", "") means the
// caller proceeds to BuildKey + Lookup; (Skipped, <reason>) means the
// caller short-circuits. Streaming requests are cacheable so they take
// the lookup path; the legacy SKIP_STREAM short-circuit is gone.
//
// A non-nil detector + skipTimeSensitive=true fires
// (Skipped, time_sensitive) when the last user message matches a compiled
// freshness rule.
func TestClassifyCachePreLookup(t *testing.T) {
	// stubDetector implements timeSensitiveDetector and always reports
	// time-sensitive for any non-empty message slice.
	alwaysSensitive := &stubTimeSensitiveDetector{matched: true}

	tests := []struct {
		name                                                            string
		cacheEnabled, hasNoCacheHeader, targets, passthroughBypassCache bool
		detector                                                        timeSensitiveDetector
		msgs                                                            []freshness.ChatMessage
		skipTimeSensitive                                               bool
		wantStatus                                                      audit.GatewayCacheStatus
		wantReason                                                      audit.GatewayCacheSkipReason
	}{
		// Cache off short-circuits before all other checks (matches
		// production: a nil cache module never sees a request).
		{
			name: "cache disabled wins over no-cache header",
			cacheEnabled: false, hasNoCacheHeader: true, targets: true, passthroughBypassCache: false,
			wantStatus: audit.GatewayCacheSkipped, wantReason: audit.GatewayCacheSkipReasonDisabled,
		},
		{
			name: "cache disabled with no targets",
			cacheEnabled: false, targets: false,
			wantStatus: audit.GatewayCacheSkipped, wantReason: audit.GatewayCacheSkipReasonDisabled,
		},

		// No-cache header skip when cache enabled and targets present.
		{
			name: "client opt-out",
			cacheEnabled: true, hasNoCacheHeader: true, targets: true,
			wantStatus: audit.GatewayCacheSkipped, wantReason: audit.GatewayCacheSkipReasonNoCache,
		},

		// Empty target list — defensive Skipped + disabled reason.
		{
			name: "empty targets",
			cacheEnabled: true, targets: false,
			wantStatus: audit.GatewayCacheSkipped, wantReason: audit.GatewayCacheSkipReasonDisabled,
		},

		// Happy path: caller proceeds to BuildKey + Lookup.
		{name: "proceed to lookup", cacheEnabled: true, targets: true, wantStatus: "", wantReason: ""},
		// Streaming requests now also proceed (cacheable).
		{name: "streaming proceeds to lookup", cacheEnabled: true, targets: true, wantStatus: "", wantReason: ""},

		// passthroughBypassCache wins over the no-cache header
		// (operator-forced emergency bypass takes precedence over
		// end-user-supplied control header) but loses to cache disabled
		// / no targets (those are precondition failures).
		{
			name: "passthrough bypass when cache enabled",
			cacheEnabled: true, targets: true, passthroughBypassCache: true,
			wantStatus: audit.GatewayCacheSkipped, wantReason: audit.GatewayCacheSkipReasonPassthrough,
		},
		{
			name: "passthrough overrides client no-cache",
			cacheEnabled: true, hasNoCacheHeader: true, targets: true, passthroughBypassCache: true,
			wantStatus: audit.GatewayCacheSkipped, wantReason: audit.GatewayCacheSkipReasonPassthrough,
		},
		{
			name: "cache disabled still wins over passthrough",
			cacheEnabled: false, targets: true, passthroughBypassCache: true,
			wantStatus: audit.GatewayCacheSkipped, wantReason: audit.GatewayCacheSkipReasonDisabled,
		},

		// Time-sensitive detection.
		{
			name:         "time_sensitive with detector + policy + matching message",
			cacheEnabled: true, targets: true,
			detector: alwaysSensitive, msgs: []freshness.ChatMessage{{Role: "user", Content: "what time is it?"}},
			skipTimeSensitive: true,
			wantStatus: audit.GatewayCacheSkipped, wantReason: audit.GatewayCacheSkipReasonTimeSensitive,
		},
		{
			name:         "time_sensitive detector but policy flag off → proceeds",
			cacheEnabled: true, targets: true,
			detector: alwaysSensitive, msgs: []freshness.ChatMessage{{Role: "user", Content: "what time is it?"}},
			skipTimeSensitive: false,
			wantStatus: "", wantReason: "",
		},
		{
			name:         "time_sensitive policy on but nil detector → proceeds",
			cacheEnabled: true, targets: true,
			detector: nil, msgs: []freshness.ChatMessage{{Role: "user", Content: "what time is it?"}},
			skipTimeSensitive: true,
			wantStatus: "", wantReason: "",
		},
		{
			name:         "time_sensitive policy on + detector + no messages → proceeds",
			cacheEnabled: true, targets: true,
			detector: alwaysSensitive, msgs: nil,
			skipTimeSensitive: true,
			wantStatus: "", wantReason: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotStatus, gotReason := classifyCachePreLookup(
				tc.cacheEnabled, tc.hasNoCacheHeader, tc.targets, tc.passthroughBypassCache,
				tc.detector, tc.msgs, tc.skipTimeSensitive,
			)
			if gotStatus != tc.wantStatus || gotReason != tc.wantReason {
				t.Fatalf("classifyCachePreLookup = (%q, %q), want (%q, %q)",
					gotStatus, gotReason, tc.wantStatus, tc.wantReason)
			}
		})
	}
}

// stubTimeSensitiveDetector is a test double for timeSensitiveDetector.
type stubTimeSensitiveDetector struct {
	matched bool
}

func (s *stubTimeSensitiveDetector) IsTimeSensitive(messages []freshness.ChatMessage) (bool, string) {
	if len(messages) == 0 {
		return false, ""
	}
	return s.matched, "stub"
}

package proxy

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/freshness"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
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
		endpointKind                                                    typology.EndpointKind
		cacheEnabled, hasNoCacheHeader, targets, passthroughBypassCache bool
		detector                                                        timeSensitiveDetector
		msgs                                                            []freshness.ChatMessage
		skipTimeSensitive                                               bool
		wantStatus                                                      audit.GatewayCacheStatus
		wantReason                                                      audit.GatewayCacheSkipReason
	}{
		// Embeddings endpoint short-circuits BEFORE every other check —
		// even when the cache is enabled with targets, and even when other
		// skip conditions (no-cache header, passthrough) would also fire.
		{
			name:         "embeddings short-circuits even with cache enabled + targets",
			endpointKind: typology.EndpointKindEmbeddings,
			cacheEnabled: true, targets: true,
			wantStatus: audit.GatewayCacheSkipped, wantReason: audit.GatewayCacheSkipReasonEmbeddingsEndpoint,
		},
		{
			name:         "embeddings wins over cache disabled / no targets",
			endpointKind: typology.EndpointKindEmbeddings,
			cacheEnabled: false, targets: false,
			wantStatus: audit.GatewayCacheSkipped, wantReason: audit.GatewayCacheSkipReasonEmbeddingsEndpoint,
		},
		{
			name:         "embeddings wins over passthrough + no-cache header",
			endpointKind: typology.EndpointKindEmbeddings,
			cacheEnabled: true, hasNoCacheHeader: true, targets: true, passthroughBypassCache: true,
			wantStatus: audit.GatewayCacheSkipped, wantReason: audit.GatewayCacheSkipReasonEmbeddingsEndpoint,
		},
		{
			name:         "chat endpoint with cache enabled proceeds (not short-circuited)",
			endpointKind: typology.EndpointKindChat,
			cacheEnabled: true, targets: true,
			wantStatus: "", wantReason: "",
		},
		// Multimodal endpoints short-circuit like embeddings: endpoint-driven,
		// regardless of admin cache config — generative image variety is the
		// product, TTS/STT payloads are binary/derived, a cached transcript
		// would be PII-at-rest. No per-modality cache knob exists by design.
		{
			name:         "image_generation short-circuits even with cache enabled + targets",
			endpointKind: typology.EndpointKindImageGeneration,
			cacheEnabled: true, targets: true,
			wantStatus: audit.GatewayCacheSkipped, wantReason: audit.GatewayCacheSkipReasonModalityEndpoint,
		},
		{
			name:         "tts short-circuits even with cache enabled + targets",
			endpointKind: typology.EndpointKindTTS,
			cacheEnabled: true, targets: true,
			wantStatus: audit.GatewayCacheSkipped, wantReason: audit.GatewayCacheSkipReasonModalityEndpoint,
		},
		{
			name:         "stt short-circuits and wins over no-cache header + passthrough",
			endpointKind: typology.EndpointKindSTT,
			cacheEnabled: true, hasNoCacheHeader: true, targets: true, passthroughBypassCache: true,
			wantStatus: audit.GatewayCacheSkipped, wantReason: audit.GatewayCacheSkipReasonModalityEndpoint,
		},
		{
			name:         "rerank short-circuits with its own reason, endpoint-driven",
			endpointKind: typology.EndpointKindRerank,
			cacheEnabled: true, targets: true,
			wantStatus: audit.GatewayCacheSkipped, wantReason: audit.GatewayCacheSkipReasonRerankEndpoint,
		},
		// Cache off short-circuits before all other checks (matches
		// production: a nil cache module never sees a request).
		{
			name:         "cache disabled wins over no-cache header",
			cacheEnabled: false, hasNoCacheHeader: true, targets: true, passthroughBypassCache: false,
			wantStatus: audit.GatewayCacheSkipped, wantReason: audit.GatewayCacheSkipReasonDisabled,
		},
		{
			// Both conditions hold. "disabled" must win: turning a tier on is the
			// actionable first step, and reporting no_targets here would send the
			// operator to their routing rules while caching was off anyway.
			name:         "cache disabled AND no targets → disabled wins",
			cacheEnabled: false, targets: false,
			wantStatus: audit.GatewayCacheSkipped, wantReason: audit.GatewayCacheSkipReasonDisabled,
		},

		// No-cache header skip when cache enabled and targets present.
		{
			name:         "client opt-out",
			cacheEnabled: true, hasNoCacheHeader: true, targets: true,
			wantStatus: audit.GatewayCacheSkipped, wantReason: audit.GatewayCacheSkipReasonNoCache,
		},

		// Empty target list with the tiers ON is a ROUTING outcome, not a config
		// posture, so it gets its own reason. While both stamped "disabled",
		// traffic_event.gateway_cache_skip_reason and
		// nexus_cache_lookups_total{result} could not tell an operator whether to
		// look at the cache settings or at their routing rules.
		{
			name:         "empty targets while cache is enabled → no_targets, not disabled",
			cacheEnabled: true, targets: false,
			wantStatus: audit.GatewayCacheSkipped, wantReason: audit.GatewayCacheSkipReasonNoTargets,
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
			name:         "passthrough bypass when cache enabled",
			cacheEnabled: true, targets: true, passthroughBypassCache: true,
			wantStatus: audit.GatewayCacheSkipped, wantReason: audit.GatewayCacheSkipReasonPassthrough,
		},
		{
			name:         "passthrough overrides client no-cache",
			cacheEnabled: true, hasNoCacheHeader: true, targets: true, passthroughBypassCache: true,
			wantStatus: audit.GatewayCacheSkipped, wantReason: audit.GatewayCacheSkipReasonPassthrough,
		},
		{
			name:         "cache disabled still wins over passthrough",
			cacheEnabled: false, targets: true, passthroughBypassCache: true,
			wantStatus: audit.GatewayCacheSkipped, wantReason: audit.GatewayCacheSkipReasonDisabled,
		},

		// Time-sensitive detection.
		{
			name:         "time_sensitive with detector + policy + matching message",
			cacheEnabled: true, targets: true,
			detector: alwaysSensitive, msgs: []freshness.ChatMessage{{Role: "user", Content: "what time is it?"}},
			skipTimeSensitive: true,
			wantStatus:        audit.GatewayCacheSkipped, wantReason: audit.GatewayCacheSkipReasonTimeSensitive,
		},
		{
			name:         "time_sensitive detector but policy flag off → proceeds",
			cacheEnabled: true, targets: true,
			detector: alwaysSensitive, msgs: []freshness.ChatMessage{{Role: "user", Content: "what time is it?"}},
			skipTimeSensitive: false,
			wantStatus:        "", wantReason: "",
		},
		{
			name:         "time_sensitive policy on but nil detector → proceeds",
			cacheEnabled: true, targets: true,
			detector: nil, msgs: []freshness.ChatMessage{{Role: "user", Content: "what time is it?"}},
			skipTimeSensitive: true,
			wantStatus:        "", wantReason: "",
		},
		{
			name:         "time_sensitive policy on + detector + no messages → proceeds",
			cacheEnabled: true, targets: true,
			detector: alwaysSensitive, msgs: nil,
			skipTimeSensitive: true,
			wantStatus:        "", wantReason: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotStatus, gotReason := classifyCachePreLookup(
				tc.endpointKind,
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

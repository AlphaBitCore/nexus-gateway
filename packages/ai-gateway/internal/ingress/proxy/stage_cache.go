// stage_cache.go — the cache stage of the proxy stage chain: pre-lookup
// classification (disabled / no-cache header / passthrough bypass /
// freshness), upstream body preparation with cross-format
// canonicalization, the L1 exact-match lookup (stream + non-stream HIT
// replay), and the L2 semantic lookup on L1 miss. Owns
// proxyState.cacheKey / gatewayCacheStatus / gatewayCacheSkipReason /
// cachePrepared*.
package proxy

import (
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/freshness"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// cacheStage consults the response cache before any upstream dispatch.
//
// Phase 5.5: Cache lookup. Every non-rejected request
// takes exactly one of these paths:
//   - DISABLED / SKIP_NO_CACHE → fall through to live upstream;
//     no cache key, no broker, no Redis touch.
//   - HIT (Redis): replay the cached chunk timeline (stream) or
//     re-encode the cached canonical response (non-stream)
//     through the same downstream pipeline used for MISS;
//     hooks always run (D2).
//   - MISS (broker): subscribe to streamcache.Registry. The
//     first subscriber stamps MISS and triggers leaderFn;
//     joiners stamp HIT_LIVE and consume the in-flight stream.
//     On the broker's terminal frame the cache layer persists
//     the timeline so subsequent cold lookups become true HITs.
//
// The cache key uses the bytes that WILL be sent upstream
// (output of adapter.PrepareBody) so equivalent requests
// (different client model aliases, different SDK JSON key
// orderings) hash to the same key.
type cacheStage struct{ s *proxyState }

func (st cacheStage) run() bool {
	s := st.s
	h := s.h

	// passthroughBypassCache short-circuits the cache lookup entirely
	// (and therefore also any cache-write later, since writes only
	// happen on misses that ran a lookup). The bypass takes precedence
	// over the client header so an operator forcing passthrough cannot
	// be overridden by an end-user header.
	passthroughBypassCache := false
	if pt := s.resolvedReq.Passthrough(); pt.AnyBypassActive() && pt.BypassCache {
		passthroughBypassCache = true
	}
	// L1 (extract / exact-match) and L2 (semantic) are INDEPENDENT tiers, each
	// with its own enable flag. The cache stage is active when EITHER is on;
	// each lookup is gated on its own flag below. This decouples them — an admin
	// can run L2 with L1 off (or the reverse) and the stage no longer
	// short-circuits on L1 alone. freshness rules (apply_freshness_rules) apply
	// across both tiers; a freshness match skips ALL cache operations.
	l1Enabled := h.deps.Cache != nil && h.deps.Cache.IsEnabled()
	l2Enabled := h.deps.SemanticReader != nil &&
		h.deps.SemanticConfigCache != nil && h.deps.SemanticConfigCache.EffectiveEnabled()
	cacheEnabled := l1Enabled || l2Enabled
	// Project canonical NormalizedPayload messages → freshness.ChatMessage
	// for the time-sensitivity detector. Computed ONLY when a cache tier is
	// active — pulling rctxFull.Normalized() materializes the lazy canonical,
	// and the freshness detector is meaningless when all tiers are off. On the
	// lean (cache-off) path this leaves canonicalMsgs nil and the canonical
	// uncomputed. Nil canonical / empty messages → nil slice → detector returns
	// false (fail-open).
	var canonicalMsgs []freshness.ChatMessage
	if cacheEnabled {
		if np := s.rctxFull.Normalized(); np != nil {
			canonicalMsgs = normMessagesToFreshness(np.Messages)
		}
	}
	// skipTimeSensitivePolicy reads the apply_freshness_rules gate
	// so freshness-rule matches actually skip cache.
	preLookupStatus, preLookupSkipReason := classifyCachePreLookup(
		typology.KindFromWireShape(s.resolved.WireShape),
		cacheEnabled,
		s.r.Header.Get("x-nexus-aigw-no-cache") != "",
		len(s.routeResult.Targets) > 0,
		passthroughBypassCache,
		h.deps.FreshnessDetector,
		canonicalMsgs,
		h.deps.Cache.ApplyFreshnessRules(),
	)
	switch preLookupStatus {
	case audit.GatewayCacheSkipped:
		s.gatewayCacheStatus = preLookupStatus
		s.gatewayCacheSkipReason = preLookupSkipReason
		switch preLookupSkipReason {
		case audit.GatewayCacheSkipReasonDisabled:
			h.deps.CacheMetrics.RecordLookup("disabled")
		case audit.GatewayCacheSkipReasonNoCache:
			h.deps.CacheMetrics.RecordLookup("skip_no_cache")
		case audit.GatewayCacheSkipReasonPassthrough:
			h.deps.CacheMetrics.RecordLookup("passthrough_skip")
		case audit.GatewayCacheSkipReasonEmbeddingsEndpoint:
			h.deps.CacheMetrics.RecordLookup("skip_embeddings")
		}
		// The semantic cache is skipped — the PROVIDER cache must not be: prepare
		// the upstream body anyway so the normaliser can inject cache markers.
		if ok, _ := st.prepareUpstreamBody(); !ok {
			return false
		}
	default:
		if ok, prepared := st.prepareUpstreamBody(); !ok {
			return false
		} else if !prepared {
			// Phase 4.1 already gated on adapter availability; defensive fallback
			// — skip cache, proceed to live upstream.
			s.gatewayCacheStatus = audit.GatewayCacheSkipped
			s.gatewayCacheSkipReason = audit.GatewayCacheSkipReasonDisabled
			h.deps.CacheMetrics.RecordLookup("disabled")
			break
		}
		primary := s.routeResult.Targets[0]

		// Cache stage is active (L1 and/or L2). Default the status to MISS; a HIT
		// branch below overrides and returns. When L1 is disabled the stage skips
		// straight to L2.
		s.gatewayCacheStatus = audit.GatewayCacheMiss

		if l1Enabled {
			// L0: key normalisation — strip volatile fields (e.g. cch=
			// billing nonce) from the body ONLY for cache key computation.
			// Never mutates cachePreparedBody; fail-open.
			keyBody := s.cachePreparedBody
			if h.deps.Normaliser != nil {
				keyBody = h.deps.Normaliser.NormalizeKey(primary.AdapterType, s.cachePreparedBody)
			}
			// L1 tenant isolation: fold the same vary_by
			// scope the L2 semantic tier uses into the L1 exact-match key.
			// Empty scope (vary_by=none / unset) preserves fleet-wide dedup.
			l1Scope := resolveL1CacheScope(h.deps.SemanticConfigCache, s.rec)
			s.cacheKey = h.deps.Cache.BuildScopedKey(primary.ProviderName, primary.ProviderModelID, keyBody, allowlistVersionFromDeps(h.deps), l1Scope)
			s.rec.CacheKey = s.cacheKey

			if s.isStream {
				if entry := h.deps.Cache.LookupStream(s.r.Context(), s.cacheKey); entry != nil {
					s.rec.GatewayCacheStatus = audit.GatewayCacheHit
					s.rec.GatewayCacheKind = audit.GatewayCacheKindExtract
					s.rec.ProviderCacheStatus = audit.ProviderCacheNA
					h.deps.Cache.RecordHit(s.r.Context())
					h.deps.CacheMetrics.RecordLookup("hit")
					h.handleStreamHit(s.r, s.w, s.rec, primary, s.routeResult, s.reqHookResult, entry, s.quotaInPrice, s.quotaOutPrice, s.quotaDecision, s.endpointType, s.requestID, s.start, s.logger)
					return false
				}
			} else {
				if entry := h.deps.Cache.LookupResponse(s.r.Context(), s.cacheKey); entry != nil {
					s.rec.GatewayCacheStatus = audit.GatewayCacheHit
					s.rec.GatewayCacheKind = audit.GatewayCacheKindExtract
					s.rec.ProviderCacheStatus = audit.ProviderCacheNA
					h.deps.Cache.RecordHit(s.r.Context())
					h.deps.CacheMetrics.RecordLookup("hit")
					h.handleNonStreamHit(s.r, s.w, s.rec, primary, s.routeResult, s.reqHookResult, entry, s.quotaInPrice, s.quotaOutPrice, s.quotaDecision, s.endpointType, s.requestID, s.start, s.logger)
					return false
				}
			}
			h.deps.Cache.RecordMiss(s.r.Context())
			h.deps.CacheMetrics.RecordLookup("miss")
		}

		// L2 semantic cache lookup on L1 miss.
		// tryL2Lookup is a no-op (returns false) when SemanticReader is nil
		// or the per-route policy has semantic.enabled=false, so it is safe
		// to call unconditionally on every L1 miss.
		if h.tryL2Lookup(l2ReadParams{
			r:             s.r,
			w:             s.w,
			rec:           s.rec,
			routeResult:   s.routeResult,
			primary:       primary,
			isStream:      s.isStream,
			resolved:      s.resolved,
			reqHookResult: s.reqHookResult,
			quotaInPrice:  s.quotaInPrice,
			quotaOutPrice: s.quotaOutPrice,
			quotaDecision: s.quotaDecision,
			endpointType:  s.endpointType,
			requestID:     s.requestID,
			start:         s.start,
			logger:        s.logger,
			canonicalMsgs: func() []normcore.Message {
				if np := s.rctxFull.Normalized(); np != nil {
					return np.Messages
				}
				return nil
			}(),
			hasTools: func() bool {
				np := s.rctxFull.Normalized()
				return np != nil && len(np.Tools) > 0
			}(),
		}) {
			return false // L2 HIT — response already written
		}
	}
	// Stamp gateway-side detail fields on the record. Unified
	// rec.CacheStatus is derived at audit-write time from these +
	// ProviderCacheStatus (which the response-usage parser stamps
	// later when the upstream returns).
	s.rec.GatewayCacheStatus = s.gatewayCacheStatus
	s.rec.GatewayCacheSkipReason = s.gatewayCacheSkipReason
	// Header value: "HIT" was already emitted on the direct-HIT branches
	// above (which stop the chain); here the request is going to upstream,
	// so emit the unified MISS.
	s.w.Header().Set("X-Nexus-Cache", string(audit.CacheStatusMiss))
	s.phaseTimer.Mark(traffic.PhaseCacheLookup)
	return true
}

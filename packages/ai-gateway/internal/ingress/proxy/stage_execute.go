// stage_execute.go — the execute stage of the proxy stage chain:
// upstream body normalisation, Gemini cachedContent injection, the
// broker fan-out on cache MISS, and the direct upstream dispatch. Owns
// proxyState.execResult / execTarget / execAttempts (direct path; the
// broker leg writes the response inside this stage).
package proxy

import (
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// executeStage dispatches the request to the live upstream.
type executeStage struct{ s *proxyState }

func (st executeStage) run() bool {
	s := st.s
	h := s.h

	// Phase 6+7+8: live upstream + downstream pipeline.
	//
	// Body normalisation — strip volatile bytes and inject
	// cache_control markers (Anthropic/Bedrock) and Gemini cachedContent
	// references before upstream dispatch. Runs on every MISS regardless
	// of broker wiring so that provider-side caching works even when the
	// response-cache dedup broker is not configured. Every path through the
	// cache stage — cache-eligible AND skipped (no-cache / time-sensitive /
	// disabled) — prepares cachePreparedBody, so provider-side caching is
	// independent of the gateway's own cache participation; empty means the
	// defensive adapter-missing path, and this block no-ops.
	if h.deps.Normaliser != nil && len(s.cachePreparedBody) > 0 {
		normStart := time.Now()
		primary := s.routeResult.Targets[0]
		normBody, normResult := h.deps.Normaliser.NormalizeUpstream(
			primary.AdapterType, primary.ProviderID, s.cachePreparedBody)
		if !normResult.DryRun {
			s.cachePreparedBody = normBody
		}
		s.rec.NormalizerRan = true
		s.rec.NormalizedStripCount = normResult.StripCount
		s.rec.NormalizedStripBytes = normResult.StripBytes
		s.rec.CacheMarkerInjected = normResult.MarkersInjected
		s.phaseTimer.MarkBetween(traffic.PhaseNormUpstream, time.Since(normStart))
	}
	// Gemini cachedContent injection: rewrite the prepared body to
	// reference a cached systemInstruction object. Runs after body
	// normalisation. Fail-open: errors are logged and the original body
	// is forwarded unchanged. Manager is per-provider (resolved against
	// the 3-tier cache_config blob) — ManagerSet.Get returns nil for
	// non-Gemini providers, which short-circuits this block.
	// geminicacheInvalidate is the per-request hook to drop the Redis
	// entry that fed this request's cachedContent injection. Set on
	// HIT below, called from the response path when the upstream
	// reports the cache has been evicted (403 / "CachedContent not
	// found"). Nil on miss so the call site can `if … != nil` cheaply.
	var geminicacheInvalidate func()
	if h.deps.GeminiCacheMgrSet != nil && len(s.cachePreparedBody) > 0 {
		primary := s.routeResult.Targets[0]
		if provcore.Format(primary.AdapterType) == provcore.FormatGemini {
			if mgr := h.deps.GeminiCacheMgrSet.Get(primary.ProviderID); mgr != nil {
				injected, injectResult, injectErr := mgr.Inject(
					s.r.Context(), primary.ProviderID, primary.ProviderModelID, s.cachePreparedBody)
				if injectErr != nil {
					s.logger.Warn("geminicache inject error, pass-through", "error", injectErr)
				} else {
					s.cachePreparedBody = injected
					geminicacheInvalidate = injectResult.Invalidate
				}
			}
		}
	}
	// Stash the invalidate hook on the context so handleNonStream /
	// stream paths can fire it without threading another parameter.
	if geminicacheInvalidate != nil {
		s.r = s.r.WithContext(withGeminiCacheInvalidate(s.r.Context(), geminicacheInvalidate))
	}

	// When cacheStatus == MISS and BrokerRegistry is wired, fan the
	// upstream out through the broker so concurrent requests with the
	// same key share one call. Joiners stamp HIT_LIVE.
	// On any other status (DISABLED / SKIP_NO_CACHE) we go direct.
	// Defense-in-depth: never fan out on an empty cache key. An empty key would
	// make the broker collapse DISTINCT concurrent requests onto one leader's
	// upstream answer (joiners would receive another request's bytes). The key
	// is built whenever the cache core is wired (stage_cache); if it is still
	// empty here (cache core not wired) go direct with no dedup.
	if s.gatewayCacheStatus == audit.GatewayCacheMiss && s.cacheKey != "" && h.deps.BrokerRegistry != nil {
		// canonicalMsgs feeds the broker-path L2 write-back —
		// without this thread-through the broker leg silently
		// skipped scheduleL2Write and L2 stayed empty.
		var brokerCanonMsgs []normcore.Message
		if np := s.rctxFull.Normalized(); np != nil {
			brokerCanonMsgs = np.Messages
		}
		h.runViaBroker(s.r, s.w, s.rec, s.routeResult, s.body, s.isStream, s.resolved, s.reqHookResult, s.cacheKey, s.cachePreparedBody, s.cachePreparedRewrites, s.cachePreparedURLOverride, s.quotaInPrice, s.quotaOutPrice, s.quotaDecision, s.endpointType, s.requestID, s.start, s.logger, brokerCanonMsgs)
		return false
	}

	// Direct path (cache disabled, no broker, or SKIP_NO_CACHE).
	// Pass the prepared+normalised body when available so the executor
	// skips its internal PrepareBody call (idempotent, saves a µs-scale
	// encode; nil body falls back to plain Execute behaviour).
	result, target, attempts, err := h.fetchUpstreamWithPreparedBody(s.r, s.w, s.rec, s.routeResult, s.body, s.isStream, s.resolved, s.cachePreparedBody, s.cachePreparedRewrites, s.cachePreparedURLOverride, s.start, s.logger)
	if err != nil {
		return false // error response already written
	}
	s.execResult = result
	s.execTarget = target
	s.execAttempts = attempts

	return true
}

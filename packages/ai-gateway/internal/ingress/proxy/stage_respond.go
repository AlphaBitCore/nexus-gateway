// stage_respond.go — the respond stage of the proxy stage chain (direct
// path only): forwarded upstream headers, the Nexus response headers,
// the L2 semantic write-back schedule, and the hand-off into the
// stream / non-stream response pipelines.
package proxy

import (
	"strings"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// respondStage writes the direct-path response to the client. It is the
// chain's terminal stage: run always reports the request as handled.
type respondStage struct{ s *proxyState }

func (st respondStage) run() bool {
	s := st.s
	h := s.h
	result := s.execResult
	target := s.execTarget

	// Forward allowlisted upstream response headers BEFORE the Nexus
	// stamps so any conflict (e.g. an upstream emitting `via` or
	// `server`) is overwritten by Nexus on the same key — see
	// docs/developers/specs/e36/e36-s2-forward-header-yaml-response.md "Nexus wins"
	// invariant. isCacheHit=false on this direct (live) path.
	writeForwardedResponseHeaders(s.w, h.deps.Allowlist, provcore.Format(target.AdapterType), result.Headers, false)

	if s.isStream {
		h.setResponseHeadersStream(s.w, s.rec, target, s.routeResult, s.execAttempts)
		s.w.Header().Set("X-Nexus-Hook", traffic.FormatHookOutcome(aigwHookOutcomeFromResult(s.reqHookResult)))
		if len(result.Coerced) > 0 {
			s.w.Header().Set("X-Nexus-Coerced", strings.Join(result.Coerced, ","))
		}
		// Wrap result.Stream into a ChunkSubscription so the
		// downstream pipeline shares one shape with the broker
		// path. There is no cache write on the direct path
		// (cache is disabled or off for this request).
		sub := newDirectStreamSubscription(result.Stream)
		h.handleStreamWithSubscription(s.r, s.w, s.rec, sub, target, result.Coerced, s.quotaInPrice, s.quotaOutPrice, s.quotaDecision, s.endpointType, s.requestID, s.start, s.logger)
	} else {
		// Stamp the PhaseSink values onto rec NOW so
		// setResponseHeaders can emit x-nexus-aigw-upstream-*
		// headers. The finalize defer redundantly does the same
		// at request end — idempotent.
		s.rec.UpstreamTtfbMs = s.phaseSink.TtfbMs()
		s.rec.UpstreamTotalMs = s.phaseSink.TotalMs()
		h.setResponseHeaders(s.w, s.rec, target, s.routeResult, s.start, s.execAttempts)
		s.w.Header().Set("X-Nexus-Hook", traffic.FormatHookOutcome(aigwHookOutcomeFromResult(s.reqHookResult)))
		if len(result.Coerced) > 0 {
			s.w.Header().Set("X-Nexus-Coerced", strings.Join(result.Coerced, ","))
		}
		// Fire L2 semantic write-back in a background goroutine. This is
		// the non-streaming arm of the DIRECT path. The streaming L2
		// write-back is fired from the broker leg (runViaBroker →
		// scheduleL2Write with isStream=true); the direct stream branch
		// above runs only when the broker is unwired or the cache is off,
		// in which case there is no L2 write to perform.
		if s.gatewayCacheStatus == audit.GatewayCacheMiss {
			var l2CanonMsgs []normcore.Message
			if np := s.rctxFull.Normalized(); np != nil {
				l2CanonMsgs = np.Messages
			}
			h.scheduleL2Write(
				s.rec,
				s.routeResult.Targets[0],
				l2CanonMsgs,
				result.Body,
				provcoreUsageToMap(&result.Usage),
				false,
				s.in,
				s.logger,
			)
		}
		h.handleNonStream(s.r, s.w, s.rec, result, target, s.quotaInPrice, s.quotaOutPrice, s.quotaDecision, s.endpointType, s.requestID, s.start, s.logger)
	}
	return false
}

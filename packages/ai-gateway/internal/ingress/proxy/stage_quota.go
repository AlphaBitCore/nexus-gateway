// stage_quota.go — the quota stage of the proxy stage chain:
// hierarchical quota enforcement against the routed targets (which may
// downgrade the primary target) plus the request-side embedding
// metadata pre-stamp. Owns proxyState.quotaInPrice / quotaOutPrice /
// quotaDecision.
package proxy

import (
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

// quotaStage checks the quota counter for the resolved route.
type quotaStage struct{ s *proxyState }

func (st quotaStage) run() bool {
	s := st.s
	h := s.h

	// Phase 4.5: Quota check.
	quotaInPrice, quotaOutPrice, quotaDecision := h.checkQuota(s.r, s.w, s.rec, s.vkMeta, s.routeResult, s.body, s.modelID)
	if s.rec.StatusCode != 0 {
		return false // quota rejected, response already written
	}
	s.quotaInPrice = quotaInPrice
	s.quotaOutPrice = quotaOutPrice
	s.quotaDecision = quotaDecision
	s.phaseTimer.Mark(traffic.PhaseQuota)

	// Pre-stamp the request-side embedding metadata so all downstream
	// paths (live, stream HIT, non-stream HIT, broker stream HIT_LIVE,
	// broker non-stream HIT_LIVE) inherit it without needing the
	// original request body. The response-side dimension field is
	// updated in each path when the response arrives (live:
	// handleNonStream; HIT paths: their response
	// replay code). crossFormatRouting detects ingress ≠ target.
	if s.endpointType == "embeddings" && len(s.routeResult.Targets) > 0 {
		crossFormatRouting := provcore.Format(s.routeResult.Targets[0].AdapterType) != s.resolved.BodyFormat
		s.rec.Metadata = preStampEmbeddingRequestMeta(s.rec.Metadata, s.body, crossFormatRouting)
	}
	return true
}

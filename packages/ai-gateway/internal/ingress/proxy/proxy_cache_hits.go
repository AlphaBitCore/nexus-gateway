// Package handler — proxy_cache_hits.go hosts the cache-HIT
// short-circuits: handleStreamHit replays a cached SSE timeline and
// handleNonStreamHit re-encodes a cached canonical response. Both feed
// the same downstream pipeline used by the MISS / broker paths so the
// transcoder + response-stage hook + writer chain runs identically on
// every outcome.
package proxy

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	cache "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/stream"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/estimator"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/metrics"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/quota"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	hookcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"github.com/tidwall/gjson"
)

// handleStreamHit serves a streaming cache HIT by replaying the cached
// chunk timeline through the same downstream pipeline used for MISS.
// Hooks always run on every replay (D2).
func (h *Handler) handleStreamHit(
	r *http.Request,
	w http.ResponseWriter,
	rec *audit.Record,
	target routingcore.RoutingTarget,
	routeResult *routingcore.RouteResult,
	reqHookResult *hookcore.CompliancePipelineResult,
	entry *cache.StreamEntry,
	quotaInPrice, quotaOutPrice float64,
	quotaDecision *quota.Decision,
	endpointType, requestID string,
	start time.Time,
	logger *slog.Logger,
) {
	rec.RoutedProviderID = target.ProviderID
	rec.RoutedProviderName = target.ProviderName
	rec.RoutedModelID = target.ModelID
	rec.RoutedModelName = target.ModelCode
	rec.TargetHost = upstreamHost(target)
	rec.PromptTokens = int64(usageInt(entry.Usage.PromptTokens))
	rec.CompletionTokens = int64(usageInt(entry.Usage.CompletionTokens))
	rec.TotalTokens = int64(usageInt(entry.Usage.TotalTokens))
	// reasoning_tokens: cache HIT serves the response from a prior
	// provider call, so we surface the same token counts (including
	// reasoning) that the original upstream returned.
	if entry.Usage.ReasoningTokens != nil {
		rec.ReasoningTokens = int64(*entry.Usage.ReasoningTokens)
	}
	// Embeddings never stream, so a stream cache HIT is never an embeddings
	// request — no embeddingTokenFallback here (it lives on the non-stream
	// HIT, live, and broker paths). Keeping it here would be dead code.
	// EstimatedCostUsd is "what this request would cost at the configured
	// Model prices" — invariant of cache outcome. The customer's actual
	// paid-upstream amount = EstimatedCostUsd − GatewayCacheSavingsUsd.
	// On a full HIT the two are equal and net is zero, but each field
	// carries information separately so dashboards can show "spend if no
	// cache" vs "savings" without re-deriving from raw token math.
	{
		units := estimator.BillableUnits{
			PromptTokens:     int(rec.PromptTokens),
			CompletionTokens: int(rec.CompletionTokens),
		}
		wouldHaveCost := estimator.Lookup(rec.EndpointType)(units, metrics.ModelPrices{
			InputUsdPerM:  &quotaInPrice,
			OutputUsdPerM: &quotaOutPrice,
		}).Total
		rec.EstimatedCostUsd = wouldHaveCost
		rec.GatewayCacheSavingsUsd = wouldHaveCost
	}
	rec.UsageExtractionStatus = "ok"
	// Stream HIT for embeddings — dimension is not extractable from SSE
	// chunks. Request-side metadata was pre-stamped in ServeProxy; no
	// response-dimension update here (embeddings rarely/never stream).

	// Forward allowlisted upstream response headers from the cached
	// entry BEFORE the Nexus stamps. isCacheHit=true strips PerRequest
	// headers (request-id, ratelimit-remaining, processing-ms) so the
	// client never sees a stale per-request value attributed to a
	// request that did not actually fire.
	writeForwardedResponseHeaders(w, h.deps.Allowlist, provcore.Format(target.AdapterType), entry.UpstreamHeaders, true)

	h.setResponseHeadersStream(w, rec, target, routeResult, 1)
	w.Header().Set("X-Nexus-Cache", string(audit.CacheStatusHit))
	if reqHookResult != nil {
		w.Header().Set("X-Nexus-Hook", traffic.FormatHookOutcome(aigwHookOutcomeFromResult(reqHookResult)))
	}

	sub := streamcache.NewReplaySubscription(entry, h.deps.CacheMetrics)

	// B2 cross-ingress reshape — if the entry was tagged with the
	// writer's origin shape and the current ingress differs, stamp the
	// origin on the context so handleStreamWithSubscription picks a
	// transcoder that re-encodes from the entry's wire shape into the
	// ingress's wire shape (instead of forwarding the cached RawBytes
	// verbatim). Legacy untagged entries skip this branch and fall
	// through to the standard (ingress, target) transcoder selection.
	if reqIngress, ok := IngressFromContext(r.Context()); ok && entry.OriginWireShape != "" {
		if entry.OriginWireShape != reqIngress.WireShape {
			ctx := WithStreamHitOrigin(r.Context(), StreamHitOrigin{
				WireShape: entry.OriginWireShape,
			})
			r = r.WithContext(ctx)
		}
	}
	h.handleStreamWithSubscription(r, w, rec, sub, target, nil, quotaInPrice, quotaOutPrice, quotaDecision, endpointType, requestID, start, logger)
}

// handleNonStreamHit serves a non-streaming cache HIT. Re-encodes the
// cached canonical response back into the ingress wire shape, runs
// response-stage hooks (D2), and writes JSON to the client.
func (h *Handler) handleNonStreamHit(
	r *http.Request,
	w http.ResponseWriter,
	rec *audit.Record,
	target routingcore.RoutingTarget,
	routeResult *routingcore.RouteResult,
	reqHookResult *hookcore.CompliancePipelineResult,
	entry *cache.ResponseEntry,
	quotaInPrice, quotaOutPrice float64,
	quotaDecision *quota.Decision,
	endpointType, requestID string,
	start time.Time,
	logger *slog.Logger,
) {
	ctx := r.Context()
	rec.RoutedProviderID = target.ProviderID
	rec.RoutedProviderName = target.ProviderName
	rec.RoutedModelID = target.ModelID
	rec.RoutedModelName = target.ModelCode
	rec.TargetHost = upstreamHost(target)
	rec.PromptTokens = int64(usageInt(entry.Usage.PromptTokens))
	rec.CompletionTokens = int64(usageInt(entry.Usage.CompletionTokens))
	rec.TotalTokens = int64(usageInt(entry.Usage.TotalTokens))
	// Embeddings cost/usage fallback (cache HIT): a cached entry from a
	// provider that reports no usage (e.g. Gemini embedContent) carries
	// prompt_tokens=0. Back-fill from the current request's local estimate so
	// the would-have-cost / savings reflect the real input size.
	if pt := embeddingTokenFallback(rec.EndpointType, rec.PromptTokens, rec.Metadata); pt != rec.PromptTokens {
		rec.PromptTokens = pt
		rec.TotalTokens = pt
	}
	// reasoning_tokens: cache HIT serves the response from a prior
	// provider call, so we surface the same token counts (including
	// reasoning) that the original upstream returned.
	if entry.Usage.ReasoningTokens != nil {
		rec.ReasoningTokens = int64(*entry.Usage.ReasoningTokens)
	}
	// EstimatedCostUsd is the would-have-paid upstream cost (tokens ×
	// current Model prices), not zero. HIT doesn't change it; the savings
	// is the separate GatewayCacheSavingsUsd field. Actual spend =
	// EstimatedCostUsd − GatewayCacheSavingsUsd.
	{
		units := estimator.BillableUnits{
			PromptTokens:     int(rec.PromptTokens),
			CompletionTokens: int(rec.CompletionTokens),
		}
		wouldHaveCost := estimator.Lookup(rec.EndpointType)(units, metrics.ModelPrices{
			InputUsdPerM:  &quotaInPrice,
			OutputUsdPerM: &quotaOutPrice,
		}).Total
		rec.EstimatedCostUsd = wouldHaveCost
		rec.GatewayCacheSavingsUsd = wouldHaveCost
	}
	rec.UsageExtractionStatus = "ok"

	respBody := []byte(entry.CanonicalResponse)
	// Update embedding dimension from cached canonical response.
	if rec.EndpointType == "embeddings" {
		rec.Metadata = updateEmbeddingDimension(rec.Metadata, respBody)
	}
	ingress, _ := IngressFromContext(ctx)
	// Egress reshape — cache HIT non-stream ("canonical→A" on replay). The
	// stored body's shape depends on the ORIGIN endpoint kind, tagged at write
	// time by OriginWireShape:
	//   - Chat-kind origins (openai-chat, anthropic /v1/messages, gemini
	//     /v1beta, …) all store CANONICAL chat — their codecs DecodeResponse the
	//     upstream to canonical OpenAI before caching. Re-encode canonical → the
	//     CURRENT reader's ingress shape via ResponseCanonicalToIngress:
	//     identity for OpenAI-family, content[]/candidates[] for anthropic/
	//     gemini, output[] for a /v1/responses reader (E57 cross-shape). The
	//     empty/legacy tag is also canonical → handled here.
	//   - openai-responses origin stores RESPONSES-shape (native passthrough is
	//     not canonicalised). Serve verbatim to a /v1/responses reader; for a
	//     different reader, ResponseAcrossFormats decodes responses→canonical→
	//     reader.
	// This replaced the prior verbatim-on-same-shape gate, which returned
	// canonical chat (`choices[]`) to a same-ingress anthropic/gemini reader
	// instead of `content[]`/`candidates[]`.
	if h.deps.CanonicalBridge != nil {
		switch {
		case gjson.GetBytes(respBody, "choices").Exists():
			// Canonical OpenAI chat envelope (`choices[]`). Every chat-kind origin
			// — openai-chat, anthropic /v1/messages, gemini /v1beta — stores
			// canonical chat (their codecs DecodeResponse the upstream to
			// canonical), and cross-format /v1/responses canonicalises before
			// caching too. Reshape canonical → the CURRENT reader's ingress:
			// identity for OpenAI-family, content[]/candidates[] for anthropic/
			// gemini, output[] for a /v1/responses reader (E57 cross-shape).
			shaped, err := h.deps.CanonicalBridge.ResponseCanonicalToIngress(ingress.BodyFormat, respBody)
			if err != nil {
				logger.Warn("cache HIT: ingress reshape failed; serving canonical bytes", "error", err)
			} else {
				respBody = shaped
			}
		case entry.OriginWireShape != "" && ingress.WireShape != entry.OriginWireShape:
			// Body is in the origin's own wire shape, not canonical chat (today
			// only /v1/responses NATIVE passthrough = responses-shape `output[]`).
			// Decode origin→canonical→reader. Sniffing `choices` rather than
			// trusting OriginWireShape alone is necessary because native vs
			// cross-format /v1/responses share the same tag but store different
			// shapes (responses-shape vs canonical chat).
			shaped, err := h.deps.CanonicalBridge.ResponseAcrossFormats(entry.OriginWireShape, ingress.WireShape, respBody)
			if err != nil {
				logger.Warn("cache HIT: cross-shape reshape failed; serving entry bytes",
					"error", err, "from", string(entry.OriginWireShape), "to", string(ingress.WireShape))
			} else {
				respBody = shaped
			}
		}
		// else: same-shape non-canonical body (responses-native → responses
		// reader) → serve verbatim.
	}

	usage := metrics.Usage{
		PromptTokens:     rec.PromptTokens,
		CompletionTokens: rec.CompletionTokens,
		TotalTokens:      rec.TotalTokens,
	}

	// Response-stage hooks: identical to handleNonStream's response
	// hook block. On Reject we write the rejection and return; on
	// Modify we swap respBody.
	{
		extractor := h.trafficAdapterFor(ingress.BodyFormat)
		ingressFormat := string(ingress.BodyFormat)
		respContent, respModel, respFinish := h.extractResponseForHooks(ctx, extractor, ingressFormat, respBody, r.URL.Path, logger)
		cacheHitEpType := typology.KindFromWireShape(ingress.WireShape)
		respInput := &hookcore.HookInput{
			RequestID:      requestID,
			Stage:          "response",
			Normalized:     respContent,
			IngressType:    "AI_GATEWAY",
			Path:           r.URL.Path,
			Model:          respModel,
			FinishReason:   respFinish,
			TokenCount:     int(usage.TotalTokens),
			SourceIP:       middleware.ClientIP(r),
			ProviderRegion: target.Region,
			EndpointType:   cacheHitEpType,
			OutputModality: []hookcore.Modality{hookcore.ModalityText},
		}
		pipeline, pErr := h.deps.HookConfigCache.Resolver(ctx).BuildPipeline(
			"response", "AI_GATEWAY",
			cacheHitEpType,
			respInput.OutputModality,
			5*time.Second, 15*time.Second, false, logger,
		)
		if pErr != nil {
			logger.Error("failed to build response hook pipeline (cache HIT)", "error", pErr)
			h.writeError(w, rec, http.StatusInternalServerError, "hook pipeline error")
			return
		}
		if pipeline != nil {
			pipeline.SetAllowModify(true)
			pipeline.SetClearSoftOnApprove(true)
			hookResult := pipeline.Execute(ctx, respInput)
			rec.ResponseHookDecision = string(hookResult.Decision)
			rec.ResponseHookReason = hookResult.Reason
			rec.ResponseHookReasonCode = hookResult.ReasonCode
			rec.ComplianceTags = mergeTagSets(rec.ComplianceTags, hookResult.Tags)
			rec.HooksPipeline = appendHookTrace(rec.HooksPipeline, "response", hookResult.HookResults)
			if br := mapBlockingRule(hookResult.BlockingRule); br != nil {
				rec.BlockingRule = br
			}
			if h.deps.Metrics != nil {
				h.deps.Metrics.RecordHookRequest(ingressFormat, "response", string(hookResult.Decision))
			}
			switch hookResult.Decision {
			case hookcore.RejectHard:
				h.writeError(w, rec, http.StatusForbidden, hookResult.Reason)
				return
			case hookcore.BlockSoft:
				h.writeError(w, rec, 246, hookResult.Reason)
				return
			case hookcore.Modify:
				if len(hookResult.ModifiedContent) > 0 {
					rewritten, n, rErr := extractor.RewriteResponseBody(ctx, respBody, r.URL.Path, contentBlocksToNormalized(hookResult.ModifiedContent))
					switch {
					case errors.Is(rErr, traffic.ErrRewriteUnsupported):
						logger.Warn("cache HIT: hook Modify but adapter does not support rewrite; returning original body",
							slog.String("adapter", extractor.ID()),
							slog.String("path", r.URL.Path),
						)
					case rErr != nil:
						logger.Error("cache HIT: hook response rewrite failed",
							slog.String("adapter", extractor.ID()),
							slog.String("path", r.URL.Path),
							slog.String("error", rErr.Error()),
						)
						h.writeError(w, rec, http.StatusInternalServerError, "response rewrite failed")
						return
					default:
						respBody = rewritten
						rec.ResponseHookRewriteCount = n
						rec.ResponseHookRewritten = true
					}
				}
			}
		}
	}

	pcCfg := h.payloadCaptureConfig()
	if pcCfg.StoreResponseBody && len(respBody) > 0 {
		rec.ResponseBody = respBody
		rec.ResponseContentType = "application/json"
	}
	rec.StatusCode = http.StatusOK

	// Forward allowlisted upstream response headers (cache HIT path);
	// see handleStreamHit for rationale. isCacheHit=true.
	writeForwardedResponseHeaders(w, h.deps.Allowlist, provcore.Format(target.AdapterType), entry.UpstreamHeaders, true)

	h.setResponseHeaders(w, rec, target, routeResult, start, 1)
	w.Header().Set("X-Nexus-Cache", string(audit.CacheStatusHit))
	if reqHookResult != nil {
		w.Header().Set("X-Nexus-Hook", traffic.FormatHookOutcome(aigwHookOutcomeFromResult(reqHookResult)))
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(respBody)

	if h.deps.Metrics != nil {
		h.deps.Metrics.RecordRequest(target.ProviderName, target.ModelID, endpointType, rec.StatusCode, time.Since(start), usage)
	}
	// Cache HIT served from L1 — no upstream call, $0 actual cost. Do NOT
	// reconcile quota: the user wasn't billed, so the quota ledger must
	// not move. The would-have-been cost is recorded as
	// gateway_cache_savings_usd for analytics.
}

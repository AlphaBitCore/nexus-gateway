// stage_hooks.go — the request-hooks stage of the proxy stage chain:
// the request-stage compliance pipeline (reject / soft-block / modify /
// storage policy) and its emergency-passthrough bypass. Owns
// proxyState.reqHookResult and may replace proxyState.body with the
// hook-rewritten bytes.
package proxy

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/middleware"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	hookcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/redact"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// requestHooksStage runs the request-stage compliance pipeline.
type requestHooksStage struct{ s *proxyState }

func (st requestHooksStage) run() bool {
	s := st.s
	h := s.h

	// Phase 5: Request hooks.
	// Pass the (post-quota) primary target so hook inputs carry
	// ProviderRegion for data-residency evaluation. Quota downgrade
	// ran in the quota stage, so routeResult.Targets[0] already
	// reflects the real upstream that will be dispatched.
	var requestHookTarget routingcore.RoutingTarget
	if len(s.routeResult.Targets) > 0 {
		requestHookTarget = s.routeResult.Targets[0]
	}
	// bypassHooks: skip the request-stage hooks pipeline entirely
	// when emergency passthrough is active for the routed provider.
	// rec.HookDecision is stamped "BYPASSED" so audit consumers can
	// SQL-filter for requests that ran without hook evaluation.
	// On the bypass path s.reqHookResult stays nil, so downstream code
	// (cache key build, audit population) sees the zero value without
	// further branching.
	if pt := s.resolvedReq.Passthrough(); pt.AnyBypassActive() && pt.BypassHooks {
		s.rec.HookDecision = "BYPASSED"
	} else {
		rewrittenBody, reqHookResult, rejected := h.runRequestHooks(s.r, s.w, s.rec, s.requestID, s.body, requestHookTarget, s.resolved, s.logger)
		if rejected {
			return false
		}
		s.reqHookResult = reqHookResult
		if rewrittenBody != nil {
			s.body = rewrittenBody
		}
	}
	return true
}

// runRequestHooks executes request-stage hooks. Returns:
//   - rewrittenBody: non-nil when a hook produced a Modify decision and
//     the traffic adapter successfully rewrote the request body with the
//     redacted content. The caller should forward these bytes upstream
//     instead of the original body. Nil when no rewrite was performed.
//   - pipelineResult: the CompliancePipelineResult from the pipeline, or nil
//     when no pipeline was built (no hooks configured). The caller uses this
//     to emit X-Nexus-Hook on the response. On the reject path the
//     header is written inside this function before the error response.
//   - rejected: true when the pipeline rejected the request and an
//     error response has already been written to w.
func (h *Handler) runRequestHooks(r *http.Request, w http.ResponseWriter, rec *audit.Record, requestID string, body []byte, target routingcore.RoutingTarget, in Ingress, logger *slog.Logger) (rewrittenBody []byte, pipelineResult *hookcore.CompliancePipelineResult, rejected bool) {
	// Pick the traffic adapter matching the detected ingress body
	// format so content extraction + rewrite run through the right
	// schema parser. For OpenAI-compat ingress this is the classic
	// `openai-compat`; for Anthropic ingress it is `anthropic`; etc.
	// Per SDD E28-s5 §4: hook rewrite runs on the ingress-format
	// bytes, so the adapter here MUST match the ingress format, not
	// the upstream provider format.
	trafficAdapter := h.trafficAdapterFor(in.BodyFormat)
	ingressFormat := string(in.BodyFormat)

	input := &hookcore.HookInput{
		RequestID:      requestID,
		Stage:          "request",
		Normalized:     h.extractRequestContentForHooks(r.Context(), trafficAdapter, ingressFormat, body, r.URL.Path, logger),
		IngressType:    "AI_GATEWAY",
		Method:         r.Method,
		Path:           r.URL.Path,
		ContentType:    r.Header.Get("Content-Type"),
		BodySize:       int64(len(body)),
		SourceIP:       middleware.ClientIP(r),
		ProviderRegion: target.Region,
		// Hook configs (`targetModels: [...]`) are authored by admins
		// using customer-facing codes ("gpt-4o"), not internal UUIDs.
		Model: target.ModelCode,
	}

	// Populate endpoint/modality context on the hook input so BuildPipeline
	// can gate Class-A text hooks out of non-text endpoints. At request
	// stage the endpoint type is known from the Ingress descriptor; default
	// to text modality (all current AI-gateway traffic is text-in).
	input.EndpointType = typology.KindFromWireShape(in.WireShape)
	input.InputModality = []hookcore.Modality{hookcore.ModalityText}

	resolver := h.deps.HookConfigCache.Resolver(r.Context())
	pipeline, err := resolver.BuildPipeline(
		"request", "AI_GATEWAY",
		input.EndpointType,
		input.InputModality,
		5*time.Second, 15*time.Second, false, true /* strictFailClosed: reverse proxy refuses fail-closed-unbuildable */, logger,
	)
	if err != nil {
		logger.Error("failed to build request hook pipeline", "error", err)
		h.writeError(w, rec, http.StatusInternalServerError, "hook pipeline error")
		return nil, nil, true
	}
	if pipeline == nil {
		return nil, nil, false
	}
	pipeline.SetAllowModify(true)
	pipeline.SetClearSoftOnApprove(true)

	hookResult := pipeline.Execute(r.Context(), input)

	rec.HookDecision = string(hookResult.Decision)
	rec.HookReason = hookResult.Reason
	rec.HookReasonCode = hookResult.ReasonCode
	rec.ComplianceTags = mergeTagSets(rec.ComplianceTags, hookResult.Tags)
	rec.BlockingRule = mapBlockingRule(hookResult.BlockingRule)
	rec.HooksPipeline = appendHookTrace(rec.HooksPipeline, "request", hookResult.HookResults)
	// Propagate TransformSpans + storage policy from the pipeline result
	// onto the audit Record. The audit writer applies storage policy to
	// the persisted NormalizedPayload at recordToMessage.
	rec.RequestTransformSpans = hookResult.TransformSpans
	rec.RequestStorageAction = string(hookResult.StorageAction)
	rec.RequestRedactRuleIDs = redact.CollectRuleIDs(hookResult.TransformSpans)
	rec.RequestRedetect = hookResult.Redetect
	// Stamp the storage-policy ReasonCode when the operator chose
	// "audit-only redact" or "drop content" — i.e. the storage path
	// diverged from the inflight path. Pure inflight-rewrite or pure
	// reject paths leave the hook's own reason code in place.
	if rec.HookReasonCode == "" {
		switch hookResult.StorageAction {
		case hookcore.StorageDropContent:
			rec.HookReasonCode = hookcore.ReasonStorageDroppedByPolicy
		case hookcore.StorageRedact:
			if hookResult.Decision == hookcore.Approve && len(hookResult.TransformSpans) > 0 {
				rec.HookReasonCode = hookcore.ReasonRedactStorageOnlyByPolicy
			}
		}
	}

	if h.deps.Metrics != nil {
		h.deps.Metrics.RecordHookRequest(ingressFormat, "request", string(hookResult.Decision))
	}

	if hookResult.Decision == hookcore.RejectHard {
		// Write X-Nexus-Hook and via before writeError commits the status
		// line, so the client sees the marker even on hook-rejected 4xx responses.
		// X-Nexus-Mode is reserved as an empty position so an outer hop's
		// PrependChain keeps 1:1 alignment with X-Nexus-Via (AI Gateway has
		// no mode concept of its own).
		traffic.PrependVia(w.Header(), "ai-gateway")
		w.Header().Set("X-Nexus-Hook", traffic.FormatHookOutcome(aigwHookOutcomeFromResult(hookResult)))
		w.Header().Set("X-Nexus-Mode", "")
		traffic.SetExposeHeaders(w.Header())
		h.writeError(w, rec, http.StatusForbidden, hookResult.Reason)
		return nil, hookResult, true
	}
	// HTTP 246 is a Nexus-specific status code for "soft reject" — the request
	// was flagged by compliance hooks but not hard-blocked. The response body
	// contains the hook's reason. Clients should treat 246 as a 200-class
	// success with a compliance warning. This convention is shared across
	// ai-gateway and compliance-proxy.
	if hookResult.Decision == hookcore.BlockSoft {
		traffic.PrependVia(w.Header(), "ai-gateway")
		w.Header().Set("X-Nexus-Hook", traffic.FormatHookOutcome(aigwHookOutcomeFromResult(hookResult)))
		w.Header().Set("X-Nexus-Mode", "")
		traffic.SetExposeHeaders(w.Header())
		h.writeError(w, rec, 246, hookResult.Reason)
		return nil, hookResult, true
	}

	// MODIFY: push hook-rewritten content back onto the upstream wire.
	// When the adapter cannot reverse-encode (ErrRewriteUnsupported) we
	// forward the original body plus a warn log rather than failing —
	// that matches how the rest of the hook pipeline treats "Modify was
	// requested but not actionable here". Any other error (malformed,
	// unknown schema after Extract succeeded) indicates an internal
	// inconsistency and surfaces as 500.
	if hookResult.Decision == hookcore.Modify && len(hookResult.ModifiedContent) > 0 {
		rewritten, n, rErr := trafficAdapter.RewriteRequestBody(r.Context(), body, r.URL.Path, contentBlocksToNormalized(hookResult.ModifiedContent))
		switch {
		case errors.Is(rErr, traffic.ErrRewriteUnsupported):
			logger.Warn("hook produced Modify but adapter does not support rewrite; forwarding original body",
				slog.String("adapter", trafficAdapter.ID()),
				slog.String("path", r.URL.Path),
			)
			// Record the degraded path on the audit row.
			rec.HookReasonCode = hookcore.ReasonRedactInflightUnsupported
		case rErr != nil:
			logger.Error("hook request rewrite failed",
				slog.String("adapter", trafficAdapter.ID()),
				slog.String("path", r.URL.Path),
				slog.String("error", rErr.Error()),
			)
			h.writeError(w, rec, http.StatusInternalServerError, "request rewrite failed")
			return nil, hookResult, true
		default:
			rec.HookRewriteCount = n
			rec.HookRewritten = true
			// The redacted wire copy is what the raw storage policy
			// persists under storageAction=redact (rec.RequestBody holds
			// the pre-hook bytes for normalization and must never reach
			// raw storage when redaction is demanded — without this stamp
			// the writer fail-safes the raw copy to NULL).
			rec.RequestBodyRedacted = rewritten
			return rewritten, hookResult, false
		}
	}
	return nil, hookResult, false
}

// stage_hooks.go — the request-hooks stage of the proxy stage chain:
// the request-stage compliance pipeline (block / redact / modify /
// storage policy) and its emergency-passthrough bypass. Owns
// proxyState.reqHookResult and may replace proxyState.body with the
// hook-rewritten bytes.
package proxy

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/middleware"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	hookcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
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
		// No hook evaluated this request, so there is no redaction demand:
		// the captured body is persisted as-is (approve). Without this the
		// zero-value action would drop the raw body in StorageRawBody.
		s.rec.RequestAction = hookcore.ActionApprove
	} else {
		rewrittenBody, reqHookResult, rejected := h.runRequestHooks(s.r, s.w, s.rec, s.requestID, s.body, requestHookTarget, s.resolved, s.phaseTimer, s.logger)
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
//
// pt may be nil (e.g. unit tests that do not wire a PhaseTimer); each
// sub-phase recording is nil-guarded. Sub-phase durations are recorded via
// MarkBetween (explicit duration) so they never disturb the main stage-mark
// cursor, and they surface in traffic_event.latency_breakdown independently of
// the per-hook RequestHooksMs aggregate.
func (h *Handler) runRequestHooks(r *http.Request, w http.ResponseWriter, rec *audit.Record, requestID string, body []byte, target routingcore.RoutingTarget, in Ingress, pt *traffic.PhaseTimer, logger *slog.Logger) (rewrittenBody []byte, pipelineResult *hookcore.CompliancePipelineResult, rejected bool) {
	// Pick the traffic adapter matching the detected ingress body
	// format so content extraction + rewrite run through the right
	// schema parser. For OpenAI-compat ingress this is the classic
	// `openai-compat`; for Anthropic ingress it is `anthropic`; etc.
	// Hook rewrite runs on the ingress-format bytes, so the adapter
	// here MUST match the ingress format, not the upstream provider
	// format.
	trafficAdapter := h.trafficAdapterFor(in.BodyFormat)
	ingressFormat := string(in.BodyFormat)

	// [perf A6] Build the pipeline FIRST. Its gating inputs (endpoint kind +
	// modality) come from the Ingress descriptor, NOT the request body, so we can
	// decide whether any hook will run before doing the expensive (gjson)
	// traffic-adapter content extraction. When no hooks are configured
	// (pipeline == nil) we return early and skip extraction entirely — the
	// extracted content only ever feeds pipeline.Execute and is never persisted,
	// so skipping it on the hooks-OFF path is behaviour-preserving.
	endpointType := typology.KindFromWireShape(in.WireShape)
	inputModality := []hookcore.Modality{hookcore.ModalityText}

	resolver := h.deps.HookConfigCache.Resolver(r.Context())
	buildStart := time.Now()
	pipeline, err := resolver.BuildPipeline(
		"request", "AI_GATEWAY",
		endpointType,
		inputModality,
		5*time.Second, 15*time.Second, perfParallelHooks(), true /* strictFailClosed: reverse proxy refuses fail-closed-unbuildable */, logger,
	)
	if pt != nil {
		pt.MarkBetween(traffic.PhaseHookBuild, time.Since(buildStart))
	}
	if err != nil {
		logger.Error("failed to build request hook pipeline", "error", err)
		h.writeError(w, rec, http.StatusInternalServerError, "hook pipeline error")
		return nil, nil, true
	}
	if pipeline == nil {
		// No hooks → extraction skipped. Still emit the traffic-extract counter
		// (it previously fired here as a side effect of always extracting) so the
		// exported series keeps moving on the hooks-OFF path; outcome "skipped"
		// records that no extraction ran.
		if h.deps.Metrics != nil {
			h.deps.Metrics.RecordTrafficExtract(ingressFormat, "request", "skipped")
		}
		// No hook ran → no redaction demand → persist the captured body as-is.
		rec.RequestAction = hookcore.ActionApprove
		return nil, nil, false
	}

	// [perf] Raw-body prefilter — skip the ~21%-CPU gjson extraction on benign
	// traffic. When the body carries no JSON backslash escape (so each extracted
	// content segment is a verbatim, contiguous substring of the raw bytes) AND
	// an anchor-stripped SUPERSET scan of every content hook's rules finds
	// nothing in the raw body, no rule can match the extracted content — so the
	// extraction (whose only consumer is this hook input) is skipped and the
	// pipeline runs with a nil Normalized payload. Content hooks then abstain
	// naturally; metadata hooks (size/ip/rate) are unaffected; the forwarded
	// body and the downstream cross-format translation path are untouched
	// (extraction here never fed them). Any backslash, or any hook that cannot
	// prefilter, falls through to full extraction — soundness over coverage.
	var normalized *normcore.NormalizedPayload
	prefiltered := perfHookPrefilter() &&
		bytes.IndexByte(body, '\\') < 0 &&
		!pipeline.MayMatchRawContent(body)
	if prefiltered {
		if h.deps.Metrics != nil {
			h.deps.Metrics.RecordTrafficExtract(ingressFormat, "request", "prefiltered")
		}
	} else {
		extractStart := time.Now()
		normalized = h.extractRequestContentForHooks(r.Context(), trafficAdapter, ingressFormat, body, r.URL.Path, logger)
		if pt != nil {
			pt.MarkBetween(traffic.PhaseHookExtract, time.Since(extractStart))
		}
	}

	input := &hookcore.HookInput{
		RequestID:      requestID,
		Stage:          "request",
		Normalized:     normalized,
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
		// Endpoint/modality context lets BuildPipeline gate Class-A text hooks
		// out of non-text endpoints; text modality (all current AI-gateway
		// traffic is text-in). Mirror the values passed to BuildPipeline above.
		EndpointType:  endpointType,
		InputModality: inputModality,
	}
	pipeline.SetAllowModify(true)
	pipeline.SetClearSoftOnApprove(true)

	pipelineStart := time.Now()
	hookResult := pipeline.Execute(r.Context(), input)
	if pt != nil {
		pt.MarkBetween(traffic.PhaseHookPipeline, time.Since(pipelineStart))
	}

	rec.HookDecision = string(hookResult.Decision)
	rec.HookReason = hookResult.Reason
	rec.HookReasonCode = hookResult.ReasonCode
	rec.ComplianceTags = mergeTagSets(rec.ComplianceTags, hookResult.Tags)
	rec.BlockingRule = mapBlockingRule(hookResult.BlockingRule)
	rec.HooksPipeline = appendHookTrace(rec.HooksPipeline, "request", hookResult.HookResults)
	// Carry the single hook action onto the audit Record. The writer keys
	// the persisted raw body off it: approve = captured bytes, redact/block
	// = the redacted wire copy (RequestBodyRedacted) only. Derive from the
	// pipeline Decision so a no-match approve (Action left empty by the
	// pipeline) still stamps ActionApprove and persists the captured bytes
	// rather than dropping them.
	rec.RequestAction = hookcore.ActionFromDecision(hookResult.Decision)

	if h.deps.Metrics != nil {
		h.deps.Metrics.RecordHookRequest(ingressFormat, "request", string(hookResult.Decision))
	}

	// Refuse on a folded BLOCK action (RejectHard OR a standalone BlockSoft):
	// ActionFromDecision folds BlockSoft → block, so the request stage dispatches
	// it exactly like RejectHard (a hard 403), matching the SSE/response path and
	// error-taxonomy-architecture.md — there is no soft-block client response.
	// The `!CarriesRedaction()` guard keeps a BlockSoft that MASKS a co-firing redact
	// out of this arm so it falls through to the redaction arm below and redact-forwards
	// (the #13 invariant): RejectHard never carries redaction, so its behavior is
	// unchanged.
	if hookcore.ActionFromDecision(hookResult.Decision) == hookcore.ActionBlock && !hookResult.CarriesRedaction() {
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

	// MODIFY: push hook-rewritten content back onto the upstream wire.
	// When the adapter cannot reverse-encode (ErrRewriteUnsupported) we
	// forward the original body plus a warn log rather than failing —
	// that matches how the rest of the hook pipeline treats "Modify was
	// requested but not actionable here". Any other error (malformed,
	// unknown schema after Extract succeeded) indicates an internal
	// inconsistency and surfaces as 500.
	if hookResult.CarriesRedaction() {
		rewriteStart := time.Now()
		rewriteContent := rewriteContentWithToolArgs(hookResult.ModifiedContent, normalized, hookResult.TransformSpans)
		rewritten, n, rErr := trafficAdapter.RewriteRequestBody(r.Context(), body, r.URL.Path, rewriteContent)
		if pt != nil {
			pt.MarkBetween(traffic.PhaseHookRewrite, time.Since(rewriteStart))
		}
		// Guard: only the OpenAI adapter reconstructs masked tool-call arguments
		// onto its wire (traffic.ToolArgMasker). A non-masker ingress adapter
		// (anthropic/gemini) masks text but silently drops ToolCallArgs — so a
		// successful (nil) rewrite that carried masked tool args would forward the
		// tool-arg PII UNMASKED upstream while the audit stamps it redacted. Fail
		// closed instead, mirroring the tlsbump path. No-op when no tool arg was
		// masked or the adapter is a masker.
		if rErr == nil {
			rErr = traffic.GuardToolArgMasking(trafficAdapter, rewriteContent)
		}
		switch {
		case errors.Is(rErr, traffic.ErrRewriteUnsupported):
			// Redaction was required (Modify) but this adapter cannot reverse-
			// encode the masked content onto the wire. Fail CLOSED: forwarding
			// the original body would leak the unredacted content upstream, and
			// persisting it raw would store the leak. Reject instead — the policy
			// attribution (rule / reason / tags) was already stamped above.
			logger.Warn("hook produced Modify but adapter does not support rewrite; failing closed",
				slog.String("adapter", trafficAdapter.ID()),
				slog.String("path", r.URL.Path),
			)
			rec.RequestAction = hookcore.ActionBlock
			rec.HookReasonCode = hookcore.ReasonRedactInflightUnsupported
			traffic.PrependVia(w.Header(), "ai-gateway")
			w.Header().Set("X-Nexus-Hook", traffic.FormatHookOutcome(aigwHookOutcomeFromResult(hookResult)))
			w.Header().Set("X-Nexus-Mode", "")
			traffic.SetExposeHeaders(w.Header())
			h.writeError(w, rec, http.StatusForbidden, "redaction required but not supported for this provider")
			return nil, hookResult, true
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
			// Stamp the DISPOSITION action: an applied rewrite is a redact, even when
			// the aggregate Decision is BlockSoft (a soft-block masking a co-firing
			// redact) — without this the audit row would read Action=block + a masked
			// request body.
			rec.RequestAction = hookcore.ActionRedact
			// The redacted wire copy is what the raw storage policy
			// persists under action=redact (rec.RequestBody holds
			// the pre-hook bytes for normalization and must never reach
			// raw storage when redaction is demanded — without this stamp
			// the writer fail-safes the raw copy to NULL).
			rec.RequestBodyRedacted = rewritten
			return rewritten, hookResult, false
		}
	}
	return nil, hookResult, false
}

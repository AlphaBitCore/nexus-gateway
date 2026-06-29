package proxy

// proxy_canonical_redact.go — the canonical (OpenAI-shape) response-redaction
// core, split out of proxy_upstream.go. redactCanonicalBuffer is the locked
// S-canon LOCUS: the single writer-free redaction entry point shared by the
// non-stream response path (runResponseHooksOnCanonical), the streaming buffer
// mode (runCanonicalBufferStream, proxy_cache_buffer.go), and the future B2
// Model-A escalation hand-off — one redaction impl, multiple delivery wrappers.

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/tidwall/gjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/requestcontext"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	hookcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// canonicalRedactOutcome is the writer-free result of running the response-stage
// compliance pipeline on an accumulated CANONICAL (OpenAI-shape) body. Produced
// by redactCanonicalBuffer and consumed by two callers that each deliver a
// fail-closed decision in their own transport idiom:
//
//   - the non-stream path (runResponseHooksOnCanonical) delivers a hard block as
//     an HTTP error via writeError (status not yet committed); and
//   - the streaming buffer path (runCanonicalBufferStream) delivers a hard block
//     as an in-band SSE error frame (the 200 preamble was already flushed).
//
// Keeping the redaction logic writer-free lets ONE implementation serve the
// non-stream response path AND the streaming buffer mode AND the future B2
// Model-A escalation hand-off (one impl, multiple entry points).
type canonicalRedactOutcome struct {
	// body is the (possibly redacted) canonical body. Equals the input when no
	// rewrite occurred.
	body []byte
	// rewritten is true only when a Modify decision actually rewrote the body.
	rewritten bool
	// failClosed is true when the pipeline demands the response NOT be delivered
	// unredacted: a RejectHard policy block, a pipeline build failure, or a
	// rewrite failure. The caller surfaces errStatus/errMsg in its own idiom.
	failClosed bool
	errStatus  int
	errMsg     string
	// result is the pipeline result (decision / tags / blocking rule), set
	// whenever the pipeline executed. nil on bypass / no-pipeline.
	result *hookcore.CompliancePipelineResult
}

// redactCanonicalBuffer runs the response-stage compliance pipeline on the
// CANONICAL (OpenAI-shape) body and returns the (possibly redacted) body plus
// the decision, WITHOUT writing to any client. It is the locked-design S-canon
// LOCUS: redaction happens at the canonical waist (so a Modify rewrite is wire-
// shape-agnostic — Anthropic / Gemini ingress redacts correctly), and the
// caller forward-encodes the redacted canonical to the ingress wire afterwards.
//
// This is the clean entry point shared by the non-stream response path
// (runResponseHooksOnCanonical wraps it), the streaming buffer mode
// (runCanonicalBufferStream wraps it), and the future B2 Model-A escalation
// hand-off — one redaction implementation, multiple delivery wrappers. It
// stamps the response-hook audit fields on rec exactly as the non-stream path
// always did; the wrappers handle fail-closed delivery + the wire copy.
func (h *Handler) redactCanonicalBuffer(
	r *http.Request, rec *audit.Record,
	ingress Ingress, target routingcore.RoutingTarget,
	canonicalBody []byte, tokenTotal int64, requestID string, logger *slog.Logger,
) canonicalRedactOutcome {
	out := canonicalRedactOutcome{body: canonicalBody}

	// bypassHooks: when the resolved passthrough has BypassHooks active, skip the
	// response-stage pipeline. Stamp BYPASSED symmetrically with the request stage
	// so a SIEM filter distinguishes a bypass from "no response hook configured".
	if resolved := requestcontext.ResolvedFrom(r.Context()); resolved != nil {
		if pt := resolved.Passthrough(); pt.AnyBypassActive() && pt.BypassHooks {
			rec.ResponseHookDecision = "BYPASSED"
			return out
		}
	}

	epType := typology.KindFromWireShape(ingress.WireShape)
	outputModality := []hookcore.Modality{hookcore.ModalityText}

	// Build the response pipeline first — its gating inputs (endpoint kind +
	// modality) come from the Ingress, not the body; when nil, skip extraction.
	pipeline, pErr := h.deps.HookConfigCache.Resolver(r.Context()).BuildPipeline(
		"response", "AI_GATEWAY",
		epType,
		outputModality,
		5*time.Second, 15*time.Second, perfParallelHooks(), true /* strictFailClosed */, logger,
	)
	if pErr != nil {
		logger.Error("failed to build response hook pipeline", "error", pErr)
		out.failClosed = true
		out.errStatus = http.StatusInternalServerError
		out.errMsg = "hook pipeline error"
		return out
	}

	// formatLabel keeps per-ingress metric labelling; the extractor + path are
	// CANONICAL (OpenAI) because the body is canonical at this stage.
	formatLabel := string(ingress.BodyFormat)
	if pipeline == nil {
		if h.deps.Metrics != nil {
			h.deps.Metrics.RecordTrafficExtract(formatLabel, "response", "skipped")
		}
		return out
	}

	extractor := h.trafficAdapterFor(provcore.FormatOpenAI)
	// Canonical body is OpenAI chat-completions (`choices[]`) in the common case;
	// a native /v1/responses passthrough is `output[]`-shape. SNIFF the body (not
	// the ingress shape) so a cross-format cache HIT — canonical chat served to a
	// /v1/responses reader — still dispatches the OpenAI rewriter's right branch.
	canonicalPath := "/v1/chat/completions"
	if !gjson.GetBytes(canonicalBody, "choices").Exists() && gjson.GetBytes(canonicalBody, "output").Exists() {
		canonicalPath = "/v1/responses"
	}

	respContent, respModel, respFinish := h.extractResponseForHooks(r.Context(), extractor, formatLabel, canonicalBody, canonicalPath, logger)
	respInput := &hookcore.HookInput{
		RequestID:      requestID,
		Stage:          "response",
		Normalized:     respContent,
		IngressType:    "AI_GATEWAY",
		Path:           canonicalPath,
		Model:          respModel,
		FinishReason:   respFinish,
		TokenCount:     int(tokenTotal),
		SourceIP:       middleware.ClientIP(r),
		ProviderRegion: target.Region,
		EndpointType:   epType,
		OutputModality: outputModality,
	}
	pipeline.SetAllowModify(true)
	pipeline.SetClearSoftOnApprove(true)

	hookResult := pipeline.Execute(r.Context(), respInput)
	out.result = hookResult

	rec.ResponseHookDecision = string(hookResult.Decision)
	rec.ResponseHookReason = hookResult.Reason
	rec.ResponseHookReasonCode = hookResult.ReasonCode
	rec.ComplianceTags = mergeTagSets(rec.ComplianceTags, hookResult.Tags)
	rec.HooksPipeline = appendHookTrace(rec.HooksPipeline, "response", hookResult.HookResults)
	if br := mapBlockingRule(hookResult.BlockingRule); br != nil {
		rec.BlockingRule = br
	}
	rec.ResponseAction = hookcore.ActionFromDecision(hookResult.Decision)
	if h.deps.Metrics != nil {
		h.deps.Metrics.RecordHookRequest(formatLabel, "response", string(hookResult.Decision))
	}

	if hookResult.Decision == hookcore.RejectHard {
		out.failClosed = true
		out.errStatus = http.StatusForbidden
		out.errMsg = hookResult.Reason
		return out
	}
	if hookResult.Decision == hookcore.Modify {
		// A Modify decision MUST produce an applied rewrite. The canonical locus
		// is fail-closed (the ai-gateway is not in the host packet path → strong
		// compliance): a redaction the policy demanded but could not apply must
		// NEVER deliver the original. Attempt the rewrite UNCONDITIONALLY — a
		// Modify carrying only TransformSpans (tool-arg masking) with empty
		// ModifiedContent still has real work to apply, so the old
		// len(ModifiedContent)>0 gate silently dropped it and returned the
		// original unredacted body (fail-OPEN).
		redacted, n, rErr := extractor.RewriteResponseBody(r.Context(), canonicalBody, canonicalPath, rewriteContentWithToolArgs(hookResult.ModifiedContent, respContent, hookResult.TransformSpans))
		if rErr != nil {
			// On canonical the rewrite is always supported: the sniff above only
			// ever yields /v1/chat/completions or /v1/responses, both handled by the
			// OpenAI RewriteResponseBody (only /embeddings + the default arm return
			// ErrRewriteUnsupported, neither reachable here). A genuine rewrite error
			// therefore fails closed.
			logger.Error("canonical response rewrite failed", slog.String("error", rErr.Error()))
			out.failClosed = true
			out.errStatus = http.StatusInternalServerError
			out.errMsg = "response rewrite failed"
			return out
		}
		if n == 0 {
			// The Modify produced NO applied change: empty ModifiedContent and no
			// applicable tool-arg spans. Mirror the rewrite-error arm — fail
			// closed so the unredacted original is never delivered. out.rewritten
			// stays false, so the relay's redacted-copy guard keeps storage NULL.
			logger.Error("canonical response Modify produced no applied rewrite — failing closed")
			out.failClosed = true
			out.errStatus = http.StatusInternalServerError
			out.errMsg = "response rewrite produced no change"
			return out
		}
		rec.ResponseHookRewriteCount = n
		rec.ResponseHookRewritten = true
		out.body = redacted
		out.rewritten = true
		return out
	}
	return out
}

// runResponseHooksOnCanonical executes the response-stage compliance pipeline on
// the CANONICAL (OpenAI-shape) body, BEFORE egress reshape. Redaction rewrites
// the canonical body in place; the caller then forward-encodes it to the ingress
// wire shape via egressReshapeNonStream — which always succeeds, so the
// reverse-encode ErrRewriteUnsupported / fail-closed / leak path is gone.
//
// Thin wrapper over the writer-free redactCanonicalBuffer for the non-stream
// path: a fail-closed decision (hard block, build failure, rewrite failure) is
// delivered as an HTTP error via writeError (the status is not yet committed on
// this path). Returns the (possibly redacted) canonical body, whether it was
// rewritten, and blocked=true when delivery short-circuited. Stamps the
// response-hook audit fields on rec; the CALLER sets rec.ResponseBodyRedacted
// from the reshaped WIRE body so the persisted copy stays wire-shaped.
func (h *Handler) runResponseHooksOnCanonical(
	w http.ResponseWriter, r *http.Request, rec *audit.Record,
	ingress Ingress, target routingcore.RoutingTarget,
	canonicalBody []byte, tokenTotal int64, requestID string, logger *slog.Logger,
) (out []byte, rewritten, blocked bool) {
	oc := h.redactCanonicalBuffer(r, rec, ingress, target, canonicalBody, tokenTotal, requestID, logger)
	if oc.failClosed {
		h.writeError(w, rec, oc.errStatus, oc.errMsg)
		return oc.body, false, true
	}
	return oc.body, oc.rewritten, false
}

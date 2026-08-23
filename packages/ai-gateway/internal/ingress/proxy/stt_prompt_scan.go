package proxy

// stt_prompt_scan.go — the request-stage compliance scan of the STT `prompt`
// form field. The prompt is the one free-text leaf of the multipart STT
// request (the audio bytes are fingerprint-only), so it is the only
// request-side content the pipeline can scan. Mirrors scanVideoPrompt with
// two deliberate differences: STT output is a scannable text transcript, so
// there is NO generative hard-block escalation (D5 covers image/video whose
// artifacts cannot be inspected); and the multipart forward CAN re-emit a
// modified prompt (sttproxy.SetPrompt feeds ReEmit), so the redact arm
// redacts-and-forwards instead of failing closed.

import (
	"net/http"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/ingress/sttproxy"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	hookcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// scanSTTPrompt runs the inline request-stage pipeline over the parsed
// `prompt` field and applies the verdict: block → 403; redact → rewrite the
// prompt in place (ReEmit forwards the redacted value); approve → forward
// unchanged. Returns true when it wrote a rejection (the caller must stop).
//
// Coverage honesty matches scanVideoPrompt: prompt-only ONLY when a content
// hook scanned the prompt cleanly; none when there is no prompt, no content
// hook, or a hook errored. An absent prompt skips the scan entirely — there
// is no request-side text to scan and coverage stays none.
func (h *Handler) scanSTTPrompt(w http.ResponseWriter, r *http.Request, rec *audit.Record, sttReq *sttproxy.STTRequest, requestID string) bool {
	prompt := sttReq.Prompt()
	if prompt == "" || h.deps.HookConfigCache == nil {
		return false
	}
	resolver := h.deps.HookConfigCache.Resolver(r.Context())
	pl, buildErr := resolver.BuildPipeline(
		"request", "AI_GATEWAY",
		typology.EndpointKindSTT,
		[]hookcore.Modality{hookcore.ModalityText},
		5*time.Second, 15*time.Second, perfParallelHooks(), true, /* strictFailClosed */
		h.deps.Logger,
	)
	if buildErr != nil {
		// strictFailClosed: an unbuildable pipeline must not let an unscanned
		// prompt through.
		h.writeDetailedErr(w, rec, http.StatusInternalServerError, "STT_PIPELINE_ERROR",
			"failed to build the compliance pipeline", "Retry; contact an operator if it persists")
		return true
	}
	if pl == nil {
		// No hooks bound for this stage: nothing scanned, coverage stays none.
		return false
	}
	pl.SetAllowModify(true)
	pl.SetClearSoftOnApprove(true)
	result := pl.Execute(r.Context(), &hookcore.HookInput{
		RequestID:     requestID,
		Stage:         "request",
		Normalized:    hookcore.PayloadFromTextSegments([]string{prompt}),
		IngressType:   "AI_GATEWAY",
		Method:        r.Method,
		Path:          r.URL.Path,
		EndpointType:  typology.EndpointKindSTT,
		InputModality: []hookcore.Modality{hookcore.ModalityText},
	})

	rec.HookDecision = string(result.Decision)
	rec.HookReason = result.Reason
	rec.HookReasonCode = result.ReasonCode
	rec.ComplianceTags = mergeTagSets(rec.ComplianceTags, result.Tags)
	rec.BlockingRule = mapBlockingRule(result.BlockingRule)
	rec.HooksPipeline = appendHookTrace(rec.HooksPipeline, "request", result.HookResults)
	rec.RequestAction = hookcore.ActionFromDecision(result.Decision)

	// Coverage: a content hook scanned the prompt AND no hook errored →
	// prompt-only; otherwise honesty requires none (same rule as video).
	if pl.HasContentScanningHook() {
		erred := false
		for i := range result.HookResults {
			if result.HookResults[i].Error != "" {
				erred = true
				break
			}
		}
		if !erred {
			rec.ComplianceCoverage = "prompt-only"
		}
	}

	// BLOCK arm.
	if hookcore.ActionFromDecision(result.Decision) == hookcore.ActionBlock && !result.CarriesRedaction() {
		applyHookRejectionHeaders(w, result)
		h.writeError(w, rec, http.StatusForbidden, "HOOK_BLOCKED", result.Reason)
		return true
	}

	// REDACT arm: apply the pipeline's spans to the prompt and forward the
	// sanitized value — ReEmit rebuilds the multipart with the replacement,
	// so the unredacted text never reaches the provider.
	if result.CarriesRedaction() {
		payload := hookcore.PayloadFromTextSegments([]string{prompt})
		redacted, _ := normcore.ApplySpans(*payload, result.TransformSpans)
		sttReq.SetPrompt(redacted.JoinedText("\n"))
	}
	return false
}

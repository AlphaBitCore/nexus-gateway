package proxy

// video_submit_scan.go — the prompt-scan and job-correlation arms of the
// video submit handler (split from video_handler.go along its two
// self-contained responsibilities: the compliance scan of the parsed prompt,
// and the provider-response → job-row correlation with its cost helpers).

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/tidwall/gjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/auth/vkauth"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store/asyncjob"
	geminicodec "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/gemini/codec"
	hookcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// scanVideoPrompt runs the inline compliance pipeline over the parsed prompt
// and applies the block arms. Returns true when it wrote a rejection (the
// caller must stop). Coverage honesty (D-V7): prompt-only ONLY when a content
// hook actually scanned the prompt cleanly; none when the pipeline has no
// content hook, or a hook errored (fail-open — the prompt was NOT cleanly
// scanned and the row must not claim it was).
func (h *Handler) scanVideoPrompt(w http.ResponseWriter, r *http.Request, rec *audit.Record, prompt, requestID string) bool {
	rec.ComplianceCoverage = "none"
	if h.deps.HookConfigCache == nil {
		return false
	}
	resolver := h.deps.HookConfigCache.Resolver(r.Context())
	pl, buildErr := resolver.BuildPipeline(
		"request", "AI_GATEWAY",
		typology.EndpointKindVideoGeneration,
		[]hookcore.Modality{hookcore.ModalityText},
		5*time.Second, 15*time.Second, perfParallelHooks(), true, /* strictFailClosed */
		h.deps.Logger,
	)
	if buildErr != nil {
		// strictFailClosed: an unbuildable pipeline must not let an
		// unscanned generative prompt through.
		h.writeDetailedErr(w, rec, http.StatusInternalServerError, "VIDEO_PIPELINE_ERROR",
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
		EndpointType:  typology.EndpointKindVideoGeneration,
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
	// prompt-only. A hook error means the content was not cleanly scanned
	// (the strict pipeline may still have approved on a fail-open observe
	// hook) — honesty requires none.
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

	// BLOCK arm (RejectHard or a standalone BlockSoft — ActionFromDecision
	// folds both to block; there is no soft-block client response).
	if hookcore.ActionFromDecision(result.Decision) == hookcore.ActionBlock && !result.CarriesRedaction() {
		applyHookRejectionHeaders(w, result)
		h.writeError(w, rec, http.StatusForbidden, result.Reason)
		return true
	}

	// MODIFY (redact) arm: the multipart wire cannot reverse-encode a
	// redacted prompt (the rebuild forwards the parsed value verbatim), so
	// forwarding would leak the unredacted content upstream. Fail CLOSED,
	// matching the image/TTS interim posture.
	if result.CarriesRedaction() {
		rec.RequestAction = hookcore.ActionBlock
		rec.HookReasonCode = hookcore.ReasonRedactInflightUnsupported
		applyHookRejectionHeaders(w, result)
		h.writeError(w, rec, http.StatusForbidden, "redaction required but not supported for video prompts")
		return true
	}

	// Generative hard-block escalation (D5): a content match — even
	// observe-only — on an uninspectable-output endpoint escalates to 403.
	return h.maybeBlockGenerativePrompt(w, rec, typology.EndpointKindVideoGeneration, result, h.deps.Logger)
}

// videoEstimateUsd computes the advisory submit estimate: requested seconds ×
// the model's per-second price (InputUsdPerM = USD per 1M seconds). priced
// reports whether the model carries a price row at all — the caller
// distinguishes "unpriced" (missing row → $0 that silently bypasses every
// cost cap → fail closed under an enforced quota) from "free" (explicit 0 —
// allowed). A missing Models dependency or a transient lookup error reports
// priced (fail open), consistent with the chat pre-check posture.
func (h *Handler) videoEstimateUsd(r *http.Request, modelID string, seconds float64) (usd float64, priced bool) {
	if h.deps.Models == nil {
		return 0, true
	}
	m, err := h.deps.Models.GetModel(r.Context(), modelID)
	if err != nil {
		return 0, true
	}
	if m == nil || m.InputPricePM == nil {
		return 0, false
	}
	return seconds / 1e6 * (*m.InputPricePM), true
}

// insertVideoJob parses the provider's job object and writes the correlation
// row. Returns true when it wrote an error response (the caller must stop):
// a provider 2xx the gateway cannot correlate is surfaced as a 502 — the
// client could never poll the job through the gateway, and the orphaned
// provider-side job is logged for operator follow-up (it exists at the
// provider; the gateway has no handle to cancel it).
func (h *Handler) insertVideoJob(w http.ResponseWriter, r *http.Request, rec *audit.Record,
	vkMeta *vkauth.VKMeta, providerID, credentialID, modelID string,
	requestedSeconds float64, traceID string, respBody []byte) bool {
	jobID := gjson.GetBytes(respBody, "id").String()
	if jobID == "" {
		h.deps.Logger.Error("video: provider 2xx submit carried no job id; provider-side job may be orphaned",
			slog.String("provider", providerID), slog.String("model", modelID))
		h.writeDetailedErr(w, rec, http.StatusBadGateway, "VIDEO_CORRELATION_FAILED",
			"the provider accepted the job but returned no job id",
			"Retry; if it persists the provider's response shape may have changed")
		return true
	}
	// The veo_ prefix is the gateway's RESERVED leg namespace: a stored id
	// carrying it routes every follow-up onto the Veo cross-shape path. An
	// OpenAI-shape provider echoing a veo_-prefixed id would hijack that
	// dispatch (delete silently local-only, poll/content mis-decoded) — a
	// 2xx-confirmed job the client could never operate. Refuse to correlate.
	if geminicodec.IsVeoJobID(jobID) {
		h.deps.Logger.Error("video: provider returned a reserved veo_-prefixed job id; refusing correlation",
			slog.String("provider", providerID), slog.String("job", jobID))
		h.writeDetailedErr(w, rec, http.StatusBadGateway, "VIDEO_CORRELATION_FAILED",
			"the provider returned a job id the gateway reserves for another provider shape",
			"Retry; if it persists the provider's response shape may have changed")
		return true
	}
	job := asyncjob.Job{
		ProviderID:     providerID,
		ID:             jobID,
		EndpointType:   string(typology.EndpointKindVideoGeneration),
		VirtualKeyID:   vkMeta.ID,
		OrganizationID: vkMeta.OrganizationID,
		ProjectID:      vkMeta.ProjectID,
		CredentialID:   credentialID,
		ModelID:        modelID,
		RequestedUnits: requestedSeconds,
		Status:         videoJobStatusFromResponse(respBody),
		SubmitTraceID:  traceID,
		ExpiresAt:      videoJobExpiry(respBody),
	}
	if insErr := h.deps.AsyncJobs.Insert(r.Context(), job); insErr != nil {
		// Conflict = a row with this provider-scoped id already exists —
		// NEVER overwritten (an upsert would rebind another submitter's job).
		// Both arms are a 502 with the provider-side job flagged orphaned.
		h.deps.Logger.Error("video: job row insert failed after provider 2xx; provider-side job is orphaned",
			slog.String("provider", providerID), slog.String("job", jobID),
			slog.Bool("conflict", errors.Is(insErr, asyncjob.ErrConflict)),
			slog.String("error", insErr.Error()))
		h.writeDetailedErr(w, rec, http.StatusBadGateway, "VIDEO_CORRELATION_FAILED",
			"the provider accepted the job but the gateway could not record it",
			"Retry; contact an operator if it persists")
		return true
	}
	return false
}

// applyHookRejectionHeaders writes the hook-outcome headers before an error
// response commits the status line, mirroring the inline block arm: via +
// X-Nexus-Hook so the client sees the attribution, X-Nexus-Mode reserved
// empty for chain alignment.
func applyHookRejectionHeaders(w http.ResponseWriter, result *hookcore.CompliancePipelineResult) {
	traffic.PrependVia(w.Header(), "ai-gateway")
	w.Header().Set("X-Nexus-Hook", traffic.FormatHookOutcome(aigwHookOutcomeFromResult(result)))
	w.Header().Set("X-Nexus-Mode", "")
	traffic.SetExposeHeaders(w.Header())
}

// videoParseErrorResponse maps a videoproxy.ParseSubmit error to the HTTP
// status, machine code, message, and hint. A MaxBytesReader trip is a 413;
// every other malformed / governance-bound multipart error is a client 400
// carrying the parser's own message.
func videoParseErrorResponse(err error) (status int, code, message, hint string) {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		return http.StatusRequestEntityTooLarge, "VIDEO_UPLOAD_TOO_LARGE",
			"submit body exceeds the video size ceiling",
			"Reduce the input_reference size, or an operator can raise AI_GATEWAY_GENERATIVE_CAP_VIDEO_GENERATION_MAX_BYTES"
	}
	return http.StatusBadRequest, "VIDEO_BAD_MULTIPART",
		err.Error(),
		"Fix the multipart body: a non-empty prompt, a model field, integer seconds, no duplicated or case-variant governance fields"
}

// videoJobStatusFromResponse normalizes the provider job object's status into
// the closed store vocabulary. A just-submitted job whose response carries an
// unknown or absent status is recorded as queued — the poll path re-observes
// the real state on the next client poll; writing an unmapped provider string
// would be rejected by the store's vocabulary guard.
func videoJobStatusFromResponse(respBody []byte) string {
	s := gjson.GetBytes(respBody, "status").String()
	switch s {
	case asyncjob.StatusQueued, asyncjob.StatusInProgress, asyncjob.StatusCompleted, asyncjob.StatusFailed:
		return s
	}
	return asyncjob.StatusQueued
}

// videoJobExpiry extracts the provider's expires_at (unix seconds) when
// present. Nil when absent — the poll path fills it in as observed.
func videoJobExpiry(respBody []byte) *time.Time {
	v := gjson.GetBytes(respBody, "expires_at")
	if !v.Exists() || v.Type != gjson.Number || v.Int() <= 0 {
		return nil
	}
	t := time.Unix(v.Int(), 0).UTC()
	return &t
}

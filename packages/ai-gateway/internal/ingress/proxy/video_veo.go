package proxy

// video_veo.go — the Veo cross-shape leg of the async video family (e88-s6
// §3b): when routing resolves a Gemini-format provider, the client still
// speaks OpenAI /v1/videos while the wire is Veo `:predictLongRunning` +
// long-running operations. The pure translation lives in
// specs/gemini/codec/videos.go; this file is the handler dispatch — submit
// encodes and correlates, poll translates the LRO onto the canonical job
// object (so the SAME observe/reconcile machinery runs on both legs), content
// dereferences the provider artifact URI under the D-V5b guard, and delete is
// best-effort local (Veo LROs have no client-facing delete — a §3b capability
// difference).

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/auth/vkauth"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/ingress/envelope"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/ingress/videoproxy"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store/asyncjob"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	geminicodec "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/gemini/codec"
	geminierrors "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/gemini/errors"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// relayVeoProviderError reshapes a Veo (Gemini-wire) non-2xx response into the
// canonical OpenAI error envelope the client expects on the /v1/videos
// surface. §3a: a cross-shape adapter owns its error normalization — a raw
// Gemini `{"error":{"code":400,"status":...}}` (numeric code, no `type`) must
// not reach an OpenAI-shaped client. The provider status code is preserved.
func (h *Handler) relayVeoProviderError(w http.ResponseWriter, status int, header http.Header, body []byte) {
	pe := geminierrors.ErrorNormalizer{}.Normalize(status, header, body)
	envBody := envelope.EncodeErrorEnvelopeForIngress(provcore.FormatOpenAI, provcore.FormatGemini, pe)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(envBody)
}

// serveVeoSubmit forwards one parsed canonical submit to the Veo wire:
// encode (allow-list-only; unknown extras → 400) → POST
// {base}/v1beta/models/{model}:predictLongRunning → correlate the returned
// operation name as the veo_-encoded canonical id → estimate stamp →
// synthesize the canonical video object for the client.
func (h *Handler) serveVeoSubmit(w http.ResponseWriter, r *http.Request, rec *audit.Record,
	vkMeta *vkauth.VKMeta, req *videoproxy.SubmitRequest, callTarget provcore.CallTarget,
	modelID string, requestedSeconds, estimateUsd float64, traceID string) {
	params := geminicodec.VeoSubmitParams{
		Prompt: req.Prompt,
		// The wire carries the exact duration the gateway BILLS (the
		// defaulted requestedSeconds), never the raw client value — so a
		// client that omits `seconds` cannot make Veo render its own
		// (longer) default while the estimate/quota/reconcile price the
		// canonical 4 s default (D-V6 estimate-as-floor would silently break).
		SecondsInt:      int(requestedSeconds),
		Size:            req.Size,
		ExtraFieldNames: req.ExtraFieldNames(),
	}
	if data, mimeType, fieldName, ok := req.InputFileBytes(); ok {
		// Allow-list-only: a file part under any name OTHER than
		// input_reference is an untranslatable canonical field on this leg —
		// reject it rather than silently reinterpret its bytes as the input
		// image (the OpenAI leg re-emits it under its own name; this leg
		// cannot).
		if fieldName != "input_reference" {
			h.writeDetailedErr(w, rec, http.StatusBadRequest, "VIDEO_FIELD_UNSUPPORTED",
				"file field \""+fieldName+"\" is not supported for this video model",
				"Send the image as input_reference, or remove the file part")
			return
		}
		params.InputRefBytes = data
		params.InputRefMime = mimeType
	}
	body, coercions, encErr := geminicodec.EncodeVeoSubmit(params)
	if encErr != nil {
		// Allow-list-only leg: an untranslatable field or unmappable size is
		// the CLIENT's 400 (the message names the field), never a forward.
		h.writeDetailedErr(w, rec, http.StatusBadRequest, "VIDEO_FIELD_UNSUPPORTED",
			encErr.Error(),
			"Remove the unsupported field, or use a size with a Veo mapping")
		return
	}

	upstreamURL := strings.TrimRight(callTarget.BaseURL, "/") + "/v1beta/models/" +
		url.PathEscape(callTarget.ProviderModelID) + ":predictLongRunning"
	upReq, ok := h.videoAuthedRequest(w, r, rec, callTarget, http.MethodPost, upstreamURL,
		"/v1beta/models/"+callTarget.ProviderModelID+":predictLongRunning")
	if !ok {
		return
	}
	upReq.Body = io.NopCloser(bytes.NewReader(body))
	upReq.ContentLength = int64(len(body))
	upReq.Header.Set("Content-Type", "application/json")

	resp, doErr := h.deps.UpstreamClient.Do(upReq)
	if doErr != nil {
		h.writeDetailedErr(w, rec, http.StatusBadGateway, "UPSTREAM_ERROR",
			"upstream provider request failed", "Retry; the provider may be unavailable")
		return
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, videoSubmitMaxResponseBytes))
	rec.StatusCode = resp.StatusCode

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// The provider owns its enums (duration, model constraints) — its
		// rejection is normalized into the OpenAI error envelope (the client
		// speaks the OpenAI /v1/videos surface, not the Gemini wire).
		h.relayVeoProviderError(w, resp.StatusCode, resp.Header, respBody)
		return
	}

	opName, opErr := geminicodec.VeoOperationNameFromSubmit(respBody)
	if opErr != nil {
		h.deps.Logger.Error("veo: provider 2xx submit carried no operation name; provider-side job may be orphaned",
			slog.String("provider", callTarget.ProviderID), slog.String("model", modelID),
			slog.String("error", opErr.Error()))
		h.writeDetailedErr(w, rec, http.StatusBadGateway, "VIDEO_CORRELATION_FAILED",
			"the provider accepted the job but returned no operation name",
			"Retry; if it persists the provider's response shape may have changed")
		return
	}
	canonicalID := geminicodec.VeoJobID(opName)
	// Validate the minted id round-trips to a safe operation name BEFORE
	// correlating — a hostile/malformed provider operation name must fail at
	// submit (502, no orphaned-but-confirmed job), not surface as a job whose
	// every follow-up 502s VIDEO_JOB_ID_UNSAFE.
	if _, nameErr := geminicodec.VeoOperationName(canonicalID); nameErr != nil {
		h.deps.Logger.Error("veo: provider operation name is unsafe; refusing correlation",
			slog.String("provider", callTarget.ProviderID), slog.String("model", modelID),
			slog.String("error", nameErr.Error()))
		h.writeDetailedErr(w, rec, http.StatusBadGateway, "VIDEO_CORRELATION_FAILED",
			"the provider returned an operation reference the gateway cannot safely track",
			"Retry; if it persists the provider's response shape may have changed")
		return
	}

	now := time.Now().UTC()
	view, decErr := geminicodec.DecodeVeoOperation(respBody, canonicalID, modelID, int(requestedSeconds), now)
	if decErr != nil {
		h.writeDetailedErr(w, rec, http.StatusBadGateway, "VIDEO_CORRELATION_FAILED",
			"the provider accepted the job but its response could not be decoded",
			"Retry; contact an operator if it persists")
		return
	}
	expires := view.ExpiresAt
	job := asyncjob.Job{
		ProviderID:     callTarget.ProviderID,
		ID:             canonicalID,
		EndpointType:   string(typology.EndpointKindVideoGeneration),
		VirtualKeyID:   vkMeta.ID,
		OrganizationID: vkMeta.OrganizationID,
		ProjectID:      vkMeta.ProjectID,
		CredentialID:   callTarget.CredentialID,
		ModelID:        modelID,
		RequestedUnits: requestedSeconds,
		Status:         view.Status,
		SubmitTraceID:  traceID,
		ExpiresAt:      &expires,
	}
	if insErr := h.deps.AsyncJobs.Insert(r.Context(), job); insErr != nil {
		h.deps.Logger.Error("veo: job row insert failed after provider 2xx; provider-side job is orphaned",
			slog.String("provider", callTarget.ProviderID), slog.String("job", canonicalID),
			slog.String("error", insErr.Error()))
		h.writeDetailedErr(w, rec, http.StatusBadGateway, "VIDEO_CORRELATION_FAILED",
			"the provider accepted the job but the gateway could not record it",
			"Retry; contact an operator if it persists")
		return
	}
	// The submit row is the job's single cost-bearing row (D-V6).
	rec.EstimatedCostUsd = estimateUsd

	// Capture the job response so the Traffic drawer renders it, symmetric with
	// the OpenAI submit leg (the Veo body is the canonical job JSON). The
	// multipart video request bytes stay uncaptured (R-7).
	h.captureParallelResponse(rec, view.ClientBody, "application/json")

	if len(coercions) > 0 {
		w.Header().Set("X-Nexus-Coerced", strings.Join(coercions, ","))
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(view.ClientBody)
}

// veoOperationForward decodes the job's operation name, resolves the pinned
// credential, and fetches the LRO once, returning the decoded canonical view.
// Returns ok=false when it wrote an error response; a non-2xx provider
// response is relayed verbatim inside (ok=false there too — the caller is
// done).
func (h *Handler) veoOperationForward(w http.ResponseWriter, r *http.Request, rec *audit.Record,
	job asyncjob.Job) (geminicodec.VeoJobView, provcore.CallTarget, bool) {
	opName, opErr := geminicodec.VeoOperationName(job.ID)
	if opErr != nil {
		// The id was minted from a provider response — a hostile mint fails
		// here on every decode, never into a URL.
		h.writeDetailedErr(w, rec, http.StatusBadGateway, "VIDEO_JOB_ID_UNSAFE",
			"the stored operation reference cannot be decoded safely",
			"The provider returned a malformed operation name at submit; contact an operator")
		return geminicodec.VeoJobView{}, provcore.CallTarget{}, false
	}
	callTarget, ok := h.videoResolvePinned(w, r, rec, job)
	if !ok {
		return geminicodec.VeoJobView{}, provcore.CallTarget{}, false
	}
	// opName is validated segment-by-segment (safe charset) — it joins the
	// path verbatim.
	upstreamURL := strings.TrimRight(callTarget.BaseURL, "/") + "/v1beta/" + opName
	upReq, ok := h.videoAuthedRequest(w, r, rec, callTarget, http.MethodGet, upstreamURL, "/v1beta/"+opName)
	if !ok {
		return geminicodec.VeoJobView{}, provcore.CallTarget{}, false
	}
	resp, doErr := h.deps.UpstreamClient.Do(upReq)
	if doErr != nil {
		h.writeDetailedErr(w, rec, http.StatusBadGateway, "UPSTREAM_ERROR",
			"upstream provider request failed", "Retry; the provider may be unavailable")
		return geminicodec.VeoJobView{}, provcore.CallTarget{}, false
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, videoFollowMaxResponseBytes))
	rec.StatusCode = resp.StatusCode
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		h.relayVeoProviderError(w, resp.StatusCode, resp.Header, respBody)
		return geminicodec.VeoJobView{}, provcore.CallTarget{}, false
	}
	view, decErr := geminicodec.DecodeVeoOperation(respBody, job.ID, job.ModelID, int(job.RequestedUnits), job.CreatedAt)
	if decErr != nil {
		h.writeDetailedErr(w, rec, http.StatusBadGateway, "VIDEO_VEO_DECODE_FAILED",
			"the provider's operation response could not be decoded",
			"Retry; if it persists the provider's response shape may have changed")
		return geminicodec.VeoJobView{}, provcore.CallTarget{}, false
	}
	return view, callTarget, true
}

// serveVeoPoll translates one LRO poll onto the canonical video object and
// feeds the SAME observe/reconcile machinery the OpenAI leg uses (the
// synthesized body carries the canonical status/expires_at the observe step
// reads).
func (h *Handler) serveVeoPoll(w http.ResponseWriter, r *http.Request, rec *audit.Record,
	vkMeta *vkauth.VKMeta, job asyncjob.Job) {
	view, callTarget, ok := h.veoOperationForward(w, r, rec, job)
	if !ok {
		return
	}
	rec.StatusCode = http.StatusOK
	h.relayVideoUpstream(w, callTarget.Format, http.Header{"Content-Type": {"application/json"}},
		http.StatusOK, view.ClientBody)
	h.observeVideoPoll(vkMeta, job, view.ClientBody)
}

// serveVeoContent dereferences the provider artifact URI (D-V5b) and streams
// it through the same hygiene relay as the OpenAI leg. Veo has no
// thumbnail/spritesheet variants (§3b capability difference).
func (h *Handler) serveVeoContent(w http.ResponseWriter, r *http.Request, rec *audit.Record,
	job asyncjob.Job) {
	if v := r.URL.Query().Get("variant"); v != "" && v != "video" {
		h.writeDetailedErr(w, rec, http.StatusBadRequest, "VIDEO_VARIANT_UNSUPPORTED",
			"this video model serves only the video variant",
			"Omit the variant parameter, or set variant=video")
		return
	}
	view, callTarget, ok := h.veoOperationForward(w, r, rec, job)
	if !ok {
		return
	}
	switch view.Status {
	case asyncjob.StatusCompleted:
		// fall through to the fetch
	case asyncjob.StatusFailed:
		h.writeDetailedErr(w, rec, http.StatusBadRequest, "VIDEO_FAILED",
			"the render failed; there is no artifact to download",
			"Poll GET /v1/videos/{id} for the failure detail")
		return
	default:
		h.writeDetailedErr(w, rec, http.StatusBadRequest, "VIDEO_NOT_READY",
			"the video has not finished generating",
			"Poll GET /v1/videos/{id} until status=completed, then download")
		return
	}
	if view.VideoURI == "" {
		h.writeDetailedErr(w, rec, http.StatusBadGateway, "VIDEO_VEO_DECODE_FAILED",
			"the completed operation carries no artifact URI",
			"Retry; if it persists the provider's response shape may have changed")
		return
	}
	resp, fetchErr := doFetchVeoArtifact(r.Context(), view.VideoURI, callTarget.APIKey)
	if fetchErr != nil {
		// Covers the allow-list refusal, the SSRF dial guard, and plain
		// network failure — none of them may leak the attempted URL to the
		// client (it is provider-controlled data).
		h.deps.Logger.Warn("veo: artifact fetch refused or failed",
			slog.String("job", job.ID), slog.String("error", fetchErr.Error()))
		h.writeDetailedErr(w, rec, http.StatusBadGateway, "VIDEO_ARTIFACT_FETCH_FAILED",
			"the provider's artifact location could not be fetched safely",
			"Retry; contact an operator if it persists")
		return
	}
	defer func() { _ = resp.Body.Close() }()
	rec.StatusCode = resp.StatusCode
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, videoFollowMaxResponseBytes))
		h.relayVideoUpstream(w, callTarget.Format, resp.Header, resp.StatusCode, respBody)
		return
	}
	h.streamVideoArtifact(w, r, rec, job, resp, callTarget.Format)
}

// serveVeoDelete is the best-effort local cancel: Veo LROs are not
// client-deletable via a documented endpoint, so the row is marked canceled
// (the provider-side render runs to completion — a §3b capability difference
// the docs state) and the client gets the canonical deletion object.
func (h *Handler) serveVeoDelete(w http.ResponseWriter, r *http.Request, rec *audit.Record,
	vkMeta *vkauth.VKMeta, job asyncjob.Job) {
	if err := h.deps.AsyncJobs.MarkCanceled(r.Context(), job.ProviderID, job.ID, vkMeta.ID); err != nil {
		// The mark IS the whole effect on this leg — a failed mark must fail
		// loud (unlike the OpenAI leg, where the provider delete already
		// happened and the mark is bookkeeping).
		h.writeDetailedErr(w, rec, http.StatusServiceUnavailable, "VIDEO_STORE_UNAVAILABLE",
			"could not record the cancel", "Retry shortly")
		return
	}
	rec.StatusCode = http.StatusOK
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"id":` + jsonQuote(job.ID) + `,"object":"video","deleted":true}`))
}

// jsonQuote marshals one string as a JSON string literal (Marshal on a plain
// string cannot fail).
func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

package proxy

// video_handler.go — the parallel async video-generation SUBMIT handler for
// multipart POST /v1/videos (e88-s6 D-V8). Like STT and guardrail, video does
// NOT go through the small-JSON ServeProxy pipeline: the submit is a large
// multipart upload whose response is a JOB object, not a completion — the
// response cache, canonical/codec bridge, and byte-slice executor do not fit.
// The handler is a straight line — admission → render-bound check → bounded
// multipart parse → prompt scan (hard-block) → advisory cost check → route +
// resolve → native multipart forward → job-row insert → estimate stamp —
// reusing the SAME cross-cutting subset (overload gate, VK auth, per-VK RPM,
// per-VK generative-caps concurrency, router + credential resolve, quota
// engine, audit writer) via the shared Handler methods and deps. Chat core
// (provcore.Request / executor / spec_adapter) is UNCHANGED.
//
// Cost model (D-V6): the submit row is the job's SINGLE cost-bearing
// traffic_event row — it stamps the requested-seconds × per-second-price
// estimate on provider 2xx (estimate-as-floor; for the OpenAI leg the
// provider renders exactly the requested seconds, so the estimate is
// essentially exact). The poll that first observes completion reconciles the
// LIVE quota counters (T6); poll/content rows stamp $0 so the rollup backfill
// (which sums estimated_cost_usd over all rows) counts each job exactly once.

import (
	"bytes"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/forwardheader"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/ingress/videoproxy"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/generativecaps"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/quota"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	provtarget "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/target"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

const (
	// videoDefaultSeconds is the provider default duration used for the cost
	// estimate when the client omits `seconds` (the OpenAI create default).
	videoDefaultSeconds = 4

	// videoMaxNonTerminalDefault bounds a VK's simultaneous non-terminal jobs
	// — the REAL render bound (the gencaps concurrency row bounds in-flight
	// HTTP requests, not provider renders; a VK could otherwise loop
	// sequential submits into unbounded concurrent paid renders). Built-in,
	// env-overridable via AI_GATEWAY_VIDEO_MAX_NONTERMINAL_JOBS in the
	// generative-caps idiom. The bound is check-then-insert without a
	// transaction (the provider round-trip sits between them, so no PG
	// transaction can span it) — concurrent submits can briefly overshoot by
	// at most the gencaps HTTP-concurrency cap, the same advisory-admission
	// posture every other gateway bound takes.
	videoMaxNonTerminalDefault = 4

	// videoSubmitMaxResponseBytes bounds the job-object body the handler
	// buffers. A job object is a small JSON document; 1 MiB is a defensive
	// ceiling so a misbehaving upstream cannot force an unbounded read.
	videoSubmitMaxResponseBytes = 1 << 20
)

// videoMaxNonTerminalJobs resolves the per-VK non-terminal jobs cap once per
// call (the env read is cheap and submit is a rare, seconds-of-GPU-priced
// request). A garbage or negative value keeps the built-in default — a typo
// must never silently disable a spend bound.
func videoMaxNonTerminalJobs(logger *slog.Logger) int {
	v, ok := os.LookupEnv("AI_GATEWAY_VIDEO_MAX_NONTERMINAL_JOBS")
	if !ok || v == "" {
		return videoMaxNonTerminalDefault
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		if logger != nil {
			logger.Warn("video: AI_GATEWAY_VIDEO_MAX_NONTERMINAL_JOBS is not a positive int; using built-in default",
				slog.String("value", v), slog.Int("default", videoMaxNonTerminalDefault))
		}
		return videoMaxNonTerminalDefault
	}
	return n
}

// ServeVideoSubmit returns the HTTP handler for POST /v1/videos.
func (h *Handler) ServeVideoSubmit(in Ingress) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// In-flight admission gate: reject-fast before any per-request setup.
		if h.gate != nil {
			if !h.gate.acquire() {
				writeOverloaded(w, in.BodyFormat)
				return
			}
			defer h.gate.release()
		}

		start := time.Now().UTC()
		requestID := r.Header.Get("X-Nexus-Request-Id")
		rec := &audit.Record{
			RequestID:       requestID,
			ClientRequestID: r.Header.Get("x-request-id"),
			TraceID:         requestID,
			Timestamp:       start,
			Method:          r.Method,
			Path:            r.URL.Path,
			SourceIP:        middleware.ClientIP(r),
			IngressFormat:   string(in.BodyFormat),
			EndpointType:    string(typology.EndpointKindVideoGeneration),
		}
		stampCallerAttribution(rec, r.Header)

		// Owned, panic-safe audit tail: the generative-caps slot release and
		// the audit enqueue run on EVERY exit path (success, error,
		// panic-unwind).
		var releaseCap func()
		defer func() {
			if releaseCap != nil {
				releaseCap()
			}
			if h.deps.AuditWriter != nil {
				h.finalize(rec, start)
			}
		}()

		// No-DB degrade: without the job store the correlation guarantee
		// (read-your-write poll, authz binding, render bound) cannot be
		// honored — every video route serves 503, stated not implied.
		if h.deps.AsyncJobs == nil {
			h.writeDetailedErr(w, rec, http.StatusServiceUnavailable, "VIDEO_STORE_UNAVAILABLE",
				"the async-job store is unavailable; video routes require a database",
				"Contact an operator — the gateway is running without its database")
			return
		}

		// VK auth.
		vkMeta, err := h.authenticate(r)
		if err != nil {
			h.writeAuthError(w, rec, err)
			return
		}
		rec.ApplyVKMeta(vkMeta)

		// Per-VK RPM rate limit (sets Retry-After on rejection).
		if err := h.checkRateLimit(w, vkMeta); err != nil {
			h.writeDetailedErr(w, rec, http.StatusTooManyRequests, "RATE_LIMITED",
				"per-key rate limit exceeded", "Reduce request rate or retry after the Retry-After interval")
			return
		}

		// Per-(VK, video) generative-caps concurrency — bounds in-flight HTTP
		// requests on this endpoint. Release is wired into the panic-safe
		// tail above.
		caps, generative := generativecaps.Lookup(typology.EndpointKindVideoGeneration)
		if generative && caps.MaxConcurrentPerVK > 0 && h.genConcurrency != nil {
			if !h.genConcurrency.Acquire(typology.EndpointKindVideoGeneration, vkMeta.ID, caps.MaxConcurrentPerVK) {
				generativeCapShedTotal.WithLabelValues(string(typology.EndpointKindVideoGeneration)).Inc()
				w.Header().Set("Retry-After", "1")
				h.writeDetailedErr(w, rec, http.StatusTooManyRequests, "GENERATIVE_CONCURRENCY_LIMIT",
					"too many concurrent generative requests for this key",
					"Reduce concurrency or retry shortly")
				return
			}
			vkID := vkMeta.ID
			releaseCap = func() { h.genConcurrency.Release(typology.EndpointKindVideoGeneration, vkID) }
		}

		// The render bound (D-V9): count the VK's non-terminal jobs and
		// reject at the cap. This — not the HTTP-concurrency cap above — is
		// what bounds concurrent paid renders.
		nonTerminal, countErr := h.deps.AsyncJobs.CountNonTerminal(r.Context(), vkMeta.ID)
		if countErr != nil {
			h.writeDetailedErr(w, rec, http.StatusServiceUnavailable, "VIDEO_STORE_UNAVAILABLE",
				"could not check the in-flight job bound", "Retry shortly")
			return
		}
		if maxJobs := videoMaxNonTerminalJobs(h.deps.Logger); nonTerminal >= maxJobs {
			w.Header().Set("Retry-After", "30")
			h.writeDetailedErr(w, rec, http.StatusTooManyRequests, "VIDEO_JOBS_LIMIT",
				"too many in-flight video jobs for this key",
				"Wait for running jobs to complete (poll GET /v1/videos/{id}) or delete unneeded ones")
			return
		}

		// Multipart boundary. A non-multipart Content-Type is a malformed
		// submit (400) — the body must be multipart/form-data.
		mediaType, params, mtErr := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if mtErr != nil || !strings.HasPrefix(mediaType, "multipart/") || params["boundary"] == "" {
			h.writeDetailedErr(w, rec, http.StatusBadRequest, "VIDEO_BAD_MULTIPART",
				"request Content-Type must be multipart/form-data with a boundary",
				"Send the submit as multipart/form-data, e.g. curl -F prompt='...' -F model=sora-2")
			return
		}

		// Bound the total upload mid-stream BEFORE parsing (the caps ceiling,
		// 16 MiB — the optional input_reference image rides inside) so a
		// chunked / lying-Content-Length upload cannot force an unbounded
		// read. ParseSubmit adds the per-part / per-field structural bounds
		// and validates seconds BEFORE any cost arithmetic (EstimatedCost()
		// does no NaN/negative clamping — the parser's positive-int ≤300
		// bound is what keeps the estimate finite).
		maxBytes := caps.MaxRequestBytes
		if maxBytes <= 0 {
			maxBytes = 16 << 20
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBytes)

		req, parseErr := videoproxy.ParseSubmit(r.Body, params["boundary"])
		if parseErr != nil {
			status, code, message, hint := videoParseErrorResponse(parseErr)
			h.writeDetailedErr(w, rec, status, code, message, hint)
			return
		}

		// The input_reference fingerprint is the INPUT artifact; the image
		// content is NOT scanned (binary).
		if refs := req.ArtifactRefsJSON(); refs != "" {
			rec.ArtifactRefs = refs
		}

		// Capture the reference image itself, under the same operator switch
		// that governs every other request body, and by handover rather than
		// copy — the bytes are already parsed and ReEmit only reads them, so
		// the request path pays a slice assignment. Without this the
		// generation was auditable and the image it was generated FROM was
		// not, which is the same gap the STT audio path had.
		if h.payloadCaptureConfig().StoreRequestBody {
			if img, mime := req.InputBytes(); len(img) > 0 {
				rec.AttachOwnedRequestBody(img, mime)
			}
		}

		// Prompt scan (D-V7): build the hook input DIRECTLY from the parsed
		// prompt (the multipart wire never rides ServeProxy's JSON
		// extraction) and run the SAME inline pipeline, strict fail-closed.
		// Then the generative hard-block escalation (D5): video output is an
		// uninspectable artifact, so a content match on the prompt — even
		// observe-only — escalates to 403.
		if blocked := h.scanVideoPrompt(w, r, rec, req.Prompt, requestID); blocked {
			return
		}

		// Route: resolve the requested model → provider target via the shared
		// router (metadata-only; the prompt is not a routable text leaf here).
		rctx := h.buildRequestContext(r, vkMeta, nil, in.BodyFormat, req.Model, string(typology.EndpointKindVideoGeneration))
		routeRes, routeErr := h.resolveRouteOrPassthrough(r.Context(), rctx, in, req.Model, typology.EndpointKindVideoGeneration)
		if routeErr != nil || routeRes == nil || len(routeRes.AllTargets()) == 0 {
			// 404, NOT 5xx: an unknown / unconfigured / not-allowed model is
			// client-correctable; a 5xx would make an SDK retry forever.
			h.writeDetailedErr(w, rec, http.StatusNotFound, "NO_COMPATIBLE_PROVIDER",
				"no provider is configured to serve this video model",
				"Check the model name, or add a provider + model mapping for it")
			return
		}
		target0 := routeRes.Primary()
		rec.ModelID = target0.ModelID
		rec.ModelName = target0.ModelName
		rec.RoutedModelID = target0.ModelID
		rec.RoutedModelName = target0.ModelName
		rec.RoutedProviderID = target0.ProviderID
		rec.RoutedProviderName = target0.ProviderName

		// Advisory cost check (D-V6): estimate = requested seconds × the
		// model's per-second price (InputUsdPerM is USD per 1M seconds).
		// There is NO admission debit anywhere in the gateway — Check is
		// advisory like every endpoint; the live counters reconcile at
		// first-observed completion (T6). seconds was range-validated by the
		// parser, so the estimate is finite and non-negative by construction.
		requestedSeconds := float64(req.SecondsInt)
		if requestedSeconds <= 0 {
			requestedSeconds = videoDefaultSeconds
		}
		estimateUsd, modelPriced := h.videoEstimateUsd(r, target0.ModelID, requestedSeconds)
		if !modelPriced {
			// The nil-vs-explicit-0 price distinction (chat-path parity): an
			// unpriced model's $0 is a missing price row, not a free model —
			// surface it so cost dashboards don't read "no spend".
			rec.Metadata = stampUnpricedCost(rec.Metadata)
		}
		if h.deps.QuotaEngine != nil {
			chain := quota.BuildCheckChain(vkMeta, h.deps.QuotaEngine.OrgParents())
			decision := h.deps.QuotaEngine.Check(r.Context(), chain,
				quota.CostEstimate{EstimatedUsd: estimateUsd}, vkMeta)
			// An unpriced model estimates $0, never trips Check, and its
			// submit row (the job's SINGLE cost-bearing row) would record $0
			// for real seconds-of-GPU spend. When a cost cap is actively
			// enforced for this caller, fail closed — the chat path's
			// QUOTA_MODEL_UNPRICED posture; video is the costliest
			// per-request endpoint, not the place to waive it.
			if !modelPriced && quotaHasCostLimit(decision) {
				h.deps.Logger.Warn("video quota: routed model has no price configured; rejecting under an active cost quota",
					slog.String("model", target0.ModelID), slog.String("vk", vkMeta.ID))
				h.writeDetailedErr(w, rec, http.StatusServiceUnavailable, "QUOTA_MODEL_UNPRICED",
					"routed model has no price configured; cost quota cannot be enforced",
					"Ask an admin to set this model's pricing before it can be used under a cost quota")
				return
			}
			if decision != nil && !decision.Allowed {
				// No downgrade arm: per-second video models are not
				// interchangeable the way chat tiers are; reject is the only
				// quota action video serves.
				h.writeDetailedErr(w, rec, http.StatusTooManyRequests, "QUOTA_EXCEEDED",
					decision.Message, "Check usage or request a quota increase")
				return
			}
		}

		// Resolve the single upstream target (BaseURL + decrypted key +
		// provider model id) via the same resolver the executor holds. Video
		// submit forwards to ONE target with NO failover — a failed submit
		// creates no job and the client retries.
		if h.deps.Resolver == nil {
			h.writeDetailedErr(w, rec, http.StatusBadGateway, "NO_COMPATIBLE_PROVIDER",
				"video target resolver is not configured", "Contact an operator")
			return
		}
		callTarget, resolveErr := h.deps.Resolver.Resolve(r.Context(), target0.ProviderID, target0.ModelID,
			provtarget.ResolveHints{StickyKey: vkMeta.ID})
		if resolveErr != nil {
			h.writeDetailedErr(w, rec, http.StatusBadGateway, "PROVIDER_RESOLVE_FAILED",
				"could not resolve the upstream provider credential", "Check the provider credential configuration")
			return
		}
		rec.ProviderID = callTarget.ProviderID
		rec.ProviderName = callTarget.ProviderName
		rec.CredentialID = callTarget.CredentialID
		rec.CredentialName = callTarget.CredentialName

		// The Veo cross-shape leg (§3b): a Gemini-format target speaks
		// `:predictLongRunning`, not the OpenAI videos wire — the codec
		// translates and the correlation uses the veo_-encoded operation
		// name as the canonical id.
		if callTarget.Format == provcore.FormatGemini {
			h.serveVeoSubmit(w, r, rec, vkMeta, req, callTarget,
				target0.ModelID, requestedSeconds, estimateUsd, requestID)
			return
		}

		// Re-emit the multipart body with the model field rewritten to the
		// resolved provider model id. The provider only ever sees this
		// gateway-emitted rebuild — the scanned prompt and the forwarded
		// prompt are the same value by construction.
		fwdBody, fwdContentType, emitErr := req.ReEmit(callTarget.ProviderModelID)
		if emitErr != nil {
			h.writeDetailedErr(w, rec, http.StatusInternalServerError, "VIDEO_REEMIT_FAILED",
				"failed to rebuild the multipart body for forwarding", "Retry; contact an operator if it persists")
			return
		}

		// Build the upstream request by hand (whitelist-by-omission headers,
		// fresh re-emit boundary as the single Content-Type — allowlist
		// BEFORE Set, mirroring the STT forward order). Native passthrough:
		// the resolved provider speaks the OpenAI videos shape (a Gemini
		// target took the Veo cross-shape branch above).
		upstreamURL := strings.TrimRight(callTarget.BaseURL, "/") + r.URL.Path
		upReq, reqErr := http.NewRequestWithContext(r.Context(), http.MethodPost, upstreamURL, bytes.NewReader(fwdBody))
		if reqErr != nil {
			h.writeDetailedErr(w, rec, http.StatusBadGateway, "VIDEO_UPSTREAM_BUILD_FAILED",
				"failed to build the upstream request", "Check the provider base URL configuration")
			return
		}
		forwardheader.Apply(upReq.Header, r.Header, h.deps.Allowlist, string(callTarget.Format))
		upReq.Header.Set("Content-Type", fwdContentType)

		adapter, ok := h.deps.ProviderReg.Get(callTarget.Format)
		if !ok {
			h.writeDetailedErr(w, rec, http.StatusBadGateway, "NO_COMPATIBLE_PROVIDER",
				"no adapter registered for the resolved provider format", "Contact an operator")
			return
		}
		auth, ok := adapter.(authApplier)
		if !ok {
			h.writeDetailedErr(w, rec, http.StatusBadGateway, "PROVIDER_AUTH_UNAVAILABLE",
				"resolved provider adapter cannot apply auth to a video forward", "Contact an operator")
			return
		}
		if authErr := auth.ApplyAuth(upReq, callTarget); authErr != nil {
			h.writeDetailedErr(w, rec, http.StatusBadGateway, "PROVIDER_AUTH_FAILED",
				"failed to apply provider authentication", "Check the provider credential")
			return
		}
		rec.TargetMethod = http.MethodPost
		rec.TargetPath = r.URL.Path
		rec.TargetHost = hostOf(callTarget.BaseURL)

		// Forward.
		resp, doErr := h.deps.UpstreamClient.Do(upReq)
		if doErr != nil {
			h.writeDetailedErr(w, rec, http.StatusBadGateway, "UPSTREAM_ERROR",
				"upstream provider request failed", "Retry; the provider may be unavailable")
			return
		}
		defer func() { _ = resp.Body.Close() }()

		respBody, _ := readResponseBounded(resp, videoSubmitMaxResponseBytes)
		rec.StatusCode = resp.StatusCode

		// On provider 2xx: correlate. The job row is the authz + credential-
		// pin + render-bound record every poll/content/delete depends on —
		// a 2xx we cannot correlate must NOT reach the client as a usable
		// job (they could never poll it through the gateway).
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			if failed := h.insertVideoJob(w, r, rec, vkMeta, callTarget.ProviderID, callTarget.CredentialID,
				target0.ModelID, requestedSeconds, requestID, respBody); failed {
				return
			}
			// The submit row is the job's single cost-bearing row
			// (estimate-as-floor): an abandoned job's cost of record is this
			// estimate, never $0. Provider-rejected submits skip this (no
			// job, nothing to account).
			rec.EstimatedCostUsd = estimateUsd
		}

		// Capture the job response (small JSON) so the Traffic drawer shows it;
		// the multipart video request bytes stay uncaptured (R-7). Gated on the
		// payload-capture toggle.
		h.captureParallelResponse(rec, respBody, resp.Header.Get("Content-Type"))

		// Relay: allowlisted response headers, upstream status, job body.
		h.relayVideoUpstream(w, callTarget.Format, resp.Header, resp.StatusCode, respBody)
	}
}

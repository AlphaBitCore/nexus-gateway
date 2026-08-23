package proxy

// video_follow_handler.go — the follow-up half of the async video family
// (e88-s6 T6): ServeVideoPoll (GET /v1/videos/{id}) and ServeVideoDelete
// (DELETE /v1/videos/{id}). Both are governed passthroughs keyed by the job
// row (D-V4): the client supplies only the provider's job id, the row supplies
// everything else — the provider scope, the VK authz binding, and the
// submit-time credential pin — so an unknown or foreign id is a 404
// NON-DISCLOSURE that never probes the upstream, and a follow-up always
// reaches the SAME provider account that owns the job (ResolveHints
// .CredentialID; pool re-selection hours later could pick a different
// credential and 404 a live job).
//
// Cost (D-V6): the poll that FIRST observes a terminal provider state claims
// the job's single live-quota reconciliation (asyncjob.ReconcileOnce) and, on
// `completed`, debits the live counters with realized = requested seconds ×
// the model's per-second price — NEVER a provider-reported usage figure: the
// submit-row estimate is the cost floor, and a gamed usage payload must not
// reconcile below it. The poll/delete traffic_event rows themselves stamp $0
// (the submit row is the job's single cost-bearing row).

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/tidwall/gjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/auth/vkauth"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store/asyncjob"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/quota"
	geminicodec "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/gemini/codec"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// videoFollowMaxResponseBytes bounds the poll/delete relay body. Both carry a
// small JSON job object; 1 MiB is the same defensive ceiling the submit relay
// applies (videoSubmitMaxResponseBytes) so a misbehaving upstream cannot force
// an unbounded read on any video metadata path.
const videoFollowMaxResponseBytes = 1 << 20

// videoBookkeepingTimeout bounds the post-relay store/quota writes. They run
// on a background context: once the PROVIDER state is observed (or a delete
// accepted), the gateway's records must catch up even if the polling client
// has already disconnected — a canceled request context here would silently
// drop a cost reconciliation or leave a provider-deleted job counting against
// the render bound.
const videoBookkeepingTimeout = 5 * time.Second

// ServeVideoPoll returns the HTTP handler for GET /v1/videos/{id}: job-row
// authz → pinned resolve → verbatim relay of the provider's job object (the
// caller sees expires_at and provider errors as-is) → last-observed cache
// update + first-terminal-observer cost reconcile.
func (h *Handler) ServeVideoPoll(in Ingress) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.gate != nil {
			if !h.gate.acquire() {
				writeOverloaded(w, in.BodyFormat)
				return
			}
			defer h.gate.release()
		}

		start := time.Now().UTC()
		rec := newVideoFollowRecord(r, in, start)
		defer func() {
			if h.deps.AuditWriter != nil {
				h.finalize(rec, start)
			}
		}()

		vkMeta, job, ok := h.videoFollowJob(w, r, rec)
		if !ok {
			return
		}
		if geminicodec.IsVeoJobID(job.ID) {
			h.serveVeoPoll(w, r, rec, vkMeta, job)
			return
		}
		respHeader, respStatus, respBody, callTarget, ok := h.videoFollowForward(w, r, rec, job, http.MethodGet)
		if !ok {
			return
		}
		// Relay FIRST (flushed), bookkeeping after: a degraded DB must not
		// hold the client's poll result hostage for the bookkeeping timeout.
		// The poll row stamps $0 cost by construction (rec.EstimatedCostUsd is
		// never set) — the submit row is the job's single cost-bearing row.
		h.relayVideoUpstream(w, callTarget.Format, respHeader, respStatus, respBody)
		if respStatus >= 200 && respStatus < 300 {
			h.observeVideoPoll(vkMeta, job, respBody)
		}
	}
}

// ServeVideoDelete returns the HTTP handler for DELETE /v1/videos/{id}: job-row
// authz → pinned resolve → relay of the provider delete; a provider 2xx marks
// the row canceled (with the VK predicate — T3 defense-in-depth) so the slot
// stops counting against the render bound.
func (h *Handler) ServeVideoDelete(in Ingress) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.gate != nil {
			if !h.gate.acquire() {
				writeOverloaded(w, in.BodyFormat)
				return
			}
			defer h.gate.release()
		}

		start := time.Now().UTC()
		rec := newVideoFollowRecord(r, in, start)
		defer func() {
			if h.deps.AuditWriter != nil {
				h.finalize(rec, start)
			}
		}()

		vkMeta, job, ok := h.videoFollowJob(w, r, rec)
		if !ok {
			return
		}
		if geminicodec.IsVeoJobID(job.ID) {
			h.serveVeoDelete(w, r, rec, vkMeta, job)
			return
		}
		respHeader, respStatus, respBody, callTarget, ok := h.videoFollowForward(w, r, rec, job, http.MethodDelete)
		if !ok {
			return
		}
		// Relay first (flushed), then record — same never-delays posture as
		// the poll arm.
		h.relayVideoUpstream(w, callTarget.Format, respHeader, respStatus, respBody)
		if respStatus >= 200 && respStatus < 300 {
			// Background context: the provider accepted the delete — the row
			// must record it even if the client has gone away. A failed mark
			// leaves the row non-terminal until the retention sweep; that
			// direction only over-counts the render bound (fails toward
			// bounding spend), so the mark failure is a WARN, never a client
			// error.
			bkCtx, cancel := context.WithTimeout(context.Background(), videoBookkeepingTimeout)
			defer cancel()
			if err := h.deps.AsyncJobs.MarkCanceled(bkCtx, job.ProviderID, job.ID, vkMeta.ID); err != nil {
				h.deps.Logger.Warn("video delete: provider accepted but the job row could not be marked canceled",
					slog.String("provider", job.ProviderID), slog.String("job", job.ID),
					slog.String("error", err.Error()))
			}
		}
	}
}

// ServeVideoUnsupported returns a handler for a deliberately-unserved video
// sub-route (list, remix, edits, extensions, characters). It answers the
// OpenAI-shaped error envelope with an actionable message + an audit row —
// NOT the mux's bare-text 404, which an SDK cannot distinguish from a wrong
// base URL (review F-prod-4/F-arch-7). A VK-scoped list served from the job
// store is a deferred seam; remix/edits/extensions render additional paid
// seconds and enter scope only with their own admission-cost design.
func (h *Handler) ServeVideoUnsupported(in Ingress, code, message, hint string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now().UTC()
		rec := newVideoFollowRecord(r, in, start)
		defer func() {
			if h.deps.AuditWriter != nil {
				h.finalize(rec, start)
			}
		}()
		h.writeDetailedErr(w, rec, http.StatusNotFound, code, message, hint)
	}
}

// newVideoFollowRecord builds the audit record shared by the poll and delete
// arms.
func newVideoFollowRecord(r *http.Request, in Ingress, start time.Time) *audit.Record {
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
	return rec
}

// videoFollowJob runs the shared follow-up admission: store presence, VK auth,
// RPM, and the owned job-row lookup. Returns ok=false when it wrote a
// response. The 404 arm is a NON-DISCLOSURE: no row and a foreign VK's row are
// the same ErrNotFound (the VK predicate lives in the store query), and the
// unknown id is NEVER forwarded upstream — the gateway must not become a probe
// of the provider account's job namespace.
func (h *Handler) videoFollowJob(w http.ResponseWriter, r *http.Request, rec *audit.Record) (*vkauth.VKMeta, asyncjob.Job, bool) {
	if h.deps.AsyncJobs == nil {
		h.writeDetailedErr(w, rec, http.StatusServiceUnavailable, "VIDEO_STORE_UNAVAILABLE",
			"the async-job store is unavailable; video routes require a database",
			"Contact an operator — the gateway is running without its database")
		return nil, asyncjob.Job{}, false
	}
	vkMeta, err := h.authenticate(r)
	if err != nil {
		h.writeAuthError(w, rec, err)
		return nil, asyncjob.Job{}, false
	}
	rec.ApplyVKMeta(vkMeta)
	if err := h.checkRateLimit(w, vkMeta); err != nil {
		h.writeDetailedErr(w, rec, http.StatusTooManyRequests, "RATE_LIMITED",
			"per-key rate limit exceeded", "Reduce request rate or retry after the Retry-After interval")
		return nil, asyncjob.Job{}, false
	}

	id := r.PathValue("id")
	if id == "" {
		h.writeDetailedErr(w, rec, http.StatusNotFound, "VIDEO_JOB_NOT_FOUND",
			"no video job with this id exists for this key",
			"Check the job id; jobs are visible only to the key that submitted them")
		return nil, asyncjob.Job{}, false
	}
	job, getErr := h.deps.AsyncJobs.GetOwned(r.Context(), id, vkMeta.ID,
		string(typology.EndpointKindVideoGeneration))
	switch {
	case errors.Is(getErr, asyncjob.ErrNotFound):
		h.writeDetailedErr(w, rec, http.StatusNotFound, "VIDEO_JOB_NOT_FOUND",
			"no video job with this id exists for this key",
			"Check the job id; jobs are visible only to the key that submitted them")
		return nil, asyncjob.Job{}, false
	case errors.Is(getErr, asyncjob.ErrAmbiguous):
		// Two providers issued the same id string to this VK. Guessing could
		// relay the poll/delete to the wrong provider account — fail loud.
		h.writeDetailedErr(w, rec, http.StatusBadGateway, "VIDEO_JOB_AMBIGUOUS",
			"this job id matches jobs on more than one provider for this key",
			"Contact an operator; the gateway cannot tell which provider job this id refers to")
		return nil, asyncjob.Job{}, false
	case getErr != nil:
		h.writeDetailedErr(w, rec, http.StatusServiceUnavailable, "VIDEO_STORE_UNAVAILABLE",
			"could not look up the video job", "Retry shortly")
		return nil, asyncjob.Job{}, false
	}

	// Audit stamps from the row (the resolve stamps overwrite with the fuller
	// names on success, but a resolve failure still leaves the trail).
	rec.ModelID = job.ModelID
	rec.ModelName = job.ModelID
	rec.ProviderID = job.ProviderID
	rec.CredentialID = job.CredentialID

	if job.Status == asyncjob.StatusExpired {
		// Gateway-invented terminal state (retention sweep) — distinguishable
		// from the 404 non-disclosure, and honest that the gateway, not the
		// provider, ended the record.
		h.writeDetailedErr(w, rec, http.StatusGone, "VIDEO_JOB_EXPIRED",
			"this video job's gateway record has passed its retention window",
			"The job is no longer tracked; any artifact may also have expired at the provider. Submit a new job.")
		return nil, asyncjob.Job{}, false
	}
	return vkMeta, job, true
}

// observeVideoPoll applies a 2xx poll body to the gateway's records: the
// last-observed status cache, and — for the FIRST observer of a terminal
// state — the job's single live-quota reconciliation. It runs AFTER the relay
// is written and EVERY call it makes rides the background bookkeeping context
// (see videoBookkeepingTimeout): the provider state has been observed, so the
// gateway's records must catch up even if the polling client is gone, and no
// bookkeeping failure can reach the already-relayed response.
func (h *Handler) observeVideoPoll(vkMeta *vkauth.VKMeta, job asyncjob.Job, respBody []byte) {
	status, mapped := videoPollStatus(respBody)
	if !mapped {
		// An unmapped provider status must not corrupt the lifecycle cache
		// (the store would reject it; regressing to a guess like "queued"
		// could flip a terminal row non-terminal). The body still relays
		// verbatim — the CLIENT sees the provider's truth either way.
		h.deps.Logger.Warn("video poll: provider status outside the job vocabulary; last-observed cache not updated",
			slog.String("provider", job.ProviderID), slog.String("job", job.ID),
			slog.String("status", gjson.GetBytes(respBody, "status").String()))
		return
	}

	bkCtx, cancel := context.WithTimeout(context.Background(), videoBookkeepingTimeout)
	defer cancel()

	obs := asyncjob.Observation{
		Status:      status,
		CompletedAt: videoJobCompletedAt(respBody),
		ExpiresAt:   videoJobExpiry(respBody),
	}
	if err := h.deps.AsyncJobs.MarkObserved(bkCtx, job.ProviderID, job.ID, obs); err != nil {
		h.deps.Logger.Warn("video poll: could not update the job's last-observed state",
			slog.String("provider", job.ProviderID), slog.String("job", job.ID),
			slog.String("error", err.Error()))
	}

	if status != asyncjob.StatusCompleted && status != asyncjob.StatusFailed {
		return
	}

	// Resolve the realized cost BEFORE taking the once-only claim: the claim
	// is irreversible, so everything that could transiently fail must happen
	// while the reconcile is still retriable by a later poll. Realized =
	// requested seconds × the model's CURRENT per-second price — the same
	// formula and price source as the submit estimate, and NEVER a
	// provider-reported usage figure: the estimate is the floor, and a gamed
	// usage payload in the poll body must not reconcile below it.
	var realized float64
	var priced bool
	if status == asyncjob.StatusCompleted {
		var priceErr error
		realized, priced, priceErr = h.videoRealizedUsd(bkCtx, job.ModelID, job.RequestedUnits)
		if priceErr != nil {
			// Transient lookup failure — do NOT consume the claim; a later
			// poll re-observes the terminal state and retries the reconcile.
			h.deps.Logger.Warn("video poll: price lookup failed; reconcile left for a later poll",
				slog.String("model", job.ModelID), slog.String("job", job.ID),
				slog.String("error", priceErr.Error()))
			return
		}
	}

	// First-terminal-observer claim: at most one poll reconciles the job's
	// live quota (concurrent polls race on the row's cost_reconciled flag).
	claimed, err := h.deps.AsyncJobs.ReconcileOnce(bkCtx, job.ProviderID, job.ID)
	if err != nil {
		// The claim was not taken — a later poll retries it. Fail toward
		// "reconcile later", never toward double-charging.
		h.deps.Logger.Warn("video poll: reconcile claim failed; a later poll will retry",
			slog.String("provider", job.ProviderID), slog.String("job", job.ID),
			slog.String("error", err.Error()))
		return
	}
	if !claimed || status != asyncjob.StatusCompleted {
		// Already reconciled by an earlier observer, or failed: a failed
		// render debits nothing, and the claim is deliberately consumed so a
		// provider that later flip-flops failed→completed cannot re-enter
		// (the submit row's estimate stays in immutable history — D-V6).
		return
	}

	if !priced || realized <= 0 {
		// Genuinely unpriced (no price row) — a later poll can do no better,
		// so the claim stays consumed; the submit row remains the cost of
		// record and the live counters self-heal via the boot backfill.
		h.deps.Logger.Warn("video poll: completed job has no positive per-second price (missing or zero price row); live counters not adjusted (the submit row remains the cost of record)",
			slog.String("model", job.ModelID), slog.String("job", job.ID))
		return
	}
	if h.deps.QuotaEngine == nil {
		return
	}
	// Check with a zero estimate only STAMPS the chain (per-level periods +
	// limits) so Reconcile debits each level under its own period key; the
	// debit lands regardless of the decision's Allowed — the render already
	// happened and its cost is owed.
	chain := quota.BuildCheckChain(vkMeta, h.deps.QuotaEngine.OrgParents())
	decision := h.deps.QuotaEngine.Check(bkCtx, chain, quota.CostEstimate{}, vkMeta)
	h.deps.QuotaEngine.Reconcile(bkCtx, decision, quota.ActualUsage{CostUSD: realized})
}

// videoRealizedUsd resolves the live-debit amount for a completed job on the
// bookkeeping context. Unlike the submit-time videoEstimateUsd (an advisory
// pre-check that deliberately fail-opens on lookup errors), this
// distinguishes a transient lookup failure (err — the caller must leave the
// reconcile claim untaken so a later poll retries) from a genuinely unpriced
// model (priced=false — retrying cannot help).
func (h *Handler) videoRealizedUsd(ctx context.Context, modelID string, seconds float64) (usd float64, priced bool, err error) {
	if h.deps.Models == nil {
		return 0, false, nil
	}
	m, lookupErr := h.deps.Models.GetModel(ctx, modelID)
	if lookupErr != nil {
		return 0, false, lookupErr
	}
	if m == nil || m.InputPricePM == nil {
		return 0, false, nil
	}
	return seconds / 1e6 * (*m.InputPricePM), true, nil
}

// videoPollStatus maps the provider job object's status into the closed store
// vocabulary. Unlike the submit-time default (a just-created job is safely
// "queued"), a poll must NOT guess: mapped=false means "leave the cache
// alone". canceled/expired never arrive from the provider — they are
// gateway-local states (delete relay / retention sweep).
func videoPollStatus(respBody []byte) (status string, mapped bool) {
	s := gjson.GetBytes(respBody, "status").String()
	switch s {
	case asyncjob.StatusQueued, asyncjob.StatusInProgress, asyncjob.StatusCompleted, asyncjob.StatusFailed:
		return s, true
	}
	return "", false
}

// videoJobCompletedAt extracts the provider's completed_at (unix seconds) when
// present; nil otherwise (MarkObserved's COALESCE keeps the prior value).
func videoJobCompletedAt(respBody []byte) *time.Time {
	v := gjson.GetBytes(respBody, "completed_at")
	if !v.Exists() || v.Type != gjson.Number || v.Int() <= 0 {
		return nil
	}
	t := time.Unix(v.Int(), 0).UTC()
	return &t
}

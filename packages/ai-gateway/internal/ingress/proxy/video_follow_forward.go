package proxy

// video_follow_forward.go — the shared upstream-forward plumbing for the
// async video follow-up routes (poll / delete / content): the hostile-id
// guard, the pinned-credential resolve (D-V4), the request build, the
// buffered small-body forward, and the shared relay tail. Split from
// video_follow_handler.go along the admission-vs-forward seam.

import (
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/forwardheader"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store/asyncjob"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	provdispatch "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/dispatch"
	provtarget "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/target"
)

// videoJobIDUnsafe rejects stored job ids that could rewrite the upstream URL
// path. The id is PROVIDER-controlled (it arrived in the submit response), so
// a hostile provider could have planted a traversal sequence; url.PathEscape
// below neutralizes reserved characters, but "." / ".." survive escaping as
// whole segments and a backslash is a path separator to some upstream stacks.
// No real provider id shape (OpenAI video_…, the veo_ base64url encoding)
// contains any of these.
func videoJobIDUnsafe(id string) bool {
	return id == "." || id == ".." || strings.ContainsAny(id, "/\\")
}

// videoResolvePinned runs the pinned-credential resolve (D-V4) with the
// audit provider stamps. Returns ok=false when it wrote an error response.
func (h *Handler) videoResolvePinned(w http.ResponseWriter, r *http.Request, rec *audit.Record,
	job asyncjob.Job) (provcore.CallTarget, bool) {
	if h.deps.Resolver == nil {
		h.writeDetailedErr(w, rec, http.StatusBadGateway, "NO_COMPATIBLE_PROVIDER",
			"video target resolver is not configured", "Contact an operator")
		return provcore.CallTarget{}, false
	}
	// Pin the submit-time credential (D-V4): the follow-up must reach the
	// SAME provider account that owns the job. A rotated/disabled/removed
	// credential fails the resolve — the job row stays intact and the client
	// can retry after an operator restores a credential for that provider.
	callTarget, resolveErr := h.deps.Resolver.Resolve(r.Context(), job.ProviderID, job.ModelID,
		provtarget.ResolveHints{CredentialID: job.CredentialID})
	if resolveErr != nil {
		h.writeDetailedErr(w, rec, http.StatusBadGateway, "PROVIDER_RESOLVE_FAILED",
			"the job's pinned provider credential could not be resolved",
			"The submit-time credential may be disabled or removed; an operator can restore one for this provider — the job record is intact")
		return provcore.CallTarget{}, false
	}
	rec.ProviderID = callTarget.ProviderID
	rec.ProviderName = callTarget.ProviderName
	rec.CredentialID = callTarget.CredentialID
	rec.CredentialName = callTarget.CredentialName
	return callTarget, true
}

// videoAuthedRequest builds the upstream request for an already-resolved
// target: allowlisted headers, provider auth, audit target stamps. targetPath
// is the audit-facing path (the URL minus the base). Returns ok=false when it
// wrote an error response.
func (h *Handler) videoAuthedRequest(w http.ResponseWriter, r *http.Request, rec *audit.Record,
	callTarget provcore.CallTarget, method, upstreamURL, targetPath string) (*http.Request, bool) {
	upReq, reqErr := http.NewRequestWithContext(r.Context(), method, upstreamURL, nil)
	if reqErr != nil {
		h.writeDetailedErr(w, rec, http.StatusBadGateway, "VIDEO_UPSTREAM_BUILD_FAILED",
			"failed to build the upstream request", "Check the provider base URL configuration")
		return nil, false
	}
	forwardheader.Apply(upReq.Header, r.Header, h.deps.Allowlist, string(callTarget.Format))

	adapter, ok := h.deps.ProviderReg.Get(callTarget.Format)
	if !ok {
		h.writeDetailedErr(w, rec, http.StatusBadGateway, "NO_COMPATIBLE_PROVIDER",
			"no adapter registered for the resolved provider format", "Contact an operator")
		return nil, false
	}
	auth, ok := adapter.(authApplier)
	if !ok {
		h.writeDetailedErr(w, rec, http.StatusBadGateway, "PROVIDER_AUTH_UNAVAILABLE",
			"resolved provider adapter cannot apply auth to a video forward", "Contact an operator")
		return nil, false
	}
	if authErr := auth.ApplyAuth(upReq, callTarget); authErr != nil {
		h.writeDetailedErr(w, rec, http.StatusBadGateway, "PROVIDER_AUTH_FAILED",
			"failed to apply provider authentication", "Check the provider credential")
		return nil, false
	}
	rec.TargetMethod = method
	rec.TargetPath = targetPath
	rec.TargetHost = hostOf(callTarget.BaseURL)
	return upReq, true
}

// videoFollowRequest runs the shared follow-up forward plumbing for the
// OpenAI leg: the hostile-id guard, the pinned resolve, and the authed
// request build. pathSuffix extends the job URL (""/poll+delete, "/content"
// for the artifact download); rawQuery is appended verbatim when non-empty
// (the caller builds it from an explicit param allow-list, never the client's
// raw query string). Returns ok=false when it wrote an error response.
func (h *Handler) videoFollowRequest(w http.ResponseWriter, r *http.Request, rec *audit.Record,
	job asyncjob.Job, method, pathSuffix, rawQuery string) (*http.Request, provcore.CallTarget, bool) {
	if videoJobIDUnsafe(job.ID) {
		h.writeDetailedErr(w, rec, http.StatusBadGateway, "VIDEO_JOB_ID_UNSAFE",
			"the stored provider job id contains path characters and will not be relayed",
			"The provider returned a malformed job id at submit; contact an operator")
		return nil, provcore.CallTarget{}, false
	}
	callTarget, ok := h.videoResolvePinned(w, r, rec, job)
	if !ok {
		return nil, provcore.CallTarget{}, false
	}
	// The id is escaped into the URL — combined with the videoJobIDUnsafe
	// guard above, a stored id can never traverse out of /v1/videos/ at the
	// provider.
	upstreamURL := strings.TrimRight(callTarget.BaseURL, "/") + "/v1/videos/" + url.PathEscape(job.ID) + pathSuffix
	if rawQuery != "" {
		upstreamURL += "?" + rawQuery
	}
	upReq, ok := h.videoAuthedRequest(w, r, rec, callTarget, method, upstreamURL, "/v1/videos/"+job.ID+pathSuffix)
	if !ok {
		return nil, provcore.CallTarget{}, false
	}
	return upReq, callTarget, true
}

// videoFollowForward resolves the job's pinned credential and relays METHOD
// {BaseURL}/v1/videos/{id} with a bounded read. Returns ok=false when it wrote
// a response. The response body is fully buffered (1 MiB ceiling) and closed
// here before returning, so the observe step reads the same bytes the client
// gets. Header + status are returned as values rather than the *http.Response
// precisely because the body is already spent — a caller must never be handed
// a response whose body it cannot read or close.
func (h *Handler) videoFollowForward(w http.ResponseWriter, r *http.Request, rec *audit.Record,
	job asyncjob.Job, method string) (http.Header, int, []byte, provcore.CallTarget, bool) {
	upReq, callTarget, ok := h.videoFollowRequest(w, r, rec, job, method, "", "")
	if !ok {
		return nil, 0, nil, provcore.CallTarget{}, false
	}
	resp, doErr := h.deps.UpstreamClient.Do(upReq)
	if doErr != nil {
		h.writeDetailedErr(w, rec, http.StatusBadGateway, "UPSTREAM_ERROR",
			"upstream provider request failed", "Retry; the provider may be unavailable")
		return nil, 0, nil, provcore.CallTarget{}, false
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, videoFollowMaxResponseBytes))
	rec.StatusCode = resp.StatusCode
	return resp.Header, resp.StatusCode, respBody, callTarget, true
}

// relayVideoUpstream writes the provider response through to the client:
// allowlisted headers, upstream status, buffered body. Shared by the submit,
// poll, and delete arms plus the content route's non-2xx error relay. The Content-Type is provider-controlled, so nosniff
// pins how a browser-navigable GET is interpreted (a hostile provider must
// not turn the authenticated gateway origin into a content-sniffed HTML
// page). The flush pushes the bytes to the client before any post-relay
// bookkeeping runs — a degraded DB delays the gateway's own writes, never the
// client's poll result.
func (h *Handler) relayVideoUpstream(w http.ResponseWriter, format provcore.Format,
	respHeader http.Header, status int, body []byte) {
	filtered := provdispatch.FilterResponseHeaders(h.deps.Allowlist, format, respHeader, false)
	for k, vs := range filtered {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	if ct := respHeader.Get("Content-Type"); ct != "" && w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", ct)
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(body)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

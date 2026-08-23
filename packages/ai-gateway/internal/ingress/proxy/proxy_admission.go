// proxy_admission.go carries the gates a request passes BEFORE any routing
// decision exists: who is calling, and are they allowed to call this often.
//
// Split from proxy_routing.go, which starts once admission has succeeded and
// asks a different question — which target should serve this. The two change
// for different reasons: admission follows the virtual-key and rate-limit
// contracts, routing follows the resolver and the catalog.
package proxy

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/auth/vkauth"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
)

// build, route resolution, and quota-enforcement helpers split out of proxy.go
// (behavior unchanged). ServeProxy orchestrates these in order.

func (h *Handler) authenticate(r *http.Request) (*vkauth.VKMeta, error) {
	if h.deps.VKAuth == nil {
		return nil, fmt.Errorf("VIRTUAL_KEY_MISSING: authenticator not configured")
	}
	return h.deps.VKAuth.Authenticate(r.Context(), r)
}

// writeAuthError writes an appropriate auth error response with machine-parseable codes.
//
// Not every failure to authenticate is the caller's fault. When the lookup
// itself could not run — database unreachable, query error — answering 401
// tells the caller their key is bad, so they rotate it, which cannot help,
// and our outage stays invisible behind a wall of client-side errors. That
// case answers 503 and says whose problem it is.
func (h *Handler) writeAuthError(w http.ResponseWriter, rec *audit.Record, err error) {
	if errors.Is(err, vkauth.ErrUnavailable) {
		h.writeDetailedErr(w, rec, http.StatusServiceUnavailable,
			"AUTH_BACKEND_UNAVAILABLE",
			"virtual key could not be verified: "+err.Error(),
			"This is a gateway-side failure, not a problem with your key — retry, and escalate if it persists")
		return
	}
	code := "AUTH_INVALID_KEY"
	hint := "Verify your virtual key is correct"
	switch {
	case errors.Is(err, vkauth.ErrMissing):
		code = "AUTH_KEY_MISSING"
		hint = "Include a virtual key via X-Nexus-Virtual-Key header or Authorization: Bearer"
	case errors.Is(err, vkauth.ErrDisabled):
		code = "AUTH_KEY_DISABLED"
		hint = "This key has been disabled by an administrator"
	case errors.Is(err, vkauth.ErrExpired):
		code = "AUTH_KEY_EXPIRED"
		hint = "This key has expired; request a new one from your admin"
	}
	h.writeDetailedErr(w, rec, http.StatusUnauthorized, code, err.Error(), hint)
}

// checkRateLimit checks per-key rate limits. Sets Retry-After header on rejection.
//
// The bucket is keyed on vkMeta.ID — the globally-unique VirtualKey id —
// NOT vkMeta.Name. VirtualKey.name has no uniqueness constraint, so two
// tenants that happen to pick the same display label would otherwise share
// one Redis bucket (`nexus:rl:<name>`) and exhaust each other's budget.
//
// /v1/estimate compare requests use a dedicated per-VK bucket
// (checkCompareRateLimit, keyed by the VK id + ":compare") so estimation
// traffic cannot exhaust the real-call quota and vice versa.
func (h *Handler) checkRateLimit(w http.ResponseWriter, vkMeta *vkauth.VKMeta) error {
	if vkMeta.RateLimitRpm == nil || h.deps.RateLimiter == nil {
		return nil
	}
	// Stamped before the verdict, not after it. The caller sets this header on
	// the allow path only, so the one response that most needs to say what the
	// limit is — the 429 — was the one that did not carry it.
	w.Header().Set("X-RateLimit-Limit", strconv.Itoa(*vkMeta.RateLimitRpm))
	allowed, retryAfter := h.deps.RateLimiter.Allow(vkMeta.ID, *vkMeta.RateLimitRpm, 60_000)
	if !allowed {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		return fmt.Errorf("rate limit exceeded")
	}
	return nil
}

// compareEndpointRateLimitDefault is the per-VK fallback when
// CompareEndpointRateLimitRpm is NULL.
const compareEndpointRateLimitDefault = 30

func (h *Handler) checkCompareRateLimit(w http.ResponseWriter, vkMeta *vkauth.VKMeta) error {
	if h.deps.RateLimiter == nil {
		return nil
	}
	limit := compareEndpointRateLimitDefault
	if vkMeta.CompareEndpointRateLimitRpm != nil {
		limit = *vkMeta.CompareEndpointRateLimitRpm
	}
	if limit <= 0 {
		return nil
	}
	// Stamped before the verdict, like the per-VK RPM bucket next door. This is
	// a SEPARATE ceiling with its own value, and answering Retry-After alone
	// told a caller when to come back without ever saying what the limit was.
	w.Header().Set("X-RateLimit-Limit", strconv.Itoa(limit))
	allowed, retryAfter := h.deps.RateLimiter.Allow(vkMeta.ID+":compare", limit, 60_000)
	if !allowed {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		return fmt.Errorf("compare-endpoint rate limit exceeded")
	}
	return nil
}

// buildRequestContext constructs the L3 request context. It performs
// exactly one normcore.Registry.Normalize call per request (skipped for
// empty bodies) and packages the canonical NormalizedPayload alongside
// identity, endpoint, headers, and raw body into an immutable
// *requestcontext.RequestContext. Downstream L4 consumers (routing,
// hooks, audit) read from this single artefact instead of re-parsing
// raw bytes.
//
// Normalize errors are swallowed: the canonical payload remains nil and
// routing/hooks fall back to their nil-Request behaviour. A malformed
// or unrecognised body must not block the request — the routing layer
// makes its own non-smart fallback.
// requestNormalizeMeta builds the request-direction normalize Meta.
// Aligned with the audit-path Meta (auditbridge.BuildAuditFn): lowercased
// AdapterType + stripped ContentType, so the registry selects the
// identical normalizer everywhere the request body is normalized —
// admission (buildRequestContext) and the post-rewrite cache canonical

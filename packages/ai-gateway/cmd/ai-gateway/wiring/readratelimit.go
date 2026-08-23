// readratelimit.go — per-VK rate limiting for authenticated read-only routes.
package wiring

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/auth/vkauth"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/ingress/envelope"
)

// readVKAuthenticator authenticates a virtual key from an HTTP request. Narrow
// seam over *vkauth.Authenticator so the rate-limit wrapper is unit-testable
// without a real DB-backed authenticator.
type readVKAuthenticator interface {
	Authenticate(ctx context.Context, r *http.Request) (*vkauth.VKMeta, error)
}

// readRateLimiter applies a per-key sliding-window limit. Narrow seam over
// *ratelimit.Limiter (Allow has the same signature).
type readRateLimiter interface {
	Allow(key string, limit int, windowMs int64) (bool, int)
}

// vkReadRateLimit wraps an authenticated read-only handler (/v1/models,
// /v1/usage*) with the same per-VK RPM limiter the data-plane ServeProxy path
// enforces. The data plane throttled writes but these DB-backed GET
// endpoints had no per-key cap, so a single valid key could drive unbounded
// catalog/usage queries.
//
// The wrapper authenticates the VK, applies the limiter keyed on the VK id, and
// returns 429 with Retry-After, X-RateLimit-Limit and the gateway error
// envelope on exceed — the same three the data-plane path gives. On any auth failure it falls through
// to the wrapped handler unchanged — the inner handler runs its own requireVK
// and emits the canonical 401, so this wrapper never duplicates the 401 shape.
// VK auth is cache-backed (VKTTL), so the second Authenticate inside the inner
// handler is a cheap in-memory hit, not a second DB round-trip.
//
// A nil limiter (cache-only / limiter-disabled boot) or a VK with no RateLimitRpm
// configured means "no throttle" — identical to the ServeProxy contract in
// proxy_routing.go checkRateLimit.
func vkReadRateLimit(
	vkAuth readVKAuthenticator,
	limiter readRateLimiter,
	logger *slog.Logger,
) func(http.HandlerFunc) http.HandlerFunc {
	_ = logger
	return func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			// Only throttle when both a limiter and an authenticator are wired.
			if limiter == nil || vkAuth == nil {
				next(w, r)
				return
			}
			vkMeta, err := vkAuth.Authenticate(r.Context(), r)
			if err != nil || vkMeta == nil || vkMeta.RateLimitRpm == nil {
				// Unauthenticated or no per-VK cap: defer to the inner handler,
				// which authenticates and emits the canonical 401 when needed.
				next(w, r)
				return
			}
			allowed, retryAfter := limiter.Allow(vkMeta.ID, *vkMeta.RateLimitRpm, 60_000)
			if !allowed {
				w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
				w.Header().Set("X-RateLimit-Limit", strconv.Itoa(*vkMeta.RateLimitRpm))
				// The same envelope and the same code the data-plane path
				// emits. http.Error answered text/plain here, so a client
				// backing off on the gateway's rate limit had to parse two
				// different things depending on which route it hit — and on
				// this one there was nothing to parse.
				envelope.WriteGatewayError(w, r, http.StatusTooManyRequests, "RATE_LIMITED",
					"rate limit exceeded",
					"Reduce request frequency or contact admin to increase limits")
				return
			}
			w.Header().Set("X-RateLimit-Limit", strconv.Itoa(*vkMeta.RateLimitRpm))
			next(w, r)
		}
	}
}

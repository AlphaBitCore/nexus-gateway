// Package auth provides bearer token authentication middleware for the
// compliance-proxy runtime API.
package auth

import (
	"crypto/subtle"
	"net/http"
	"strings"

	cphttperr "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/httperr"
)

// TokenAuth provides bearer token authentication middleware. The token is
// supplied at construction time (sourced from COMPLIANCE_PROXY_API_TOKEN via
// config) and gates every authenticated endpoint, including the break-glass
// PUT /runtime/config/{key} surface.
//
// The middleware is FAIL-CLOSED: an empty token does NOT
// disable auth. config.validate() makes COMPLIANCE_PROXY_API_TOKEN boot-
// required so the proxy refuses to start without it; should an empty token
// ever reach this layer, Require returns 503 rather than passing the request
// through. This matches every other token gate in the platform (ai-gateway
// internalAuth / rstokenauth / runtimeintrospect all fail closed on an empty
// secret) and closes the prior fail-OPEN hole where an unset token left the
// mutating break-glass verb reachable with no credential.
type TokenAuth struct {
	apiToken []byte
}

// NewTokenAuth creates auth middleware from the configured token. The token is
// boot-required (config.validate enforces non-empty), so a non-empty value is
// the only valid production state; an empty value is rejected at request time
// (503) rather than silently opening the surface.
func NewTokenAuth(token string) *TokenAuth {
	return &TokenAuth{apiToken: []byte(token)}
}

// extractBearer extracts the bearer token from the Authorization header.
// Returns empty string if the header is missing or malformed.
func extractBearer(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return ""
	}
	const prefix = "Bearer "
	if len(auth) < len(prefix) || !strings.EqualFold(auth[:len(prefix)], prefix) {
		return ""
	}
	return auth[len(prefix):]
}

// Require returns middleware that requires a valid bearer token. Gates both
// read-only GETs and the break-glass PUT — the elevated tier was retired;
// operators use the same COMPLIANCE_PROXY_API_TOKEN for break-glass.
//
//   - Unconfigured token (empty)          -> 503 Service Unavailable (fail closed)
//   - Missing / malformed Authorization   -> 401 Unauthorized
//   - Wrong token                         -> 401 Unauthorized
func (a *TokenAuth) Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(a.apiToken) == 0 {
			// Fail closed: an unset runtime API token must never leave the
			// mutating break-glass surface open. Boot validation should have
			// prevented this state; this is defence-in-depth.
			cphttperr.WriteError(w, http.StatusServiceUnavailable, "runtime API authentication is not configured", "service_unavailable", "AUTH_NOT_CONFIGURED")
			return
		}

		token := extractBearer(r)
		if token == "" {
			cphttperr.WriteError(w, http.StatusUnauthorized, "missing or malformed Authorization header", "auth_error", "MISSING_TOKEN")
			return
		}

		if subtle.ConstantTimeCompare([]byte(token), a.apiToken) != 1 {
			cphttperr.WriteError(w, http.StatusUnauthorized, "invalid token", "auth_error", "INVALID_TOKEN")
			return
		}

		next.ServeHTTP(w, r)
	})
}

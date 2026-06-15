// internalauth.go — service-token guard for the /internal/* operator endpoints.
package wiring

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// internalAuth gates the /internal/* operator endpoints on the shared
// internal-service bearer token (env INTERNAL_SERVICE_TOKEN — the same
// [MUST MATCH] secret the Control Plane and Hub already hold). These are
// service-to-service admin surfaces (provider-test, routing-simulate,
// credential probe, hooks-test, embedding-probe, semantic-prewarm), NOT
// /v1/* data-plane routes — so they authenticate with the service token,
// never a virtual key. Mirrors internal/runtimeapi/auth.go (Bearer +
// constant-time compare) with an added fail-closed empty-token branch.
type internalAuth struct {
	token string
}

// newInternalAuth captures the configured internal-service token. The token
// is required at boot (config.validate), so in production it is never empty;
// the guard still defends against an empty token at request time.
func newInternalAuth(token string) *internalAuth {
	return &internalAuth{token: token}
}

// require wraps an operator handler, rejecting any request that does not
// present `Authorization: Bearer <token>`. Semantics:
//   - empty configured token  -> 503 (fail-closed; mirrors
//     shared/identity/rstokenauth — an unconfigured secret is a wiring
//     error, not a license to match every "" attempt).
//   - missing/malformed bearer -> 401.
//   - wrong token              -> 401 (constant-time compare).
func (a *internalAuth) require(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.token == "" {
			http.Error(w, "internal service token not configured", http.StatusServiceUnavailable)
			return
		}
		tok := internalBearerFromHeader(r.Header.Get("Authorization"))
		if tok == "" || subtle.ConstantTimeCompare([]byte(tok), []byte(a.token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// internalBearerFromHeader extracts the token from an `Authorization: Bearer <token>`
// header, returning "" when the prefix is absent.
func internalBearerFromHeader(h string) string {
	const pfx = "Bearer "
	if !strings.HasPrefix(h, pfx) {
		return ""
	}
	return strings.TrimSpace(h[len(pfx):])
}

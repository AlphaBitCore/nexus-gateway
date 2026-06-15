package runtimeapi

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// auth enforces bearer-token authentication for the AI Gateway runtimeapi.
// A single token is recognized — the InternalServiceToken injected through
// newAuth at server construction. There is no elevated tier because the
// runtimeapi exposes read-only GETs.
//
// The middleware is FAIL-CLOSED: an empty configured token does NOT
// silently turn into a default-401 surface that could be mistaken for an auth
// failure rather than a misconfiguration. When apiToken is empty (the runtime
// API token was never provisioned) every request gets 503 Service Unavailable,
// matching compliance-proxy runtime/auth and the other platform token gates.
type auth struct {
	apiToken string
}

func newAuth(apiToken string) *auth {
	return &auth{apiToken: apiToken}
}

// require returns an http.HandlerFunc that rejects requests without a valid bearer.
//
//   - Unconfigured token (empty)          -> 503 Service Unavailable (fail closed)
//   - Missing / malformed / wrong bearer  -> 401 Unauthorized
func (a *auth) require(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.apiToken == "" {
			// Fail closed: the runtime API token was never provisioned, so
			// there is no credential any caller could present. Surface this as
			// a 503 (service not configured) rather than a 401 (bad credential)
			// so operators can distinguish misconfiguration from auth failure.
			http.Error(w, `{"error":"runtime API authentication is not configured"}`, http.StatusServiceUnavailable)
			return
		}
		tok := bearerFromHeader(r.Header.Get("Authorization"))
		if tok == "" || !constTimeEq(tok, a.apiToken) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func bearerFromHeader(h string) string {
	const pfx = "Bearer "
	if !strings.HasPrefix(h, pfx) {
		return ""
	}
	return strings.TrimSpace(h[len(pfx):])
}

func constTimeEq(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// Package auth provides bearer token authentication middleware for the
// compliance-proxy runtime API.
package auth

import (
	"crypto/subtle"
	"log/slog"
	"net/http"
	"os"
	"strings"
)

// TokenAuth provides bearer token authentication middleware.
// The token is loaded from COMPLIANCE_PROXY_API_TOKEN at construction time
// and gates every authenticated endpoint, including the break-glass
// PUT /runtime/config/{key} surface. If the env var is empty, auth is
// disabled (dev mode) and a warning is logged.
type TokenAuth struct {
	apiToken []byte
	disabled bool
	logger   *slog.Logger
}

// NewTokenAuth creates auth middleware from environment.
// If COMPLIANCE_PROXY_API_TOKEN is empty, auth is disabled (dev mode).
func NewTokenAuth(logger *slog.Logger) *TokenAuth {
	tok := os.Getenv("COMPLIANCE_PROXY_API_TOKEN")

	a := &TokenAuth{
		apiToken: []byte(tok),
		logger:   logger,
	}

	if tok == "" {
		a.disabled = true
		logger.Warn("runtime API auth disabled: COMPLIANCE_PROXY_API_TOKEN not set (dev mode)")
	}

	return a
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

// Require returns middleware that requires a valid bearer token.
// Returns 401 for missing or invalid tokens. Gates both read-only GETs
// and the break-glass PUT — the elevated tier was retired; operators use
// the same COMPLIANCE_PROXY_API_TOKEN for break-glass.
func (a *TokenAuth) Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.disabled {
			next.ServeHTTP(w, r)
			return
		}

		token := extractBearer(r)
		if token == "" {
			http.Error(w, `{"error":"missing or malformed Authorization header"}`, http.StatusUnauthorized)
			return
		}

		tokenBytes := []byte(token)
		if len(a.apiToken) == 0 || subtle.ConstantTimeCompare(tokenBytes, a.apiToken) != 1 {
			http.Error(w, `{"error":"invalid token"}`, http.StatusUnauthorized)
			return
		}

		next.ServeHTTP(w, r)
	})
}

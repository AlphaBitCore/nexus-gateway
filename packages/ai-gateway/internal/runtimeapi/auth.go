package runtimeapi

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// auth enforces bearer-token authentication for the AI Gateway runtimeapi.
// A single token is recognized — provisioned via env AI_GATEWAY_API_TOKEN
// and injected through newAuth at server construction. There is no elevated
// tier because the runtimeapi exposes read-only GETs.
type auth struct {
	apiToken string
}

func newAuth(apiToken string) *auth {
	return &auth{apiToken: apiToken}
}

// require returns an http.HandlerFunc that rejects requests without a valid bearer.
func (a *auth) require(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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

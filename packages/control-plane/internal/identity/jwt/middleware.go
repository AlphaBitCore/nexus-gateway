package jwtverifier

import (
	"errors"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"
)

// ClaimsContextKey is the echo.Context key under which Middleware stashes
// the verified *Claims. Use ClaimsFrom to read it type-safely.
const ClaimsContextKey = "jwtverifier.claims"

// Middleware returns an echo.MiddlewareFunc that requires a Bearer access
// token on every request. On success the verified *Claims is attached to
// the Echo context under ClaimsContextKey. On failure the response carries
// an RFC 6750 WWW-Authenticate challenge and HTTP 401.
//
// The error_description emitted in the challenge is drawn from a fixed
// allow-list (see descForError); raw err.Error() is never echoed back, so
// package-internal prefixes and future error values cannot leak into or
// inject characters through the response header.
func (v *Verifier) Middleware() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			raw, ok := bearerToken(c.Request().Header.Get("Authorization"))
			if !ok {
				c.Response().Header().Set("WWW-Authenticate", `Bearer error="invalid_request"`)
				return echo.NewHTTPError(http.StatusUnauthorized, "missing bearer token")
			}
			claims, err := v.Verify(c.Request().Context(), raw)
			if err != nil {
				desc := descForError(err)
				if desc == "" {
					c.Response().Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
				} else {
					c.Response().Header().Set("WWW-Authenticate", `Bearer error="invalid_token", error_description="`+desc+`"`)
				}
				return echo.NewHTTPError(http.StatusUnauthorized, desc)
			}
			c.Set(ClaimsContextKey, claims)
			return next(c)
		}
	}
}

// ClaimsFrom returns the *Claims previously attached by Middleware, or nil
// if the caller is invoked on a route that has no Middleware in front of it.
func ClaimsFrom(c echo.Context) *Claims {
	v, _ := c.Get(ClaimsContextKey).(*Claims)
	return v
}

// bearerToken extracts the raw token from an Authorization header, rejecting
// missing / non-Bearer / empty-token cases with (_, false).
func bearerToken(authz string) (string, bool) {
	const prefix = "Bearer "
	if !strings.HasPrefix(authz, prefix) {
		return "", false
	}
	tok := strings.TrimSpace(strings.TrimPrefix(authz, prefix))
	if tok == "" {
		return "", false
	}
	return tok, true
}

// descForError maps our error sentinels to short RFC-safe strings used as
// error_description in the WWW-Authenticate challenge. Returning "" means
// the caller emits no error_description (for unclassified errors).
func descForError(err error) string {
	switch {
	case errors.Is(err, ErrExpired):
		return "expired"
	case errors.Is(err, ErrNotYetValid):
		return "not_yet_valid"
	case errors.Is(err, ErrRevoked):
		return "revoked"
	case errors.Is(err, ErrWrongIssuer):
		return "wrong_issuer"
	case errors.Is(err, ErrWrongAudience):
		return "wrong_audience"
	case errors.Is(err, ErrMalformed):
		return "malformed"
	case errors.Is(err, ErrInvalidSignature):
		return "invalid_signature"
	case errors.Is(err, ErrJWKSUnavailable):
		return "jwks_unavailable"
	}
	return ""
}

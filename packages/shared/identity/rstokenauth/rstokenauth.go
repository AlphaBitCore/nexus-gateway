// Package rstokenauth provides a service-token authentication middleware
// for internal service-to-service endpoints. Tokens are compared via
// crypto/subtle.ConstantTimeCompare. Both an Echo middleware and a stdlib
// http.Handler wrapper are provided so services built on either routing
// model can share the same auth surface.
//
// The middleware is intended for internal service-to-service calls between
// trusted platform components (e.g. Nexus Hub -> Control Plane, or
// Control Plane -> AI Gateway /v1/ai-guard/classify); it is NOT a
// replacement for admin JWT / API-key / mTLS surfaces.
//
// Semantics (both Echo and stdlib variants):
//   - Empty secret -> 503 + RS_TOKEN_NOT_CONFIGURED. An unconfigured secret
//     is a wiring error; returning 503 keeps the route shut without matching
//     every "" token attempt as valid.
//   - Missing X-RS-Token header -> 401 + RS_TOKEN_REQUIRED.
//   - Mismatching X-RS-Token header -> 401 + RS_TOKEN_INVALID.
//   - Comparison uses crypto/subtle.ConstantTimeCompare to avoid exposing a
//     timing side channel on the shared secret.
package rstokenauth

import (
	"crypto/subtle"
	"github.com/goccy/go-json"
	"net/http"

	"github.com/labstack/echo/v4"
)

// Header is the HTTP header carrying the service token. Callers embedding
// this package in clients should reference this constant rather than the
// literal "X-RS-Token" string.
const Header = "X-RS-Token"

// errorEnvelope mirrors the Control Plane error envelope shape (message +
// error type + stable code) so clients keep a single JSON contract across
// CP-gated and AI-Gateway-gated internal endpoints.
type errorEnvelope struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

func newEnvelope(message, errType, code string) errorEnvelope {
	var e errorEnvelope
	e.Error.Message = message
	e.Error.Type = errType
	e.Error.Code = code
	return e
}

// Middleware returns an echo.MiddlewareFunc that gates a route on the
// X-RS-Token header. See the package comment for semantics.
func Middleware(secret string) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if secret == "" {
				return c.JSON(http.StatusServiceUnavailable, newEnvelope(
					"Internal service token not configured",
					"server_error",
					"RS_TOKEN_NOT_CONFIGURED",
				))
			}
			presented := c.Request().Header.Get(Header)
			if presented == "" {
				return c.JSON(http.StatusUnauthorized, newEnvelope(
					"unauthorized", "unauthorized", "RS_TOKEN_REQUIRED",
				))
			}
			if subtle.ConstantTimeCompare([]byte(presented), []byte(secret)) != 1 {
				return c.JSON(http.StatusUnauthorized, newEnvelope(
					"unauthorized", "unauthorized", "RS_TOKEN_INVALID",
				))
			}
			return next(c)
		}
	}
}

// MiddlewareHTTP returns a stdlib net/http handler wrapper with identical
// semantics to Middleware. It is intended for services that route via
// http.ServeMux (e.g. ai-gateway) rather than Echo.
func MiddlewareHTTP(secret string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if secret == "" {
				writeJSON(w, http.StatusServiceUnavailable, newEnvelope(
					"Internal service token not configured",
					"server_error",
					"RS_TOKEN_NOT_CONFIGURED",
				))
				return
			}
			presented := r.Header.Get(Header)
			if presented == "" {
				writeJSON(w, http.StatusUnauthorized, newEnvelope(
					"unauthorized", "unauthorized", "RS_TOKEN_REQUIRED",
				))
				return
			}
			if subtle.ConstantTimeCompare([]byte(presented), []byte(secret)) != 1 {
				writeJSON(w, http.StatusUnauthorized, newEnvelope(
					"unauthorized", "unauthorized", "RS_TOKEN_INVALID",
				))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

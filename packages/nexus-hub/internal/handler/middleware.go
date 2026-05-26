package handler

import (
	"crypto/subtle"
	"strings"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	nexushttp "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/http"
)

// ServiceAuth returns middleware that validates INTERNAL_SERVICE_TOKEN.
func ServiceAuth(token string) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			auth := c.Request().Header.Get("Authorization")
			if auth == "" {
				return unauthorized(c, "missing authorization header")
			}
			t := strings.TrimPrefix(auth, "Bearer ")
			if subtle.ConstantTimeCompare([]byte(t), []byte(token)) != 1 {
				return c.JSON(403, ErrorResponse{Error: "invalid service token", Code: "FORBIDDEN"})
			}
			return next(c)
		}
	}
}

const nexusRequestIDHeader = "X-Nexus-Request-Id"

// NexusRequestID returns middleware that sets the X-Nexus-Request-Id header on
// every response and seeds the request ID into the request context so any
// outbound nexushttp.Client created downstream propagates it forward. If the
// inbound request already carries the header, it is reused; otherwise a new
// UUID is generated.
func NexusRequestID() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			id := c.Request().Header.Get(nexusRequestIDHeader)
			if id == "" {
				id = uuid.New().String()
			}
			c.Response().Header().Set(nexusRequestIDHeader, id)
			c.Set("nexusRequestId", id)
			c.SetRequest(c.Request().WithContext(nexushttp.WithRequestID(c.Request().Context(), id)))
			return next(c)
		}
	}
}

// NexusRequestIDFromContext extracts the nexus request ID from an Echo context.
func NexusRequestIDFromContext(c echo.Context) string {
	if v := c.Get("nexusRequestId"); v != nil {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

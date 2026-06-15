package handler

import (
	"crypto/subtle"
	"strings"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	nexushttp "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/http"
	nexushttperr "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/httperr"
)

// ServiceAuth returns middleware that constant-time-compares the request's
// Bearer token against the supplied token. It is parameterized by edge:
// /api/internal/things + WS register pass INTERNAL_SERVICE_TOKEN;
// /api/hub + /api/v1/admin/alerts pass the dedicated HUB_CONFIG_TOKEN. The
// caller must ensure the configured token is non-empty (an empty token would
// accept an empty bearer) — both config loaders require theirs at boot.
func ServiceAuth(token string) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			auth := c.Request().Header.Get("Authorization")
			if auth == "" {
				return unauthorized(c, "missing authorization header")
			}
			t := strings.TrimPrefix(auth, "Bearer ")
			if subtle.ConstantTimeCompare([]byte(t), []byte(token)) != 1 {
				return c.JSON(403, nexushttperr.ErrJSON("invalid service token", "auth_error", "FORBIDDEN"))
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

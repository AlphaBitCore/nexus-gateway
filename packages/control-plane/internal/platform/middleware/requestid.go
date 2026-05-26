package middleware

import (
	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	nexushttp "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/http"
)

const nexusRequestIDHeader = "X-Nexus-Request-Id"

// NexusRequestID returns middleware that sets the X-Nexus-Request-Id header
// on every response. If the request already carries the header, it is kept;
// otherwise a new UUID is generated.
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
		return v.(string)
	}
	return ""
}

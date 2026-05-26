package wiring

import (
	"log/slog"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
)

// InitMiddleware registers the global Echo middleware stack:
// recovery, request-id, access log, and request metrics.
// This must be called before any routes are mounted.
func InitMiddleware(e *echo.Echo, logger *slog.Logger) {
	e.Use(middleware.Recovery(logger))
	e.Use(middleware.NexusRequestID())
	e.Use(middleware.AccessLog(logger))
	e.Use(middleware.RequestMetrics())
}

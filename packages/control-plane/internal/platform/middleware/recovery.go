package middleware

import (
	"fmt"
	"log/slog"
	"net/http"
	"runtime"

	"github.com/labstack/echo/v4"
)

// Recovery returns Echo middleware that recovers from panics and logs them
// through slog rather than Echo's internal logger.
func Recovery(logger *slog.Logger) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			defer func() {
				if rec := recover(); rec != nil {
					// Capture a short stack trace for debugging.
					buf := make([]byte, 2048)
					n := runtime.Stack(buf, false)

					logger.Error("panic recovered",
						slog.Any("panic", rec),
						slog.String("method", c.Request().Method),
						slog.String("path", c.Request().URL.Path),
						slog.String("requestId", NexusRequestIDFromContext(c)),
						slog.String("stack", string(buf[:n])),
					)

					_ = c.JSON(http.StatusInternalServerError, map[string]string{
						"error": fmt.Sprintf("internal server error: %v", rec),
					})
				}
			}()
			return next(c)
		}
	}
}

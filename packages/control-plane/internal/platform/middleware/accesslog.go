package middleware

import (
	"log/slog"
	"time"

	"github.com/labstack/echo/v4"
)

// AccessLog returns Echo middleware that logs every HTTP request via slog.
// Logged fields: method, path, status, duration, requestId, remoteAddr, userAgent.
// Health/metrics endpoints are logged at Debug level to reduce noise.
func AccessLog(logger *slog.Logger) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			start := time.Now()
			err := next(c)

			req := c.Request()
			res := c.Response()
			path := req.URL.Path
			status := res.Status
			duration := time.Since(start)
			requestID := NexusRequestIDFromContext(c)

			attrs := []slog.Attr{
				slog.String("method", req.Method),
				slog.String("path", path),
				slog.String("query", req.URL.RawQuery),
				slog.Int("status", status),
				slog.Duration("duration", duration),
				slog.String("requestId", requestID),
				slog.String("remoteAddr", c.RealIP()),
			}

			// Reduce noise: health/metrics probes go to Debug.
			if path == "/healthz" || path == "/metrics" {
				logger.LogAttrs(req.Context(), slog.LevelDebug, "http request", attrs...)
				return err
			}

			// Pick level by status code.
			level := slog.LevelInfo
			if status >= 500 {
				level = slog.LevelError
			} else if status >= 400 {
				level = slog.LevelWarn
			}

			logger.LogAttrs(req.Context(), level, "http request", attrs...)
			return err
		}
	}
}

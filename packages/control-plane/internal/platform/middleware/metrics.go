package middleware

import (
	"time"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/metrics"
)

// RequestMetrics returns Echo middleware that records per-request opsmetrics
// counters and latency histograms. The route_class label uses the registered
// Echo route template (c.Path()) rather than the actual URL to keep
// cardinality low — concrete IDs in the path collapse onto the parameter
// name (e.g. /admin/users/u_abc123 → /admin/users/:id).
//
// status_class buckets the HTTP status code into 1xx/2xx/3xx/4xx/5xx so a
// stream of 200/201/204 responses doesn't fan out to three separate label
// pairs.
func RequestMetrics() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			start := time.Now()
			err := next(c)
			duration := time.Since(start)

			method := c.Request().Method
			routeClass := c.Path()
			if routeClass == "" {
				routeClass = "unknown"
			}
			statusClass := statusBucket(c.Response().Status)

			if metrics.RequestsTotal != nil {
				metrics.RequestsTotal.With(method, routeClass, statusClass).Inc()
			}
			if metrics.RequestDurationMs != nil {
				metrics.RequestDurationMs.With(routeClass).Observe(float64(duration.Milliseconds()))
			}

			return err
		}
	}
}

// statusBucket reduces an HTTP status code to its class string ("1xx".."5xx",
// or "unknown" for the zero / negative codes Echo emits when a handler fails
// before writing the response).
func statusBucket(status int) string {
	switch {
	case status >= 100 && status < 200:
		return "1xx"
	case status >= 200 && status < 300:
		return "2xx"
	case status >= 300 && status < 400:
		return "3xx"
	case status >= 400 && status < 500:
		return "4xx"
	case status >= 500 && status < 600:
		return "5xx"
	default:
		return "unknown"
	}
}

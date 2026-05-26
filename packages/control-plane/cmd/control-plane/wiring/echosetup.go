package wiring

import (
	"log/slog"
	"net/http"

	"github.com/labstack/echo/v4"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// InitEcho creates and returns a configured Echo instance with the global
// middleware stack, /healthz, and /metrics endpoints already mounted.
func InitEcho(logger *slog.Logger) *echo.Echo {
	e := echo.New()
	e.HideBanner = true
	e.HidePort = true
	InitMiddleware(e, logger)
	e.GET("/healthz", func(c echo.Context) error {
		return c.JSON(http.StatusOK, map[string]string{"status": "ok"})
	})
	e.GET("/metrics", echo.WrapHandler(promhttp.Handler()))
	return e
}

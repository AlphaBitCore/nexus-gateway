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

	// Decide explicitly whose X-Forwarded-For we believe.
	//
	// Echo's default RealIP() honours X-Forwarded-For and X-Real-IP from ANY
	// peer, and `sourceIP(c)` is that value — stamped onto audit rows and onto
	// the Hub config_change_event that records "who toggled this and from
	// where". Left at the default, any caller could set
	// `X-Forwarded-For: 10.0.0.1` and choose what the audit trail says about
	// them, which makes the field worse than absent: it is forgeable evidence
	// that reads as fact.
	//
	// ExtractIPFromXFFHeader with no options trusts the header only when the
	// immediate peer is loopback or a private range — the reverse proxy or
	// ingress this service sits behind — and otherwise falls back to
	// RemoteAddr. A client on the public internet therefore cannot influence
	// it, while a genuine proxy hop still resolves the real client.
	e.IPExtractor = echo.ExtractIPFromXFFHeader()
	InitMiddleware(e, logger)
	e.GET("/healthz", func(c echo.Context) error {
		return c.JSON(http.StatusOK, map[string]string{"status": "ok"})
	})
	e.GET("/metrics", echo.WrapHandler(promhttp.Handler()))
	return e
}

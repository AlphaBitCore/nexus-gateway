// Package aigwsim owns the Control Plane admin API for the
// AI-gateway request simulator (admin UI tooling).
package aigwsim

import (
	"log/slog"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/httperr"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/peer"
)

type Deps struct {
	Logger *slog.Logger
	// GatewayBase resolves the AI Gateway base URL from the Hub at request
	// time (never configured locally). The simulator always forwards there —
	// the caller can never choose the upstream host (SSRF posture).
	GatewayBase peer.URLProvider
}

type Handler struct {
	logger      *slog.Logger
	gatewayBase peer.URLProvider
}

func New(d Deps) *Handler {
	logger := d.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{logger: logger, gatewayBase: d.GatewayBase}
}

// errJSON is the canonical admin error envelope helper (see internal/platform/httperr).
var errJSON = httperr.ErrJSON

func internalServerError(c echo.Context, msg string) error {
	return c.JSON(http.StatusInternalServerError, errJSON(msg, "server_error", ""))
}

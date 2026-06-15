// Package aigwsim owns the Control Plane admin API for the
// AI-gateway request simulator (admin UI tooling).
package aigwsim

import (
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/httperr"
	"log/slog"
	"net/http"

	"github.com/labstack/echo/v4"
)

type Deps struct {
	Logger *slog.Logger
}

type Handler struct {
	logger *slog.Logger
}

func New(d Deps) *Handler {
	logger := d.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{logger: logger}
}

// errJSON is the canonical admin error envelope helper (see internal/platform/httperr).
var errJSON = httperr.ErrJSON

func internalServerError(c echo.Context, msg string) error {
	return c.JSON(http.StatusInternalServerError, errJSON(msg, "server_error", ""))
}

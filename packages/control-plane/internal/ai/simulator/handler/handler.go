// Package aigwsim owns the Control Plane admin API for the
// AI-gateway request simulator (admin UI tooling).
package aigwsim

import (
	"log/slog"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
)

type Deps struct {
	Audit  *audit.Writer
	Logger *slog.Logger
}

type Handler struct {
	audit  *audit.Writer
	logger *slog.Logger
}

func New(d Deps) *Handler {
	logger := d.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{audit: d.Audit, logger: logger}
}

func errJSON(message, errType, code string) map[string]any {
	return map[string]any{
		"error": map[string]any{"message": message, "type": errType, "code": code},
	}
}

func internalServerError(c echo.Context, msg string) error {
	return c.JSON(http.StatusInternalServerError, errJSON(msg, "server_error", ""))
}

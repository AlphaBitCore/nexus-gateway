// Package observability owns the CP admin API for observability
// retention config (per-layer retention settings).
package observability

import (
	"context"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/httperr"
	"log/slog"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/observability/opsmetrics/opsstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/hub"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
)

type HubAPI interface {
	NotifyConfigChange(ctx context.Context, req hub.ConfigChangeRequest) (*hub.ConfigChangeResponse, error)
	InvalidateConfig(ctx context.Context, thingType, configKey string)
}

type Deps struct {
	Pool   opsstore.PgxPool
	Hub    HubAPI
	Audit  *audit.Writer
	Logger *slog.Logger
}

type Handler struct {
	ops    *opsstore.Store
	hub    HubAPI
	audit  *audit.Writer
	logger *slog.Logger
}

func New(d Deps) *Handler {
	logger := d.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{
		ops:    opsstore.New(d.Pool),
		hub:    d.Hub,
		audit:  d.Audit,
		logger: logger,
	}
}

// errJSON is the canonical admin error envelope helper (see internal/platform/httperr).
var errJSON = httperr.ErrJSON

type Actor struct{ UserID, Name string }

func actorFromContext(c echo.Context) Actor {
	aa := middleware.AdminAuthFromContext(c)
	if aa == nil {
		return Actor{}
	}
	return Actor{UserID: aa.KeyID, Name: aa.KeyName}
}

func sourceIP(c echo.Context) string { return c.RealIP() }

func internalServerError(c echo.Context, msg string) error {
	return c.JSON(http.StatusInternalServerError, errJSON(msg, "server_error", ""))
}

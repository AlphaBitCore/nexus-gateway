// Package siem owns the CP admin API for SIEM settings.
package siem

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/hub"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/store/systemmetastore"
)

type HubAPI interface {
	NotifyConfigChange(ctx context.Context, req hub.ConfigChangeRequest) (*hub.ConfigChangeResponse, error)
	InvalidateConfig(ctx context.Context, thingType, configKey string)
}

type Deps struct {
	Meta   *systemmetastore.Store
	Hub    HubAPI
	Audit  *audit.Writer
	Logger *slog.Logger
}

type Handler struct {
	db     *systemmetastore.Store
	hub    HubAPI
	audit  *audit.Writer
	logger *slog.Logger
}

func New(d Deps) *Handler {
	logger := d.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{db: d.Meta, hub: d.Hub, audit: d.Audit, logger: logger}
}

func errJSON(message, errType, code string) map[string]any {
	return map[string]any{"error": map[string]any{"message": message, "type": errType, "code": code}}
}

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

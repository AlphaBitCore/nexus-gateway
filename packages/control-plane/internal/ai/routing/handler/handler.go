// Package routing owns the Control Plane admin API for routing rule
// CRUD + the routing-simulate proxy to ai-gateway. R6 seventh domain
// extracted from the flat handler/ package; recipe documented in
// docs/_archive/2026-q2/programs/r6-handler-decomp-runbook.md.
package routing

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/ai/routing/routingstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/store/systemmetastore"
)

// HubInvalidator is the narrow Hub surface routing/ needs:
// fire-and-forget InvalidateConfig on every CUD path (ai-gateway
// reads routing rules from DB on every request — invalidation just
// wakes its short-TTL cache).
type HubInvalidator interface {
	InvalidateConfig(ctx context.Context, thingType, configKey string)
}

// ProxyConfig is the BFF proxy snapshot routing-simulate needs.
type ProxyConfig struct {
	AIGatewayURL string
}

// Deps is the construction-time arg shape.
type Deps struct {
	Pool   routingstore.PgxPool
	Meta   *systemmetastore.Store
	Hub    HubInvalidator
	Audit  *audit.Writer
	Logger *slog.Logger
	Proxy  ProxyConfig
}

// Handler is the per-domain admin handler for /api/admin/routing-rules*.
type Handler struct {
	meta    *systemmetastore.Store // for GetSystemMetadata/SetSystemMetadata in incrementConfigVersion
	routing *routingstore.Store    // routing rule CRUD
	hub     HubInvalidator
	audit   *audit.Writer
	logger  *slog.Logger
	proxy   ProxyConfig
}

// New constructs a routing Handler from its narrow Deps.
func New(d Deps) *Handler {
	h := &Handler{meta: d.Meta, hub: d.Hub, audit: d.Audit, logger: d.Logger, proxy: d.Proxy}
	if d.Pool != nil {
		h.routing = routingstore.New(d.Pool)
	}
	return h
}

func errJSON(message, errType, code string) map[string]any {
	return map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    errType,
			"code":    code,
		},
	}
}

func internalServerError(c echo.Context, msg string) error {
	return c.JSON(http.StatusInternalServerError, errJSON(msg, "server_error", ""))
}

type actor struct {
	UserID string
	Name   string
}

func actorFromContext(c echo.Context) actor {
	aa := middleware.AdminAuthFromContext(c)
	if aa == nil {
		return actor{}
	}
	return actor{UserID: aa.KeyID, Name: aa.KeyName}
}

type pagination struct {
	Limit  int
	Offset int
}

func parsePagination(c echo.Context) pagination {
	limit := 50
	offset := 0
	if v := c.QueryParam("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
			if limit > 1000 {
				limit = 1000
			}
		}
	}
	if v := c.QueryParam("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			offset = n
		}
	}
	return pagination{Limit: limit, Offset: offset}
}

// incrementConfigVersion atomically increments the agent config
// version. Local copy of *AdminHandler.incrementConfigVersion.
func (h *Handler) incrementConfigVersion(ctx context.Context) {
	const key = "agent.config.version"
	version := 0
	raw, err := h.meta.GetSystemMetadata(ctx, key)
	if err == nil && raw != nil {
		var v int
		if json.Unmarshal(raw, &v) == nil {
			version = v
		}
	}
	version++
	if err := h.meta.SetSystemMetadata(ctx, key, version, "system"); err != nil {
		h.logger.Error("increment agent config version", "error", err)
	}
}

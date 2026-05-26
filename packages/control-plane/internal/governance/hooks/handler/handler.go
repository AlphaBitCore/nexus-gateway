// Package hooks owns the Control Plane admin API for hook config CRUD
// — list / get / create / update / delete / reorder / force-refresh.
// R6 third domain extracted from the flat handler/ package; recipe
// documented in docs/_archive/2026-q2/programs/r6-handler-decomp-runbook.md.
package hooks

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/governance/hooks/hookstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/store/systemmetastore"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configkey"
)

// HubInvalidator is the narrow Hub surface hooks/ needs: a fire-and-
// forget per-(thingType, configKey) invalidation. The parent
// handler.HubNotifier interface is wider; this local interface keeps
// hooks/ from depending on the full god-object — same pattern as
// alerts/.HubBaseURLToken.
type HubInvalidator interface {
	InvalidateConfig(ctx context.Context, thingType, configKey string)
}

// Deps is the construction-time arg shape.
type Deps struct {
	Pool       hookstore.PgxPool
	HooksStore *hookstore.Store // optional: if set, used instead of constructing from Pool
	Meta       *systemmetastore.Store
	Hub        HubInvalidator // may be nil — handlers tolerate it
	Audit      *audit.Writer
	Logger     *slog.Logger
}

// Handler is the per-domain admin handler for /api/admin/hooks*
// endpoints.
type Handler struct {
	hooks  *hookstore.Store
	meta   *systemmetastore.Store
	hub    HubInvalidator
	audit  *audit.Writer
	logger *slog.Logger
	proxy  ProxyConfig // set by RegisterHookExtrasRoutes
}

// New constructs a hooks Handler from its narrow Deps.
func New(d Deps) *Handler {
	hooks := d.HooksStore
	if hooks == nil && d.Pool != nil {
		hooks = hookstore.New(d.Pool)
	}
	return &Handler{
		hooks:  hooks,
		meta:   d.Meta,
		hub:    d.Hub,
		audit:  d.Audit,
		logger: d.Logger,
	}
}

// RegisterRoutes registers hook config CRUD routes.
func (h *Handler) RegisterRoutes(g *echo.Group, iamMW func(action string) echo.MiddlewareFunc) {
	g.GET("/hooks", h.ListHookConfigs, iamMW(iam.ResourceHook.Action(iam.VerbRead)))
	g.POST("/hooks", h.CreateHookConfig, iamMW(iam.ResourceHook.Action(iam.VerbCreate)))
	g.POST("/hooks/reorder", h.ReorderHooks, iamMW(iam.ResourceHook.Action(iam.VerbUpdate)))
	g.POST("/hooks/refresh", h.HookForceRefresh, iamMW(iam.ResourceHook.Action(iam.VerbUpdate)))
	g.GET("/hooks/:id", h.GetHookConfig, iamMW(iam.ResourceHook.Action(iam.VerbRead)))
	g.PUT("/hooks/:id", h.UpdateHookConfig, iamMW(iam.ResourceHook.Action(iam.VerbUpdate)))
	g.DELETE("/hooks/:id", h.DeleteHookConfig, iamMW(iam.ResourceHook.Action(iam.VerbDelete)))
}

// errJSON builds a canonical JSON error envelope used across admin
// handlers. Local copy of the helper in handler/helpers.go.
func errJSON(message, errType, code string) map[string]any {
	return map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    errType,
			"code":    code,
		},
	}
}

// pagination holds parsed limit/offset query params.
type pagination struct {
	Limit  int
	Offset int
}

// parsePagination extracts limit and offset from query parameters.
// Local copy of handler/helpers.go parsePagination.
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
// version stored in system_metadata so that agents can detect
// configuration changes. Errors are logged but not propagated.
// Local copy of *AdminHandler.incrementConfigVersion.
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

// invalidateHookConfigEverywhere fires the three-way Hub invalidation
// every CUD path needs (ai-gateway + compliance-proxy + agent). Local
// helper to avoid 3-line repeat across 5 callers.
func (h *Handler) invalidateHookConfigEverywhere(ctx context.Context) {
	if h.hub == nil {
		return
	}
	h.hub.InvalidateConfig(ctx, "ai-gateway", configkey.Hooks)
	h.hub.InvalidateConfig(ctx, "compliance-proxy", configkey.Hooks)
	h.hub.InvalidateConfig(ctx, "agent", configkey.Hooks)
}

// internalServerError is the canonical 500 used across this domain.
func internalServerError(c echo.Context, msg string) error {
	return c.JSON(http.StatusInternalServerError, errJSON(msg, "server_error", ""))
}

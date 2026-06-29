package infra

import (
	"net/http"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
	"github.com/labstack/echo/v4"
)

// RegisterNodeRuntimeRoutes wires the runtime introspection bridge route.
// The handler is a pass-through to Hub's
// GET /api/hub/things/:id/runtime — CP does not interpret the response,
// it delegates parsing to the UI's Runtime State tab.
func (h *Handler) RegisterNodeRuntimeRoutes(g *echo.Group, iamMW func(action string) echo.MiddlewareFunc) {
	g.GET("/nodes/:id/runtime", h.GetNodeRuntime, iamMW(iam.ResourceSettings.Action(iam.VerbRead)))
}

// GetNodeRuntime returns the runtime introspection snapshot for a node.
// The response shape is { "snapshot": {...service-side envelope}, "meta": {...Hub view} }.
func (h *Handler) GetNodeRuntime(c echo.Context) error {
	id := c.Param("id")
	if id == "" {
		return c.JSON(http.StatusBadRequest, errJSON("id is required", "validation_error", "VALIDATION_ERROR"))
	}
	if h.hub == nil {
		return c.JSON(http.StatusServiceUnavailable, errJSON("Hub is not configured", "hub_error", "HUB_UNAVAILABLE"))
	}
	body, status, err := h.hub.GetThingRuntime(c.Request().Context(), id)
	if err != nil {
		h.logger.Error("get node runtime", "error", err, "thing_id", id)
		return c.JSON(http.StatusBadGateway, errJSON("Hub call failed: "+err.Error(), "hub_error", "HUB_UNAVAILABLE"))
	}
	if status >= 500 {
		// Surface upstream 5xx as 502 — the request itself was well-formed.
		return c.Blob(http.StatusBadGateway, "application/json", body)
	}
	return c.Blob(status, "application/json", body)
}

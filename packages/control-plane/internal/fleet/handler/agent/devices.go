package agent

import (
	"errors"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/fleet/store/agentauditstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/fleet/store/agentstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/hub"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
)

// RegisterAdminAgentDeviceRoutes registers admin agent device routes.
//
// Per-device routes (`/agent-devices/:id/*`) use the device-aware
// middleware variant so policies with Resource scoped to
// `agent-device/group:<id>/*` enforce membership at request time.
// Fleet-wide list / create routes use the standard middleware — they
// don't target a specific device.
func (h *Handler) RegisterAdminAgentDeviceRoutes(
	g *echo.Group,
	iamMW func(action string) echo.MiddlewareFunc,
	iamMWDevice func(action, deviceIDParam string) echo.MiddlewareFunc,
) {
	g.GET("/agent-devices", h.ListAgentDevices, iamMW(iam.ResourceAgentDevice.Action(iam.VerbRead)))
	g.GET("/agent-devices/health", h.AgentFleetHealth, iamMW(iam.ResourceAgentDevice.Action(iam.VerbRead)))
	g.GET("/agent-devices/:id", h.GetAgentDevice, iamMWDevice(iam.ResourceAgentDevice.Action(iam.VerbRead), "id"))
	g.GET("/agent-devices/:id/events", h.ListDeviceEvents, iamMWDevice(iam.ResourceAgentDevice.Action(iam.VerbRead), "id"))
	// Assignment history — paginated DeviceAssignment rows for this device.
	// Read-only; same IAM scope as the device detail page that consumes it.
	g.GET("/agent-devices/:id/assignments", h.ListDeviceAssignments, iamMWDevice(iam.ResourceAgentDevice.Action(iam.VerbRead), "id"))
	g.POST("/agent-devices/enroll-token", h.GenerateEnrollToken, iamMW(iam.ResourceAgentDevice.Action(iam.VerbCreate)))
	g.POST("/agent-devices/:id/unenroll", h.UnenrollDevice, iamMWDevice(iam.ResourceAgentDevice.Action(iam.VerbDelete), "id"))
	// Force config refresh — gated on `force-resync` to mirror the Node
	// resource's verb. Admin presses "Force refresh" on the Device
	// detail page; CP forwards to Hub's existing per-thing resync path.
	g.POST("/agent-devices/:id/force-refresh", h.ForceRefreshAgentDevice, iamMWDevice(iam.ResourceAgentDevice.Action(iam.VerbForceResync), "id"))
	// Free-form tags. PUT replaces the full set; tags compose into
	// smart-group predicates (`tags_contains`) and UI filters.
	g.PUT("/agent-devices/:id/tags", h.PutDeviceTags, iamMWDevice(iam.ResourceAgentDevice.Action(iam.VerbUpdate), "id"))
}

func (h *Handler) ListAgentDevices(c echo.Context) error {
	pg := parsePagination(c)
	params := agentstore.ThingNodeListParams{
		Q:      c.QueryParam("q"),
		Status: c.QueryParam("status"),
		OS:     c.QueryParam("os"),
		Limit:  pg.Limit,
		Offset: pg.Offset,
	}

	devices, total, err := h.agents.ListThingNodes(c.Request().Context(), params)
	if err != nil {
		h.logger.Error("list agent devices", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", "INTERNAL_ERROR"))
	}
	return c.JSON(http.StatusOK, map[string]any{"data": devices, "total": total})
}

func (h *Handler) GetAgentDevice(c echo.Context) error {
	d, err := h.agents.GetThingNode(c.Request().Context(), c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", "INTERNAL_ERROR"))
	}
	if d == nil {
		return c.JSON(http.StatusNotFound, errJSON("Device not found", "not_found", "NOT_FOUND"))
	}
	return c.JSON(http.StatusOK, d)
}

func (h *Handler) ListDeviceEvents(c echo.Context) error {
	pg := parsePagination(c)
	events, total, err := h.agentAudit.ListAgentTrafficEvents(c.Request().Context(), agentauditstore.AgentTrafficEventListParams{
		DeviceID: c.Param("id"),
		Limit:    pg.Limit,
		Offset:   pg.Offset,
	})
	if err != nil {
		h.logger.Error("list device events", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", "INTERNAL_ERROR"))
	}
	return c.JSON(http.StatusOK, map[string]any{"data": events, "total": total})
}

func (h *Handler) GenerateEnrollToken(c echo.Context) error {
	aa := middleware.AdminAuthFromContext(c)
	if aa == nil {
		return c.JSON(http.StatusUnauthorized, errJSON("Authentication required", "authentication_error", ""))
	}

	var body struct {
		Hostname string `json:"hostname"`
	}
	_ = c.Bind(&body)

	label := body.Hostname
	if label == "" {
		label = "agent"
	}

	if h.hub == nil {
		h.logger.Error("hub enrollment client not configured")
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", "INTERNAL_ERROR"))
	}

	tok, err := h.hub.CreateEnrollmentToken(c.Request().Context(), hub.CreateEnrollmentTokenRequest{
		ThingType: "agent",
		Label:     label,
		CreatedBy: aa.KeyID,
	})
	if errors.Is(err, hub.ErrNotConfigured) {
		return c.JSON(http.StatusServiceUnavailable, errJSON("Nexus Hub is not configured", "service_unavailable", "HUB_NOT_CONFIGURED"))
	}
	if err != nil {
		h.logger.Error("generate enrollment token via hub", "error", err)
		return c.JSON(http.StatusBadGateway, errJSON("Nexus Hub enrollment failed", "bad_gateway", "HUB_ERROR"))
	}

	ae := audit.EntryFor(c, iam.ResourceNode, iam.VerbCreate)
	ae.EntityID = "token"
	ae.AfterState = map[string]any{"hostname": body.Hostname, "expiresAt": tok.ExpiresAt}
	h.audit.LogObserved(c.Request().Context(), ae)

	return c.JSON(http.StatusCreated, map[string]any{"token": tok.Token, "expiresAt": tok.ExpiresAt})
}

func (h *Handler) UnenrollDevice(c echo.Context) error {
	id := c.Param("id")
	existing, err := h.agents.GetThingNode(c.Request().Context(), id)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", "INTERNAL_ERROR"))
	}
	if existing == nil {
		return c.JSON(http.StatusNotFound, errJSON("Device not found", "not_found", "NOT_FOUND"))
	}

	updated, err := h.agents.UpdateThingNodeStatus(c.Request().Context(), id, "revoked")
	if err != nil {
		h.logger.Error("unenroll device", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", "INTERNAL_ERROR"))
	}

	ae := audit.EntryFor(c, iam.ResourceAgentDevice, iam.VerbUpdate)
	ae.EntityID = id
	ae.BeforeState = map[string]any{"status": existing.Status}
	ae.AfterState = map[string]any{"status": "revoked"}
	h.audit.LogObserved(c.Request().Context(), ae)

	h.logger.Info("Device unenrolled", "deviceId", id)
	return c.JSON(http.StatusOK, updated)
}

// ListDeviceAssignments returns the paginated history of DeviceAssignment
// rows for this device — who owned it, when, and the source channel
// (enrollment / login / heartbeat / manual). Newest first.
func (h *Handler) ListDeviceAssignments(c echo.Context) error {
	id := c.Param("id")
	if id == "" {
		return c.JSON(http.StatusBadRequest, errJSON("device id is required", "validation_error", ""))
	}
	pg := parsePagination(c)
	rows, total, err := h.fleet.ListDeviceAssignmentsByDevice(c.Request().Context(), id, pg.Limit, pg.Offset)
	if err != nil {
		h.logger.Error("list device assignments", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", "INTERNAL_ERROR"))
	}
	return c.JSON(http.StatusOK, map[string]any{"data": rows, "total": total})
}

// ForceRefreshAgentDevice asks Hub to re-broadcast every desired config key
// to this device right now, without waiting for the next shadow tick.
// Useful when admin has changed a fleet-wide setting and wants the new
// value visible on the device immediately. Hub's `/things/:id/resync`
// endpoint already supports "all keys" semantics when configKey is
// omitted (RePushAllKeys); we forward an empty body.
func (h *Handler) ForceRefreshAgentDevice(c echo.Context) error {
	id := c.Param("id")
	if id == "" {
		return c.JSON(http.StatusBadRequest, errJSON("device id is required", "validation_error", ""))
	}

	// Sanity-check the device exists before pushing to Hub — surfaces a
	// proper 404 instead of leaking Hub's internal error.
	existing, err := h.agents.GetThingNode(c.Request().Context(), id)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", "INTERNAL_ERROR"))
	}
	if existing == nil {
		return c.JSON(http.StatusNotFound, errJSON("Device not found", "not_found", "NOT_FOUND"))
	}

	// Empty body → Hub re-pushes every desired key for this thing.
	resp, hubErr := h.hub.ForceResyncAll(c.Request().Context(), id)
	if errors.Is(hubErr, hub.ErrNotConfigured) {
		return c.JSON(http.StatusServiceUnavailable, errJSON("Nexus Hub is not configured", "service_unavailable", "HUB_NOT_CONFIGURED"))
	}
	if hubErr != nil {
		h.logger.Error("force-refresh hub call", "deviceId", id, "error", hubErr)
		return c.JSON(http.StatusBadGateway, errJSON("Nexus Hub force-refresh failed", "bad_gateway", "HUB_ERROR"))
	}

	ae := audit.EntryFor(c, iam.ResourceAgentDevice, iam.VerbForceResync)
	ae.EntityID = id
	ae.AfterState = map[string]any{"thingId": id, "result": "ok"}
	h.audit.LogObserved(c.Request().Context(), ae)

	return c.JSON(http.StatusOK, resp)
}

func (h *Handler) AgentFleetHealth(c echo.Context) error {
	health, err := h.agents.GetAgentFleetHealth(c.Request().Context())
	if err != nil {
		h.logger.Error("agent fleet health", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", "INTERNAL_ERROR"))
	}
	return c.JSON(http.StatusOK, health)
}

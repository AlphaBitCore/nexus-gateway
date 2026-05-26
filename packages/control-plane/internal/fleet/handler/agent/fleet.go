package agent

import (
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/ai/providers/providerstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/fleet/store/fleetstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/users/userstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
)

// RegisterFleetRoutes registers fleet management routes for agent users and devices.
func (h *Handler) RegisterFleetRoutes(g *echo.Group, iamMW func(action string) echo.MiddlewareFunc) {
	g.GET("/agent-users", h.ListAgentUsers, iamMW(iam.ResourceAgentDevice.Action(iam.VerbRead)))
	g.GET("/agent-users/:id", h.GetAgentUser, iamMW(iam.ResourceAgentDevice.Action(iam.VerbRead)))
	g.GET("/agent-users/:id/devices", h.ListAgentUserDevices, iamMW(iam.ResourceAgentDevice.Action(iam.VerbRead)))
	g.GET("/agent-users/:id/audit", h.ListAgentUserAudit, iamMW(iam.ResourceAgentDevice.Action(iam.VerbRead)))
	g.POST("/agent-users/:id/suspend", h.SuspendAgentUser, iamMW(iam.ResourceAgentDevice.Action(iam.VerbUpdate)))
	g.POST("/agent-users/:id/activate", h.ActivateAgentUser, iamMW(iam.ResourceAgentDevice.Action(iam.VerbUpdate)))

	g.GET("/agent-devices/:id/audit", h.ListDeviceAudit, iamMW(iam.ResourceAgentDevice.Action(iam.VerbRead)))
	g.GET("/agent-devices/:id/config", h.GetDeviceConfig, iamMW(iam.ResourceAgentDevice.Action(iam.VerbRead)))
	g.GET("/agent-devices/:id/timeline", h.GetDeviceTimeline, iamMW(iam.ResourceAgentDevice.Action(iam.VerbRead)))

	// Self-service: current admin user's own enrolled agent devices.
	// No IAM gate — the data is inherently scoped to the caller's
	// NexusUser.id via DeviceAssignment lookup, so even a viewer
	// without `agent-device:read` can see THEIR OWN install status on
	// the Agent Setup page (used by the live Verify panel). Matches
	// the pattern of /me/admin-audit-logs (admin_traffic.go).
	g.GET("/me/agent-devices", h.ListMyAgentDevices)
}

// ListAgentUsers returns agent users (NexusUser with canAccessControlPlane=false).
func (h *Handler) ListAgentUsers(c echo.Context) error {
	pg := parsePagination(c)
	canAccess := false
	params := userstore.NexusUserListParams{
		Q:                     c.QueryParam("q"),
		CanAccessControlPlane: &canAccess,
		Limit:                 pg.Limit,
		Offset:                pg.Offset,
	}
	if v := c.QueryParam("enabled"); v == "true" {
		t := true
		params.Enabled = &t
	} else if v == "false" {
		f := false
		params.Enabled = &f
	}

	users, total, err := h.users.ListNexusUsers(c.Request().Context(), params)
	if err != nil {
		h.logger.Error("list agent users", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", "INTERNAL_ERROR"))
	}
	return c.JSON(http.StatusOK, map[string]any{"data": users, "total": total})
}

// GetAgentUser returns a single agent user by ID.
func (h *Handler) GetAgentUser(c echo.Context) error {
	user, err := h.users.FindNexusUserByID(c.Request().Context(), c.Param("id"))
	if err != nil {
		h.logger.Error("get agent user", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", "INTERNAL_ERROR"))
	}
	if user == nil || user.CanAccessControlPlane {
		return c.JSON(http.StatusNotFound, errJSON("User not found", "not_found", "NOT_FOUND"))
	}
	return c.JSON(http.StatusOK, map[string]any{
		"id":          user.ID,
		"displayName": user.DisplayName,
		"email":       user.Email,
		"status":      user.Status,
		"osUsername":  user.OsUsername,
		"osDomain":    user.OsDomain,
		"lastLoginAt": user.LastLoginAt,
		"createdAt":   user.CreatedAt,
		"updatedAt":   user.UpdatedAt,
	})
}

// ListAgentUserDevices returns devices assigned to an agent user.
func (h *Handler) ListAgentUserDevices(c echo.Context) error {
	pg := parsePagination(c)
	devices, total, err := h.fleet.ListDevicesByUserID(c.Request().Context(), c.Param("id"), pg.Limit, pg.Offset)
	if err != nil {
		h.logger.Error("list agent user devices", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", "INTERNAL_ERROR"))
	}
	return c.JSON(http.StatusOK, map[string]any{"data": devices, "total": total})
}

// ListMyAgentDevices returns agent devices enrolled to the currently
// authenticated admin user. Used by the Agent Setup page's live
// "Verify" panel to show real-time install status (✅/⏳/❌) per
// device without requiring the user to also have `agent-device:read`
// on the entire fleet.
//
// AdminAuth.KeyID is the NexusUser.id (JWT `sub` claim for admin
// users); ListDevicesByUserID joins through DeviceAssignment so we
// get the canonical "this user's devices" view. Empty list = no
// enrolled devices yet — the UI renders an "install in progress"
// hint instead of an error.
func (h *Handler) ListMyAgentDevices(c echo.Context) error {
	aa := middleware.AdminAuthFromContext(c)
	if aa == nil || aa.KeyID == "" {
		return c.JSON(http.StatusUnauthorized, errJSON("Authentication required", "authentication_error", ""))
	}
	pg := parsePagination(c)
	devices, total, err := h.fleet.ListDevicesByUserID(c.Request().Context(), aa.KeyID, pg.Limit, pg.Offset)
	if err != nil {
		h.logger.Error("list my agent devices", "userID", aa.KeyID, "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", "INTERNAL_ERROR"))
	}
	return c.JSON(http.StatusOK, map[string]any{"data": devices, "total": total})
}

// ListAgentUserAudit returns audit events for an agent user.
func (h *Handler) ListAgentUserAudit(c echo.Context) error {
	pg := parsePagination(c)
	params := fleetstore.AuditEventListParams{
		SubjectID: c.Param("id"),
		Limit:     pg.Limit,
		Offset:    pg.Offset,
	}
	if v := c.QueryParam("start"); v != "" {
		if t, ok := parseRFC3339Flexible(v); ok {
			params.StartTime = &t
		}
	}
	if v := c.QueryParam("end"); v != "" {
		if t, ok := parseRFC3339Flexible(v); ok {
			params.EndTime = &t
		}
	}

	events, total, err := h.fleet.ListAuditEventsBySubjectID(c.Request().Context(), params)
	if err != nil {
		h.logger.Error("list agent user audit", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", "INTERNAL_ERROR"))
	}
	return c.JSON(http.StatusOK, map[string]any{"data": events, "total": total})
}

// SuspendAgentUser sets an agent user's status to suspended.
func (h *Handler) SuspendAgentUser(c echo.Context) error {
	return h.setAgentUserStatus(c, "suspended")
}

// ActivateAgentUser sets an agent user's status to active.
func (h *Handler) ActivateAgentUser(c echo.Context) error {
	return h.setAgentUserStatus(c, "active")
}

func (h *Handler) setAgentUserStatus(c echo.Context, status string) error {
	id := c.Param("id")
	ctx := c.Request().Context()

	existing, err := h.users.FindNexusUserByID(ctx, id)
	if err != nil {
		h.logger.Error("find agent user for status change", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", "INTERNAL_ERROR"))
	}
	if existing == nil || existing.CanAccessControlPlane {
		return c.JSON(http.StatusNotFound, errJSON("User not found", "not_found", "NOT_FOUND"))
	}

	enabled := status == "active"
	user, err := h.users.UpdateNexusUser(ctx, id, userstore.UpdateNexusUserParams{
		Enabled: &enabled,
	})
	if err != nil {
		h.logger.Error("update agent user status", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to update user", "server_error", "INTERNAL_ERROR"))
	}

	ae := audit.EntryFor(c, iam.ResourceUser, iam.VerbUpdate)
	ae.EntityID = id
	ae.BeforeState = map[string]any{"status": existing.Status}
	ae.AfterState = map[string]any{"status": status}
	h.audit.LogObserved(ctx, ae)

	return c.JSON(http.StatusOK, user)
}

// ListDeviceAudit returns audit events for a specific device.
func (h *Handler) ListDeviceAudit(c echo.Context) error {
	pg := parsePagination(c)
	params := fleetstore.AuditEventListParams{
		DeviceID: c.Param("id"),
		Limit:    pg.Limit,
		Offset:   pg.Offset,
	}
	if v := c.QueryParam("start"); v != "" {
		if t, ok := parseRFC3339Flexible(v); ok {
			params.StartTime = &t
		}
	}
	if v := c.QueryParam("end"); v != "" {
		if t, ok := parseRFC3339Flexible(v); ok {
			params.EndTime = &t
		}
	}

	events, total, err := h.fleet.ListAuditEventsByDeviceID(c.Request().Context(), params)
	if err != nil {
		h.logger.Error("list device audit", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", "INTERNAL_ERROR"))
	}
	return c.JSON(http.StatusOK, map[string]any{"data": events, "total": total})
}

// GetDeviceConfig returns the effective configuration for a device (read-only).
func (h *Handler) GetDeviceConfig(c echo.Context) error {
	id := c.Param("id")
	ctx := c.Request().Context()

	device, err := h.agents.GetThingNode(ctx, id)
	if err != nil {
		h.logger.Error("get device for config", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", "INTERNAL_ERROR"))
	}
	if device == nil {
		return c.JSON(http.StatusNotFound, errJSON("Device not found", "not_found", "NOT_FOUND"))
	}

	// Build effective config — mirrors AgentHandler.buildAgentConfig logic
	aiDomains := []string{}
	providers, _, err := h.provStore.ListProviders(ctx, providerstore.ListParams{Limit: 1000})
	if err != nil {
		h.logger.Error("load providers for device config", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", "INTERNAL_ERROR"))
	}
	for _, p := range providers {
		if !p.Enabled {
			continue
		}
		if len(p.BaseURL) > 8 {
			host := p.BaseURL
			for _, prefix := range []string{"https://", "http://"} {
				host = strings.TrimPrefix(host, prefix)
			}
			if idx := strings.IndexByte(host, '/'); idx >= 0 {
				host = host[:idx]
			}
			aiDomains = append(aiDomains, host)
		}
	}

	return c.JSON(http.StatusOK, map[string]any{
		"deviceId":  id,
		"hostname":  device.Hostname,
		"aiDomains": aiDomains,
		"sysinfo":   device.Sysinfo,
		"metadata":  device.Metadata,
	})
}

// GetDeviceTimeline returns the assignment history for a device.
func (h *Handler) GetDeviceTimeline(c echo.Context) error {
	assignments, err := h.fleet.ListDeviceAssignments(c.Request().Context(), c.Param("id"))
	if err != nil {
		h.logger.Error("list device timeline", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", "INTERNAL_ERROR"))
	}
	return c.JSON(http.StatusOK, map[string]any{"data": assignments})
}

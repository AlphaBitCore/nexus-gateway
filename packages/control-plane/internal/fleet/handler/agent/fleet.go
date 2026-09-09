package agent

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/ai/providers/providerstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/fleet/store/fleetstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/users/userstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"

	// cpiam is the ENGINE side (evaluation, candidate NRNs); iam above is the
	// shared CATALOG side (resources and verbs). Both are needed here because
	// this listing re-runs the middleware's own evaluation per row.
	cpiam "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/iam"
)

// RegisterFleetRoutes registers fleet management routes for agent users and devices.
func (h *Handler) RegisterFleetRoutes(g *echo.Group, iamMW func(action string) echo.MiddlewareFunc) {
	g.GET("/agent-users", h.ListAgentUsers, iamMW(iam.ResourceAgentDevice.Action(iam.VerbRead)))
	g.GET("/agent-users/:id", h.GetAgentUser, iamMW(iam.ResourceAgentDevice.Action(iam.VerbRead)))
	g.GET("/agent-users/:id/devices", h.ListAgentUserDevices, iamMW(iam.ResourceAgentDevice.Action(iam.VerbRead)))
	g.GET("/agent-users/:id/audit", h.ListAgentUserAudit, iamMW(iam.ResourceAgentDevice.Action(iam.VerbRead)))
	g.POST("/agent-users/:id/suspend", h.SuspendAgentUser, iamMW(iam.ResourceAgentDevice.Action(iam.VerbUpdate)))
	g.POST("/agent-users/:id/activate", h.ActivateAgentUser, iamMW(iam.ResourceAgentDevice.Action(iam.VerbUpdate)))

	// The per-device reads — audit, config and timeline — live in
	// RegisterAdminAgentDeviceRoutes with their six siblings, on the
	// device-aware middleware. This function receives no device-scoped route
	// at all, which is the point: the safe variant is the one you get by
	// default rather than the one you have to remember.

	// Self-service: current admin user's own enrolled agent devices.
	// No IAM gate — the data is inherently scoped to the caller's
	// NexusUser.id via DeviceAssignment lookup, so even a viewer
	// without `agent-device:read` can see THEIR OWN install status on
	// the Agent Setup page (used by the live Verify panel). Matches
	// the pattern of /me/admin-audit-logs (admin_traffic.go).
	g.GET("/me/agent-devices", h.ListMyAgentDevices) // iam-exempt: self-service, scoped to the caller's own devices
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

	// This route is keyed on a USER, so no device-aware middleware can gate it:
	// the device ids only exist after the query. Without a per-row check it
	// returned every one of that user's devices regardless of which device
	// groups the caller may see — the same fail-open hole the three per-device
	// routes had, reached from the other direction. A caller holding an
	// unscoped Allow plus a group-scoped Deny passes the route gate (the Deny
	// does not match a wildcard target) and then reads the rows the Deny exists
	// to withhold.
	visible, ferr := h.visibleDevices(c, devices)
	if ferr != nil {
		h.logger.Error("scope agent user devices", "error", ferr)
		return c.JSON(http.StatusInternalServerError, errJSON("Authorization service error", "server_error", "IAM_EVAL_ERROR"))
	}
	// total counts the user's assignments, not the visible subset: it comes
	// from the same COUNT the pagination is built on, and recomputing it would
	// mean evaluating every row of every page. A caller who cannot see a device
	// learns only that some exist, never which.
	return c.JSON(http.StatusOK, map[string]any{"data": visible, "total": total})
}

// visibleDevices drops rows the caller may not read, re-running the same
// candidate-NRN evaluation RequireIAMPermissionForDevice would have run had the
// route carried a device id.
//
// Fail-closed in three ways, each deliberate: no engine wired refuses the whole
// listing rather than serving it unscoped; a group-lookup error treats the
// device as having no memberships, so a group-scoped grant does not silently
// match; and an evaluation error is surfaced as a 500 rather than dropped.
func (h *Handler) visibleDevices(c echo.Context, devices []fleetstore.FleetUserDevice) ([]fleetstore.FleetUserDevice, error) {
	if len(devices) == 0 {
		return devices, nil
	}
	if h.iamEngine == nil {
		return nil, fmt.Errorf("agent devices: no IAM engine wired; refusing to serve an unscoped device listing")
	}
	aa := middleware.AdminAuthFromContext(c)
	if aa == nil {
		return nil, fmt.Errorf("agent devices: no admin principal on an authenticated route")
	}
	principalType := aa.AuthPrincipalType
	if principalType == "admin_user" {
		principalType = "nexus_user"
	}
	action := iam.ResourceAgentDevice.Action(iam.VerbRead)
	cond := cpiam.ConditionContext{"nexus:SourceIp": c.RealIP()}
	ctx := c.Request().Context()

	out := make([]fleetstore.FleetUserDevice, 0, len(devices))
	for _, d := range devices {
		var groups []string
		if h.deviceGroups != nil {
			if gs, err := h.deviceGroups.GroupsOfDevice(ctx, d.ID); err == nil {
				groups = gs
			}
			// On error groups stays empty — fail closed, matching the
			// middleware's own disposition.
		}
		res, err := h.iamEngine.EvaluateMulti(ctx, principalType, aa.KeyID, action,
			cpiam.BuildDeviceCandidateNRNs(action, d.ID, groups), cond)
		if err != nil {
			return nil, err
		}
		if res.Decision == "Allow" {
			out = append(out, d)
		}
	}
	return out, nil
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

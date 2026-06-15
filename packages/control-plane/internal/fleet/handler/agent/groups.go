package agent

import (
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/fleet/store/agentstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
)

// RegisterDeviceGroupRoutes registers device group management routes.
func (h *Handler) RegisterDeviceGroupRoutes(g *echo.Group, iamMW func(action string) echo.MiddlewareFunc) {
	g.GET("/device-groups", h.ListDeviceGroups, iamMW(iam.ResourceDeviceGroup.Action(iam.VerbRead)))
	g.POST("/device-groups", h.CreateDeviceGroup, iamMW(iam.ResourceDeviceGroup.Action(iam.VerbCreate)))
	g.GET("/device-groups/:id", h.GetDeviceGroup, iamMW(iam.ResourceDeviceGroup.Action(iam.VerbRead)))
	g.PUT("/device-groups/:id", h.UpdateDeviceGroup, iamMW(iam.ResourceDeviceGroup.Action(iam.VerbUpdate)))
	g.DELETE("/device-groups/:id", h.DeleteDeviceGroup, iamMW(iam.ResourceDeviceGroup.Action(iam.VerbDelete)))
	// Smart-group endpoints. Preview is a dry-run — operator validates the
	// predicate against the live fleet before saving; SetGroupMembershipQuery
	// is the save. Recompute is deferred to the Hub job (≤60s convergence).
	g.POST("/device-groups/preview-membership", h.PreviewMembership, iamMW(iam.ResourceDeviceGroup.Action(iam.VerbRead)))
	g.PUT("/device-groups/:id/membership-query", h.SetGroupMembershipQuery, iamMW(iam.ResourceDeviceGroup.Action(iam.VerbUpdate)))
	// Bulk-by-group admin ops. Fans out to per-device admin handlers
	// (force-refresh) with bounded parallelism; returns per-device
	// {ok, error} for partial-success rendering.
	g.POST("/device-groups/:id/force-refresh", h.BulkForceRefreshGroup, iamMW(iam.ResourceDeviceGroup.Action(iam.VerbUpdate)))
	g.POST("/device-groups/:id/members", h.AddGroupMember, iamMW(iam.ResourceDeviceGroup.Action(iam.VerbUpdate)))
	g.DELETE("/device-groups/:id/members/:deviceId", h.RemoveGroupMember, iamMW(iam.ResourceDeviceGroup.Action(iam.VerbUpdate)))
}

func (h *Handler) ListDeviceGroups(c echo.Context) error {
	pg := parsePagination(c)
	groups, total, err := h.agents.ListDeviceGroups(c.Request().Context(), agentstore.DeviceGroupListParams{
		Q: c.QueryParam("q"), Limit: pg.Limit, Offset: pg.Offset,
	})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}
	return c.JSON(http.StatusOK, map[string]any{"data": groups, "total": total})
}

// GetDeviceGroup returns the full Device Group detail payload the admin UI
// renders on /device-groups/:id: the group row plus joined memberships
// (with device hostname/os/status). Memberships is always a non-nil array
// so the UI's DataTable never crashes reading `.length` on undefined.
func (h *Handler) GetDeviceGroup(c echo.Context) error {
	ctx := c.Request().Context()
	id := c.Param("id")

	g, err := h.agents.GetDeviceGroup(ctx, id)
	if err != nil || g == nil {
		return c.JSON(http.StatusNotFound, errJSON("Device group not found", "not_found", ""))
	}

	members, err := h.agents.ListDeviceGroupMemberships(ctx, id)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to load memberships", "server_error", ""))
	}

	return c.JSON(http.StatusOK, map[string]any{
		"id":          g.ID,
		"name":        g.Name,
		"description": g.Description,
		"createdBy":   g.CreatedBy,
		"createdAt":   g.CreatedAt,
		"updatedAt":   g.UpdatedAt,
		"memberships": members,
	})
}

func (h *Handler) CreateDeviceGroup(c echo.Context) error {
	var body struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := c.Bind(&body); err != nil || body.Name == "" {
		return c.JSON(http.StatusBadRequest, errJSON("name is required", "validation_error", ""))
	}

	aa := middleware.AdminAuthFromContext(c)
	createdBy := "unknown"
	if aa != nil {
		createdBy = aa.KeyID
	}
	var desc *string
	if body.Description != "" {
		desc = &body.Description
	}

	g, err := h.agents.CreateDeviceGroup(c.Request().Context(), body.Name, desc, createdBy)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}

	ae := audit.EntryFor(c, iam.ResourceDeviceGroup, iam.VerbCreate)
	ae.EntityID = g.ID
	ae.AfterState = g
	h.audit.LogObserved(c.Request().Context(), ae)

	return c.JSON(http.StatusCreated, g)
}

func (h *Handler) UpdateDeviceGroup(c echo.Context) error {
	id := c.Param("id")
	var body struct {
		Name        *string `json:"name"`
		Description *string `json:"description"`
	}
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("Invalid request body", "validation_error", ""))
	}

	params := agentstore.UpdateDeviceGroupParams{
		Name:        body.Name,
		Description: body.Description,
	}

	g, err := h.agents.UpdateDeviceGroup(c.Request().Context(), id, params)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}

	ae := audit.EntryFor(c, iam.ResourceDeviceGroup, iam.VerbUpdate)
	ae.EntityID = id
	ae.AfterState = g
	h.audit.LogObserved(c.Request().Context(), ae)

	return c.JSON(http.StatusOK, g)
}

func (h *Handler) DeleteDeviceGroup(c echo.Context) error {
	id := c.Param("id")
	if err := h.agents.DeleteDeviceGroup(c.Request().Context(), id); err != nil {
		return c.JSON(http.StatusNotFound, errJSON("Device group not found", "not_found", ""))
	}

	ae := audit.EntryFor(c, iam.ResourceDeviceGroup, iam.VerbDelete)
	ae.EntityID = id
	h.audit.LogObserved(c.Request().Context(), ae)

	return c.NoContent(http.StatusNoContent)
}

func (h *Handler) AddGroupMember(c echo.Context) error {
	var body struct {
		DeviceID  string  `json:"deviceId"`
		ExpiresAt *string `json:"expiresAt,omitempty"` // RFC3339; nil = permanent
	}
	if err := c.Bind(&body); err != nil || body.DeviceID == "" {
		return c.JSON(http.StatusBadRequest, errJSON("deviceId is required", "validation_error", ""))
	}

	var expiresAtTime *time.Time
	if body.ExpiresAt != nil && *body.ExpiresAt != "" {
		t, err := time.Parse(time.RFC3339, *body.ExpiresAt)
		if err != nil {
			return c.JSON(http.StatusBadRequest, errJSON("expiresAt must be RFC3339", "validation_error", "INVALID_EXPIRES_AT"))
		}
		if !t.After(time.Now()) {
			return c.JSON(http.StatusBadRequest, errJSON("expiresAt must be in the future", "validation_error", "EXPIRES_AT_IN_PAST"))
		}
		expiresAtTime = &t
	}

	id, err := h.agents.AddDeviceToGroup(c.Request().Context(), c.Param("id"), body.DeviceID, expiresAtTime)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}

	ae := audit.EntryFor(c, iam.ResourceDeviceGroup, iam.VerbUpdate)
	ae.EntityID = id
	if expiresAtTime != nil {
		ae.AfterState = map[string]any{"deviceId": body.DeviceID, "expiresAt": expiresAtTime.UTC().Format(time.RFC3339)}
	} else {
		ae.AfterState = map[string]any{"deviceId": body.DeviceID}
	}
	h.audit.LogObserved(c.Request().Context(), ae)

	resp := map[string]any{"id": id, "groupId": c.Param("id"), "deviceId": body.DeviceID}
	if expiresAtTime != nil {
		resp["expiresAt"] = expiresAtTime.UTC().Format(time.RFC3339)
	}
	return c.JSON(http.StatusCreated, resp)
}

func (h *Handler) RemoveGroupMember(c echo.Context) error {
	groupID := c.Param("id")
	deviceID := c.Param("deviceId")
	if err := h.agents.RemoveDeviceFromGroup(c.Request().Context(), groupID, deviceID); err != nil {
		return c.JSON(http.StatusNotFound, errJSON("Membership not found", "not_found", ""))
	}

	ae := audit.EntryFor(c, iam.ResourceDeviceGroup, iam.VerbDelete)
	ae.EntityID = groupID + ":" + deviceID
	h.audit.LogObserved(c.Request().Context(), ae)
	return c.NoContent(http.StatusNoContent)
}

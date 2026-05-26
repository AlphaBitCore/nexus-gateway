package iam

import (
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authn"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/revocation"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/users/userstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/fleet/store/fleetstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
)

// RegisterUserRoutes registers admin user management routes.
func (h *Handler) RegisterUserRoutes(g *echo.Group, iamMW func(action string) echo.MiddlewareFunc) {
	g.GET("/users", h.ListUsers, iamMW(iam.ResourceUser.Action(iam.VerbRead)))
	g.GET("/users/:id", h.GetUser, iamMW(iam.ResourceUser.Action(iam.VerbRead)))
	g.POST("/users", h.CreateUser, iamMW(iam.ResourceUser.Action(iam.VerbCreate)))
	g.PUT("/users/:id", h.UpdateUser, iamMW(iam.ResourceUser.Action(iam.VerbUpdate)))
	g.DELETE("/users/:id", h.DeleteUser, iamMW(iam.ResourceUser.Action(iam.VerbDelete)))

	// Cross-path governance endpoints
	g.GET("/users/:id/audit", h.GetUserAudit, iamMW(iam.ResourceUser.Action(iam.VerbRead)))
	g.GET("/users/:id/identity", h.GetUserIdentity, iamMW(iam.ResourceUser.Action(iam.VerbRead)))
	g.GET("/users/:id/device-assignments", h.ListUserDeviceAssignments, iamMW(iam.ResourceUser.Action(iam.VerbRead)))
	g.POST("/users/:id/revoke-access", h.RevokeUserAccess, iamMW(iam.ResourceUser.Action(iam.VerbRevoke)))
}

func (h *Handler) ListUsers(c echo.Context) error {
	ctx := c.Request().Context()
	pg := parsePagination(c)
	params := userstore.NexusUserListParams{Q: c.QueryParam("q"), OrgID: c.QueryParam("organizationId"), IncludeSubOrgs: c.QueryParam("includeSubOrgs") == "true", Limit: pg.Limit, Offset: pg.Offset}
	if v := c.QueryParam("enabled"); v == "true" {
		t := true
		params.Enabled = &t
	} else if v == "false" {
		f := false
		params.Enabled = &f
	}
	if v := c.QueryParam("canAccessControlPlane"); v == "true" {
		t := true
		params.CanAccessControlPlane = &t
	} else if v == "false" {
		f := false
		params.CanAccessControlPlane = &f
	}

	users, total, err := h.users.ListNexusUsers(ctx, params)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}

	// Enrich each user with attached policy names (roles).
	enriched := make([]map[string]any, len(users))
	for i, u := range users {
		policyNames, _ := h.iam.ListPolicyNamesForPrincipal(ctx, "nexus_user", u.ID)
		row := map[string]any{
			"id":                    u.ID,
			"displayName":           u.DisplayName,
			"email":                 u.Email,
			"status":                u.Status,
			"canAccessControlPlane": u.CanAccessControlPlane,
			"source":                u.Source,
			"roles":                 policyNames,
			"lastLoginAt":           u.LastLoginAt,
			"createdAt":             u.CreatedAt,
			"updatedAt":             u.UpdatedAt,
		}
		if u.OrganizationID != nil {
			row["organizationId"] = *u.OrganizationID
			row["organizationName"] = u.OrganizationName
		}
		enriched[i] = row
	}

	return c.JSON(http.StatusOK, map[string]any{"data": enriched, "total": total})
}

func (h *Handler) GetUser(c echo.Context) error {
	ctx := c.Request().Context()
	id := c.Param("id")

	user, err := h.users.GetNexusUserSafe(ctx, id)
	if err != nil {
		h.logger.Error("get user", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}
	if user == nil {
		return c.JSON(http.StatusNotFound, errJSON("User not found", "not_found", ""))
	}

	policyNames, _ := h.iam.ListPolicyNamesForPrincipal(ctx, "nexus_user", user.ID)
	policyAttachments, _ := h.iam.ListPrincipalPolicyAttachments(ctx, "nexus_user", user.ID)
	orgID, orgName, _ := h.users.GetNexusUserOrgInfo(ctx, user.ID)

	resp := map[string]any{
		"id":                    user.ID,
		"displayName":           user.DisplayName,
		"email":                 user.Email,
		"status":                user.Status,
		"canAccessControlPlane": user.CanAccessControlPlane,
		"source":                user.Source,
		"roles":                 policyNames,
		"policyAttachments":     policyAttachments,
		"lastLoginAt":           user.LastLoginAt,
		"createdAt":             user.CreatedAt,
		"updatedAt":             user.UpdatedAt,
	}
	if orgID != "" {
		resp["organizationId"] = orgID
		resp["organizationName"] = orgName
	}
	return c.JSON(http.StatusOK, resp)
}

func (h *Handler) CreateUser(c echo.Context) error {
	var body struct {
		Username              string  `json:"username"`
		Email                 string  `json:"email"`
		Password              string  `json:"password"`
		OrganizationID        *string `json:"organizationId"`
		CanAccessControlPlane *bool   `json:"canAccessControlPlane"`
	}
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("Invalid request body", "validation_error", ""))
	}
	if body.Username == "" || body.Password == "" {
		return c.JSON(http.StatusBadRequest, errJSON("username and password are required", "validation_error", ""))
	}

	aa := middleware.AdminAuthFromContext(c)
	createdBy := "unknown"
	if aa != nil {
		createdBy = aa.KeyID
	}
	var email *string
	if body.Email != "" {
		email = &body.Email
	}

	canAccessCP := true
	if body.CanAccessControlPlane != nil {
		canAccessCP = *body.CanAccessControlPlane
	}

	hashedPw, err := auth.HashPassword(body.Password)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Password hashing failed", "server_error", ""))
	}

	user, err := h.users.CreateNexusUser(c.Request().Context(), userstore.CreateNexusUserParams{
		DisplayName:           body.Username,
		Email:                 email,
		PasswordHash:          &hashedPw,
		CanAccessControlPlane: &canAccessCP,
		OrganizationID:        body.OrganizationID,
		CreatedBy:             createdBy,
		Source:                "local",
	})
	if err != nil {
		h.logger.Error("create user", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to create user", "server_error", ""))
	}

	ae := audit.EntryFor(c, iam.ResourceUser, iam.VerbCreate)
	ae.EntityID = user.ID
	ae.AfterState = map[string]any{"displayName": user.DisplayName}
	h.audit.LogObserved(c.Request().Context(), ae)

	return c.JSON(http.StatusCreated, user)
}

func (h *Handler) UpdateUser(c echo.Context) error {
	id := c.Param("id")
	var body struct {
		DisplayName           *string `json:"displayName"`
		Email                 *string `json:"email"`
		Enabled               *bool   `json:"enabled"`
		Password              *string `json:"password"`
		OrganizationID        *string `json:"organizationId"`
		CanAccessControlPlane *bool   `json:"canAccessControlPlane"`
	}
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("Invalid request body", "validation_error", ""))
	}

	params := userstore.UpdateNexusUserParams{
		DisplayName:           body.DisplayName,
		Email:                 body.Email,
		Enabled:               body.Enabled,
		OrganizationID:        body.OrganizationID,
		CanAccessControlPlane: body.CanAccessControlPlane,
	}
	if body.Password != nil && *body.Password != "" {
		hashed, err := auth.HashPassword(*body.Password)
		if err == nil {
			params.PasswordHash = &hashed
		}
	}

	user, err := h.users.UpdateNexusUser(c.Request().Context(), id, params)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to update user", "server_error", ""))
	}

	// Disabling a user must kick any live sessions off the data plane. We
	// fire on every Enabled=false write rather than only the true->false
	// edge because a duplicate scope=user event is idempotent (the RS-side
	// checker dedupes on cutoff maps; see spec 8.6) and this avoids an
	// extra read round-trip to determine the previous state.
	if body.Enabled != nil && !*body.Enabled {
		h.revokeUserScope(c.Request().Context(), id, revocation.ReasonAdminDisable)
	}

	ae := audit.EntryFor(c, iam.ResourceUser, iam.VerbUpdate)
	ae.EntityID = id
	ae.AfterState = map[string]any{
		"id": user.ID, "displayName": user.DisplayName, "email": user.Email,
		"status": user.Status, "canAccessControlPlane": user.CanAccessControlPlane,
		"passwordChanged": body.Password != nil && *body.Password != "",
	}
	h.audit.LogObserved(c.Request().Context(), ae)

	return c.JSON(http.StatusOK, user)
}

func (h *Handler) DeleteUser(c echo.Context) error {
	id := c.Param("id")
	if err := h.users.DeleteNexusUser(c.Request().Context(), id); err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to delete user", "server_error", ""))
	}

	ae := audit.EntryFor(c, iam.ResourceUser, iam.VerbDelete)
	ae.EntityID = id
	h.audit.LogObserved(c.Request().Context(), ae)
	return c.NoContent(http.StatusNoContent)
}

// GetUserAudit returns unified audit events for a user across all paths
// (VK ownership, device assignments).
func (h *Handler) GetUserAudit(c echo.Context) error {
	id := c.Param("id")
	pg := parsePagination(c)

	events, total, err := h.governance.GetUserAuditEvents(c.Request().Context(), id, pg.Limit, pg.Offset)
	if err != nil {
		h.logger.Error("get user audit events", "userId", id, "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to fetch audit events", "server_error", ""))
	}
	return c.JSON(http.StatusOK, map[string]any{"data": events, "total": total})
}

// GetUserIdentity returns a cross-path identity summary for a user, including
// their VirtualKeys, assigned devices, and audit event counts per source.
func (h *Handler) GetUserIdentity(c echo.Context) error {
	ctx := c.Request().Context()
	id := c.Param("id")

	// Fetch user and build safe view (without password hash).
	user, err := h.users.FindNexusUserByID(ctx, id)
	if err != nil {
		h.logger.Error("get user identity: find user", "userId", id, "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to fetch user", "server_error", ""))
	}
	if user == nil {
		return c.JSON(http.StatusNotFound, errJSON("User not found", "not_found", "USER_NOT_FOUND"))
	}

	userSafe := userstore.NexusUserSafe{
		ID:                    user.ID,
		DisplayName:           user.DisplayName,
		Email:                 user.Email,
		Status:                user.Status,
		CanAccessControlPlane: user.CanAccessControlPlane,
		Source:                user.Source,
		LastLoginAt:           user.LastLoginAt,
		CreatedAt:             user.CreatedAt,
		UpdatedAt:             user.UpdatedAt,
	}

	// Fetch virtual keys, devices, and audit summary in sequence (could be parallelized later).
	vks, err := h.governance.ListVirtualKeysByOwner(ctx, id)
	if err != nil {
		h.logger.Error("get user identity: list vks", "userId", id, "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to fetch virtual keys", "server_error", ""))
	}

	devices, err := h.governance.ListActiveDevicesByUser(ctx, id)
	if err != nil {
		h.logger.Error("get user identity: list devices", "userId", id, "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to fetch devices", "server_error", ""))
	}

	auditSummary, err := h.governance.GetUserAuditSummary(ctx, id)
	if err != nil {
		h.logger.Error("get user identity: audit summary", "userId", id, "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to fetch audit summary", "server_error", ""))
	}

	return c.JSON(http.StatusOK, map[string]any{
		"user":         userSafe,
		"virtualKeys":  vks,
		"devices":      devices,
		"auditSummary": auditSummary,
	})
}

// RevokeUserAccess revokes a user's access across all paths: disables VirtualKeys,
// revokes assigned devices, and suspends the user account.
func (h *Handler) RevokeUserAccess(c echo.Context) error {
	ctx := c.Request().Context()
	id := c.Param("id")

	// Verify user exists.
	user, err := h.users.FindNexusUserByID(ctx, id)
	if err != nil {
		h.logger.Error("revoke user access: find user", "userId", id, "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to fetch user", "server_error", ""))
	}
	if user == nil {
		return c.JSON(http.StatusNotFound, errJSON("User not found", "not_found", "USER_NOT_FOUND"))
	}

	// 1. Disable all VirtualKeys owned by this user.
	keysDisabled, err := h.governance.DisableVirtualKeysByOwner(ctx, id)
	if err != nil {
		h.logger.Error("revoke user access: disable keys", "userId", id, "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to disable virtual keys", "server_error", ""))
	}

	// 2. Revoke all devices assigned to this user.
	devicesRevoked, err := h.governance.RevokeDevicesByUser(ctx, id)
	if err != nil {
		h.logger.Error("revoke user access: revoke devices", "userId", id, "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to revoke devices", "server_error", ""))
	}

	// 3. Suspend the user.
	if err := h.governance.SuspendUser(ctx, id); err != nil {
		h.logger.Error("revoke user access: suspend user", "userId", id, "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to suspend user", "server_error", ""))
	}

	// 4. Kick live auth sessions off the data plane. Mints a scope=user
	// revocation and deletes matching RefreshToken rows.
	h.revokeUserScope(ctx, id, revocation.ReasonAdminDisable)

	// Audit log the revocation.
	ae := audit.EntryFor(c, iam.ResourceUser, iam.VerbRevoke)
	ae.EntityID = id
	ae.AfterState = map[string]any{
		"keysDisabled":   keysDisabled,
		"devicesRevoked": devicesRevoked,
		"userSuspended":  true,
	}
	h.audit.LogObserved(ctx, ae)

	return c.JSON(http.StatusOK, map[string]any{
		"keysDisabled":   keysDisabled,
		"devicesRevoked": devicesRevoked,
		"userSuspended":  true,
		"userId":         id,
	})
}

// ListUserDeviceAssignments returns all device assignments (active + historical)
// for a user, ordered by most recent first.
func (h *Handler) ListUserDeviceAssignments(c echo.Context) error {
	id := c.Param("id")
	assignments, err := h.fleet.ListDeviceAssignmentsByUser(c.Request().Context(), id)
	if err != nil {
		h.logger.Error("list user device assignments", "userId", id, "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to fetch device assignments", "server_error", ""))
	}
	if assignments == nil {
		assignments = []fleetstore.DeviceAssignmentDetail{}
	}
	return c.JSON(http.StatusOK, map[string]any{"data": assignments, "total": len(assignments)})
}

package me

import (
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/users/userstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/ai/virtualkeys/handler"
	authpkg "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authn"
)

// RegisterMyRoutes registers personal self-service routes under /api/my.
// No IAM gate — every authenticated user can manage their own resources.
func (h *Handler) RegisterMyRoutes(g *echo.Group) {
	// Profile
	g.GET("/profile", h.GetMyProfile)
	g.PATCH("/profile", h.UpdateMyProfile)

	// Activity
	g.GET("/activity", h.ListMyActivity)

	// API keys (personal)
	g.GET("/api-keys", h.ListUserAPIKeys)
	g.POST("/api-keys", h.CreateUserAPIKey)
	g.DELETE("/api-keys/:id", h.DeleteUserAPIKey)
	g.POST("/api-keys/:id/regenerate", h.RegenerateUserAPIKey)

	// Virtual keys (personal) — R6 extracted into handler/virtualkey/.
	virtualkey.New(virtualkey.Deps{
		Pool:   h.pool,
		Hub:    h.hub,
		Audit:  h.audit,
		Logger: h.logger,
	}).RegisterUserVirtualKeyRoutes(g)
}

// GetMyProfile returns the current user's profile.
func (h *Handler) GetMyProfile(c echo.Context) error {
	userID := currentUserID(c)
	if userID == "" {
		return c.JSON(http.StatusUnauthorized, errJSON("Authentication required", "authentication_error", ""))
	}

	user, err := h.users.FindNexusUserByID(c.Request().Context(), userID)
	if err != nil || user == nil {
		return c.JSON(http.StatusNotFound, errJSON("User not found", "not_found", ""))
	}

	aa := middleware.AdminAuthFromContext(c)
	var groups []string
	if aa != nil {
		pt := aa.AuthPrincipalType
		if pt == "admin_user" {
			pt = "nexus_user"
		}
		groups, _ = h.iam.ListGroupNamesForPrincipal(c.Request().Context(), pt, aa.KeyID)
	}

	return c.JSON(http.StatusOK, map[string]any{
		"id":                user.ID,
		"displayName":       user.DisplayName,
		"email":             user.Email,
		"status":            user.Status,
		"roles":             groups,
		"createdAt":         user.CreatedAt,
		"preferredTimezone": user.PreferredTimezone,
	})
}

// UpdateMyProfile updates the current user's profile (display name, email, password).
func (h *Handler) UpdateMyProfile(c echo.Context) error {
	userID := currentUserID(c)
	if userID == "" {
		return c.JSON(http.StatusUnauthorized, errJSON("Authentication required", "authentication_error", ""))
	}

	aa := middleware.AdminAuthFromContext(c)
	if aa == nil || aa.AuthPrincipalType != "admin_user" {
		return c.JSON(http.StatusForbidden, errJSON("Only admin users can update profile", "authorization_error", ""))
	}

	var body struct {
		DisplayName       *string `json:"displayName"`
		Username          *string `json:"username"`
		Email             *string `json:"email"`
		CurrentPassword   *string `json:"currentPassword"`
		NewPassword       *string `json:"newPassword"`
		PreferredTimezone *string `json:"preferredTimezone"`
	}
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("Invalid request body", "validation_error", ""))
	}

	// Reject unknown IANA TZ names so a typo doesn't end up in the DB.
	// Empty string is allowed and means "clear back to browser default".
	if body.PreferredTimezone != nil && *body.PreferredTimezone != "" {
		if _, err := time.LoadLocation(*body.PreferredTimezone); err != nil {
			return c.JSON(http.StatusBadRequest, errJSON(
				"preferredTimezone must be a valid IANA timezone name",
				"validation_error", ""))
		}
	}

	// Allow "username" as alias for "displayName"
	displayName := body.DisplayName
	if displayName == nil {
		displayName = body.Username
	}

	params := userstore.UpdateNexusUserParams{
		DisplayName:       displayName,
		Email:             body.Email,
		PreferredTimezone: body.PreferredTimezone,
	}

	if body.NewPassword != nil {
		if body.CurrentPassword == nil {
			return c.JSON(http.StatusBadRequest, errJSON("currentPassword is required to change password", "validation_error", ""))
		}
		existing, err := h.users.FindNexusUserByID(c.Request().Context(), userID)
		if err != nil || existing == nil {
			return c.JSON(http.StatusInternalServerError, errJSON("Failed to load user", "server_error", ""))
		}
		if existing.PasswordHash == nil || !authpkg.VerifyPassword(*body.CurrentPassword, *existing.PasswordHash) {
			return c.JSON(http.StatusUnauthorized, errJSON("Current password is incorrect", "authorization_error", ""))
		}
		newHash, err := authpkg.HashPassword(*body.NewPassword)
		if err != nil {
			h.logger.Error("hash new password", "error", err)
			return c.JSON(http.StatusInternalServerError, errJSON("Failed to hash password", "server_error", ""))
		}
		params.PasswordHash = &newHash
	}

	user, err := h.users.UpdateNexusUser(c.Request().Context(), userID, params)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to update profile", "server_error", ""))
	}

	ae := audit.EntryFor(c, iam.ResourceUser, iam.VerbUpdate)
	ae.EntityID = userID
	h.audit.LogObserved(c.Request().Context(), ae)

	return c.JSON(http.StatusOK, user)
}

// ListMyActivity returns admin audit logs for the current user.
func (h *Handler) ListMyActivity(c echo.Context) error {
	userID := currentUserID(c)
	if userID == "" {
		return c.JSON(http.StatusUnauthorized, errJSON("Authentication required", "authentication_error", ""))
	}

	params := parseAdminAuditParams(c)
	params.ActorID = userID

	data, total, err := h.traffic.ListAdminAuditLogs(c.Request().Context(), params)
	if err != nil {
		h.logger.Error("list my activity", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}
	return c.JSON(http.StatusOK, map[string]any{"data": data, "total": total, "limit": params.Limit, "offset": params.Offset})
}

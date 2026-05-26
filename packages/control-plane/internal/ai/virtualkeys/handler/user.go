package virtualkey

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/ai/virtualkeys/vkstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
)

// RegisterUserVirtualKeyRoutes registers personal VK self-service routes.
// These are placed under /api/user/ and only require session auth — no IAM
// middleware is needed because ownership is enforced in every handler.
func (h *Handler) RegisterUserVirtualKeyRoutes(g *echo.Group) {
	g.GET("/virtual-keys", h.ListUserVirtualKeys)
	g.POST("/virtual-keys", h.CreateUserVirtualKey)
	g.PUT("/virtual-keys/:id", h.UpdateUserVirtualKey)
	g.DELETE("/virtual-keys/:id", h.DeleteUserVirtualKey)
	g.POST("/virtual-keys/:id/regenerate", h.RegenerateUserVirtualKey)
}

// currentUserID extracts the authenticated user's key ID from the session.
// Returns "" if no session is present (should not happen on a protected route).
func currentUserID(c echo.Context) string {
	aa := middleware.AdminAuthFromContext(c)
	if aa == nil {
		return ""
	}
	return aa.KeyID
}

// ListUserVirtualKeys returns the current user's personal virtual keys.
func (h *Handler) ListUserVirtualKeys(c echo.Context) error {
	userID := currentUserID(c)
	if userID == "" {
		return c.JSON(http.StatusUnauthorized, errJSON("Authentication required", "authentication_error", ""))
	}

	pg := parsePagination(c)
	params := vkstore.VirtualKeyListParams{
		OwnerID: userID,
		VKType:  "personal",
		Q:       c.QueryParam("q"),
		Limit:   pg.Limit,
		Offset:  pg.Offset,
	}
	if v := c.QueryParam("enabled"); v == "true" {
		t := true
		params.Enabled = &t
	} else if v == "false" {
		f := false
		params.Enabled = &f
	}

	keys, total, err := h.vks.ListVirtualKeys(c.Request().Context(), params)
	if err != nil {
		h.logger.Error("list user virtual keys", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}
	return c.JSON(http.StatusOK, map[string]any{"data": keys, "total": total})
}

// CreateUserVirtualKey creates a personal virtual key owned by the current user.
func (h *Handler) CreateUserVirtualKey(c echo.Context) error {
	userID := currentUserID(c)
	if userID == "" {
		return c.JSON(http.StatusUnauthorized, errJSON("Authentication required", "authentication_error", ""))
	}

	var body struct {
		Name                        string          `json:"name"`
		SourceApp                   *string         `json:"sourceApp"`
		Enabled                     *bool           `json:"enabled"`
		RateLimitRpm                *int            `json:"rateLimitRpm"`
		CompareEndpointRateLimitRpm *int            `json:"compareEndpointRateLimitRpm"`
		AllowedModels               json.RawMessage `json:"allowedModels"`
		ExpiresAt                   *time.Time      `json:"expiresAt"`
	}
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("Invalid request body", "validation_error", ""))
	}
	if body.Name == "" {
		return c.JSON(http.StatusBadRequest, errJSON("name is required", "validation_error", ""))
	}

	rawKey, keyHash, keyPrefix, err := generateVirtualKey()
	if err != nil {
		h.logger.Error("rand.Read for user virtual key", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to generate virtual key", "server_error", ""))
	}

	enabled := true
	if body.Enabled != nil {
		enabled = *body.Enabled
	}

	allowedModels := []byte("[]")
	if body.AllowedModels != nil {
		allowedModels = body.AllowedModels
	}

	vk, err := h.vks.CreateVirtualKey(c.Request().Context(), vkstore.CreateVirtualKeyParams{
		Name:                        body.Name,
		KeyHash:                     keyHash,
		KeyPrefix:                   keyPrefix,
		SourceApp:                   body.SourceApp,
		Enabled:                     enabled,
		RateLimitRpm:                body.RateLimitRpm,
		CompareEndpointRateLimitRpm: body.CompareEndpointRateLimitRpm,
		AllowedModels:               allowedModels,
		OwnerID:                     &userID,
		ExpiresAt:                   body.ExpiresAt,
		VKType:                      "personal",
		VKStatus:                    "active",
	})
	if err != nil {
		h.logger.Error("create user virtual key", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}

	ae := audit.EntryFor(c, iam.ResourceVirtualKey, iam.VerbCreate)
	ae.EntityID = vk.Name
	ae.AfterState = map[string]any{
		"id": vk.ID, "name": vk.Name, "keyPrefix": vk.KeyPrefix,
		"vkType": "personal", "ownerId": userID,
	}
	h.audit.LogObserved(c.Request().Context(), ae)

	// Return raw key once — it will not be shown again.
	resp := map[string]any{
		"id": vk.ID, "name": vk.Name, "keyPrefix": vk.KeyPrefix,
		"sourceApp": vk.SourceApp, "enabled": vk.Enabled,
		"rateLimitRpm":  vk.RateLimitRpm,
		"allowedModels": vk.AllowedModels,
		"ownerId":       vk.OwnerID, "vkType": "personal", "vkStatus": "active",
		"expiresAt": vk.ExpiresAt,
		"createdAt": vk.CreatedAt, "updatedAt": vk.UpdatedAt,
		"key": rawKey,
	}
	return c.JSON(http.StatusCreated, resp)
}

// UpdateUserVirtualKey updates a personal VK owned by the current user.
func (h *Handler) UpdateUserVirtualKey(c echo.Context) error {
	userID := currentUserID(c)
	if userID == "" {
		return c.JSON(http.StatusUnauthorized, errJSON("Authentication required", "authentication_error", ""))
	}

	id := c.Param("id")
	existing, err := h.vks.GetVirtualKey(c.Request().Context(), id)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}
	if existing == nil {
		return c.JSON(http.StatusNotFound, errJSON("Virtual key not found", "not_found", ""))
	}
	if existing.OwnerID == nil || *existing.OwnerID != userID {
		return c.JSON(http.StatusForbidden, errJSON("Access denied", "authorization_error", ""))
	}

	var body struct {
		SourceApp                   *string  `json:"sourceApp"`
		Enabled                     *bool    `json:"enabled"`
		RateLimitRpm                *int     `json:"rateLimitRpm"`
		CompareEndpointRateLimitRpm *int     `json:"compareEndpointRateLimitRpm"`
		AllowedModels               any      `json:"allowedModels"`
	}
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("Invalid request body", "validation_error", ""))
	}

	params := vkstore.UpdateVirtualKeyParams{
		SourceApp:                   body.SourceApp,
		Enabled:                     body.Enabled,
		RateLimitRpm:                body.RateLimitRpm,
		CompareEndpointRateLimitRpm: body.CompareEndpointRateLimitRpm,
	}
	if body.AllowedModels != nil {
		raw, _ := json.Marshal(body.AllowedModels)
		params.AllowedModels = raw
	}

	updated, err := h.vks.UpdateVirtualKey(c.Request().Context(), existing.ID, params)
	if err != nil {
		h.logger.Error("update user virtual key", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}

	// Targeted invalidate-by-keyHash on the ai-gateway VK cache —
	// mirrors the admin path (admin_virtual_keys.go::UpdateVirtualKey).
	// Without this, the personal-self-service edit (disable, change
	// allowedModels, change rate limit) doesn't take effect until the
	// gateway VK cache TTL elapses or the gateway restarts.
	h.notifyVKInvalidate(c, existing.KeyHash)

	ae := audit.EntryFor(c, iam.ResourceVirtualKey, iam.VerbUpdate)
	ae.EntityID = updated.Name
	ae.BeforeState = existing
	ae.AfterState = updated
	h.audit.LogObserved(c.Request().Context(), ae)

	return c.JSON(http.StatusOK, updated)
}

// DeleteUserVirtualKey deletes a personal VK owned by the current user.
func (h *Handler) DeleteUserVirtualKey(c echo.Context) error {
	userID := currentUserID(c)
	if userID == "" {
		return c.JSON(http.StatusUnauthorized, errJSON("Authentication required", "authentication_error", ""))
	}

	id := c.Param("id")
	existing, err := h.vks.GetVirtualKey(c.Request().Context(), id)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}
	if existing == nil {
		return c.JSON(http.StatusNotFound, errJSON("Virtual key not found", "not_found", ""))
	}
	if existing.OwnerID == nil || *existing.OwnerID != userID {
		return c.JSON(http.StatusForbidden, errJSON("Access denied", "authorization_error", ""))
	}

	if err := h.vks.DeleteVirtualKey(c.Request().Context(), existing.ID); err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}

	h.notifyVKInvalidate(c, existing.KeyHash)

	ae := audit.EntryFor(c, iam.ResourceVirtualKey, iam.VerbDelete)
	ae.EntityID = existing.Name
	ae.BeforeState = existing
	h.audit.LogObserved(c.Request().Context(), ae)

	return c.JSON(http.StatusOK, map[string]any{"deleted": true, "name": existing.Name})
}

// RegenerateUserVirtualKey regenerates the key secret for a personal VK owned by the current user.
func (h *Handler) RegenerateUserVirtualKey(c echo.Context) error {
	userID := currentUserID(c)
	if userID == "" {
		return c.JSON(http.StatusUnauthorized, errJSON("Authentication required", "authentication_error", ""))
	}

	id := c.Param("id")
	vk, err := h.vks.GetVirtualKey(c.Request().Context(), id)
	if err != nil || vk == nil {
		return c.JSON(http.StatusNotFound, errJSON("Virtual key not found", "not_found", ""))
	}
	if vk.OwnerID == nil || *vk.OwnerID != userID {
		return c.JSON(http.StatusForbidden, errJSON("Access denied", "authorization_error", ""))
	}

	rawKey, keyHash, keyPrefix, err := generateVirtualKey()
	if err != nil {
		h.logger.Error("rand.Read for regenerated user virtual key", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to regenerate virtual key", "server_error", ""))
	}
	if err := h.vks.RegenerateVirtualKeyHash(c.Request().Context(), id, keyHash, keyPrefix); err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}

	// Invalidate the OLD keyHash — captured pre-rotate above — so the
	// gateway stops accepting the previous secret immediately. Without
	// this the old key and the new key would both authenticate for the
	// duration of the gateway's VK LRU window.
	h.notifyVKInvalidate(c, vk.KeyHash)

	ae := audit.EntryFor(c, iam.ResourceVirtualKey, iam.VerbUpdate)
	ae.EntityID = id
	ae.AfterState = map[string]any{"regenerated": true}
	h.audit.LogObserved(c.Request().Context(), ae)

	return c.JSON(http.StatusOK, map[string]any{
		"id": id, "key": rawKey, "keyPrefix": keyPrefix,
		"message": "Key regenerated. Save this key — it will not be shown again.",
	})
}

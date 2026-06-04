package me

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authn"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/users/userstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
)

// isDuplicateKeyError checks if a DB error is a PostgreSQL unique violation (23505).
func isDuplicateKeyError(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// RegisterUserAPIKeyRoutes registers personal API key self-service routes.
// Placed under /api/user/ — no IAM gate, ownership enforced per handler.
func (h *Handler) RegisterUserAPIKeyRoutes(g *echo.Group) {
	g.GET("/api-keys", h.ListUserAPIKeys)
	g.POST("/api-keys", h.CreateUserAPIKey)
	g.DELETE("/api-keys/:id", h.DeleteUserAPIKey)
	g.POST("/api-keys/:id/regenerate", h.RegenerateUserAPIKey)
}

// ListUserAPIKeys returns API keys owned by the current session user.
func (h *Handler) ListUserAPIKeys(c echo.Context) error {
	userID := currentUserID(c)
	if userID == "" {
		return c.JSON(http.StatusUnauthorized, errJSON("Authentication required", "authentication_error", ""))
	}

	keys, err := h.users.ListAdminAPIKeys(c.Request().Context(), userID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to list API keys", "server_error", ""))
	}
	return c.JSON(http.StatusOK, map[string]any{"data": keys})
}

// CreateUserAPIKey creates an API key owned by the current session user.
func (h *Handler) CreateUserAPIKey(c echo.Context) error {
	userID := currentUserID(c)
	if userID == "" {
		return c.JSON(http.StatusUnauthorized, errJSON("Authentication required", "authentication_error", ""))
	}

	var body struct {
		Name      string `json:"name"`
		ExpiresAt string `json:"expiresAt"`
	}
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("Invalid request body", "validation_error", ""))
	}
	if body.Name == "" {
		return c.JSON(http.StatusBadRequest, errJSON("name is required", "validation_error", ""))
	}

	aa := middleware.AdminAuthFromContext(c)
	createdBy := "unknown"
	if aa != nil {
		createdBy = aa.KeyName
	}

	rawBytes := make([]byte, 32)
	if _, err := rand.Read(rawBytes); err != nil {
		h.logger.Error("rand.Read for new api key", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to generate API key", "server_error", ""))
	}
	rawKey := "nxk_" + hex.EncodeToString(rawBytes)
	keyHash := auth.HashAPIKey(rawKey)
	keyPrefix := rawKey[:12]

	var expiresAt *time.Time
	if body.ExpiresAt != "" {
		t, err := time.Parse(time.RFC3339, body.ExpiresAt)
		if err != nil {
			return c.JSON(http.StatusBadRequest, errJSON("invalid expiresAt; expected RFC3339", "validation_error", ""))
		}
		expiresAt = &t
	}

	k, err := h.users.CreateAdminAPIKey(c.Request().Context(), userstore.CreateAdminAPIKeyParams{
		Name:        body.Name,
		KeyHash:     keyHash,
		KeyPrefix:   keyPrefix,
		CreatedBy:   createdBy,
		ExpiresAt:   expiresAt,
		OwnerUserID: &userID,
	})
	if err != nil {
		if isDuplicateKeyError(err) {
			return c.JSON(http.StatusConflict, errJSON("An API key with this name already exists", "duplicate_name", ""))
		}
		h.logger.Error("create user api key", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to create API key", "server_error", ""))
	}

	ae := audit.EntryFor(c, iam.ResourceApiKey, iam.VerbCreate)
	ae.EntityID = k.ID
	ae.AfterState = map[string]any{"name": k.Name, "keyPrefix": keyPrefix}
	h.audit.LogObserved(c.Request().Context(), ae)

	return c.JSON(http.StatusCreated, map[string]any{
		"id": k.ID, "name": k.Name, "key": rawKey, "keyPrefix": keyPrefix,
		"expiresAt": k.ExpiresAt, "createdAt": k.CreatedAt,
	})
}

// DeleteUserAPIKey deletes an API key owned by the current user.
func (h *Handler) DeleteUserAPIKey(c echo.Context) error {
	userID := currentUserID(c)
	if userID == "" {
		return c.JSON(http.StatusUnauthorized, errJSON("Authentication required", "authentication_error", ""))
	}

	id := c.Param("id")
	k, err := h.users.GetAdminAPIKey(c.Request().Context(), id)
	if err != nil || k == nil {
		return c.JSON(http.StatusNotFound, errJSON("API key not found", "not_found", ""))
	}
	if k.OwnerUserID == nil || *k.OwnerUserID != userID {
		return c.JSON(http.StatusForbidden, errJSON("Access denied", "authorization_error", ""))
	}

	if err := h.users.DeleteAdminAPIKey(c.Request().Context(), id); err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to delete API key", "server_error", ""))
	}

	ae := audit.EntryFor(c, iam.ResourceApiKey, iam.VerbDelete)
	ae.EntityID = id
	h.audit.LogObserved(c.Request().Context(), ae)

	return c.JSON(http.StatusOK, map[string]any{"deleted": true, "id": id})
}

// RegenerateUserAPIKey regenerates an API key owned by the current user.
func (h *Handler) RegenerateUserAPIKey(c echo.Context) error {
	userID := currentUserID(c)
	if userID == "" {
		return c.JSON(http.StatusUnauthorized, errJSON("Authentication required", "authentication_error", ""))
	}

	id := c.Param("id")
	k, err := h.users.GetAdminAPIKey(c.Request().Context(), id)
	if err != nil || k == nil {
		return c.JSON(http.StatusNotFound, errJSON("API key not found", "not_found", ""))
	}
	if k.OwnerUserID == nil || *k.OwnerUserID != userID {
		return c.JSON(http.StatusForbidden, errJSON("Access denied", "authorization_error", ""))
	}

	rawBytes := make([]byte, 32)
	if _, err := rand.Read(rawBytes); err != nil {
		h.logger.Error("rand.Read for regenerated api key", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to regenerate API key", "server_error", ""))
	}
	rawKey := "nxk_" + hex.EncodeToString(rawBytes)
	keyHash := auth.HashAPIKey(rawKey)
	keyPrefix := rawKey[:12]

	if err := h.users.RegenerateAdminAPIKey(c.Request().Context(), id, keyHash, keyPrefix); err != nil {
		h.logger.Error("regenerate user api key", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to regenerate API key", "server_error", ""))
	}

	ae := audit.EntryFor(c, iam.ResourceApiKey, iam.VerbUpdate)
	ae.EntityID = id
	ae.AfterState = map[string]any{"regenerated": true, "keyPrefix": keyPrefix}
	h.audit.LogObserved(c.Request().Context(), ae)

	return c.JSON(http.StatusOK, map[string]any{
		"id": id, "key": rawKey, "keyPrefix": keyPrefix,
	})
}

package virtualkey

import (
	"bytes"
	"errors"
	"github.com/goccy/go-json"
	"io"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/ai/virtualkeys/vkstore"
	auth "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authn"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/hub"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
)

func (h *Handler) ListVirtualKeys(c echo.Context) error {
	aa := middleware.AdminAuthFromContext(c)

	pg := parsePagination(c)
	params := vkstore.VirtualKeyListParams{
		Q:              c.QueryParam("q"),
		ProjectID:      c.QueryParam("projectId"),
		OrganizationID: c.QueryParam("organizationId"),
		OwnerID:        c.QueryParam("ownerId"),
		VKType:         c.QueryParam("vkType"),
		VKStatus:       c.QueryParam("vkStatus"),
		Limit:          pg.Limit,
		Offset:         pg.Offset,
	}
	if v := c.QueryParam("enabled"); v == "true" {
		t := true
		params.Enabled = &t
	} else if v == "false" {
		f := false
		params.Enabled = &f
	}

	// Non-admin users can only see their own VKs.
	if aa != nil && !h.isSuperAdmin(c, aa) {
		params.OwnerID = aa.KeyID
	}

	keys, total, err := h.vks.ListVirtualKeys(c.Request().Context(), params)
	if err != nil {
		h.logger.Error("list virtual keys", "error", err)
		return internalServerError(c, "Internal server error")
	}
	return c.JSON(http.StatusOK, map[string]any{"data": keys, "total": total})
}

func (h *Handler) GetVirtualKey(c echo.Context) error {
	vk, err := h.resolveVK(c)
	if err != nil {
		return internalServerError(c, "Internal server error")
	}
	if vk == nil {
		return c.JSON(http.StatusNotFound, errJSON("Virtual key not found", "not_found", ""))
	}

	aa := middleware.AdminAuthFromContext(c)
	if aa != nil && !h.isSuperAdmin(c, aa) {
		if vk.OwnerID == nil || *vk.OwnerID != aa.KeyID {
			return c.JSON(http.StatusForbidden, errJSON("Access denied", "authorization_error", ""))
		}
	}

	return c.JSON(http.StatusOK, vk)
}

// vkNameConflict maps a unique violation on one of the VirtualKey name
// partial indexes (see tools/db-migrate/schema-extras.sql) to a caller-facing
// message. Application VK names are unique fleet-wide; personal VK names are
// unique per owner.
func vkNameConflict(err error) (string, bool) {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		return "", false
	}
	switch pgErr.ConstraintName {
	case "VirtualKey_application_name_uniq":
		return "An application virtual key with this name already exists", true
	case "VirtualKey_personal_owner_name_uniq":
		return "You already have a virtual key with this name", true
	}
	return "", false
}

func (h *Handler) CreateVirtualKey(c echo.Context) error {
	var body struct {
		Name                        string          `json:"name"`
		ProjectID                   *string         `json:"projectId"`
		SourceApp                   *string         `json:"sourceApp"`
		Enabled                     *bool           `json:"enabled"`
		RateLimitRpm                *int            `json:"rateLimitRpm"`
		CompareEndpointRateLimitRpm *int            `json:"compareEndpointRateLimitRpm"`
		AllowedModels               json.RawMessage `json:"allowedModels"`
		VKType                      string          `json:"vkType"`
		ExpiresAt                   *time.Time      `json:"expiresAt"`
	}
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("Invalid request body", "validation_error", ""))
	}
	if body.Name == "" {
		return c.JSON(http.StatusBadRequest, errJSON("name is required", "validation_error", ""))
	}

	// /api/admin/virtual-keys is the application-VK admin surface: an omitted
	// vkType defaults to "application". Resolve the EFFECTIVE vkType BEFORE the
	// approval gate so an omitted vkType cannot slip an application key past the
	// maker-checker workflow. Gating on the literal request field while
	// defaulting later let a `virtual-keys:create`-only principal mint an
	// immediately-active application key with no project, bypassing :approve.
	vkType := body.VKType
	if vkType == "" {
		vkType = "application"
	}

	vkStatus := "active"
	if vkType == "application" {
		if body.ProjectID == nil || *body.ProjectID == "" {
			return c.JSON(http.StatusBadRequest, errJSON("projectId is required for application virtual keys", "validation_error", ""))
		}
		if msg := capApplicationExpiry(body.ExpiresAt); msg != "" {
			return c.JSON(http.StatusBadRequest, errJSON(msg, "validation_error", ""))
		}
		vkStatus = "pending"
	}

	aa := middleware.AdminAuthFromContext(c)
	var ownerID *string
	if aa != nil && aa.KeyID != "" {
		ownerID = &aa.KeyID
	}

	rawKey, keyHash, keyPrefix, err := generateVirtualKey()
	if err != nil {
		h.logger.Error("rand.Read for virtual key", "error", err)
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
		KeyVersion:                  auth.CurrentKeyVersion(),
		KeyPrefix:                   keyPrefix,
		ProjectID:                   body.ProjectID,
		SourceApp:                   body.SourceApp,
		Enabled:                     enabled,
		RateLimitRpm:                body.RateLimitRpm,
		CompareEndpointRateLimitRpm: body.CompareEndpointRateLimitRpm,
		AllowedModels:               allowedModels,
		OwnerID:                     ownerID,
		ExpiresAt:                   body.ExpiresAt,
		VKType:                      vkType,
		VKStatus:                    vkStatus,
	})
	if err != nil {
		if msg, ok := vkNameConflict(err); ok {
			return c.JSON(http.StatusConflict, errJSON(msg, "duplicate_name", ""))
		}
		h.logger.Error("create virtual key", "error", err)
		return internalServerError(c, "Internal server error")
	}

	ae := audit.EntryFor(c, iam.ResourceVirtualKey, iam.VerbCreate)
	ae.EntityID = vk.Name
	ae.AfterState = map[string]any{
		"id": vk.ID, "name": vk.Name, "keyPrefix": vk.KeyPrefix,
		"projectId": vk.ProjectID, "sourceApp": vk.SourceApp,
		"enabled": vk.Enabled, "rateLimitRpm": vk.RateLimitRpm,
	}
	h.audit.LogObserved(c.Request().Context(), ae)

	// Return raw key ONCE
	resp := map[string]any{
		"id": vk.ID, "name": vk.Name, "keyPrefix": vk.KeyPrefix,
		"projectId": vk.ProjectID, "sourceApp": vk.SourceApp,
		"enabled": vk.Enabled, "rateLimitRpm": vk.RateLimitRpm,
		"allowedModels": vk.AllowedModels,
		"ownerId":       vk.OwnerID,
		"createdAt":     vk.CreatedAt, "updatedAt": vk.UpdatedAt,
		"key": rawKey,
	}
	return c.JSON(http.StatusCreated, resp)
}

func (h *Handler) UpdateVirtualKey(c echo.Context) error {
	existing, err := h.resolveVK(c)
	if err != nil {
		return internalServerError(c, "Internal server error")
	}
	if existing == nil {
		return c.JSON(http.StatusNotFound, errJSON("Virtual key not found", "not_found", ""))
	}

	aa := middleware.AdminAuthFromContext(c)
	if aa != nil && !h.isSuperAdmin(c, aa) {
		if existing.OwnerID == nil || *existing.OwnerID != aa.KeyID {
			return c.JSON(http.StatusForbidden, errJSON("Access denied", "authorization_error", ""))
		}
	}

	rawBody, readErr := io.ReadAll(c.Request().Body)
	if readErr != nil {
		return c.JSON(http.StatusBadRequest, errJSON("Failed to read request", "validation_error", ""))
	}
	c.Request().Body = io.NopCloser(bytes.NewReader(rawBody))

	var body struct {
		ProjectID                   *string `json:"projectId"`
		SourceApp                   *string `json:"sourceApp"`
		Enabled                     *bool   `json:"enabled"`
		RateLimitRpm                *int    `json:"rateLimitRpm"`
		CompareEndpointRateLimitRpm *int    `json:"compareEndpointRateLimitRpm"`
		AllowedModels               any     `json:"allowedModels"`
	}
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("Invalid request body", "validation_error", ""))
	}

	updateExpiresAt, newExpiresAt, expiresAtErr := extractNullableTimeFromBody(rawBody, "expiresAt")
	if expiresAtErr != "" {
		return c.JSON(http.StatusBadRequest, errJSON(expiresAtErr, "validation_error", ""))
	}

	// Application VKs carry a maker-checker governance cap: their lifetime is
	// bound to 3 months at create (vk.go CreateVirtualKey) and on renewal
	// (approval.go RenewVirtualKey). The general PUT update path must enforce
	// the SAME ceiling — otherwise an edit could set an arbitrarily-far expiry,
	// or clear it to never-expire, and silently escape the re-approval cadence
	// the cap exists to force. The check is
	// scoped to vkType == "application": personal VKs have no cap and may carry
	// any (or no) expiry, so they are intentionally exempt.
	if updateExpiresAt && existing.VKType != nil && *existing.VKType == "application" {
		if msg := capApplicationExpiry(newExpiresAt); msg != "" {
			return c.JSON(http.StatusBadRequest, errJSON(msg, "validation_error", ""))
		}
	}

	params := vkstore.UpdateVirtualKeyParams{
		ProjectID:                   body.ProjectID,
		SourceApp:                   body.SourceApp,
		Enabled:                     body.Enabled,
		RateLimitRpm:                body.RateLimitRpm,
		CompareEndpointRateLimitRpm: body.CompareEndpointRateLimitRpm,
		UpdateExpiresAt:             updateExpiresAt,
		ExpiresAt:                   newExpiresAt,
	}
	if body.AllowedModels != nil {
		raw, _ := json.Marshal(body.AllowedModels)
		params.AllowedModels = raw
	}

	updated, err := h.vks.UpdateVirtualKey(c.Request().Context(), existing.ID, params)
	if err != nil {
		h.logger.Error("update virtual key", "error", err)
		return internalServerError(c, "Internal server error")
	}

	if err := h.notifyVKInvalidate(c, existing.KeyHash); err != nil {
		return hub.RespondPropagationFailure(c, err)
	}

	ae := audit.EntryFor(c, iam.ResourceVirtualKey, iam.VerbUpdate)
	ae.EntityID = updated.Name
	ae.BeforeState = existing
	ae.AfterState = updated
	h.audit.LogObserved(c.Request().Context(), ae)

	return c.JSON(http.StatusOK, updated)
}

func (h *Handler) DeleteVirtualKey(c echo.Context) error {
	existing, err := h.resolveVK(c)
	if err != nil {
		return internalServerError(c, "Internal server error")
	}
	if existing == nil {
		return c.JSON(http.StatusNotFound, errJSON("Virtual key not found", "not_found", ""))
	}

	aa := middleware.AdminAuthFromContext(c)
	if aa != nil && !h.isSuperAdmin(c, aa) {
		if existing.OwnerID == nil || *existing.OwnerID != aa.KeyID {
			return c.JSON(http.StatusForbidden, errJSON("Access denied", "authorization_error", ""))
		}
	}

	if err := h.vks.DeleteVirtualKey(c.Request().Context(), existing.ID); err != nil {
		return internalServerError(c, "Internal server error")
	}

	if err := h.notifyVKInvalidate(c, existing.KeyHash); err != nil {
		return hub.RespondPropagationFailure(c, err)
	}

	ae := audit.EntryFor(c, iam.ResourceVirtualKey, iam.VerbDelete)
	ae.EntityID = existing.Name
	ae.BeforeState = existing
	h.audit.LogObserved(c.Request().Context(), ae)

	return c.JSON(http.StatusOK, map[string]any{"deleted": true, "name": existing.Name})
}

func (h *Handler) RegenerateVirtualKey(c echo.Context) error {
	id := c.Param("id")
	vk, err := h.vks.GetVirtualKey(c.Request().Context(), id)
	if err != nil || vk == nil {
		return c.JSON(http.StatusNotFound, errJSON("Virtual key not found", "not_found", ""))
	}

	aa := middleware.AdminAuthFromContext(c)
	if aa != nil && !h.isSuperAdmin(c, aa) {
		if vk.OwnerID == nil || *vk.OwnerID != aa.KeyID {
			return c.JSON(http.StatusForbidden, errJSON("Access denied", "authorization_error", ""))
		}
	}

	rawKey, keyHash, keyPrefix, err := generateVirtualKey()
	if err != nil {
		h.logger.Error("rand.Read for regenerated virtual key", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to regenerate virtual key", "server_error", ""))
	}
	if err := h.vks.RegenerateVirtualKeyHash(c.Request().Context(), id, keyHash, auth.CurrentKeyVersion(), keyPrefix); err != nil {
		return internalServerError(c, "Internal server error")
	}

	// Invalidate the OLD hash on the data plane so any in-flight or
	// freshly-cached entry for the rotated key is dropped. The new hash
	// is not in cache yet — first /v1 call will load it. Fail loud: if the
	// push fails the gateway keeps accepting the OLD secret from its cache.
	if err := h.notifyVKInvalidate(c, vk.KeyHash); err != nil {
		return hub.RespondPropagationFailure(c, err)
	}

	ae := audit.EntryFor(c, iam.ResourceVirtualKey, iam.VerbUpdate)
	ae.EntityID = id
	ae.AfterState = map[string]any{"regenerated": true}
	h.audit.LogObserved(c.Request().Context(), ae)

	return c.JSON(http.StatusOK, map[string]any{
		"id": id, "key": rawKey, "keyPrefix": keyPrefix,
		"message": "Key regenerated. Save this key — it will not be shown again.",
	})
}

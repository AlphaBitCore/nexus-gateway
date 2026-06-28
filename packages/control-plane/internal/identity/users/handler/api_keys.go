package iam

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authn"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/users/userstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
)

// RegisterAPIKeyRoutes registers admin API key management routes.
func (h *Handler) RegisterAPIKeyRoutes(g *echo.Group, iamMW func(action string) echo.MiddlewareFunc) {
	g.GET("/api-keys", h.ListAPIKeys, iamMW(iam.ResourceApiKey.Action(iam.VerbRead)))
	g.GET("/api-keys/:id", h.GetAPIKey, iamMW(iam.ResourceApiKey.Action(iam.VerbRead)))
	g.POST("/api-keys", h.CreateAPIKey, iamMW(iam.ResourceApiKey.Action(iam.VerbCreate)))
	g.PATCH("/api-keys/:id", h.UpdateAPIKey, iamMW(iam.ResourceApiKey.Action(iam.VerbUpdate)))
	g.DELETE("/api-keys/:id", h.DeleteAPIKey, iamMW(iam.ResourceApiKey.Action(iam.VerbDelete)))
	g.POST("/api-keys/:id/regenerate", h.RegenerateAPIKey, iamMW(iam.ResourceApiKey.Action(iam.VerbUpdate)))
	// Multi-key rotation surface — POST /api-keys/:id/rotate mints a successor
	// and flips the predecessor to 'rotating'. Gated by VerbRotate so an
	// operator can be granted the carve-out without full update rights.
	g.POST("/api-keys/:id/rotate", h.RotateAPIKey, iamMW(iam.ResourceApiKey.Action(iam.VerbRotate)))
	// PUT /api-keys/:id/retire closes the rotation window OR actively revokes
	// a key. Body field `targetStatus` selects 'expired' (natural sunset) or
	// 'unavailable' (compromise / withdrawal). Kept on VerbUpdate — operator
	// who can edit a key can also retire it.
	g.PUT("/api-keys/:id/retire", h.RetireAPIKey, iamMW(iam.ResourceApiKey.Action(iam.VerbUpdate)))
}

func (h *Handler) ListAPIKeys(c echo.Context) error {
	ownerUserId := c.QueryParam("ownerUserId")

	// scope=owned → filter to keys owned by the currently authenticated user
	if c.QueryParam("scope") == "owned" {
		aa := middleware.AdminAuthFromContext(c)
		if aa != nil {
			ownerUserId = aa.KeyID
		}
	}

	keys, err := h.users.ListAdminAPIKeys(c.Request().Context(), ownerUserId)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to list API keys", "server_error", ""))
	}
	return c.JSON(http.StatusOK, map[string]any{"data": keys})
}

func (h *Handler) GetAPIKey(c echo.Context) error {
	k, err := h.users.GetAdminAPIKey(c.Request().Context(), c.Param("id"))
	if err != nil || k == nil {
		return c.JSON(http.StatusNotFound, errJSON("API key not found", "not_found", ""))
	}
	return c.JSON(http.StatusOK, map[string]any{"data": k})
}

func (h *Handler) CreateAPIKey(c echo.Context) error {
	var body struct {
		Name        string  `json:"name"`
		ExpiresAt   string  `json:"expiresAt"`
		OwnerUserID *string `json:"ownerUserId"`
	}
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("Invalid request body", "validation_error", ""))
	}
	if body.Name == "" {
		return c.JSON(http.StatusBadRequest, errJSON("name is required", "validation_error", ""))
	}

	// Generate API key
	rawBytes := make([]byte, 32)
	_, _ = rand.Read(rawBytes)
	rawKey := "nxk_" + hex.EncodeToString(rawBytes)
	keyHash := auth.HashAPIKey(rawKey)
	keyPrefix := rawKey[:12]

	aa := middleware.AdminAuthFromContext(c)
	createdBy := "unknown"
	if aa != nil {
		createdBy = aa.KeyName
		// Default ownerUserId to the authenticated user when not explicitly provided
		if body.OwnerUserID == nil && aa.AuthPrincipalType == "admin_user" {
			body.OwnerUserID = &aa.KeyID
		}
	}

	// Grant ceiling: an admin API key whose ownerUserId is set
	// authenticates AS that owner (authn.EffectivePrincipal). Minting a key for
	// an owner OTHER than the caller would let a narrowly-scoped caller inherit a
	// more-powerful principal's authority (e.g. mint a super-admin-owned key with
	// only admin:api-key.create). Require the caller to already cover every
	// permission the target owner holds before the key is created. When the owner
	// is unset or is the caller's own user id, the ceiling is skipped (behavior
	// unchanged — the key confers nothing the caller does not already have).
	if body.OwnerUserID != nil && *body.OwnerUserID != "" &&
		(aa == nil || *body.OwnerUserID != aa.KeyID) {
		if blocked, resp := h.ceilingBlocksOwner(c, "nexus_user", *body.OwnerUserID); blocked {
			return resp
		}
	}

	var expiresAt *time.Time
	if body.ExpiresAt != "" {
		t, err := time.Parse(time.RFC3339, body.ExpiresAt)
		if err == nil {
			expiresAt = &t
		}
	}

	k, err := h.users.CreateAdminAPIKey(c.Request().Context(), userstore.CreateAdminAPIKeyParams{
		Name:        body.Name,
		KeyHash:     keyHash,
		KeyVersion:  auth.CurrentKeyVersion(),
		KeyPrefix:   keyPrefix,
		CreatedBy:   createdBy,
		ExpiresAt:   expiresAt,
		OwnerUserID: body.OwnerUserID,
	})
	if err != nil {
		h.logger.Error("create api key", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to create API key", "server_error", ""))
	}

	ae := audit.EntryFor(c, iam.ResourceApiKey, iam.VerbCreate)
	ae.EntityID = k.ID
	ae.AfterState = map[string]any{"name": k.Name, "keyPrefix": keyPrefix}
	h.audit.LogObserved(c.Request().Context(), ae)

	return c.JSON(http.StatusCreated, map[string]any{
		"id": k.ID, "name": k.Name, "key": rawKey, "keyPrefix": keyPrefix,
		"ownerUserId": k.OwnerUserID, "expiresAt": k.ExpiresAt, "createdAt": k.CreatedAt,
	})
}

func (h *Handler) UpdateAPIKey(c echo.Context) error {
	id := c.Param("id")
	var body struct {
		Name      *string `json:"name"`
		Enabled   *bool   `json:"enabled"`
		ExpiresAt *string `json:"expiresAt"`
	}
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("Invalid request body", "validation_error", ""))
	}

	params := userstore.UpdateAdminAPIKeyParams{}
	hasUpdate := false
	if body.Name != nil && *body.Name != "" {
		params.Name = body.Name
		hasUpdate = true
	}
	if body.Enabled != nil {
		params.Enabled = body.Enabled
		hasUpdate = true
	}
	if body.ExpiresAt != nil {
		if *body.ExpiresAt == "" {
			// Explicitly clearing expiresAt — pass nil (COALESCE preserves NULL)
			// The store uses COALESCE, so nil means "no change". To clear, we need
			// a sentinel. For now, treat empty string as "no change" consistent
			// with the old behavior of passing nil.
		} else {
			if t, err := time.Parse(time.RFC3339, *body.ExpiresAt); err == nil {
				params.ExpiresAt = &t
				hasUpdate = true
			}
		}
	}

	if !hasUpdate {
		return c.JSON(http.StatusBadRequest, errJSON("No valid fields to update", "validation_error", ""))
	}

	k, err := h.users.UpdateAdminAPIKey(c.Request().Context(), id, params)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to update API key", "server_error", ""))
	}

	ae := audit.EntryFor(c, iam.ResourceApiKey, iam.VerbUpdate)
	ae.EntityID = id
	ae.AfterState = k
	h.audit.LogObserved(c.Request().Context(), ae)

	return c.JSON(http.StatusOK, map[string]any{"data": k})
}

func (h *Handler) RegenerateAPIKey(c echo.Context) error {
	id := c.Param("id")
	k, err := h.users.GetAdminAPIKey(c.Request().Context(), id)
	if err != nil || k == nil {
		return c.JSON(http.StatusNotFound, errJSON("API key not found", "not_found", ""))
	}

	rawBytes := make([]byte, 32)
	_, _ = rand.Read(rawBytes)
	rawKey := "nxk_" + hex.EncodeToString(rawBytes)
	keyHash := auth.HashAPIKey(rawKey)
	keyPrefix := rawKey[:12]

	if err := h.users.RegenerateAdminAPIKey(c.Request().Context(), id, keyHash, auth.CurrentKeyVersion(), keyPrefix); err != nil {
		h.logger.Error("regenerate api key", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to regenerate API key", "server_error", ""))
	}

	ae := audit.EntryFor(c, iam.ResourceApiKey, iam.VerbUpdate)
	ae.EntityID = id
	ae.AfterState = map[string]any{"regenerated": true, "keyPrefix": keyPrefix}
	h.audit.LogObserved(c.Request().Context(), ae)

	return c.JSON(http.StatusOK, map[string]any{
		"id": id, "key": rawKey, "keyPrefix": keyPrefix,
		"message": "Key regenerated. Save this key — it will not be shown again.",
	})
}

func (h *Handler) DeleteAPIKey(c echo.Context) error {
	id := c.Param("id")
	aa := middleware.AdminAuthFromContext(c)
	if aa != nil && aa.AuthPrincipalType == "api_key" && aa.KeyID == id {
		return c.JSON(http.StatusConflict, errJSON("Cannot delete the API key used for this session", "validation_error", "SESSION_KEY_IN_USE"))
	}

	if err := h.users.DeleteAdminAPIKey(c.Request().Context(), id); err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to delete API key", "server_error", ""))
	}

	ae := audit.EntryFor(c, iam.ResourceApiKey, iam.VerbDelete)
	ae.EntityID = id
	h.audit.LogObserved(c.Request().Context(), ae)

	return c.JSON(http.StatusOK, map[string]any{"deleted": true, "id": id})
}

// RotateAPIKey mints a successor key that inherits the predecessor's name +
// owner, marks the predecessor as 'rotating', and returns the successor's
// plaintext value (visible exactly once, mirroring the create / regenerate
// contract). Both keys remain valid until the operator calls retire on the
// predecessor.
//
// Body (optional):
//
//	{ "expiresAt": "RFC3339" }   override the successor's expiry; default is
//	                             to inherit the predecessor's expiry so the
//	                             operator's intended lifetime is preserved.
//
// Failure modes:
//   - 404 — predecessor does not exist
//   - 409 — predecessor is not in 'active' state (e.g. already rotating)
//   - 500 — DB error
func (h *Handler) RotateAPIKey(c echo.Context) error {
	id := c.Param("id")

	var body struct {
		ExpiresAt string `json:"expiresAt"`
	}
	// Empty body is acceptable; Bind on a JSON request with no body yields
	// an EOF-style error which we swallow because all fields are optional.
	_ = c.Bind(&body)

	var newExpiresAt *time.Time
	if body.ExpiresAt != "" {
		t, err := time.Parse(time.RFC3339, body.ExpiresAt)
		if err != nil {
			return c.JSON(http.StatusBadRequest, errJSON("invalid expiresAt; expected RFC3339", "validation_error", ""))
		}
		newExpiresAt = &t
	}

	// Generate the successor key material outside the DB transaction so we
	// never log it on failure (rotate-failure paths must not leak a usable
	// key into the error log).
	rawBytes := make([]byte, 32)
	if _, err := rand.Read(rawBytes); err != nil {
		h.logger.Error("rand.Read for rotated api key", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to rotate API key", "server_error", ""))
	}
	rawKey := "nxk_" + hex.EncodeToString(rawBytes)
	keyHash := auth.HashAPIKey(rawKey)
	keyPrefix := rawKey[:12]

	aa := middleware.AdminAuthFromContext(c)
	createdBy := "unknown"
	if aa != nil {
		createdBy = aa.KeyName
	}

	res, err := h.users.RotateAdminAPIKey(c.Request().Context(), userstore.RotateAdminAPIKeyParams{
		PredecessorID: id,
		NewKeyHash:    keyHash,
		NewKeyVersion: auth.CurrentKeyVersion(),
		NewKeyPrefix:  keyPrefix,
		NewCreatedBy:  createdBy,
		NewExpiresAt:  newExpiresAt,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return c.JSON(http.StatusNotFound, errJSON("API key not found", "not_found", ""))
	}
	if err != nil {
		h.logger.Error("rotate api key", "error", err, "predecessorId", id)
		// Predecessor in a non-active state is the only "validation"-shaped
		// failure path that bubbles back here; everything else is genuinely
		// 500-class. Match on the message because the store helper returns
		// a plain error rather than a typed sentinel.
		if err.Error() != "" && (containsStatusInvariantHint(err.Error())) {
			return c.JSON(http.StatusConflict, errJSON(err.Error(), "validation_error", "ROTATE_INVALID_STATE"))
		}
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to rotate API key", "server_error", ""))
	}

	ae := audit.EntryFor(c, iam.ResourceApiKey, iam.VerbRotate)
	ae.EntityID = res.Successor.ID
	ae.BeforeState = map[string]any{
		"predecessorId":     res.Predecessor.ID,
		"predecessorStatus": "active",
	}
	ae.AfterState = map[string]any{
		"predecessorId":      res.Predecessor.ID,
		"predecessorStatus":  res.Predecessor.Status,
		"successorId":        res.Successor.ID,
		"successorKeyPrefix": res.Successor.KeyPrefix,
		"successorExpiresAt": res.Successor.ExpiresAt,
	}
	h.audit.LogObserved(c.Request().Context(), ae)

	return c.JSON(http.StatusCreated, map[string]any{
		"id":        res.Successor.ID,
		"key":       rawKey,
		"keyPrefix": res.Successor.KeyPrefix,
		"expiresAt": res.Successor.ExpiresAt,
		"predecessor": map[string]any{
			"id":        res.Predecessor.ID,
			"status":    res.Predecessor.Status,
			"rotatedAt": res.Predecessor.RotatedAt,
		},
		"message": "Key rotated. The predecessor remains valid until you retire it. Save the new key — it will not be shown again.",
	})
}

// RetireAPIKey closes the rotation window OR actively revokes a key. The
// caller picks the target status:
//   - targetStatus = "expired"     — natural sunset (operator confirms the
//     rotation handoff is complete)
//   - targetStatus = "unavailable" — active revocation (compromise, key
//     leaked, withdrawn from service)
//
// After this call the key is no longer accepted by the auth middleware.
//
// Failure modes:
//   - 400 — invalid targetStatus
//   - 404 — key does not exist
//   - 409 — key is already in a terminal state (expired / unavailable)
//   - 500 — DB error
func (h *Handler) RetireAPIKey(c echo.Context) error {
	id := c.Param("id")
	aa := middleware.AdminAuthFromContext(c)
	if aa != nil && aa.AuthPrincipalType == "api_key" && aa.KeyID == id {
		return c.JSON(http.StatusConflict, errJSON("Cannot retire the API key used for this session", "validation_error", "SESSION_KEY_IN_USE"))
	}

	var body struct {
		TargetStatus string `json:"targetStatus"`
	}
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("Invalid request body", "validation_error", ""))
	}
	if body.TargetStatus == "" {
		body.TargetStatus = userstore.AdminAPIKeyStatusExpired
	}
	if body.TargetStatus != userstore.AdminAPIKeyStatusExpired &&
		body.TargetStatus != userstore.AdminAPIKeyStatusUnavailable {
		return c.JSON(http.StatusBadRequest, errJSON(
			"targetStatus must be 'expired' or 'unavailable'",
			"validation_error", "INVALID_TARGET_STATUS"))
	}

	before, _ := h.users.GetAdminAPIKey(c.Request().Context(), id)
	k, err := h.users.RetireAdminAPIKey(c.Request().Context(), id, body.TargetStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return c.JSON(http.StatusNotFound, errJSON("API key not found", "not_found", ""))
	}
	if err != nil {
		// Already-terminal-state is the documented 409 path; treat any other
		// error string as a 500.
		if containsAlreadyRetiredHint(err.Error()) {
			return c.JSON(http.StatusConflict, errJSON(err.Error(), "validation_error", "ALREADY_RETIRED"))
		}
		h.logger.Error("retire api key", "error", err, "id", id, "targetStatus", body.TargetStatus)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to retire API key", "server_error", ""))
	}

	ae := audit.EntryFor(c, iam.ResourceApiKey, iam.VerbUpdate)
	ae.EntityID = id
	if before != nil {
		ae.BeforeState = map[string]any{"status": before.Status}
	}
	ae.AfterState = map[string]any{"status": k.Status}
	h.audit.LogObserved(c.Request().Context(), ae)

	return c.JSON(http.StatusOK, map[string]any{"data": k})
}

// containsStatusInvariantHint returns true when the error message indicates
// that RotateAdminAPIKey rejected the predecessor because it was not in
// 'active' state. Matching on the helper's message string keeps the store
// helper itself untyped (it returns plain errors today); if we ever switch
// to typed sentinels this helper goes away.
func containsStatusInvariantHint(msg string) bool {
	// Mirror the format string in RotateAdminAPIKey — keep the substring
	// match deliberately narrow so unrelated DB errors do not accidentally
	// downgrade to 409.
	return msg != "" && strings.Contains(msg, "only active keys can be rotated")
}

// containsAlreadyRetiredHint returns true when the error message indicates
// RetireAdminAPIKey was called on a key that is already expired or unavailable.
func containsAlreadyRetiredHint(msg string) bool {
	return msg != "" && strings.Contains(msg, "already expired or unavailable")
}

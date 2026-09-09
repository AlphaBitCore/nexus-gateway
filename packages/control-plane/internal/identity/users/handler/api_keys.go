package iam

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"

	auth "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authn"
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
	// err and a nil key mean DIFFERENT things and the store already separates
	// them: GetAdminAPIKey returns (nil, nil) for no rows and (nil, err) for a
	// read failure. Folding them into one 404 tells a caller their key is gone
	// when the database is merely unreachable — the one answer that makes them
	// stop retrying and start re-provisioning.
	k, err := h.users.GetAdminAPIKey(c.Request().Context(), c.Param("id"))
	if err != nil {
		h.logger.Error("get api key", "error", err, "id", c.Param("id"))
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to read API key", "server_error", ""))
	}
	if k == nil {
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

	// Grant ceiling: an admin API key whose ownerUserId is set authenticates AS
	// that owner. The skip conditions live inside ceilingBlocksKeyOwner so all
	// three minting paths share one spelling of them.
	if blocked, resp := h.ceilingBlocksKeyOwner(c, body.OwnerUserID); blocked {
		return resp
	}

	expiresAt, ok, err := parseGrantExpiry(c, body.ExpiresAt)
	if !ok {
		return err
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
		if msg, code, status := apiKeyWriteConstraintError(err); status != 0 {
			return c.JSON(status, errJSON(msg, "validation_error", code))
		}
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
	// `"expiresAt": ""` is a no-op, not a clear: the store UPDATE is a COALESCE,
	// so nil means "leave the column alone" and there is no value that means
	// "set it back to NULL". Removing an expiry is therefore not something this
	// endpoint offers — which is the safer direction for a credential, and the
	// direction the other expiry-bearing endpoints take too.
	if body.ExpiresAt != nil && *body.ExpiresAt != "" {
		t, ok, err := parseGrantExpiry(c, *body.ExpiresAt)
		if !ok {
			return err
		}
		params.ExpiresAt = t
		hasUpdate = true
	}

	if !hasUpdate {
		return c.JSON(http.StatusBadRequest, errJSON("No valid fields to update", "validation_error", ""))
	}

	k, err := h.users.UpdateAdminAPIKey(c.Request().Context(), id, params)
	if errors.Is(err, pgx.ErrNoRows) {
		// The store's UPDATE ... RETURNING matches no row for an id that is
		// gone, which is the caller naming something that does not exist.
		return c.JSON(http.StatusNotFound, errJSON("API key not found", "not_found", ""))
	}
	if err != nil {
		if msg, code, status := apiKeyWriteConstraintError(err); status != 0 {
			return c.JSON(status, errJSON(msg, "validation_error", code))
		}
		h.logger.Error("update api key", "error", err, "id", id)
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
	if err != nil {
		h.logger.Error("regenerate api key: owner lookup", "error", err, "id", id)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to read API key", "server_error", ""))
	}
	if k == nil {
		return c.JSON(http.StatusNotFound, errJSON("API key not found", "not_found", ""))
	}

	// Regenerating mints a fresh plaintext credential for the SAME owner, so it
	// hands out exactly what create's ceiling exists to withhold.
	if blocked, resp := h.ceilingBlocksKeyOwner(c, k.OwnerUserID); blocked {
		return resp
	}

	rawBytes := make([]byte, 32)
	_, _ = rand.Read(rawBytes)
	rawKey := "nxk_" + hex.EncodeToString(rawBytes)
	keyHash := auth.HashAPIKey(rawKey)
	keyPrefix := rawKey[:12]

	if err := h.users.RegenerateAdminAPIKey(c.Request().Context(), id, keyHash, auth.CurrentKeyVersion(), keyPrefix); err != nil {
		if msg, code, status := apiKeyWriteConstraintError(err); status != 0 {
			return c.JSON(status, errJSON(msg, "validation_error", code))
		}
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
		// The store already tells us which of the two this is: it returns
		// pgx.ErrNoRows when nothing matched. Collapsing that into a 500 threw
		// away an answer we had been handed, and reported a key that is
		// already gone as a server fault — the caller then cannot tell "your
		// id is stale" from "the admin API is broken".
		//
		// Every other read in this file already spells the 404; delete was the
		// one that skipped it.
		if errors.Is(err, pgx.ErrNoRows) {
			return c.JSON(http.StatusNotFound, errJSON("API key not found", "not_found", ""))
		}
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

	newExpiresAt, ok, err := parseGrantExpiry(c, body.ExpiresAt)
	if !ok {
		return err
	}

	// The successor inherits the predecessor's owner, so rotate mints a usable
	// credential for that principal exactly as create and regenerate do. The
	// predecessor is loaded here rather than trusted from the path, because the
	// owner is the input the ceiling evaluates. A missing key is refused with the
	// same 404 the rotate store call would have produced.
	pred, perr := h.users.GetAdminAPIKey(c.Request().Context(), id)
	if perr != nil {
		h.logger.Error("rotate api key: predecessor lookup", "error", perr, "id", id)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to read API key", "server_error", ""))
	}
	if pred == nil {
		return c.JSON(http.StatusNotFound, errJSON("API key not found", "not_found", ""))
	}
	if blocked, resp := h.ceilingBlocksKeyOwner(c, pred.OwnerUserID); blocked {
		return resp
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
		if msg, code, status := apiKeyWriteConstraintError(err); status != 0 {
			return c.JSON(status, errJSON(msg, "validation_error", code))
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

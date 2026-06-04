package iam

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/revocation"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/idptest"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/scim/scimstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
)

// fanOutIdPRevocations issues scope=user revocations for every user linked to
// the given IdP and purges their refresh-token rows. Best-effort: failures are
// logged via revokeUserScope's own error path and never roll back the parent
// admin mutation.
//
// Called from UpdateIdentityProvider on an enabled→disabled transition and from
// DeleteIdentityProvider on force=true cascade. Revocation propagates via MQ to
// AI Gateway + Compliance Proxy verifiers within ~5 seconds.
func (h *Handler) fanOutIdPRevocations(ctx context.Context, userIDs []string) {
	for _, uid := range userIDs {
		h.revokeUserScope(ctx, uid, revocation.ReasonIdPDisable)
	}
}

// sensitiveMaskIdP is the placeholder value the API uses in place of
// secret fields on read. The same constant is recognised on write as
// "leave unchanged" so the UI can round-trip GET → PUT without ever
// holding the real secret in browser memory.
const sensitiveMaskIdP = "********"

// idpSecretFields enumerates per-protocol keys inside `config` that hold
// secrets — masked on read, "leave unchanged" on write.
var idpSecretFields = map[string][]string{
	"oidc": {"clientSecret"},
	"saml": {"certificatePem"},
}

// RegisterIdentityProviderRoutes registers admin IdP CRUD + probe routes,
// plus SCIM-token and group-mapping subresources.
// The IdP page is the canonical home for external IdP configuration
// (Okta, Azure AD, Google Workspace, etc.).
func (h *Handler) RegisterIdentityProviderRoutes(g *echo.Group, iamMW func(action string) echo.MiddlewareFunc) {
	idp := iam.ResourceIdentityProvider

	// IdP CRUD + probe.
	g.GET("/identity-providers", h.ListIdentityProviders, iamMW(idp.Action(iam.VerbRead)))
	g.POST("/identity-providers", h.CreateIdentityProvider, iamMW(idp.Action(iam.VerbCreate)))
	g.POST("/identity-providers/test", h.TestCandidateIdentityProvider, iamMW(idp.Action(iam.VerbProbe)))
	g.GET("/identity-providers/:idpId", h.GetIdentityProvider, iamMW(idp.Action(iam.VerbRead)))
	g.PUT("/identity-providers/:idpId", h.UpdateIdentityProvider, iamMW(idp.Action(iam.VerbUpdate)))
	g.DELETE("/identity-providers/:idpId", h.DeleteIdentityProvider, iamMW(idp.Action(iam.VerbDelete)))
	g.POST("/identity-providers/:idpId/test", h.TestSavedIdentityProvider, iamMW(idp.Action(iam.VerbProbe)))

	// SCIM tokens — IdP-scoped subresource. Gated by IdP update because
	// generating a SCIM token effectively grants provisioning authority
	// to the IdP it's attached to.
	g.GET("/identity-providers/:idpId/scim-tokens", h.ListScimTokens, iamMW(idp.Action(iam.VerbUpdate)))
	g.POST("/identity-providers/:idpId/scim-tokens", h.CreateScimToken, iamMW(idp.Action(iam.VerbUpdate)))
	g.DELETE("/identity-providers/:idpId/scim-tokens/:tokenId", h.RevokeScimToken, iamMW(idp.Action(iam.VerbUpdate)))

	// IdP group → IamGroup mappings.
	g.GET("/identity-providers/:idpId/group-mappings", h.ListIdpGroupMappings, iamMW(idp.Action(iam.VerbUpdate)))
	g.POST("/identity-providers/:idpId/group-mappings", h.CreateIdpGroupMapping, iamMW(idp.Action(iam.VerbUpdate)))
	g.DELETE("/identity-providers/:idpId/group-mappings/:mappingId", h.DeleteIdpGroupMapping, iamMW(idp.Action(iam.VerbUpdate)))
}

// idpResponse is the JSON shape returned by list/get/create/update.
// Config is included with secrets masked. The seed `local` row's Type
// is "local"; the UI filters it out of the list view but the API still
// returns it so power users can see the fallback exists.
type idpResponse struct {
	ID          string         `json:"id"`
	Type        string         `json:"type"`
	Name        string         `json:"name"`
	Enabled     bool           `json:"enabled"`
	Config      map[string]any `json:"config"`
	RoleMapping any            `json:"roleMapping"`
	DefaultRole string         `json:"defaultRole"`
	JITEnabled  bool           `json:"jitEnabled"`
	CreatedAt   time.Time      `json:"createdAt"`
	UpdatedAt   time.Time      `json:"updatedAt"`
}

// toIdpResponse converts a store record to the API response shape with
// secrets masked.
func toIdpResponse(r *scimstore.IdentityProviderRecord) idpResponse {
	var cfg map[string]any
	if len(r.Config) > 0 {
		_ = json.Unmarshal(r.Config, &cfg)
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	for _, k := range idpSecretFields[r.Type] {
		if v, ok := cfg[k].(string); ok && v != "" {
			cfg[k] = sensitiveMaskIdP
		}
	}
	var roleMapping any
	if len(r.RoleMapping) > 0 {
		_ = json.Unmarshal(r.RoleMapping, &roleMapping)
	}
	if roleMapping == nil {
		roleMapping = []any{}
	}
	return idpResponse{
		ID:          r.ID,
		Type:        r.Type,
		Name:        r.Name,
		Enabled:     r.Enabled,
		Config:      cfg,
		RoleMapping: roleMapping,
		DefaultRole: r.DefaultRole,
		JITEnabled:  r.JITEnabled,
		CreatedAt:   r.CreatedAt,
		UpdatedAt:   r.UpdatedAt,
	}
}

// GET /api/admin/identity-providers
func (h *Handler) ListIdentityProviders(c echo.Context) error {
	idps, err := h.scim.ListIdentityProviders(c.Request().Context())
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}
	out := make([]idpResponse, 0, len(idps))
	for i := range idps {
		out = append(out, toIdpResponse(&idps[i]))
	}
	return c.JSON(http.StatusOK, map[string]any{"data": out, "total": len(out)})
}

// GET /api/admin/identity-providers/:idpId
func (h *Handler) GetIdentityProvider(c echo.Context) error {
	idp, err := h.scim.GetIdentityProvider(c.Request().Context(), c.Param("idpId"))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return c.JSON(http.StatusNotFound, errJSON("Identity provider not found", "not_found", ""))
		}
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}
	return c.JSON(http.StatusOK, toIdpResponse(idp))
}

// idpWriteRequest is the body shape for POST/PUT. `Type` must be "oidc"
// or "saml" — creating a "local" row through this surface is disallowed
// (the seed row is platform-owned).
type idpWriteRequest struct {
	Type        string         `json:"type"`
	Name        string         `json:"name"`
	Enabled     *bool          `json:"enabled,omitempty"`
	Config      map[string]any `json:"config"`
	RoleMapping any            `json:"roleMapping,omitempty"`
	DefaultRole string         `json:"defaultRole,omitempty"`
	JITEnabled  *bool          `json:"jitEnabled,omitempty"`
}

// validateIdPRequest returns an error message if the body is malformed.
func validateIdPRequest(body *idpWriteRequest, isCreate bool) string {
	body.Type = strings.ToLower(strings.TrimSpace(body.Type))
	if body.Type != "oidc" && body.Type != "saml" {
		return "type must be \"oidc\" or \"saml\""
	}
	if strings.TrimSpace(body.Name) == "" {
		return "name is required"
	}
	if body.Config == nil {
		return "config is required"
	}
	if body.Type == "oidc" {
		issuer, _ := body.Config["issuer"].(string)
		clientID, _ := body.Config["clientId"].(string)
		redirectURI, _ := body.Config["redirectUri"].(string)
		if issuer == "" || clientID == "" || redirectURI == "" {
			return "OIDC config requires issuer, clientId, redirectUri"
		}
		if isCreate {
			if cs, _ := body.Config["clientSecret"].(string); cs == "" || cs == sensitiveMaskIdP {
				return "OIDC config requires clientSecret on create"
			}
		}
	}
	if body.Type == "saml" {
		entityID, _ := body.Config["entityId"].(string)
		ssoURL, _ := body.Config["ssoUrl"].(string)
		if entityID == "" || ssoURL == "" {
			return "SAML config requires entityId, ssoUrl"
		}
		if isCreate {
			if cert, _ := body.Config["certificatePem"].(string); cert == "" || cert == sensitiveMaskIdP {
				return "SAML config requires certificatePem on create"
			}
		}
	}
	return ""
}

// mergeMaskedSecrets restores any field equal to the sensitive-mask
// placeholder from the existing config — used on PUT so the UI can
// round-trip without ever holding the cleartext.
func mergeMaskedSecrets(incoming, existing map[string]any, idpType string) {
	for _, k := range idpSecretFields[idpType] {
		if v, ok := incoming[k].(string); ok && v == sensitiveMaskIdP {
			if old, ok := existing[k].(string); ok {
				incoming[k] = old
			} else {
				delete(incoming, k)
			}
		}
	}
}

// POST /api/admin/identity-providers
func (h *Handler) CreateIdentityProvider(c echo.Context) error {
	var body idpWriteRequest
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("Invalid request body", "validation_error", ""))
	}
	if msg := validateIdPRequest(&body, true); msg != "" {
		return c.JSON(http.StatusBadRequest, errJSON(msg, "validation_error", ""))
	}

	enabled := true
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	jit := true
	if body.JITEnabled != nil {
		jit = *body.JITEnabled
	}

	cfgBytes, _ := json.Marshal(body.Config)
	roleMapBytes := []byte(`[]`)
	if body.RoleMapping != nil {
		if b, err := json.Marshal(body.RoleMapping); err == nil {
			roleMapBytes = b
		}
	}

	r, err := h.scim.CreateIdentityProvider(c.Request().Context(), scimstore.CreateIdentityProviderParams{
		Type:        body.Type,
		Name:        body.Name,
		Enabled:     enabled,
		Config:      cfgBytes,
		RoleMapping: roleMapBytes,
		DefaultRole: body.DefaultRole,
		JITEnabled:  jit,
	})
	if err != nil {
		h.logger.Error("create identity provider", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to create identity provider", "server_error", ""))
	}

	ae := audit.EntryFor(c, iam.ResourceIdentityProvider, iam.VerbCreate)
	ae.EntityID = r.ID
	ae.AfterState = map[string]any{
		"type": r.Type, "name": r.Name, "enabled": r.Enabled,
		"jitEnabled": r.JITEnabled, "defaultRole": r.DefaultRole,
	}
	h.audit.LogObserved(c.Request().Context(), ae)

	return c.JSON(http.StatusCreated, toIdpResponse(r))
}

// PUT /api/admin/identity-providers/:idpId
func (h *Handler) UpdateIdentityProvider(c echo.Context) error {
	idpID := c.Param("idpId")
	existing, err := h.scim.GetIdentityProvider(c.Request().Context(), idpID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return c.JSON(http.StatusNotFound, errJSON("Identity provider not found", "not_found", ""))
		}
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}
	if existing.Type == "local" {
		return c.JSON(http.StatusForbidden, errJSON("The built-in local identity store is not editable", "forbidden", ""))
	}

	var body idpWriteRequest
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("Invalid request body", "validation_error", ""))
	}
	if msg := validateIdPRequest(&body, false); msg != "" {
		return c.JSON(http.StatusBadRequest, errJSON(msg, "validation_error", ""))
	}

	// Restore any masked secret fields from the existing config so the
	// caller can omit (or send "********") for unchanged credentials.
	var existingCfg map[string]any
	if len(existing.Config) > 0 {
		_ = json.Unmarshal(existing.Config, &existingCfg)
	}
	mergeMaskedSecrets(body.Config, existingCfg, body.Type)

	enabled := existing.Enabled
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	jit := existing.JITEnabled
	if body.JITEnabled != nil {
		jit = *body.JITEnabled
	}
	defaultRole := body.DefaultRole
	if defaultRole == "" {
		defaultRole = existing.DefaultRole
	}

	cfgBytes, _ := json.Marshal(body.Config)
	roleMapBytes := existing.RoleMapping
	if body.RoleMapping != nil {
		if b, err := json.Marshal(body.RoleMapping); err == nil {
			roleMapBytes = b
		}
	}

	beforeSnapshot := map[string]any{
		"type": existing.Type, "name": existing.Name, "enabled": existing.Enabled,
		"jitEnabled": existing.JITEnabled, "defaultRole": existing.DefaultRole,
	}

	ctx := c.Request().Context()
	r, err := h.scim.UpdateIdentityProvider(ctx, scimstore.UpdateIdentityProviderParams{
		ID:          idpID,
		Type:        body.Type,
		Name:        body.Name,
		Enabled:     enabled,
		Config:      cfgBytes,
		RoleMapping: roleMapBytes,
		DefaultRole: defaultRole,
		JITEnabled:  jit,
	})
	if err != nil {
		h.logger.Error("update identity provider", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to update identity provider", "server_error", ""))
	}

	// Enabled true→false transition: every user linked to this IdP loses
	// their active sessions. SDD NFR-3 requires this fan-out within ~5s.
	// Best-effort; failures are logged inside revokeUserScope.
	if existing.Enabled && !r.Enabled {
		userIDs, listErr := h.fed.ListUserIDsByIdP(ctx, idpID)
		if listErr != nil {
			h.logger.Error("update identity provider: snapshot users for revocation",
				"idp_id", idpID, "error", listErr)
		} else if len(userIDs) > 0 {
			h.fanOutIdPRevocations(ctx, userIDs)
		}
	}

	ae := audit.EntryFor(c, iam.ResourceIdentityProvider, iam.VerbUpdate)
	ae.EntityID = r.ID
	ae.BeforeState = beforeSnapshot
	ae.AfterState = map[string]any{
		"type": r.Type, "name": r.Name, "enabled": r.Enabled,
		"jitEnabled": r.JITEnabled, "defaultRole": r.DefaultRole,
	}
	h.audit.LogObserved(ctx, ae)

	return c.JSON(http.StatusOK, toIdpResponse(r))
}

// DELETE /api/admin/identity-providers/:idpId
//
// Refuses with 409 if `UserFederatedIdentity` rows are linked to this
// IdP unless `?force=true` is passed. With force, pre-deletes the
// federated-identity rows, revokes scoped SCIM tokens, and cascades
// IdpGroupMappings (FK cascade).
func (h *Handler) DeleteIdentityProvider(c echo.Context) error {
	idpID := c.Param("idpId")
	ctx := c.Request().Context()

	existing, err := h.scim.GetIdentityProvider(ctx, idpID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return c.JSON(http.StatusNotFound, errJSON("Identity provider not found", "not_found", ""))
		}
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}
	if existing.Type == "local" {
		return c.JSON(http.StatusForbidden, errJSON("The built-in local identity store cannot be deleted", "forbidden", ""))
	}

	force, _ := strconv.ParseBool(c.QueryParam("force"))

	// Snapshot linked user IDs BEFORE the cascade — once
	// UserFederatedIdentity rows are gone the link is unreachable.
	// Used for the post-delete revocation fan-out (SDD NFR-3).
	var revokeUserIDs []string
	if !force {
		linked, err := h.scim.CountFederatedIdentitiesForIdP(ctx, idpID)
		if err != nil {
			return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
		}
		if linked > 0 {
			return c.JSON(http.StatusConflict, errJSON(
				"Identity provider has linked users — pass ?force=true to cascade",
				"linked_users", strconv.Itoa(linked),
			))
		}
	} else {
		ids, listErr := h.fed.ListUserIDsByIdP(ctx, idpID)
		if listErr != nil {
			h.logger.Error("delete identity provider: snapshot users for revocation",
				"idp_id", idpID, "error", listErr)
		} else {
			revokeUserIDs = ids
		}
	}

	if err := h.scim.DeleteIdentityProvider(ctx, idpID, force); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return c.JSON(http.StatusNotFound, errJSON("Identity provider not found", "not_found", ""))
		}
		h.logger.Error("delete identity provider", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to delete identity provider", "server_error", ""))
	}

	// Fire revocations AFTER the delete tx commits so any RS-side
	// verifier that races us reads the IdP-gone state, not a half-
	// state with the IdP still present.
	if len(revokeUserIDs) > 0 {
		h.fanOutIdPRevocations(ctx, revokeUserIDs)
	}

	ae := audit.EntryFor(c, iam.ResourceIdentityProvider, iam.VerbDelete)
	ae.EntityID = idpID
	ae.BeforeState = map[string]any{
		"type": existing.Type, "name": existing.Name, "enabled": existing.Enabled,
		"forceCascade": force,
	}
	h.audit.LogObserved(ctx, ae)

	return c.NoContent(http.StatusNoContent)
}

// POST /api/admin/identity-providers/test
//
// Probe a candidate (unsaved) IdP config. Body is the same shape as
// CreateIdentityProvider. Returns `idptest.Result` verbatim. Audited
// with VerbProbe.
func (h *Handler) TestCandidateIdentityProvider(c echo.Context) error {
	var body idpWriteRequest
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("Invalid request body", "validation_error", ""))
	}
	if msg := validateIdPRequest(&body, true); msg != "" {
		return c.JSON(http.StatusBadRequest, errJSON(msg, "validation_error", ""))
	}
	result, err := idptest.Probe(c.Request().Context(), body.Type, body.Config)
	if err != nil {
		return c.JSON(http.StatusBadRequest, errJSON(err.Error(), "validation_error", ""))
	}
	ae := audit.EntryFor(c, iam.ResourceIdentityProvider, iam.VerbProbe)
	ae.AfterState = map[string]any{"candidate": true, "type": body.Type, "ok": result.OK}
	h.audit.LogObserved(c.Request().Context(), ae)
	return c.JSON(http.StatusOK, result)
}

// POST /api/admin/identity-providers/:idpId/test
//
// Probe a saved IdP. Body may be `{token: <jwt>}` to validate a JWT
// against the IdP's JWKS instead — useful for debugging an OIDC sign-in
// chain. Returns `idptest.Result`.
func (h *Handler) TestSavedIdentityProvider(c echo.Context) error {
	idpID := c.Param("idpId")
	existing, err := h.scim.GetIdentityProvider(c.Request().Context(), idpID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return c.JSON(http.StatusNotFound, errJSON("Identity provider not found", "not_found", ""))
		}
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}
	if existing.Type == "local" {
		return c.JSON(http.StatusBadRequest, errJSON("Local identity store does not support probe", "validation_error", ""))
	}
	var cfg map[string]any
	if len(existing.Config) > 0 {
		_ = json.Unmarshal(existing.Config, &cfg)
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	result, err := idptest.Probe(c.Request().Context(), existing.Type, cfg)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON(err.Error(), "server_error", ""))
	}
	ae := audit.EntryFor(c, iam.ResourceIdentityProvider, iam.VerbProbe)
	ae.EntityID = idpID
	ae.AfterState = map[string]any{"type": existing.Type, "ok": result.OK}
	h.audit.LogObserved(c.Request().Context(), ae)
	return c.JSON(http.StatusOK, result)
}

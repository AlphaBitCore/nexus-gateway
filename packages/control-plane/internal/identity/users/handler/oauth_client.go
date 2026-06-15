package iam

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/labstack/echo/v4"
	"golang.org/x/crypto/bcrypt"

	authstore "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
)

// iamOAuthClientStore is the narrow surface the OAuth-client admin endpoints
// need from the authserver ClientStore. Defined here so tests can drop in an
// in-memory fake without touching the runtime store.
type iamOAuthClientStore interface {
	List(ctx context.Context) ([]*authstore.OAuthClient, error)
	GetByID(ctx context.Context, id string) (*authstore.OAuthClient, error)
	Create(ctx context.Context, in authstore.CreateInput) (*authstore.OAuthClient, error)
	Update(ctx context.Context, id string, in authstore.UpdateInput) (*authstore.OAuthClient, error)
	Delete(ctx context.Context, id string) error
	RotateSecret(ctx context.Context, id string, newHash []byte) (*authstore.OAuthClient, error)
	CountActiveRefreshTokens(ctx context.Context, clientID string) (int, error)
}

// RegisterOAuthClientRoutes registers admin OAuth client CRUD + rotate routes.
// IAM gating uses the dedicated oauth-client resource verbs (see catalog_data.go).
func (h *Handler) RegisterOAuthClientRoutes(g *echo.Group, iamMW func(action string) echo.MiddlewareFunc) {
	g.GET("/oauth-clients", h.ListOAuthClients, iamMW(iam.ResourceOAuthClient.Action(iam.VerbRead)))
	g.GET("/oauth-clients/:id", h.GetOAuthClient, iamMW(iam.ResourceOAuthClient.Action(iam.VerbRead)))
	g.POST("/oauth-clients", h.CreateOAuthClient, iamMW(iam.ResourceOAuthClient.Action(iam.VerbCreate)))
	g.PATCH("/oauth-clients/:id", h.UpdateOAuthClient, iamMW(iam.ResourceOAuthClient.Action(iam.VerbUpdate)))
	// Rotate is a sibling of the api-key rotate carve-out — gated separately
	// so an operator can be granted "rotate compromised secret" without full
	// update rights.
	g.POST("/oauth-clients/:id/rotate-secret", h.RotateOAuthClientSecret, iamMW(iam.ResourceOAuthClient.Action(iam.VerbRotate)))
	g.DELETE("/oauth-clients/:id", h.DeleteOAuthClient, iamMW(iam.ResourceOAuthClient.Action(iam.VerbDelete)))
}

// oauthClientSecretPrefix tags the plaintext client secret so admins can
// recognise it in their secret vaults / CI environments. The bcrypt hash
// stored at rest is over the prefixed string so /token verification matches.
const oauthClientSecretPrefix = "nx_cs_"

// idRegex enforces the kebab-case shape used by the seeded clients
// (agent-desktop / cp-ui) so operator-side configs read naturally.
var idRegex = regexp.MustCompile(`^[a-z][a-z0-9-]{2,63}$`)

// scopeRegex constrains each allowedScopes entry to a lowercase token without
// whitespace, mirroring OAuth scope syntax (RFC 6749 §3.3 scope-token).
var scopeRegex = regexp.MustCompile(`^[a-z0-9_:./-]{1,100}$`)

// ListOAuthClients returns every registered client. clientSecretHash is never
// in the response — the store layer reads it but the handler omits it.
func (h *Handler) ListOAuthClients(c echo.Context) error {
	rows, err := h.oauth.List(c.Request().Context())
	if err != nil {
		h.logger.Error("oauth-client: list failed", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to list OAuth clients", "server_error", ""))
	}
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		out = append(out, oauthClientPublic(r))
	}
	return c.JSON(http.StatusOK, map[string]any{"data": out})
}

// GetOAuthClient returns one client and its active-refresh-token count so the
// detail page can render the Activity card without a second round-trip.
func (h *Handler) GetOAuthClient(c echo.Context) error {
	ctx := c.Request().Context()
	id := c.Param("id")
	row, err := h.oauth.GetByID(ctx, id)
	if errors.Is(err, authstore.ErrClientNotFound) {
		return c.JSON(http.StatusNotFound, errJSON("OAuth client not found", "not_found", ""))
	}
	if err != nil {
		h.logger.Error("oauth-client: get failed", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to load OAuth client", "server_error", ""))
	}
	count, err := h.oauth.CountActiveRefreshTokens(ctx, id)
	if err != nil {
		// Activity card degrades to "unknown" rather than blocking the page —
		// log + return 0 so the detail view still renders.
		h.logger.Warn("oauth-client: refresh-token count failed", "error", err, "id", id)
		count = 0
	}
	body := oauthClientPublic(row)
	body["activeRefreshTokenCount"] = count
	return c.JSON(http.StatusOK, map[string]any{"data": body})
}

// oauthClientCreateBody captures the incoming JSON for POST /oauth-clients.
// Defaults mirror the spec's "sensible defaults" table; the validator fills
// them when the caller omits the field.
type oauthClientCreateBody struct {
	ID                string   `json:"id"`
	Name              string   `json:"name"`
	Type              string   `json:"type"`
	RedirectURIs      []string `json:"redirectUris"`
	AllowedScopes     []string `json:"allowedScopes"`
	AccessTTLSeconds  *int     `json:"accessTtlSeconds"`
	RefreshTTLSeconds *int     `json:"refreshTtlSeconds"`
}

// CreateOAuthClient inserts a new client. The plaintext secret is returned
// in the 201 body exactly once for confidential clients; public clients get
// no secret at all.
func (h *Handler) CreateOAuthClient(c echo.Context) error {
	ctx := c.Request().Context()
	var body oauthClientCreateBody
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("Invalid request body", "validation_error", ""))
	}
	body.applyDefaults()
	if msg := body.validate(); msg != "" {
		return c.JSON(http.StatusBadRequest, errJSON(msg, "validation_error", ""))
	}

	in := authstore.CreateInput{
		ID:                body.ID,
		Name:              body.Name,
		Type:              body.Type,
		RedirectURIs:      body.RedirectURIs,
		AllowedScopes:     body.AllowedScopes,
		AccessTTLSeconds:  *body.AccessTTLSeconds,
		RefreshTTLSeconds: *body.RefreshTTLSeconds,
	}

	var plaintextSecret string
	if body.Type == "confidential" {
		plain, hash, err := generateClientSecret()
		if err != nil {
			h.logger.Error("oauth-client: secret generation failed", "error", err)
			return c.JSON(http.StatusInternalServerError, errJSON("Failed to generate client secret", "server_error", ""))
		}
		plaintextSecret = plain
		hashStr := string(hash)
		in.SecretHash = &hashStr
	}

	row, err := h.oauth.Create(ctx, in)
	if errors.Is(err, authstore.ErrClientIDExists) {
		return c.JSON(http.StatusConflict, errJSON("OAuth client id already exists", "duplicate_id", ""))
	}
	if err != nil {
		h.logger.Error("oauth-client: create failed", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to create OAuth client", "server_error", ""))
	}

	// Audit AFTER persistence so a duplicate-id 409 does not log a phantom
	// "created" event. Plaintext secret is NEVER recorded in the audit row.
	ae := audit.EntryFor(c, iam.ResourceOAuthClient, iam.VerbCreate)
	ae.EntityID = row.ID
	ae.AfterState = map[string]any{
		"name":          row.Name,
		"type":          row.Type,
		"redirectUris":  row.RedirectURIs,
		"allowedScopes": row.AllowedScopes,
	}
	h.audit.LogObserved(ctx, ae)

	resp := oauthClientPublic(row)
	if plaintextSecret != "" {
		resp["clientSecret"] = plaintextSecret
	}
	return c.JSON(http.StatusCreated, map[string]any{"data": resp})
}

// oauthClientUpdateBody is the PATCH body. Pointer fields preserve the
// "field omitted vs explicit empty" distinction so COALESCE-based update
// semantics work end-to-end.
type oauthClientUpdateBody struct {
	Name              *string   `json:"name"`
	RedirectURIs      *[]string `json:"redirectUris"`
	AllowedScopes     *[]string `json:"allowedScopes"`
	AccessTTLSeconds  *int      `json:"accessTtlSeconds"`
	RefreshTTLSeconds *int      `json:"refreshTtlSeconds"`
}

// UpdateOAuthClient applies a partial update. id and type are immutable
// (per design); attempting to set them via PATCH is silently ignored because
// the bound struct does not declare them.
func (h *Handler) UpdateOAuthClient(c echo.Context) error {
	ctx := c.Request().Context()
	id := c.Param("id")
	var body oauthClientUpdateBody
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("Invalid request body", "validation_error", ""))
	}
	if msg := body.validate(); msg != "" {
		return c.JSON(http.StatusBadRequest, errJSON(msg, "validation_error", ""))
	}

	in := authstore.UpdateInput{
		Name:              body.Name,
		RedirectURIs:      body.RedirectURIs,
		AllowedScopes:     body.AllowedScopes,
		AccessTTLSeconds:  body.AccessTTLSeconds,
		RefreshTTLSeconds: body.RefreshTTLSeconds,
	}

	row, err := h.oauth.Update(ctx, id, in)
	if errors.Is(err, authstore.ErrClientNotFound) {
		return c.JSON(http.StatusNotFound, errJSON("OAuth client not found", "not_found", ""))
	}
	if err != nil {
		h.logger.Error("oauth-client: update failed", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to update OAuth client", "server_error", ""))
	}

	ae := audit.EntryFor(c, iam.ResourceOAuthClient, iam.VerbUpdate)
	ae.EntityID = row.ID
	ae.AfterState = map[string]any{
		"name":          row.Name,
		"redirectUris":  row.RedirectURIs,
		"allowedScopes": row.AllowedScopes,
	}
	h.audit.LogObserved(ctx, ae)

	return c.JSON(http.StatusOK, map[string]any{"data": oauthClientPublic(row)})
}

// RotateOAuthClientSecret mints a new secret + hash and returns the plaintext
// in the response exactly once. Refuses on public clients (no secret to rotate).
// Does NOT revoke active refresh tokens — see the spec's rotate-confirm UX.
func (h *Handler) RotateOAuthClientSecret(c echo.Context) error {
	ctx := c.Request().Context()
	id := c.Param("id")

	row, err := h.oauth.GetByID(ctx, id)
	if errors.Is(err, authstore.ErrClientNotFound) {
		return c.JSON(http.StatusNotFound, errJSON("OAuth client not found", "not_found", ""))
	}
	if err != nil {
		h.logger.Error("oauth-client: rotate lookup failed", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to load OAuth client", "server_error", ""))
	}
	if row.Type != "confidential" {
		return c.JSON(http.StatusConflict, errJSON("Cannot rotate secret for non-confidential client", "client_type_no_secret", ""))
	}

	plain, hash, err := generateClientSecret()
	if err != nil {
		h.logger.Error("oauth-client: secret generation failed", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to generate client secret", "server_error", ""))
	}
	updated, err := h.oauth.RotateSecret(ctx, id, hash)
	if errors.Is(err, authstore.ErrClientNotFound) {
		// Lost the row between GetByID and RotateSecret (concurrent delete).
		return c.JSON(http.StatusNotFound, errJSON("OAuth client not found", "not_found", ""))
	}
	if err != nil {
		h.logger.Error("oauth-client: rotate failed", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to rotate secret", "server_error", ""))
	}

	ae := audit.EntryFor(c, iam.ResourceOAuthClient, iam.VerbRotate)
	ae.EntityID = updated.ID
	// Plaintext secret is NEVER in the audit payload. Only the fact + timing.
	ae.AfterState = map[string]any{"type": updated.Type}
	h.audit.LogObserved(ctx, ae)

	resp := oauthClientPublic(updated)
	resp["clientSecret"] = plain
	return c.JSON(http.StatusOK, map[string]any{"data": resp})
}

// DeleteOAuthClient removes a client; dependent refresh-tokens cascade away
// via the FK rewritten in migration 20260612000000_oauth_client_admin.
func (h *Handler) DeleteOAuthClient(c echo.Context) error {
	ctx := c.Request().Context()
	id := c.Param("id")

	// Snapshot the row before delete so the audit payload describes what
	// disappeared (otherwise the audit log shows only the id).
	pre, err := h.oauth.GetByID(ctx, id)
	if errors.Is(err, authstore.ErrClientNotFound) {
		return c.JSON(http.StatusNotFound, errJSON("OAuth client not found", "not_found", ""))
	}
	if err != nil {
		h.logger.Error("oauth-client: delete lookup failed", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to load OAuth client", "server_error", ""))
	}

	if err := h.oauth.Delete(ctx, id); err != nil {
		if errors.Is(err, authstore.ErrClientNotFound) {
			return c.JSON(http.StatusNotFound, errJSON("OAuth client not found", "not_found", ""))
		}
		h.logger.Error("oauth-client: delete failed", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to delete OAuth client", "server_error", ""))
	}

	ae := audit.EntryFor(c, iam.ResourceOAuthClient, iam.VerbDelete)
	ae.EntityID = id
	ae.BeforeState = map[string]any{"name": pre.Name, "type": pre.Type}
	h.audit.LogObserved(ctx, ae)

	return c.NoContent(http.StatusNoContent)
}

// generateClientSecret returns the plaintext to surface to the admin (with the
// nx_cs_ prefix) and the bcrypt hash to store at rest. The plaintext is the
// full prefixed string so verifyClientAuth at /token compares the same value.
func generateClientSecret() (plaintext string, hash []byte, err error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, err
	}
	plaintext = oauthClientSecretPrefix + base64.RawURLEncoding.EncodeToString(raw)
	hash, err = bcrypt.GenerateFromPassword([]byte(plaintext), bcrypt.DefaultCost)
	if err != nil {
		return "", nil, err
	}
	return plaintext, hash, nil
}

// oauthClientPublic projects the store row into the response shape. Drops
// clientSecretHash unconditionally — callers that need to surface a freshly
// minted plaintext secret splice it in separately.
func oauthClientPublic(r *authstore.OAuthClient) map[string]any {
	body := map[string]any{
		"id":                r.ID,
		"name":              r.Name,
		"type":              r.Type,
		"redirectUris":      r.RedirectURIs,
		"allowedScopes":     r.AllowedScopes,
		"accessTtlSeconds":  r.AccessTTLSeconds,
		"refreshTtlSeconds": r.RefreshTTLSeconds,
		"createdAt":         r.CreatedAt,
		"updatedAt":         r.UpdatedAt,
	}
	if r.LastSecretRotatedAt != nil {
		body["lastSecretRotatedAt"] = *r.LastSecretRotatedAt
	} else {
		body["lastSecretRotatedAt"] = nil
	}
	return body
}

// applyDefaults fills the spec's "sensible defaults" so an admin can submit
// a near-empty body and still get a usable client.
func (b *oauthClientCreateBody) applyDefaults() {
	if b.Type == "" {
		b.Type = "confidential"
	}
	if b.AllowedScopes == nil {
		b.AllowedScopes = []string{"openid", "profile", "email"}
	}
	if b.AccessTTLSeconds == nil {
		n := 3600
		b.AccessTTLSeconds = &n
	}
	if b.RefreshTTLSeconds == nil {
		n := 86400
		b.RefreshTTLSeconds = &n
	}
}

// validate returns "" on success or a human-readable error message on
// the first invalid field. Centralises the spec's validation table.
func (b *oauthClientCreateBody) validate() string {
	if !idRegex.MatchString(b.ID) {
		return "id must match ^[a-z][a-z0-9-]{2,63}$"
	}
	if b.Name == "" || len(b.Name) > 100 {
		return "name must be non-empty and at most 100 characters"
	}
	if b.Type != "public" && b.Type != "confidential" {
		return "type must be 'public' or 'confidential'"
	}
	if err := validateRedirectURIs(b.RedirectURIs); err != "" {
		return err
	}
	if err := validateScopes(b.AllowedScopes); err != "" {
		return err
	}
	if *b.AccessTTLSeconds < 60 || *b.AccessTTLSeconds > 86400 {
		return "accessTtlSeconds must be between 60 and 86400"
	}
	if *b.RefreshTTLSeconds < 3600 || *b.RefreshTTLSeconds > 30*86400 {
		return "refreshTtlSeconds must be between 3600 and 2592000"
	}
	return ""
}

// validate for the PATCH body — every field is optional, but a present
// field must satisfy the same constraints as create.
func (b *oauthClientUpdateBody) validate() string {
	if b.Name != nil && (*b.Name == "" || len(*b.Name) > 100) {
		return "name must be non-empty and at most 100 characters"
	}
	if b.RedirectURIs != nil {
		if err := validateRedirectURIs(*b.RedirectURIs); err != "" {
			return err
		}
	}
	if b.AllowedScopes != nil {
		if err := validateScopes(*b.AllowedScopes); err != "" {
			return err
		}
	}
	if b.AccessTTLSeconds != nil && (*b.AccessTTLSeconds < 60 || *b.AccessTTLSeconds > 86400) {
		return "accessTtlSeconds must be between 60 and 86400"
	}
	if b.RefreshTTLSeconds != nil && (*b.RefreshTTLSeconds < 3600 || *b.RefreshTTLSeconds > 30*86400) {
		return "refreshTtlSeconds must be between 3600 and 2592000"
	}
	return ""
}

func validateRedirectURIs(uris []string) string {
	if len(uris) == 0 || len(uris) > 20 {
		return "redirectUris must contain between 1 and 20 entries"
	}
	for _, raw := range uris {
		// Delegate to the authserver store helper so registration accepts
		// exactly what the authorize-time matcher (exact-match / matchLoopback)
		// can later honor — including the RFC 8252 §7.3 ":*" loopback port
		// wildcard (e.g. the "tui" CLI client's http://127.0.0.1:*/callback).
		if !authstore.ValidRedirectURIPattern(raw) {
			return "redirectUris entries must be https:// or http loopback (localhost / 127.0.0.1 / [::1], optional :* port for 127.0.0.1 / [::1])"
		}
	}
	return ""
}

func validateScopes(scopes []string) string {
	if len(scopes) == 0 || len(scopes) > 20 {
		return "allowedScopes must contain between 1 and 20 entries"
	}
	for _, s := range scopes {
		if !scopeRegex.MatchString(s) {
			return "allowedScopes contains an invalid scope name: " + s
		}
		if strings.ContainsRune(s, ' ') {
			return "allowedScopes entries must not contain whitespace"
		}
	}
	return ""
}

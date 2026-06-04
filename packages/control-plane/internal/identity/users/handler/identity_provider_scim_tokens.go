package iam

import (
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/scim/scimstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
)

// GET /api/admin/identity-providers/:idpId/scim-tokens
func (h *Handler) ListScimTokens(c echo.Context) error {
	idpID := c.Param("idpId")
	tokens, err := h.scim.ListScimTokens(c.Request().Context(), &idpID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}
	// Never return tokenHash in the response.
	type tokenResp struct {
		ID                 string  `json:"id"`
		Name               string  `json:"name"`
		TokenPrefix        string  `json:"tokenPrefix"`
		IdentityProviderID *string `json:"identityProviderId,omitempty"`
		CreatedBy          string  `json:"createdBy"`
		CreatedAt          any     `json:"createdAt"`
		LastUsedAt         any     `json:"lastUsedAt"`
	}
	out := make([]tokenResp, len(tokens))
	for i, t := range tokens {
		out[i] = tokenResp{
			ID: t.ID, Name: t.Name, TokenPrefix: t.TokenPrefix,
			IdentityProviderID: t.IdentityProviderID,
			CreatedBy:          t.CreatedBy, CreatedAt: t.CreatedAt, LastUsedAt: t.LastUsedAt,
		}
	}
	return c.JSON(http.StatusOK, map[string]any{"data": out, "total": len(out)})
}

// POST /api/admin/identity-providers/:idpId/scim-tokens
func (h *Handler) CreateScimToken(c echo.Context) error {
	idpID := c.Param("idpId")
	var body struct {
		Name string `json:"name"`
	}
	if err := c.Bind(&body); err != nil || body.Name == "" {
		return c.JSON(http.StatusBadRequest, errJSON("name is required", "validation_error", ""))
	}

	createdBy := "unknown"
	if aa := middleware.AdminAuthFromContext(c); aa != nil {
		createdBy = aa.KeyID
	}

	rawToken, prefix, err := scimstore.GenerateScimToken()
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Token generation failed", "server_error", ""))
	}
	tokenHash := scimstore.HashScimToken(rawToken)

	tok, err := h.scim.CreateScimToken(c.Request().Context(), scimstore.CreateScimTokenParams{
		Name:               body.Name,
		TokenHash:          tokenHash,
		TokenPrefix:        prefix,
		IdentityProviderID: &idpID,
		CreatedBy:          createdBy,
	})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to create token", "server_error", ""))
	}

	// Return the raw token ONCE — never stored, never retrievable again.
	return c.JSON(http.StatusCreated, map[string]any{
		"id":                 tok.ID,
		"name":               tok.Name,
		"token":              rawToken, // shown once
		"tokenPrefix":        tok.TokenPrefix,
		"identityProviderId": tok.IdentityProviderID,
		"createdAt":          tok.CreatedAt,
	})
}

// DELETE /api/admin/identity-providers/:idpId/scim-tokens/:tokenId
func (h *Handler) RevokeScimToken(c echo.Context) error {
	if err := h.scim.RevokeScimToken(c.Request().Context(), c.Param("tokenId")); err != nil {
		return c.JSON(http.StatusNotFound, errJSON("Token not found", "not_found", ""))
	}
	return c.NoContent(http.StatusNoContent)
}

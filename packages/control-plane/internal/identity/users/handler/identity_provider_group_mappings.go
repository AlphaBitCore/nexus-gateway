package iam

import (
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/scim/scimstore"
)

// GET /api/admin/identity-providers/:idpId/group-mappings
func (h *Handler) ListIdpGroupMappings(c echo.Context) error {
	mappings, err := h.scim.ListIdpGroupMappings(c.Request().Context(), c.Param("idpId"))
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}
	if mappings == nil {
		mappings = []scimstore.IdpGroupMapping{}
	}
	return c.JSON(http.StatusOK, map[string]any{"data": mappings, "total": len(mappings)})
}

// POST /api/admin/identity-providers/:idpId/group-mappings
func (h *Handler) CreateIdpGroupMapping(c echo.Context) error {
	idpID := c.Param("idpId")
	var body struct {
		ExternalGroupID   string  `json:"externalGroupId"`
		ExternalGroupName *string `json:"externalGroupName"`
		IamGroupID        string  `json:"iamGroupId"`
	}
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("Invalid request body", "validation_error", ""))
	}
	if body.ExternalGroupID == "" || body.IamGroupID == "" {
		return c.JSON(http.StatusBadRequest, errJSON("externalGroupId and iamGroupId are required", "validation_error", ""))
	}

	m, err := h.scim.CreateIdpGroupMapping(c.Request().Context(), scimstore.CreateIdpGroupMappingParams{
		IdentityProviderID: idpID,
		ExternalGroupID:    body.ExternalGroupID,
		ExternalGroupName:  body.ExternalGroupName,
		IamGroupID:         body.IamGroupID,
	})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to create mapping", "server_error", ""))
	}
	return c.JSON(http.StatusCreated, m)
}

// DELETE /api/admin/identity-providers/:idpId/group-mappings/:mappingId
func (h *Handler) DeleteIdpGroupMapping(c echo.Context) error {
	if err := h.scim.DeleteIdpGroupMapping(c.Request().Context(), c.Param("mappingId")); err != nil {
		return c.JSON(http.StatusNotFound, errJSON("Mapping not found", "not_found", ""))
	}
	return c.NoContent(http.StatusNoContent)
}

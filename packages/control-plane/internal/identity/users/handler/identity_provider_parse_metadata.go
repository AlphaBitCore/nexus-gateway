package iam

import (
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/samlmeta"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
)

// parseSAMLMetadataRequest is the POST body for the SAML metadata-import
// helper: the raw IdP metadata XML the admin uploaded or pasted into the
// Add-IdP form. There is deliberately no metadata-URL field — the server never
// fetches, keeping the surface free of SSRF exposure (mirrors the samlmeta
// package contract).
type parseSAMLMetadataRequest struct {
	MetadataXML string `json:"metadataXml"`
}

// ParseSAMLMetadata handles POST /api/admin/identity-providers/parse-saml-metadata.
//
// It parses an uploaded/pasted SAML IdP metadata document and returns the
// SP-relevant fields (entityId, ssoUrl, certificatePem) plus the auto-detected
// email / groups attribute names, so the Add-IdP form is pre-filled instead of
// the admin hand-copying the signing certificate. Pure parse: no DB, no
// network. Gated by VerbProbe — the same pre-create "I'm composing an IdP
// config" tier as POST /identity-providers/test — so it introduces no new IAM
// action and no UI allowedActions drift. A parse failure is a 400 carrying the
// specific reason (empty document, no IDPSSODescriptor, no entityID, no SSO
// URL, no signing certificate, malformed certificate).
func (h *Handler) ParseSAMLMetadata(c echo.Context) error {
	var body parseSAMLMetadataRequest
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("Invalid request body", "validation_error", ""))
	}
	parsed, err := samlmeta.Parse([]byte(body.MetadataXML))
	if err != nil {
		// samlmeta returns a typed, admin-actionable reason; surface it on the
		// metadataXml field so the form can flag the upload inline.
		return c.JSON(http.StatusBadRequest, errJSON(err.Error(), "validation_error", "metadataXml"))
	}
	ae := audit.EntryFor(c, iam.ResourceIdentityProvider, iam.VerbProbe)
	ae.AfterState = map[string]any{
		"entityId":       parsed.EntityID,
		"emailDetected":  parsed.EmailAttribute != "",
		"groupsDetected": parsed.GroupsAttribute != "",
	}
	h.audit.LogObserved(c.Request().Context(), ae)
	return c.JSON(http.StatusOK, parsed)
}

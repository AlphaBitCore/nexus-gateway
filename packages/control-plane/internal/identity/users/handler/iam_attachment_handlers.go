package iam

import (
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/revocation"
	iamengine "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/iam"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
)

func (h *Handler) AttachPrincipalPolicy(c echo.Context) error {
	var body struct {
		PolicyID  string  `json:"policyId"`
		ExpiresAt *string `json:"expiresAt,omitempty"` // RFC3339; nil = permanent
	}
	if err := c.Bind(&body); err != nil || body.PolicyID == "" {
		return c.JSON(http.StatusBadRequest, errJSON("policyId required", "validation_error", ""))
	}
	var expiresAtTime *time.Time
	if body.ExpiresAt != nil && *body.ExpiresAt != "" {
		t, err := time.Parse(time.RFC3339, *body.ExpiresAt)
		if err != nil {
			return c.JSON(http.StatusBadRequest, errJSON("expiresAt must be RFC3339", "validation_error", "INVALID_EXPIRES_AT"))
		}
		if !t.After(time.Now()) {
			return c.JSON(http.StatusBadRequest, errJSON("expiresAt must be in the future", "validation_error", "EXPIRES_AT_IN_PAST"))
		}
		expiresAtTime = &t
	}
	ctx := c.Request().Context()
	// Normalise on the way IN so storage only ever holds the canonical
	// spelling. LoadPolicies matches principalType for EQUALITY, so a row
	// written under the session's own "admin_user" would be listable here and
	// invisible to every evaluation — the console showing a grant the request
	// still refuses.
	principalType, ptOK := iamengine.NormalisePrincipalType(c.Param("type"))
	if !ptOK {
		return c.JSON(http.StatusBadRequest, errJSON(
			"principalType must be one of "+strings.Join(iamengine.AcceptedPrincipalTypes(), ", "),
			"validation_error", "INVALID_PRINCIPAL_TYPE"))
	}
	principalID := c.Param("id")

	// Grant ceiling — attaching a policy confers its permissions to
	// the target principal (including the caller itself). The caller must already
	// hold every permission the policy grants, else this is privilege escalation.
	if blocked, resp := h.ceilingBlocksPolicyID(c, body.PolicyID); blocked {
		return resp
	}

	id, err := h.iam.AttachPrincipalPolicy(ctx, principalType, principalID, body.PolicyID, expiresAtTime)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to attach policy", "server_error", ""))
	}
	if h.iamEngine != nil {
		h.iamEngine.InvalidateCache("", "")
	}

	// A direct policy attachment grants new permissions to the principal;
	// kick live sessions so the data plane picks up the change on next
	// token mint. Only nexus_user principals have CP sessions.
	if principalType == "nexus_user" {
		h.revokeUserScope(ctx, principalID, revocation.ReasonRoleChange)
	}

	ae := audit.EntryFor(c, iam.ResourceIamPolicy, iam.VerbCreate)
	ae.EntityID = id
	if err := h.audit.LogCritical(ctx, ae); err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Audit failure", "server_error", ""))
	}
	return c.JSON(http.StatusCreated, map[string]any{"id": id})
}

func (h *Handler) ListPrincipalPolicies(c echo.Context) error {
	// Same normalisation as the write path: listing under "admin_user" must
	// show what was stored under "nexus_user", or the two disagree again in
	// the opposite direction.
	principalType, ptOK := iamengine.NormalisePrincipalType(c.Param("type"))
	if !ptOK {
		return c.JSON(http.StatusBadRequest, errJSON(
			"principalType must be one of "+strings.Join(iamengine.AcceptedPrincipalTypes(), ", "),
			"validation_error", "INVALID_PRINCIPAL_TYPE"))
	}
	principalID := c.Param("id")

	attachments, err := h.iam.ListPrincipalPolicyAttachments(c.Request().Context(), principalType, principalID)
	if err != nil {
		h.logger.Error("list principal policies", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}
	return c.JSON(http.StatusOK, map[string]any{"data": attachments})
}

func (h *Handler) DetachPrincipalPolicy(c echo.Context) error {
	ctx := c.Request().Context()
	attachmentID := c.Param("attachmentId")

	// Look up the principal before the delete so we can still emit a
	// scope=user revocation after the attachment is gone.
	principalType, principalID, _, lookupErr := h.iam.GetPrincipalPolicyAttachmentByID(ctx, attachmentID)

	if err := h.iam.DetachPrincipalPolicy(ctx, attachmentID); err != nil {
		return c.JSON(http.StatusNotFound, errJSON("Attachment not found", "not_found", ""))
	}
	if h.iamEngine != nil {
		h.iamEngine.InvalidateCache("", "")
	}

	if lookupErr == nil && principalType == "nexus_user" {
		h.revokeUserScope(ctx, principalID, revocation.ReasonRoleChange)
	}

	ae := audit.EntryFor(c, iam.ResourceIamPolicy, iam.VerbDelete)
	ae.EntityID = attachmentID
	if err := h.audit.LogCritical(ctx, ae); err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Audit failure", "server_error", ""))
	}
	return c.NoContent(http.StatusNoContent)
}

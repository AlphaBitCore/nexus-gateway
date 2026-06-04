package iam

import (
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/revocation"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/users/iamstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
)

func (h *Handler) ListIAMGroups(c echo.Context) error {
	groups, err := h.iam.ListIamGroups(c.Request().Context())
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to list groups", "server_error", ""))
	}
	return c.JSON(http.StatusOK, map[string]any{"data": groups, "total": len(groups)})
}

func (h *Handler) GetIAMGroup(c echo.Context) error {
	ctx := c.Request().Context()
	id := c.Param("id")
	g, err := h.iam.GetIamGroup(ctx, id)
	if err != nil || g == nil {
		return c.JSON(http.StatusNotFound, errJSON("Group not found", "not_found", ""))
	}

	members, _ := h.iam.ListGroupMembers(ctx, id)
	policies, _ := h.iam.ListGroupPolicies(ctx, id)

	// Build policyAttachments with nested policy object to match the UI's IamGroupDetail type.
	type policyAttachment struct {
		ID        string `json:"id"`
		PolicyID  string `json:"policyId"`
		Policy    any    `json:"policy"`
		CreatedAt any    `json:"createdAt"`
	}
	attachments := make([]policyAttachment, len(policies))
	for i, p := range policies {
		attachments[i] = policyAttachment{
			ID:       p.ID,
			PolicyID: p.PolicyID,
			Policy: map[string]string{
				"id":   p.PolicyID,
				"name": p.PolicyName,
			},
			CreatedAt: p.CreatedAt,
		}
	}

	return c.JSON(http.StatusOK, map[string]any{
		"id": g.ID, "name": g.Name, "description": g.Description,
		"createdBy": g.CreatedBy, "createdAt": g.CreatedAt, "updatedAt": g.UpdatedAt,
		"members":           members,
		"policyAttachments": attachments,
	})
}

// ListIAMGroupMembers returns paginated members for a group.
func (h *Handler) ListIAMGroupMembers(c echo.Context) error {
	pg := parsePagination(c)
	members, total, err := h.iam.ListGroupMembersPaginated(c.Request().Context(), c.Param("id"), pg.Limit, pg.Offset)
	if err != nil {
		h.logger.Error("list group members", "groupId", c.Param("id"), "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to list members", "server_error", ""))
	}
	return c.JSON(http.StatusOK, map[string]any{"data": members, "total": total})
}

func (h *Handler) CreateIAMGroup(c echo.Context) error {
	var body struct {
		Name        string  `json:"name"`
		Description *string `json:"description"`
	}
	if err := c.Bind(&body); err != nil || body.Name == "" {
		return c.JSON(http.StatusBadRequest, errJSON("name is required", "validation_error", ""))
	}
	aa := middleware.AdminAuthFromContext(c)
	createdBy := "unknown"
	if aa != nil {
		createdBy = aa.KeyID
	}
	g, err := h.iam.CreateIamGroup(c.Request().Context(), body.Name, body.Description, createdBy)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to create group", "server_error", ""))
	}
	ae := audit.EntryFor(c, iam.ResourceIamGroup, iam.VerbCreate)
	ae.EntityID = g.ID
	ae.AfterState = g
	h.audit.LogObserved(c.Request().Context(), ae)
	return c.JSON(http.StatusCreated, g)
}

func (h *Handler) UpdateIAMGroup(c echo.Context) error {
	id := c.Param("id")
	var body struct {
		Name        *string `json:"name"`
		Description *string `json:"description"`
	}
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("Invalid request body", "validation_error", ""))
	}
	g, err := h.iam.UpdateIamGroup(c.Request().Context(), id, iamstore.UpdateIamGroupParams{Name: body.Name, Description: body.Description})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to update group", "server_error", ""))
	}
	ae := audit.EntryFor(c, iam.ResourceIamGroup, iam.VerbUpdate)
	ae.EntityID = id
	ae.AfterState = g
	h.audit.LogObserved(c.Request().Context(), ae)
	return c.JSON(http.StatusOK, g)
}

func (h *Handler) DeleteIAMGroup(c echo.Context) error {
	id := c.Param("id")
	if err := h.iam.DeleteIamGroup(c.Request().Context(), id); err != nil {
		return c.JSON(http.StatusNotFound, errJSON("Group not found", "not_found", ""))
	}
	ae := audit.EntryFor(c, iam.ResourceIamGroup, iam.VerbDelete)
	ae.EntityID = id
	h.audit.LogObserved(c.Request().Context(), ae)
	return c.NoContent(http.StatusNoContent)
}

func (h *Handler) AddIAMGroupMember(c echo.Context) error {
	var body struct {
		PrincipalType string `json:"principalType"`
		PrincipalID   string `json:"principalId"`
	}
	if err := c.Bind(&body); err != nil || body.PrincipalID == "" {
		return c.JSON(http.StatusBadRequest, errJSON("principalType and principalId required", "validation_error", ""))
	}
	if body.PrincipalType == "" {
		body.PrincipalType = "nexus_user"
	}
	id, err := h.iam.AddGroupMember(c.Request().Context(), c.Param("id"), body.PrincipalType, body.PrincipalID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to add member", "server_error", ""))
	}
	if h.iamEngine != nil {
		h.iamEngine.InvalidateCache("", "")
	}

	// Joining a group can grant new policies to a user; kick any live
	// sessions so the data plane sees the updated effective permissions on
	// the next token mint. Only nexus_user principals have CP sessions --
	// device_instance / api_key principals are out of scope here.
	if body.PrincipalType == "nexus_user" {
		h.revokeUserScope(c.Request().Context(), body.PrincipalID, revocation.ReasonRoleChange)
	}

	ae := audit.EntryFor(c, iam.ResourceIamGroup, iam.VerbCreate)
	ae.EntityID = id
	h.audit.LogObserved(c.Request().Context(), ae)
	return c.JSON(http.StatusCreated, map[string]any{"id": id})
}

func (h *Handler) RemoveIAMGroupMember(c echo.Context) error {
	ctx := c.Request().Context()
	membershipID := c.Param("membershipId")

	// Look up the membership first so we still know whose sessions to
	// revoke after the row is deleted. A miss here just skips the
	// revocation fan-out; the delete below still returns the canonical
	// not-found response.
	_, principalType, principalID, lookupErr := h.iam.GetGroupMembershipByID(ctx, membershipID)

	if err := h.iam.RemoveGroupMember(ctx, membershipID); err != nil {
		return c.JSON(http.StatusNotFound, errJSON("Membership not found", "not_found", ""))
	}
	if h.iamEngine != nil {
		h.iamEngine.InvalidateCache("", "")
	}

	if lookupErr == nil && principalType == "nexus_user" {
		h.revokeUserScope(ctx, principalID, revocation.ReasonRoleChange)
	}

	ae := audit.EntryFor(c, iam.ResourceIamGroup, iam.VerbDelete)
	ae.EntityID = membershipID
	h.audit.LogObserved(ctx, ae)
	return c.NoContent(http.StatusNoContent)
}

func (h *Handler) AttachIAMGroupPolicy(c echo.Context) error {
	var body struct {
		PolicyID string `json:"policyId"`
	}
	if err := c.Bind(&body); err != nil || body.PolicyID == "" {
		return c.JSON(http.StatusBadRequest, errJSON("policyId required", "validation_error", ""))
	}
	ctx := c.Request().Context()
	groupID := c.Param("id")
	id, err := h.iam.AttachGroupPolicy(ctx, groupID, body.PolicyID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to attach policy", "server_error", ""))
	}
	if h.iamEngine != nil {
		h.iamEngine.InvalidateCache("", "")
	}

	// Adding a policy to a group expands every member's effective
	// permissions; fan out to all nexus_user members so their tokens pick
	// up the change on the next mint.
	members, mErr := h.iam.ListGroupMembers(ctx, groupID)
	if mErr != nil {
		h.logger.Error("attach group policy: list members", "groupId", groupID, "error", mErr)
	} else {
		for _, m := range members {
			if m.PrincipalType == "nexus_user" {
				h.revokeUserScope(ctx, m.PrincipalID, revocation.ReasonRoleChange)
			}
		}
	}

	ae := audit.EntryFor(c, iam.ResourceIamGroup, iam.VerbCreate)
	ae.EntityID = id
	h.audit.LogObserved(ctx, ae)
	return c.JSON(http.StatusCreated, map[string]any{"id": id})
}

func (h *Handler) DetachIAMGroupPolicy(c echo.Context) error {
	ctx := c.Request().Context()
	attachmentID := c.Param("attachmentId")

	// Look up the group coordinates before the delete so we can still fan
	// out revocations to members after the row is gone.
	groupID, _, lookupErr := h.iam.GetGroupPolicyAttachmentByID(ctx, attachmentID)

	if err := h.iam.DetachGroupPolicy(ctx, attachmentID); err != nil {
		return c.JSON(http.StatusNotFound, errJSON("Attachment not found", "not_found", ""))
	}
	if h.iamEngine != nil {
		h.iamEngine.InvalidateCache("", "")
	}

	if lookupErr == nil {
		members, mErr := h.iam.ListGroupMembers(ctx, groupID)
		if mErr != nil {
			h.logger.Error("detach group policy: list members", "groupId", groupID, "error", mErr)
		} else {
			for _, m := range members {
				if m.PrincipalType == "nexus_user" {
					h.revokeUserScope(ctx, m.PrincipalID, revocation.ReasonRoleChange)
				}
			}
		}
	}

	ae := audit.EntryFor(c, iam.ResourceIamGroup, iam.VerbDelete)
	ae.EntityID = attachmentID
	h.audit.LogObserved(ctx, ae)
	return c.NoContent(http.StatusNoContent)
}

package iam

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/revocation"
	iamengine "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/iam"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/users/iamstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
)

// RegisterIAMRoutes registers IAM policy/group management routes.
func (h *Handler) RegisterIAMRoutes(g *echo.Group, iamMW func(action string) echo.MiddlewareFunc) {
	// Policies
	g.GET("/iam/policies", h.ListIAMPolicies, iamMW(iam.ResourceIamPolicy.Action(iam.VerbRead)))
	g.GET("/iam/policies/:id", h.GetIAMPolicy, iamMW(iam.ResourceIamPolicy.Action(iam.VerbRead)))
	g.GET("/iam/policies/:id/attachments", h.ListPolicyAttachments, iamMW(iam.ResourceIamPolicy.Action(iam.VerbRead)))
	g.POST("/iam/policies", h.CreateIAMPolicy, iamMW(iam.ResourceIamPolicy.Action(iam.VerbCreate)))
	g.PUT("/iam/policies/:id", h.UpdateIAMPolicy, iamMW(iam.ResourceIamPolicy.Action(iam.VerbUpdate)))
	g.DELETE("/iam/policies/:id", h.DeleteIAMPolicy, iamMW(iam.ResourceIamPolicy.Action(iam.VerbDelete)))
	// Groups — gated on the canonical admin:iam-group.<verb> action so the
	// catalog row in shared/iam.Catalog is reachable and an operator can
	// be granted group management without policy management (and vice
	// versa). Audit emissions already use ResourceIamGroup; the iamMW
	// gate now lines up with them.
	g.GET("/iam/groups", h.ListIAMGroups, iamMW(iam.ResourceIamGroup.Action(iam.VerbRead)))
	g.GET("/iam/groups/:id", h.GetIAMGroup, iamMW(iam.ResourceIamGroup.Action(iam.VerbRead)))
	g.POST("/iam/groups", h.CreateIAMGroup, iamMW(iam.ResourceIamGroup.Action(iam.VerbCreate)))
	g.PUT("/iam/groups/:id", h.UpdateIAMGroup, iamMW(iam.ResourceIamGroup.Action(iam.VerbUpdate)))
	g.DELETE("/iam/groups/:id", h.DeleteIAMGroup, iamMW(iam.ResourceIamGroup.Action(iam.VerbDelete)))
	g.GET("/iam/groups/:id/members", h.ListIAMGroupMembers, iamMW(iam.ResourceIamGroup.Action(iam.VerbRead)))
	g.POST("/iam/groups/:id/members", h.AddIAMGroupMember, iamMW(iam.ResourceIamGroup.Action(iam.VerbUpdate)))
	g.DELETE("/iam/groups/:id/members/:membershipId", h.RemoveIAMGroupMember, iamMW(iam.ResourceIamGroup.Action(iam.VerbUpdate)))
	g.POST("/iam/groups/:id/policies", h.AttachIAMGroupPolicy, iamMW(iam.ResourceIamGroup.Action(iam.VerbUpdate)))
	g.DELETE("/iam/groups/:id/policies/:attachmentId", h.DetachIAMGroupPolicy, iamMW(iam.ResourceIamGroup.Action(iam.VerbUpdate)))
	// Principal attachments
	g.GET("/iam/principals/:type/:id/policies", h.ListPrincipalPolicies, iamMW(iam.ResourceIamPolicy.Action(iam.VerbRead)))
	g.POST("/iam/principals/:type/:id/policies", h.AttachPrincipalPolicy, iamMW(iam.ResourceIamPolicy.Action(iam.VerbUpdate)))
	g.DELETE("/iam/principals/:type/:id/policies/:attachmentId", h.DetachPrincipalPolicy, iamMW(iam.ResourceIamPolicy.Action(iam.VerbUpdate)))
	// Simulator
	g.POST("/iam/simulate", h.SimulateIAM, iamMW(iam.ResourceIamPolicy.Action(iam.VerbRead)))
}

func (h *Handler) ListIAMPolicies(c echo.Context) error {
	pg := parsePagination(c)
	var enabled *bool
	if v := c.QueryParam("enabled"); v == "true" {
		t := true
		enabled = &t
	} else if v == "false" {
		f := false
		enabled = &f
	}
	policies, total, err := h.iam.ListIamPolicies(c.Request().Context(), c.QueryParam("q"), c.QueryParam("type"), enabled, pg.Limit, pg.Offset)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to list policies", "server_error", ""))
	}
	return c.JSON(http.StatusOK, map[string]any{"data": policies, "total": total})
}

func (h *Handler) GetIAMPolicy(c echo.Context) error {
	p, err := h.iam.GetIamPolicy(c.Request().Context(), c.Param("id"))
	if err != nil || p == nil {
		return c.JSON(http.StatusNotFound, errJSON("Policy not found", "not_found", ""))
	}
	return c.JSON(http.StatusOK, p)
}

// ListPolicyAttachments returns groups (roles) and direct principal attachments for a policy.
// Response shape: { roles: [...], directAttachments: [...] }
func (h *Handler) ListPolicyAttachments(c echo.Context) error {
	ctx := c.Request().Context()
	policyID := c.Param("id")

	// 1. Groups that have this policy attached.
	groupRows, err := h.iam.ListGroupsForPolicy(ctx, policyID)
	if err != nil {
		h.logger.Error("list groups for policy", "policyId", policyID, "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to list policy attachments", "server_error", ""))
	}

	type memberEntry struct {
		PrincipalType string `json:"principalType"`
		PrincipalID   string `json:"principalId"`
	}
	type roleEntry struct {
		ID          string        `json:"id"`
		Name        string        `json:"name"`
		MemberCount int           `json:"memberCount"`
		Members     []memberEntry `json:"members"`
	}

	roles := make([]roleEntry, 0, len(groupRows))
	for _, g := range groupRows {
		members, err := h.iam.ListGroupMembers(ctx, g.ID)
		if err != nil {
			h.logger.Error("list group members", "groupId", g.ID, "error", err)
			continue
		}
		me := make([]memberEntry, len(members))
		for i, m := range members {
			me[i] = memberEntry{PrincipalType: m.PrincipalType, PrincipalID: m.PrincipalID}
		}
		roles = append(roles, roleEntry{
			ID:          g.ID,
			Name:        g.Name,
			MemberCount: len(members),
			Members:     me,
		})
	}

	// 2. Direct principal attachments.
	directRows, err := h.iam.ListDirectPolicyAttachments(ctx, policyID)
	if err != nil {
		h.logger.Error("list direct policy attachments", "policyId", policyID, "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to list policy attachments", "server_error", ""))
	}

	return c.JSON(http.StatusOK, map[string]any{
		"roles":             roles,
		"directAttachments": directRows,
	})
}

func (h *Handler) CreateIAMPolicy(c echo.Context) error {
	var body struct {
		Name        string          `json:"name"`
		Description *string         `json:"description"`
		Document    json.RawMessage `json:"document"`
	}
	if err := c.Bind(&body); err != nil || body.Name == "" || body.Document == nil {
		return c.JSON(http.StatusBadRequest, errJSON("name and document are required", "validation_error", ""))
	}

	aa := middleware.AdminAuthFromContext(c)
	createdBy := "unknown"
	if aa != nil {
		createdBy = aa.KeyID
	}

	p, err := h.iam.CreateIamPolicy(c.Request().Context(), body.Name, body.Description, body.Document, createdBy)
	if err != nil {
		h.logger.Error("create iam policy", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to create policy", "server_error", ""))
	}

	ae := audit.EntryFor(c, iam.ResourceIamPolicy, iam.VerbCreate)
	ae.EntityID = p.ID
	h.audit.LogObserved(c.Request().Context(), ae)

	return c.JSON(http.StatusCreated, p)
}

func (h *Handler) UpdateIAMPolicy(c echo.Context) error {
	id := c.Param("id")
	var body struct {
		Name        *string `json:"name"`
		Description *string `json:"description"`
		Enabled     *bool   `json:"enabled"`
		Document    any     `json:"document"`
	}
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("Invalid request body", "validation_error", ""))
	}

	params := iamstore.UpdateIamPolicyParams{
		Name:        body.Name,
		Description: body.Description,
		Enabled:     body.Enabled,
	}
	if body.Document != nil {
		raw, _ := json.Marshal(body.Document)
		params.Document = raw
	}

	p, err := h.iam.UpdateIamPolicy(c.Request().Context(), id, params)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to update policy", "server_error", ""))
	}

	if h.iamEngine != nil {
		h.iamEngine.InvalidateCache("", "")
	}

	// Fan out a scope=user revocation to every admin_user principal who has
	// this policy attached directly or via group membership. Changing a
	// policy body silently grants or removes permissions, so data-plane
	// tokens minted against the old body must be rejected on their next
	// refresh.
	ctx := c.Request().Context()
	userIDs, ulErr := h.iam.ListPolicyAttachedUserIDs(ctx, id)
	if ulErr != nil {
		h.logger.Error("update policy: list attached users", "policyId", id, "error", ulErr)
	} else {
		for _, uid := range userIDs {
			h.revokeUserScope(ctx, uid, revocation.ReasonRoleChange)
		}
	}

	ae := audit.EntryFor(c, iam.ResourceIamPolicy, iam.VerbUpdate)
	ae.EntityID = id
	ae.AfterState = p
	h.audit.LogObserved(c.Request().Context(), ae)

	return c.JSON(http.StatusOK, p)
}

func (h *Handler) DeleteIAMPolicy(c echo.Context) error {
	id := c.Param("id")
	if err := h.iam.DeleteIamPolicy(c.Request().Context(), id); err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to delete policy", "server_error", ""))
	}
	if h.iamEngine != nil {
		h.iamEngine.InvalidateCache("", "")
	}

	ae := audit.EntryFor(c, iam.ResourceIamPolicy, iam.VerbDelete)
	ae.EntityID = id
	h.audit.LogObserved(c.Request().Context(), ae)

	return c.NoContent(http.StatusNoContent)
}

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
	principalType := c.Param("type")
	principalID := c.Param("id")
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
	h.audit.LogObserved(ctx, ae)
	return c.JSON(http.StatusCreated, map[string]any{"id": id})
}

func (h *Handler) ListPrincipalPolicies(c echo.Context) error {
	principalType := c.Param("type")
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
	h.audit.LogObserved(ctx, ae)
	return c.NoContent(http.StatusNoContent)
}

// SimulateIAM evaluates an IAM policy decision for a given principal, action, and resource.
func (h *Handler) SimulateIAM(c echo.Context) error {
	var body struct {
		Principal struct {
			Type string `json:"type"`
			ID   string `json:"id"`
		} `json:"principal"`
		Action   string                 `json:"action"`
		Resource string                 `json:"resource"`
		Context  map[string]interface{} `json:"context"`
	}
	if err := c.Bind(&body); err != nil || body.Principal.Type == "" || body.Principal.ID == "" || body.Action == "" || body.Resource == "" {
		return c.JSON(http.StatusBadRequest, errJSON("principal, action, and resource are required", "validation_error", ""))
	}

	if h.iamEngine == nil {
		return c.JSON(http.StatusServiceUnavailable, errJSON("IAM engine not available", "server_error", ""))
	}

	condCtx := make(iamengine.ConditionContext)
	if body.Context != nil {
		for k, v := range body.Context {
			if s, ok := v.(string); ok {
				condCtx[k] = s
			}
		}
	}

	result, err := h.iamEngine.Evaluate(c.Request().Context(), body.Principal.Type, body.Principal.ID, body.Action, body.Resource, condCtx)
	if err != nil {
		h.logger.Error("IAM simulate failed", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Simulation failed", "server_error", ""))
	}
	return c.JSON(http.StatusOK, result)
}

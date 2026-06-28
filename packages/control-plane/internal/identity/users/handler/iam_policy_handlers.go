package iam

import (
	"github.com/goccy/go-json"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/revocation"
	iamengine "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/iam"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/users/iamstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
)

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

// validatePolicyDocumentJSON decodes a raw IAM policy document and runs the
// structural validator (effect enum, non-empty action/resource, no consecutive
// wildcards, statement cap, known condition operators). It returns the list of
// human-readable validation errors; an empty slice means the document is valid.
// Malformed JSON is itself reported as a validation error so the CRUD handlers
// reject it with 400 rather than persisting an undecodable blob.
func validatePolicyDocumentJSON(raw []byte) []string {
	var doc iamengine.PolicyDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		return []string{"document is not valid JSON: " + err.Error()}
	}
	return iamengine.ValidatePolicyDocument(&doc)
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

	if verrs := validatePolicyDocumentJSON(body.Document); len(verrs) > 0 {
		return c.JSON(http.StatusBadRequest, errJSON("Invalid policy document: "+strings.Join(verrs, "; "), "validation_error", "document"))
	}

	// Grant ceiling — a principal may not author a policy that grants
	// permissions it does not itself hold (prevents staging an admin:* policy for
	// later self-attachment).
	if blocked, resp := h.ceilingBlocksRaw(c, body.Document); blocked {
		return resp
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
	if err := h.audit.LogCritical(c.Request().Context(), ae); err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Audit failure", "server_error", ""))
	}

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
		if verrs := validatePolicyDocumentJSON(raw); len(verrs) > 0 {
			return c.JSON(http.StatusBadRequest, errJSON("Invalid policy document: "+strings.Join(verrs, "; "), "validation_error", "document"))
		}
		// Grant ceiling — broadening an (attached) policy beyond the
		// caller's own permissions is a live escalation, so the new document must
		// be within the caller's authority.
		if blocked, resp := h.ceilingBlocksRaw(c, raw); blocked {
			return resp
		}
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
	if err := h.audit.LogCritical(c.Request().Context(), ae); err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Audit failure", "server_error", ""))
	}

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
	if err := h.audit.LogCritical(c.Request().Context(), ae); err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Audit failure", "server_error", ""))
	}

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

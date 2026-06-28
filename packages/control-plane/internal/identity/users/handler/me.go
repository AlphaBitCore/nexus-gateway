package iam

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authn"
	iamengine "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/iam"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/users/userstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
)

// The "authn" import path has package name "auth" — all uses below
// reference functions as auth.Xxx (the compiled package name).

// RegisterMeRoutes registers /me, /me/permissions, PATCH /me, and
// /iam/action-catalog admin routes.
func (h *Handler) RegisterMeRoutes(g *echo.Group, iamMW func(action string) echo.MiddlewareFunc) {
	g.GET("/me", h.GetMe)                        // iam-exempt: self-service, returns the caller's own profile
	g.GET("/me/permissions", h.GetMePermissions) // iam-exempt: self-service, the caller's own permissions
	g.PATCH("/me", h.UpdateMe, iamMW(iam.ResourceSettings.Action(iam.VerbUpdate)))
	// IAM action catalog — pure metadata; any authenticated admin can read.
	g.GET("/iam/action-catalog", h.GetActionCatalog) // iam-exempt: static reference data (action/verb catalog)
}

// RegisterOrganizationTreeRoute registers the org tree endpoint.
func (h *Handler) RegisterOrganizationTreeRoute(g *echo.Group, iamMW func(action string) echo.MiddlewareFunc) {
	g.GET("/organizations/tree", h.OrganizationTree, iamMW(iam.ResourceOrganization.Action(iam.VerbRead)))
}

// meUserLookup is the narrow view of userstore.Store that GetMe needs for NexusUser.
type meUserLookup interface {
	FindNexusUserByID(ctx context.Context, id string) (*userstore.NexusUser, error)
}

// meGroupLookup is the narrow view of iamstore.Store that GetMe needs for IAM groups.
type meGroupLookup interface {
	ListGroupNamesForPrincipal(ctx context.Context, principalType, principalID string) ([]string, error)
}

// meResponse is the shape of GET /api/admin/me.
type meResponse struct {
	KeyID                 string   `json:"keyId"`
	KeyName               string   `json:"keyName"`
	Roles                 []string `json:"roles"`
	AuthPrincipalType     string   `json:"authPrincipalType"`
	Email                 string   `json:"email,omitempty"`
	DelegatedFromAPIKeyID string   `json:"delegatedFromApiKeyId,omitempty"`
	PreferredTimezone     string   `json:"preferredTimezone,omitempty"`
}

// GetMe returns the identity and role bindings for the currently-authenticated
// principal.
func (h *Handler) GetMe(c echo.Context) error {
	aa := middleware.AdminAuthFromContext(c)
	if aa == nil {
		return c.JSON(http.StatusUnauthorized, errJSON("Authentication required", "authentication_error", ""))
	}
	return buildMeResponse(c, aa, h.users, h.iam, h.logger)
}

// buildMeResponse is the testable core of GetMe.
func buildMeResponse(c echo.Context, aa *auth.AdminAuth, users meUserLookup, groups meGroupLookup, logger *slog.Logger) error {
	ctx := c.Request().Context()

	iamPrincipalType := aa.AuthPrincipalType
	if iamPrincipalType == "admin_user" {
		iamPrincipalType = "nexus_user"
	}
	roles, err := groups.ListGroupNamesForPrincipal(ctx, iamPrincipalType, aa.KeyID)
	if err != nil && logger != nil {
		logger.Warn("list group names for principal", "principalType", iamPrincipalType, "principalId", aa.KeyID, "error", err)
	}
	if roles == nil {
		roles = []string{}
	}

	resp := meResponse{
		KeyID:                 aa.KeyID,
		KeyName:               aa.KeyName,
		Roles:                 roles,
		AuthPrincipalType:     aa.AuthPrincipalType,
		DelegatedFromAPIKeyID: aa.DelegatedFromAPIKeyID,
	}

	if aa.AuthPrincipalType == "admin_user" {
		user, err := users.FindNexusUserByID(ctx, aa.KeyID)
		if err != nil && logger != nil {
			logger.Warn("find nexus user by id for /me", "keyId", aa.KeyID, "error", err)
		}
		if user != nil && user.Email != nil {
			resp.Email = *user.Email
		}
		if user != nil && user.PreferredTimezone != nil {
			resp.PreferredTimezone = *user.PreferredTimezone
		}
	}

	return c.JSON(http.StatusOK, resp)
}

// UpdateMe handles PATCH /api/admin/me — profile + password updates.
func (h *Handler) UpdateMe(c echo.Context) error {
	aa := middleware.AdminAuthFromContext(c)
	if aa == nil || aa.AuthPrincipalType != "admin_user" {
		return c.JSON(http.StatusForbidden, errJSON("Only admin users can update profile", "authorization_error", ""))
	}
	var body struct {
		DisplayName     *string `json:"displayName"`
		Email           *string `json:"email"`
		CurrentPassword *string `json:"currentPassword"`
		NewPassword     *string `json:"newPassword"`
	}
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("Invalid request body", "validation_error", ""))
	}

	params := userstore.UpdateNexusUserParams{
		DisplayName: body.DisplayName,
		Email:       body.Email,
	}

	if body.NewPassword != nil {
		if body.CurrentPassword == nil {
			return c.JSON(http.StatusBadRequest, errJSON("currentPassword is required to change password", "validation_error", ""))
		}
		existing, err := h.users.FindNexusUserByID(c.Request().Context(), aa.KeyID)
		if err != nil || existing == nil {
			return c.JSON(http.StatusInternalServerError, errJSON("Failed to load user", "server_error", ""))
		}
		// SSO accounts have no local password to change — guide them to their IdP
		// rather than failing the current-password check with a confusing message.
		if existing.Source != "local" {
			return c.JSON(http.StatusBadRequest, errJSON(
				"Your account signs in through single sign-on (SSO); there is no local password to change. Manage your credentials with your identity provider.",
				"sso_account", "newPassword"))
		}
		if existing.PasswordHash == nil || !auth.VerifyPassword(*body.CurrentPassword, *existing.PasswordHash) {
			return c.JSON(http.StatusUnauthorized, errJSON("Current password is incorrect", "authorization_error", ""))
		}
		newHash, err := auth.HashPassword(*body.NewPassword)
		if err != nil {
			h.logger.Error("hash new password", "error", err)
			return c.JSON(http.StatusInternalServerError, errJSON("Failed to hash password", "server_error", ""))
		}
		params.PasswordHash = &newHash
	}

	user, err := h.users.UpdateNexusUser(c.Request().Context(), aa.KeyID, params)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to update profile", "server_error", ""))
	}

	ae := audit.EntryFor(c, iam.ResourceUser, iam.VerbUpdate)
	ae.EntityID = aa.KeyID
	h.audit.LogObserved(c.Request().Context(), ae)

	return c.JSON(http.StatusOK, user)
}

// allAdminActions is the canonical set of fine-grained admin actions evaluated
// by GET /api/admin/me/permissions (from the shared iam.Catalog).
var allAdminActions = iam.AllActions()

// GetMePermissions evaluates the caller's IAM policy for every known admin
// action and returns the subset the principal is allowed to perform.
func (h *Handler) GetMePermissions(c echo.Context) error {
	aa := middleware.AdminAuthFromContext(c)
	if aa == nil {
		return c.JSON(http.StatusUnauthorized, errJSON("Authentication required", "authentication_error", ""))
	}

	if h.iamEngine == nil {
		return c.JSON(http.StatusOK, map[string]any{"actions": []string{}})
	}

	pt := aa.AuthPrincipalType
	if pt == "admin_user" {
		pt = "nexus_user"
	}

	ctx := c.Request().Context()
	condCtx := iamengine.ConditionContext{"nexus:SourceIp": c.RealIP()}

	h.iamEngine.InvalidateCache(pt, aa.KeyID)

	allowed := make([]string, 0, len(allAdminActions))
	for _, action := range allAdminActions {
		resourceService := "*"
		if svc, ok := iam.ServiceForAction(action); ok {
			resourceService = string(svc)
		}
		resource := iamengine.BuildNRN(resourceService, "*", "*", "*")
		result, err := h.iamEngine.Evaluate(ctx, pt, aa.KeyID, action, resource, condCtx)
		if err != nil {
			continue
		}
		if result.Decision == "Allow" {
			allowed = append(allowed, action)
		}
	}
	return c.JSON(http.StatusOK, map[string]any{"actions": allowed})
}

type actionCatalogEntry struct {
	Type    string                `json:"type"`
	Service string                `json:"service"`
	NRN     string                `json:"nrn"`
	Actions []actionCatalogAction `json:"actions"`
}

type actionCatalogAction struct {
	Verb string `json:"verb"`
	Name string `json:"name"`
	SIEM string `json:"siem"`
}

// GetActionCatalog returns the canonical IAM resource × verb table.
func (h *Handler) GetActionCatalog(c echo.Context) error {
	out := make([]actionCatalogEntry, 0, len(iam.Catalog))
	for i := range iam.Catalog {
		r := &iam.Catalog[i]
		actions := make([]actionCatalogAction, 0, len(r.Verbs))
		for _, v := range r.Verbs {
			actions = append(actions, actionCatalogAction{
				Verb: string(v),
				Name: r.Action(v),
				SIEM: iam.SIEMEventType(r.Name, v),
			})
		}
		out = append(out, actionCatalogEntry{
			Type:    r.Name,
			Service: string(r.Service),
			NRN:     r.NRN("*", "*"),
			Actions: actions,
		})
	}
	return c.JSON(http.StatusOK, map[string]any{"resources": out})
}

// OrganizationTree returns all orgs as a nested tree with quota + cost data.
func (h *Handler) OrganizationTree(c echo.Context) error {
	ctx := c.Request().Context()

	orgs, err := h.orgs.ListOrganizations(ctx)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}

	overrideLimits := make(map[string]float64)
	if overRows, qErr := h.pool.Query(ctx, `
		SELECT "targetId", "costLimitUsd"::double precision
		FROM "QuotaOverride"
		WHERE "targetType" = 'organization' AND "costLimitUsd" IS NOT NULL
	`); qErr == nil {
		defer overRows.Close() //nolint:errcheck
		for overRows.Next() {
			var targetID string
			var lim float64
			if scanErr := overRows.Scan(&targetID, &lim); scanErr == nil {
				overrideLimits[targetID] = lim
			}
		}
	}

	policyLimits := make(map[string]float64)
	if polRows, qErr := h.pool.Query(ctx, `
		SELECT "organizationId", MIN("costLimitUsd"::double precision)
		FROM "QuotaPolicy"
		WHERE scope = 'organization' AND enabled = true
		  AND "organizationId" IS NOT NULL AND "costLimitUsd" IS NOT NULL
		GROUP BY "organizationId"
	`); qErr == nil {
		defer polRows.Close() //nolint:errcheck
		for polRows.Next() {
			var orgID string
			var lim float64
			if scanErr := polRows.Scan(&orgID, &lim); scanErr == nil {
				policyLimits[orgID] = lim
			}
		}
	}

	costUsed := make(map[string]float64)
	if costRows, qErr := h.pool.Query(ctx, `
		SELECT "dimensionKey", SUM(value)
		FROM "metric_rollup_1h"
		WHERE "bucketStart" >= date_trunc('month', NOW() AT TIME ZONE 'UTC')
		  AND "metricName" = 'billed_cost_usd'
		  AND "dimensionKey" LIKE 'organization=%'
		GROUP BY "dimensionKey"
	`); qErr == nil {
		defer costRows.Close() //nolint:errcheck
		for costRows.Next() {
			var dimKey string
			var cost float64
			if scanErr := costRows.Scan(&dimKey, &cost); scanErr == nil {
				orgID := strings.TrimPrefix(dimKey, "organization=")
				costUsed[orgID] = cost
			}
		}
	}

	type treeNode struct {
		ID            string      `json:"id"`
		Name          string      `json:"name"`
		Code          string      `json:"code"`
		ParentID      *string     `json:"parentId"`
		Enabled       bool        `json:"enabled"`
		ChildCount    *int        `json:"childCount"`
		ProjectCount  *int        `json:"projectCount"`
		UserCount     *int        `json:"userCount"`
		QuotaLimitUsd *float64    `json:"quotaLimitUsd"`
		QuotaUsedUsd  float64     `json:"quotaUsedUsd"`
		Children      []*treeNode `json:"children"`
	}

	nodeMap := make(map[string]*treeNode)
	for _, o := range orgs {
		var quotaLimit *float64
		if lim, ok := overrideLimits[o.ID]; ok {
			quotaLimit = &lim
		} else if lim, ok := policyLimits[o.ID]; ok {
			quotaLimit = &lim
		}

		nodeMap[o.ID] = &treeNode{
			ID: o.ID, Name: o.Name, Code: o.Code, ParentID: o.ParentID,
			Enabled: o.Enabled, ChildCount: o.ChildCount, ProjectCount: o.ProjectCount,
			UserCount:     o.UserCount,
			QuotaLimitUsd: quotaLimit,
			QuotaUsedUsd:  costUsed[o.ID],
			Children:      []*treeNode{},
		}
	}

	var roots []*treeNode
	for _, n := range nodeMap {
		if n.ParentID != nil {
			if parent, ok := nodeMap[*n.ParentID]; ok {
				parent.Children = append(parent.Children, n)
				continue
			}
		}
		roots = append(roots, n)
	}
	return c.JSON(http.StatusOK, map[string]any{"data": roots})
}

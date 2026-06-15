package scim

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/scim/scimstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/users/iamstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/users/userstore"
)

const (
	scimContentType = "application/scim+json"
	scimSchemaUser  = "urn:ietf:params:scim:schemas:core:2.0:User"
	scimSchemaGroup = "urn:ietf:params:scim:schemas:core:2.0:Group"
	scimSchemaList  = "urn:ietf:params:scim:api:messages:2.0:ListResponse"
	scimSchemaError = "urn:ietf:params:scim:api:messages:2.0:Error"
	scimSchemaPatch = "urn:ietf:params:scim:api:messages:2.0:PatchOp"
	scimSchemaSPC   = "urn:ietf:params:scim:schemas:core:2.0:ServiceProviderConfig"
	scimBaseOrg     = "urn:ietf:params:scim:schemas:extension:enterprise:2.0:User"
)

// scimUserStore is the narrow userstore surface the SCIM handler needs.
type scimUserStore interface {
	ListNexusUsers(ctx context.Context, p userstore.NexusUserListParams) ([]userstore.NexusUserSafe, int, error)
	GetNexusUserSafe(ctx context.Context, id string) (*userstore.NexusUserSafe, error)
	FindNexusUserByEmail(ctx context.Context, email string) (*userstore.NexusUser, error)
	CreateNexusUser(ctx context.Context, p userstore.CreateNexusUserParams) (*userstore.NexusUserSafe, error)
	UpdateNexusUser(ctx context.Context, id string, p userstore.UpdateNexusUserParams) (*userstore.NexusUserSafe, error)
	FindDefaultOrganizationID(ctx context.Context) (string, error)
}

// scimIAMStore is the narrow iamstore surface the SCIM handler needs.
type scimIAMStore interface {
	ListIamGroups(ctx context.Context) ([]iamstore.GroupRow, error)
	GetIamGroup(ctx context.Context, id string) (*iamstore.GroupRow, error)
	UpdateIamGroup(ctx context.Context, id string, p iamstore.UpdateIamGroupParams) (*iamstore.GroupRow, error)
	DeleteIamGroup(ctx context.Context, id string) error
	AddGroupMember(ctx context.Context, groupID, principalType, principalID string) (string, error)
	RemoveGroupMember(ctx context.Context, membershipID string) error
	RemoveGroupMemberByPrincipal(ctx context.Context, groupID, principalType, principalID string) error
	ListGroupMembersRaw(ctx context.Context, groupID string) ([]map[string]string, error)
}

// scimTokenStore is the narrow scimstore surface the SCIM handler needs.
type scimTokenStore interface {
	GetScimTokenByHash(ctx context.Context, hash string) (*scimstore.ScimToken, error)
	TouchScimToken(ctx context.Context, id string)
	LinkUserToIdP(ctx context.Context, userID, idpID, externalSubject string, externalEmail *string) error
	FindIdpGroupMappingByExternal(ctx context.Context, idpID, externalGroupID string) (*scimstore.IdpGroupMapping, error)
	CreateScimIamGroup(ctx context.Context, name string, description *string, idpID, createdBy string) (*iamstore.GroupRow, error)
	CreateIdpGroupMapping(ctx context.Context, p scimstore.CreateIdpGroupMappingParams) (*scimstore.IdpGroupMapping, error)
	GetIamGroupSource(ctx context.Context, id string) (source string, idpID *string, err error)
	UserOwnedByIdP(ctx context.Context, userID, idpID string) (bool, error)
}

// Handler handles SCIM 2.0 provisioning endpoints mounted at /scim/v2/.
// Authentication uses a Bearer token looked up by SHA-256 hash in ScimToken table.
type Handler struct {
	users  scimUserStore
	iam    scimIAMStore
	scim   scimTokenStore
	Logger *slog.Logger
	// BaseURL is the canonical SCIM base URL returned in meta.location fields.
	BaseURL string
}

// scimPool is the minimal pgx surface the SCIM handler needs for sub-store construction.
type scimPool interface {
	userstore.PgxPool
}

// New constructs a Handler from a pool.
func New(pool scimPool, logger *slog.Logger, baseURL string) *Handler {
	h := &Handler{Logger: logger, BaseURL: baseURL}
	if pool != nil {
		h.users = userstore.New(pool)
		h.iam = iamstore.New(pool)
		h.scim = scimstore.New(pool)
	}
	return h
}

// RegisterSCIMRoutes mounts all SCIM 2.0 routes on the given echo group.
// The group must NOT carry any pre-existing auth middleware — SCIM uses its own
// Bearer-token authentication via scimAuth().
func (h *Handler) RegisterSCIMRoutes(g *echo.Group) {
	g.Use(h.scimAuth)

	g.GET("/ServiceProviderConfig", h.ServiceProviderConfig)
	g.GET("/Schemas", h.Schemas)
	g.GET("/Schemas/:id", h.SchemaByID)

	g.GET("/Users", h.ListUsers)
	g.GET("/Users/:id", h.GetUser)
	g.POST("/Users", h.CreateUser)
	g.PUT("/Users/:id", h.ReplaceUser)
	g.PATCH("/Users/:id", h.PatchUser)
	g.DELETE("/Users/:id", h.DeleteUser)

	g.GET("/Groups", h.ListGroups)
	g.GET("/Groups/:id", h.GetGroup)
	g.POST("/Groups", h.CreateGroup)
	g.PUT("/Groups/:id", h.ReplaceGroup)
	g.PATCH("/Groups/:id", h.PatchGroup)
	g.DELETE("/Groups/:id", h.DeleteGroup)
}

// Auth middleware

func (h *Handler) scimAuth(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		auth := c.Request().Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			return h.scimError(c, http.StatusUnauthorized, "missing bearer token", "invalidCredentials")
		}
		raw := strings.TrimPrefix(auth, "Bearer ")
		tokenHash := scimstore.HashScimToken(raw)
		tok, err := h.scim.GetScimTokenByHash(c.Request().Context(), tokenHash)
		if err != nil || tok == nil {
			return h.scimError(c, http.StatusUnauthorized, "invalid or revoked token", "invalidCredentials")
		}
		h.scim.TouchScimToken(c.Request().Context(), tok.ID)
		c.Set("scimToken", tok)
		return next(c)
	}
}

// Metadata endpoints

func (h *Handler) ServiceProviderConfig(c echo.Context) error {
	return c.JSON(http.StatusOK, map[string]any{
		"schemas":          []string{scimSchemaSPC},
		"documentationUri": "",
		"patch":            map[string]any{"supported": true},
		"bulk":             map[string]any{"supported": false, "maxOperations": 0, "maxPayloadSize": 0},
		"filter":           map[string]any{"supported": true, "maxResults": 200},
		"changePassword":   map[string]any{"supported": false},
		"sort":             map[string]any{"supported": false},
		"etag":             map[string]any{"supported": false},
		"authenticationSchemes": []map[string]any{
			{"type": "oauthbearertoken", "name": "OAuth Bearer Token", "description": "Bearer token issued via the Nexus Gateway admin console."},
		},
		"meta": map[string]any{
			"resourceType": "ServiceProviderConfig",
			"created":      "2026-01-01T00:00:00Z",
			"lastModified": "2026-01-01T00:00:00Z",
			"location":     h.BaseURL + "/ServiceProviderConfig",
		},
	})
}

func (h *Handler) Schemas(c echo.Context) error {
	return c.JSON(http.StatusOK, map[string]any{
		"schemas":      []string{scimSchemaList},
		"totalResults": 2,
		"Resources":    []any{userSchema(), groupSchema()},
	})
}

func (h *Handler) SchemaByID(c echo.Context) error {
	switch c.Param("id") {
	case scimSchemaUser:
		return c.JSON(http.StatusOK, userSchema())
	case scimSchemaGroup:
		return c.JSON(http.StatusOK, groupSchema())
	default:
		return h.scimError(c, http.StatusNotFound, "schema not found", "noTarget")
	}
}

func (h *Handler) ListUsers(c echo.Context) error {
	ctx := c.Request().Context()
	startIndex := queryInt(c, "startIndex", 1)
	count := queryInt(c, "count", 100)
	if count > 200 {
		count = 200
	}
	offset := 0
	if startIndex > 1 {
		offset = startIndex - 1
	}

	params := userstore.NexusUserListParams{Limit: count, Offset: offset}
	// SCIM filter: userName eq "user@example.com"
	if f := c.QueryParam("filter"); strings.Contains(f, "userName eq ") {
		email := strings.Trim(strings.TrimPrefix(f, "userName eq "), `"`)
		params.Q = email
	}
	// Scope enumeration to the calling token's IdP so a per-IdP SCIM
	// token cannot list (and harvest the ids/emails of) users owned by another
	// IdP or local admins. A global/admin token (no IdP) is unrestricted.
	if tok, ok := c.Get("scimToken").(*scimstore.ScimToken); ok && tok != nil {
		params.OwnedByIdP = tok.IdentityProviderID
	}

	users, total, err := h.users.ListNexusUsers(ctx, params)
	if err != nil {
		return h.scimError(c, http.StatusInternalServerError, "internal error", "serverError")
	}

	resources := make([]any, len(users))
	for i, u := range users {
		resources[i] = h.userToSCIM(&u)
	}
	return c.JSON(http.StatusOK, map[string]any{
		"schemas":      []string{scimSchemaList},
		"totalResults": total,
		"startIndex":   startIndex,
		"itemsPerPage": len(resources),
		"Resources":    resources,
	})
}

func (h *Handler) GetUser(c echo.Context) error {
	ctx := c.Request().Context()
	tok, _ := c.Get("scimToken").(*scimstore.ScimToken)
	// A SCIM token may only read users its own IdP provisioned. The
	// guard writes the error response itself; a nil user means it denied.
	u, errResp := h.assertScimUser(c, ctx, c.Param("id"), tok)
	if u == nil {
		return errResp
	}
	return c.JSON(http.StatusOK, h.userToSCIM(u))
}

func (h *Handler) CreateUser(c echo.Context) error {
	ctx := c.Request().Context()
	var body scimUserBody
	if err := bindSCIMBody(c, &body); err != nil {
		return h.scimError(c, http.StatusBadRequest, "invalid request body", "invalidValue")
	}
	if body.UserName == "" {
		return h.scimError(c, http.StatusBadRequest, "userName is required", "invalidValue")
	}
	// Check for existing user by email.
	existing, _ := h.users.FindNexusUserByEmail(ctx, body.UserName)
	if existing != nil {
		return h.scimError(c, http.StatusConflict, "user already exists", "uniqueness")
	}

	displayName := body.DisplayName
	if displayName == "" {
		displayName = body.UserName
	}
	// The NexusUser.organizationId column carries a DB default of
	// `'default'::text` but the seed never inserts an Organization
	// row with id='default'. Resolve a real org (root org → earliest)
	// instead of trusting the column default. See
	// userstore.FindDefaultOrganizationID for the resolution order.
	orgID, err := h.users.FindDefaultOrganizationID(ctx)
	if err != nil {
		return h.scimError(c, http.StatusInternalServerError,
			"resolve default organization: "+err.Error(), "serverError")
	}
	if orgID == "" {
		return h.scimError(c, http.StatusInternalServerError,
			"no organizations exist — SCIM cannot provision a user", "serverError")
	}
	canAccess := false
	u, err := h.users.CreateNexusUser(ctx, userstore.CreateNexusUserParams{
		DisplayName:           displayName,
		Email:                 &body.UserName,
		OrganizationID:        &orgID,
		CanAccessControlPlane: &canAccess,
		CreatedBy:             "scim",
		Source:                "scim",
	})
	if err != nil {
		return h.scimError(c, http.StatusInternalServerError, fmt.Sprintf("create user: %v", err), "serverError")
	}
	// Stamp the new user with a UserFederatedIdentity row pointing at the IdP
	// the SCIM token was scoped to. The external subject is the IdP-side ID
	// (`externalId` in SCIM) when present, else the SCIM userName. Failure is
	// non-fatal — the user is already created; we log and continue so the SCIM
	// operation stays RFC-compliant.
	if tok, ok := c.Get("scimToken").(*scimstore.ScimToken); ok && tok != nil && tok.IdentityProviderID != nil && *tok.IdentityProviderID != "" {
		externalSubject := body.ExternalID
		if externalSubject == "" {
			externalSubject = body.UserName
		}
		if linkErr := h.scim.LinkUserToIdP(ctx, u.ID, *tok.IdentityProviderID, externalSubject, &body.UserName); linkErr != nil {
			h.Logger.Warn("scim: link user to IdP failed", "user_id", u.ID, "idp_id", *tok.IdentityProviderID, "error", linkErr)
		}
	}
	c.Response().Header().Set("Location", h.BaseURL+"/Users/"+u.ID)
	return c.JSON(http.StatusCreated, h.userToSCIM(u))
}

func (h *Handler) ReplaceUser(c echo.Context) error {
	ctx := c.Request().Context()
	id := c.Param("id")
	var body scimUserBody
	if err := bindSCIMBody(c, &body); err != nil {
		return h.scimError(c, http.StatusBadRequest, "invalid request body", "invalidValue")
	}

	// Only the owning IdP's token may mutate this user.
	tok, _ := c.Get("scimToken").(*scimstore.ScimToken)
	if u, errResp := h.assertScimUser(c, ctx, id, tok); u == nil {
		return errResp
	}

	active := body.Active == nil || *body.Active
	status := "active"
	if !active {
		status = "suspended"
	}
	name := body.DisplayName
	email := body.UserName

	u, err := h.users.UpdateNexusUser(ctx, id, userstore.UpdateNexusUserParams{
		DisplayName: &name,
		Email:       &email,
		Status:      &status,
	})
	if err != nil {
		return h.scimError(c, http.StatusInternalServerError, "update user: "+err.Error(), "serverError")
	}
	if u == nil {
		return h.scimError(c, http.StatusNotFound, "user not found", "noTarget")
	}
	return c.JSON(http.StatusOK, h.userToSCIM(u))
}

func (h *Handler) PatchUser(c echo.Context) error {
	ctx := c.Request().Context()
	id := c.Param("id")
	var body scimPatchBody
	if err := bindSCIMBody(c, &body); err != nil {
		return h.scimError(c, http.StatusBadRequest, "invalid patch body", "invalidValue")
	}

	// Only the owning IdP's token may patch this user (blocks the
	// cross-IdP email-rewrite account-takeover and active=false DoS).
	tok, _ := c.Get("scimToken").(*scimstore.ScimToken)
	if u, errResp := h.assertScimUser(c, ctx, id, tok); u == nil {
		return errResp
	}

	params := userstore.UpdateNexusUserParams{}
	for _, op := range body.Operations {
		switch strings.ToLower(op.Op) {
		case "replace":
			applyUserPatchOp(&params, op.Path, op.Value)
		case "add":
			applyUserPatchOp(&params, op.Path, op.Value)
		}
	}

	u, err := h.users.UpdateNexusUser(ctx, id, params)
	if err != nil {
		return h.scimError(c, http.StatusInternalServerError, "update user: "+err.Error(), "serverError")
	}
	if u == nil {
		return h.scimError(c, http.StatusNotFound, "user not found", "noTarget")
	}
	return c.JSON(http.StatusOK, h.userToSCIM(u))
}

func (h *Handler) DeleteUser(c echo.Context) error {
	ctx := c.Request().Context()
	id := c.Param("id")
	// Only the owning IdP's token may deprovision this user.
	tok, _ := c.Get("scimToken").(*scimstore.ScimToken)
	if u, errResp := h.assertScimUser(c, ctx, id, tok); u == nil {
		return errResp
	}
	// Deprovision = suspend, not hard delete.
	status := "suspended"
	_, err := h.users.UpdateNexusUser(ctx, id, userstore.UpdateNexusUserParams{Status: &status})
	if err != nil {
		return h.scimError(c, http.StatusInternalServerError, "deprovision user: "+err.Error(), "serverError")
	}
	return c.NoContent(http.StatusNoContent)
}

func (h *Handler) ListGroups(c echo.Context) error {
	ctx := c.Request().Context()
	groups, err := h.iam.ListIamGroups(ctx)
	if err != nil {
		return h.scimError(c, http.StatusInternalServerError, "internal error", "serverError")
	}
	resources := make([]any, len(groups))
	for i, g := range groups {
		members, _ := h.groupMembers(ctx, g.ID)
		resources[i] = h.groupToSCIM(&g, members)
	}
	return c.JSON(http.StatusOK, map[string]any{
		"schemas":      []string{scimSchemaList},
		"totalResults": len(resources),
		"startIndex":   1,
		"itemsPerPage": len(resources),
		"Resources":    resources,
	})
}

func (h *Handler) GetGroup(c echo.Context) error {
	ctx := c.Request().Context()
	g, err := h.iam.GetIamGroup(ctx, c.Param("id"))
	if err != nil || g == nil {
		return h.scimError(c, http.StatusNotFound, "group not found", "noTarget")
	}
	members, _ := h.groupMembers(ctx, g.ID)
	return c.JSON(http.StatusOK, h.groupToSCIM(g, members))
}

// CreateGroup provisions a SCIM Group pushed by an external IdP.
//
// Resolution order:
//  1. Look up IdpGroupMapping(scimToken.IdentityProviderID, body.ExternalID).
//     If present, the SCIM Group is an alias for the pre-existing admin-
//     configured Nexus IamGroup — members land in that group; the response
//     uses the mapped IamGroup ID.
//  2. Otherwise create a new IamGroup tagged source='scim' +
//     identity_provider_id=<idpId>, then auto-backfill the mapping so
//     future PATCHes route to the same group. The admin can later re-point
//     the mapping at a different IamGroup with appropriate policies.
//
// The SCIM token's IdentityProviderID is required: an IdP-scoped token
// is the only meaningful way to identify which external IdP is doing
// the pushing. Unscoped tokens get 400.
func (h *Handler) CreateGroup(c echo.Context) error {
	ctx := c.Request().Context()
	var body scimGroupBody
	if err := bindSCIMBody(c, &body); err != nil {
		return h.scimError(c, http.StatusBadRequest, "invalid request body", "invalidValue")
	}
	if body.DisplayName == "" {
		return h.scimError(c, http.StatusBadRequest, "displayName is required", "invalidValue")
	}

	tok, _ := c.Get("scimToken").(*scimstore.ScimToken)
	if tok == nil || tok.IdentityProviderID == nil || *tok.IdentityProviderID == "" {
		return h.scimError(c, http.StatusBadRequest,
			"SCIM token must be scoped to an IdP to provision groups", "invalidValue")
	}
	idpID := *tok.IdentityProviderID

	// Step 1: existing mapping for this external group → route to it.
	if body.ExternalID != "" {
		mapping, err := h.scim.FindIdpGroupMappingByExternal(ctx, idpID, body.ExternalID)
		if err != nil {
			return h.scimError(c, http.StatusInternalServerError, "lookup group mapping: "+err.Error(), "serverError")
		}
		if mapping != nil {
			for _, m := range body.Members {
				_, _ = h.iam.AddGroupMember(ctx, mapping.IamGroupID, "nexus_user", m.Value)
			}
			g, _ := h.iam.GetIamGroup(ctx, mapping.IamGroupID)
			members, _ := h.groupMembers(ctx, mapping.IamGroupID)
			c.Response().Header().Set("Location", h.BaseURL+"/Groups/"+mapping.IamGroupID)
			return c.JSON(http.StatusCreated, h.groupToSCIM(g, members))
		}
	}

	// Step 2: no mapping — auto-create a SCIM-managed group and back-fill.
	g, err := h.scim.CreateScimIamGroup(ctx, body.DisplayName, nil, idpID, "scim")
	if err != nil {
		return h.scimError(c, http.StatusInternalServerError, "create group: "+err.Error(), "serverError")
	}
	if body.ExternalID != "" {
		extName := body.DisplayName
		if _, mapErr := h.scim.CreateIdpGroupMapping(ctx, scimstore.CreateIdpGroupMappingParams{
			IdentityProviderID: idpID,
			ExternalGroupID:    body.ExternalID,
			ExternalGroupName:  &extName,
			IamGroupID:         g.ID,
		}); mapErr != nil {
			h.Logger.Warn("scim: auto-create group mapping failed",
				"idp_id", idpID, "external_group_id", body.ExternalID, "error", mapErr)
		}
	}
	for _, m := range body.Members {
		_, _ = h.iam.AddGroupMember(ctx, g.ID, "nexus_user", m.Value)
	}
	members, _ := h.groupMembers(ctx, g.ID)
	c.Response().Header().Set("Location", h.BaseURL+"/Groups/"+g.ID)
	return c.JSON(http.StatusCreated, h.groupToSCIM(g, members))
}

// assertScimGroup refuses to mutate an IamGroup whose source is not
// 'scim'. Prevents an external IdP with a stale/guessed Group id from
// clobbering a manually-configured admin group. Returns the source
// row for the caller to use; non-nil error response is returned when
// the check fails (handler should propagate as-is).
// assertScimUser enforces SCIM per-user ownership, the User-path
// equivalent of assertScimGroup. The target user must (1) exist, (2) be
// SCIM-provisioned (Source == "scim"; local / oidc / saml accounts are
// admin-managed and out of SCIM's reach), and (3) when the token is IdP-scoped,
// be owned by the token's IdP via a UserFederatedIdentity link. Without this a
// SCIM token minted for IdP-A could read, re-email (account takeover), suspend,
// or deprovision any user — including local super-admins and users from IdP-B.
// On success it returns the loaded user; otherwise the echo error to return.
func (h *Handler) assertScimUser(c echo.Context, ctx context.Context, id string, tok *scimstore.ScimToken) (*userstore.NexusUserSafe, error) {
	u, err := h.users.GetNexusUserSafe(ctx, id)
	if err != nil {
		return nil, h.scimError(c, http.StatusInternalServerError, "lookup user: "+err.Error(), "serverError")
	}
	if u == nil {
		return nil, h.scimError(c, http.StatusNotFound, "user not found", "noTarget")
	}
	if u.Source != "scim" {
		return nil, h.scimError(c, http.StatusForbidden,
			"target NexusUser is admin-managed; SCIM cannot mutate it", "mutability")
	}
	// Token must own the user: the IdP that provisioned it is the only IdP
	// allowed to keep managing it. A token with no IdP scope (a global/admin
	// token) skips this check, mirroring assertScimGroup.
	if tok != nil && tok.IdentityProviderID != nil && *tok.IdentityProviderID != "" {
		owned, oerr := h.scim.UserOwnedByIdP(ctx, id, *tok.IdentityProviderID)
		if oerr != nil {
			return nil, h.scimError(c, http.StatusInternalServerError, "verify ownership: "+oerr.Error(), "serverError")
		}
		if !owned {
			return nil, h.scimError(c, http.StatusForbidden,
				"SCIM token's IdP does not own this user", "noPermission")
		}
	}
	return u, nil
}

func (h *Handler) assertScimGroup(c echo.Context, ctx context.Context, id string, tok *scimstore.ScimToken) (source string, idpID *string, errResp error) {
	src, ipid, err := h.scim.GetIamGroupSource(ctx, id)
	if err != nil {
		return "", nil, h.scimError(c, http.StatusInternalServerError, "lookup group: "+err.Error(), "serverError")
	}
	if src == "" {
		return "", nil, h.scimError(c, http.StatusNotFound, "group not found", "noTarget")
	}
	if src != "scim" {
		return "", nil, h.scimError(c, http.StatusForbidden,
			"target IamGroup is admin-managed; SCIM cannot mutate it", "mutability")
	}
	// Token must own the group: the IdP that pushed it is the only IdP
	// allowed to keep pushing into it.
	if tok != nil && tok.IdentityProviderID != nil && ipid != nil && *tok.IdentityProviderID != *ipid {
		return "", nil, h.scimError(c, http.StatusForbidden,
			"SCIM token's IdP does not own this group", "noPermission")
	}
	return src, ipid, nil
}

func (h *Handler) ReplaceGroup(c echo.Context) error {
	ctx := c.Request().Context()
	id := c.Param("id")
	var body scimGroupBody
	if err := bindSCIMBody(c, &body); err != nil {
		return h.scimError(c, http.StatusBadRequest, "invalid request body", "invalidValue")
	}
	tok, _ := c.Get("scimToken").(*scimstore.ScimToken)
	if _, _, errResp := h.assertScimGroup(c, ctx, id, tok); errResp != nil {
		return errResp
	}
	g, err := h.iam.UpdateIamGroup(ctx, id, iamstore.UpdateIamGroupParams{Name: &body.DisplayName})
	if err != nil || g == nil {
		return h.scimError(c, http.StatusNotFound, "group not found", "noTarget")
	}
	// Replace all members: remove existing then re-add.
	existingMembers, _ := h.groupMembers(ctx, id)
	for _, m := range existingMembers {
		_ = h.iam.RemoveGroupMember(ctx, m["membershipId"])
	}
	for _, m := range body.Members {
		_, _ = h.iam.AddGroupMember(ctx, id, "nexus_user", m.Value)
	}
	members, _ := h.groupMembers(ctx, id)
	return c.JSON(http.StatusOK, h.groupToSCIM(g, members))
}

func (h *Handler) PatchGroup(c echo.Context) error {
	ctx := c.Request().Context()
	id := c.Param("id")
	var body scimPatchBody
	if err := bindSCIMBody(c, &body); err != nil {
		return h.scimError(c, http.StatusBadRequest, "invalid patch body", "invalidValue")
	}

	tok, _ := c.Get("scimToken").(*scimstore.ScimToken)
	if _, _, errResp := h.assertScimGroup(c, ctx, id, tok); errResp != nil {
		return errResp
	}

	g, err := h.iam.GetIamGroup(ctx, id)
	if err != nil || g == nil {
		return h.scimError(c, http.StatusNotFound, "group not found", "noTarget")
	}

	for _, op := range body.Operations {
		switch strings.ToLower(op.Op) {
		case "add":
			if strings.EqualFold(op.Path, "members") {
				for _, m := range parseMembersValue(op.Value) {
					_, _ = h.iam.AddGroupMember(ctx, id, "nexus_user", m)
				}
			}
		case "remove":
			if strings.HasPrefix(strings.ToLower(op.Path), "members") {
				// members[value eq "<userId>"]
				if userID := parseMemberFilterValue(op.Path); userID != "" {
					_ = h.iam.RemoveGroupMemberByPrincipal(ctx, id, "nexus_user", userID)
				}
			}
		case "replace":
			if strings.EqualFold(op.Path, "displayname") {
				name := fmt.Sprintf("%v", op.Value)
				_, _ = h.iam.UpdateIamGroup(ctx, id, iamstore.UpdateIamGroupParams{Name: &name})
			}
		}
	}

	updated, _ := h.iam.GetIamGroup(ctx, id)
	members, _ := h.groupMembers(ctx, id)
	return c.JSON(http.StatusOK, h.groupToSCIM(updated, members))
}

func (h *Handler) DeleteGroup(c echo.Context) error {
	ctx := c.Request().Context()
	if err := h.iam.DeleteIamGroup(ctx, c.Param("id")); err != nil {
		return h.scimError(c, http.StatusNotFound, "group not found", "noTarget")
	}
	return c.NoContent(http.StatusNoContent)
}

// Conversion helpers

func (h *Handler) userToSCIM(u *userstore.NexusUserSafe) map[string]any {
	active := u.Status == "active"
	email := ""
	if u.Email != nil {
		email = *u.Email
	}
	return map[string]any{
		"schemas":     []string{scimSchemaUser},
		"id":          u.ID,
		"userName":    email,
		"displayName": u.DisplayName,
		"active":      active,
		"emails":      []map[string]any{{"value": email, "primary": true}},
		"meta": map[string]any{
			"resourceType": "User",
			"created":      u.CreatedAt.Format(time.RFC3339),
			"lastModified": u.UpdatedAt.Format(time.RFC3339),
			"location":     h.BaseURL + "/Users/" + u.ID,
		},
	}
}

func (h *Handler) groupToSCIM(g *iamstore.GroupRow, members []map[string]string) map[string]any {
	scimMembers := make([]map[string]any, 0, len(members))
	for _, m := range members {
		scimMembers = append(scimMembers, map[string]any{
			"value":   m["userId"],
			"display": m["displayName"],
			"$ref":    h.BaseURL + "/Users/" + m["userId"],
		})
	}
	return map[string]any{
		"schemas":     []string{scimSchemaGroup},
		"id":          g.ID,
		"displayName": g.Name,
		"members":     scimMembers,
		"meta": map[string]any{
			"resourceType": "Group",
			"created":      g.CreatedAt.Format(time.RFC3339),
			"lastModified": g.UpdatedAt.Format(time.RFC3339),
			"location":     h.BaseURL + "/Groups/" + g.ID,
		},
	}
}

func (h *Handler) groupMembers(ctx context.Context, groupID string) ([]map[string]string, error) {
	rows, err := h.iam.ListGroupMembersRaw(ctx, groupID)
	return rows, err
}

// bindSCIMBody decodes a JSON body into `dst`, accepting both
// "application/json" and the RFC 7644 mandated "application/scim+json"
// content type. Echo's DefaultBinder switches on exact mediatype match
// (`application/json` only) and returns ErrUnsupportedMediaType for the
// `+json` suffix — which Okta, Azure AD, and Google Workspace all send.
// We decode directly here so the SCIM handler is RFC-compliant without
// the rest of the CP needing a custom binder.
func bindSCIMBody(c echo.Context, dst any) error {
	body := c.Request().Body
	if body == nil {
		return fmt.Errorf("empty body")
	}
	defer func() { _ = body.Close() }()
	dec := json.NewDecoder(body)
	return dec.Decode(dst)
}

func (h *Handler) scimError(c echo.Context, status int, detail, scimType string) error {
	return c.JSON(status, map[string]any{
		"schemas":  []string{scimSchemaError},
		"status":   strconv.Itoa(status),
		"scimType": scimType,
		"detail":   detail,
	})
}

// Request body types

type scimUserBody struct {
	Schemas     []string `json:"schemas"`
	UserName    string   `json:"userName"`
	DisplayName string   `json:"displayName"`
	Active      *bool    `json:"active"`
	// ExternalID is the IdP-side stable identifier (RFC 7643 §3.1).
	// Used as the externalSubject when stamping UserFederatedIdentity.
	ExternalID string `json:"externalId"`
}

type scimGroupBody struct {
	Schemas     []string     `json:"schemas"`
	DisplayName string       `json:"displayName"`
	Members     []scimMember `json:"members"`
	// ExternalID is the IdP-side stable group identifier (RFC 7643 §4.2).
	// Used to look up IdpGroupMapping at provision time so admin-configured
	// "Okta group → Nexus IAM group" routing actually takes effect.
	ExternalID string `json:"externalId"`
}

type scimMember struct {
	Value   string `json:"value"`
	Display string `json:"display"`
}

type scimPatchBody struct {
	Schemas    []string      `json:"schemas"`
	Operations []scimPatchOp `json:"Operations"`
}

type scimPatchOp struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value any    `json:"value"`
}

// Patch helpers

func applyUserPatchOp(params *userstore.UpdateNexusUserParams, path string, value any) {
	switch strings.ToLower(path) {
	case "active":
		active := parseBoolValue(value)
		status := "active"
		if !active {
			status = "suspended"
		}
		params.Status = &status
	case "displayname":
		s := fmt.Sprintf("%v", value)
		params.DisplayName = &s
	case "username", "emails[type eq \"work\"].value":
		s := fmt.Sprintf("%v", value)
		params.Email = &s
	}
}

func parseBoolValue(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		return strings.EqualFold(x, "true")
	}
	return false
}

func parseMembersValue(v any) []string {
	var out []string
	if x, ok := v.([]any); ok {
		for _, item := range x {
			if m, ok := item.(map[string]any); ok {
				if id, ok := m["value"].(string); ok {
					out = append(out, id)
				}
			}
		}
	}
	return out
}

// parseMemberFilterValue extracts user ID from SCIM path like members[value eq "userId"]
func parseMemberFilterValue(path string) string {
	start := strings.Index(path, `"`)
	end := strings.LastIndex(path, `"`)
	if start >= 0 && end > start {
		return path[start+1 : end]
	}
	return ""
}

func queryInt(c echo.Context, key string, def int) int {
	v := c.QueryParam(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return def
	}
	return n
}

// SCIM schema definitions

func userSchema() map[string]any {
	return map[string]any{
		"id":          scimSchemaUser,
		"name":        "User",
		"description": "User Account",
		"attributes": []map[string]any{
			{"name": "userName", "type": "string", "required": true, "uniqueness": "server"},
			{"name": "displayName", "type": "string", "required": false},
			{"name": "active", "type": "boolean", "required": false},
			{"name": "emails", "type": "complex", "multiValued": true, "required": false, "subAttributes": []map[string]any{
				{"name": "value", "type": "string"},
				{"name": "primary", "type": "boolean"},
			}},
		},
	}
}

func groupSchema() map[string]any {
	return map[string]any{
		"id":          scimSchemaGroup,
		"name":        "Group",
		"description": "Group",
		"attributes": []map[string]any{
			{"name": "displayName", "type": "string", "required": true},
			{"name": "members", "type": "complex", "multiValued": true, "required": false, "subAttributes": []map[string]any{
				{"name": "value", "type": "string"},
				{"name": "display", "type": "string"},
				{"name": "$ref", "type": "reference"},
			}},
		},
	}
}

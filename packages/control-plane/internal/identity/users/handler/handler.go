// Package iam owns the Control Plane admin API for the full IAM/Auth
// cluster — IAM (policies/groups/role attachments), users, admin API
// keys, identity providers (IdP/SAML/SCIM-token management),
// organizations + projects, and auth-session revocation. R8-B17
// final flat-handler bundle.
package iam

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/ai/virtualkeys/vkstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/fleet/store/fleetstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/revocation"
	authstore "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/store"
	cpiam "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/iam"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/scim/scimstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/users/governancestore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/users/iamstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/users/orgstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/users/userstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/infrastructure/store/federatedstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/hub"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	cpgx "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/pgx"
)

// HubAPI is the union Hub surface the iam bundle needs.
type HubAPI interface {
	NotifyConfigChange(ctx context.Context, req hub.ConfigChangeRequest) (*hub.ConfigChangeResponse, error)
	InvalidateConfig(ctx context.Context, thingType, configKey string)
}

// iamUserStore is the narrow userstore surface the iam handler needs.
// Satisfied by *userstore.Store; test stubs implement this interface.
type iamUserStore interface {
	ListNexusUsers(ctx context.Context, p userstore.NexusUserListParams) ([]userstore.NexusUserSafe, int, error)
	GetNexusUserSafe(ctx context.Context, id string) (*userstore.NexusUserSafe, error)
	GetNexusUserOrgInfo(ctx context.Context, userID string) (orgID, orgName string, err error)
	FindNexusUserByID(ctx context.Context, id string) (*userstore.NexusUser, error)
	CreateNexusUser(ctx context.Context, p userstore.CreateNexusUserParams) (*userstore.NexusUserSafe, error)
	UpdateNexusUser(ctx context.Context, id string, p userstore.UpdateNexusUserParams) (*userstore.NexusUserSafe, error)
	DeleteNexusUser(ctx context.Context, id string) error
	ListAdminAPIKeys(ctx context.Context, ownerUserID string) ([]userstore.AdminAPIKey, error)
	GetAdminAPIKey(ctx context.Context, id string) (*userstore.AdminAPIKey, error)
	CreateAdminAPIKey(ctx context.Context, p userstore.CreateAdminAPIKeyParams) (*userstore.AdminAPIKey, error)
	UpdateAdminAPIKey(ctx context.Context, id string, p userstore.UpdateAdminAPIKeyParams) (*userstore.AdminAPIKey, error)
	RegenerateAdminAPIKey(ctx context.Context, id, keyHash, keyPrefix string) error
	RotateAdminAPIKey(ctx context.Context, p userstore.RotateAdminAPIKeyParams) (*userstore.RotateAdminAPIKeyResult, error)
	RetireAdminAPIKey(ctx context.Context, id, targetStatus string) (*userstore.AdminAPIKey, error)
	DeleteAdminAPIKey(ctx context.Context, id string) error
}

// iamIAMStore is the narrow iamstore surface the iam handler needs.
// Satisfied by *iamstore.Store; test stubs implement this interface.
type iamIAMStore interface {
	ListIamPolicies(ctx context.Context, q, typeFilter string, enabled *bool, limit, offset int) ([]iamstore.PolicyRow, int, error)
	GetIamPolicy(ctx context.Context, id string) (*iamstore.PolicyRow, error)
	CreateIamPolicy(ctx context.Context, name string, description *string, document json.RawMessage, createdBy string) (*iamstore.PolicyRow, error)
	UpdateIamPolicy(ctx context.Context, id string, p iamstore.UpdateIamPolicyParams) (*iamstore.PolicyRow, error)
	DeleteIamPolicy(ctx context.Context, id string) error
	ListGroupsForPolicy(ctx context.Context, policyID string) ([]iamstore.PolicyGroupRow, error)
	ListDirectPolicyAttachments(ctx context.Context, policyID string) ([]iamstore.DirectPolicyAttachmentRow, error)
	ListPolicyAttachedUserIDs(ctx context.Context, policyID string) ([]string, error)
	ListIamGroups(ctx context.Context) ([]iamstore.GroupRow, error)
	GetIamGroup(ctx context.Context, id string) (*iamstore.GroupRow, error)
	CreateIamGroup(ctx context.Context, name string, description *string, createdBy string) (*iamstore.GroupRow, error)
	UpdateIamGroup(ctx context.Context, id string, p iamstore.UpdateIamGroupParams) (*iamstore.GroupRow, error)
	DeleteIamGroup(ctx context.Context, id string) error
	AddGroupMember(ctx context.Context, groupID, principalType, principalID string) (string, error)
	RemoveGroupMember(ctx context.Context, membershipID string) error
	GetGroupMembershipByID(ctx context.Context, membershipID string) (groupID, principalType, principalID string, err error)
	ListGroupMembers(ctx context.Context, groupID string) ([]iamstore.GroupMemberRow, error)
	ListGroupMembersPaginated(ctx context.Context, groupID string, limit, offset int) ([]iamstore.GroupMemberRow, int, error)
	ListGroupPolicies(ctx context.Context, groupID string) ([]iamstore.GroupPolicyRow, error)
	AttachGroupPolicy(ctx context.Context, groupID, policyID string) (string, error)
	DetachGroupPolicy(ctx context.Context, attachmentID string) error
	GetGroupPolicyAttachmentByID(ctx context.Context, attachmentID string) (groupID, policyID string, err error)
	AttachPrincipalPolicy(ctx context.Context, principalType, principalID, policyID string, expiresAt *time.Time) (string, error)
	DetachPrincipalPolicy(ctx context.Context, attachmentID string) error
	GetPrincipalPolicyAttachmentByID(ctx context.Context, attachmentID string) (principalType, principalID, policyID string, err error)
	ListPrincipalPolicyAttachments(ctx context.Context, principalType, principalID string) ([]iamstore.PrincipalPolicyAttachment, error)
	ListPolicyNamesForPrincipal(ctx context.Context, principalType, principalID string) ([]string, error)
	ListGroupNamesForPrincipal(ctx context.Context, principalType, principalID string) ([]string, error)
}

// iamOrgStore is the narrow orgstore surface the iam handler needs.
// Satisfied by *orgstore.Store; test stubs implement this interface.
type iamOrgStore interface {
	ListOrganizations(ctx context.Context) ([]orgstore.Organization, error)
	ListChildOrganizations(ctx context.Context, parentID string) ([]orgstore.Organization, error)
	GetOrganization(ctx context.Context, id string) (*orgstore.Organization, error)
	CreateOrganization(ctx context.Context, updates map[string]any) (*orgstore.Organization, error)
	UpdateOrganization(ctx context.Context, id string, p orgstore.UpdateOrganizationParams) (*orgstore.Organization, error)
	DeleteOrganization(ctx context.Context, id string) error
	CountOrgDependents(ctx context.Context, id string) (children, projects int, err error)
	ListProjects(ctx context.Context, p orgstore.ProjectListParams) ([]orgstore.Project, int, error)
	GetProject(ctx context.Context, id string) (*orgstore.Project, error)
	CreateProject(ctx context.Context, updates map[string]any) (*orgstore.Project, error)
	UpdateProject(ctx context.Context, id string, p orgstore.UpdateProjectParams) (*orgstore.Project, error)
	DeleteProject(ctx context.Context, id string) error
	CountProjectVirtualKeys(ctx context.Context, projectID string) (int, error)
}

// iamScimStore is the narrow scimstore surface the iam handler needs.
// Satisfied by *scimstore.Store; test stubs implement this interface.
type iamScimStore interface {
	ListIdentityProviders(ctx context.Context) ([]scimstore.IdentityProviderRecord, error)
	GetIdentityProvider(ctx context.Context, id string) (*scimstore.IdentityProviderRecord, error)
	CreateIdentityProvider(ctx context.Context, p scimstore.CreateIdentityProviderParams) (*scimstore.IdentityProviderRecord, error)
	UpdateIdentityProvider(ctx context.Context, p scimstore.UpdateIdentityProviderParams) (*scimstore.IdentityProviderRecord, error)
	DeleteIdentityProvider(ctx context.Context, idpID string, force bool) error
	CountFederatedIdentitiesForIdP(ctx context.Context, idpID string) (int, error)
	CreateScimToken(ctx context.Context, p scimstore.CreateScimTokenParams) (*scimstore.ScimToken, error)
	ListScimTokens(ctx context.Context, idpID *string) ([]scimstore.ScimToken, error)
	RevokeScimToken(ctx context.Context, id string) error
	CreateIdpGroupMapping(ctx context.Context, p scimstore.CreateIdpGroupMappingParams) (*scimstore.IdpGroupMapping, error)
	ListIdpGroupMappings(ctx context.Context, idpID string) ([]scimstore.IdpGroupMapping, error)
	DeleteIdpGroupMapping(ctx context.Context, id string) error
}

// iamFleetStore is the narrow fleetstore surface the iam handler needs.
// Satisfied by *fleetstore.Store; test stubs implement this interface.
type iamFleetStore interface {
	ListDeviceAssignmentsByUser(ctx context.Context, userID string) ([]fleetstore.DeviceAssignmentDetail, error)
}

// iamVKStore is the narrow vkstore surface the iam handler needs.
// Satisfied by *vkstore.Store; test stubs implement this interface.
type iamVKStore interface {
	ListVirtualKeys(ctx context.Context, p vkstore.VirtualKeyListParams) ([]vkstore.VirtualKey, int, error)
}

// iamFedStore is the narrow federatedstore surface the iam handler needs.
// Satisfied by *federatedstore.Store; test stubs implement this interface.
type iamFedStore interface {
	ListUserIDsByIdP(ctx context.Context, idpID string) ([]string, error)
}

// iamRevocationStore is the narrow revocation.Store surface that ListRevocations needs.
// Satisfied by *revocation.Store; test stubs implement this interface.
type iamRevocationStore interface {
	ListSince(ctx context.Context, sinceID int64, limit int) ([]revocation.Row, int64, error)
}

// iamGovernanceStore is the narrow governancestore surface the iam handler needs.
// Satisfied by *governancestore.Store; test stubs implement this interface.
type iamGovernanceStore interface {
	GetUserAuditEvents(ctx context.Context, userID string, limit, offset int) ([]governancestore.AuditEventRow, int, error)
	GetUserAuditSummary(ctx context.Context, userID string) (*governancestore.UserAuditSummary, error)
	DisableVirtualKeysByOwner(ctx context.Context, ownerID string) (int64, error)
	RevokeDevicesByUser(ctx context.Context, userID string) (int64, error)
	SuspendUser(ctx context.Context, userID string) error
	ListVirtualKeysByOwner(ctx context.Context, ownerID string) ([]governancestore.UserVirtualKeySummary, error)
	ListActiveDevicesByUser(ctx context.Context, userID string) ([]governancestore.UserDeviceSummary, error)
}

// Deps is the construction-time arg shape.
type Deps struct {
	Pool            cpgx.PgxPool
	Hub             HubAPI
	Audit           *audit.Writer
	Logger          *slog.Logger
	IAM             *cpiam.Engine
	Revocation      *revocation.Service
	RevocationStore *revocation.Store
	AuthRefreshTTL  time.Duration
}

// Handler owns the IAM/Auth admin API surface.
type Handler struct {
	users      iamUserStore
	iam        iamIAMStore
	orgs       iamOrgStore
	scim       iamScimStore
	fleet      iamFleetStore
	vk         iamVKStore
	fed        iamFedStore
	governance iamGovernanceStore
	// oauth backs the OAuth client admin endpoints (issue #40). Distinct
	// from the OAuth runtime store wired into authserver/oauth: the admin
	// surface here CRUD's the OAuthClient row, the runtime surface there
	// reads it during /token verification.
	oauth iamOAuthClientStore
	// pool is the direct SQL surface for auth_sessions queries that issue
	// inline SQL against RefreshToken and related tables.
	pool            cpgx.PgxPool
	hub             HubAPI
	audit           *audit.Writer
	logger          *slog.Logger
	iamEngine       *cpiam.Engine
	revocation      *revocation.Service
	revocationStore iamRevocationStore
	authRefreshTTL  time.Duration
}

// New constructs a Handler from Deps.
func New(d Deps) *Handler {
	logger := d.Logger
	if logger == nil {
		logger = slog.Default()
	}
	h := &Handler{
		pool:            d.Pool,
		hub:             d.Hub,
		audit:           d.Audit,
		logger:          logger,
		iamEngine:       d.IAM,
		revocation:      d.Revocation,
		revocationStore: d.RevocationStore,
		authRefreshTTL:  d.AuthRefreshTTL,
	}
	if d.Pool != nil {
		h.users = userstore.New(d.Pool)
		h.iam = iamstore.New(d.Pool)
		h.orgs = orgstore.New(d.Pool)
		h.scim = scimstore.New(d.Pool)
		h.fleet = fleetstore.New(d.Pool)
		h.vk = vkstore.New(d.Pool)
		h.fed = federatedstore.New(d.Pool)
		h.governance = governancestore.New(d.Pool)
		// d.Pool is the narrow cpgx.PgxPool interface (Exec+Query+QueryRow)
		// which satisfies authstore.ClientPgxPool; the wider concrete-typed
		// constructor would force an unnecessary type assertion here.
		h.oauth = authstore.NewClientStoreWithPool(d.Pool)
	}
	return h
}

func errJSON(message, errType, code string) map[string]any {
	return map[string]any{"error": map[string]any{"message": message, "type": errType, "code": code}}
}

// Actor mirrors handler.Actor.
type Actor struct {
	UserID string
	Name   string
}

func actorFromContext(c echo.Context) Actor {
	aa := middleware.AdminAuthFromContext(c)
	if aa == nil {
		return Actor{}
	}
	return Actor{UserID: aa.KeyID, Name: aa.KeyName}
}

func sourceIP(c echo.Context) string { return c.RealIP() }

func currentUserID(c echo.Context) string {
	aa := middleware.AdminAuthFromContext(c)
	if aa == nil {
		return ""
	}
	return aa.KeyID
}

type pagination struct {
	Limit  int
	Offset int
}

func parsePagination(c echo.Context) pagination {
	limit := 50
	offset := 0
	if v := c.QueryParam("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
			if limit > 1000 {
				limit = 1000
			}
		}
	}
	if v := c.QueryParam("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			offset = n
		}
	}
	return pagination{Limit: limit, Offset: offset}
}

func internalServerError(c echo.Context, msg string) error {
	return c.JSON(http.StatusInternalServerError, errJSON(msg, "server_error", ""))
}

func parseRFC3339Flexible(s string) (time.Time, bool) {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, true
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, true
	}
	return time.Time{}, false
}

func parseTimeRange(c echo.Context) (start, end *time.Time) {
	if v := c.QueryParam("startTime"); v != "" {
		if t, ok := parseRFC3339Flexible(v); ok {
			start = &t
		}
	}
	if v := c.QueryParam("endTime"); v != "" {
		if t, ok := parseRFC3339Flexible(v); ok {
			end = &t
		}
	}
	return
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

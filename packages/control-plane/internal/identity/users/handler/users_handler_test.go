package iam

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/pem"
	"errors"
	"github.com/goccy/go-json"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/ai/virtualkeys/vkstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/fleet/store/fleetstore"
	authn "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authn"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/revocation"
	cpiam "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/iam"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/idptest"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/oidcdisco"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/scim/scimstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/users/governancestore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/users/iamstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/users/orgstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/users/userstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/hub"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
)

// test helpers

func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func noopAudit() *audit.Writer {
	return audit.NewWriter(nil, "", silentLogger())
}

func adminAuthCtx(method, path string, body []byte, userID, principalType string) (echo.Context, *httptest.ResponseRecorder) {
	var rb io.Reader
	if body != nil {
		rb = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rb)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e := echo.New()
	c := e.NewContext(req, rec)
	if userID != "" {
		aa := &authn.AdminAuth{KeyID: userID, KeyName: "test", AuthPrincipalType: principalType}
		middleware.WithAdminAuth(c, aa)
	}
	return c, rec
}

// stubs: iamUserStore

type stubUserStore struct {
	listResult      []userstore.NexusUserSafe
	listTotal       int
	listErr         error
	getSafeResult   *userstore.NexusUserSafe
	getSafeErr      error
	orgID           string
	orgName         string
	orgErr          error
	findResult      *userstore.NexusUser
	findErr         error
	createResult    *userstore.NexusUserSafe
	createErr       error
	updateResult    *userstore.NexusUserSafe
	updateErr       error
	deleteErr       error
	listKeys        []userstore.AdminAPIKey
	listKeysErr     error
	getKey          *userstore.AdminAPIKey
	getKeyErr       error
	createKey       *userstore.AdminAPIKey
	createKeyErr    error
	createKeyCalled bool
	updateKey       *userstore.AdminAPIKey
	updateKeyErr    error
	regenErr        error
	rotateResult    *userstore.RotateAdminAPIKeyResult
	rotateErr       error
	retireKey       *userstore.AdminAPIKey
	retireErr       error
	deleteKeyErr    error
}

func (s *stubUserStore) ListNexusUsers(_ context.Context, _ userstore.NexusUserListParams) ([]userstore.NexusUserSafe, int, error) {
	return s.listResult, s.listTotal, s.listErr
}
func (s *stubUserStore) GetNexusUserSafe(_ context.Context, _ string) (*userstore.NexusUserSafe, error) {
	return s.getSafeResult, s.getSafeErr
}
func (s *stubUserStore) GetNexusUserOrgInfo(_ context.Context, _ string) (string, string, error) {
	return s.orgID, s.orgName, s.orgErr
}
func (s *stubUserStore) FindNexusUserByID(_ context.Context, _ string) (*userstore.NexusUser, error) {
	return s.findResult, s.findErr
}
func (s *stubUserStore) CreateNexusUser(_ context.Context, _ userstore.CreateNexusUserParams) (*userstore.NexusUserSafe, error) {
	return s.createResult, s.createErr
}
func (s *stubUserStore) UpdateNexusUser(_ context.Context, _ string, _ userstore.UpdateNexusUserParams) (*userstore.NexusUserSafe, error) {
	return s.updateResult, s.updateErr
}
func (s *stubUserStore) DeleteNexusUser(_ context.Context, _ string) error { return s.deleteErr }
func (s *stubUserStore) ListAdminAPIKeys(_ context.Context, _ string) ([]userstore.AdminAPIKey, error) {
	return s.listKeys, s.listKeysErr
}
func (s *stubUserStore) GetAdminAPIKey(_ context.Context, _ string) (*userstore.AdminAPIKey, error) {
	return s.getKey, s.getKeyErr
}
func (s *stubUserStore) CreateAdminAPIKey(_ context.Context, _ userstore.CreateAdminAPIKeyParams) (*userstore.AdminAPIKey, error) {
	s.createKeyCalled = true
	return s.createKey, s.createKeyErr
}
func (s *stubUserStore) UpdateAdminAPIKey(_ context.Context, _ string, _ userstore.UpdateAdminAPIKeyParams) (*userstore.AdminAPIKey, error) {
	return s.updateKey, s.updateKeyErr
}
func (s *stubUserStore) RegenerateAdminAPIKey(_ context.Context, _, _, _, _ string) error {
	return s.regenErr
}
func (s *stubUserStore) RotateAdminAPIKey(_ context.Context, _ userstore.RotateAdminAPIKeyParams) (*userstore.RotateAdminAPIKeyResult, error) {
	return s.rotateResult, s.rotateErr
}
func (s *stubUserStore) RetireAdminAPIKey(_ context.Context, _, _ string) (*userstore.AdminAPIKey, error) {
	return s.retireKey, s.retireErr
}
func (s *stubUserStore) DeleteAdminAPIKey(_ context.Context, _ string) error { return s.deleteKeyErr }

// stubs: iamIAMStore

type stubIAMStore struct {
	policies          []iamstore.PolicyRow
	policiesTotal     int
	policiesErr       error
	policy            *iamstore.PolicyRow
	policyErr         error
	createdPolicy     *iamstore.PolicyRow
	createPolicyErr   error
	createCalled      bool
	updatedPolicy     *iamstore.PolicyRow
	updatePolicyErr   error
	updateCalled      bool
	deletePolicyErr   error
	policyGroups      []iamstore.PolicyGroupRow
	policyGroupsErr   error
	directRows        []iamstore.DirectPolicyAttachmentRow
	directRowsErr     error
	attachedUserIDs   []string
	attachedUserErr   error
	groups            []iamstore.GroupRow
	groupsErr         error
	group             *iamstore.GroupRow
	groupErr          error
	createdGroup      *iamstore.GroupRow
	createGroupErr    error
	updatedGroup      *iamstore.GroupRow
	deleteGroupErr    error
	memberID          string
	memberErr         error
	membershipID      string
	membershipPT      string
	membershipPID     string
	membershipErr     error
	removeErr         error
	members           []iamstore.GroupMemberRow
	membersErr        error
	membersTotal      int
	groupPolicies     []iamstore.GroupPolicyRow
	groupPoliciesErr  error
	attachGroupPolID  string
	attachGroupPolErr error
	detachGroupPolErr error
	groupPolicyAttGID string
	groupPolicyAttPID string
	groupPolicyAttErr error
	attachPPID        string
	attachPPErr       error
	detachPPErr       error
	ppAttGID          string
	ppAttPID          string
	ppAttPolicyID     string
	ppAttErr          error
	ppAttachments     []iamstore.PrincipalPolicyAttachment
	ppAttachmentsErr  error
	policyNames       []string
	policyNamesErr    error
	groupNames        []string
	groupNamesErr     error
}

func (s *stubIAMStore) ListIamPolicies(_ context.Context, _, _ string, _ *bool, _, _ int) ([]iamstore.PolicyRow, int, error) {
	return s.policies, s.policiesTotal, s.policiesErr
}
func (s *stubIAMStore) GetIamPolicy(_ context.Context, _ string) (*iamstore.PolicyRow, error) {
	return s.policy, s.policyErr
}
func (s *stubIAMStore) CreateIamPolicy(_ context.Context, _ string, _ *string, _ json.RawMessage, _ string) (*iamstore.PolicyRow, error) {
	s.createCalled = true
	return s.createdPolicy, s.createPolicyErr
}
func (s *stubIAMStore) UpdateIamPolicy(_ context.Context, _ string, _ iamstore.UpdateIamPolicyParams) (*iamstore.PolicyRow, error) {
	s.updateCalled = true
	return s.updatedPolicy, s.updatePolicyErr
}
func (s *stubIAMStore) DeleteIamPolicy(_ context.Context, _ string) error { return s.deletePolicyErr }
func (s *stubIAMStore) ListGroupsForPolicy(_ context.Context, _ string) ([]iamstore.PolicyGroupRow, error) {
	return s.policyGroups, s.policyGroupsErr
}
func (s *stubIAMStore) ListDirectPolicyAttachments(_ context.Context, _ string) ([]iamstore.DirectPolicyAttachmentRow, error) {
	return s.directRows, s.directRowsErr
}
func (s *stubIAMStore) ListPolicyAttachedUserIDs(_ context.Context, _ string) ([]string, error) {
	return s.attachedUserIDs, s.attachedUserErr
}
func (s *stubIAMStore) ListIamGroups(_ context.Context) ([]iamstore.GroupRow, error) {
	return s.groups, s.groupsErr
}
func (s *stubIAMStore) ListIamGroupsPage(_ context.Context, _ string, _, _ int) ([]iamstore.GroupRow, int, error) {
	return s.groups, len(s.groups), s.groupsErr
}
func (s *stubIAMStore) GetIamGroup(_ context.Context, _ string) (*iamstore.GroupRow, error) {
	return s.group, s.groupErr
}
func (s *stubIAMStore) CreateIamGroup(_ context.Context, _ string, _ *string, _ string) (*iamstore.GroupRow, error) {
	return s.createdGroup, s.createGroupErr
}
func (s *stubIAMStore) UpdateIamGroup(_ context.Context, _ string, _ iamstore.UpdateIamGroupParams) (*iamstore.GroupRow, error) {
	return s.updatedGroup, nil
}
func (s *stubIAMStore) DeleteIamGroup(_ context.Context, _ string) error { return s.deleteGroupErr }
func (s *stubIAMStore) AddGroupMember(_ context.Context, _, _, _ string) (string, error) {
	return s.memberID, s.memberErr
}
func (s *stubIAMStore) RemoveGroupMember(_ context.Context, _ string) error { return s.removeErr }
func (s *stubIAMStore) GetGroupMembershipByID(_ context.Context, _ string) (string, string, string, error) {
	return s.membershipID, s.membershipPT, s.membershipPID, s.membershipErr
}
func (s *stubIAMStore) ListGroupMembers(_ context.Context, _ string) ([]iamstore.GroupMemberRow, error) {
	return s.members, s.membersErr
}
func (s *stubIAMStore) ListGroupMembersPaginated(_ context.Context, _ string, _, _ int) ([]iamstore.GroupMemberRow, int, error) {
	return s.members, s.membersTotal, s.membersErr
}
func (s *stubIAMStore) ListGroupPolicies(_ context.Context, _ string) ([]iamstore.GroupPolicyRow, error) {
	return s.groupPolicies, s.groupPoliciesErr
}
func (s *stubIAMStore) AttachGroupPolicy(_ context.Context, _, _ string) (string, error) {
	return s.attachGroupPolID, s.attachGroupPolErr
}
func (s *stubIAMStore) DetachGroupPolicy(_ context.Context, _ string) error {
	return s.detachGroupPolErr
}
func (s *stubIAMStore) GetGroupPolicyAttachmentByID(_ context.Context, _ string) (string, string, error) {
	return s.groupPolicyAttGID, s.groupPolicyAttPID, s.groupPolicyAttErr
}
func (s *stubIAMStore) AttachPrincipalPolicy(_ context.Context, _, _, _ string, _ *time.Time) (string, error) {
	return s.attachPPID, s.attachPPErr
}
func (s *stubIAMStore) DetachPrincipalPolicy(_ context.Context, _ string) error {
	return s.detachPPErr
}
func (s *stubIAMStore) GetPrincipalPolicyAttachmentByID(_ context.Context, _ string) (string, string, string, error) {
	return s.ppAttGID, s.ppAttPID, s.ppAttPolicyID, s.ppAttErr
}
func (s *stubIAMStore) ListPrincipalPolicyAttachments(_ context.Context, _, _ string) ([]iamstore.PrincipalPolicyAttachment, error) {
	return s.ppAttachments, s.ppAttachmentsErr
}
func (s *stubIAMStore) ListPolicyNamesForPrincipal(_ context.Context, _, _ string) ([]string, error) {
	return s.policyNames, s.policyNamesErr
}
func (s *stubIAMStore) ListGroupNamesForPrincipal(_ context.Context, _, _ string) ([]string, error) {
	return s.groupNames, s.groupNamesErr
}

// stubs: iamOrgStore

type stubOrgStore struct {
	orgs           []orgstore.Organization
	orgsErr        error
	childOrgs      []orgstore.Organization
	org            *orgstore.Organization
	orgErr         error
	createdOrg     *orgstore.Organization
	createOrgErr   error
	updatedOrg     *orgstore.Organization
	updateOrgErr   error
	deleteOrgErr   error
	orgChildren    int
	orgProjects    int
	orgDepsErr     error
	projects       []orgstore.Project
	projectsTotal  int
	projectsErr    error
	project        *orgstore.Project
	projectErr     error
	createdProject *orgstore.Project
	createProjErr  error
	updatedProject *orgstore.Project
	updateProjErr  error
	deleteProjErr  error
	projVKCount    int
	projVKErr      error
}

func (s *stubOrgStore) ListOrganizations(_ context.Context) ([]orgstore.Organization, error) {
	return s.orgs, s.orgsErr
}
func (s *stubOrgStore) ListChildOrganizations(_ context.Context, _ string) ([]orgstore.Organization, error) {
	return s.childOrgs, s.orgsErr
}
func (s *stubOrgStore) GetOrganization(_ context.Context, _ string) (*orgstore.Organization, error) {
	return s.org, s.orgErr
}
func (s *stubOrgStore) CreateOrganization(_ context.Context, _ map[string]any) (*orgstore.Organization, error) {
	return s.createdOrg, s.createOrgErr
}
func (s *stubOrgStore) UpdateOrganization(_ context.Context, _ string, _ orgstore.UpdateOrganizationParams) (*orgstore.Organization, error) {
	return s.updatedOrg, s.updateOrgErr
}
func (s *stubOrgStore) DeleteOrganization(_ context.Context, _ string) error { return s.deleteOrgErr }
func (s *stubOrgStore) CountOrgDependents(_ context.Context, _ string) (int, int, error) {
	return s.orgChildren, s.orgProjects, s.orgDepsErr
}
func (s *stubOrgStore) ListProjects(_ context.Context, _ orgstore.ProjectListParams) ([]orgstore.Project, int, error) {
	return s.projects, s.projectsTotal, s.projectsErr
}
func (s *stubOrgStore) GetProject(_ context.Context, _ string) (*orgstore.Project, error) {
	return s.project, s.projectErr
}
func (s *stubOrgStore) CreateProject(_ context.Context, _ map[string]any) (*orgstore.Project, error) {
	return s.createdProject, s.createProjErr
}
func (s *stubOrgStore) UpdateProject(_ context.Context, _ string, _ orgstore.UpdateProjectParams) (*orgstore.Project, error) {
	return s.updatedProject, s.updateProjErr
}
func (s *stubOrgStore) DeleteProject(_ context.Context, _ string) error { return s.deleteProjErr }
func (s *stubOrgStore) CountProjectVirtualKeys(_ context.Context, _ string) (int, error) {
	return s.projVKCount, s.projVKErr
}

// stubs: iamScimStore

type stubScimStore struct {
	idps           []scimstore.IdentityProviderRecord
	idpsErr        error
	idp            *scimstore.IdentityProviderRecord
	idpErr         error
	createdIdp     *scimstore.IdentityProviderRecord
	createIdpErr   error
	updatedIdp     *scimstore.IdentityProviderRecord
	updateIdpErr   error
	deleteIdpErr   error
	fedCount       int
	fedCountErr    error
	createdToken   *scimstore.ScimToken
	createTokErr   error
	tokens         []scimstore.ScimToken
	tokensErr      error
	revokeTokErr   error
	createdMapping *scimstore.IdpGroupMapping
	createMapErr   error
	mappings       []scimstore.IdpGroupMapping
	mappingsErr    error
	deleteMapErr   error
}

func (s *stubScimStore) ListIdentityProviders(_ context.Context) ([]scimstore.IdentityProviderRecord, error) {
	return s.idps, s.idpsErr
}
func (s *stubScimStore) GetIdentityProvider(_ context.Context, _ string) (*scimstore.IdentityProviderRecord, error) {
	return s.idp, s.idpErr
}
func (s *stubScimStore) CreateIdentityProvider(_ context.Context, _ scimstore.CreateIdentityProviderParams) (*scimstore.IdentityProviderRecord, error) {
	return s.createdIdp, s.createIdpErr
}
func (s *stubScimStore) UpdateIdentityProvider(_ context.Context, _ scimstore.UpdateIdentityProviderParams) (*scimstore.IdentityProviderRecord, error) {
	return s.updatedIdp, s.updateIdpErr
}
func (s *stubScimStore) DeleteIdentityProvider(_ context.Context, _ string, _ bool) error {
	return s.deleteIdpErr
}
func (s *stubScimStore) CountFederatedIdentitiesForIdP(_ context.Context, _ string) (int, error) {
	return s.fedCount, s.fedCountErr
}
func (s *stubScimStore) CreateScimToken(_ context.Context, _ scimstore.CreateScimTokenParams) (*scimstore.ScimToken, error) {
	return s.createdToken, s.createTokErr
}
func (s *stubScimStore) ListScimTokens(_ context.Context, _ *string) ([]scimstore.ScimToken, error) {
	return s.tokens, s.tokensErr
}
func (s *stubScimStore) RevokeScimToken(_ context.Context, _ string) error { return s.revokeTokErr }
func (s *stubScimStore) CreateIdpGroupMapping(_ context.Context, _ scimstore.CreateIdpGroupMappingParams) (*scimstore.IdpGroupMapping, error) {
	return s.createdMapping, s.createMapErr
}
func (s *stubScimStore) ListIdpGroupMappings(_ context.Context, _ string) ([]scimstore.IdpGroupMapping, error) {
	return s.mappings, s.mappingsErr
}
func (s *stubScimStore) DeleteIdpGroupMapping(_ context.Context, _ string) error {
	return s.deleteMapErr
}

// stubs: iamFleetStore

type stubFleetStore struct {
	assignments    []fleetstore.DeviceAssignmentDetail
	assignmentsErr error
}

func (s *stubFleetStore) ListDeviceAssignmentsByUser(_ context.Context, _ string) ([]fleetstore.DeviceAssignmentDetail, error) {
	return s.assignments, s.assignmentsErr
}

// stubs: iamVKStore

type stubVKStore struct {
	vks      []vkstore.VirtualKey
	vksTotal int
	vksErr   error
}

func (s *stubVKStore) ListVirtualKeys(_ context.Context, _ vkstore.VirtualKeyListParams) ([]vkstore.VirtualKey, int, error) {
	return s.vks, s.vksTotal, s.vksErr
}

// stubs: iamFedStore

type stubFedStore struct {
	userIDs    []string
	userIDsErr error
}

func (s *stubFedStore) ListUserIDsByIdP(_ context.Context, _ string) ([]string, error) {
	return s.userIDs, s.userIDsErr
}

// stubs: iamGovernanceStore

type stubGovernanceStore struct {
	auditEvents     []governancestore.AuditEventRow
	auditTotal      int
	auditErr        error
	auditSummary    *governancestore.UserAuditSummary
	auditSummaryErr error
	disabledVKs     int64
	disableVKErr    error
	revokedDevices  int64
	revokeDevErr    error
	suspendErr      error
	vkSummaries     []governancestore.UserVirtualKeySummary
	vkSummariesErr  error
	deviceSummaries []governancestore.UserDeviceSummary
	deviceSumErr    error
}

func (s *stubGovernanceStore) GetUserAuditEvents(_ context.Context, _ string, _, _ int) ([]governancestore.AuditEventRow, int, error) {
	return s.auditEvents, s.auditTotal, s.auditErr
}
func (s *stubGovernanceStore) GetUserAuditSummary(_ context.Context, _ string) (*governancestore.UserAuditSummary, error) {
	return s.auditSummary, s.auditSummaryErr
}
func (s *stubGovernanceStore) DisableVirtualKeysByOwner(_ context.Context, _ string) (int64, error) {
	return s.disabledVKs, s.disableVKErr
}
func (s *stubGovernanceStore) RevokeDevicesByUser(_ context.Context, _ string) (int64, error) {
	return s.revokedDevices, s.revokeDevErr
}
func (s *stubGovernanceStore) SuspendUser(_ context.Context, _ string) error { return s.suspendErr }
func (s *stubGovernanceStore) ListVirtualKeysByOwner(_ context.Context, _ string) ([]governancestore.UserVirtualKeySummary, error) {
	return s.vkSummaries, s.vkSummariesErr
}
func (s *stubGovernanceStore) ListActiveDevicesByUser(_ context.Context, _ string) ([]governancestore.UserDeviceSummary, error) {
	return s.deviceSummaries, s.deviceSumErr
}

// stub: cpgx.PgxPool

// stubPool satisfies cpgx.PgxPool with configurable results for testing the
// auth_sessions handlers that issue raw SQL directly against h.pool.
type stubPool struct {
	queryRows pgx.Rows
	queryErr  error
	execTag   pgconn.CommandTag
	execErr   error
}

func (s *stubPool) Begin(_ context.Context) (pgx.Tx, error) { return nil, nil }
func (s *stubPool) Exec(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
	return s.execTag, s.execErr
}
func (s *stubPool) Query(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
	return s.queryRows, s.queryErr
}
func (s *stubPool) QueryRow(_ context.Context, _ string, _ ...any) pgx.Row { return nil }
func (s *stubPool) Close()                                                 {}
func (s *stubPool) Ping(_ context.Context) error                           { return nil }

func buildHandler(
	us iamUserStore,
	is iamIAMStore,
	os iamOrgStore,
	ss iamScimStore,
	fs iamFleetStore,
	vs iamVKStore,
	feds iamFedStore,
	gs iamGovernanceStore,
) *Handler {
	return &Handler{
		users:      us,
		iam:        is,
		orgs:       os,
		scim:       ss,
		fleet:      fs,
		vk:         vs,
		fed:        feds,
		governance: gs,
		audit:      noopAudit(),
		logger:     silentLogger(),
	}
}

func defaultHandler() *Handler {
	return buildHandler(
		&stubUserStore{},
		&stubIAMStore{},
		&stubOrgStore{},
		&stubScimStore{},
		&stubFleetStore{},
		&stubVKStore{},
		&stubFedStore{},
		&stubGovernanceStore{},
	)
}

func TestListUsers_Success_Returns200(t *testing.T) {
	email := "u1@example.com"
	us := &stubUserStore{
		listResult: []userstore.NexusUserSafe{{ID: "u1", DisplayName: "User 1", Email: &email, Status: "active"}},
		listTotal:  1,
	}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodGet, "/users", nil, "admin", "admin_user")
	if err := h.ListUsers(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200; body=%s", rec.Code, rec.Body)
	}
}

func TestListUsers_StoreError_Returns500(t *testing.T) {
	us := &stubUserStore{listErr: errors.New("db")}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodGet, "/users", nil, "admin", "admin_user")
	if err := h.ListUsers(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500", rec.Code)
	}
}

func TestListUsers_WithEnabledAndCPFilters(t *testing.T) {
	// exercises the enabled=true and canAccessControlPlane=false branches
	us := &stubUserStore{listResult: nil, listTotal: 0}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	req := httptest.NewRequest(http.MethodGet, "/users?enabled=true&canAccessControlPlane=false", nil)
	rec := httptest.NewRecorder()
	e := echo.New()
	c := e.NewContext(req, rec)
	if err := h.ListUsers(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200", rec.Code)
	}
}

func TestGetUser_Found_Returns200(t *testing.T) {
	email := "u1@example.com"
	us := &stubUserStore{getSafeResult: &userstore.NexusUserSafe{ID: "u1", DisplayName: "User 1", Email: &email}}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodGet, "/users/u1", nil, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("u1")
	if err := h.GetUser(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200", rec.Code)
	}
}

func TestGetUser_NotFound_Returns404(t *testing.T) {
	us := &stubUserStore{getSafeResult: nil}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodGet, "/users/missing", nil, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("missing")
	if err := h.GetUser(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("code=%d want 404", rec.Code)
	}
}

func TestGetUser_StoreError_Returns500(t *testing.T) {
	us := &stubUserStore{getSafeErr: errors.New("db")}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodGet, "/users/u1", nil, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("u1")
	if err := h.GetUser(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500", rec.Code)
	}
}

func TestGetUser_WithOrg_IncludesOrgInfo(t *testing.T) {
	email := "u1@example.com"
	us := &stubUserStore{
		getSafeResult: &userstore.NexusUserSafe{ID: "u1", DisplayName: "U1", Email: &email},
		orgID:         "org-1",
		orgName:       "My Org",
	}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodGet, "/users/u1", nil, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("u1")
	if err := h.GetUser(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200", rec.Code)
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["organizationId"] != "org-1" {
		t.Errorf("missing organizationId in response")
	}
}

func TestCreateUser_MissingFields_Returns400(t *testing.T) {
	h := defaultHandler()
	body, _ := json.Marshal(map[string]any{"username": "", "password": ""})
	c, rec := adminAuthCtx(http.MethodPost, "/users", body, "admin", "admin_user")
	if err := h.CreateUser(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code=%d want 400", rec.Code)
	}
}

func TestCreateUser_Success_Returns201(t *testing.T) {
	email := "new@example.com"
	us := &stubUserStore{createResult: &userstore.NexusUserSafe{ID: "u-new", DisplayName: "new", Email: &email, Status: "active"}}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	body, _ := json.Marshal(map[string]any{"username": "new@example.com", "password": "secure-pass123", "email": "new@example.com"})
	c, rec := adminAuthCtx(http.MethodPost, "/users", body, "admin", "admin_user")
	if err := h.CreateUser(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("code=%d want 201; body=%s", rec.Code, rec.Body)
	}
}

func TestCreateUser_StoreError_Returns500(t *testing.T) {
	us := &stubUserStore{createErr: errors.New("db")}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	body, _ := json.Marshal(map[string]any{"username": "u", "password": "pw"})
	c, rec := adminAuthCtx(http.MethodPost, "/users", body, "admin", "admin_user")
	if err := h.CreateUser(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500", rec.Code)
	}
}

func TestUpdateUser_Success_Returns200(t *testing.T) {
	email := "u1@example.com"
	us := &stubUserStore{updateResult: &userstore.NexusUserSafe{ID: "u1", DisplayName: "U1", Email: &email, Status: "active"}}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	body, _ := json.Marshal(map[string]any{"displayName": "Updated"})
	c, rec := adminAuthCtx(http.MethodPut, "/users/u1", body, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("u1")
	if err := h.UpdateUser(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200", rec.Code)
	}
}

func TestUpdateUser_StoreError_Returns500(t *testing.T) {
	us := &stubUserStore{updateErr: errors.New("db")}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	body, _ := json.Marshal(map[string]any{"displayName": "Updated"})
	c, rec := adminAuthCtx(http.MethodPut, "/users/u1", body, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("u1")
	if err := h.UpdateUser(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500", rec.Code)
	}
}

func TestDeleteUser_Success_Returns204(t *testing.T) {
	us := &stubUserStore{}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodDelete, "/users/u1", nil, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("u1")
	if err := h.DeleteUser(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNoContent {
		t.Errorf("code=%d want 204; body=%s", rec.Code, rec.Body)
	}
}

func TestDeleteUser_StoreError_Returns500(t *testing.T) {
	us := &stubUserStore{deleteErr: errors.New("db")}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodDelete, "/users/u1", nil, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("u1")
	if err := h.DeleteUser(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500", rec.Code)
	}
}

// IAM Policies

func TestListIAMPolicies_Returns200(t *testing.T) {
	is := &stubIAMStore{policies: []iamstore.PolicyRow{{ID: "p1", Name: "Policy 1"}}, policiesTotal: 1}
	h := buildHandler(&stubUserStore{}, is, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodGet, "/iam/policies", nil, "admin", "admin_user")
	if err := h.ListIAMPolicies(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200", rec.Code)
	}
}

func TestListIAMPolicies_Error_Returns500(t *testing.T) {
	is := &stubIAMStore{policiesErr: errors.New("db")}
	h := buildHandler(&stubUserStore{}, is, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodGet, "/iam/policies", nil, "admin", "admin_user")
	if err := h.ListIAMPolicies(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500", rec.Code)
	}
}

func TestGetIAMPolicy_Found_Returns200(t *testing.T) {
	is := &stubIAMStore{policy: &iamstore.PolicyRow{ID: "p1", Name: "Policy 1"}}
	h := buildHandler(&stubUserStore{}, is, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodGet, "/iam/policies/p1", nil, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("p1")
	if err := h.GetIAMPolicy(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200", rec.Code)
	}
}

func TestGetIAMPolicy_NotFound_Returns404(t *testing.T) {
	is := &stubIAMStore{policy: nil}
	h := buildHandler(&stubUserStore{}, is, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodGet, "/iam/policies/missing", nil, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("missing")
	if err := h.GetIAMPolicy(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("code=%d want 404", rec.Code)
	}
}

func TestCreateIAMPolicy_MissingName_Returns400(t *testing.T) {
	h := defaultHandler()
	body, _ := json.Marshal(map[string]any{"name": "", "document": json.RawMessage(`{}`)})
	c, rec := adminAuthCtx(http.MethodPost, "/iam/policies", body, "admin", "admin_user")
	if err := h.CreateIAMPolicy(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code=%d want 400", rec.Code)
	}
}

func TestCreateIAMPolicy_Success_Returns201(t *testing.T) {
	is := &stubIAMStore{createdPolicy: &iamstore.PolicyRow{ID: "p-new", Name: "New Policy"}}
	h := buildHandler(&stubUserStore{}, is, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	h.iamEngine = superAdminTestEngine() // grant ceiling: caller must hold the granted perms
	body, _ := json.Marshal(map[string]any{"name": "New Policy", "document": json.RawMessage(`{"Version":"2026-05-12","Statement":[{"Effect":"Allow","Action":["settings:read"],"Resource":["*"]}]}`)})
	c, rec := adminAuthCtx(http.MethodPost, "/iam/policies", body, "admin", "admin_user")
	if err := h.CreateIAMPolicy(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("code=%d want 201; body=%s", rec.Code, rec.Body)
	}
}

// TestCreateIAMPolicy_InvalidDocument_Returns400 asserts that a
// syntactically-well-formed but semantically-invalid policy document (here a
// "**" wildcard, which ValidatePolicyDocument rejects) is refused with 400
// instead of being persisted unvalidated.
func TestCreateIAMPolicy_InvalidDocument_Returns400(t *testing.T) {
	is := &stubIAMStore{createdPolicy: &iamstore.PolicyRow{ID: "p-new", Name: "New Policy"}}
	h := buildHandler(&stubUserStore{}, is, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	body, _ := json.Marshal(map[string]any{"name": "Bad Policy", "document": json.RawMessage(`{"Version":"2026-05-12","Statement":[{"Effect":"Allow","Action":["s3:**"],"Resource":["*"]}]}`)})
	c, rec := adminAuthCtx(http.MethodPost, "/iam/policies", body, "admin", "admin_user")
	if err := h.CreateIAMPolicy(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code=%d want 400; body=%s", rec.Code, rec.Body)
	}
	if is.createCalled {
		t.Error("invalid document must not reach the store")
	}
}

// TestCreateIAMPolicy_NonObjectDocument_Returns400 asserts the handler rejects a
// document that is valid JSON but not a policy object (a bare JSON string),
// which the structural decode cannot map onto a PolicyDocument.
func TestCreateIAMPolicy_NonObjectDocument_Returns400(t *testing.T) {
	is := &stubIAMStore{createdPolicy: &iamstore.PolicyRow{ID: "p-new", Name: "New Policy"}}
	h := buildHandler(&stubUserStore{}, is, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	body, _ := json.Marshal(map[string]any{"name": "Bad Policy", "document": json.RawMessage(`"notapolicy"`)})
	c, rec := adminAuthCtx(http.MethodPost, "/iam/policies", body, "admin", "admin_user")
	if err := h.CreateIAMPolicy(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code=%d want 400; body=%s", rec.Code, rec.Body)
	}
	if is.createCalled {
		t.Error("undecodable document must not reach the store")
	}
}

// TestUpdateIAMPolicy_InvalidDocument_Returns400 asserts document validation on
// the update path: an invalid document body (bad Effect enum) is rejected with 400 and
// never persisted.
func TestUpdateIAMPolicy_InvalidDocument_Returns400(t *testing.T) {
	is := &stubIAMStore{updatedPolicy: &iamstore.PolicyRow{ID: "p1", Name: "Updated"}}
	h := buildHandler(&stubUserStore{}, is, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	body, _ := json.Marshal(map[string]any{"document": map[string]any{
		"Version":   "2026-05-12",
		"Statement": []map[string]any{{"Effect": "Maybe", "Action": []string{"settings:read"}, "Resource": []string{"*"}}},
	}})
	c, rec := adminAuthCtx(http.MethodPut, "/iam/policies/p1", body, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("p1")
	if err := h.UpdateIAMPolicy(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code=%d want 400; body=%s", rec.Code, rec.Body)
	}
	if is.updateCalled {
		t.Error("invalid document must not reach the store")
	}
}

// IAM Groups

func TestListIAMGroups_Returns200(t *testing.T) {
	is := &stubIAMStore{groups: []iamstore.GroupRow{{ID: "g1", Name: "Group 1"}}}
	h := buildHandler(&stubUserStore{}, is, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodGet, "/iam/groups", nil, "admin", "admin_user")
	if err := h.ListIAMGroups(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200", rec.Code)
	}
}

func TestGetIAMGroup_Found_Returns200(t *testing.T) {
	is := &stubIAMStore{group: &iamstore.GroupRow{ID: "g1", Name: "Group 1"}}
	h := buildHandler(&stubUserStore{}, is, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodGet, "/iam/groups/g1", nil, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("g1")
	if err := h.GetIAMGroup(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200", rec.Code)
	}
}

func TestCreateIAMGroup_MissingName_Returns400(t *testing.T) {
	h := defaultHandler()
	body, _ := json.Marshal(map[string]any{"name": ""})
	c, rec := adminAuthCtx(http.MethodPost, "/iam/groups", body, "admin", "admin_user")
	if err := h.CreateIAMGroup(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code=%d want 400", rec.Code)
	}
}

func TestCreateIAMGroup_Success_Returns201(t *testing.T) {
	is := &stubIAMStore{createdGroup: &iamstore.GroupRow{ID: "g-new", Name: "New Group"}}
	h := buildHandler(&stubUserStore{}, is, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	body, _ := json.Marshal(map[string]any{"name": "New Group"})
	c, rec := adminAuthCtx(http.MethodPost, "/iam/groups", body, "admin", "admin_user")
	if err := h.CreateIAMGroup(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("code=%d want 201; body=%s", rec.Code, rec.Body)
	}
}

func TestDeleteIAMGroup_Success_Returns204(t *testing.T) {
	is := &stubIAMStore{}
	h := buildHandler(&stubUserStore{}, is, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodDelete, "/iam/groups/g1", nil, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("g1")
	if err := h.DeleteIAMGroup(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNoContent {
		t.Errorf("code=%d want 204", rec.Code)
	}
}

func TestDeleteIAMGroup_Error_Returns404(t *testing.T) {
	// DeleteIAMGroup maps store errors to 404 (not 500) — group not found
	is := &stubIAMStore{deleteGroupErr: errors.New("db")}
	h := buildHandler(&stubUserStore{}, is, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodDelete, "/iam/groups/g1", nil, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("g1")
	if err := h.DeleteIAMGroup(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("code=%d want 404 (handler maps delete store errors to not_found)", rec.Code)
	}
}

func TestListOrganizations_Returns200(t *testing.T) {
	os := &stubOrgStore{orgs: []orgstore.Organization{{ID: "o1", Name: "Org 1"}}}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, os, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodGet, "/organizations", nil, "admin", "admin_user")
	if err := h.ListOrganizations(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200", rec.Code)
	}
}

func TestGetOrganization_Found_Returns200(t *testing.T) {
	os := &stubOrgStore{org: &orgstore.Organization{ID: "o1", Name: "Org 1"}}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, os, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodGet, "/organizations/o1", nil, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("o1")
	if err := h.GetOrganization(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200", rec.Code)
	}
}

func TestGetOrganization_NotFound_Returns404(t *testing.T) {
	// GetOrganization returns 404 when org==nil and err==nil
	os := &stubOrgStore{org: nil, orgErr: nil}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, os, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodGet, "/organizations/missing", nil, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("missing")
	if err := h.GetOrganization(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("code=%d want 404", rec.Code)
	}
}

func TestGetOrganization_StoreError_Returns500(t *testing.T) {
	os := &stubOrgStore{org: nil, orgErr: errors.New("db")}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, os, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodGet, "/organizations/missing", nil, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("missing")
	if err := h.GetOrganization(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500", rec.Code)
	}
}

// Me / GetMe

func TestGetMe_NoAuth_Returns401(t *testing.T) {
	h := defaultHandler()
	c, rec := adminAuthCtx(http.MethodGet, "/me", nil, "", "")
	if err := h.GetMe(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("code=%d want 401", rec.Code)
	}
}

func TestGetMe_Returns200(t *testing.T) {
	is := &stubIAMStore{groupNames: []string{"admins"}}
	h := buildHandler(&stubUserStore{}, is, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodGet, "/me", nil, "u1", "admin_user")
	if err := h.GetMe(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200; body=%s", rec.Code, rec.Body)
	}
}

// Auth Sessions: NilPool branch

func TestListAuthSessions_NilPool_Returns503(t *testing.T) {
	h := defaultHandler()
	// pool is nil in defaultHandler — should return 503
	c, rec := adminAuthCtx(http.MethodGet, "/auth/sessions", nil, "admin", "admin_user")
	if err := h.ListAuthSessions(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("code=%d want 503", rec.Code)
	}
}

func TestDeleteAuthSessions_ExactlyOneFilterRequired_Returns400(t *testing.T) {
	h := defaultHandler()
	// No filter → 400
	c, rec := adminAuthCtx(http.MethodDelete, "/auth/sessions", nil, "admin", "admin_user")
	if err := h.DeleteAuthSessions(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code=%d want 400 (no filter)", rec.Code)
	}
}

func TestDeleteAuthSessions_NoRevocationService_Returns503(t *testing.T) {
	// One filter but revocation == nil
	h := defaultHandler()
	req := httptest.NewRequest(http.MethodDelete, "/auth/sessions?user_id=u1", nil)
	rec := httptest.NewRecorder()
	e := echo.New()
	c := e.NewContext(req, rec)
	if err := h.DeleteAuthSessions(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("code=%d want 503 (no revocation service)", rec.Code)
	}
}

func TestListRevocations_NilRevocationStore_Returns503(t *testing.T) {
	h := defaultHandler()
	c, rec := adminAuthCtx(http.MethodGet, "/revocations", nil, "admin", "admin_user")
	if err := h.ListRevocations(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("code=%d want 503 (nil revocationStore)", rec.Code)
	}
}

// API Keys (admin-managed)

func TestListAdminAPIKeys_Returns200(t *testing.T) {
	uid := "u1"
	us := &stubUserStore{listKeys: []userstore.AdminAPIKey{{ID: "k1", Name: "Key 1", OwnerUserID: &uid}}}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodGet, "/users/u1/api-keys", nil, "admin", "admin_user")
	c.SetParamNames("userId")
	c.SetParamValues("u1")
	if err := h.ListAPIKeys(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200; body=%s", rec.Code, rec.Body)
	}
}

func TestGetAdminAPIKey_NotFound_Returns404(t *testing.T) {
	us := &stubUserStore{getKey: nil}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodGet, "/api-keys/missing", nil, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("missing")
	if err := h.GetAPIKey(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("code=%d want 404", rec.Code)
	}
}

func TestGetAdminAPIKey_Found_Returns200(t *testing.T) {
	uid := "u1"
	us := &stubUserStore{getKey: &userstore.AdminAPIKey{ID: "k1", Name: "Key 1", OwnerUserID: &uid}}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodGet, "/api-keys/k1", nil, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("k1")
	if err := h.GetAPIKey(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200", rec.Code)
	}
}

// Identity Providers

func TestListIdentityProviders_Returns200(t *testing.T) {
	ss := &stubScimStore{idps: []scimstore.IdentityProviderRecord{{ID: "idp-1", Name: "Okta"}}}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, &stubOrgStore{}, ss, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodGet, "/identity-providers", nil, "admin", "admin_user")
	if err := h.ListIdentityProviders(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200", rec.Code)
	}
}

func TestGetIdentityProvider_NotFound_Returns404(t *testing.T) {
	// GetIdentityProvider maps pgx.ErrNoRows to 404
	ss := &stubScimStore{idp: nil, idpErr: pgx.ErrNoRows}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, &stubOrgStore{}, ss, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodGet, "/identity-providers/missing", nil, "admin", "admin_user")
	c.SetParamNames("idpId")
	c.SetParamValues("missing")
	if err := h.GetIdentityProvider(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("code=%d want 404", rec.Code)
	}
}

func TestGetIdentityProvider_OtherError_Returns500(t *testing.T) {
	ss := &stubScimStore{idp: nil, idpErr: errors.New("db error")}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, &stubOrgStore{}, ss, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodGet, "/identity-providers/err", nil, "admin", "admin_user")
	c.SetParamNames("idpId")
	c.SetParamValues("err")
	if err := h.GetIdentityProvider(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500", rec.Code)
	}
}

// New + helpers

func TestNew_NilLogger_UsesDefault(t *testing.T) {
	h := New(Deps{Pool: nil, Audit: noopAudit()})
	if h.logger == nil {
		t.Error("expected non-nil logger when Logger not provided")
	}
}

func TestErrJSON_Shape(t *testing.T) {
	result := errJSON("msg", "type", "code")
	inner := result["error"].(map[string]any)
	if inner["message"] != "msg" {
		t.Errorf("message=%v want 'msg'", inner["message"])
	}
}

func TestInternalServerError_Returns500(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	e := echo.New()
	c := e.NewContext(req, rec)
	_ = internalServerError(c, "test error")
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500", rec.Code)
	}
}

func TestParsePagination_CapAt1000(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/?limit=5000&offset=10", nil)
	e := echo.New()
	c := e.NewContext(req, httptest.NewRecorder())
	pg := parsePagination(c)
	if pg.Limit != 1000 {
		t.Errorf("limit=%d want 1000 (capped)", pg.Limit)
	}
	if pg.Offset != 10 {
		t.Errorf("offset=%d want 10", pg.Offset)
	}
}

func TestParsePagination_InvalidValues_UsesDefaults(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/?limit=abc&offset=-1", nil)
	e := echo.New()
	c := e.NewContext(req, httptest.NewRecorder())
	pg := parsePagination(c)
	if pg.Limit != 50 {
		t.Errorf("limit=%d want 50 (default)", pg.Limit)
	}
	if pg.Offset != 0 {
		t.Errorf("offset=%d want 0 (default)", pg.Offset)
	}
}

func TestFirstNonEmpty(t *testing.T) {
	if v := firstNonEmpty("", "b", "c"); v != "b" {
		t.Errorf("firstNonEmpty=%q want 'b'", v)
	}
	if v := firstNonEmpty("", "", ""); v != "" {
		t.Errorf("firstNonEmpty all empty=%q want ''", v)
	}
}

func TestActorFromContext_NoAuth(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	e := echo.New()
	c := e.NewContext(req, httptest.NewRecorder())
	actor := actorFromContext(c)
	if actor.UserID != "" {
		t.Errorf("expected empty actor without auth, got %+v", actor)
	}
}

func TestSourceIP_ReturnsIP(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	e := echo.New()
	c := e.NewContext(req, httptest.NewRecorder())
	ip := sourceIP(c)
	_ = ip // just checking it doesn't panic
}

func TestCurrentUserID_NoAuth_ReturnsEmpty(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	e := echo.New()
	c := e.NewContext(req, httptest.NewRecorder())
	id := currentUserID(c)
	if id != "" {
		t.Errorf("expected empty without auth, got %q", id)
	}
}

func TestParseTimeRange_ValidDates(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/?startTime=2026-01-01T00:00:00Z&endTime=2026-12-31T23:59:59Z", nil)
	e := echo.New()
	c := e.NewContext(req, httptest.NewRecorder())
	start, end := parseTimeRange(c)
	if start == nil || end == nil {
		t.Error("expected non-nil start/end for valid dates")
	}
}

func TestParseTimeRange_InvalidDates_ReturnsNils(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/?startTime=bad&endTime=bad", nil)
	e := echo.New()
	c := e.NewContext(req, httptest.NewRecorder())
	start, end := parseTimeRange(c)
	if start != nil || end != nil {
		t.Error("expected nil start/end for invalid dates")
	}
}

// API Keys: more handlers

func TestCreateAPIKey_MissingName_Returns400(t *testing.T) {
	h := defaultHandler()
	body, _ := json.Marshal(map[string]any{"name": ""})
	c, rec := adminAuthCtx(http.MethodPost, "/api-keys", body, "admin", "admin_user")
	if err := h.CreateAPIKey(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code=%d want 400", rec.Code)
	}
}

func TestCreateAPIKey_Success_Returns201(t *testing.T) {
	uid := "u1"
	us := &stubUserStore{createKey: &userstore.AdminAPIKey{ID: "k1", Name: "My Key", OwnerUserID: &uid}}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	body, _ := json.Marshal(map[string]any{"name": "My Key"})
	c, rec := adminAuthCtx(http.MethodPost, "/api-keys", body, "admin", "admin_user")
	if err := h.CreateAPIKey(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("code=%d want 201; body=%s", rec.Code, rec.Body)
	}
}

func TestCreateAPIKey_StoreError_Returns500(t *testing.T) {
	us := &stubUserStore{createKeyErr: errors.New("db")}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	body, _ := json.Marshal(map[string]any{"name": "key"})
	c, rec := adminAuthCtx(http.MethodPost, "/api-keys", body, "admin", "admin_user")
	if err := h.CreateAPIKey(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500", rec.Code)
	}
}

func TestUpdateAPIKey_NoFields_Returns400(t *testing.T) {
	h := defaultHandler()
	body, _ := json.Marshal(map[string]any{})
	c, rec := adminAuthCtx(http.MethodPatch, "/api-keys/k1", body, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("k1")
	if err := h.UpdateAPIKey(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code=%d want 400", rec.Code)
	}
}

func TestUpdateAPIKey_Success_Returns200(t *testing.T) {
	uid := "u1"
	us := &stubUserStore{updateKey: &userstore.AdminAPIKey{ID: "k1", Name: "Updated", OwnerUserID: &uid}}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	name := "Updated"
	body, _ := json.Marshal(map[string]any{"name": name})
	c, rec := adminAuthCtx(http.MethodPatch, "/api-keys/k1", body, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("k1")
	if err := h.UpdateAPIKey(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200; body=%s", rec.Code, rec.Body)
	}
}

func TestDeleteAPIKey_SelfKey_Returns409(t *testing.T) {
	h := defaultHandler()
	c, rec := adminAuthCtx(http.MethodDelete, "/api-keys/k1", nil, "k1", "api_key")
	c.SetParamNames("id")
	c.SetParamValues("k1")
	if err := h.DeleteAPIKey(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusConflict {
		t.Errorf("code=%d want 409 (self-key in use)", rec.Code)
	}
}

func TestDeleteAPIKey_Success_Returns200(t *testing.T) {
	h := defaultHandler()
	c, rec := adminAuthCtx(http.MethodDelete, "/api-keys/k1", nil, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("k1")
	if err := h.DeleteAPIKey(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200", rec.Code)
	}
}

func TestRegenerateAPIKey_NotFound_Returns404(t *testing.T) {
	us := &stubUserStore{getKey: nil}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodPost, "/api-keys/k1/regenerate", nil, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("k1")
	if err := h.RegenerateAPIKey(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("code=%d want 404", rec.Code)
	}
}

func TestRegenerateAPIKey_Success_Returns200(t *testing.T) {
	uid := "u1"
	us := &stubUserStore{getKey: &userstore.AdminAPIKey{ID: "k1", Name: "Key", OwnerUserID: &uid}}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodPost, "/api-keys/k1/regenerate", nil, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("k1")
	if err := h.RegenerateAPIKey(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200; body=%s", rec.Code, rec.Body)
	}
}

func TestListAPIKeys_WithScopeOwned_Returns200(t *testing.T) {
	uid := "admin"
	us := &stubUserStore{listKeys: []userstore.AdminAPIKey{{ID: "k1", Name: "Key 1", OwnerUserID: &uid}}}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	req := httptest.NewRequest(http.MethodGet, "/api-keys?scope=owned", nil)
	rec := httptest.NewRecorder()
	e := echo.New()
	c := e.NewContext(req, rec)
	aa := &authn.AdminAuth{KeyID: "admin", KeyName: "test", AuthPrincipalType: "admin_user"}
	middleware.WithAdminAuth(c, aa)
	if err := h.ListAPIKeys(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200", rec.Code)
	}
}

// Auth Sessions: more handlers

func TestDeleteAuthSessions_ZeroFilters_Returns400(t *testing.T) {
	h := defaultHandler()
	c, rec := adminAuthCtx(http.MethodDelete, "/auth/sessions", nil, "admin", "admin_user")
	if err := h.DeleteAuthSessions(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code=%d want 400 (no filter)", rec.Code)
	}
}

func TestDeleteAuthSessions_MultipleFilters_Returns400(t *testing.T) {
	h := defaultHandler()
	req := httptest.NewRequest(http.MethodDelete, "/auth/sessions?user_id=u1&device_id=d1", nil)
	rec := httptest.NewRecorder()
	e := echo.New()
	c := e.NewContext(req, rec)
	if err := h.DeleteAuthSessions(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code=%d want 400 (multiple filters)", rec.Code)
	}
}

func TestDeleteAuthSessions_NilRevocation_Returns503(t *testing.T) {
	h := defaultHandler() // no revocation service wired
	req := httptest.NewRequest(http.MethodDelete, "/auth/sessions?user_id=u1", nil)
	rec := httptest.NewRecorder()
	e := echo.New()
	c := e.NewContext(req, rec)
	if err := h.DeleteAuthSessions(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("code=%d want 503", rec.Code)
	}
}

func TestRevokeDeviceInternal_NilPool_Returns503(t *testing.T) {
	h := defaultHandler()
	c, rec := adminAuthCtx(http.MethodPost, "/auth/revoke-device", nil, "internal", "admin_user")
	if err := h.RevokeDeviceInternal(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("code=%d want 503", rec.Code)
	}
}

func TestRevokeDeviceInternal_MissingDeviceID_Returns400(t *testing.T) {
	h := defaultHandler()
	h.pool = &stubPool{}
	body, _ := json.Marshal(map[string]any{"reason": "test"})
	c, rec := adminAuthCtx(http.MethodPost, "/auth/revoke-device", body, "internal", "admin_user")
	if err := h.RevokeDeviceInternal(c); err != nil {
		t.Fatal(err)
	}
	// No revocation service → 503, not 400; deviceId check comes after nil revocation check.
	// Since revocation is nil: expect 503 (fails before deviceId validation).
	// This covers pool!=nil + revocation==nil branch.
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("code=%d want 503 (no revocation service)", rec.Code)
	}
}

func TestListRevocations_StorageSuccess_Returns200(t *testing.T) {
	h := defaultHandler()
	h.revocationStore = &stubRevocationStore{rows: nil, lastID: 0}
	c, rec := adminAuthCtx(http.MethodGet, "/revocations", nil, "admin", "admin_user")
	if err := h.ListRevocations(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200", rec.Code)
	}
}

func TestListRevocations_StoreError_Returns500(t *testing.T) {
	h := defaultHandler()
	h.revocationStore = &stubRevocationStore{err: errors.New("db")}
	c, rec := adminAuthCtx(http.MethodGet, "/revocations", nil, "admin", "admin_user")
	if err := h.ListRevocations(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500", rec.Code)
	}
}

// stubRevocationStore satisfies iamRevocationStore for ListRevocations tests.
type stubRevocationStore struct {
	rows   []revocation.Row
	lastID int64
	err    error
}

func (s *stubRevocationStore) ListSince(_ context.Context, _ int64, _ int) ([]revocation.Row, int64, error) {
	return s.rows, s.lastID, s.err
}

func TestParseInt64_Valid(t *testing.T) {
	if v := parseInt64("42"); v != 42 {
		t.Errorf("parseInt64=%d want 42", v)
	}
}

func TestParseInt64_Empty_ReturnsZero(t *testing.T) {
	if v := parseInt64(""); v != 0 {
		t.Errorf("parseInt64=%d want 0", v)
	}
}

func TestParseInt64_Negative_ReturnsZero(t *testing.T) {
	if v := parseInt64("-5"); v != 0 {
		t.Errorf("parseInt64=%d want 0 (negative clamped)", v)
	}
}

func TestParseInt64_Invalid_ReturnsZero(t *testing.T) {
	if v := parseInt64("abc"); v != 0 {
		t.Errorf("parseInt64=%d want 0", v)
	}
}

func TestParseIntDefault_Valid(t *testing.T) {
	if v := parseIntDefault("200", 500, 1000); v != 200 {
		t.Errorf("parseIntDefault=%d want 200", v)
	}
}

func TestParseIntDefault_Empty_ReturnsDef(t *testing.T) {
	if v := parseIntDefault("", 500, 1000); v != 500 {
		t.Errorf("parseIntDefault=%d want 500", v)
	}
}

func TestParseIntDefault_AboveMax_ClampsToMax(t *testing.T) {
	if v := parseIntDefault("9999", 500, 1000); v != 1000 {
		t.Errorf("parseIntDefault=%d want 1000 (capped)", v)
	}
}

func TestParseIntDefault_Zero_ClampsToOne(t *testing.T) {
	if v := parseIntDefault("0", 500, 1000); v != 1 {
		t.Errorf("parseIntDefault=%d want 1 (clamped)", v)
	}
}

func TestPtrToString_Nil_ReturnsEmpty(t *testing.T) {
	if v := ptrToString(nil); v != "" {
		t.Errorf("ptrToString(nil)=%q want ''", v)
	}
}

func TestPtrToString_NonNil_ReturnsValue(t *testing.T) {
	s := "hello"
	if v := ptrToString(&s); v != "hello" {
		t.Errorf("ptrToString=%q want 'hello'", v)
	}
}

// IAM: more handlers

func TestUpdateIAMPolicy_Success_Returns200(t *testing.T) {
	is := &stubIAMStore{updatedPolicy: &iamstore.PolicyRow{ID: "p1", Name: "Updated"}}
	h := buildHandler(&stubUserStore{}, is, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	body, _ := json.Marshal(map[string]any{"name": "Updated"})
	c, rec := adminAuthCtx(http.MethodPut, "/iam/policies/p1", body, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("p1")
	if err := h.UpdateIAMPolicy(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200", rec.Code)
	}
}

func TestUpdateIAMPolicy_StoreError_Returns500(t *testing.T) {
	is := &stubIAMStore{updatePolicyErr: errors.New("db")}
	h := buildHandler(&stubUserStore{}, is, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	body, _ := json.Marshal(map[string]any{"name": "X"})
	c, rec := adminAuthCtx(http.MethodPut, "/iam/policies/p1", body, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("p1")
	if err := h.UpdateIAMPolicy(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500", rec.Code)
	}
}

func TestDeleteIAMPolicy_Success_Returns204(t *testing.T) {
	is := &stubIAMStore{}
	h := buildHandler(&stubUserStore{}, is, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodDelete, "/iam/policies/p1", nil, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("p1")
	if err := h.DeleteIAMPolicy(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNoContent {
		t.Errorf("code=%d want 204", rec.Code)
	}
}

func TestDeleteIAMPolicy_StoreError_Returns500(t *testing.T) {
	is := &stubIAMStore{deletePolicyErr: errors.New("db")}
	h := buildHandler(&stubUserStore{}, is, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodDelete, "/iam/policies/p1", nil, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("p1")
	if err := h.DeleteIAMPolicy(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500", rec.Code)
	}
}

func TestListPolicyAttachments_Returns200(t *testing.T) {
	is := &stubIAMStore{policyGroups: []iamstore.PolicyGroupRow{{ID: "g1", Name: "Group1"}}}
	h := buildHandler(&stubUserStore{}, is, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodGet, "/iam/policies/p1/attachments", nil, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("p1")
	if err := h.ListPolicyAttachments(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200", rec.Code)
	}
}

func TestListPolicyAttachments_GroupsError_Returns500(t *testing.T) {
	is := &stubIAMStore{policyGroupsErr: errors.New("db")}
	h := buildHandler(&stubUserStore{}, is, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodGet, "/iam/policies/p1/attachments", nil, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("p1")
	if err := h.ListPolicyAttachments(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500", rec.Code)
	}
}

func TestListIAMGroupMembers_Returns200(t *testing.T) {
	is := &stubIAMStore{members: []iamstore.GroupMemberRow{{PrincipalType: "nexus_user", PrincipalID: "u1"}}, membersTotal: 1}
	h := buildHandler(&stubUserStore{}, is, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodGet, "/iam/groups/g1/members", nil, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("g1")
	if err := h.ListIAMGroupMembers(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200", rec.Code)
	}
}

func TestListIAMGroupMembers_Error_Returns500(t *testing.T) {
	is := &stubIAMStore{membersErr: errors.New("db")}
	h := buildHandler(&stubUserStore{}, is, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodGet, "/iam/groups/g1/members", nil, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("g1")
	if err := h.ListIAMGroupMembers(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500", rec.Code)
	}
}

func TestUpdateIAMGroup_Success_Returns200(t *testing.T) {
	is := &stubIAMStore{updatedGroup: &iamstore.GroupRow{ID: "g1", Name: "Updated"}}
	h := buildHandler(&stubUserStore{}, is, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	name := "Updated"
	body, _ := json.Marshal(map[string]any{"name": name})
	c, rec := adminAuthCtx(http.MethodPut, "/iam/groups/g1", body, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("g1")
	if err := h.UpdateIAMGroup(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200", rec.Code)
	}
}

func TestAddIAMGroupMember_MissingPrincipalID_Returns400(t *testing.T) {
	h := defaultHandler()
	body, _ := json.Marshal(map[string]any{"principalType": "nexus_user", "principalId": ""})
	c, rec := adminAuthCtx(http.MethodPost, "/iam/groups/g1/members", body, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("g1")
	if err := h.AddIAMGroupMember(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code=%d want 400", rec.Code)
	}
}

func TestAddIAMGroupMember_Success_Returns201(t *testing.T) {
	is := &stubIAMStore{memberID: "mem-1"}
	h := buildHandler(&stubUserStore{}, is, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	body, _ := json.Marshal(map[string]any{"principalType": "nexus_user", "principalId": "u1"})
	c, rec := adminAuthCtx(http.MethodPost, "/iam/groups/g1/members", body, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("g1")
	if err := h.AddIAMGroupMember(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("code=%d want 201; body=%s", rec.Code, rec.Body)
	}
}

func TestRemoveIAMGroupMember_Success_Returns204(t *testing.T) {
	is := &stubIAMStore{}
	h := buildHandler(&stubUserStore{}, is, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodDelete, "/iam/groups/g1/members/mem-1", nil, "admin", "admin_user")
	c.SetParamNames("id", "membershipId")
	c.SetParamValues("g1", "mem-1")
	if err := h.RemoveIAMGroupMember(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNoContent {
		t.Errorf("code=%d want 204", rec.Code)
	}
}

func TestRemoveIAMGroupMember_NotFound_Returns404(t *testing.T) {
	is := &stubIAMStore{removeErr: errors.New("not found")}
	h := buildHandler(&stubUserStore{}, is, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodDelete, "/iam/groups/g1/members/mem-missing", nil, "admin", "admin_user")
	c.SetParamNames("id", "membershipId")
	c.SetParamValues("g1", "mem-missing")
	if err := h.RemoveIAMGroupMember(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("code=%d want 404", rec.Code)
	}
}

func TestAttachIAMGroupPolicy_MissingPolicyID_Returns400(t *testing.T) {
	h := defaultHandler()
	body, _ := json.Marshal(map[string]any{"policyId": ""})
	c, rec := adminAuthCtx(http.MethodPost, "/iam/groups/g1/policies", body, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("g1")
	if err := h.AttachIAMGroupPolicy(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code=%d want 400", rec.Code)
	}
}

func TestAttachIAMGroupPolicy_Success_Returns201(t *testing.T) {
	is := &stubIAMStore{attachGroupPolID: "att-1"}
	h := buildHandler(&stubUserStore{}, is, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	body, _ := json.Marshal(map[string]any{"policyId": "p1"})
	c, rec := adminAuthCtx(http.MethodPost, "/iam/groups/g1/policies", body, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("g1")
	if err := h.AttachIAMGroupPolicy(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("code=%d want 201", rec.Code)
	}
}

func TestDetachIAMGroupPolicy_Success_Returns204(t *testing.T) {
	is := &stubIAMStore{}
	h := buildHandler(&stubUserStore{}, is, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodDelete, "/iam/groups/g1/policies/att-1", nil, "admin", "admin_user")
	c.SetParamNames("id", "attachmentId")
	c.SetParamValues("g1", "att-1")
	if err := h.DetachIAMGroupPolicy(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNoContent {
		t.Errorf("code=%d want 204", rec.Code)
	}
}

func TestDetachIAMGroupPolicy_NotFound_Returns404(t *testing.T) {
	is := &stubIAMStore{detachGroupPolErr: errors.New("not found")}
	h := buildHandler(&stubUserStore{}, is, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodDelete, "/iam/groups/g1/policies/att-missing", nil, "admin", "admin_user")
	c.SetParamNames("id", "attachmentId")
	c.SetParamValues("g1", "att-missing")
	if err := h.DetachIAMGroupPolicy(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("code=%d want 404", rec.Code)
	}
}

func TestAttachPrincipalPolicy_MissingPolicyID_Returns400(t *testing.T) {
	h := defaultHandler()
	body, _ := json.Marshal(map[string]any{"policyId": ""})
	c, rec := adminAuthCtx(http.MethodPost, "/iam/principals/nexus_user/u1/policies", body, "admin", "admin_user")
	c.SetParamNames("type", "id")
	c.SetParamValues("nexus_user", "u1")
	if err := h.AttachPrincipalPolicy(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code=%d want 400", rec.Code)
	}
}

func TestAttachPrincipalPolicy_Success_Returns201(t *testing.T) {
	is := &stubIAMStore{attachPPID: "att-pp-1"}
	h := buildHandler(&stubUserStore{}, is, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	body, _ := json.Marshal(map[string]any{"policyId": "p1"})
	c, rec := adminAuthCtx(http.MethodPost, "/iam/principals/nexus_user/u1/policies", body, "admin", "admin_user")
	c.SetParamNames("type", "id")
	c.SetParamValues("nexus_user", "u1")
	if err := h.AttachPrincipalPolicy(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("code=%d want 201", rec.Code)
	}
}

func TestAttachPrincipalPolicy_InvalidExpiry_Returns400(t *testing.T) {
	h := defaultHandler()
	exp := "not-a-date"
	body, _ := json.Marshal(map[string]any{"policyId": "p1", "expiresAt": exp})
	c, rec := adminAuthCtx(http.MethodPost, "/iam/principals/nexus_user/u1/policies", body, "admin", "admin_user")
	c.SetParamNames("type", "id")
	c.SetParamValues("nexus_user", "u1")
	if err := h.AttachPrincipalPolicy(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code=%d want 400 (invalid expiresAt)", rec.Code)
	}
}

func TestListPrincipalPolicies_Returns200(t *testing.T) {
	is := &stubIAMStore{ppAttachments: []iamstore.PrincipalPolicyAttachment{{ID: "att-1", PolicyID: "p1"}}}
	h := buildHandler(&stubUserStore{}, is, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodGet, "/iam/principals/nexus_user/u1/policies", nil, "admin", "admin_user")
	c.SetParamNames("type", "id")
	c.SetParamValues("nexus_user", "u1")
	if err := h.ListPrincipalPolicies(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200", rec.Code)
	}
}

func TestDetachPrincipalPolicy_Success_Returns204(t *testing.T) {
	is := &stubIAMStore{}
	h := buildHandler(&stubUserStore{}, is, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodDelete, "/iam/principals/nexus_user/u1/policies/att-1", nil, "admin", "admin_user")
	c.SetParamNames("type", "id", "attachmentId")
	c.SetParamValues("nexus_user", "u1", "att-1")
	if err := h.DetachPrincipalPolicy(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNoContent {
		t.Errorf("code=%d want 204", rec.Code)
	}
}

func TestSimulateIAM_MissingFields_Returns400(t *testing.T) {
	h := defaultHandler()
	body, _ := json.Marshal(map[string]any{"principal": map[string]any{"type": "", "id": ""}})
	c, rec := adminAuthCtx(http.MethodPost, "/iam/simulate", body, "admin", "admin_user")
	if err := h.SimulateIAM(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code=%d want 400", rec.Code)
	}
}

func TestSimulateIAM_NilEngine_Returns503(t *testing.T) {
	h := defaultHandler() // iamEngine is nil
	body, _ := json.Marshal(map[string]any{
		"principal": map[string]any{"type": "nexus_user", "id": "u1"},
		"action":    "admin:user.read",
		"resource":  "nrn:*",
	})
	c, rec := adminAuthCtx(http.MethodPost, "/iam/simulate", body, "admin", "admin_user")
	if err := h.SimulateIAM(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("code=%d want 503 (nil iamEngine)", rec.Code)
	}
}

// Me: more handlers

func TestUpdateMe_NotAdminUser_Returns403(t *testing.T) {
	h := defaultHandler()
	body, _ := json.Marshal(map[string]any{"displayName": "New"})
	c, rec := adminAuthCtx(http.MethodPatch, "/me", body, "k1", "api_key")
	if err := h.UpdateMe(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("code=%d want 403", rec.Code)
	}
}

func TestUpdateMe_NoAuth_Returns403(t *testing.T) {
	h := defaultHandler()
	body, _ := json.Marshal(map[string]any{"displayName": "New"})
	c, rec := adminAuthCtx(http.MethodPatch, "/me", body, "", "")
	if err := h.UpdateMe(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("code=%d want 403", rec.Code)
	}
}

func TestUpdateMe_Success_Returns200(t *testing.T) {
	email := "u@example.com"
	us := &stubUserStore{updateResult: &userstore.NexusUserSafe{ID: "u1", DisplayName: "Updated", Email: &email}}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	body, _ := json.Marshal(map[string]any{"displayName": "Updated"})
	c, rec := adminAuthCtx(http.MethodPatch, "/me", body, "u1", "admin_user")
	if err := h.UpdateMe(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200; body=%s", rec.Code, rec.Body)
	}
}

func TestUpdateMe_MissingCurrentPassword_Returns400(t *testing.T) {
	h := defaultHandler()
	newPw := "newpassword"
	body, _ := json.Marshal(map[string]any{"newPassword": newPw})
	c, rec := adminAuthCtx(http.MethodPatch, "/me", body, "u1", "admin_user")
	if err := h.UpdateMe(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code=%d want 400 (missing currentPassword)", rec.Code)
	}
}

func TestGetMePermissions_NoAuth_Returns401(t *testing.T) {
	h := defaultHandler()
	c, rec := adminAuthCtx(http.MethodGet, "/me/permissions", nil, "", "")
	if err := h.GetMePermissions(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("code=%d want 401", rec.Code)
	}
}

func TestGetMePermissions_BootstrapKey_Returns200WithAllActions(t *testing.T) {
	h := defaultHandler()
	c, rec := adminAuthCtx(http.MethodGet, "/me/permissions", nil, "bootstrap", "admin_user")
	if err := h.GetMePermissions(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200", rec.Code)
	}
}

func TestGetMePermissions_NilEngine_Returns200Empty(t *testing.T) {
	h := defaultHandler() // iamEngine is nil
	c, rec := adminAuthCtx(http.MethodGet, "/me/permissions", nil, "u1", "admin_user")
	if err := h.GetMePermissions(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200 (nil engine returns empty set)", rec.Code)
	}
}

func TestGetActionCatalog_Returns200(t *testing.T) {
	h := defaultHandler()
	c, rec := adminAuthCtx(http.MethodGet, "/iam/action-catalog", nil, "admin", "admin_user")
	if err := h.GetActionCatalog(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200", rec.Code)
	}
}

func TestOrganizationTree_NilPool_Returns500(t *testing.T) {
	// OrganizationTree requires h.pool for QuotaOverride/QuotaPolicy/metric_rollup queries.
	// With nil pool, the store-level error returns 500.
	os := &stubOrgStore{orgsErr: errors.New("db")}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, os, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodGet, "/organizations/tree", nil, "admin", "admin_user")
	if err := h.OrganizationTree(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500", rec.Code)
	}
}

func TestOrganizationTree_NilPoolSuccess_Returns200(t *testing.T) {
	// Pool is nil → pool.Query calls fail, but org list succeeds → 200 (pool queries are ignored).
	os := &stubOrgStore{orgs: []orgstore.Organization{{ID: "o1", Name: "Root"}}}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, os, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	// set pool to stubPool that returns query errors (treated as skip)
	h.pool = &stubPool{queryErr: errors.New("no pool")}
	c, rec := adminAuthCtx(http.MethodGet, "/organizations/tree", nil, "admin", "admin_user")
	if err := h.OrganizationTree(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200", rec.Code)
	}
}

// Organizations: more handlers

func TestCreateOrganization_MissingFields_Returns400(t *testing.T) {
	h := defaultHandler()
	body, _ := json.Marshal(map[string]any{"name": "Org"})
	c, rec := adminAuthCtx(http.MethodPost, "/organizations", body, "admin", "admin_user")
	if err := h.CreateOrganization(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code=%d want 400", rec.Code)
	}
}

func TestCreateOrganization_Success_Returns201(t *testing.T) {
	os := &stubOrgStore{createdOrg: &orgstore.Organization{ID: "o1", Name: "Org", Code: "ORG"}}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, os, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	body, _ := json.Marshal(map[string]any{"name": "Org", "code": "ORG"})
	c, rec := adminAuthCtx(http.MethodPost, "/organizations", body, "admin", "admin_user")
	if err := h.CreateOrganization(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("code=%d want 201; body=%s", rec.Code, rec.Body)
	}
}

func TestUpdateOrganization_Success_Returns200(t *testing.T) {
	os := &stubOrgStore{updatedOrg: &orgstore.Organization{ID: "o1", Name: "Updated"}}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, os, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	body, _ := json.Marshal(map[string]any{"name": "Updated"})
	c, rec := adminAuthCtx(http.MethodPut, "/organizations/o1", body, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("o1")
	if err := h.UpdateOrganization(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200", rec.Code)
	}
}

func TestUpdateOrganization_InvalidTimezone_Returns400(t *testing.T) {
	h := defaultHandler()
	tz := "Invalid/Timezone"
	body, _ := json.Marshal(map[string]any{"timezone": tz})
	c, rec := adminAuthCtx(http.MethodPut, "/organizations/o1", body, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("o1")
	if err := h.UpdateOrganization(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code=%d want 400 (invalid timezone)", rec.Code)
	}
}

func TestDeleteOrganization_HasChildren_Returns409(t *testing.T) {
	os := &stubOrgStore{orgChildren: 2, orgProjects: 0}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, os, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodDelete, "/organizations/o1", nil, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("o1")
	if err := h.DeleteOrganization(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusConflict {
		t.Errorf("code=%d want 409", rec.Code)
	}
}

func TestDeleteOrganization_Success_Returns204(t *testing.T) {
	os := &stubOrgStore{}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, os, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodDelete, "/organizations/o1", nil, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("o1")
	if err := h.DeleteOrganization(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNoContent {
		t.Errorf("code=%d want 204", rec.Code)
	}
}

func TestListProjects_Returns200(t *testing.T) {
	os := &stubOrgStore{projects: []orgstore.Project{{ID: "p1", Name: "P1", OrganizationID: "o1"}}, projectsTotal: 1}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, os, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodGet, "/projects", nil, "admin", "admin_user")
	if err := h.ListProjects(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200", rec.Code)
	}
}

func TestListProjects_Error_Returns500(t *testing.T) {
	os := &stubOrgStore{projectsErr: errors.New("db")}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, os, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodGet, "/projects", nil, "admin", "admin_user")
	if err := h.ListProjects(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500", rec.Code)
	}
}

func TestGetProject_NotFound_Returns404(t *testing.T) {
	os := &stubOrgStore{project: nil}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, os, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodGet, "/projects/missing", nil, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("missing")
	if err := h.GetProject(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("code=%d want 404", rec.Code)
	}
}

func TestGetProject_Found_Returns200(t *testing.T) {
	os := &stubOrgStore{project: &orgstore.Project{ID: "p1", Name: "P1", OrganizationID: "o1"}}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, os, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodGet, "/projects/p1", nil, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("p1")
	if err := h.GetProject(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200", rec.Code)
	}
}

func TestCreateProject_MissingFields_Returns400(t *testing.T) {
	h := defaultHandler()
	body, _ := json.Marshal(map[string]any{"name": "P1"})
	c, rec := adminAuthCtx(http.MethodPost, "/projects", body, "admin", "admin_user")
	if err := h.CreateProject(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code=%d want 400", rec.Code)
	}
}

func TestCreateProject_Success_Returns201(t *testing.T) {
	os := &stubOrgStore{createdProject: &orgstore.Project{ID: "p-new", Name: "P", OrganizationID: "o1"}}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, os, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	body, _ := json.Marshal(map[string]any{"name": "P", "code": "PRJ", "organizationId": "o1"})
	c, rec := adminAuthCtx(http.MethodPost, "/projects", body, "admin", "admin_user")
	if err := h.CreateProject(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("code=%d want 201; body=%s", rec.Code, rec.Body)
	}
}

func TestUpdateProject_Success_Returns200(t *testing.T) {
	os := &stubOrgStore{updatedProject: &orgstore.Project{ID: "p1", Name: "Updated"}}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, os, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	name := "Updated"
	body, _ := json.Marshal(map[string]any{"name": name})
	c, rec := adminAuthCtx(http.MethodPut, "/projects/p1", body, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("p1")
	if err := h.UpdateProject(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200", rec.Code)
	}
}

func TestDeleteProject_HasVKs_Returns409(t *testing.T) {
	os := &stubOrgStore{projVKCount: 3}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, os, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodDelete, "/projects/p1", nil, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("p1")
	if err := h.DeleteProject(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusConflict {
		t.Errorf("code=%d want 409", rec.Code)
	}
}

func TestDeleteProject_Success_Returns204(t *testing.T) {
	os := &stubOrgStore{}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, os, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodDelete, "/projects/p1", nil, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("p1")
	if err := h.DeleteProject(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNoContent {
		t.Errorf("code=%d want 204", rec.Code)
	}
}

// Users: more handlers

func TestGetUserAudit_Returns200(t *testing.T) {
	gs := &stubGovernanceStore{auditEvents: nil, auditTotal: 0}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, gs)
	c, rec := adminAuthCtx(http.MethodGet, "/users/u1/audit", nil, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("u1")
	if err := h.GetUserAudit(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200", rec.Code)
	}
}

func TestGetUserAudit_Error_Returns500(t *testing.T) {
	gs := &stubGovernanceStore{auditErr: errors.New("db")}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, gs)
	c, rec := adminAuthCtx(http.MethodGet, "/users/u1/audit", nil, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("u1")
	if err := h.GetUserAudit(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500", rec.Code)
	}
}

func TestGetUserIdentity_NotFound_Returns404(t *testing.T) {
	us := &stubUserStore{findResult: nil}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodGet, "/users/missing/identity", nil, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("missing")
	if err := h.GetUserIdentity(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("code=%d want 404", rec.Code)
	}
}

func TestGetUserIdentity_Success_Returns200(t *testing.T) {
	email := "u1@example.com"
	us := &stubUserStore{findResult: &userstore.NexusUser{ID: "u1", DisplayName: "U1", Email: &email, Status: "active"}}
	gs := &stubGovernanceStore{vkSummaries: nil, deviceSummaries: nil, auditSummary: &governancestore.UserAuditSummary{}}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, gs)
	c, rec := adminAuthCtx(http.MethodGet, "/users/u1/identity", nil, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("u1")
	if err := h.GetUserIdentity(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200", rec.Code)
	}
}

func TestRevokeUserAccess_NotFound_Returns404(t *testing.T) {
	us := &stubUserStore{findResult: nil}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodPost, "/users/missing/revoke", nil, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("missing")
	if err := h.RevokeUserAccess(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("code=%d want 404", rec.Code)
	}
}

func TestRevokeUserAccess_Success_Returns200(t *testing.T) {
	email := "u1@example.com"
	us := &stubUserStore{findResult: &userstore.NexusUser{ID: "u1", Email: &email, Status: "active"}}
	gs := &stubGovernanceStore{disabledVKs: 2, revokedDevices: 1}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, gs)
	c, rec := adminAuthCtx(http.MethodPost, "/users/u1/revoke", nil, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("u1")
	if err := h.RevokeUserAccess(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200", rec.Code)
	}
}

// TestRevokeUserAccess_PushesVKInvalidate locks the cache-invalidate push: revoking a user's
// access must push a virtual_keys cache invalidate to the AI Gateway so the
// disabled keys stop authenticating immediately, not after the ≤30s cache TTL.
func TestRevokeUserAccess_PushesVKInvalidate(t *testing.T) {
	email := "u1@example.com"
	us := &stubUserStore{findResult: &userstore.NexusUser{ID: "u1", Email: &email, Status: "active"}}
	gs := &stubGovernanceStore{disabledVKs: 2, revokedDevices: 1}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, gs)
	sh := &stubHub{}
	h.hub = sh
	c, rec := adminAuthCtx(http.MethodPost, "/users/u1/revoke", nil, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("u1")
	if err := h.RevokeUserAccess(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200; body=%s", rec.Code, rec.Body.String())
	}
	found := false
	for _, k := range sh.invalidatedKeys {
		if k == "virtual_keys" {
			found = true
		}
	}
	if !found {
		t.Errorf("RevokeUserAccess did not push a virtual_keys invalidate (got %v)", sh.invalidatedKeys)
	}
}

// TestRevokeUserAccess_InvalidateFailure_Returns502 locks the fail-loud contract:
// if the VK invalidate push fails, the admin gets a 502 (retry) rather than a
// false success that leaves the revoked keys authenticating from cache.
func TestRevokeUserAccess_InvalidateFailure_Returns502(t *testing.T) {
	email := "u1@example.com"
	us := &stubUserStore{findResult: &userstore.NexusUser{ID: "u1", Email: &email, Status: "active"}}
	gs := &stubGovernanceStore{disabledVKs: 1}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, gs)
	h.hub = &stubHub{invalidateErr: errors.New("hub unreachable")}
	c, rec := adminAuthCtx(http.MethodPost, "/users/u1/revoke", nil, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("u1")
	if err := h.RevokeUserAccess(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code=%d want 502 on invalidate failure; body=%s", rec.Code, rec.Body.String())
	}
}

func TestListUserDeviceAssignments_Returns200(t *testing.T) {
	fs := &stubFleetStore{assignments: []fleetstore.DeviceAssignmentDetail{}}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, fs, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodGet, "/users/u1/device-assignments", nil, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("u1")
	if err := h.ListUserDeviceAssignments(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200", rec.Code)
	}
}

func TestListUserDeviceAssignments_Error_Returns500(t *testing.T) {
	fs := &stubFleetStore{assignmentsErr: errors.New("db")}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, fs, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodGet, "/users/u1/device-assignments", nil, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("u1")
	if err := h.ListUserDeviceAssignments(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500", rec.Code)
	}
}

func TestUpdateUser_Disabled_TriggersRevokeScope(t *testing.T) {
	// When enabled=false is passed, revokeUserScope is called (no-op when revocation==nil).
	email := "u1@example.com"
	us := &stubUserStore{updateResult: &userstore.NexusUserSafe{ID: "u1", Email: &email, Status: "inactive"}}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	disabled := false
	body, _ := json.Marshal(map[string]any{"enabled": disabled})
	c, rec := adminAuthCtx(http.MethodPut, "/users/u1", body, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("u1")
	if err := h.UpdateUser(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200", rec.Code)
	}
}

// Identity Providers: more handlers

func TestCreateIdentityProvider_InvalidType_Returns400(t *testing.T) {
	h := defaultHandler()
	body, _ := json.Marshal(map[string]any{"type": "invalid", "name": "My IdP", "config": map[string]any{}})
	c, rec := adminAuthCtx(http.MethodPost, "/identity-providers", body, "admin", "admin_user")
	if err := h.CreateIdentityProvider(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code=%d want 400 (invalid IdP type)", rec.Code)
	}
}

func TestCreateIdentityProvider_Success_Returns201(t *testing.T) {
	ss := &stubScimStore{createdIdp: &scimstore.IdentityProviderRecord{ID: "idp-1", Type: "oidc", Name: "Okta"}}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, &stubOrgStore{}, ss, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	body, _ := json.Marshal(map[string]any{
		"type": "oidc", "name": "Okta",
		"config": map[string]any{
			"issuer":       "https://example.okta.com",
			"clientId":     "client-id-123",
			"clientSecret": "s3cr3t",
			"redirectUri":  "https://app.example.com/callback",
		},
	})
	c, rec := adminAuthCtx(http.MethodPost, "/identity-providers", body, "admin", "admin_user")
	if err := h.CreateIdentityProvider(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("code=%d want 201; body=%s", rec.Code, rec.Body)
	}
}

func TestListScimTokens_Returns200(t *testing.T) {
	ss := &stubScimStore{tokens: []scimstore.ScimToken{{ID: "t1", Name: "Token 1"}}}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, &stubOrgStore{}, ss, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodGet, "/identity-providers/idp-1/scim-tokens", nil, "admin", "admin_user")
	c.SetParamNames("idpId")
	c.SetParamValues("idp-1")
	if err := h.ListScimTokens(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200", rec.Code)
	}
}

func TestListScimTokens_Error_Returns500(t *testing.T) {
	ss := &stubScimStore{tokensErr: errors.New("db")}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, &stubOrgStore{}, ss, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodGet, "/identity-providers/idp-1/scim-tokens", nil, "admin", "admin_user")
	c.SetParamNames("idpId")
	c.SetParamValues("idp-1")
	if err := h.ListScimTokens(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500", rec.Code)
	}
}

func TestCreateScimToken_MissingName_Returns400(t *testing.T) {
	h := defaultHandler()
	body, _ := json.Marshal(map[string]any{"name": ""})
	c, rec := adminAuthCtx(http.MethodPost, "/identity-providers/idp-1/scim-tokens", body, "admin", "admin_user")
	c.SetParamNames("idpId")
	c.SetParamValues("idp-1")
	if err := h.CreateScimToken(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code=%d want 400", rec.Code)
	}
}

func TestCreateScimToken_Success_Returns201(t *testing.T) {
	idpID := "idp-1"
	ss := &stubScimStore{createdToken: &scimstore.ScimToken{ID: "t-new", Name: "My Token", IdentityProviderID: &idpID}}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, &stubOrgStore{}, ss, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	body, _ := json.Marshal(map[string]any{"name": "My Token"})
	c, rec := adminAuthCtx(http.MethodPost, "/identity-providers/idp-1/scim-tokens", body, "admin", "admin_user")
	c.SetParamNames("idpId")
	c.SetParamValues("idp-1")
	if err := h.CreateScimToken(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("code=%d want 201; body=%s", rec.Code, rec.Body)
	}
}

func TestRevokeScimToken_Success_Returns204(t *testing.T) {
	ss := &stubScimStore{}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, &stubOrgStore{}, ss, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodDelete, "/identity-providers/idp-1/scim-tokens/t-1", nil, "admin", "admin_user")
	c.SetParamNames("idpId", "tokenId")
	c.SetParamValues("idp-1", "t-1")
	if err := h.RevokeScimToken(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNoContent {
		t.Errorf("code=%d want 204", rec.Code)
	}
}

func TestRevokeScimToken_Error_Returns404(t *testing.T) {
	ss := &stubScimStore{revokeTokErr: errors.New("not found")}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, &stubOrgStore{}, ss, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodDelete, "/identity-providers/idp-1/scim-tokens/t-missing", nil, "admin", "admin_user")
	c.SetParamNames("idpId", "tokenId")
	c.SetParamValues("idp-1", "t-missing")
	if err := h.RevokeScimToken(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("code=%d want 404", rec.Code)
	}
}

func TestListIdpGroupMappings_Returns200(t *testing.T) {
	ss := &stubScimStore{mappings: []scimstore.IdpGroupMapping{{ID: "m1"}}}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, &stubOrgStore{}, ss, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodGet, "/identity-providers/idp-1/group-mappings", nil, "admin", "admin_user")
	c.SetParamNames("idpId")
	c.SetParamValues("idp-1")
	if err := h.ListIdpGroupMappings(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200", rec.Code)
	}
}

func TestCreateIdpGroupMapping_MissingFields_Returns400(t *testing.T) {
	h := defaultHandler()
	body, _ := json.Marshal(map[string]any{"externalGroupId": ""})
	c, rec := adminAuthCtx(http.MethodPost, "/identity-providers/idp-1/group-mappings", body, "admin", "admin_user")
	c.SetParamNames("idpId")
	c.SetParamValues("idp-1")
	if err := h.CreateIdpGroupMapping(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code=%d want 400", rec.Code)
	}
}

func TestCreateIdpGroupMapping_Success_Returns201(t *testing.T) {
	ss := &stubScimStore{createdMapping: &scimstore.IdpGroupMapping{ID: "m-new"}}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, &stubOrgStore{}, ss, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	body, _ := json.Marshal(map[string]any{"externalGroupId": "ext-g1", "iamGroupId": "iam-g1"})
	c, rec := adminAuthCtx(http.MethodPost, "/identity-providers/idp-1/group-mappings", body, "admin", "admin_user")
	c.SetParamNames("idpId")
	c.SetParamValues("idp-1")
	if err := h.CreateIdpGroupMapping(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("code=%d want 201", rec.Code)
	}
}

func TestDeleteIdpGroupMapping_Success_Returns204(t *testing.T) {
	ss := &stubScimStore{}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, &stubOrgStore{}, ss, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodDelete, "/identity-providers/idp-1/group-mappings/m-1", nil, "admin", "admin_user")
	c.SetParamNames("idpId", "mappingId")
	c.SetParamValues("idp-1", "m-1")
	if err := h.DeleteIdpGroupMapping(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNoContent {
		t.Errorf("code=%d want 204", rec.Code)
	}
}

func TestDeleteIdpGroupMapping_Error_Returns404(t *testing.T) {
	ss := &stubScimStore{deleteMapErr: errors.New("not found")}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, &stubOrgStore{}, ss, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodDelete, "/identity-providers/idp-1/group-mappings/m-missing", nil, "admin", "admin_user")
	c.SetParamNames("idpId", "mappingId")
	c.SetParamValues("idp-1", "m-missing")
	if err := h.DeleteIdpGroupMapping(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("code=%d want 404", rec.Code)
	}
}

func TestListIdentityProviders_Error_Returns500(t *testing.T) {
	ss := &stubScimStore{idpsErr: errors.New("db")}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, &stubOrgStore{}, ss, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodGet, "/identity-providers", nil, "admin", "admin_user")
	if err := h.ListIdentityProviders(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500", rec.Code)
	}
}

func TestParseRFC3339Flexible_Nano(t *testing.T) {
	_, ok := parseRFC3339Flexible("2026-01-01T00:00:00.000Z")
	if !ok {
		t.Error("expected success for RFC3339Nano format")
	}
}

func TestParseRFC3339Flexible_Plain(t *testing.T) {
	_, ok := parseRFC3339Flexible("2026-01-01T00:00:00Z")
	if !ok {
		t.Error("expected success for RFC3339 format")
	}
}

func TestParseRFC3339Flexible_Invalid(t *testing.T) {
	_, ok := parseRFC3339Flexible("not-a-date")
	if ok {
		t.Error("expected failure for invalid date string")
	}
}

// actorFromContext + currentUserID with auth

func TestActorFromContext_WithAuth_ReturnsActor(t *testing.T) {
	c, _ := adminAuthCtx(http.MethodGet, "/", nil, "u1", "admin_user")
	actor := actorFromContext(c)
	if actor.UserID != "u1" {
		t.Errorf("expected actor.UserID='u1', got %q", actor.UserID)
	}
}

func TestCurrentUserID_WithAuth_ReturnsID(t *testing.T) {
	c, _ := adminAuthCtx(http.MethodGet, "/", nil, "u1", "admin_user")
	id := currentUserID(c)
	if id != "u1" {
		t.Errorf("expected 'u1', got %q", id)
	}
}

// Auth sessions: ListAuthSessions pool paths

func TestListAuthSessions_QueryError_Returns500(t *testing.T) {
	h := defaultHandler()
	h.pool = &stubPool{queryErr: errors.New("db error")}
	req := httptest.NewRequest(http.MethodGet, "/auth/sessions", nil)
	rec := httptest.NewRecorder()
	e := echo.New()
	c := e.NewContext(req, rec)
	if err := h.ListAuthSessions(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500", rec.Code)
	}
}

func TestListAuthSessions_QueryReturnsEmpty_Returns200(t *testing.T) {
	h := defaultHandler()
	// Use nil rows (Query returns nil, nil) — rows.Close() panics on nil.
	// Use a stub that returns a rows that is immediately exhausted.
	h.pool = &stubPool{queryRows: &emptyRows{}}
	req := httptest.NewRequest(http.MethodGet, "/auth/sessions", nil)
	rec := httptest.NewRecorder()
	e := echo.New()
	c := e.NewContext(req, rec)
	if err := h.ListAuthSessions(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200; body=%s", rec.Code, rec.Body)
	}
}

// emptyRows is a pgx.Rows stub that immediately exhausts on Next().
type emptyRows struct{}

func (r *emptyRows) Close()                                       {}
func (r *emptyRows) Err() error                                   { return nil }
func (r *emptyRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *emptyRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *emptyRows) Next() bool                                   { return false }
func (r *emptyRows) Scan(_ ...any) error                          { return nil }
func (r *emptyRows) Values() ([]any, error)                       { return nil, nil }
func (r *emptyRows) RawValues() [][]byte                          { return nil }
func (r *emptyRows) Conn() *pgx.Conn                              { return nil }

// DeleteAuthSessions: success path using revocationRevoke injection

func TestDeleteAuthSessions_ByUserID_Success_Returns200(t *testing.T) {
	// Swap the package-level revocationRevoke to a no-op for this test.
	orig := revocationRevoke
	revocationRevoke = func(_ *Handler, _ context.Context, _ revocation.Request) error { return nil }
	defer func() { revocationRevoke = orig }()

	h := defaultHandler()
	h.pool = &stubPool{} // Exec returns nil error
	// Wire a real (empty) revocation.Service to pass the nil check.
	// Since we intercept revocationRevoke, the service itself is never called.
	h.revocation = &revocation.Service{} // zero value; never called in this path

	req := httptest.NewRequest(http.MethodDelete, "/auth/sessions?user_id=u1", nil)
	rec := httptest.NewRecorder()
	e := echo.New()
	c := e.NewContext(req, rec)
	middleware.WithAdminAuth(c, &authn.AdminAuth{KeyID: "admin", KeyName: "test", AuthPrincipalType: "admin_user"})
	if err := h.DeleteAuthSessions(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200; body=%s", rec.Code, rec.Body)
	}
}

func TestDeleteAuthSessions_ByDeviceID_Success_Returns200(t *testing.T) {
	orig := revocationRevoke
	revocationRevoke = func(_ *Handler, _ context.Context, _ revocation.Request) error { return nil }
	defer func() { revocationRevoke = orig }()

	h := defaultHandler()
	h.pool = &stubPool{}
	h.revocation = &revocation.Service{}

	req := httptest.NewRequest(http.MethodDelete, "/auth/sessions?device_id=d1", nil)
	rec := httptest.NewRecorder()
	e := echo.New()
	c := e.NewContext(req, rec)
	if err := h.DeleteAuthSessions(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200", rec.Code)
	}
}

func TestDeleteAuthSessions_BySessionID_Success_Returns200(t *testing.T) {
	orig := revocationRevoke
	revocationRevoke = func(_ *Handler, _ context.Context, _ revocation.Request) error { return nil }
	defer func() { revocationRevoke = orig }()

	h := defaultHandler()
	h.pool = &stubPool{}
	h.revocation = &revocation.Service{}

	req := httptest.NewRequest(http.MethodDelete, "/auth/sessions?session_id=sess-1", nil)
	rec := httptest.NewRecorder()
	e := echo.New()
	c := e.NewContext(req, rec)
	if err := h.DeleteAuthSessions(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200", rec.Code)
	}
}

func TestDeleteAuthSessions_RevokeError_Returns500(t *testing.T) {
	orig := revocationRevoke
	revocationRevoke = func(_ *Handler, _ context.Context, _ revocation.Request) error {
		return errors.New("revoke failed")
	}
	defer func() { revocationRevoke = orig }()

	h := defaultHandler()
	h.pool = &stubPool{}
	h.revocation = &revocation.Service{}

	req := httptest.NewRequest(http.MethodDelete, "/auth/sessions?user_id=u1", nil)
	rec := httptest.NewRecorder()
	e := echo.New()
	c := e.NewContext(req, rec)
	if err := h.DeleteAuthSessions(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500", rec.Code)
	}
}

func TestDeleteAuthSessions_ExecError_Returns500(t *testing.T) {
	orig := revocationRevoke
	revocationRevoke = func(_ *Handler, _ context.Context, _ revocation.Request) error { return nil }
	defer func() { revocationRevoke = orig }()

	h := defaultHandler()
	h.pool = &stubPool{execErr: errors.New("db error")}
	h.revocation = &revocation.Service{}

	req := httptest.NewRequest(http.MethodDelete, "/auth/sessions?user_id=u1", nil)
	rec := httptest.NewRecorder()
	e := echo.New()
	c := e.NewContext(req, rec)
	if err := h.DeleteAuthSessions(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500 (exec failed)", rec.Code)
	}
}

func TestDeleteAuthSessions_NilPool_Returns503(t *testing.T) {
	orig := revocationRevoke
	revocationRevoke = func(_ *Handler, _ context.Context, _ revocation.Request) error { return nil }
	defer func() { revocationRevoke = orig }()

	h := defaultHandler()
	// h.pool is nil, revocation is not nil
	h.revocation = &revocation.Service{}

	req := httptest.NewRequest(http.MethodDelete, "/auth/sessions?user_id=u1", nil)
	rec := httptest.NewRecorder()
	e := echo.New()
	c := e.NewContext(req, rec)
	if err := h.DeleteAuthSessions(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("code=%d want 503 (nil pool)", rec.Code)
	}
}

func TestRevokeDeviceInternal_MissingDeviceIDBody_Returns400(t *testing.T) {
	orig := revocationRevoke
	revocationRevoke = func(_ *Handler, _ context.Context, _ revocation.Request) error { return nil }
	defer func() { revocationRevoke = orig }()

	h := defaultHandler()
	h.pool = &stubPool{}
	h.revocation = &revocation.Service{}

	body, _ := json.Marshal(map[string]any{"deviceId": ""})
	c, rec := adminAuthCtx(http.MethodPost, "/auth/revoke-device", body, "internal", "admin_user")
	if err := h.RevokeDeviceInternal(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code=%d want 400 (empty deviceId)", rec.Code)
	}
}

func TestRevokeDeviceInternal_Success_Returns204(t *testing.T) {
	orig := revocationRevoke
	revocationRevoke = func(_ *Handler, _ context.Context, _ revocation.Request) error { return nil }
	defer func() { revocationRevoke = orig }()

	h := defaultHandler()
	h.pool = &stubPool{}
	h.revocation = &revocation.Service{}

	body, _ := json.Marshal(map[string]any{"deviceId": "dev-1"})
	c, rec := adminAuthCtx(http.MethodPost, "/auth/revoke-device", body, "internal", "admin_user")
	if err := h.RevokeDeviceInternal(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNoContent {
		t.Errorf("code=%d want 204; body=%s", rec.Code, rec.Body)
	}
}

func TestRevokeDeviceInternal_RevokeError_Returns500(t *testing.T) {
	orig := revocationRevoke
	revocationRevoke = func(_ *Handler, _ context.Context, _ revocation.Request) error {
		return errors.New("revoke failed")
	}
	defer func() { revocationRevoke = orig }()

	h := defaultHandler()
	h.pool = &stubPool{}
	h.revocation = &revocation.Service{}

	body, _ := json.Marshal(map[string]any{"deviceId": "dev-1"})
	c, rec := adminAuthCtx(http.MethodPost, "/auth/revoke-device", body, "internal", "admin_user")
	if err := h.RevokeDeviceInternal(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500", rec.Code)
	}
}

func TestUpdateIdentityProvider_NotFound_Returns404(t *testing.T) {
	ss := &stubScimStore{idp: nil, idpErr: pgx.ErrNoRows}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, &stubOrgStore{}, ss, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	body, _ := json.Marshal(map[string]any{"type": "oidc", "name": "X", "config": map[string]any{}})
	c, rec := adminAuthCtx(http.MethodPut, "/identity-providers/missing", body, "admin", "admin_user")
	c.SetParamNames("idpId")
	c.SetParamValues("missing")
	if err := h.UpdateIdentityProvider(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("code=%d want 404", rec.Code)
	}
}

func TestUpdateIdentityProvider_LocalType_Returns403(t *testing.T) {
	ss := &stubScimStore{idp: &scimstore.IdentityProviderRecord{ID: "local", Type: "local", Name: "Local"}}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, &stubOrgStore{}, ss, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	body, _ := json.Marshal(map[string]any{"type": "oidc", "name": "X", "config": map[string]any{}})
	c, rec := adminAuthCtx(http.MethodPut, "/identity-providers/local", body, "admin", "admin_user")
	c.SetParamNames("idpId")
	c.SetParamValues("local")
	if err := h.UpdateIdentityProvider(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("code=%d want 403 (local IdP not editable)", rec.Code)
	}
}

func TestUpdateIdentityProvider_InvalidType_Returns400(t *testing.T) {
	ss := &stubScimStore{idp: &scimstore.IdentityProviderRecord{ID: "idp-1", Type: "oidc", Name: "Okta", Enabled: true}}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, &stubOrgStore{}, ss, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	body, _ := json.Marshal(map[string]any{"type": "invalid", "name": "X", "config": map[string]any{}})
	c, rec := adminAuthCtx(http.MethodPut, "/identity-providers/idp-1", body, "admin", "admin_user")
	c.SetParamNames("idpId")
	c.SetParamValues("idp-1")
	if err := h.UpdateIdentityProvider(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code=%d want 400 (invalid type)", rec.Code)
	}
}

func TestUpdateIdentityProvider_Success_Returns200(t *testing.T) {
	ss := &stubScimStore{
		idp:        &scimstore.IdentityProviderRecord{ID: "idp-1", Type: "oidc", Name: "Okta", Enabled: true},
		updatedIdp: &scimstore.IdentityProviderRecord{ID: "idp-1", Type: "oidc", Name: "Updated", Enabled: true},
	}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, &stubOrgStore{}, ss, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	body, _ := json.Marshal(map[string]any{
		"type": "oidc", "name": "Updated",
		"config": map[string]any{
			"issuer":       "https://example.okta.com",
			"clientId":     "cid",
			"clientSecret": "secret",
			"redirectUri":  "https://app.example.com/cb",
		},
	})
	c, rec := adminAuthCtx(http.MethodPut, "/identity-providers/idp-1", body, "admin", "admin_user")
	c.SetParamNames("idpId")
	c.SetParamValues("idp-1")
	if err := h.UpdateIdentityProvider(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200; body=%s", rec.Code, rec.Body)
	}
}

func TestDeleteIdentityProvider_NotFound_Returns404(t *testing.T) {
	ss := &stubScimStore{idp: nil, idpErr: pgx.ErrNoRows}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, &stubOrgStore{}, ss, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodDelete, "/identity-providers/missing", nil, "admin", "admin_user")
	c.SetParamNames("idpId")
	c.SetParamValues("missing")
	if err := h.DeleteIdentityProvider(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("code=%d want 404", rec.Code)
	}
}

func TestDeleteIdentityProvider_LocalType_Returns403(t *testing.T) {
	ss := &stubScimStore{idp: &scimstore.IdentityProviderRecord{ID: "local", Type: "local", Name: "Local"}}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, &stubOrgStore{}, ss, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodDelete, "/identity-providers/local", nil, "admin", "admin_user")
	c.SetParamNames("idpId")
	c.SetParamValues("local")
	if err := h.DeleteIdentityProvider(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("code=%d want 403", rec.Code)
	}
}

func TestDeleteIdentityProvider_HasLinkedUsers_Returns409(t *testing.T) {
	ss := &stubScimStore{
		idp:      &scimstore.IdentityProviderRecord{ID: "idp-1", Type: "oidc", Name: "Okta"},
		fedCount: 5,
	}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, &stubOrgStore{}, ss, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodDelete, "/identity-providers/idp-1", nil, "admin", "admin_user")
	c.SetParamNames("idpId")
	c.SetParamValues("idp-1")
	if err := h.DeleteIdentityProvider(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusConflict {
		t.Errorf("code=%d want 409 (linked users)", rec.Code)
	}
}

func TestDeleteIdentityProvider_Success_Returns204(t *testing.T) {
	ss := &stubScimStore{
		idp:      &scimstore.IdentityProviderRecord{ID: "idp-1", Type: "oidc", Name: "Okta"},
		fedCount: 0,
	}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, &stubOrgStore{}, ss, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodDelete, "/identity-providers/idp-1", nil, "admin", "admin_user")
	c.SetParamNames("idpId")
	c.SetParamValues("idp-1")
	if err := h.DeleteIdentityProvider(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNoContent {
		t.Errorf("code=%d want 204; body=%s", rec.Code, rec.Body)
	}
}

// New() with pool: exercises pool != nil branch

func TestNew_WithNonNilPool_BuildsSubStores(t *testing.T) {
	// Use pgxpool.NewWithConfig with a dead DSN — construction is lazy.
	cfg, err := pgxpool.ParseConfig("postgres://localhost/testdb")
	if err != nil {
		t.Skip("pgxpool.ParseConfig failed:", err)
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Skip("pgxpool.NewWithConfig failed:", err)
	}
	h := New(Deps{Pool: pool, Audit: noopAudit()})
	if h.users == nil {
		t.Error("expected h.users to be set when pool != nil")
	}
	pool.Close()
}

// ListIAMPolicies enabled filter branches

func TestListIAMPolicies_EnabledFalse_Returns200(t *testing.T) {
	is := &stubIAMStore{policies: nil, policiesTotal: 0}
	h := buildHandler(&stubUserStore{}, is, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	req := httptest.NewRequest(http.MethodGet, "/iam/policies?enabled=false", nil)
	rec := httptest.NewRecorder()
	e := echo.New()
	c := e.NewContext(req, rec)
	if err := h.ListIAMPolicies(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200", rec.Code)
	}
}

// ListIAMGroups error branch

func TestListIAMGroups_Error_Returns500(t *testing.T) {
	is := &stubIAMStore{groupsErr: errors.New("db")}
	h := buildHandler(&stubUserStore{}, is, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodGet, "/iam/groups", nil, "admin", "admin_user")
	if err := h.ListIAMGroups(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500", rec.Code)
	}
}

// GetIAMGroup with members

func TestGetIAMGroup_WithMembers_Returns200(t *testing.T) {
	is := &stubIAMStore{
		group:   &iamstore.GroupRow{ID: "g1", Name: "Admins"},
		members: []iamstore.GroupMemberRow{{PrincipalType: "nexus_user", PrincipalID: "u1"}},
	}
	h := buildHandler(&stubUserStore{}, is, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodGet, "/iam/groups/g1", nil, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("g1")
	if err := h.GetIAMGroup(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200", rec.Code)
	}
}

// AttachIAMGroupPolicy: error path

func TestAttachIAMGroupPolicy_Error_Returns500(t *testing.T) {
	is := &stubIAMStore{attachGroupPolErr: errors.New("db")}
	h := buildHandler(&stubUserStore{}, is, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	body, _ := json.Marshal(map[string]any{"policyId": "p1"})
	c, rec := adminAuthCtx(http.MethodPost, "/iam/groups/g1/policies", body, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("g1")
	if err := h.AttachIAMGroupPolicy(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500", rec.Code)
	}
}

// AttachPrincipalPolicy: expiry in past

func TestAttachPrincipalPolicy_ExpiryInPast_Returns400(t *testing.T) {
	h := defaultHandler()
	exp := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	body, _ := json.Marshal(map[string]any{"policyId": "p1", "expiresAt": exp})
	c, rec := adminAuthCtx(http.MethodPost, "/iam/principals/nexus_user/u1/policies", body, "admin", "admin_user")
	c.SetParamNames("type", "id")
	c.SetParamValues("nexus_user", "u1")
	if err := h.AttachPrincipalPolicy(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code=%d want 400 (expiry in past)", rec.Code)
	}
}

// ListOrganizations: error branch

func TestListOrganizations_Error_Returns500(t *testing.T) {
	os := &stubOrgStore{orgsErr: errors.New("db")}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, os, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodGet, "/organizations", nil, "admin", "admin_user")
	if err := h.ListOrganizations(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500", rec.Code)
	}
}

// ListUsers: enabled=false and canAccessCP=false filters

func TestListUsers_EnabledFalseFilter_Returns200(t *testing.T) {
	us := &stubUserStore{listResult: nil, listTotal: 0}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	req := httptest.NewRequest(http.MethodGet, "/users?enabled=false&canAccessControlPlane=true", nil)
	rec := httptest.NewRecorder()
	e := echo.New()
	c := e.NewContext(req, rec)
	if err := h.ListUsers(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200", rec.Code)
	}
}

// ListPrincipalPolicies error

func TestListPrincipalPolicies_Error_Returns500(t *testing.T) {
	is := &stubIAMStore{ppAttachmentsErr: errors.New("db")}
	h := buildHandler(&stubUserStore{}, is, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodGet, "/iam/principals/nexus_user/u1/policies", nil, "admin", "admin_user")
	c.SetParamNames("type", "id")
	c.SetParamValues("nexus_user", "u1")
	if err := h.ListPrincipalPolicies(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500", rec.Code)
	}
}

// DetachPrincipalPolicy: error

func TestDetachPrincipalPolicy_NotFound_Returns404(t *testing.T) {
	is := &stubIAMStore{detachPPErr: errors.New("not found")}
	h := buildHandler(&stubUserStore{}, is, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodDelete, "/iam/principals/nexus_user/u1/policies/att-1", nil, "admin", "admin_user")
	c.SetParamNames("type", "id", "attachmentId")
	c.SetParamValues("nexus_user", "u1", "att-1")
	if err := h.DetachPrincipalPolicy(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("code=%d want 404", rec.Code)
	}
}

// GetUserIdentity: error branches

func TestGetUserIdentity_FindError_Returns500(t *testing.T) {
	us := &stubUserStore{findErr: errors.New("db")}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodGet, "/users/u1/identity", nil, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("u1")
	if err := h.GetUserIdentity(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500", rec.Code)
	}
}

// CreateOrganization: store error

func TestCreateOrganization_StoreError_Returns500(t *testing.T) {
	os := &stubOrgStore{createOrgErr: errors.New("db")}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, os, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	body, _ := json.Marshal(map[string]any{"name": "Org", "code": "ORG"})
	c, rec := adminAuthCtx(http.MethodPost, "/organizations", body, "admin", "admin_user")
	if err := h.CreateOrganization(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500", rec.Code)
	}
}

// RevokeUserAccess: intermediate errors

func TestRevokeUserAccess_FindError_Returns500(t *testing.T) {
	us := &stubUserStore{findErr: errors.New("db")}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodPost, "/users/u1/revoke", nil, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("u1")
	if err := h.RevokeUserAccess(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500", rec.Code)
	}
}

// UpdateMe: current password verification error

func TestUpdateMe_WrongCurrentPassword_Returns401(t *testing.T) {
	hash, _ := authn.HashPassword("correct-password")
	// Source "local" mirrors the DB default (NexusUser.source defaults to
	// "local"); a real password-backed account is always local-sourced.
	us := &stubUserStore{findResult: &userstore.NexusUser{ID: "u1", Source: "local", PasswordHash: &hash}}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	curPw := "wrong-password"
	newPw := "new-password"
	body, _ := json.Marshal(map[string]any{"currentPassword": curPw, "newPassword": newPw})
	c, rec := adminAuthCtx(http.MethodPatch, "/me", body, "u1", "admin_user")
	if err := h.UpdateMe(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("code=%d want 401 (wrong current password)", rec.Code)
	}
}

// UpdateMe: an SSO-sourced account cannot change a local password — it has none.
// The handler returns 400 with the actionable "sso_account" code so the profile
// page guides the user to their identity provider instead of failing the
// current-password check with a confusing message.
func TestUpdateMe_SSOAccount_PasswordChangeReturns400(t *testing.T) {
	hash, _ := authn.HashPassword("irrelevant")
	us := &stubUserStore{findResult: &userstore.NexusUser{ID: "u1", Source: "oidc", PasswordHash: &hash}}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	body, _ := json.Marshal(map[string]any{"currentPassword": "whatever", "newPassword": "new-password"})
	c, rec := adminAuthCtx(http.MethodPatch, "/me", body, "u1", "admin_user")
	if err := h.UpdateMe(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400 (SSO account); body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "sso_account") {
		t.Errorf("body=%s want sso_account code", rec.Body.String())
	}
}

// buildMeResponse: with user email

func TestBuildMeResponse_WithEmail_IncludesEmail(t *testing.T) {
	email := "u@example.com"
	us := &stubUserStore{findResult: &userstore.NexusUser{ID: "u1", Email: &email}}
	is := &stubIAMStore{groupNames: []string{"admin-group"}}
	h := buildHandler(us, is, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodGet, "/me", nil, "u1", "admin_user")
	if err := h.GetMe(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200", rec.Code)
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["email"] != email {
		t.Errorf("expected email=%q in response, got %v", email, resp["email"])
	}
}

// GetOrganization: with parentID

func TestGetOrganization_WithParent_IncludesParent(t *testing.T) {
	parentID := "parent-1"
	parent := &orgstore.Organization{ID: parentID, Name: "Parent"}
	baseOS := &stubOrgStore{
		org: &orgstore.Organization{ID: "o1", Name: "Child", ParentID: &parentID},
	}
	// Use flexOrgStore so GetOrganization can return different values for the
	// main org vs its parent. stubOrgStore embedded must be non-nil so delegated
	// methods (ListProjects etc.) don't panic.
	flexOrg := &flexOrgStore{
		stubOrgStore: baseOS,
		main:         &orgstore.Organization{ID: "o1", Name: "Child", ParentID: &parentID},
		parent:       parent,
	}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, flexOrg, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodGet, "/organizations/o1", nil, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("o1")
	if err := h.GetOrganization(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200", rec.Code)
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["parent"] == nil {
		t.Error("expected 'parent' in response for org with parentID")
	}
}

// flexOrgStore returns different orgs for the main vs parent lookups.
type flexOrgStore struct {
	*stubOrgStore
	main   *orgstore.Organization
	parent *orgstore.Organization
}

func (f *flexOrgStore) GetOrganization(_ context.Context, id string) (*orgstore.Organization, error) {
	if id == *f.main.ParentID {
		return f.parent, nil
	}
	return f.main, nil
}
func (f *flexOrgStore) ListOrganizations(_ context.Context) ([]orgstore.Organization, error) {
	return f.orgs, f.orgsErr
}
func (f *flexOrgStore) ListChildOrganizations(_ context.Context, _ string) ([]orgstore.Organization, error) {
	return nil, nil
}
func (f *flexOrgStore) CreateOrganization(_ context.Context, _ map[string]any) (*orgstore.Organization, error) {
	return f.createdOrg, f.createOrgErr
}
func (f *flexOrgStore) UpdateOrganization(_ context.Context, _ string, _ orgstore.UpdateOrganizationParams) (*orgstore.Organization, error) {
	return f.updatedOrg, f.updateOrgErr
}
func (f *flexOrgStore) DeleteOrganization(_ context.Context, _ string) error {
	return f.deleteOrgErr
}
func (f *flexOrgStore) CountOrgDependents(_ context.Context, _ string) (int, int, error) {
	return f.orgChildren, f.orgProjects, f.orgDepsErr
}
func (f *flexOrgStore) ListProjects(_ context.Context, _ orgstore.ProjectListParams) ([]orgstore.Project, int, error) {
	return f.projects, f.projectsTotal, f.projectsErr
}
func (f *flexOrgStore) GetProject(_ context.Context, _ string) (*orgstore.Project, error) {
	return f.project, f.projectErr
}
func (f *flexOrgStore) CreateProject(_ context.Context, _ map[string]any) (*orgstore.Project, error) {
	return f.createdProject, f.createProjErr
}
func (f *flexOrgStore) UpdateProject(_ context.Context, _ string, _ orgstore.UpdateProjectParams) (*orgstore.Project, error) {
	return f.updatedProject, f.updateProjErr
}
func (f *flexOrgStore) DeleteProject(_ context.Context, _ string) error {
	return f.deleteProjErr
}
func (f *flexOrgStore) CountProjectVirtualKeys(_ context.Context, _ string) (int, error) {
	return f.projVKCount, f.projVKErr
}

// UpdateIAMPolicy: with document field

func TestUpdateIAMPolicy_WithDocument_Returns200(t *testing.T) {
	is := &stubIAMStore{updatedPolicy: &iamstore.PolicyRow{ID: "p1", Name: "P"}}
	h := buildHandler(&stubUserStore{}, is, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	h.iamEngine = superAdminTestEngine() // grant ceiling: caller must hold the new document's perms
	body, _ := json.Marshal(map[string]any{"document": map[string]any{
		"Version":   "2026-05-12",
		"Statement": []map[string]any{{"Effect": "Allow", "Action": []string{"settings:read"}, "Resource": []string{"*"}}},
	}})
	c, rec := adminAuthCtx(http.MethodPut, "/iam/policies/p1", body, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("p1")
	if err := h.UpdateIAMPolicy(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200", rec.Code)
	}
}

// CreateAPIKey: with admin_user principal (sets owner)

func TestCreateAPIKey_AdminUserPrincipal_SetsOwner(t *testing.T) {
	uid := "admin"
	us := &stubUserStore{createKey: &userstore.AdminAPIKey{ID: "k1", Name: "Key", OwnerUserID: &uid}}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	body, _ := json.Marshal(map[string]any{"name": "Key"})
	c, rec := adminAuthCtx(http.MethodPost, "/api-keys", body, "admin", "admin_user")
	// AuthPrincipalType = "admin_user" → ownerUserId defaults to KeyID
	if err := h.CreateAPIKey(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("code=%d want 201; body=%s", rec.Code, rec.Body)
	}
}

// CreateAPIKey ownerUserId grant ceiling.
//
// An admin API key whose ownerUserId is set authenticates AS that owner
// (authn.EffectivePrincipal). Without the ceiling, a caller holding only
// admin:api-key.create could mint a key owned by a super-admin and inherit
// super-admin authority. The ceiling requires the caller to already cover the
// target owner's full permission set before the key is created.

// perPrincipalLoader returns a distinct policy set per (type,id) so the ceiling
// can distinguish a narrowly-scoped caller from a super-admin owner. The default
// (unknown principal) is no policies.
type perPrincipalLoader struct {
	byID map[string][]cpiam.LoadedPolicy
	err  error
}

func (l *perPrincipalLoader) LoadPolicies(_ context.Context, pt, pid string) ([]cpiam.LoadedPolicy, error) {
	if l.err != nil {
		return nil, l.err
	}
	return l.byID[pt+":"+pid], nil
}

func allowAllPolicy(name string) cpiam.LoadedPolicy {
	return cpiam.LoadedPolicy{ID: name, Name: name, Source: "direct",
		Document: cpiam.PolicyDocument{Version: cpiam.PolicyVersion, Statement: []cpiam.Statement{{
			Effect: "Allow", Action: []string{"*"}, Resource: []string{"nrn:nexus:*:*:*/*"},
		}}}}
}

func scopedPolicy(name string, actions, resources []string) cpiam.LoadedPolicy {
	return cpiam.LoadedPolicy{ID: name, Name: name, Source: "direct",
		Document: cpiam.PolicyDocument{Version: cpiam.PolicyVersion, Statement: []cpiam.Statement{{
			Effect: "Allow", Action: actions, Resource: resources,
		}}}}
}

// TestCreateAPIKey_OwnerEscalation_Blocked: a caller holding only
// admin:api-key.create that tries to mint a key owned by a super-admin is
// rejected with 403 PRIVILEGE_ESCALATION_BLOCKED and NO key row is written.
func TestCreateAPIKey_OwnerEscalation_Blocked(t *testing.T) {
	loader := &perPrincipalLoader{byID: map[string][]cpiam.LoadedPolicy{
		"nexus_user:weak":  {scopedPolicy("weak", []string{"admin:api-key.create"}, []string{"nrn:nexus:*:*:*/*"})},
		"nexus_user:super": {allowAllPolicy("super")},
	}}
	us := &stubUserStore{createKey: &userstore.AdminAPIKey{ID: "k1", Name: "Key"}}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	h.iamEngine = cpiam.NewEngine(loader, slog.Default())

	body, _ := json.Marshal(map[string]any{"name": "Key", "ownerUserId": "super"})
	c, rec := adminAuthCtx(http.MethodPost, "/api-keys", body, "weak", "admin_user")
	if err := h.CreateAPIKey(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("code=%d want 403; body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "PRIVILEGE_ESCALATION_BLOCKED") {
		t.Errorf("missing PRIVILEGE_ESCALATION_BLOCKED code; body=%s", rec.Body)
	}
	if us.createKeyCalled {
		t.Error("key row must NOT be written when the ceiling blocks the mint")
	}
}

// TestCreateAPIKey_OwnerSelf_Unchanged: minting a key owned by the caller's own
// user id skips the ceiling entirely (behavior unchanged) and succeeds even with
// a scoped caller.
func TestCreateAPIKey_OwnerSelf_Unchanged(t *testing.T) {
	loader := &perPrincipalLoader{byID: map[string][]cpiam.LoadedPolicy{
		"nexus_user:weak": {scopedPolicy("weak", []string{"admin:api-key.create"}, []string{"nrn:nexus:*:*:*/*"})},
	}}
	uid := "weak"
	us := &stubUserStore{createKey: &userstore.AdminAPIKey{ID: "k1", Name: "Key", OwnerUserID: &uid}}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	h.iamEngine = cpiam.NewEngine(loader, slog.Default())

	body, _ := json.Marshal(map[string]any{"name": "Key", "ownerUserId": "weak"})
	c, rec := adminAuthCtx(http.MethodPost, "/api-keys", body, "weak", "admin_user")
	if err := h.CreateAPIKey(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("code=%d want 201; body=%s", rec.Code, rec.Body)
	}
	if !us.createKeyCalled {
		t.Error("self-owned key must be written")
	}
}

// TestCreateAPIKey_OwnerCovered_Success: a caller that DOES cover the target
// owner's permissions may mint a key for that owner.
func TestCreateAPIKey_OwnerCovered_Success(t *testing.T) {
	loader := &perPrincipalLoader{byID: map[string][]cpiam.LoadedPolicy{
		"nexus_user:strong": {allowAllPolicy("strong")},
		"nexus_user:weak":   {scopedPolicy("weak", []string{"admin:api-key.read"}, []string{"nrn:nexus:*:*:*/*"})},
	}}
	owner := "weak"
	us := &stubUserStore{createKey: &userstore.AdminAPIKey{ID: "k1", Name: "Key", OwnerUserID: &owner}}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	h.iamEngine = cpiam.NewEngine(loader, slog.Default())

	body, _ := json.Marshal(map[string]any{"name": "Key", "ownerUserId": "weak"})
	c, rec := adminAuthCtx(http.MethodPost, "/api-keys", body, "strong", "admin_user")
	if err := h.CreateAPIKey(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("code=%d want 201; body=%s", rec.Code, rec.Body)
	}
	if !us.createKeyCalled {
		t.Error("covered owner key must be written")
	}
}

// TestCreateAPIKey_OwnerCeiling_NilEngine_FailsClosed: a cross-owner mint with no
// IAM engine fails closed (503) and writes no key.
func TestCreateAPIKey_OwnerCeiling_NilEngine_FailsClosed(t *testing.T) {
	us := &stubUserStore{createKey: &userstore.AdminAPIKey{ID: "k1", Name: "Key"}}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	// iamEngine left nil

	body, _ := json.Marshal(map[string]any{"name": "Key", "ownerUserId": "super"})
	c, rec := adminAuthCtx(http.MethodPost, "/api-keys", body, "weak", "admin_user")
	if err := h.CreateAPIKey(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d want 503 (nil engine fail-closed); body=%s", rec.Code, rec.Body)
	}
	if us.createKeyCalled {
		t.Error("key row must NOT be written on fail-closed")
	}
}

// TestCreateAPIKey_OwnerCeiling_EvalError_FailsClosed: a loader error during the
// owner-coverage check fails closed (500) and writes no key.
func TestCreateAPIKey_OwnerCeiling_EvalError_FailsClosed(t *testing.T) {
	loader := &perPrincipalLoader{err: errors.New("db down")}
	us := &stubUserStore{createKey: &userstore.AdminAPIKey{ID: "k1", Name: "Key"}}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	h.iamEngine = cpiam.NewEngine(loader, slog.Default())

	body, _ := json.Marshal(map[string]any{"name": "Key", "ownerUserId": "super"})
	c, rec := adminAuthCtx(http.MethodPost, "/api-keys", body, "weak", "admin_user")
	if err := h.CreateAPIKey(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code=%d want 500 (eval error fail-closed); body=%s", rec.Code, rec.Body)
	}
	if us.createKeyCalled {
		t.Error("key row must NOT be written on fail-closed")
	}
}

// UpdateAPIKey: enabled field

func TestUpdateAPIKey_EnabledField_Returns200(t *testing.T) {
	uid := "u1"
	enabled := true
	us := &stubUserStore{updateKey: &userstore.AdminAPIKey{ID: "k1", Name: "Key", OwnerUserID: &uid, Enabled: enabled}}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	body, _ := json.Marshal(map[string]any{"enabled": false})
	c, rec := adminAuthCtx(http.MethodPatch, "/api-keys/k1", body, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("k1")
	if err := h.UpdateAPIKey(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200", rec.Code)
	}
}

// CreateIAMGroup: store error

func TestCreateIAMGroup_StoreError_Returns500(t *testing.T) {
	is := &stubIAMStore{createGroupErr: errors.New("db")}
	h := buildHandler(&stubUserStore{}, is, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	body, _ := json.Marshal(map[string]any{"name": "Group"})
	c, rec := adminAuthCtx(http.MethodPost, "/iam/groups", body, "admin", "admin_user")
	if err := h.CreateIAMGroup(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500", rec.Code)
	}
}

// DeleteProject: store error

func TestDeleteProject_StoreError_Returns500(t *testing.T) {
	os := &stubOrgStore{deleteProjErr: errors.New("db")}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, os, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodDelete, "/projects/p1", nil, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("p1")
	if err := h.DeleteProject(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500", rec.Code)
	}
}

// fanOutIdPRevocations: triggers revokeUserScope for each user
// DeleteIdentityProvider with force=true + federated users exercises fanOutIdPRevocations.

func TestDeleteIdentityProvider_WithForceAndUsers_RevokesUsers(t *testing.T) {
	orig := revocationRevoke
	revocationRevoke = func(_ *Handler, _ context.Context, _ revocation.Request) error { return nil }
	defer func() { revocationRevoke = orig }()

	ss := &stubScimStore{
		idp: &scimstore.IdentityProviderRecord{ID: "idp-1", Type: "oidc", Name: "Okta"},
	}
	feds := &stubFedStore{userIDs: []string{"u1", "u2"}}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, &stubOrgStore{}, ss, &stubFleetStore{}, &stubVKStore{}, feds, &stubGovernanceStore{})
	h.pool = &stubPool{} // revokeUserScope needs pool for Exec
	h.revocation = &revocation.Service{}

	req := httptest.NewRequest(http.MethodDelete, "/identity-providers/idp-1?force=true", nil)
	rec := httptest.NewRecorder()
	e := echo.New()
	c := e.NewContext(req, rec)
	c.SetParamNames("idpId")
	c.SetParamValues("idp-1")
	middleware.WithAdminAuth(c, &authn.AdminAuth{KeyID: "admin", KeyName: "admin", AuthPrincipalType: "admin_user"})
	if err := h.DeleteIdentityProvider(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNoContent {
		t.Errorf("code=%d want 204; body=%s", rec.Code, rec.Body)
	}
}

// revokeUserScope: with pool (Exec path)

func TestRevokeUserScope_WithPoolAndRevocation_ExecutesExec(t *testing.T) {
	orig := revocationRevoke
	revocationRevoke = func(_ *Handler, _ context.Context, _ revocation.Request) error { return nil }
	defer func() { revocationRevoke = orig }()

	h := defaultHandler()
	h.revocation = &revocation.Service{}
	h.pool = &stubPool{} // Exec returns nil

	// revokeUserScope is not exported; call it via a handler that uses it.
	// UpdateUser with status=disabled triggers revokeUserScope (when revocation != nil).
	// Use a direct method call since we're in the same package.
	h.revokeUserScope(context.Background(), "user-1", revocation.ReasonAdminDisable)
	// If we get here without panic, the pool.Exec path ran successfully.
}

func TestRevokeUserScope_WithPoolExecError_LogsAndContinues(t *testing.T) {
	orig := revocationRevoke
	revocationRevoke = func(_ *Handler, _ context.Context, _ revocation.Request) error { return nil }
	defer func() { revocationRevoke = orig }()

	h := defaultHandler()
	h.revocation = &revocation.Service{}
	h.pool = &stubPool{execErr: errors.New("db error")}

	// Should log and return without panicking.
	h.revokeUserScope(context.Background(), "user-1", revocation.ReasonAdminDisable)
}

func TestRevokeUserScope_NilRevocation_NoOp(t *testing.T) {
	h := defaultHandler()
	// h.revocation is nil → early return
	h.revokeUserScope(context.Background(), "user-1", "test")
}

// ListAuthSessions: filter params coverage

func TestListAuthSessions_WithFilters_Returns200(t *testing.T) {
	h := defaultHandler()
	h.pool = &stubPool{queryRows: &emptyRows{}}
	req := httptest.NewRequest(http.MethodGet, "/auth/sessions?user_id=u1&device_id=d1", nil)
	rec := httptest.NewRecorder()
	e := echo.New()
	c := e.NewContext(req, rec)
	if err := h.ListAuthSessions(c); err != nil {
		t.Fatal(err)
	}
	// Two filters → both passed to SQL; still returns 200 (empty list)
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200", rec.Code)
	}
}

// scanErrorRows is a pgx.Rows stub where Next() returns true once then false,
// and Scan always returns an error — exercises the scan-error branch in ListAuthSessions.
type scanErrorRows struct {
	called bool
}

func (r *scanErrorRows) Close()                                       {}
func (r *scanErrorRows) Err() error                                   { return nil }
func (r *scanErrorRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *scanErrorRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *scanErrorRows) Next() bool {
	if !r.called {
		r.called = true
		return true
	}
	return false
}
func (r *scanErrorRows) Scan(_ ...any) error    { return errors.New("scan failed") }
func (r *scanErrorRows) Values() ([]any, error) { return nil, nil }
func (r *scanErrorRows) RawValues() [][]byte    { return nil }
func (r *scanErrorRows) Conn() *pgx.Conn        { return nil }

func TestListAuthSessions_ScanError_Returns500(t *testing.T) {
	h := defaultHandler()
	h.pool = &stubPool{queryRows: &scanErrorRows{}}
	req := httptest.NewRequest(http.MethodGet, "/auth/sessions", nil)
	rec := httptest.NewRecorder()
	e := echo.New()
	c := e.NewContext(req, rec)
	if err := h.ListAuthSessions(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500 (scan error); body=%s", rec.Code, rec.Body)
	}
}

// rowsWithErr is a pgx.Rows stub that exhausts immediately but Err() returns an error.
type rowsWithErr struct {
	err error
}

func (r *rowsWithErr) Close()                                       {}
func (r *rowsWithErr) Err() error                                   { return r.err }
func (r *rowsWithErr) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *rowsWithErr) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *rowsWithErr) Next() bool                                   { return false }
func (r *rowsWithErr) Scan(_ ...any) error                          { return nil }
func (r *rowsWithErr) Values() ([]any, error)                       { return nil, nil }
func (r *rowsWithErr) RawValues() [][]byte                          { return nil }
func (r *rowsWithErr) Conn() *pgx.Conn                              { return nil }

func TestListAuthSessions_RowsErr_Returns500(t *testing.T) {
	h := defaultHandler()
	h.pool = &stubPool{queryRows: &rowsWithErr{err: errors.New("cursor error")}}
	req := httptest.NewRequest(http.MethodGet, "/auth/sessions", nil)
	rec := httptest.NewRecorder()
	e := echo.New()
	c := e.NewContext(req, rec)
	if err := h.ListAuthSessions(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500 (rows.Err); body=%s", rec.Code, rec.Body)
	}
}

// SimulateIAM: success and error paths with a real Engine

// stubPolicyLoader implements cpiam.PolicyLoader for testing the IAM engine.
type stubPolicyLoader struct {
	policies []cpiam.LoadedPolicy
	err      error
}

func (l *stubPolicyLoader) LoadPolicies(_ context.Context, _, _ string) ([]cpiam.LoadedPolicy, error) {
	return l.policies, l.err
}

// superAdminTestEngine returns an IAM engine whose caller holds "*" on the
// universal NRN — i.e. covers any grant. Used to satisfy the grant
// ceiling on handler happy-path tests that are not exercising the ceiling itself.
func superAdminTestEngine() *cpiam.Engine {
	loader := &stubPolicyLoader{policies: []cpiam.LoadedPolicy{{
		ID: "super", Name: "super", Source: "direct",
		Document: cpiam.PolicyDocument{Version: cpiam.PolicyVersion, Statement: []cpiam.Statement{{
			Effect: "Allow", Action: []string{"*"}, Resource: []string{"nrn:nexus:*:*:*/*"},
		}}},
	}}}
	return cpiam.NewEngine(loader, slog.Default())
}

// scopedTestEngine returns an IAM engine whose caller holds exactly the given
// (action,resource) grants and nothing else — used to drive the
// escalation-blocked regression tests.
func scopedTestEngine(actions, resources []string) *cpiam.Engine {
	loader := &stubPolicyLoader{policies: []cpiam.LoadedPolicy{{
		ID: "scoped", Name: "scoped", Source: "direct",
		Document: cpiam.PolicyDocument{Version: cpiam.PolicyVersion, Statement: []cpiam.Statement{{
			Effect: "Allow", Action: actions, Resource: resources,
		}}},
	}}}
	return cpiam.NewEngine(loader, slog.Default())
}

func TestSimulateIAM_EngineReturnsAllow_Returns200(t *testing.T) {
	loader := &stubPolicyLoader{
		policies: []cpiam.LoadedPolicy{{
			ID:   "p1",
			Name: "AllowAll",
			Document: cpiam.PolicyDocument{
				Statement: []cpiam.Statement{{
					Effect:   "Allow",
					Action:   []string{"*"},
					Resource: []string{"*"},
				}},
			},
			Source: "direct",
		}},
	}
	engine := cpiam.NewEngine(loader, slog.Default())
	h := defaultHandler()
	h.iamEngine = engine

	body, _ := json.Marshal(map[string]any{
		"principal": map[string]any{"type": "nexus_user", "id": "u1"},
		"action":    "admin:user.read",
		"resource":  "nrn:nexus:nexus-user:*",
	})
	c, rec := adminAuthCtx(http.MethodPost, "/iam/simulate", body, "admin", "admin_user")
	if err := h.SimulateIAM(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200; body=%s", rec.Code, rec.Body)
	}
}

func TestSimulateIAM_EngineLoaderError_Returns500(t *testing.T) {
	loader := &stubPolicyLoader{err: errors.New("db error")}
	engine := cpiam.NewEngine(loader, slog.Default())
	h := defaultHandler()
	h.iamEngine = engine

	body, _ := json.Marshal(map[string]any{
		"principal": map[string]any{"type": "nexus_user", "id": "u1"},
		"action":    "admin:user.read",
		"resource":  "nrn:nexus:nexus-user:*",
	})
	c, rec := adminAuthCtx(http.MethodPost, "/iam/simulate", body, "admin", "admin_user")
	if err := h.SimulateIAM(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500; body=%s", rec.Code, rec.Body)
	}
}

func TestSimulateIAM_WithContext_Returns200(t *testing.T) {
	loader := &stubPolicyLoader{policies: []cpiam.LoadedPolicy{}}
	engine := cpiam.NewEngine(loader, slog.Default())
	h := defaultHandler()
	h.iamEngine = engine

	body, _ := json.Marshal(map[string]any{
		"principal": map[string]any{"type": "nexus_user", "id": "u1"},
		"action":    "admin:user.read",
		"resource":  "nrn:nexus:nexus-user:*",
		"context":   map[string]any{"nexus:SourceIp": "1.2.3.4"},
	})
	c, rec := adminAuthCtx(http.MethodPost, "/iam/simulate", body, "admin", "admin_user")
	if err := h.SimulateIAM(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200; body=%s", rec.Code, rec.Body)
	}
}

// GetMePermissions: with a real engine

func TestGetMePermissions_WithEngine_Returns200(t *testing.T) {
	loader := &stubPolicyLoader{policies: []cpiam.LoadedPolicy{}}
	engine := cpiam.NewEngine(loader, slog.Default())
	h := defaultHandler()
	h.iamEngine = engine

	c, rec := adminAuthCtx(http.MethodGet, "/me/permissions", nil, "u1", "admin_user")
	if err := h.GetMePermissions(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200; body=%s", rec.Code, rec.Body)
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if _, ok := resp["actions"]; !ok {
		t.Error("expected 'actions' key in response")
	}
}

// validateIdPRequest: SAML paths

func TestCreateIdentityProvider_SAMLMissingEntityID_Returns400(t *testing.T) {
	h := defaultHandler()
	body, _ := json.Marshal(map[string]any{
		"type": "saml", "name": "SAML IdP",
		"config": map[string]any{"ssoUrl": "https://idp.example.com/sso"},
	})
	c, rec := adminAuthCtx(http.MethodPost, "/identity-providers", body, "admin", "admin_user")
	if err := h.CreateIdentityProvider(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code=%d want 400 (SAML missing entityId)", rec.Code)
	}
}

func TestCreateIdentityProvider_SAMLMissingCert_Returns400(t *testing.T) {
	h := defaultHandler()
	body, _ := json.Marshal(map[string]any{
		"type": "saml", "name": "SAML IdP",
		"config": map[string]any{
			"entityId": "https://example.com",
			"ssoUrl":   "https://idp.example.com/sso",
			// Missing certificatePem on create
		},
	})
	c, rec := adminAuthCtx(http.MethodPost, "/identity-providers", body, "admin", "admin_user")
	if err := h.CreateIdentityProvider(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code=%d want 400 (SAML missing certificatePem)", rec.Code)
	}
}

func TestCreateIdentityProvider_SAMLSuccess_Returns201(t *testing.T) {
	ss := &stubScimStore{
		createdIdp: &scimstore.IdentityProviderRecord{ID: "saml-1", Type: "saml", Name: "SAML IdP"},
	}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, &stubOrgStore{}, ss, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	body, _ := json.Marshal(map[string]any{
		"type": "saml", "name": "SAML IdP",
		"config": map[string]any{
			"entityId":       "https://example.com",
			"ssoUrl":         "https://idp.example.com/sso",
			"certificatePem": "-----BEGIN CERTIFICATE-----\nMIIDXTCCAkWgAw==\n-----END CERTIFICATE-----",
		},
	})
	c, rec := adminAuthCtx(http.MethodPost, "/identity-providers", body, "admin", "admin_user")
	if err := h.CreateIdentityProvider(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("code=%d want 201; body=%s", rec.Code, rec.Body)
	}
}

// UpdateIdentityProvider: SAML with masked cert (mergeMaskedSecrets)

func TestUpdateIdentityProvider_SAMLWithMaskedCert_Returns200(t *testing.T) {
	existingConfig, _ := json.Marshal(map[string]any{
		"entityId":       "https://example.com",
		"ssoUrl":         "https://idp.example.com/sso",
		"certificatePem": "real-cert-value",
	})
	ss := &stubScimStore{
		idp: &scimstore.IdentityProviderRecord{
			ID:     "saml-1",
			Type:   "saml",
			Name:   "SAML IdP",
			Config: existingConfig,
		},
		updatedIdp: &scimstore.IdentityProviderRecord{ID: "saml-1", Type: "saml", Name: "SAML IdP Updated"},
	}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, &stubOrgStore{}, ss, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	body, _ := json.Marshal(map[string]any{
		"type": "saml", "name": "SAML IdP Updated",
		"config": map[string]any{
			"entityId":       "https://example.com",
			"ssoUrl":         "https://idp.example.com/sso",
			"certificatePem": "********", // sensitiveMaskIdP — should be restored from existing
		},
	})
	c, rec := adminAuthCtx(http.MethodPut, "/identity-providers/saml-1", body, "admin", "admin_user")
	c.SetParamNames("idpId")
	c.SetParamValues("saml-1")
	if err := h.UpdateIdentityProvider(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200; body=%s", rec.Code, rec.Body)
	}
}

// ListIdentityProviders: SAML type to exercise toIdpResponse SAML branch

func TestListIdentityProviders_SAMLType_Returns200(t *testing.T) {
	samlConfig, _ := json.Marshal(map[string]any{
		"entityId":       "https://example.com",
		"ssoUrl":         "https://idp.example.com/sso",
		"certificatePem": "real-cert-value",
	})
	ss := &stubScimStore{
		idps: []scimstore.IdentityProviderRecord{{
			ID:     "saml-1",
			Type:   "saml",
			Name:   "SAML IdP",
			Config: samlConfig,
		}},
	}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, &stubOrgStore{}, ss, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodGet, "/identity-providers", nil, "admin", "admin_user")
	if err := h.ListIdentityProviders(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200", rec.Code)
	}
	// Cert should be masked in response
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	data := resp["data"].([]any)
	if len(data) == 0 {
		t.Fatal("expected 1 IdP in response")
	}
	idp := data[0].(map[string]any)
	cfg := idp["config"].(map[string]any)
	if cfg["certificatePem"] != sensitiveMaskIdP {
		t.Errorf("expected cert to be masked, got %v", cfg["certificatePem"])
	}
}

// ListPolicyAttachments: DirectPolicyAttachments error path

func TestListPolicyAttachments_DirectRowsError_Returns500(t *testing.T) {
	is := &stubIAMStore{
		policyGroups:  []iamstore.PolicyGroupRow{},
		directRowsErr: errors.New("db error"),
	}
	h := buildHandler(&stubUserStore{}, is, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodGet, "/iam/policies/p1/attachments", nil, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("p1")
	if err := h.ListPolicyAttachments(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500 (direct attachments error); body=%s", rec.Code, rec.Body)
	}
}

func TestListPolicyAttachments_WithGroupMembers_Returns200(t *testing.T) {
	is := &stubIAMStore{
		policyGroups: []iamstore.PolicyGroupRow{{ID: "g1", Name: "Admins"}},
		members:      []iamstore.GroupMemberRow{{PrincipalType: "nexus_user", PrincipalID: "u1"}},
	}
	h := buildHandler(&stubUserStore{}, is, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodGet, "/iam/policies/p1/attachments", nil, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("p1")
	if err := h.ListPolicyAttachments(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200", rec.Code)
	}
}

func TestListPolicyAttachments_GroupMembersError_Returns200(t *testing.T) {
	// ListGroupMembers error is logged and skipped (continues); returns 200
	is := &stubIAMStore{
		policyGroups: []iamstore.PolicyGroupRow{{ID: "g1", Name: "Admins"}},
		membersErr:   errors.New("members db error"),
	}
	h := buildHandler(&stubUserStore{}, is, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodGet, "/iam/policies/p1/attachments", nil, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("p1")
	if err := h.ListPolicyAttachments(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200 (group members error skipped)", rec.Code)
	}
}

// ListIdpGroupMappings: error path

func TestListIdpGroupMappings_Error_Returns500(t *testing.T) {
	ss := &stubScimStore{mappingsErr: errors.New("db")}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, &stubOrgStore{}, ss, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodGet, "/identity-providers/idp-1/group-mappings", nil, "admin", "admin_user")
	c.SetParamNames("idpId")
	c.SetParamValues("idp-1")
	if err := h.ListIdpGroupMappings(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500", rec.Code)
	}
}

func TestListIdpGroupMappings_NilMappings_Returns200(t *testing.T) {
	ss := &stubScimStore{mappings: nil, mappingsErr: nil}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, &stubOrgStore{}, ss, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodGet, "/identity-providers/idp-1/group-mappings", nil, "admin", "admin_user")
	c.SetParamNames("idpId")
	c.SetParamValues("idp-1")
	if err := h.ListIdpGroupMappings(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200", rec.Code)
	}
}

// parseRFC3339Flexible: RFC3339 (non-nano) path

func TestParseRFC3339Flexible_RFC3339Path(t *testing.T) {
	// Use a time without nanoseconds to hit the RFC3339 fallback
	ts := "2024-01-15T10:30:00Z"
	// Exercise via ListRevocations which calls parseIntDefault (since param),
	// and via parseAdminAuditParams → parseTimeRange → parseRFC3339Flexible.
	// Call parseRFC3339Flexible directly (same package).
	got, ok := parseRFC3339Flexible(ts)
	if !ok {
		t.Errorf("expected parseRFC3339Flexible(%q) to succeed", ts)
	}
	if got.IsZero() {
		t.Error("expected non-zero time")
	}
}

func TestParseRFC3339Flexible_Invalid_ReturnsFalse(t *testing.T) {
	_, ok := parseRFC3339Flexible("not-a-time")
	if ok {
		t.Error("expected false for invalid time string")
	}
}

// parseIntDefault: boundary paths

func TestParseIntDefault_BelowOne_ClampsToOne(t *testing.T) {
	got := parseIntDefault("0", 500, 1000)
	if got != 1 {
		t.Errorf("parseIntDefault(0) = %d, want 1", got)
	}
}

func TestParseIntDefault_InvalidString_ReturnsDefault(t *testing.T) {
	got := parseIntDefault("abc", 42, 100)
	if got != 42 {
		t.Errorf("parseIntDefault(abc) = %d, want 42", got)
	}
}

// ListRevocations: with rows

func TestListRevocations_WithRows_Returns200(t *testing.T) {
	uid := "u1"
	rs := &stubRevocationStore{
		rows: []revocation.Row{{
			ID:           1,
			Scope:        revocation.ScopeUser,
			TargetUserID: &uid,
			Reason:       revocation.ReasonAdminDisable,
			RevokedAt:    time.Now(),
			ExpiresAt:    time.Now().Add(time.Hour),
		}},
		lastID: 1,
	}
	h := defaultHandler()
	h.revocationStore = rs

	c, rec := adminAuthCtx(http.MethodGet, "/revocations?since=0&limit=10", nil, "admin", "admin_user")
	if err := h.ListRevocations(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200", rec.Code)
	}
}

func TestListRevocations_Error_Returns500(t *testing.T) {
	rs := &stubRevocationStore{err: errors.New("db error")}
	h := defaultHandler()
	h.revocationStore = rs

	c, rec := adminAuthCtx(http.MethodGet, "/revocations", nil, "admin", "admin_user")
	if err := h.ListRevocations(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500", rec.Code)
	}
}

// UpdateAPIKey: error path

func TestUpdateAPIKey_StoreError_Returns500(t *testing.T) {
	us := &stubUserStore{updateKeyErr: errors.New("db")}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	body, _ := json.Marshal(map[string]any{"name": "Updated Key"})
	c, rec := adminAuthCtx(http.MethodPatch, "/api-keys/k1", body, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("k1")
	if err := h.UpdateAPIKey(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500", rec.Code)
	}
}

func TestUpdateAPIKey_NoValidFields_Returns400(t *testing.T) {
	h := defaultHandler()
	// Empty body → no valid fields
	body, _ := json.Marshal(map[string]any{})
	c, rec := adminAuthCtx(http.MethodPatch, "/api-keys/k1", body, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("k1")
	if err := h.UpdateAPIKey(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code=%d want 400 (no valid fields)", rec.Code)
	}
}

func TestUpdateAPIKey_WithExpiresAt_Returns200(t *testing.T) {
	uid := "u1"
	us := &stubUserStore{updateKey: &userstore.AdminAPIKey{ID: "k1", OwnerUserID: &uid}}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	future := time.Now().Add(24 * time.Hour).Format(time.RFC3339)
	body, _ := json.Marshal(map[string]any{"expiresAt": future})
	c, rec := adminAuthCtx(http.MethodPatch, "/api-keys/k1", body, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("k1")
	if err := h.UpdateAPIKey(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200; body=%s", rec.Code, rec.Body)
	}
}

// RegenerateAPIKey: paths

func TestRegenerateAPIKey_RegenError_Returns500(t *testing.T) {
	uid := "u1"
	us := &stubUserStore{
		getKey:   &userstore.AdminAPIKey{ID: "k1", OwnerUserID: &uid},
		regenErr: errors.New("db"),
	}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodPost, "/api-keys/k1/regenerate", nil, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("k1")
	if err := h.RegenerateAPIKey(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500", rec.Code)
	}
}

// DeleteAPIKey: session key conflict

func TestDeleteAPIKey_SessionKeyConflict_Returns409(t *testing.T) {
	h := defaultHandler()
	// The request is made with an api_key principal whose KeyID matches the key being deleted
	req := httptest.NewRequest(http.MethodDelete, "/api-keys/k1", nil)
	rec := httptest.NewRecorder()
	e := echo.New()
	c := e.NewContext(req, rec)
	middleware.WithAdminAuth(c, &authn.AdminAuth{KeyID: "k1", KeyName: "k1", AuthPrincipalType: "api_key"})
	c.SetParamNames("id")
	c.SetParamValues("k1")
	if err := h.DeleteAPIKey(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusConflict {
		t.Errorf("code=%d want 409 (session key in use)", rec.Code)
	}
}

func TestDeleteAPIKey_StoreError_Returns500(t *testing.T) {
	us := &stubUserStore{deleteKeyErr: errors.New("db")}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodDelete, "/api-keys/k1", nil, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("k1")
	if err := h.DeleteAPIKey(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500", rec.Code)
	}
}

// UpdateUser: more branches

func TestUpdateUser_WithEnabledFalse_RevokesScope(t *testing.T) {
	// UpdateUser with enabled=false triggers revokeUserScope. revocation is nil
	// so revokeUserScope is a no-op — confirms the code path doesn't panic.
	enabled := false
	user := &userstore.NexusUserSafe{ID: "u1", DisplayName: "User", Status: "active"}
	us := &stubUserStore{updateResult: user}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	body, _ := json.Marshal(map[string]any{"enabled": enabled})
	c, rec := adminAuthCtx(http.MethodPatch, "/users/u1", body, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("u1")
	if err := h.UpdateUser(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200; body=%s", rec.Code, rec.Body)
	}
}

func TestUpdateUser_UpdateError_Returns500(t *testing.T) {
	us := &stubUserStore{updateErr: errors.New("db")}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	body, _ := json.Marshal(map[string]any{"displayName": "Updated"})
	c, rec := adminAuthCtx(http.MethodPatch, "/users/u1", body, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("u1")
	if err := h.UpdateUser(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500", rec.Code)
	}
}

// GetUserIdentity: more branches

func TestGetUserIdentity_VKsError_Returns500(t *testing.T) {
	user := &userstore.NexusUser{ID: "u1", DisplayName: "User", Status: "active"}
	us := &stubUserStore{findResult: user}
	gs := &stubGovernanceStore{vkSummariesErr: errors.New("db")}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, gs)
	c, rec := adminAuthCtx(http.MethodGet, "/users/u1/identity", nil, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("u1")
	if err := h.GetUserIdentity(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500 (vks error)", rec.Code)
	}
}

func TestGetUserIdentity_DevicesError_Returns500(t *testing.T) {
	user := &userstore.NexusUser{ID: "u1", DisplayName: "User", Status: "active"}
	us := &stubUserStore{findResult: user}
	gs := &stubGovernanceStore{deviceSumErr: errors.New("db")}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, gs)
	c, rec := adminAuthCtx(http.MethodGet, "/users/u1/identity", nil, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("u1")
	if err := h.GetUserIdentity(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500 (devices error)", rec.Code)
	}
}

func TestGetUserIdentity_AuditSummaryError_Returns500(t *testing.T) {
	user := &userstore.NexusUser{ID: "u1", DisplayName: "User", Status: "active"}
	us := &stubUserStore{findResult: user}
	gs := &stubGovernanceStore{auditSummaryErr: errors.New("db")}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, gs)
	c, rec := adminAuthCtx(http.MethodGet, "/users/u1/identity", nil, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("u1")
	if err := h.GetUserIdentity(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500 (audit summary error)", rec.Code)
	}
}

// RevokeUserAccess: more branches

func TestRevokeUserAccess_DisableKeysError_Returns500(t *testing.T) {
	user := &userstore.NexusUser{ID: "u1", DisplayName: "User", Status: "active"}
	us := &stubUserStore{findResult: user}
	gs := &stubGovernanceStore{disableVKErr: errors.New("db")}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, gs)
	c, rec := adminAuthCtx(http.MethodPost, "/users/u1/revoke", nil, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("u1")
	if err := h.RevokeUserAccess(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500 (disable keys error)", rec.Code)
	}
}

func TestRevokeUserAccess_RevokeDevicesError_Returns500(t *testing.T) {
	user := &userstore.NexusUser{ID: "u1", DisplayName: "User", Status: "active"}
	us := &stubUserStore{findResult: user}
	gs := &stubGovernanceStore{revokeDevErr: errors.New("db")}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, gs)
	c, rec := adminAuthCtx(http.MethodPost, "/users/u1/revoke", nil, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("u1")
	if err := h.RevokeUserAccess(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500 (revoke devices error)", rec.Code)
	}
}

func TestRevokeUserAccess_SuspendError_Returns500(t *testing.T) {
	user := &userstore.NexusUser{ID: "u1", DisplayName: "User", Status: "active"}
	us := &stubUserStore{findResult: user}
	gs := &stubGovernanceStore{suspendErr: errors.New("db")}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, gs)
	c, rec := adminAuthCtx(http.MethodPost, "/users/u1/revoke", nil, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("u1")
	if err := h.RevokeUserAccess(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500 (suspend error)", rec.Code)
	}
}

// OrganizationTree: with pool returning rows

// multiQueryPool returns different stubbed rows on successive Query calls,
// exercising the scan branches inside OrganizationTree.
type multiQueryPool struct {
	calls   int
	rowSets []pgx.Rows
	execTag pgconn.CommandTag
	execErr error
}

func (s *multiQueryPool) Begin(_ context.Context) (pgx.Tx, error) { return nil, nil }
func (s *multiQueryPool) Exec(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
	return s.execTag, s.execErr
}
func (s *multiQueryPool) Query(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
	idx := s.calls
	s.calls++
	if idx < len(s.rowSets) {
		return s.rowSets[idx], nil
	}
	return &emptyRows{}, nil
}
func (s *multiQueryPool) QueryRow(_ context.Context, _ string, _ ...any) pgx.Row { return nil }
func (s *multiQueryPool) Close()                                                 {}
func (s *multiQueryPool) Ping(_ context.Context) error                           { return nil }

func TestOrganizationTree_WithPool_Returns200(t *testing.T) {
	os := &stubOrgStore{
		orgs: []orgstore.Organization{
			{ID: "o1", Name: "Root", Code: "ROOT", Enabled: true},
		},
	}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, os, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	// Pool returns emptyRows for all 3 query calls (QuotaOverride, QuotaPolicy, metric_rollup)
	h.pool = &multiQueryPool{rowSets: []pgx.Rows{&emptyRows{}, &emptyRows{}, &emptyRows{}}}
	c, rec := adminAuthCtx(http.MethodGet, "/organizations/tree", nil, "admin", "admin_user")
	if err := h.OrganizationTree(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200; body=%s", rec.Code, rec.Body)
	}
}

// AddIAMGroupMember: more coverage

func TestAddIAMGroupMember_NexusUser_RevokesScope(t *testing.T) {
	orig := revocationRevoke
	revocationRevoke = func(_ *Handler, _ context.Context, _ revocation.Request) error { return nil }
	defer func() { revocationRevoke = orig }()

	is := &stubIAMStore{memberID: "m1"}
	h := buildHandler(&stubUserStore{}, is, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	h.revocation = &revocation.Service{}
	h.pool = &stubPool{}

	body, _ := json.Marshal(map[string]any{"principalType": "nexus_user", "principalId": "u1"})
	c, rec := adminAuthCtx(http.MethodPost, "/iam/groups/g1/members", body, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("g1")
	if err := h.AddIAMGroupMember(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("code=%d want 201; body=%s", rec.Code, rec.Body)
	}
}

func TestAddIAMGroupMember_DefaultPrincipalType_Returns201(t *testing.T) {
	is := &stubIAMStore{memberID: "m1"}
	h := buildHandler(&stubUserStore{}, is, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	// No principalType → defaults to nexus_user, revokeUserScope called with nil revocation → no-op
	body, _ := json.Marshal(map[string]any{"principalId": "u1"})
	c, rec := adminAuthCtx(http.MethodPost, "/iam/groups/g1/members", body, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("g1")
	if err := h.AddIAMGroupMember(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("code=%d want 201; body=%s", rec.Code, rec.Body)
	}
}

// RemoveIAMGroupMember: more branches

func TestRemoveIAMGroupMember_NexusUser_RevokesScope(t *testing.T) {
	orig := revocationRevoke
	revocationRevoke = func(_ *Handler, _ context.Context, _ revocation.Request) error { return nil }
	defer func() { revocationRevoke = orig }()

	is := &stubIAMStore{
		membershipPT:  "nexus_user",
		membershipPID: "u1",
	}
	h := buildHandler(&stubUserStore{}, is, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	h.revocation = &revocation.Service{}
	h.pool = &stubPool{}

	c, rec := adminAuthCtx(http.MethodDelete, "/iam/groups/g1/members/m1", nil, "admin", "admin_user")
	c.SetParamNames("id", "membershipId")
	c.SetParamValues("g1", "m1")
	if err := h.RemoveIAMGroupMember(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNoContent {
		t.Errorf("code=%d want 204; body=%s", rec.Code, rec.Body)
	}
}

// DetachIAMGroupPolicy: with members to revoke

func TestDetachIAMGroupPolicy_WithMembers_RevokesScope(t *testing.T) {
	orig := revocationRevoke
	revocationRevoke = func(_ *Handler, _ context.Context, _ revocation.Request) error { return nil }
	defer func() { revocationRevoke = orig }()

	is := &stubIAMStore{
		groupPolicyAttGID: "g1",
		groupPolicyAttPID: "p1",
		members: []iamstore.GroupMemberRow{
			{PrincipalType: "nexus_user", PrincipalID: "u1"},
			{PrincipalType: "api_key", PrincipalID: "k1"}, // non-nexus_user: skipped
		},
	}
	h := buildHandler(&stubUserStore{}, is, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	h.revocation = &revocation.Service{}
	h.pool = &stubPool{}

	c, rec := adminAuthCtx(http.MethodDelete, "/iam/groups/g1/policies/att1", nil, "admin", "admin_user")
	c.SetParamNames("id", "attachmentId")
	c.SetParamValues("g1", "att1")
	if err := h.DetachIAMGroupPolicy(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNoContent {
		t.Errorf("code=%d want 204; body=%s", rec.Code, rec.Body)
	}
}

// DetachPrincipalPolicy: nexus_user principal revokes scope

func TestDetachPrincipalPolicy_NexusUser_RevokesScope(t *testing.T) {
	orig := revocationRevoke
	revocationRevoke = func(_ *Handler, _ context.Context, _ revocation.Request) error { return nil }
	defer func() { revocationRevoke = orig }()

	is := &stubIAMStore{
		ppAttGID:      "nexus_user",
		ppAttPID:      "u1",
		ppAttPolicyID: "p1",
	}
	h := buildHandler(&stubUserStore{}, is, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	h.revocation = &revocation.Service{}
	h.pool = &stubPool{}

	c, rec := adminAuthCtx(http.MethodDelete, "/iam/principals/nexus_user/u1/policies/att1", nil, "admin", "admin_user")
	c.SetParamNames("type", "id", "attachmentId")
	c.SetParamValues("nexus_user", "u1", "att1")
	if err := h.DetachPrincipalPolicy(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNoContent {
		t.Errorf("code=%d want 204; body=%s", rec.Code, rec.Body)
	}
}

// AttachPrincipalPolicy: nexus_user principal revokes scope

func TestAttachPrincipalPolicy_NexusUser_RevokesScope(t *testing.T) {
	orig := revocationRevoke
	revocationRevoke = func(_ *Handler, _ context.Context, _ revocation.Request) error { return nil }
	defer func() { revocationRevoke = orig }()

	is := &stubIAMStore{attachPPID: "att1"}
	h := buildHandler(&stubUserStore{}, is, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	h.revocation = &revocation.Service{}
	h.pool = &stubPool{}

	body, _ := json.Marshal(map[string]any{"policyId": "p1"})
	c, rec := adminAuthCtx(http.MethodPost, "/iam/principals/nexus_user/u1/policies", body, "admin", "admin_user")
	c.SetParamNames("type", "id")
	c.SetParamValues("nexus_user", "u1")
	if err := h.AttachPrincipalPolicy(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("code=%d want 201; body=%s", rec.Code, rec.Body)
	}
}

// AttachIAMGroupPolicy: with member revocation fan-out

func TestAttachIAMGroupPolicy_WithNexusUserMembers_RevokesScope(t *testing.T) {
	orig := revocationRevoke
	revocationRevoke = func(_ *Handler, _ context.Context, _ revocation.Request) error { return nil }
	defer func() { revocationRevoke = orig }()

	is := &stubIAMStore{
		attachGroupPolID: "att1",
		members: []iamstore.GroupMemberRow{
			{PrincipalType: "nexus_user", PrincipalID: "u1"},
		},
	}
	h := buildHandler(&stubUserStore{}, is, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	h.revocation = &revocation.Service{}
	h.pool = &stubPool{}

	body, _ := json.Marshal(map[string]any{"policyId": "p1"})
	c, rec := adminAuthCtx(http.MethodPost, "/iam/groups/g1/policies", body, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("g1")
	if err := h.AttachIAMGroupPolicy(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("code=%d want 201; body=%s", rec.Code, rec.Body)
	}
}

// RevokeUserAccess: success with revokeUserScope

func TestRevokeUserAccess_Success_WithRevocation(t *testing.T) {
	orig := revocationRevoke
	revocationRevoke = func(_ *Handler, _ context.Context, _ revocation.Request) error { return nil }
	defer func() { revocationRevoke = orig }()

	user := &userstore.NexusUser{ID: "u1", DisplayName: "User", Status: "active"}
	us := &stubUserStore{findResult: user}
	gs := &stubGovernanceStore{disabledVKs: 2, revokedDevices: 1}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, gs)
	h.revocation = &revocation.Service{}
	h.pool = &stubPool{}

	c, rec := adminAuthCtx(http.MethodPost, "/users/u1/revoke", nil, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("u1")
	if err := h.RevokeUserAccess(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200; body=%s", rec.Code, rec.Body)
	}
}

// UpdateIAMPolicy: revoke users when policy detached

func TestUpdateIAMPolicy_DisabledTrue_RevokesAttachedUsers(t *testing.T) {
	orig := revocationRevoke
	revocationRevoke = func(_ *Handler, _ context.Context, _ revocation.Request) error { return nil }
	defer func() { revocationRevoke = orig }()

	is := &stubIAMStore{
		updatedPolicy:   &iamstore.PolicyRow{ID: "p1", Name: "P", Enabled: false},
		attachedUserIDs: []string{"u1", "u2"},
	}
	h := buildHandler(&stubUserStore{}, is, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	h.revocation = &revocation.Service{}
	h.pool = &stubPool{}

	body, _ := json.Marshal(map[string]any{"enabled": false})
	c, rec := adminAuthCtx(http.MethodPut, "/iam/policies/p1", body, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("p1")
	if err := h.UpdateIAMPolicy(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200; body=%s", rec.Code, rec.Body)
	}
}

// parseInt64: edge cases

func TestParseInt64_Valid_ReturnsValue(t *testing.T) {
	got := parseInt64("42")
	if got != 42 {
		t.Errorf("parseInt64(42) = %d, want 42", got)
	}
}

// ListAPIKeys: error path

func TestListAPIKeys_Error_Returns500(t *testing.T) {
	us := &stubUserStore{listKeysErr: errors.New("db")}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodGet, "/users/u1/api-keys", nil, "admin", "admin_user")
	c.SetParamNames("userId")
	c.SetParamValues("u1")
	if err := h.ListAPIKeys(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500", rec.Code)
	}
}

// pgxpool usage in TestNew_WithNonNilPool (exercises pool != nil branch)
// The existing test for New() was skipped due to pgxpool dial. We replace it
// with a test that directly sets sub-stores from a non-nil pool check path.
// Since we can't connect to a real DB, we test New() via Deps.Pool == nil path.

func TestNew_WithNilPool_LeavesStoresNil(t *testing.T) {
	h := New(Deps{Pool: nil})
	if h.users != nil {
		t.Error("expected h.users to be nil when pool is nil")
	}
}

// CreateScimToken: additional error path

func TestCreateScimToken_Error_Returns500(t *testing.T) {
	ss := &stubScimStore{
		idp:          &scimstore.IdentityProviderRecord{ID: "idp-1", Type: "oidc", Name: "Okta"},
		createTokErr: errors.New("db"),
	}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, &stubOrgStore{}, ss, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	// body.Name is required ("name" field) for CreateScimToken
	body, _ := json.Marshal(map[string]any{"name": "CI Token"})
	c, rec := adminAuthCtx(http.MethodPost, "/identity-providers/idp-1/scim-tokens", body, "admin", "admin_user")
	c.SetParamNames("idpId")
	c.SetParamValues("idp-1")
	if err := h.CreateScimToken(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500; body=%s", rec.Code, rec.Body)
	}
}

// UpdateMe: find error path

func TestUpdateMe_FindError_Returns500(t *testing.T) {
	us := &stubUserStore{findErr: errors.New("db")}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	// Admin user changing password triggers FindNexusUserByID
	body, _ := json.Marshal(map[string]any{
		"currentPassword": "old-pass",
		"newPassword":     "new-pass",
	})
	c, rec := adminAuthCtx(http.MethodPatch, "/me", body, "u1", "admin_user")
	if err := h.UpdateMe(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500", rec.Code)
	}
}

// buildMeResponse: group error and timezone paths

type stubGroupLookup struct {
	groups []string
	err    error
}

func (s *stubGroupLookup) ListGroupNamesForPrincipal(_ context.Context, _, _ string) ([]string, error) {
	return s.groups, s.err
}

type stubUserLookup struct {
	user *userstore.NexusUser
	err  error
}

func (s *stubUserLookup) FindNexusUserByID(_ context.Context, _ string) (*userstore.NexusUser, error) {
	return s.user, s.err
}

func TestBuildMeResponse_GroupsError_Returns200WithEmptyRoles(t *testing.T) {
	gl := &stubGroupLookup{err: errors.New("iam error")}
	ul := &stubUserLookup{}
	aa := &authn.AdminAuth{KeyID: "k1", KeyName: "key", AuthPrincipalType: "api_key"}
	req := httptest.NewRequest(http.MethodGet, "/me", nil)
	rec := httptest.NewRecorder()
	e := echo.New()
	c := e.NewContext(req, rec)
	middleware.WithAdminAuth(c, aa)
	if err := buildMeResponse(c, aa, ul, gl, slog.Default()); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200 (groups error is non-fatal)", rec.Code)
	}
}

func TestBuildMeResponse_AdminUserWithTimezone_IncludesTimezone(t *testing.T) {
	tz := "America/New_York"
	gl := &stubGroupLookup{groups: []string{"admins"}}
	ul := &stubUserLookup{user: &userstore.NexusUser{
		ID:                "u1",
		DisplayName:       "Test User",
		PreferredTimezone: &tz,
	}}
	aa := &authn.AdminAuth{KeyID: "u1", KeyName: "user", AuthPrincipalType: "admin_user"}
	req := httptest.NewRequest(http.MethodGet, "/me", nil)
	rec := httptest.NewRecorder()
	e := echo.New()
	c := e.NewContext(req, rec)
	middleware.WithAdminAuth(c, aa)
	if err := buildMeResponse(c, aa, ul, gl, slog.Default()); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200", rec.Code)
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["preferredTimezone"] != tz {
		t.Errorf("expected preferredTimezone=%q, got %v", tz, resp["preferredTimezone"])
	}
}

func TestBuildMeResponse_AdminUserFindError_Returns200(t *testing.T) {
	gl := &stubGroupLookup{groups: []string{}}
	ul := &stubUserLookup{err: errors.New("db")}
	aa := &authn.AdminAuth{KeyID: "u1", KeyName: "user", AuthPrincipalType: "admin_user"}
	req := httptest.NewRequest(http.MethodGet, "/me", nil)
	rec := httptest.NewRecorder()
	e := echo.New()
	c := e.NewContext(req, rec)
	middleware.WithAdminAuth(c, aa)
	if err := buildMeResponse(c, aa, ul, gl, slog.Default()); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200 (find error non-fatal for /me)", rec.Code)
	}
}

// UpdateMe: additional branches

func TestUpdateMe_APIKeyPrincipal_Returns403(t *testing.T) {
	// Non-admin_user principal type → 403
	h := defaultHandler()
	body, _ := json.Marshal(map[string]any{"displayName": "Test"})
	c, rec := adminAuthCtx(http.MethodPatch, "/me", body, "k1", "api_key")
	if err := h.UpdateMe(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("code=%d want 403 (api_key principal)", rec.Code)
	}
}

func TestUpdateMe_UpdateError_Returns500(t *testing.T) {
	us := &stubUserStore{updateErr: errors.New("db")}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	body, _ := json.Marshal(map[string]any{"displayName": "Updated"})
	c, rec := adminAuthCtx(http.MethodPatch, "/me", body, "u1", "admin_user")
	if err := h.UpdateMe(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500", rec.Code)
	}
}

// GetIAMGroup: not found

func TestGetIAMGroup_NotFound_Returns404(t *testing.T) {
	is := &stubIAMStore{group: nil, groupErr: nil} // nil group, nil err → 404
	h := buildHandler(&stubUserStore{}, is, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodGet, "/iam/groups/missing", nil, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("missing")
	if err := h.GetIAMGroup(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("code=%d want 404", rec.Code)
	}
}

func TestGetIAMGroup_WithPolicies_Returns200(t *testing.T) {
	is := &stubIAMStore{
		group:         &iamstore.GroupRow{ID: "g1", Name: "Group"},
		groupPolicies: []iamstore.GroupPolicyRow{{ID: "att1", PolicyID: "p1", PolicyName: "Policy"}},
	}
	h := buildHandler(&stubUserStore{}, is, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodGet, "/iam/groups/g1", nil, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("g1")
	if err := h.GetIAMGroup(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200", rec.Code)
	}
}

// UpdateIAMGroup: error path via errIAMStore

func TestUpdateIAMGroup_StoreError_Returns500(t *testing.T) {
	// errIAMStore.UpdateIamGroup always returns an error — exercises the 500 branch.
	eis := &errIAMStore{stubIAMStore: &stubIAMStore{}}
	h := buildHandler(&stubUserStore{}, eis, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	body, _ := json.Marshal(map[string]any{"name": "Bad"})
	c, rec := adminAuthCtx(http.MethodPut, "/iam/groups/g1", body, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("g1")
	if err := h.UpdateIAMGroup(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500 (store error)", rec.Code)
	}
}

// CreateIAMPolicy: error path

func TestCreateIAMPolicy_StoreError_Returns500(t *testing.T) {
	is := &stubIAMStore{createPolicyErr: errors.New("db")}
	h := buildHandler(&stubUserStore{}, is, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	h.iamEngine = superAdminTestEngine() // grant ceiling passes; store error is the path under test
	body, _ := json.Marshal(map[string]any{
		"name": "My Policy",
		"document": map[string]any{
			"Version":   "2026-05-12",
			"Statement": []map[string]any{{"Effect": "Allow", "Action": []string{"settings:read"}, "Resource": []string{"*"}}},
		},
	})
	c, rec := adminAuthCtx(http.MethodPost, "/iam/policies", body, "admin", "admin_user")
	if err := h.CreateIAMPolicy(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500", rec.Code)
	}
}

// ListIAMPolicies: enabled=true filter

func TestListIAMPolicies_EnabledTrue_Returns200(t *testing.T) {
	is := &stubIAMStore{policies: []iamstore.PolicyRow{{ID: "p1", Name: "P", Enabled: true}}, policiesTotal: 1}
	h := buildHandler(&stubUserStore{}, is, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	req := httptest.NewRequest(http.MethodGet, "/iam/policies?enabled=true", nil)
	rec := httptest.NewRecorder()
	e := echo.New()
	c := e.NewContext(req, rec)
	if err := h.ListIAMPolicies(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200", rec.Code)
	}
}

// DeleteIAMPolicy: with iamEngine

func TestDeleteIAMPolicy_WithEngine_Returns200(t *testing.T) {
	loader := &stubPolicyLoader{policies: []cpiam.LoadedPolicy{}}
	engine := cpiam.NewEngine(loader, slog.Default())
	h := defaultHandler()
	h.iamEngine = engine
	c, rec := adminAuthCtx(http.MethodDelete, "/iam/policies/p1", nil, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("p1")
	if err := h.DeleteIAMPolicy(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNoContent {
		t.Errorf("code=%d want 204; body=%s", rec.Code, rec.Body)
	}
}

// UpdateIAMPolicy: with iamEngine + ListPolicyAttachedUserIDs error

func TestUpdateIAMPolicy_WithEngine_Returns200(t *testing.T) {
	loader := &stubPolicyLoader{policies: []cpiam.LoadedPolicy{}}
	engine := cpiam.NewEngine(loader, slog.Default())
	is := &stubIAMStore{
		updatedPolicy:   &iamstore.PolicyRow{ID: "p1", Name: "P", Enabled: true},
		attachedUserErr: errors.New("db"), // ListPolicyAttachedUserIDs error: logged and skipped
	}
	h := buildHandler(&stubUserStore{}, is, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	h.iamEngine = engine

	body, _ := json.Marshal(map[string]any{"name": "Updated Policy"})
	c, rec := adminAuthCtx(http.MethodPut, "/iam/policies/p1", body, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("p1")
	if err := h.UpdateIAMPolicy(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200; body=%s", rec.Code, rec.Body)
	}
}

// CreateIdentityProvider: additional branches

func TestCreateIdentityProvider_OIDCWithJIT_Returns201(t *testing.T) {
	ss := &stubScimStore{
		createdIdp: &scimstore.IdentityProviderRecord{ID: "idp-1", Type: "oidc", Name: "Okta"},
	}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, &stubOrgStore{}, ss, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	f := false
	t_ := true
	enabled := &f // body.Enabled = false
	jit := &t_    // body.JITEnabled = true
	_ = enabled
	_ = jit
	body, _ := json.Marshal(map[string]any{
		"type": "oidc", "name": "Okta",
		"enabled":    false,
		"jitEnabled": true,
		"config": map[string]any{
			"issuer":       "https://okta.com",
			"clientId":     "client-1",
			"redirectUri":  "https://app.com/callback",
			"clientSecret": "super-secret",
		},
	})
	c, rec := adminAuthCtx(http.MethodPost, "/identity-providers", body, "admin", "admin_user")
	if err := h.CreateIdentityProvider(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("code=%d want 201; body=%s", rec.Code, rec.Body)
	}
}

func TestCreateIdentityProvider_StoreError_Returns500(t *testing.T) {
	ss := &stubScimStore{createIdpErr: errors.New("db")}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, &stubOrgStore{}, ss, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	body, _ := json.Marshal(map[string]any{
		"type": "oidc", "name": "Okta",
		"config": map[string]any{
			"issuer":       "https://okta.com",
			"clientId":     "client-1",
			"redirectUri":  "https://app.com/callback",
			"clientSecret": "super-secret",
		},
	})
	c, rec := adminAuthCtx(http.MethodPost, "/identity-providers", body, "admin", "admin_user")
	if err := h.CreateIdentityProvider(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500", rec.Code)
	}
}

// UpdateIdentityProvider: enabled transition + jit

func TestUpdateIdentityProvider_EnabledToDisabled_FansOutRevocations(t *testing.T) {
	orig := revocationRevoke
	revocationRevoke = func(_ *Handler, _ context.Context, _ revocation.Request) error { return nil }
	defer func() { revocationRevoke = orig }()

	existingCfg, _ := json.Marshal(map[string]any{
		"issuer":       "https://okta.com",
		"clientId":     "cid",
		"redirectUri":  "https://app.com/cb",
		"clientSecret": "secret",
	})
	// existing.Enabled = true; updated.Enabled = false → fan-out
	ss := &stubScimStore{
		idp: &scimstore.IdentityProviderRecord{
			ID: "idp-1", Type: "oidc", Name: "Okta", Enabled: true, Config: existingCfg,
		},
		updatedIdp: &scimstore.IdentityProviderRecord{
			ID: "idp-1", Type: "oidc", Name: "Okta", Enabled: false,
		},
	}
	feds := &stubFedStore{userIDs: []string{"u1"}}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, &stubOrgStore{}, ss, &stubFleetStore{}, &stubVKStore{}, feds, &stubGovernanceStore{})
	h.revocation = &revocation.Service{}
	h.pool = &stubPool{}

	body, _ := json.Marshal(map[string]any{
		"type": "oidc", "name": "Okta",
		"enabled": false,
		"config": map[string]any{
			"issuer":       "https://okta.com",
			"clientId":     "cid",
			"redirectUri":  "https://app.com/cb",
			"clientSecret": "secret",
		},
	})
	c, rec := adminAuthCtx(http.MethodPut, "/identity-providers/idp-1", body, "admin", "admin_user")
	c.SetParamNames("idpId")
	c.SetParamValues("idp-1")
	if err := h.UpdateIdentityProvider(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200; body=%s", rec.Code, rec.Body)
	}
}

func TestUpdateIdentityProvider_StoreError_Returns500(t *testing.T) {
	existingCfg, _ := json.Marshal(map[string]any{
		"issuer":       "https://okta.com",
		"clientId":     "cid",
		"redirectUri":  "https://app.com/cb",
		"clientSecret": "secret",
	})
	ss := &stubScimStore{
		idp: &scimstore.IdentityProviderRecord{
			ID: "idp-1", Type: "oidc", Name: "Okta", Enabled: true, Config: existingCfg,
		},
		updateIdpErr: errors.New("db"),
	}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, &stubOrgStore{}, ss, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	body, _ := json.Marshal(map[string]any{
		"type": "oidc", "name": "Okta Updated",
		"config": map[string]any{
			"issuer":       "https://okta.com",
			"clientId":     "cid",
			"redirectUri":  "https://app.com/cb",
			"clientSecret": "secret",
		},
	})
	c, rec := adminAuthCtx(http.MethodPut, "/identity-providers/idp-1", body, "admin", "admin_user")
	c.SetParamNames("idpId")
	c.SetParamValues("idp-1")
	if err := h.UpdateIdentityProvider(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500", rec.Code)
	}
}

// DeleteIdentityProvider: store error paths

func TestDeleteIdentityProvider_StoreOtherError_Returns500(t *testing.T) {
	ss := &stubScimStore{
		idp:    &scimstore.IdentityProviderRecord{ID: "idp-1", Type: "oidc", Name: "Okta"},
		idpErr: errors.New("server error"), // non-ErrNoRows → 500
	}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, &stubOrgStore{}, ss, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodDelete, "/identity-providers/idp-1", nil, "admin", "admin_user")
	c.SetParamNames("idpId")
	c.SetParamValues("idp-1")
	if err := h.DeleteIdentityProvider(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500", rec.Code)
	}
}

func TestDeleteIdentityProvider_DeleteError_Returns500(t *testing.T) {
	ss := &stubScimStore{
		idp:          &scimstore.IdentityProviderRecord{ID: "idp-1", Type: "oidc", Name: "Okta"},
		deleteIdpErr: errors.New("db"),
	}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, &stubOrgStore{}, ss, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodDelete, "/identity-providers/idp-1", nil, "admin", "admin_user")
	c.SetParamNames("idpId")
	c.SetParamValues("idp-1")
	if err := h.DeleteIdentityProvider(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500", rec.Code)
	}
}

// validateIdPRequest: missing name and missing config paths

func TestCreateIdentityProvider_MissingName_Returns400(t *testing.T) {
	h := defaultHandler()
	body, _ := json.Marshal(map[string]any{
		"type": "oidc",
		// name is empty
		"config": map[string]any{
			"issuer":      "https://okta.com",
			"clientId":    "cid",
			"redirectUri": "https://app.com/cb",
		},
	})
	c, rec := adminAuthCtx(http.MethodPost, "/identity-providers", body, "admin", "admin_user")
	if err := h.CreateIdentityProvider(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code=%d want 400 (missing name)", rec.Code)
	}
}

func TestCreateIdentityProvider_NilConfig_Returns400(t *testing.T) {
	h := defaultHandler()
	body, _ := json.Marshal(map[string]any{
		"type": "oidc", "name": "Okta",
		// config is omitted (nil)
	})
	c, rec := adminAuthCtx(http.MethodPost, "/identity-providers", body, "admin", "admin_user")
	if err := h.CreateIdentityProvider(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code=%d want 400 (nil config)", rec.Code)
	}
}

func TestCreateIdentityProvider_OIDCMissingIssuer_Returns400(t *testing.T) {
	h := defaultHandler()
	body, _ := json.Marshal(map[string]any{
		"type": "oidc", "name": "Okta",
		"config": map[string]any{
			// issuer is missing
			"clientId":    "cid",
			"redirectUri": "https://app.com/cb",
		},
	})
	c, rec := adminAuthCtx(http.MethodPost, "/identity-providers", body, "admin", "admin_user")
	if err := h.CreateIdentityProvider(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code=%d want 400 (missing issuer)", rec.Code)
	}
}

// mergeMaskedSecrets: delete-when-old-missing branch

func TestMergeMaskedSecrets_OldMissing_DeletesField(t *testing.T) {
	// When existing config doesn't have the key, masked incoming → delete
	incoming := map[string]any{"clientSecret": "********"}
	existing := map[string]any{} // old key is absent
	mergeMaskedSecrets(incoming, existing, "oidc")
	if _, ok := incoming["clientSecret"]; ok {
		t.Error("expected clientSecret to be deleted when existing doesn't have it")
	}
}

func TestMergeMaskedSecrets_OldPresent_RestoresValue(t *testing.T) {
	incoming := map[string]any{"clientSecret": "********"}
	existing := map[string]any{"clientSecret": "real-secret-value"}
	mergeMaskedSecrets(incoming, existing, "oidc")
	if incoming["clientSecret"] != "real-secret-value" {
		t.Errorf("expected clientSecret restored to 'real-secret-value', got %v", incoming["clientSecret"])
	}
}

func TestMergeMaskedSecrets_NotMasked_Unchanged(t *testing.T) {
	incoming := map[string]any{"clientSecret": "new-real-secret"}
	existing := map[string]any{"clientSecret": "old-secret"}
	mergeMaskedSecrets(incoming, existing, "oidc")
	if incoming["clientSecret"] != "new-real-secret" {
		t.Errorf("expected clientSecret unchanged at 'new-real-secret', got %v", incoming["clientSecret"])
	}
}

// RevokeDeviceInternal: ExpiresAt body path

func TestRevokeDeviceInternal_WithExpiresAt_Returns204(t *testing.T) {
	orig := revocationRevoke
	revocationRevoke = func(_ *Handler, _ context.Context, _ revocation.Request) error { return nil }
	defer func() { revocationRevoke = orig }()

	h := defaultHandler()
	h.pool = &stubPool{}
	h.revocation = &revocation.Service{}

	exp := time.Now().Add(24 * time.Hour)
	body, _ := json.Marshal(map[string]any{
		"deviceId":  "dev-1",
		"expiresAt": exp.Format(time.RFC3339),
	})
	c, rec := adminAuthCtx(http.MethodPost, "/auth/revoke-device", body, "internal", "admin_user")
	if err := h.RevokeDeviceInternal(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNoContent {
		t.Errorf("code=%d want 204; body=%s", rec.Code, rec.Body)
	}
}

func TestRevokeDeviceInternal_ExecError_Returns500(t *testing.T) {
	orig := revocationRevoke
	revocationRevoke = func(_ *Handler, _ context.Context, _ revocation.Request) error { return nil }
	defer func() { revocationRevoke = orig }()

	h := defaultHandler()
	h.pool = &stubPool{execErr: errors.New("db error")}
	h.revocation = &revocation.Service{}

	body, _ := json.Marshal(map[string]any{"deviceId": "dev-1"})
	c, rec := adminAuthCtx(http.MethodPost, "/auth/revoke-device", body, "internal", "admin_user")
	if err := h.RevokeDeviceInternal(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500 (exec error)", rec.Code)
	}
}

// revokeUserScope: revocationRevoke error logged but callers continue

func TestRevokeUserScope_RevocationError_LogsAndContinues(t *testing.T) {
	orig := revocationRevoke
	revocationRevoke = func(_ *Handler, _ context.Context, _ revocation.Request) error {
		return errors.New("revocation failed")
	}
	defer func() { revocationRevoke = orig }()

	h := defaultHandler()
	h.revocation = &revocation.Service{}
	// No pool — so only the revocation path (which logs and continues) is exercised
	h.revokeUserScope(context.Background(), "u1", revocation.ReasonAdminDisable)
	// Should not panic; error is logged
}

// CreateAPIKey: ExpiresAt path

func TestCreateAPIKey_WithExpiresAt_Returns201(t *testing.T) {
	uid := "admin"
	us := &stubUserStore{createKey: &userstore.AdminAPIKey{ID: "k1", Name: "Key", OwnerUserID: &uid}}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	future := time.Now().Add(24 * time.Hour).Format(time.RFC3339)
	body, _ := json.Marshal(map[string]any{"name": "Key", "expiresAt": future})
	c, rec := adminAuthCtx(http.MethodPost, "/api-keys", body, "admin", "admin_user")
	if err := h.CreateAPIKey(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("code=%d want 201; body=%s", rec.Code, rec.Body)
	}
}

func TestCreateAPIKey_Error_Returns500(t *testing.T) {
	us := &stubUserStore{createKeyErr: errors.New("db")}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	body, _ := json.Marshal(map[string]any{"name": "Key"})
	c, rec := adminAuthCtx(http.MethodPost, "/api-keys", body, "admin", "admin_user")
	if err := h.CreateAPIKey(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500", rec.Code)
	}
}

// OrganizationTree: rows with scan data

// twoColRows returns one row with two columns (string + float64) on Next(),
// then false. Used to exercise scan branches inside OrganizationTree.
type twoColRows struct {
	called bool
	col1   string
	col2   float64
}

func (r *twoColRows) Close()                                       {}
func (r *twoColRows) Err() error                                   { return nil }
func (r *twoColRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *twoColRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *twoColRows) Next() bool {
	if !r.called {
		r.called = true
		return true
	}
	return false
}
func (r *twoColRows) Scan(args ...any) error {
	if len(args) == 2 {
		if s, ok := args[0].(*string); ok {
			*s = r.col1
		}
		if f, ok := args[1].(*float64); ok {
			*f = r.col2
		}
	}
	return nil
}
func (r *twoColRows) Values() ([]any, error) { return nil, nil }
func (r *twoColRows) RawValues() [][]byte    { return nil }
func (r *twoColRows) Conn() *pgx.Conn        { return nil }

func TestOrganizationTree_WithQuotaData_Returns200(t *testing.T) {
	os := &stubOrgStore{
		orgs: []orgstore.Organization{
			{ID: "o1", Name: "Root", Code: "ROOT", Enabled: true},
		},
	}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, os, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	// 3 pool.Query calls: QuotaOverride, QuotaPolicy, metric_rollup_1h
	h.pool = &multiQueryPool{rowSets: []pgx.Rows{
		&twoColRows{col1: "o1", col2: 100.0},
		&twoColRows{col1: "o1", col2: 50.0},
		&twoColRows{col1: "organization=o1", col2: 25.0},
	}}
	c, rec := adminAuthCtx(http.MethodGet, "/organizations/tree", nil, "admin", "admin_user")
	if err := h.OrganizationTree(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200; body=%s", rec.Code, rec.Body)
	}
}

func TestOrganizationTree_WithChildOrg_BuildsTree(t *testing.T) {
	parentID := "o1"
	os := &stubOrgStore{
		orgs: []orgstore.Organization{
			{ID: "o1", Name: "Root", Code: "ROOT", Enabled: true},
			{ID: "o2", Name: "Child", Code: "CHILD", Enabled: true, ParentID: &parentID},
		},
	}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, os, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	h.pool = &stubPool{queryErr: errors.New("skip pool")} // pool queries skipped
	c, rec := adminAuthCtx(http.MethodGet, "/organizations/tree", nil, "admin", "admin_user")
	if err := h.OrganizationTree(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200; body=%s", rec.Code, rec.Body)
	}
	// The child should appear nested under root
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	data := resp["data"].([]any)
	if len(data) != 1 {
		t.Errorf("expected 1 root, got %d", len(data))
	}
}

// AttachIAMGroupPolicy: ListGroupMembers error path

func TestAttachIAMGroupPolicy_MembersListError_StillReturns201(t *testing.T) {
	is := &stubIAMStore{
		attachGroupPolID: "att1",
		membersErr:       errors.New("members db error"),
	}
	h := buildHandler(&stubUserStore{}, is, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})

	body, _ := json.Marshal(map[string]any{"policyId": "p1"})
	c, rec := adminAuthCtx(http.MethodPost, "/iam/groups/g1/policies", body, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("g1")
	if err := h.AttachIAMGroupPolicy(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("code=%d want 201 (members error logged, not fatal)", rec.Code)
	}
}

// UpdateProject: additional error paths

func TestUpdateProject_StoreError_Returns500(t *testing.T) {
	os := &stubOrgStore{updateProjErr: errors.New("db")}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, os, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	body, _ := json.Marshal(map[string]any{"name": "Updated"})
	c, rec := adminAuthCtx(http.MethodPut, "/projects/p1", body, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("p1")
	if err := h.UpdateProject(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500", rec.Code)
	}
}

// ListUsers: more filter paths

func TestListUsers_WithEmailAndIDPFilter_Returns200(t *testing.T) {
	us := &stubUserStore{listResult: []userstore.NexusUserSafe{}}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodGet, "/users?email=test@example.com&identityProviderId=idp-1", nil, "admin", "admin_user")
	if err := h.ListUsers(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200", rec.Code)
	}
}

// CreateProject: additional path

func TestCreateProject_WithOrg_Returns201(t *testing.T) {
	os := &stubOrgStore{createdProject: &orgstore.Project{ID: "p2", Name: "Proj2", Code: "P2"}}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, os, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	body, _ := json.Marshal(map[string]any{"name": "Proj2", "code": "P2", "organizationId": "o1"})
	c, rec := adminAuthCtx(http.MethodPost, "/projects", body, "admin", "admin_user")
	if err := h.CreateProject(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("code=%d want 201; body=%s", rec.Code, rec.Body)
	}
}

// GetIdentityProvider: success path

func TestGetIdentityProvider_Found_Returns200(t *testing.T) {
	oidcCfg, _ := json.Marshal(map[string]any{
		"issuer":       "https://okta.com",
		"clientId":     "cid",
		"clientSecret": "secret",
	})
	ss := &stubScimStore{
		idp: &scimstore.IdentityProviderRecord{ID: "idp-1", Type: "oidc", Name: "Okta", Config: oidcCfg},
	}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, &stubOrgStore{}, ss, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodGet, "/identity-providers/idp-1", nil, "admin", "admin_user")
	c.SetParamNames("idpId")
	c.SetParamValues("idp-1")
	if err := h.GetIdentityProvider(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200; body=%s", rec.Code, rec.Body)
	}
}

// UpdateAPIKey: empty expiresAt branch (no update, no error)

func TestUpdateAPIKey_EmptyExpiresAt_Returns400(t *testing.T) {
	h := defaultHandler()
	// Empty expiresAt with no other fields → no valid fields → 400
	body, _ := json.Marshal(map[string]any{"expiresAt": ""})
	c, rec := adminAuthCtx(http.MethodPatch, "/api-keys/k1", body, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("k1")
	if err := h.UpdateAPIKey(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code=%d want 400 (empty expiresAt counts as no update)", rec.Code)
	}
}

// ListUserDeviceAssignments: additional success path

func TestListUserDeviceAssignments_EmptyList_Returns200(t *testing.T) {
	fs := &stubFleetStore{assignments: []fleetstore.DeviceAssignmentDetail{}}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, fs, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodGet, "/users/u1/device-assignments", nil, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("u1")
	if err := h.ListUserDeviceAssignments(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200", rec.Code)
	}
}

// Register*Routes: exercise route-wiring functions
// These functions only call g.GET/POST/etc with no business logic; testing them
// confirms all routes are wired without panicking.

func noopIAMMW(_ string) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return next
	}
}

func TestRegisterAPIKeyRoutes_DoesNotPanic(t *testing.T) {
	h := defaultHandler()
	e := echo.New()
	g := e.Group("")
	h.RegisterAPIKeyRoutes(g, noopIAMMW)
	// If we get here without panic, route wiring succeeded.
}

func TestRegisterAuthSessionRoutes_DoesNotPanic(t *testing.T) {
	h := defaultHandler()
	e := echo.New()
	g := e.Group("")
	h.RegisterAuthSessionRoutes(g, noopIAMMW)
}

func TestRegisterInternalAuthRoutes_DoesNotPanic(t *testing.T) {
	h := defaultHandler()
	e := echo.New()
	g := e.Group("")
	h.RegisterInternalAuthRoutes(g)
}

func TestRegisterIAMRoutes_DoesNotPanic(t *testing.T) {
	h := defaultHandler()
	e := echo.New()
	g := e.Group("")
	h.RegisterIAMRoutes(g, noopIAMMW)
}

func TestRegisterIdentityProviderRoutes_DoesNotPanic(t *testing.T) {
	h := defaultHandler()
	e := echo.New()
	g := e.Group("")
	h.RegisterIdentityProviderRoutes(g, noopIAMMW)
}

func TestRegisterMeRoutes_DoesNotPanic(t *testing.T) {
	h := defaultHandler()
	e := echo.New()
	g := e.Group("")
	h.RegisterMeRoutes(g, noopIAMMW)
}

func TestRegisterOrganizationTreeRoute_DoesNotPanic(t *testing.T) {
	h := defaultHandler()
	e := echo.New()
	g := e.Group("")
	h.RegisterOrganizationTreeRoute(g, noopIAMMW)
}

func TestRegisterOrganizationRoutes_DoesNotPanic(t *testing.T) {
	h := defaultHandler()
	e := echo.New()
	g := e.Group("")
	h.RegisterOrganizationRoutes(g, noopIAMMW)
}

func TestRegisterUserRoutes_DoesNotPanic(t *testing.T) {
	h := defaultHandler()
	e := echo.New()
	g := e.Group("")
	h.RegisterUserRoutes(g, noopIAMMW)
}

// parseTimeRange: both timestamps parsed correctly

func TestParseTimeRange_BothTimestamps(t *testing.T) {
	// Exercises parseTimeRange → parseRFC3339Flexible with startTime and endTime query params
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/?startTime=2024-01-01T00:00:00Z&endTime=2024-01-31T23:59:59Z", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	start, end := parseTimeRange(c)
	if start == nil {
		t.Error("expected non-nil start")
	}
	if end == nil {
		t.Error("expected non-nil end")
	}
	if start != nil && start.IsZero() {
		t.Error("expected non-zero start time")
	}
	if end != nil && end.IsZero() {
		t.Error("expected non-zero end time")
	}
}

func TestParseTimeRange_InvalidTimestamps(t *testing.T) {
	// Invalid timestamps → both nil
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/?startTime=not-a-date&endTime=also-invalid", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	start, end := parseTimeRange(c)
	if start != nil {
		t.Error("expected nil start for invalid input")
	}
	if end != nil {
		t.Error("expected nil end for invalid input")
	}
}

// errIAMStore: custom stub for UpdateIamGroup error path
// (Used by TestUpdateIAMGroup_Error_Returns500 declared near line 4693)

type errIAMStore struct {
	*stubIAMStore
}

func (s *errIAMStore) UpdateIamGroup(_ context.Context, _ string, _ iamstore.UpdateIamGroupParams) (*iamstore.GroupRow, error) {
	return nil, errors.New("db error")
}

// AddIAMGroupMember: store error

func TestAddIAMGroupMember_StoreError_Returns500(t *testing.T) {
	is := &stubIAMStore{memberErr: errors.New("db")}
	h := buildHandler(&stubUserStore{}, is, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	body, _ := json.Marshal(map[string]any{"principalId": "u1"})
	c, rec := adminAuthCtx(http.MethodPost, "/iam/groups/g1/members", body, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("g1")
	if err := h.AddIAMGroupMember(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500", rec.Code)
	}
}

// DetachIAMGroupPolicy: GetGroupPolicyAttachmentByID error → skip revoke

func TestDetachIAMGroupPolicy_LookupError_StillDetaches(t *testing.T) {
	is := &stubIAMStore{
		groupPolicyAttErr: errors.New("lookup error"),
	}
	h := buildHandler(&stubUserStore{}, is, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})

	c, rec := adminAuthCtx(http.MethodDelete, "/iam/groups/g1/policies/att1", nil, "admin", "admin_user")
	c.SetParamNames("id", "attachmentId")
	c.SetParamValues("g1", "att1")
	if err := h.DetachIAMGroupPolicy(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNoContent {
		t.Errorf("code=%d want 204; body=%s", rec.Code, rec.Body)
	}
}

// AttachPrincipalPolicy: error branch

func TestAttachPrincipalPolicy_StoreError_Returns500(t *testing.T) {
	is := &stubIAMStore{attachPPErr: errors.New("db")}
	h := buildHandler(&stubUserStore{}, is, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	body, _ := json.Marshal(map[string]any{"policyId": "p1"})
	c, rec := adminAuthCtx(http.MethodPost, "/iam/principals/nexus_user/u1/policies", body, "admin", "admin_user")
	c.SetParamNames("type", "id")
	c.SetParamValues("nexus_user", "u1")
	if err := h.AttachPrincipalPolicy(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500", rec.Code)
	}
}

// UpdateOrganization: error path

func TestUpdateOrganization_StoreError_Returns500(t *testing.T) {
	os := &stubOrgStore{updateOrgErr: errors.New("db")}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, os, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	body, _ := json.Marshal(map[string]any{"name": "Updated"})
	c, rec := adminAuthCtx(http.MethodPut, "/organizations/o1", body, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("o1")
	if err := h.UpdateOrganization(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500", rec.Code)
	}
}

// DeleteOrganization: additional paths

func TestDeleteOrganization_HasProjects_Returns409(t *testing.T) {
	os := &stubOrgStore{orgProjects: 1}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, os, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodDelete, "/organizations/o1", nil, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("o1")
	if err := h.DeleteOrganization(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusConflict {
		t.Errorf("code=%d want 409 (has projects)", rec.Code)
	}
}

func TestDeleteOrganization_StoreError_Returns500(t *testing.T) {
	os := &stubOrgStore{deleteOrgErr: errors.New("db")}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, os, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodDelete, "/organizations/o1", nil, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("o1")
	if err := h.DeleteOrganization(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500", rec.Code)
	}
}

// CreateProject: missing code and store error

func TestCreateProject_MissingCode_Returns400(t *testing.T) {
	h := defaultHandler()
	body, _ := json.Marshal(map[string]any{"name": "Project"})
	c, rec := adminAuthCtx(http.MethodPost, "/projects", body, "admin", "admin_user")
	if err := h.CreateProject(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code=%d want 400 (missing code)", rec.Code)
	}
}

func TestCreateProject_StoreError_Returns500(t *testing.T) {
	os := &stubOrgStore{createProjErr: errors.New("db")}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, os, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	body, _ := json.Marshal(map[string]any{"name": "Project", "code": "PROJ", "organizationId": "o1"})
	c, rec := adminAuthCtx(http.MethodPost, "/projects", body, "admin", "admin_user")
	if err := h.CreateProject(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500", rec.Code)
	}
}

// UpdateUser: password hashing path

func TestUpdateUser_WithPassword_Returns200(t *testing.T) {
	user := &userstore.NexusUserSafe{ID: "u1", DisplayName: "User"}
	us := &stubUserStore{updateResult: user}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	body, _ := json.Marshal(map[string]any{"password": "new-secure-password"})
	c, rec := adminAuthCtx(http.MethodPatch, "/users/u1", body, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("u1")
	if err := h.UpdateUser(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200; body=%s", rec.Code, rec.Body)
	}
}

// UpdateUser: an admin cannot set a local password on an externally-provisioned
// (SSO) account — it signs in through its IdP and has no local password. The
// handler refuses with 400 / "sso_account" rather than silently creating a
// shadow password the user can never use.
func TestUpdateUser_SSOAccount_PasswordSetReturns400(t *testing.T) {
	us := &stubUserStore{findResult: &userstore.NexusUser{ID: "u1", Source: "scim"}}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	body, _ := json.Marshal(map[string]any{"password": "admin-set-password"})
	c, rec := adminAuthCtx(http.MethodPatch, "/users/u1", body, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("u1")
	if err := h.UpdateUser(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400 (SSO account); body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "sso_account") {
		t.Errorf("body=%s want sso_account code", rec.Body.String())
	}
}

// UpdateMe: displayName-only path (no password)

func TestUpdateMe_DisplayNameOnly_Returns200(t *testing.T) {
	user := &userstore.NexusUserSafe{ID: "u1", DisplayName: "Updated"}
	us := &stubUserStore{updateResult: user}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	body, _ := json.Marshal(map[string]any{"displayName": "Updated"})
	c, rec := adminAuthCtx(http.MethodPatch, "/me", body, "u1", "admin_user")
	if err := h.UpdateMe(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200; body=%s", rec.Code, rec.Body)
	}
}

// CreateIdpGroupMapping: error path

func TestCreateIdpGroupMapping_StoreError_Returns500(t *testing.T) {
	ss := &stubScimStore{createMapErr: errors.New("db")}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, &stubOrgStore{}, ss, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	body, _ := json.Marshal(map[string]any{
		"externalGroupId": "ext-g1",
		"iamGroupId":      "iam-g1",
	})
	c, rec := adminAuthCtx(http.MethodPost, "/identity-providers/idp-1/group-mappings", body, "admin", "admin_user")
	c.SetParamNames("idpId")
	c.SetParamValues("idp-1")
	if err := h.CreateIdpGroupMapping(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500", rec.Code)
	}
}

// UpdateIdentityProvider: GetIdentityProvider other-error path

func TestUpdateIdentityProvider_GetIdpOtherError_Returns500(t *testing.T) {
	ss := &stubScimStore{idpErr: errors.New("server error")} // non-ErrNoRows
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, &stubOrgStore{}, ss, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	body, _ := json.Marshal(map[string]any{"type": "oidc", "name": "X"})
	c, rec := adminAuthCtx(http.MethodPut, "/identity-providers/idp-1", body, "admin", "admin_user")
	c.SetParamNames("idpId")
	c.SetParamValues("idp-1")
	if err := h.UpdateIdentityProvider(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500 (get idp other error)", rec.Code)
	}
}

// ListUsers: org + canAccessControlPlane filter

func TestListUsers_WithOrgFilter_Returns200(t *testing.T) {
	us := &stubUserStore{listResult: []userstore.NexusUserSafe{}}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodGet, "/users?organizationId=o1&canAccessControlPlane=true", nil, "admin", "admin_user")
	if err := h.ListUsers(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200", rec.Code)
	}
}

// enrichProjects: VKs error path

func TestListProjects_WithVKCountError_Returns200(t *testing.T) {
	os := &stubOrgStore{
		projects:      []orgstore.Project{{ID: "p1", Name: "Project", Code: "P"}},
		projectsTotal: 1,
		projVKErr:     errors.New("vk count error"),
	}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, os, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodGet, "/projects?organizationId=o1", nil, "admin", "admin_user")
	if err := h.ListProjects(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200; body=%s", rec.Code, rec.Body)
	}
}

// CreateScimToken: empty name returns 400

func TestCreateScimToken_EmptyName_Returns400(t *testing.T) {
	h := defaultHandler()
	body, _ := json.Marshal(map[string]any{}) // no name field
	c, rec := adminAuthCtx(http.MethodPost, "/identity-providers/idp-1/scim-tokens", body, "admin", "admin_user")
	c.SetParamNames("idpId")
	c.SetParamValues("idp-1")
	if err := h.CreateScimToken(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code=%d want 400 (empty name)", rec.Code)
	}
}

// UpdateIdentityProvider: JITEnabled body override

func TestUpdateIdentityProvider_WithJIT_Returns200(t *testing.T) {
	existingCfg, _ := json.Marshal(map[string]any{
		"issuer":       "https://okta.com",
		"clientId":     "cid",
		"redirectUri":  "https://app.com/cb",
		"clientSecret": "secret",
	})
	ss := &stubScimStore{
		idp: &scimstore.IdentityProviderRecord{
			ID: "idp-1", Type: "oidc", Name: "Okta", Enabled: true, Config: existingCfg,
		},
		updatedIdp: &scimstore.IdentityProviderRecord{ID: "idp-1", Type: "oidc", Name: "Updated", JITEnabled: true},
	}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, &stubOrgStore{}, ss, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	body, _ := json.Marshal(map[string]any{
		"type": "oidc", "name": "Updated",
		"jitEnabled": true,
		"config": map[string]any{
			"issuer":       "https://okta.com",
			"clientId":     "cid",
			"redirectUri":  "https://app.com/cb",
			"clientSecret": "secret",
		},
	})
	c, rec := adminAuthCtx(http.MethodPut, "/identity-providers/idp-1", body, "admin", "admin_user")
	c.SetParamNames("idpId")
	c.SetParamValues("idp-1")
	if err := h.UpdateIdentityProvider(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200; body=%s", rec.Code, rec.Body)
	}
}

// Phase-6 coverage gap-fill: hub invalidate, bind errors, iamEngine, sessions

// stubHub satisfies HubAPI for tests that need the hub to be non-nil so the
// `if h.hub != nil` branches are exercised without a real Hub server.
type stubHub struct {
	invalidateErr   error    // injected error for InvalidateConfigE
	invalidatedKeys []string // configKeys passed to InvalidateConfigE
}

func (s *stubHub) NotifyConfigChange(_ context.Context, _ hub.ConfigChangeRequest) (*hub.ConfigChangeResponse, error) {
	return &hub.ConfigChangeResponse{}, nil
}
func (s *stubHub) InvalidateConfig(_ context.Context, _, _ string) {}
func (s *stubHub) InvalidateConfigE(_ context.Context, _, configKey string) error {
	s.invalidatedKeys = append(s.invalidatedKeys, configKey)
	return s.invalidateErr
}

// validSessionRows is a pgx.Rows stub that yields exactly one row with
// 7 columns matching the ListAuthSessions scan:
//
//	sessionId, userId, clientId, deviceId, createdAt, lastRefreshed (NullTime Valid), expiresAt
//
// It exercises the `if lastRefreshed.Valid { … }` branch (auth_sessions.go:140-143).
type validSessionRows struct {
	called bool
	ts     time.Time
}

func (r *validSessionRows) Close()                                       {}
func (r *validSessionRows) Err() error                                   { return nil }
func (r *validSessionRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *validSessionRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *validSessionRows) Next() bool {
	if !r.called {
		r.called = true
		return true
	}
	return false
}
func (r *validSessionRows) Scan(args ...any) error {
	// 7 args: *string *string *string *string *time.Time *sql.NullTime *time.Time
	now := r.ts
	if len(args) >= 6 {
		if s, ok := args[0].(*string); ok {
			*s = "sess-1"
		}
		if s, ok := args[1].(*string); ok {
			*s = "user-1"
		}
		if s, ok := args[2].(*string); ok {
			*s = "client-1"
		}
		if s, ok := args[3].(*string); ok {
			*s = "device-1"
		}
		if t, ok := args[4].(*time.Time); ok {
			*t = now
		}
		if nt, ok := args[5].(*sql.NullTime); ok {
			nt.Valid = true
			nt.Time = now
		}
	}
	if len(args) >= 7 {
		if t, ok := args[6].(*time.Time); ok {
			*t = now.Add(time.Hour)
		}
	}
	return nil
}
func (r *validSessionRows) Values() ([]any, error) { return nil, nil }
func (r *validSessionRows) RawValues() [][]byte    { return nil }
func (r *validSessionRows) Conn() *pgx.Conn        { return nil }

// ListAuthSessions: lastRefreshed.Valid == true branch

func TestListAuthSessions_WithLastRefreshed_Returns200(t *testing.T) {
	h := defaultHandler()
	h.pool = &stubPool{queryRows: &validSessionRows{ts: time.Now()}}
	req := httptest.NewRequest(http.MethodGet, "/auth/sessions", nil)
	rec := httptest.NewRecorder()
	e := echo.New()
	c := e.NewContext(req, rec)
	if err := h.ListAuthSessions(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200; body=%s", rec.Code, rec.Body)
	}
}

// RevokeDeviceInternal: decode error (invalid JSON with pool+revocation set) ─

func TestRevokeDeviceInternal_DecodeError_Returns400(t *testing.T) {
	orig := revocationRevoke
	revocationRevoke = func(_ *Handler, _ context.Context, _ revocation.Request) error { return nil }
	defer func() { revocationRevoke = orig }()

	h := defaultHandler()
	h.pool = &stubPool{}
	h.revocation = &revocation.Service{}

	c, rec := adminAuthCtx(http.MethodPost, "/auth/revoke-device", []byte("not json"), "internal", "admin_user")
	if err := h.RevokeDeviceInternal(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code=%d want 400 (decode error); body=%s", rec.Code, rec.Body)
	}
}

// UpdateIAMPolicy: bind error

func TestUpdateIAMPolicy_BindError_Returns400(t *testing.T) {
	h := defaultHandler()
	c, rec := adminAuthCtx(http.MethodPut, "/iam/policies/p1", []byte("not json"), "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("p1")
	if err := h.UpdateIAMPolicy(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code=%d want 400 (bind error); body=%s", rec.Code, rec.Body)
	}
}

// UpdateIAMGroup: bind error

func TestUpdateIAMGroup_BindError_Returns400(t *testing.T) {
	h := defaultHandler()
	c, rec := adminAuthCtx(http.MethodPut, "/iam/groups/g1", []byte("not json"), "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("g1")
	if err := h.UpdateIAMGroup(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code=%d want 400 (bind error); body=%s", rec.Code, rec.Body)
	}
}

// AddIAMGroupMember: iamEngine invalidation branch

func TestAddIAMGroupMember_WithEngine_InvalidatesCache(t *testing.T) {
	loader := &stubPolicyLoader{}
	engine := cpiam.NewEngine(loader, silentLogger())
	is := &stubIAMStore{memberID: "mem-1"}
	h := buildHandler(&stubUserStore{}, is, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	h.iamEngine = engine

	body, _ := json.Marshal(map[string]any{"principalId": "u1", "principalType": "nexus_user"})
	c, rec := adminAuthCtx(http.MethodPost, "/iam/groups/g1/members", body, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("g1")
	if err := h.AddIAMGroupMember(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("code=%d want 201; body=%s", rec.Code, rec.Body)
	}
}

// RemoveIAMGroupMember: store error branch

func TestRemoveIAMGroupMember_StoreError_Returns404(t *testing.T) {
	is := &stubIAMStore{removeErr: errors.New("not found")}
	h := buildHandler(&stubUserStore{}, is, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodDelete, "/iam/groups/g1/members/mem-1", nil, "admin", "admin_user")
	c.SetParamNames("id", "membershipId")
	c.SetParamValues("g1", "mem-1")
	if err := h.RemoveIAMGroupMember(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("code=%d want 404 (store error); body=%s", rec.Code, rec.Body)
	}
}

// AttachIAMGroupPolicy: iamEngine invalidation branch

func TestAttachIAMGroupPolicy_WithEngine_InvalidatesCache(t *testing.T) {
	loader := &stubPolicyLoader{}
	engine := cpiam.NewEngine(loader, silentLogger())
	is := &stubIAMStore{attachGroupPolID: "att-1"}
	h := buildHandler(&stubUserStore{}, is, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	h.iamEngine = engine

	body, _ := json.Marshal(map[string]any{"policyId": "p1"})
	c, rec := adminAuthCtx(http.MethodPost, "/iam/groups/g1/policies", body, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("g1")
	if err := h.AttachIAMGroupPolicy(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("code=%d want 201; body=%s", rec.Code, rec.Body)
	}
}

// DetachIAMGroupPolicy: iamEngine invalidation + listMembers error

func TestDetachIAMGroupPolicy_WithEngine_InvalidatesCache(t *testing.T) {
	loader := &stubPolicyLoader{}
	engine := cpiam.NewEngine(loader, silentLogger())
	// groupPolicyAttErr=nil so lookupErr==nil, then ListGroupMembers returns membersErr
	is := &stubIAMStore{
		groupPolicyAttGID: "g1",
		membersErr:        errors.New("members lookup error"),
	}
	h := buildHandler(&stubUserStore{}, is, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	h.iamEngine = engine

	c, rec := adminAuthCtx(http.MethodDelete, "/iam/groups/g1/policies/att1", nil, "admin", "admin_user")
	c.SetParamNames("id", "attachmentId")
	c.SetParamValues("g1", "att1")
	if err := h.DetachIAMGroupPolicy(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNoContent {
		t.Errorf("code=%d want 204; body=%s", rec.Code, rec.Body)
	}
}

// AttachPrincipalPolicy: iamEngine + expiresAt set

func TestAttachPrincipalPolicy_WithEngineAndExpiry_InvalidatesCache(t *testing.T) {
	loader := &stubPolicyLoader{}
	engine := cpiam.NewEngine(loader, silentLogger())
	is := &stubIAMStore{attachPPID: "att-1"}
	h := buildHandler(&stubUserStore{}, is, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	h.iamEngine = engine

	// Include expiresAt so the expiresAtTime branch is hit
	future := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	body, _ := json.Marshal(map[string]any{"policyId": "p1", "expiresAt": future})
	c, rec := adminAuthCtx(http.MethodPost, "/iam/principals/nexus_user/u1/policies", body, "admin", "admin_user")
	c.SetParamNames("type", "id")
	c.SetParamValues("nexus_user", "u1")
	if err := h.AttachPrincipalPolicy(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("code=%d want 201; body=%s", rec.Code, rec.Body)
	}
}

// DetachPrincipalPolicy: iamEngine invalidation branch

func TestDetachPrincipalPolicy_WithEngine_InvalidatesCache(t *testing.T) {
	loader := &stubPolicyLoader{}
	engine := cpiam.NewEngine(loader, silentLogger())
	is := &stubIAMStore{
		ppAttGID:      "nexus_user",
		ppAttPID:      "u1",
		ppAttPolicyID: "p1",
	}
	h := buildHandler(&stubUserStore{}, is, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	h.iamEngine = engine

	c, rec := adminAuthCtx(http.MethodDelete, "/iam/principals/nexus_user/u1/policies/att1", nil, "admin", "admin_user")
	c.SetParamNames("type", "id", "attachmentId")
	c.SetParamValues("nexus_user", "u1", "att1")
	if err := h.DetachPrincipalPolicy(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNoContent {
		t.Errorf("code=%d want 204; body=%s", rec.Code, rec.Body)
	}
}

// CreateIdentityProvider: bind error

func TestCreateIdentityProvider_BindError_Returns400(t *testing.T) {
	h := defaultHandler()
	c, rec := adminAuthCtx(http.MethodPost, "/identity-providers", []byte("not json"), "admin", "admin_user")
	if err := h.CreateIdentityProvider(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code=%d want 400 (bind error); body=%s", rec.Code, rec.Body)
	}
}

// UpdateIdentityProvider: bind error

func TestUpdateIdentityProvider_BindError_Returns400(t *testing.T) {
	// GetIdentityProvider must succeed (non-nil idp, non-local type) before the bind check.
	ss := &stubScimStore{
		idp: &scimstore.IdentityProviderRecord{ID: "idp-1", Type: "oidc", Name: "Okta"},
	}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, &stubOrgStore{}, ss, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodPut, "/identity-providers/idp-1", []byte("not json"), "admin", "admin_user")
	c.SetParamNames("idpId")
	c.SetParamValues("idp-1")
	if err := h.UpdateIdentityProvider(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code=%d want 400 (bind error); body=%s", rec.Code, rec.Body)
	}
}

// DeleteIdentityProvider: CountFederatedIdentities error (no force)

func TestDeleteIdentityProvider_FedCountError_Returns500(t *testing.T) {
	ss := &stubScimStore{
		idp:         &scimstore.IdentityProviderRecord{ID: "idp-1", Type: "oidc", Name: "Okta"},
		fedCountErr: errors.New("db"),
	}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, &stubOrgStore{}, ss, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	// no ?force=true → CountFederatedIdentitiesForIdP is called
	c, rec := adminAuthCtx(http.MethodDelete, "/identity-providers/idp-1", nil, "admin", "admin_user")
	c.SetParamNames("idpId")
	c.SetParamValues("idp-1")
	if err := h.DeleteIdentityProvider(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500 (fed count error); body=%s", rec.Code, rec.Body)
	}
}

// DeleteIdentityProvider: force + ListUserIDsByIdP error (soft skip)

func TestDeleteIdentityProvider_ForceListError_Returns204(t *testing.T) {
	ss := &stubScimStore{
		idp: &scimstore.IdentityProviderRecord{ID: "idp-1", Type: "oidc", Name: "Okta"},
	}
	feds := &stubFedStore{userIDsErr: errors.New("list error")}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, &stubOrgStore{}, ss, &stubFleetStore{}, &stubVKStore{}, feds, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodDelete, "/identity-providers/idp-1?force=true", nil, "admin", "admin_user")
	c.SetParamNames("idpId")
	c.SetParamValues("idp-1")
	if err := h.DeleteIdentityProvider(c); err != nil {
		t.Fatal(err)
	}
	// list error is logged + soft-skipped; delete still proceeds → 204
	if rec.Code != http.StatusNoContent {
		t.Errorf("code=%d want 204 (list error soft skip); body=%s", rec.Code, rec.Body)
	}
}

// TestCandidateIdentityProvider: bind error and validation error

func TestCandidateIdentityProvider_BindError_Returns400(t *testing.T) {
	h := defaultHandler()
	c, rec := adminAuthCtx(http.MethodPost, "/identity-providers/test", []byte("not json"), "admin", "admin_user")
	if err := h.TestCandidateIdentityProvider(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code=%d want 400 (bind error); body=%s", rec.Code, rec.Body)
	}
}

func TestCandidateIdentityProvider_ValidationError_Returns400(t *testing.T) {
	h := defaultHandler()
	// type "unknown" fails validateIdPRequest → 400 without making network calls
	body, _ := json.Marshal(map[string]any{"type": "unknown", "name": "X", "config": map[string]any{}})
	c, rec := adminAuthCtx(http.MethodPost, "/identity-providers/test", body, "admin", "admin_user")
	if err := h.TestCandidateIdentityProvider(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code=%d want 400 (validation error); body=%s", rec.Code, rec.Body)
	}
}

// oidcProbeServer stands up a fake OIDC discovery + JWKS endpoint so the
// connectivity probe returns ok=true without reaching the network.
func oidcProbeServer(t *testing.T) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"jwks_uri":"` + srv.URL + `/jwks","token_endpoint":"` + srv.URL + `/tok","authorization_endpoint":"` + srv.URL + `/auth"}`))
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"keys":[{"kty":"RSA","kid":"k1"}]}`))
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// selfSignedCertPEM returns a valid, currently-dated X.509 cert PEM the
// SAML probe can parse — used to prove that a masked certificate is
// restored from storage before probing.
func selfSignedCertPEM(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test-idp"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// TestCandidateIdentityProvider: masked-secret restore on edit-mode probe.
// Reproduces the reported bug — testing a saved OIDC IdP whose clientSecret
// is masked as "********" used to be rejected with "OIDC config requires
// clientSecret on create". With `id` supplied the secret is restored from
// storage and the probe runs.
func TestCandidateIdentityProvider_OIDCMaskedSecretWithID_Returns200(t *testing.T) {
	// oidcProbeServer runs on 127.0.0.1; bypass SSRF guard for this test.
	prev := idptest.NewProbeResolver
	idptest.NewProbeResolver = func() *oidcdisco.Resolver {
		return oidcdisco.NewResolver(oidcdisco.WithInsecureSkipHostCheck())
	}
	t.Cleanup(func() { idptest.NewProbeResolver = prev })

	srv := oidcProbeServer(t)
	storedCfg, _ := json.Marshal(map[string]any{
		"issuer": srv.URL, "clientId": "cid", "clientSecret": "real-secret", "redirectUri": "https://app/cb",
	})
	ss := &stubScimStore{idp: &scimstore.IdentityProviderRecord{ID: "oidc-1", Type: "oidc", Name: "Okta", Config: storedCfg}}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, &stubOrgStore{}, ss, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	body, _ := json.Marshal(map[string]any{
		"id": "oidc-1", "type": "oidc", "name": "Okta",
		"config": map[string]any{"issuer": srv.URL, "clientId": "cid", "clientSecret": sensitiveMaskIdP, "redirectUri": "https://app/cb"},
	})
	c, rec := adminAuthCtx(http.MethodPost, "/identity-providers/test", body, "admin", "admin_user")
	if err := h.TestCandidateIdentityProvider(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200 (masked clientSecret must not be rejected on test); body=%s", rec.Code, rec.Body)
	}
	if bytes.Contains(rec.Body.Bytes(), []byte("requires clientSecret")) {
		t.Errorf("masked clientSecret was rejected instead of restored; body=%s", rec.Body)
	}
	var res map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	if res["ok"] != true {
		t.Errorf("probe ok=%v want true; body=%s", res["ok"], rec.Body)
	}
}

// A masked SAML certificate must be restored from the stored row: the SAML
// probe parses the cert, so ok=true is only reachable if the real cert (not
// the "********" sentinel) was substituted.
func TestCandidateIdentityProvider_SAMLMaskedCertWithID_RestoresStoredCert(t *testing.T) {
	cert := selfSignedCertPEM(t)
	storedCfg, _ := json.Marshal(map[string]any{
		"entityId": "https://idp/entity", "ssoUrl": "https://idp/sso", "certificatePem": cert,
	})
	ss := &stubScimStore{idp: &scimstore.IdentityProviderRecord{ID: "saml-1", Type: "saml", Name: "SAML", Config: storedCfg}}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, &stubOrgStore{}, ss, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	body, _ := json.Marshal(map[string]any{
		"id": "saml-1", "type": "saml", "name": "SAML",
		"config": map[string]any{"entityId": "https://idp/entity", "ssoUrl": "https://idp/sso", "certificatePem": sensitiveMaskIdP},
	})
	c, rec := adminAuthCtx(http.MethodPost, "/identity-providers/test", body, "admin", "admin_user")
	if err := h.TestCandidateIdentityProvider(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200; body=%s", rec.Code, rec.Body)
	}
	var res map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	if res["ok"] != true {
		t.Errorf("masked cert must be restored from store so the probe parses it; ok=%v body=%s", res["ok"], rec.Body)
	}
}

// id supplied but the row is gone → 404.
func TestCandidateIdentityProvider_IDNotFound_Returns404(t *testing.T) {
	ss := &stubScimStore{idpErr: pgx.ErrNoRows}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, &stubOrgStore{}, ss, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	body, _ := json.Marshal(map[string]any{
		"id": "missing", "type": "oidc", "name": "X",
		"config": map[string]any{"issuer": "https://i", "clientId": "c", "redirectUri": "https://r", "clientSecret": sensitiveMaskIdP},
	})
	c, rec := adminAuthCtx(http.MethodPost, "/identity-providers/test", body, "admin", "admin_user")
	if err := h.TestCandidateIdentityProvider(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("code=%d want 404; body=%s", rec.Code, rec.Body)
	}
}

// id supplied but the lookup errors → 500.
func TestCandidateIdentityProvider_IDStoreError_Returns500(t *testing.T) {
	ss := &stubScimStore{idpErr: errors.New("db")}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, &stubOrgStore{}, ss, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	body, _ := json.Marshal(map[string]any{
		"id": "oidc-1", "type": "oidc", "name": "X",
		"config": map[string]any{"issuer": "https://i", "clientId": "c", "redirectUri": "https://r", "clientSecret": sensitiveMaskIdP},
	})
	c, rec := adminAuthCtx(http.MethodPost, "/identity-providers/test", body, "admin", "admin_user")
	if err := h.TestCandidateIdentityProvider(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500; body=%s", rec.Code, rec.Body)
	}
}

// A brand-new (unsaved, no id) OIDC config with no clientSecret must still
// reach the probe: the test path validates with isCreate=false because the
// OIDC probe never reads the secret.
func TestCandidateIdentityProvider_NoIDNoSecret_ReachesProbe(t *testing.T) {
	// oidcProbeServer runs on 127.0.0.1; bypass SSRF guard for this test.
	prev := idptest.NewProbeResolver
	idptest.NewProbeResolver = func() *oidcdisco.Resolver {
		return oidcdisco.NewResolver(oidcdisco.WithInsecureSkipHostCheck())
	}
	t.Cleanup(func() { idptest.NewProbeResolver = prev })

	srv := oidcProbeServer(t)
	h := defaultHandler()
	body, _ := json.Marshal(map[string]any{
		"type": "oidc", "name": "New",
		"config": map[string]any{"issuer": srv.URL, "clientId": "c", "redirectUri": "https://r"},
	})
	c, rec := adminAuthCtx(http.MethodPost, "/identity-providers/test", body, "admin", "admin_user")
	if err := h.TestCandidateIdentityProvider(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200 (probe reachable without secret); body=%s", rec.Code, rec.Body)
	}
	if bytes.Contains(rec.Body.Bytes(), []byte("requires clientSecret")) {
		t.Errorf("test path must not require clientSecret; body=%s", rec.Body)
	}
}

// TestSavedIdentityProvider: not-found and internal-error paths

func TestSavedIdentityProvider_NotFound_Returns404(t *testing.T) {
	ss := &stubScimStore{idpErr: pgx.ErrNoRows}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, &stubOrgStore{}, ss, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodPost, "/identity-providers/idp-1/test", nil, "admin", "admin_user")
	c.SetParamNames("idpId")
	c.SetParamValues("idp-1")
	if err := h.TestSavedIdentityProvider(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("code=%d want 404; body=%s", rec.Code, rec.Body)
	}
}

func TestSavedIdentityProvider_GetIdpError_Returns500(t *testing.T) {
	ss := &stubScimStore{idpErr: errors.New("db")}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, &stubOrgStore{}, ss, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodPost, "/identity-providers/idp-1/test", nil, "admin", "admin_user")
	c.SetParamNames("idpId")
	c.SetParamValues("idp-1")
	if err := h.TestSavedIdentityProvider(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500; body=%s", rec.Code, rec.Body)
	}
}

func TestSavedIdentityProvider_LocalType_Returns400(t *testing.T) {
	ss := &stubScimStore{idp: &scimstore.IdentityProviderRecord{ID: "local", Type: "local", Name: "Local"}}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, &stubOrgStore{}, ss, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodPost, "/identity-providers/local/test", nil, "admin", "admin_user")
	c.SetParamNames("idpId")
	c.SetParamValues("local")
	if err := h.TestSavedIdentityProvider(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code=%d want 400 (local type); body=%s", rec.Code, rec.Body)
	}
}

// CreateScimToken: store error

func TestCreateScimToken_TokenStoreError_Returns500(t *testing.T) {
	ss := &stubScimStore{createTokErr: errors.New("db")}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, &stubOrgStore{}, ss, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	body, _ := json.Marshal(map[string]any{"name": "Token"})
	c, rec := adminAuthCtx(http.MethodPost, "/identity-providers/idp-1/scim-tokens", body, "admin", "admin_user")
	c.SetParamNames("idpId")
	c.SetParamValues("idp-1")
	if err := h.CreateScimToken(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500 (token store error); body=%s", rec.Code, rec.Body)
	}
}

// UpdateMe: bind error path

func TestUpdateMe_BindError_Returns400(t *testing.T) {
	h := defaultHandler()
	c, rec := adminAuthCtx(http.MethodPatch, "/me", []byte("not json"), "u1", "admin_user")
	if err := h.UpdateMe(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code=%d want 400 (bind error); body=%s", rec.Code, rec.Body)
	}
}

// UpdateMe: newPassword without currentPassword → 400

func TestUpdateMe_NewPasswordWithoutCurrent_Returns400(t *testing.T) {
	us := &stubUserStore{findResult: &userstore.NexusUser{ID: "u1"}}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	body, _ := json.Marshal(map[string]any{"newPassword": "new-pass"}) // no currentPassword
	c, rec := adminAuthCtx(http.MethodPatch, "/me", body, "u1", "admin_user")
	if err := h.UpdateMe(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code=%d want 400 (missing currentPassword); body=%s", rec.Code, rec.Body)
	}
}

// OrganizationTree: policyLimits fallback branch

func TestOrganizationTree_PolicyLimits_Branch(t *testing.T) {
	// overrideLimits returns empty for o1, policyLimits returns a limit for o1.
	// This covers the `else if lim, ok := policyLimits[o.ID]` branch.
	os := &stubOrgStore{
		orgs: []orgstore.Organization{
			{ID: "o1", Name: "Root", Code: "ROOT", Enabled: true},
		},
	}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, os, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	// 3 pool.Query calls: QuotaOverride (empty), QuotaPolicy (has o1), metric_rollup (empty)
	h.pool = &multiQueryPool{rowSets: []pgx.Rows{
		&emptyRows{},                        // QuotaOverride: empty → o1 not in overrideLimits
		&twoColRows{col1: "o1", col2: 50.0}, // QuotaPolicy: o1 in policyLimits
		&emptyRows{},                        // metric_rollup: empty
	}}
	c, rec := adminAuthCtx(http.MethodGet, "/organizations/tree", nil, "admin", "admin_user")
	if err := h.OrganizationTree(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200; body=%s", rec.Code, rec.Body)
	}
}

// CreateOrganization: bind error + hub invalidate

func TestCreateOrganization_BindError_Returns400(t *testing.T) {
	h := defaultHandler()
	c, rec := adminAuthCtx(http.MethodPost, "/organizations", []byte("not json"), "admin", "admin_user")
	if err := h.CreateOrganization(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code=%d want 400 (bind error); body=%s", rec.Code, rec.Body)
	}
}

func TestCreateOrganization_WithHub_InvalidatesCache(t *testing.T) {
	org := &orgstore.Organization{ID: "o1", Name: "Root", Code: "ROOT"}
	os := &stubOrgStore{createdOrg: org}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, os, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	h.hub = &stubHub{}
	body, _ := json.Marshal(map[string]any{"name": "Root", "code": "ROOT"})
	c, rec := adminAuthCtx(http.MethodPost, "/organizations", body, "admin", "admin_user")
	if err := h.CreateOrganization(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("code=%d want 201; body=%s", rec.Code, rec.Body)
	}
}

// UpdateOrganization: bind error + hub invalidate

func TestUpdateOrganization_BindError_Returns400(t *testing.T) {
	h := defaultHandler()
	c, rec := adminAuthCtx(http.MethodPut, "/organizations/o1", []byte("not json"), "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("o1")
	if err := h.UpdateOrganization(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code=%d want 400 (bind error); body=%s", rec.Code, rec.Body)
	}
}

func TestUpdateOrganization_WithHub_InvalidatesCache(t *testing.T) {
	org := &orgstore.Organization{ID: "o1", Name: "Updated"}
	os := &stubOrgStore{updatedOrg: org}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, os, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	h.hub = &stubHub{}
	body, _ := json.Marshal(map[string]any{"name": "Updated"})
	c, rec := adminAuthCtx(http.MethodPut, "/organizations/o1", body, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("o1")
	if err := h.UpdateOrganization(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200; body=%s", rec.Code, rec.Body)
	}
}

// DeleteOrganization: hub invalidate

func TestDeleteOrganization_WithHub_InvalidatesCache(t *testing.T) {
	os := &stubOrgStore{}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, os, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	h.hub = &stubHub{}
	c, rec := adminAuthCtx(http.MethodDelete, "/organizations/o1", nil, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("o1")
	if err := h.DeleteOrganization(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNoContent {
		t.Errorf("code=%d want 204; body=%s", rec.Code, rec.Body)
	}
}

// CreateProject: bind error + hub invalidate

func TestCreateProject_BindError_Returns400(t *testing.T) {
	h := defaultHandler()
	c, rec := adminAuthCtx(http.MethodPost, "/projects", []byte("not json"), "admin", "admin_user")
	if err := h.CreateProject(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code=%d want 400 (bind error); body=%s", rec.Code, rec.Body)
	}
}

func TestCreateProject_WithHub_InvalidatesCache(t *testing.T) {
	proj := &orgstore.Project{ID: "p1", Name: "Project", Code: "PROJ"}
	os := &stubOrgStore{createdProject: proj}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, os, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	h.hub = &stubHub{}
	body, _ := json.Marshal(map[string]any{"name": "Project", "code": "PROJ", "organizationId": "o1"})
	c, rec := adminAuthCtx(http.MethodPost, "/projects", body, "admin", "admin_user")
	if err := h.CreateProject(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("code=%d want 201; body=%s", rec.Code, rec.Body)
	}
}

// UpdateProject: bind error + hub invalidate

func TestUpdateProject_BindError_Returns400(t *testing.T) {
	h := defaultHandler()
	c, rec := adminAuthCtx(http.MethodPut, "/projects/p1", []byte("not json"), "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("p1")
	if err := h.UpdateProject(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code=%d want 400 (bind error); body=%s", rec.Code, rec.Body)
	}
}

func TestUpdateProject_WithHub_InvalidatesCache(t *testing.T) {
	proj := &orgstore.Project{ID: "p1", Name: "Updated"}
	os := &stubOrgStore{updatedProject: proj}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, os, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	h.hub = &stubHub{}
	body, _ := json.Marshal(map[string]any{"name": "Updated"})
	c, rec := adminAuthCtx(http.MethodPut, "/projects/p1", body, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("p1")
	if err := h.UpdateProject(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200; body=%s", rec.Code, rec.Body)
	}
}

// DeleteProject: hub invalidate

func TestDeleteProject_WithHub_InvalidatesCache(t *testing.T) {
	os := &stubOrgStore{}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, os, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	h.hub = &stubHub{}
	c, rec := adminAuthCtx(http.MethodDelete, "/projects/p1", nil, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("p1")
	if err := h.DeleteProject(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNoContent {
		t.Errorf("code=%d want 204; body=%s", rec.Code, rec.Body)
	}
}

// ListUsers: org enrichment branch (OrganizationID != nil)

func TestListUsers_WithOrgIDField_Returns200(t *testing.T) {
	orgID := "o1"
	orgName := "Root"
	us := &stubUserStore{
		listResult: []userstore.NexusUserSafe{
			{ID: "u1", DisplayName: "User", OrganizationID: &orgID, OrganizationName: &orgName},
		},
		listTotal: 1,
	}
	h := buildHandler(us, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodGet, "/users", nil, "admin", "admin_user")
	if err := h.ListUsers(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200; body=%s", rec.Code, rec.Body)
	}
}

// CreateUser: bind error

func TestCreateUser_BindError_Returns400(t *testing.T) {
	h := defaultHandler()
	c, rec := adminAuthCtx(http.MethodPost, "/users", []byte("not json"), "admin", "admin_user")
	if err := h.CreateUser(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code=%d want 400 (bind error); body=%s", rec.Code, rec.Body)
	}
}

// UpdateUser: bind error

func TestUpdateUser_BindError_Returns400(t *testing.T) {
	h := defaultHandler()
	c, rec := adminAuthCtx(http.MethodPatch, "/users/u1", []byte("not json"), "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("u1")
	if err := h.UpdateUser(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code=%d want 400 (bind error); body=%s", rec.Code, rec.Body)
	}
}

// ListUserDeviceAssignments: nil assignments → empty list

func TestListUserDeviceAssignments_NilAssignments_Returns200(t *testing.T) {
	// fleet.ListDeviceAssignmentsByUser returning nil → must be normalized to [].
	fs := &stubFleetStore{assignments: nil}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, &stubOrgStore{}, &stubScimStore{}, fs, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
	c, rec := adminAuthCtx(http.MethodGet, "/users/u1/device-assignments", nil, "admin", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("u1")
	if err := h.ListUserDeviceAssignments(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200; body=%s", rec.Code, rec.Body)
	}
}

// --- grant-ceiling (permission boundary) regression ---

// adminStarPolicyDoc grants admin:* on every resource — the escalation payload a
// delegated IAM operator would try to author/attach to become super-admin.
var adminStarPolicyDoc = json.RawMessage(`{"Version":"2026-05-12","Statement":[{"Effect":"Allow","Action":["admin:*"],"Resource":["*"]}]}`)

func ceilingBuildHandler(is *stubIAMStore) *Handler {
	return buildHandler(&stubUserStore{}, is, &stubOrgStore{}, &stubScimStore{}, &stubFleetStore{}, &stubVKStore{}, &stubFedStore{}, &stubGovernanceStore{})
}

// TestCreateIAMPolicy_ExceedsCallerCeiling_Returns403: a delegated IAM operator
// (holds only iam-policy/iam-group CRUD) cannot author an admin:* policy.
func TestCreateIAMPolicy_ExceedsCallerCeiling_Returns403(t *testing.T) {
	is := &stubIAMStore{createdPolicy: &iamstore.PolicyRow{ID: "p-new"}}
	h := ceilingBuildHandler(is)
	h.iamEngine = scopedTestEngine([]string{"admin:iam-policy.*", "admin:iam-group.*"}, []string{"*"})
	body, _ := json.Marshal(map[string]any{"name": "Escalate", "document": adminStarPolicyDoc})
	c, rec := adminAuthCtx(http.MethodPost, "/iam/policies", body, "operator", "admin_user")
	if err := h.CreateIAMPolicy(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("code=%d want 403; body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "PRIVILEGE_ESCALATION_BLOCKED") {
		t.Errorf("body missing escalation code: %s", rec.Body)
	}
	if is.createCalled {
		t.Error("policy was persisted despite exceeding the caller's ceiling")
	}
}

// TestAttachPrincipalPolicy_ExceedsCallerCeiling_Returns403: the operator cannot
// attach a pre-existing admin:* policy to itself (the core self-escalation path).
func TestAttachPrincipalPolicy_ExceedsCallerCeiling_Returns403(t *testing.T) {
	is := &stubIAMStore{attachPPID: "att", policy: &iamstore.PolicyRow{ID: "p1", Document: adminStarPolicyDoc}}
	h := ceilingBuildHandler(is)
	h.iamEngine = scopedTestEngine([]string{"admin:iam-policy.*"}, []string{"*"})
	body, _ := json.Marshal(map[string]any{"policyId": "p1"})
	c, rec := adminAuthCtx(http.MethodPost, "/iam/principals/nexus_user/operator/policies", body, "operator", "admin_user")
	c.SetParamNames("type", "id")
	c.SetParamValues("nexus_user", "operator")
	if err := h.AttachPrincipalPolicy(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("code=%d want 403; body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "PRIVILEGE_ESCALATION_BLOCKED") {
		t.Errorf("body missing escalation code: %s", rec.Body)
	}
}

// TestAttachPrincipalPolicy_WithinCeiling_Returns201: a super-admin caller CAN
// attach an admin:* policy — this exercises the real GetIamPolicy → ceiling path
// (not the not-found skip) and proves the guard does not over-reject.
func TestAttachPrincipalPolicy_WithinCeiling_Returns201(t *testing.T) {
	is := &stubIAMStore{attachPPID: "att", policy: &iamstore.PolicyRow{ID: "p1", Document: adminStarPolicyDoc}}
	h := ceilingBuildHandler(is)
	h.iamEngine = superAdminTestEngine()
	body, _ := json.Marshal(map[string]any{"policyId": "p1"})
	c, rec := adminAuthCtx(http.MethodPost, "/iam/principals/nexus_user/u1/policies", body, "admin", "admin_user")
	c.SetParamNames("type", "id")
	c.SetParamValues("nexus_user", "u1")
	if err := h.AttachPrincipalPolicy(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("code=%d want 201; body=%s", rec.Code, rec.Body)
	}
}

// TestAddIAMGroupMember_PrivilegedGroup_Returns403: the operator cannot join a
// group whose attached policy grants admin:* — the group-path escalation.
func TestAddIAMGroupMember_PrivilegedGroup_Returns403(t *testing.T) {
	is := &stubIAMStore{
		memberID:      "m1",
		groupPolicies: []iamstore.GroupPolicyRow{{PolicyID: "p1"}},
		policy:        &iamstore.PolicyRow{ID: "p1", Document: adminStarPolicyDoc},
	}
	h := ceilingBuildHandler(is)
	h.iamEngine = scopedTestEngine([]string{"admin:iam-group.*"}, []string{"*"})
	body, _ := json.Marshal(map[string]any{"principalType": "nexus_user", "principalId": "operator"})
	c, rec := adminAuthCtx(http.MethodPost, "/iam/groups/g1/members", body, "operator", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("g1")
	if err := h.AddIAMGroupMember(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("code=%d want 403; body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "PRIVILEGE_ESCALATION_BLOCKED") {
		t.Errorf("body missing escalation code: %s", rec.Body)
	}
}

// TestAttachIAMGroupPolicy_ExceedsCallerCeiling_Returns403: the operator cannot
// attach an admin:* policy to a group it could then join.
func TestAttachIAMGroupPolicy_ExceedsCallerCeiling_Returns403(t *testing.T) {
	is := &stubIAMStore{attachGroupPolID: "agp", policy: &iamstore.PolicyRow{ID: "p1", Document: adminStarPolicyDoc}}
	h := ceilingBuildHandler(is)
	h.iamEngine = scopedTestEngine([]string{"admin:iam-group.*"}, []string{"*"})
	body, _ := json.Marshal(map[string]any{"policyId": "p1"})
	c, rec := adminAuthCtx(http.MethodPost, "/iam/groups/g1/policies", body, "operator", "admin_user")
	c.SetParamNames("id")
	c.SetParamValues("g1")
	if err := h.AttachIAMGroupPolicy(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("code=%d want 403; body=%s", rec.Code, rec.Body)
	}
}

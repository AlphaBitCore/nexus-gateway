package scim

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/scim/scimstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/users/iamstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/users/userstore"
)

// test helpers

func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func nowTime() time.Time { return time.Now() }

// makeUser returns a minimal NexusUserSafe for stubs.
func makeUser(id, email, status string) *userstore.NexusUserSafe {
	e := email
	return &userstore.NexusUserSafe{
		ID:          id,
		DisplayName: "Test User " + id,
		Email:       &e,
		Status:      status,
		CreatedAt:   nowTime(),
		UpdatedAt:   nowTime(),
	}
}

func makeGroup(id, name string) *iamstore.GroupRow {
	return &iamstore.GroupRow{
		ID:        id,
		Name:      name,
		CreatedAt: nowTime(),
		UpdatedAt: nowTime(),
	}
}

// validToken is a pre-hashed bearer token for tests.
const rawToken = "test-bearer-token-12345"

var validTokenHash = scimstore.HashScimToken(rawToken)


// stubUserStore satisfies scimUserStore.
type stubUserStore struct {
	users         []userstore.NexusUserSafe
	total         int
	listErr       error
	byEmail       *userstore.NexusUser
	byEmailErr    error
	getSafeResult *userstore.NexusUserSafe
	getSafeErr    error
	createResult  *userstore.NexusUserSafe
	createErr     error
	updateResult  *userstore.NexusUserSafe
	updateErr     error
	// defaultOrgID is returned from FindDefaultOrganizationID. Tests that
	// hit CreateUser must set this so the handler does not 500 on the
	// "no organizations exist" guard.
	defaultOrgID  string
	defaultOrgErr error
}

func (s *stubUserStore) ListNexusUsers(_ context.Context, _ userstore.NexusUserListParams) ([]userstore.NexusUserSafe, int, error) {
	return s.users, s.total, s.listErr
}
func (s *stubUserStore) GetNexusUserSafe(_ context.Context, _ string) (*userstore.NexusUserSafe, error) {
	return s.getSafeResult, s.getSafeErr
}
func (s *stubUserStore) FindNexusUserByEmail(_ context.Context, _ string) (*userstore.NexusUser, error) {
	return s.byEmail, s.byEmailErr
}
func (s *stubUserStore) CreateNexusUser(_ context.Context, _ userstore.CreateNexusUserParams) (*userstore.NexusUserSafe, error) {
	return s.createResult, s.createErr
}
func (s *stubUserStore) UpdateNexusUser(_ context.Context, _ string, _ userstore.UpdateNexusUserParams) (*userstore.NexusUserSafe, error) {
	return s.updateResult, s.updateErr
}
func (s *stubUserStore) FindDefaultOrganizationID(_ context.Context) (string, error) {
	return s.defaultOrgID, s.defaultOrgErr
}

// stubIAMStore satisfies scimIAMStore.
type stubIAMStore struct {
	groups      []iamstore.GroupRow
	listErr     error
	group       *iamstore.GroupRow
	groupErr    error
	updateGroup *iamstore.GroupRow
	deleteErr   error
	membersRaw  []map[string]string
	membersErr  error
}

func (s *stubIAMStore) ListIamGroups(_ context.Context) ([]iamstore.GroupRow, error) {
	return s.groups, s.listErr
}
func (s *stubIAMStore) GetIamGroup(_ context.Context, _ string) (*iamstore.GroupRow, error) {
	return s.group, s.groupErr
}
func (s *stubIAMStore) UpdateIamGroup(_ context.Context, _ string, _ iamstore.UpdateIamGroupParams) (*iamstore.GroupRow, error) {
	return s.updateGroup, nil
}
func (s *stubIAMStore) DeleteIamGroup(_ context.Context, _ string) error {
	return s.deleteErr
}
func (s *stubIAMStore) AddGroupMember(_ context.Context, _, _, _ string) (string, error) {
	return "membership-id", nil
}
func (s *stubIAMStore) RemoveGroupMember(_ context.Context, _ string) error { return nil }
func (s *stubIAMStore) RemoveGroupMemberByPrincipal(_ context.Context, _, _, _ string) error {
	return nil
}
func (s *stubIAMStore) ListGroupMembersRaw(_ context.Context, _ string) ([]map[string]string, error) {
	return s.membersRaw, s.membersErr
}

// stubScimStore satisfies scimTokenStore.
type stubScimStore struct {
	token          *scimstore.ScimToken
	tokenErr       error
	linkErr        error
	findMapping    *scimstore.IdpGroupMapping
	findMappingErr error
	createdGroup   *iamstore.GroupRow
	createGroupErr error
	createdMapping *scimstore.IdpGroupMapping
	createMapErr   error
	groupSrc       string
	groupIdpID     *string
	groupSrcErr    error
}

func (s *stubScimStore) GetScimTokenByHash(_ context.Context, _ string) (*scimstore.ScimToken, error) {
	return s.token, s.tokenErr
}
func (s *stubScimStore) TouchScimToken(_ context.Context, _ string) {}
func (s *stubScimStore) LinkUserToIdP(_ context.Context, _, _, _ string, _ *string) error {
	return s.linkErr
}
func (s *stubScimStore) FindIdpGroupMappingByExternal(_ context.Context, _, _ string) (*scimstore.IdpGroupMapping, error) {
	return s.findMapping, s.findMappingErr
}
func (s *stubScimStore) CreateScimIamGroup(_ context.Context, _ string, _ *string, _, _ string) (*iamstore.GroupRow, error) {
	return s.createdGroup, s.createGroupErr
}
func (s *stubScimStore) CreateIdpGroupMapping(_ context.Context, _ scimstore.CreateIdpGroupMappingParams) (*scimstore.IdpGroupMapping, error) {
	return s.createdMapping, s.createMapErr
}
func (s *stubScimStore) GetIamGroupSource(_ context.Context, _ string) (string, *string, error) {
	return s.groupSrc, s.groupIdpID, s.groupSrcErr
}

// validScimStore returns a scimTokenStore stub that passes authentication.
func validScimStore(idpID *string) *stubScimStore {
	return &stubScimStore{
		token: &scimstore.ScimToken{
			ID:                 "tok-1",
			IdentityProviderID: idpID,
		},
	}
}

// buildHandler returns a Handler wired with the given stubs.
func buildHandler(us scimUserStore, is scimIAMStore, ss scimTokenStore) *Handler {
	return &Handler{
		users:   us,
		iam:     is,
		scim:    ss,
		Logger:  silentLogger(),
		BaseURL: "https://nexus.test/scim/v2",
	}
}

// echoCtx creates an Echo context with the SCIM bearer token pre-set.
func echoCtx(method, path string, body []byte) (echo.Context, *httptest.ResponseRecorder) {
	var reqBody io.Reader
	if body != nil {
		reqBody = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reqBody)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+rawToken)
	rec := httptest.NewRecorder()
	e := echo.New()
	c := e.NewContext(req, rec)
	return c, rec
}

// echoCtxWithToken creates a context with a pre-loaded scimToken value.
func echoCtxWithToken(method, path string, body []byte, tok *scimstore.ScimToken) (echo.Context, *httptest.ResponseRecorder) {
	c, rec := echoCtx(method, path, body)
	c.Set("scimToken", tok)
	return c, rec
}

// scimAuth middleware

func TestScimAuth_MissingBearer_Returns401(t *testing.T) {
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, &stubScimStore{})
	req := httptest.NewRequest(http.MethodGet, "/ServiceProviderConfig", nil)
	rec := httptest.NewRecorder()
	e := echo.New()
	c := e.NewContext(req, rec)

	err := h.scimAuth(func(echo.Context) error { return nil })(c)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("code=%d want 401 (missing bearer)", rec.Code)
	}
}

func TestScimAuth_InvalidToken_Returns401(t *testing.T) {
	ss := &stubScimStore{token: nil} // lookup returns nil → token not found
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, ss)
	req := httptest.NewRequest(http.MethodGet, "/ServiceProviderConfig", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	rec := httptest.NewRecorder()
	e := echo.New()
	c := e.NewContext(req, rec)

	_ = h.scimAuth(func(echo.Context) error { return nil })(c)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("code=%d want 401 (invalid token)", rec.Code)
	}
}

func TestScimAuth_StoreError_Returns401(t *testing.T) {
	ss := &stubScimStore{tokenErr: errors.New("db error")}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, ss)
	req := httptest.NewRequest(http.MethodGet, "/ServiceProviderConfig", nil)
	req.Header.Set("Authorization", "Bearer some-token")
	rec := httptest.NewRecorder()
	e := echo.New()
	c := e.NewContext(req, rec)

	_ = h.scimAuth(func(echo.Context) error { return nil })(c)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("code=%d want 401 (store error)", rec.Code)
	}
}

func TestScimAuth_ValidToken_CallsNext(t *testing.T) {
	ss := validScimStore(nil)
	ss.token = &scimstore.ScimToken{ID: "tok-valid", TokenHash: validTokenHash}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, ss)
	req := httptest.NewRequest(http.MethodGet, "/ServiceProviderConfig", nil)
	req.Header.Set("Authorization", "Bearer "+rawToken)
	rec := httptest.NewRecorder()
	e := echo.New()
	c := e.NewContext(req, rec)

	called := false
	_ = h.scimAuth(func(echo.Context) error { called = true; return nil })(c)
	if !called {
		t.Error("next handler was not called with valid token")
	}
}


func TestServiceProviderConfig_Returns200(t *testing.T) {
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, &stubScimStore{})
	c, rec := echoCtx(http.MethodGet, "/ServiceProviderConfig", nil)
	if err := h.ServiceProviderConfig(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200", rec.Code)
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if _, ok := resp["schemas"]; !ok {
		t.Error("response missing schemas")
	}
}


func TestSchemas_Returns200(t *testing.T) {
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, &stubScimStore{})
	c, rec := echoCtx(http.MethodGet, "/Schemas", nil)
	if err := h.Schemas(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200", rec.Code)
	}
}

func TestSchemaByID_UserSchema_Returns200(t *testing.T) {
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, &stubScimStore{})
	c, rec := echoCtx(http.MethodGet, "/Schemas/"+scimSchemaUser, nil)
	c.SetParamNames("id")
	c.SetParamValues(scimSchemaUser)
	if err := h.SchemaByID(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200", rec.Code)
	}
}

func TestSchemaByID_GroupSchema_Returns200(t *testing.T) {
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, &stubScimStore{})
	c, rec := echoCtx(http.MethodGet, "/Schemas/"+scimSchemaGroup, nil)
	c.SetParamNames("id")
	c.SetParamValues(scimSchemaGroup)
	if err := h.SchemaByID(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200", rec.Code)
	}
}

func TestSchemaByID_Unknown_Returns404(t *testing.T) {
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, &stubScimStore{})
	c, rec := echoCtx(http.MethodGet, "/Schemas/unknown", nil)
	c.SetParamNames("id")
	c.SetParamValues("unknown")
	if err := h.SchemaByID(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("code=%d want 404", rec.Code)
	}
}


func TestListUsers_ReturnsPagedList(t *testing.T) {
	u := makeUser("u1", "u1@example.com", "active")
	us := &stubUserStore{users: []userstore.NexusUserSafe{*u}, total: 1}
	h := buildHandler(us, &stubIAMStore{}, &stubScimStore{})
	c, rec := echoCtx(http.MethodGet, "/Users", nil)
	if err := h.ListUsers(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200", rec.Code)
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["totalResults"].(float64) != 1 {
		t.Errorf("totalResults=%v want 1", resp["totalResults"])
	}
}


func TestGetUser_Found_Returns200(t *testing.T) {
	u := makeUser("u1", "u1@example.com", "active")
	us := &stubUserStore{getSafeResult: u}
	h := buildHandler(us, &stubIAMStore{}, &stubScimStore{})
	c, rec := echoCtx(http.MethodGet, "/Users/u1", nil)
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
	h := buildHandler(us, &stubIAMStore{}, &stubScimStore{})
	c, rec := echoCtx(http.MethodGet, "/Users/unknown", nil)
	c.SetParamNames("id")
	c.SetParamValues("unknown")
	if err := h.GetUser(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("code=%d want 404", rec.Code)
	}
}

func TestGetUser_StoreError_Returns500(t *testing.T) {
	us := &stubUserStore{getSafeErr: errors.New("db error")}
	h := buildHandler(us, &stubIAMStore{}, &stubScimStore{})
	c, rec := echoCtx(http.MethodGet, "/Users/u1", nil)
	c.SetParamNames("id")
	c.SetParamValues("u1")
	if err := h.GetUser(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500", rec.Code)
	}
}


func TestCreateUser_MissingUserName_Returns400(t *testing.T) {
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, &stubScimStore{})
	body, _ := json.Marshal(map[string]any{"userName": ""})
	c, rec := echoCtxWithToken(http.MethodPost, "/Users", body, &scimstore.ScimToken{ID: "t1"})
	if err := h.CreateUser(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code=%d want 400 (missing userName)", rec.Code)
	}
}

func TestCreateUser_DuplicateUser_Returns409(t *testing.T) {
	existing := &userstore.NexusUser{ID: "existing"}
	us := &stubUserStore{byEmail: existing}
	h := buildHandler(us, &stubIAMStore{}, &stubScimStore{})
	body, _ := json.Marshal(map[string]any{"userName": "alice@example.com"})
	c, rec := echoCtxWithToken(http.MethodPost, "/Users", body, &scimstore.ScimToken{ID: "t1"})
	if err := h.CreateUser(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusConflict {
		t.Errorf("code=%d want 409 (duplicate user)", rec.Code)
	}
}

func TestCreateUser_StoreError_Returns500(t *testing.T) {
	us := &stubUserStore{createErr: errors.New("db error"), defaultOrgID: "org-default"}
	h := buildHandler(us, &stubIAMStore{}, &stubScimStore{})
	body, _ := json.Marshal(map[string]any{"userName": "new@example.com"})
	c, rec := echoCtxWithToken(http.MethodPost, "/Users", body, &scimstore.ScimToken{ID: "t1"})
	if err := h.CreateUser(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500 (store error)", rec.Code)
	}
}

func TestCreateUser_Success_Returns201(t *testing.T) {
	created := makeUser("u-new", "new@example.com", "active")
	us := &stubUserStore{createResult: created, defaultOrgID: "org-default"}
	h := buildHandler(us, &stubIAMStore{}, &stubScimStore{})
	body, _ := json.Marshal(map[string]any{"userName": "new@example.com"})
	c, rec := echoCtxWithToken(http.MethodPost, "/Users", body, &scimstore.ScimToken{ID: "t1"})
	if err := h.CreateUser(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("code=%d want 201 (created); body=%s", rec.Code, rec.Body)
	}
}

func TestCreateUser_NoDefaultOrg_Returns500(t *testing.T) {
	// FindDefaultOrganizationID returns "" → no Organization rows;
	// CreateUser must fail closed with 500 instead of attempting an
	// INSERT that would FK-violate.
	us := &stubUserStore{defaultOrgID: ""}
	h := buildHandler(us, &stubIAMStore{}, &stubScimStore{})
	body, _ := json.Marshal(map[string]any{"userName": "lonely@example.com"})
	c, rec := echoCtxWithToken(http.MethodPost, "/Users", body, &scimstore.ScimToken{ID: "t1"})
	if err := h.CreateUser(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500 (no default org)", rec.Code)
	}
}

func TestCreateUser_DefaultOrgLookupError_Returns500(t *testing.T) {
	us := &stubUserStore{defaultOrgErr: errors.New("db down")}
	h := buildHandler(us, &stubIAMStore{}, &stubScimStore{})
	body, _ := json.Marshal(map[string]any{"userName": "broken@example.com"})
	c, rec := echoCtxWithToken(http.MethodPost, "/Users", body, &scimstore.ScimToken{ID: "t1"})
	if err := h.CreateUser(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500 (default org lookup error)", rec.Code)
	}
}

func TestCreateUser_WithIdPToken_LinksUserToIdP(t *testing.T) {
	created := makeUser("u-idp", "idp@example.com", "active")
	us := &stubUserStore{createResult: created, defaultOrgID: "org-default"}
	idpID := "idp-1"
	ss := &stubScimStore{}
	h := buildHandler(us, &stubIAMStore{}, ss)
	body, _ := json.Marshal(map[string]any{
		"userName":   "idp@example.com",
		"externalId": "external-sub-1",
	})
	tok := &scimstore.ScimToken{ID: "t-idp", IdentityProviderID: &idpID}
	c, rec := echoCtxWithToken(http.MethodPost, "/Users", body, tok)
	if err := h.CreateUser(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("code=%d want 201; body=%s", rec.Code, rec.Body)
	}
}


func TestReplaceUser_NotFound_Returns404(t *testing.T) {
	us := &stubUserStore{updateResult: nil}
	h := buildHandler(us, &stubIAMStore{}, &stubScimStore{})
	active := true
	body, _ := json.Marshal(map[string]any{"userName": "u@example.com", "active": active})
	c, rec := echoCtxWithToken(http.MethodPut, "/Users/u1", body, &scimstore.ScimToken{ID: "t1"})
	c.SetParamNames("id")
	c.SetParamValues("u1")
	if err := h.ReplaceUser(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("code=%d want 404 (not found)", rec.Code)
	}
}

func TestReplaceUser_Success_Returns200(t *testing.T) {
	updated := makeUser("u1", "u1@example.com", "active")
	us := &stubUserStore{updateResult: updated}
	h := buildHandler(us, &stubIAMStore{}, &stubScimStore{})
	active := true
	body, _ := json.Marshal(map[string]any{"userName": "u1@example.com", "active": active})
	c, rec := echoCtxWithToken(http.MethodPut, "/Users/u1", body, &scimstore.ScimToken{ID: "t1"})
	c.SetParamNames("id")
	c.SetParamValues("u1")
	if err := h.ReplaceUser(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200", rec.Code)
	}
}

func TestReplaceUser_StoreError_Returns500(t *testing.T) {
	us := &stubUserStore{updateErr: errors.New("db")}
	h := buildHandler(us, &stubIAMStore{}, &stubScimStore{})
	body, _ := json.Marshal(map[string]any{"userName": "u@example.com"})
	c, rec := echoCtxWithToken(http.MethodPut, "/Users/u1", body, &scimstore.ScimToken{ID: "t1"})
	c.SetParamNames("id")
	c.SetParamValues("u1")
	if err := h.ReplaceUser(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500", rec.Code)
	}
}


func TestPatchUser_ActiveFalse_SuspendsUser(t *testing.T) {
	updated := makeUser("u1", "u1@example.com", "suspended")
	us := &stubUserStore{updateResult: updated}
	h := buildHandler(us, &stubIAMStore{}, &stubScimStore{})
	body, _ := json.Marshal(map[string]any{
		"schemas": []string{scimSchemaPatch},
		"Operations": []map[string]any{
			{"op": "replace", "path": "active", "value": false},
		},
	})
	c, rec := echoCtxWithToken(http.MethodPatch, "/Users/u1", body, &scimstore.ScimToken{ID: "t1"})
	c.SetParamNames("id")
	c.SetParamValues("u1")
	if err := h.PatchUser(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200", rec.Code)
	}
}

func TestPatchUser_NotFound_Returns404(t *testing.T) {
	us := &stubUserStore{updateResult: nil}
	h := buildHandler(us, &stubIAMStore{}, &stubScimStore{})
	body, _ := json.Marshal(map[string]any{
		"schemas":    []string{scimSchemaPatch},
		"Operations": []map[string]any{},
	})
	c, rec := echoCtxWithToken(http.MethodPatch, "/Users/missing", body, &scimstore.ScimToken{ID: "t1"})
	c.SetParamNames("id")
	c.SetParamValues("missing")
	if err := h.PatchUser(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("code=%d want 404", rec.Code)
	}
}


func TestDeleteUser_Success_Returns204(t *testing.T) {
	updated := makeUser("u1", "u1@example.com", "suspended")
	us := &stubUserStore{updateResult: updated}
	h := buildHandler(us, &stubIAMStore{}, &stubScimStore{})
	c, rec := echoCtxWithToken(http.MethodDelete, "/Users/u1", nil, &scimstore.ScimToken{ID: "t1"})
	c.SetParamNames("id")
	c.SetParamValues("u1")
	if err := h.DeleteUser(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNoContent {
		t.Errorf("code=%d want 204", rec.Code)
	}
}

func TestDeleteUser_StoreError_Returns500(t *testing.T) {
	us := &stubUserStore{updateErr: errors.New("db error")}
	h := buildHandler(us, &stubIAMStore{}, &stubScimStore{})
	c, rec := echoCtxWithToken(http.MethodDelete, "/Users/u1", nil, &scimstore.ScimToken{ID: "t1"})
	c.SetParamNames("id")
	c.SetParamValues("u1")
	if err := h.DeleteUser(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500", rec.Code)
	}
}


func TestListGroups_ReturnsGroups(t *testing.T) {
	g := makeGroup("g1", "Engineering")
	is := &stubIAMStore{groups: []iamstore.GroupRow{*g}}
	h := buildHandler(&stubUserStore{}, is, &stubScimStore{})
	c, rec := echoCtx(http.MethodGet, "/Groups", nil)
	if err := h.ListGroups(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200", rec.Code)
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["totalResults"].(float64) != 1 {
		t.Errorf("totalResults=%v want 1", resp["totalResults"])
	}
}


func TestGetGroup_Found_Returns200(t *testing.T) {
	g := makeGroup("g1", "Engineering")
	is := &stubIAMStore{group: g}
	h := buildHandler(&stubUserStore{}, is, &stubScimStore{})
	c, rec := echoCtx(http.MethodGet, "/Groups/g1", nil)
	c.SetParamNames("id")
	c.SetParamValues("g1")
	if err := h.GetGroup(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200", rec.Code)
	}
}

func TestGetGroup_NotFound_Returns404(t *testing.T) {
	is := &stubIAMStore{group: nil, groupErr: errors.New("not found")}
	h := buildHandler(&stubUserStore{}, is, &stubScimStore{})
	c, rec := echoCtx(http.MethodGet, "/Groups/missing", nil)
	c.SetParamNames("id")
	c.SetParamValues("missing")
	if err := h.GetGroup(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("code=%d want 404", rec.Code)
	}
}


func TestCreateGroup_MissingDisplayName_Returns400(t *testing.T) {
	idpID := "idp-1"
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, &stubScimStore{})
	body, _ := json.Marshal(map[string]any{"displayName": ""})
	tok := &scimstore.ScimToken{ID: "t1", IdentityProviderID: &idpID}
	c, rec := echoCtxWithToken(http.MethodPost, "/Groups", body, tok)
	if err := h.CreateGroup(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code=%d want 400 (missing displayName)", rec.Code)
	}
}

func TestCreateGroup_UnscopedToken_Returns400(t *testing.T) {
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, &stubScimStore{})
	body, _ := json.Marshal(map[string]any{"displayName": "MyGroup"})
	// No IdentityProviderID → returns 400
	tok := &scimstore.ScimToken{ID: "t1"}
	c, rec := echoCtxWithToken(http.MethodPost, "/Groups", body, tok)
	if err := h.CreateGroup(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code=%d want 400 (unscoped token)", rec.Code)
	}
}

func TestCreateGroup_ExistingMapping_ReusesGroup(t *testing.T) {
	idpID := "idp-1"
	mapping := &scimstore.IdpGroupMapping{IamGroupID: "g-existing"}
	g := makeGroup("g-existing", "Existing Group")
	is := &stubIAMStore{group: g}
	ss := &stubScimStore{findMapping: mapping, createdGroup: g}
	h := buildHandler(&stubUserStore{}, is, ss)
	body, _ := json.Marshal(map[string]any{"displayName": "MyGroup", "externalId": "ext-1"})
	tok := &scimstore.ScimToken{ID: "t1", IdentityProviderID: &idpID}
	c, rec := echoCtxWithToken(http.MethodPost, "/Groups", body, tok)
	if err := h.CreateGroup(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("code=%d want 201 (mapped group reuse); body=%s", rec.Code, rec.Body)
	}
}

func TestCreateGroup_NewGroup_Returns201(t *testing.T) {
	idpID := "idp-1"
	g := makeGroup("g-new", "New Group")
	ss := &stubScimStore{findMapping: nil, createdGroup: g}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, ss)
	body, _ := json.Marshal(map[string]any{"displayName": "New Group"})
	tok := &scimstore.ScimToken{ID: "t1", IdentityProviderID: &idpID}
	c, rec := echoCtxWithToken(http.MethodPost, "/Groups", body, tok)
	if err := h.CreateGroup(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("code=%d want 201; body=%s", rec.Code, rec.Body)
	}
}

func TestCreateGroup_CreateGroupError_Returns500(t *testing.T) {
	idpID := "idp-1"
	ss := &stubScimStore{createGroupErr: errors.New("db error")}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, ss)
	body, _ := json.Marshal(map[string]any{"displayName": "Bad Group"})
	tok := &scimstore.ScimToken{ID: "t1", IdentityProviderID: &idpID}
	c, rec := echoCtxWithToken(http.MethodPost, "/Groups", body, tok)
	if err := h.CreateGroup(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500", rec.Code)
	}
}


func TestReplaceGroup_NonScimGroup_Returns403(t *testing.T) {
	ss := &stubScimStore{groupSrc: "admin"} // not "scim"
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, ss)
	body, _ := json.Marshal(map[string]any{"displayName": "Updated"})
	tok := &scimstore.ScimToken{ID: "t1"}
	c, rec := echoCtxWithToken(http.MethodPut, "/Groups/g1", body, tok)
	c.SetParamNames("id")
	c.SetParamValues("g1")
	if err := h.ReplaceGroup(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("code=%d want 403 (admin-managed group)", rec.Code)
	}
}

func TestReplaceGroup_NotFound_Returns404(t *testing.T) {
	ss := &stubScimStore{groupSrc: ""} // empty source → not found
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, ss)
	body, _ := json.Marshal(map[string]any{"displayName": "Updated"})
	tok := &scimstore.ScimToken{ID: "t1"}
	c, rec := echoCtxWithToken(http.MethodPut, "/Groups/missing", body, tok)
	c.SetParamNames("id")
	c.SetParamValues("missing")
	if err := h.ReplaceGroup(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("code=%d want 404", rec.Code)
	}
}

func TestReplaceGroup_ScimGroup_Returns200(t *testing.T) {
	idpID := "idp-1"
	g := makeGroup("g1", "Updated Group")
	ss := &stubScimStore{groupSrc: "scim", groupIdpID: &idpID}
	is := &stubIAMStore{updateGroup: g}
	h := buildHandler(&stubUserStore{}, is, ss)
	body, _ := json.Marshal(map[string]any{"displayName": "Updated Group"})
	tok := &scimstore.ScimToken{ID: "t1", IdentityProviderID: &idpID}
	c, rec := echoCtxWithToken(http.MethodPut, "/Groups/g1", body, tok)
	c.SetParamNames("id")
	c.SetParamValues("g1")
	if err := h.ReplaceGroup(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200; body=%s", rec.Code, rec.Body)
	}
}


func TestPatchGroup_AddMember(t *testing.T) {
	idpID := "idp-1"
	g := makeGroup("g1", "Engineering")
	ss := &stubScimStore{groupSrc: "scim", groupIdpID: &idpID}
	is := &stubIAMStore{group: g}
	h := buildHandler(&stubUserStore{}, is, ss)
	body, _ := json.Marshal(map[string]any{
		"schemas": []string{scimSchemaPatch},
		"Operations": []map[string]any{
			{"op": "add", "path": "members", "value": []map[string]any{{"value": "user-1"}}},
		},
	})
	tok := &scimstore.ScimToken{ID: "t1", IdentityProviderID: &idpID}
	c, rec := echoCtxWithToken(http.MethodPatch, "/Groups/g1", body, tok)
	c.SetParamNames("id")
	c.SetParamValues("g1")
	if err := h.PatchGroup(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200", rec.Code)
	}
}

func TestPatchGroup_RemoveMember(t *testing.T) {
	idpID := "idp-1"
	g := makeGroup("g1", "Engineering")
	ss := &stubScimStore{groupSrc: "scim", groupIdpID: &idpID}
	is := &stubIAMStore{group: g}
	h := buildHandler(&stubUserStore{}, is, ss)
	body, _ := json.Marshal(map[string]any{
		"schemas": []string{scimSchemaPatch},
		"Operations": []map[string]any{
			{"op": "remove", "path": `members[value eq "user-1"]`},
		},
	})
	tok := &scimstore.ScimToken{ID: "t1", IdentityProviderID: &idpID}
	c, rec := echoCtxWithToken(http.MethodPatch, "/Groups/g1", body, tok)
	c.SetParamNames("id")
	c.SetParamValues("g1")
	if err := h.PatchGroup(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200", rec.Code)
	}
}

func TestPatchGroup_ReplaceDisplayName(t *testing.T) {
	idpID := "idp-1"
	g := makeGroup("g1", "New Name")
	ss := &stubScimStore{groupSrc: "scim", groupIdpID: &idpID}
	is := &stubIAMStore{group: g, updateGroup: g}
	h := buildHandler(&stubUserStore{}, is, ss)
	body, _ := json.Marshal(map[string]any{
		"schemas": []string{scimSchemaPatch},
		"Operations": []map[string]any{
			{"op": "replace", "path": "displayname", "value": "New Name"},
		},
	})
	tok := &scimstore.ScimToken{ID: "t1", IdentityProviderID: &idpID}
	c, rec := echoCtxWithToken(http.MethodPatch, "/Groups/g1", body, tok)
	c.SetParamNames("id")
	c.SetParamValues("g1")
	if err := h.PatchGroup(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200", rec.Code)
	}
}


func TestDeleteGroup_Success_Returns204(t *testing.T) {
	is := &stubIAMStore{deleteErr: nil}
	h := buildHandler(&stubUserStore{}, is, &stubScimStore{})
	c, rec := echoCtx(http.MethodDelete, "/Groups/g1", nil)
	c.SetParamNames("id")
	c.SetParamValues("g1")
	if err := h.DeleteGroup(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNoContent {
		t.Errorf("code=%d want 204", rec.Code)
	}
}

func TestDeleteGroup_Error_Returns404(t *testing.T) {
	is := &stubIAMStore{deleteErr: errors.New("not found")}
	h := buildHandler(&stubUserStore{}, is, &stubScimStore{})
	c, rec := echoCtx(http.MethodDelete, "/Groups/missing", nil)
	c.SetParamNames("id")
	c.SetParamValues("missing")
	if err := h.DeleteGroup(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("code=%d want 404", rec.Code)
	}
}

// patch helpers

func TestApplyUserPatchOp_Active(t *testing.T) {
	var p userstore.UpdateNexusUserParams
	applyUserPatchOp(&p, "active", true)
	if p.Status == nil || *p.Status != "active" {
		t.Errorf("status=%v want 'active'", p.Status)
	}
	applyUserPatchOp(&p, "active", false)
	if p.Status == nil || *p.Status != "suspended" {
		t.Errorf("status=%v want 'suspended'", p.Status)
	}
}

func TestApplyUserPatchOp_DisplayName(t *testing.T) {
	var p userstore.UpdateNexusUserParams
	applyUserPatchOp(&p, "displayname", "New Name")
	if p.DisplayName == nil || *p.DisplayName != "New Name" {
		t.Errorf("displayName=%v want 'New Name'", p.DisplayName)
	}
}

func TestApplyUserPatchOp_Email(t *testing.T) {
	var p userstore.UpdateNexusUserParams
	applyUserPatchOp(&p, "username", "new@example.com")
	if p.Email == nil || *p.Email != "new@example.com" {
		t.Errorf("email=%v want 'new@example.com'", p.Email)
	}
}

func TestParseBoolValue(t *testing.T) {
	for _, tc := range []struct {
		in   any
		want bool
	}{
		{true, true},
		{false, false},
		{"true", true},
		{"false", false},
		{"TRUE", true},
		{42, false},
	} {
		got := parseBoolValue(tc.in)
		if got != tc.want {
			t.Errorf("parseBoolValue(%v)=%v want %v", tc.in, got, tc.want)
		}
	}
}

func TestParseMembersValue(t *testing.T) {
	v := []any{
		map[string]any{"value": "user-1"},
		map[string]any{"value": "user-2"},
		"not-a-map",
	}
	got := parseMembersValue(v)
	if len(got) != 2 {
		t.Fatalf("len=%d want 2", len(got))
	}
	if got[0] != "user-1" || got[1] != "user-2" {
		t.Errorf("unexpected values: %v", got)
	}
}

func TestParseMemberFilterValue(t *testing.T) {
	path := `members[value eq "user-abc"]`
	got := parseMemberFilterValue(path)
	if got != "user-abc" {
		t.Errorf("got %q want 'user-abc'", got)
	}
	// No quotes → empty
	if v := parseMemberFilterValue("members"); v != "" {
		t.Errorf("parseMemberFilterValue(no-quotes)=%q want ''", v)
	}
}

func TestQueryInt(t *testing.T) {
	for _, tc := range []struct{ input, def, want int }{
		{0, 100, 100},  // empty (default)
		{50, 100, 50},  // explicit valid
		{-1, 100, 100}, // negative → default
	} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		e := echo.New()
		c := e.NewContext(req, httptest.NewRecorder())
		if tc.input > 0 {
			req.URL.RawQuery = "count=" + string(rune('0'+tc.input/10)) + string(rune('0'+tc.input%10))
		}
		got := queryInt(c, "count", tc.def)
		_ = got // just checking it doesn't panic
	}
}

// Note: scimError calls c.JSON() which writes the response and returns nil.
// So assertScimGroup returns a non-nil errResp only on unexpected errors; for
// SCIM-protocol errors the response is written to the recorder and errResp is
// nil. Tests must inspect rec.Code to verify the correct status was written.

func TestAssertScimGroup_NotFoundSrc_Returns404(t *testing.T) {
	ss := &stubScimStore{groupSrc: ""}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, ss)
	c, rec := echoCtx(http.MethodPut, "/Groups/g1", nil)
	tok := &scimstore.ScimToken{ID: "t1"}
	src, _, _ := h.assertScimGroup(c, c.Request().Context(), "g1", tok)
	// group not found → 404 written to recorder and empty source returned
	if src != "" {
		t.Errorf("expected empty source on not-found, got %q", src)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rec.Code)
	}
}

func TestAssertScimGroup_AdminManagedSource_Returns403(t *testing.T) {
	ss := &stubScimStore{groupSrc: "admin"}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, ss)
	c, rec := echoCtx(http.MethodPut, "/Groups/g1", nil)
	tok := &scimstore.ScimToken{ID: "t1"}
	src, _, _ := h.assertScimGroup(c, c.Request().Context(), "g1", tok)
	// admin-managed → 403 written to recorder
	if src != "" {
		t.Errorf("expected empty source on admin-managed, got %q", src)
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d", rec.Code)
	}
}

func TestAssertScimGroup_WrongIdP_Returns403(t *testing.T) {
	idpA := "idp-a"
	idpB := "idp-b"
	ss := &stubScimStore{groupSrc: "scim", groupIdpID: &idpA}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, ss)
	c, rec := echoCtx(http.MethodPut, "/Groups/g1", nil)
	tok := &scimstore.ScimToken{ID: "t1", IdentityProviderID: &idpB}
	src, _, _ := h.assertScimGroup(c, c.Request().Context(), "g1", tok)
	// wrong IdP → 403 written to recorder
	if src != "" {
		t.Errorf("expected empty source on wrong-idp, got %q", src)
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d", rec.Code)
	}
}

func TestAssertScimGroup_StoreError_Returns500(t *testing.T) {
	ss := &stubScimStore{groupSrcErr: errors.New("db error")}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, ss)
	c, rec := echoCtx(http.MethodPut, "/Groups/g1", nil)
	tok := &scimstore.ScimToken{ID: "t1"}
	_, _, err := h.assertScimGroup(c, c.Request().Context(), "g1", tok)
	// Store error → scimError writes 500, returns nil; errResp is nil too
	_ = err
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 on store error, got %d", rec.Code)
	}
}

// New / RegisterSCIMRoutes

// stubPool satisfies userstore.PgxPool (and therefore scimPool) with no-op methods.
// Allows testing the New(pool != nil) branch without a real DB connection.
type stubPool struct{}

func (stubPool) Begin(_ context.Context) (pgx.Tx, error) { return nil, nil }
func (stubPool) Exec(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}
func (stubPool) Query(_ context.Context, _ string, _ ...any) (pgx.Rows, error) { return nil, nil }
func (stubPool) QueryRow(_ context.Context, _ string, _ ...any) pgx.Row        { return nil }

func TestNew_NilPool_ReturnsHandler(t *testing.T) {
	h := New(nil, silentLogger(), "https://nexus.test/scim/v2")
	if h == nil {
		t.Fatal("expected non-nil handler from New(nil pool)")
	}
	// With nil pool, stores are nil — the zero handler should not panic on init.
	if h.Logger == nil {
		t.Error("logger should not be nil")
	}
}

func TestNew_WithPool_ConstructsStores(t *testing.T) {
	// Passes a non-nil stub pool to exercise the pool != nil branch in New().
	h := New(stubPool{}, silentLogger(), "https://nexus.test/scim/v2")
	if h == nil {
		t.Fatal("expected non-nil handler from New(pool)")
	}
	if h.users == nil || h.iam == nil || h.scim == nil {
		t.Error("expected stores to be initialized with non-nil pool")
	}
}

func TestRegisterSCIMRoutes_MountsRoutes(t *testing.T) {
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, &stubScimStore{})
	e := echo.New()
	g := e.Group("/scim/v2")
	// Should not panic.
	h.RegisterSCIMRoutes(g)
	// Verify routes were registered by checking route count.
	if len(e.Routes()) == 0 {
		t.Error("RegisterSCIMRoutes did not register any routes")
	}
}

// Additional branch coverage

func TestListUsers_Error_Returns500(t *testing.T) {
	us := &stubUserStore{listErr: errors.New("db error")}
	h := buildHandler(us, &stubIAMStore{}, &stubScimStore{})
	c, rec := echoCtx(http.MethodGet, "/Users", nil)
	if err := h.ListUsers(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500", rec.Code)
	}
}

func TestListUsers_CountCapAt200(t *testing.T) {
	// count > 200 should be capped; no error expected.
	us := &stubUserStore{users: nil, total: 0}
	h := buildHandler(us, &stubIAMStore{}, &stubScimStore{})
	req := httptest.NewRequest(http.MethodGet, "/Users?count=500&startIndex=2", nil)
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

func TestListUsers_FilterByUserName(t *testing.T) {
	u := makeUser("u1", "alice@example.com", "active")
	us := &stubUserStore{users: []userstore.NexusUserSafe{*u}, total: 1}
	h := buildHandler(us, &stubIAMStore{}, &stubScimStore{})
	req := httptest.NewRequest(http.MethodGet, `/Users?filter=userName+eq+"alice@example.com"`, nil)
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

func TestListGroups_Error_Returns500(t *testing.T) {
	is := &stubIAMStore{listErr: errors.New("db error")}
	h := buildHandler(&stubUserStore{}, is, &stubScimStore{})
	c, rec := echoCtx(http.MethodGet, "/Groups", nil)
	if err := h.ListGroups(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500", rec.Code)
	}
}

func TestCreateUser_WithIdPToken_NoExternalID_UsesUserName(t *testing.T) {
	// When tok has IdP but body.ExternalID == "", fallback to body.UserName.
	created := makeUser("u-no-ext", "noext@example.com", "active")
	us := &stubUserStore{createResult: created, defaultOrgID: "org-default"}
	idpID := "idp-1"
	h := buildHandler(us, &stubIAMStore{}, validScimStore(&idpID))
	body, _ := json.Marshal(map[string]any{
		"userName":   "noext@example.com",
		"externalId": "", // empty → fallback to userName
	})
	tok := &scimstore.ScimToken{ID: "t-idp", IdentityProviderID: &idpID}
	c, rec := echoCtxWithToken(http.MethodPost, "/Users", body, tok)
	if err := h.CreateUser(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("code=%d want 201", rec.Code)
	}
}

func TestReplaceUser_ActiveNilTreatedAsActive(t *testing.T) {
	// active=nil → treated as true, status="active"
	updated := makeUser("u1", "u1@example.com", "active")
	us := &stubUserStore{updateResult: updated}
	h := buildHandler(us, &stubIAMStore{}, &stubScimStore{})
	// No "active" field in the body → body.Active == nil
	body, _ := json.Marshal(map[string]any{"userName": "u1@example.com"})
	c, rec := echoCtxWithToken(http.MethodPut, "/Users/u1", body, &scimstore.ScimToken{ID: "t1"})
	c.SetParamNames("id")
	c.SetParamValues("u1")
	if err := h.ReplaceUser(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200 (active=nil treated as true)", rec.Code)
	}
}

func TestReplaceUser_ActiveFalse_Suspends(t *testing.T) {
	updated := makeUser("u1", "u1@example.com", "suspended")
	us := &stubUserStore{updateResult: updated}
	h := buildHandler(us, &stubIAMStore{}, &stubScimStore{})
	activeFalse := false
	body, _ := json.Marshal(map[string]any{"userName": "u1@example.com", "active": activeFalse})
	c, rec := echoCtxWithToken(http.MethodPut, "/Users/u1", body, &scimstore.ScimToken{ID: "t1"})
	c.SetParamNames("id")
	c.SetParamValues("u1")
	if err := h.ReplaceUser(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200", rec.Code)
	}
}

func TestPatchUser_AddOp_AppliesUpdate(t *testing.T) {
	updated := makeUser("u1", "u1@example.com", "active")
	us := &stubUserStore{updateResult: updated}
	h := buildHandler(us, &stubIAMStore{}, &stubScimStore{})
	body, _ := json.Marshal(map[string]any{
		"schemas": []string{scimSchemaPatch},
		"Operations": []map[string]any{
			{"op": "add", "path": "displayname", "value": "New Name"},
		},
	})
	c, rec := echoCtxWithToken(http.MethodPatch, "/Users/u1", body, &scimstore.ScimToken{ID: "t1"})
	c.SetParamNames("id")
	c.SetParamValues("u1")
	if err := h.PatchUser(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200", rec.Code)
	}
}

func TestPatchUser_StoreError_Returns500(t *testing.T) {
	us := &stubUserStore{updateErr: errors.New("db error")}
	h := buildHandler(us, &stubIAMStore{}, &stubScimStore{})
	body, _ := json.Marshal(map[string]any{
		"schemas":    []string{scimSchemaPatch},
		"Operations": []map[string]any{},
	})
	c, rec := echoCtxWithToken(http.MethodPatch, "/Users/u1", body, &scimstore.ScimToken{ID: "t1"})
	c.SetParamNames("id")
	c.SetParamValues("u1")
	if err := h.PatchUser(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500", rec.Code)
	}
}

func TestCreateGroup_MappingLookupError_Returns500(t *testing.T) {
	idpID := "idp-1"
	ss := &stubScimStore{findMappingErr: errors.New("db error")}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, ss)
	body, _ := json.Marshal(map[string]any{"displayName": "MyGroup", "externalId": "ext-1"})
	tok := &scimstore.ScimToken{ID: "t1", IdentityProviderID: &idpID}
	c, rec := echoCtxWithToken(http.MethodPost, "/Groups", body, tok)
	if err := h.CreateGroup(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500", rec.Code)
	}
}

func TestCreateGroup_NewGroupWithExternalID_AutoBackfills(t *testing.T) {
	// ExternalID set but no existing mapping → creates group + mapping.
	idpID := "idp-1"
	g := makeGroup("g-auto", "Auto Group")
	ss := &stubScimStore{findMapping: nil, createdGroup: g}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, ss)
	body, _ := json.Marshal(map[string]any{"displayName": "Auto Group", "externalId": "ext-auto"})
	tok := &scimstore.ScimToken{ID: "t1", IdentityProviderID: &idpID}
	c, rec := echoCtxWithToken(http.MethodPost, "/Groups", body, tok)
	if err := h.CreateGroup(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("code=%d want 201", rec.Code)
	}
}

func TestReplaceGroup_WithMembers_ReplacesAll(t *testing.T) {
	idpID := "idp-1"
	g := makeGroup("g1", "Engineering")
	existingMembers := []map[string]string{{"membershipId": "m1", "userId": "u1"}}
	ss := &stubScimStore{groupSrc: "scim", groupIdpID: &idpID}
	is := &stubIAMStore{updateGroup: g, membersRaw: existingMembers}
	h := buildHandler(&stubUserStore{}, is, ss)
	body, _ := json.Marshal(map[string]any{
		"displayName": "Engineering",
		"members": []map[string]any{
			{"value": "u2"},
		},
	})
	tok := &scimstore.ScimToken{ID: "t1", IdentityProviderID: &idpID}
	c, rec := echoCtxWithToken(http.MethodPut, "/Groups/g1", body, tok)
	c.SetParamNames("id")
	c.SetParamValues("g1")
	if err := h.ReplaceGroup(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200", rec.Code)
	}
}

func TestReplaceGroup_UpdateGroupError_Returns404(t *testing.T) {
	idpID := "idp-1"
	ss := &stubScimStore{groupSrc: "scim", groupIdpID: &idpID}
	// updateGroup nil + no err → treated as not found
	is := &stubIAMStore{updateGroup: nil}
	h := buildHandler(&stubUserStore{}, is, ss)
	body, _ := json.Marshal(map[string]any{"displayName": "Updated"})
	tok := &scimstore.ScimToken{ID: "t1", IdentityProviderID: &idpID}
	c, rec := echoCtxWithToken(http.MethodPut, "/Groups/g1", body, tok)
	c.SetParamNames("id")
	c.SetParamValues("g1")
	if err := h.ReplaceGroup(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("code=%d want 404 (update returned nil group)", rec.Code)
	}
}

func TestPatchGroup_GetGroupError_Returns404(t *testing.T) {
	idpID := "idp-1"
	ss := &stubScimStore{groupSrc: "scim", groupIdpID: &idpID}
	is := &stubIAMStore{group: nil, groupErr: errors.New("not found")}
	h := buildHandler(&stubUserStore{}, is, ss)
	body, _ := json.Marshal(map[string]any{
		"schemas":    []string{scimSchemaPatch},
		"Operations": []map[string]any{},
	})
	tok := &scimstore.ScimToken{ID: "t1", IdentityProviderID: &idpID}
	c, rec := echoCtxWithToken(http.MethodPatch, "/Groups/g1", body, tok)
	c.SetParamNames("id")
	c.SetParamValues("g1")
	if err := h.PatchGroup(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("code=%d want 404", rec.Code)
	}
}

func TestGroupToSCIM_WithMembers(t *testing.T) {
	// groupToSCIM with non-empty members exercises the loop body.
	g := makeGroup("g1", "Engineering")
	members := []map[string]string{
		{"userId": "u1", "displayName": "Alice"},
		{"userId": "u2", "displayName": "Bob"},
	}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, &stubScimStore{})
	result := h.groupToSCIM(g, members)
	scimMems, ok := result["members"].([]map[string]any)
	if !ok {
		t.Fatal("members field is not []map[string]any")
	}
	if len(scimMems) != 2 {
		t.Errorf("expected 2 members, got %d", len(scimMems))
	}
}

func TestQueryInt_NegativeValue_ReturnsDefault(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/?count=-5", nil)
	e := echo.New()
	c := e.NewContext(req, httptest.NewRecorder())
	got := queryInt(c, "count", 100)
	if got != 100 {
		t.Errorf("queryInt with negative value=%d, want 100 (default)", got)
	}
}

func TestQueryInt_InvalidString_ReturnsDefault(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/?count=abc", nil)
	e := echo.New()
	c := e.NewContext(req, httptest.NewRecorder())
	got := queryInt(c, "count", 50)
	if got != 50 {
		t.Errorf("queryInt with invalid string=%d, want 50 (default)", got)
	}
}

// gap-closure: bindSCIMBody empty / invalid body paths

// TestBindSCIMBody_EmptyBody asserts the "empty body" guard kicks before the
// JSON decoder is touched. Echo gives us a nil Body when nothing is sent.
func TestBindSCIMBody_EmptyBody(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/Users", nil)
	req.Header.Set("Content-Type", "application/scim+json")
	req.Body = nil // explicit nil body
	rec := httptest.NewRecorder()
	e := echo.New()
	c := e.NewContext(req, rec)
	var dst map[string]any
	if err := bindSCIMBody(c, &dst); err == nil {
		t.Fatal("bindSCIMBody on nil body should error, got nil")
	}
}

func TestCreateUser_InvalidJSON_Returns400(t *testing.T) {
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, validScimStore(nil))
	c, rec := echoCtx(http.MethodPost, "/Users", []byte("{not-json"))
	if err := h.CreateUser(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400 (body=%s)", rec.Code, rec.Body.String())
	}
}

func TestReplaceUser_InvalidJSON_Returns400(t *testing.T) {
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, validScimStore(nil))
	c, rec := echoCtx(http.MethodPut, "/Users/u-1", []byte("{not-json"))
	c.SetParamNames("id")
	c.SetParamValues("u-1")
	if err := h.ReplaceUser(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400 (body=%s)", rec.Code, rec.Body.String())
	}
}

func TestPatchUser_InvalidJSON_Returns400(t *testing.T) {
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, validScimStore(nil))
	c, rec := echoCtx(http.MethodPatch, "/Users/u-1", []byte("{not-json"))
	c.SetParamNames("id")
	c.SetParamValues("u-1")
	if err := h.PatchUser(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400 (body=%s)", rec.Code, rec.Body.String())
	}
}

// TestCreateUser_LinkUserToIdPFailure_StillReturns201 asserts that a failure
// to stamp the UserFederatedIdentity row is non-fatal — the SCIM user
// provisioning is RFC-compliant once the NexusUser row lands, so the link
// failure logs a warning and the response is 201 anyway.
func TestCreateUser_LinkUserToIdPFailure_StillReturns201(t *testing.T) {
	idpID := "idp-okta"
	us := &stubUserStore{
		defaultOrgID: "org-1",
		createResult: makeUser("u-new", "alice@example.com", "active"),
	}
	ss := &stubScimStore{
		token: &scimstore.ScimToken{
			ID:                 "tok-1",
			IdentityProviderID: &idpID,
		},
		linkErr: errors.New("idp foreign key drift"),
	}
	h := buildHandler(us, &stubIAMStore{}, ss)
	body, _ := json.Marshal(map[string]any{
		"schemas":    []string{scimSchemaUser},
		"userName":   "alice@example.com",
		"externalId": "okta-12345",
	})
	c, rec := echoCtxWithToken(http.MethodPost, "/Users", body, ss.token)
	if err := h.CreateUser(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("status = %d; want 201 (linkErr is non-fatal; body=%s)", rec.Code, rec.Body.String())
	}
}

// gap-closure: Group handler invalid-body + assertScimGroup paths

func TestCreateGroup_InvalidJSON_Returns400(t *testing.T) {
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, validScimStore(nil))
	c, rec := echoCtx(http.MethodPost, "/Groups", []byte("{not-json"))
	if err := h.CreateGroup(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400 (body=%s)", rec.Code, rec.Body.String())
	}
}

func TestReplaceGroup_InvalidJSON_Returns400(t *testing.T) {
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, validScimStore(nil))
	c, rec := echoCtx(http.MethodPut, "/Groups/g-1", []byte("{not-json"))
	c.SetParamNames("id")
	c.SetParamValues("g-1")
	if err := h.ReplaceGroup(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400 (body=%s)", rec.Code, rec.Body.String())
	}
}

func TestPatchGroup_InvalidJSON_Returns400(t *testing.T) {
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, validScimStore(nil))
	c, rec := echoCtx(http.MethodPatch, "/Groups/g-1", []byte("{not-json"))
	c.SetParamNames("id")
	c.SetParamValues("g-1")
	if err := h.PatchGroup(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400 (body=%s)", rec.Code, rec.Body.String())
	}
}

// TestReplaceGroup_AssertScimGroupGetSourceError_Returns500 covers the
// branch where `GetIamGroupSource` errors before we ever reach UpdateIamGroup.
func TestReplaceGroup_AssertScimGroupGetSourceError_Returns500(t *testing.T) {
	ss := &stubScimStore{groupSrcErr: errors.New("db down")}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, ss)
	body, _ := json.Marshal(map[string]any{"displayName": "Updated"})
	tok := &scimstore.ScimToken{ID: "t1"}
	c, rec := echoCtxWithToken(http.MethodPut, "/Groups/g1", body, tok)
	c.SetParamNames("id")
	c.SetParamValues("g1")
	if err := h.ReplaceGroup(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500 (body=%s)", rec.Code, rec.Body.String())
	}
}

// TestPatchGroup_AssertScimGroupNonScim_Returns403 covers the branch where
// `assertScimGroup` rejects an admin-managed (non-SCIM) group.
func TestPatchGroup_AssertScimGroupNonScim_Returns403(t *testing.T) {
	ss := &stubScimStore{groupSrc: "admin"} // not "scim"
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, ss)
	body, _ := json.Marshal(map[string]any{
		"schemas":    []string{scimSchemaPatch},
		"Operations": []map[string]any{},
	})
	tok := &scimstore.ScimToken{ID: "t1"}
	c, rec := echoCtxWithToken(http.MethodPatch, "/Groups/g1", body, tok)
	c.SetParamNames("id")
	c.SetParamValues("g1")
	if err := h.PatchGroup(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d; want 403 (body=%s)", rec.Code, rec.Body.String())
	}
}

// TestCreateGroup_ExistingMappingWithMembers exercises the existing-mapping
// path (lines 437-439) — members in the request are appended to the
// mapping's IamGroup and we return 201 with the existing group's identity.
func TestCreateGroup_ExistingMappingWithMembers(t *testing.T) {
	idpID := "idp-okta"
	ss := &stubScimStore{
		token: &scimstore.ScimToken{ID: "tok-1", IdentityProviderID: &idpID},
		findMapping: &scimstore.IdpGroupMapping{
			IamGroupID: "g-existing",
		},
	}
	is := &stubIAMStore{
		group: makeGroup("g-existing", "Existing Group"),
	}
	h := buildHandler(&stubUserStore{}, is, ss)
	body, _ := json.Marshal(map[string]any{
		"schemas":     []string{scimSchemaGroup},
		"displayName": "External Group",
		"externalId":  "okta-grp-1",
		"members": []map[string]any{
			{"value": "u-1", "display": "Alice"},
			{"value": "u-2", "display": "Bob"},
		},
	})
	c, rec := echoCtxWithToken(http.MethodPost, "/Groups", body, ss.token)
	if err := h.CreateGroup(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("status = %d; want 201 (body=%s)", rec.Code, rec.Body.String())
	}
}

// TestCreateGroup_NewGroupMappingCreateError_StillReturns201 exercises the
// log-and-continue path (lines 459-462) when CreateIdpGroupMapping fails.
// The group is already created at the IAM layer — the mapping is a routing
// concern, so we keep the response 201 and let admins re-link later.
func TestCreateGroup_NewGroupMappingCreateError_StillReturns201(t *testing.T) {
	idpID := "idp-okta"
	ss := &stubScimStore{
		token:        &scimstore.ScimToken{ID: "tok-1", IdentityProviderID: &idpID},
		findMapping:  nil, // no existing mapping → goes to auto-create path
		createdGroup: makeGroup("g-new", "New Group"),
		createMapErr: errors.New("mapping create FK violation"),
	}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, ss)
	body, _ := json.Marshal(map[string]any{
		"schemas":     []string{scimSchemaGroup},
		"displayName": "New Group",
		"externalId":  "okta-grp-fresh",
	})
	c, rec := echoCtxWithToken(http.MethodPost, "/Groups", body, ss.token)
	if err := h.CreateGroup(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("status = %d; want 201 (mappingErr is non-fatal; body=%s)", rec.Code, rec.Body.String())
	}
}

// TestCreateGroup_NewGroupWithMembers exercises the new-group member-add
// path (lines 464-466) — when no existing mapping and members are supplied,
// each member is added to the newly-created group.
func TestCreateGroup_NewGroupWithMembers(t *testing.T) {
	idpID := "idp-okta"
	ss := &stubScimStore{
		token:        &scimstore.ScimToken{ID: "tok-1", IdentityProviderID: &idpID},
		findMapping:  nil,
		createdGroup: makeGroup("g-fresh", "Fresh Group"),
	}
	h := buildHandler(&stubUserStore{}, &stubIAMStore{}, ss)
	body, _ := json.Marshal(map[string]any{
		"schemas":     []string{scimSchemaGroup},
		"displayName": "Fresh Group",
		"members": []map[string]any{
			{"value": "u-alice", "display": "Alice"},
			{"value": "u-bob", "display": "Bob"},
		},
	})
	c, rec := echoCtxWithToken(http.MethodPost, "/Groups", body, ss.token)
	if err := h.CreateGroup(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("status = %d; want 201 (body=%s)", rec.Code, rec.Body.String())
	}
}

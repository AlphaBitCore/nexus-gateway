package me

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v4"

	authn "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authn"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/users/userstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/traffic/store/trafficstore"
)

// test helpers

func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func noopAudit() *audit.Writer {
	return audit.NewWriter(nil, "", silentLogger())
}

// adminAuth builds an AdminAuth for the given user ID and principal type.
func adminAuth(userID, principalType string) *authn.AdminAuth {
	return &authn.AdminAuth{
		KeyID:             userID,
		KeyName:           "test-user",
		AuthPrincipalType: principalType,
	}
}

// echoCtx builds an Echo context with an optional body and optional AdminAuth.
func echoCtx(method, path string, body []byte, aa *authn.AdminAuth) (echo.Context, *httptest.ResponseRecorder) {
	var rb io.Reader
	if body != nil {
		rb = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rb)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e := echo.New()
	c := e.NewContext(req, rec)
	if aa != nil {
		middleware.WithAdminAuth(c, aa)
	}
	return c, rec
}

// stubMeUserStore satisfies meUserStore.
type stubMeUserStore struct {
	findResult   *userstore.NexusUser
	findErr      error
	updateResult *userstore.NexusUserSafe
	updateErr    error
	listKeys     []userstore.AdminAPIKey
	listKeysErr  error
	createKey    *userstore.AdminAPIKey
	createKeyErr error
	getKey       *userstore.AdminAPIKey
	getKeyErr    error
	deleteKeyErr error
	regenKeyErr  error
}

func (s *stubMeUserStore) FindNexusUserByID(_ context.Context, _ string) (*userstore.NexusUser, error) {
	return s.findResult, s.findErr
}
func (s *stubMeUserStore) UpdateNexusUser(_ context.Context, _ string, _ userstore.UpdateNexusUserParams) (*userstore.NexusUserSafe, error) {
	return s.updateResult, s.updateErr
}
func (s *stubMeUserStore) ListAdminAPIKeys(_ context.Context, _ string) ([]userstore.AdminAPIKey, error) {
	return s.listKeys, s.listKeysErr
}
func (s *stubMeUserStore) CreateAdminAPIKey(_ context.Context, _ userstore.CreateAdminAPIKeyParams) (*userstore.AdminAPIKey, error) {
	return s.createKey, s.createKeyErr
}
func (s *stubMeUserStore) GetAdminAPIKey(_ context.Context, _ string) (*userstore.AdminAPIKey, error) {
	return s.getKey, s.getKeyErr
}
func (s *stubMeUserStore) DeleteAdminAPIKey(_ context.Context, _ string) error {
	return s.deleteKeyErr
}
func (s *stubMeUserStore) RegenerateAdminAPIKey(_ context.Context, _, _, _ string) error {
	return s.regenKeyErr
}

// stubMeIAMStore satisfies meIAMStore.
type stubMeIAMStore struct {
	groups    []string
	groupsErr error
}

func (s *stubMeIAMStore) ListGroupNamesForPrincipal(_ context.Context, _, _ string) ([]string, error) {
	return s.groups, s.groupsErr
}

// stubMeTrafficStore satisfies meTrafficStore.
type stubMeTrafficStore struct {
	logs    []trafficstore.AdminAuditLogEntry
	total   int
	logsErr error
}

func (s *stubMeTrafficStore) ListAdminAuditLogs(_ context.Context, _ trafficstore.AdminAuditLogListParams) ([]trafficstore.AdminAuditLogEntry, int, error) {
	return s.logs, s.total, s.logsErr
}

// buildHandler wires a Handler with stubs — no real DB.
func buildHandler(us meUserStore, is meIAMStore, ts meTrafficStore) *Handler {
	return &Handler{
		users:   us,
		iam:     is,
		traffic: ts,
		audit:   noopAudit(),
		logger:  silentLogger(),
	}
}

func TestGetMyProfile_NoAuth_Returns401(t *testing.T) {
	h := buildHandler(&stubMeUserStore{}, &stubMeIAMStore{}, &stubMeTrafficStore{})
	c, rec := echoCtx(http.MethodGet, "/api/my/profile", nil, nil)
	if err := h.GetMyProfile(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("code=%d want 401", rec.Code)
	}
}

func TestGetMyProfile_UserNotFound_Returns404(t *testing.T) {
	us := &stubMeUserStore{findResult: nil}
	h := buildHandler(us, &stubMeIAMStore{}, &stubMeTrafficStore{})
	c, rec := echoCtx(http.MethodGet, "/api/my/profile", nil, adminAuth("u1", "admin_user"))
	if err := h.GetMyProfile(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("code=%d want 404", rec.Code)
	}
}

func TestGetMyProfile_StoreError_Returns404(t *testing.T) {
	us := &stubMeUserStore{findErr: errors.New("db")}
	h := buildHandler(us, &stubMeIAMStore{}, &stubMeTrafficStore{})
	c, rec := echoCtx(http.MethodGet, "/api/my/profile", nil, adminAuth("u1", "admin_user"))
	if err := h.GetMyProfile(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("code=%d want 404 on store error", rec.Code)
	}
}

func TestGetMyProfile_Success_Returns200(t *testing.T) {
	email := "alice@example.com"
	us := &stubMeUserStore{
		findResult: &userstore.NexusUser{
			ID:          "u1",
			DisplayName: "Alice",
			Email:       &email,
			Status:      "active",
		},
	}
	is := &stubMeIAMStore{groups: []string{"admins"}}
	h := buildHandler(us, is, &stubMeTrafficStore{})
	c, rec := echoCtx(http.MethodGet, "/api/my/profile", nil, adminAuth("u1", "admin_user"))
	if err := h.GetMyProfile(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200; body=%s", rec.Code, rec.Body)
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["id"] != "u1" {
		t.Errorf("id=%v want 'u1'", resp["id"])
	}
}

func TestGetMyProfile_NonAdminPrincipalType_FetchesGroups(t *testing.T) {
	// Exercises the pt != "admin_user" → "nexus_user" branch.
	email := "bob@example.com"
	us := &stubMeUserStore{
		findResult: &userstore.NexusUser{
			ID: "u2", DisplayName: "Bob", Email: &email, Status: "active",
		},
	}
	is := &stubMeIAMStore{groups: []string{"viewers"}}
	h := buildHandler(us, is, &stubMeTrafficStore{})
	c, rec := echoCtx(http.MethodGet, "/api/my/profile", nil, adminAuth("u2", "api_key"))
	if err := h.GetMyProfile(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200", rec.Code)
	}
}

func TestUpdateMyProfile_NoAuth_Returns401(t *testing.T) {
	h := buildHandler(&stubMeUserStore{}, &stubMeIAMStore{}, &stubMeTrafficStore{})
	c, rec := echoCtx(http.MethodPatch, "/api/my/profile", nil, nil)
	if err := h.UpdateMyProfile(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("code=%d want 401", rec.Code)
	}
}

func TestUpdateMyProfile_NonAdminUser_Returns403(t *testing.T) {
	h := buildHandler(&stubMeUserStore{}, &stubMeIAMStore{}, &stubMeTrafficStore{})
	// AuthPrincipalType != "admin_user" → 403
	c, rec := echoCtx(http.MethodPatch, "/api/my/profile", nil, adminAuth("u1", "api_key"))
	if err := h.UpdateMyProfile(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("code=%d want 403", rec.Code)
	}
}

func TestUpdateMyProfile_InvalidTimezone_Returns400(t *testing.T) {
	h := buildHandler(&stubMeUserStore{}, &stubMeIAMStore{}, &stubMeTrafficStore{})
	tz := "Not/A/Timezone"
	body, _ := json.Marshal(map[string]any{"preferredTimezone": tz})
	c, rec := echoCtx(http.MethodPatch, "/api/my/profile", body, adminAuth("u1", "admin_user"))
	if err := h.UpdateMyProfile(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code=%d want 400 (invalid timezone)", rec.Code)
	}
}

func TestUpdateMyProfile_NewPassword_NoCurrentPassword_Returns400(t *testing.T) {
	h := buildHandler(&stubMeUserStore{}, &stubMeIAMStore{}, &stubMeTrafficStore{})
	body, _ := json.Marshal(map[string]any{"newPassword": "newpass123"})
	c, rec := echoCtx(http.MethodPatch, "/api/my/profile", body, adminAuth("u1", "admin_user"))
	if err := h.UpdateMyProfile(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code=%d want 400 (missing currentPassword)", rec.Code)
	}
}

func TestUpdateMyProfile_WrongCurrentPassword_Returns401(t *testing.T) {
	// Set up: user exists with a known password hash.
	hash, err := authn.HashPassword("correct-pass")
	if err != nil {
		t.Fatal(err)
	}
	email := "u1@example.com"
	us := &stubMeUserStore{
		// Source "local" mirrors the DB default; a password-backed account is
		// always local-sourced (SSO accounts are blocked before this check).
		findResult: &userstore.NexusUser{ID: "u1", Email: &email, Source: "local", PasswordHash: &hash},
	}
	h := buildHandler(us, &stubMeIAMStore{}, &stubMeTrafficStore{})
	current := "wrong-pass"
	newPass := "new-pass"
	body, _ := json.Marshal(map[string]any{"currentPassword": current, "newPassword": newPass})
	c, rec := echoCtx(http.MethodPatch, "/api/my/profile", body, adminAuth("u1", "admin_user"))
	if err := h.UpdateMyProfile(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("code=%d want 401 (wrong current password)", rec.Code)
	}
}

// TestUpdateMyProfile_SSOAccount_PasswordChangeReturns400 is the regression
// guard for the /account self-service path: a federated (oidc/scim) user must
// get an explicit sso_account 400 — not the confusing "current password is
// incorrect" 401 — when attempting to change a local password they don't have.
func TestUpdateMyProfile_SSOAccount_PasswordChangeReturns400(t *testing.T) {
	email := "steve@example.com"
	us := &stubMeUserStore{
		findResult: &userstore.NexusUser{ID: "u1", Email: &email, Source: "oidc"},
	}
	h := buildHandler(us, &stubMeIAMStore{}, &stubMeTrafficStore{})
	body, _ := json.Marshal(map[string]any{"currentPassword": "whatever", "newPassword": "new-pass"})
	c, rec := echoCtx(http.MethodPatch, "/api/my/profile", body, adminAuth("u1", "admin_user"))
	if err := h.UpdateMyProfile(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400 (SSO account); body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "sso_account") {
		t.Errorf("body=%s want sso_account code", rec.Body.String())
	}
}

func TestUpdateMyProfile_Success_Returns200(t *testing.T) {
	email := "u1@example.com"
	safe := &userstore.NexusUserSafe{ID: "u1", DisplayName: "Alice", Email: &email, Status: "active"}
	us := &stubMeUserStore{updateResult: safe}
	h := buildHandler(us, &stubMeIAMStore{}, &stubMeTrafficStore{})
	body, _ := json.Marshal(map[string]any{"displayName": "Alice Updated"})
	c, rec := echoCtx(http.MethodPatch, "/api/my/profile", body, adminAuth("u1", "admin_user"))
	if err := h.UpdateMyProfile(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200; body=%s", rec.Code, rec.Body)
	}
}

func TestUpdateMyProfile_UpdateFails_Returns500(t *testing.T) {
	us := &stubMeUserStore{updateErr: errors.New("db")}
	h := buildHandler(us, &stubMeIAMStore{}, &stubMeTrafficStore{})
	body, _ := json.Marshal(map[string]any{"displayName": "Name"})
	c, rec := echoCtx(http.MethodPatch, "/api/my/profile", body, adminAuth("u1", "admin_user"))
	if err := h.UpdateMyProfile(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500", rec.Code)
	}
}

func TestUpdateMyProfile_UserNotFoundForPasswordChange_Returns500(t *testing.T) {
	us := &stubMeUserStore{findResult: nil, findErr: nil}
	h := buildHandler(us, &stubMeIAMStore{}, &stubMeTrafficStore{})
	current := "pass"
	newPass := "new"
	body, _ := json.Marshal(map[string]any{"currentPassword": current, "newPassword": newPass})
	c, rec := echoCtx(http.MethodPatch, "/api/my/profile", body, adminAuth("u1", "admin_user"))
	if err := h.UpdateMyProfile(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500 (user not found for pw change)", rec.Code)
	}
}

func TestUpdateMyProfile_ValidTimezone_Returns200(t *testing.T) {
	email := "u1@example.com"
	safe := &userstore.NexusUserSafe{ID: "u1", DisplayName: "Alice", Email: &email, Status: "active"}
	us := &stubMeUserStore{updateResult: safe}
	h := buildHandler(us, &stubMeIAMStore{}, &stubMeTrafficStore{})
	tz := "America/New_York"
	body, _ := json.Marshal(map[string]any{"preferredTimezone": tz})
	c, rec := echoCtx(http.MethodPatch, "/api/my/profile", body, adminAuth("u1", "admin_user"))
	if err := h.UpdateMyProfile(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200 (valid tz)", rec.Code)
	}
}

func TestUpdateMyProfile_UsernameAlias_Returns200(t *testing.T) {
	// "username" field is an alias for "displayName"
	email := "u1@example.com"
	safe := &userstore.NexusUserSafe{ID: "u1", DisplayName: "Alice2", Email: &email, Status: "active"}
	us := &stubMeUserStore{updateResult: safe}
	h := buildHandler(us, &stubMeIAMStore{}, &stubMeTrafficStore{})
	body, _ := json.Marshal(map[string]any{"username": "Alice2"})
	c, rec := echoCtx(http.MethodPatch, "/api/my/profile", body, adminAuth("u1", "admin_user"))
	if err := h.UpdateMyProfile(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200 (username alias)", rec.Code)
	}
}

func TestListMyActivity_NoAuth_Returns401(t *testing.T) {
	h := buildHandler(&stubMeUserStore{}, &stubMeIAMStore{}, &stubMeTrafficStore{})
	c, rec := echoCtx(http.MethodGet, "/api/my/activity", nil, nil)
	if err := h.ListMyActivity(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("code=%d want 401", rec.Code)
	}
}

func TestListMyActivity_StoreError_Returns500(t *testing.T) {
	ts := &stubMeTrafficStore{logsErr: errors.New("db")}
	h := buildHandler(&stubMeUserStore{}, &stubMeIAMStore{}, ts)
	c, rec := echoCtx(http.MethodGet, "/api/my/activity", nil, adminAuth("u1", "admin_user"))
	if err := h.ListMyActivity(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500", rec.Code)
	}
}

func TestListMyActivity_Success_Returns200(t *testing.T) {
	ts := &stubMeTrafficStore{logs: []trafficstore.AdminAuditLogEntry{
		{ID: "entry-1", Action: "admin:user.read"},
	}, total: 1}
	h := buildHandler(&stubMeUserStore{}, &stubMeIAMStore{}, ts)
	c, rec := echoCtx(http.MethodGet, "/api/my/activity", nil, adminAuth("u1", "admin_user"))
	if err := h.ListMyActivity(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200; body=%s", rec.Code, rec.Body)
	}
}

func TestListUserAPIKeys_NoAuth_Returns401(t *testing.T) {
	h := buildHandler(&stubMeUserStore{}, &stubMeIAMStore{}, &stubMeTrafficStore{})
	c, rec := echoCtx(http.MethodGet, "/api/user/api-keys", nil, nil)
	if err := h.ListUserAPIKeys(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("code=%d want 401", rec.Code)
	}
}

func TestListUserAPIKeys_StoreError_Returns500(t *testing.T) {
	us := &stubMeUserStore{listKeysErr: errors.New("db")}
	h := buildHandler(us, &stubMeIAMStore{}, &stubMeTrafficStore{})
	c, rec := echoCtx(http.MethodGet, "/api/user/api-keys", nil, adminAuth("u1", "admin_user"))
	if err := h.ListUserAPIKeys(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500", rec.Code)
	}
}

func TestListUserAPIKeys_Success_Returns200(t *testing.T) {
	uid := "u1"
	us := &stubMeUserStore{listKeys: []userstore.AdminAPIKey{
		{ID: "k1", Name: "my-key", OwnerUserID: &uid},
	}}
	h := buildHandler(us, &stubMeIAMStore{}, &stubMeTrafficStore{})
	c, rec := echoCtx(http.MethodGet, "/api/user/api-keys", nil, adminAuth("u1", "admin_user"))
	if err := h.ListUserAPIKeys(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200; body=%s", rec.Code, rec.Body)
	}
}

func TestCreateUserAPIKey_NoAuth_Returns401(t *testing.T) {
	h := buildHandler(&stubMeUserStore{}, &stubMeIAMStore{}, &stubMeTrafficStore{})
	c, rec := echoCtx(http.MethodPost, "/api/user/api-keys", nil, nil)
	if err := h.CreateUserAPIKey(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("code=%d want 401", rec.Code)
	}
}

func TestCreateUserAPIKey_MissingName_Returns400(t *testing.T) {
	h := buildHandler(&stubMeUserStore{}, &stubMeIAMStore{}, &stubMeTrafficStore{})
	body, _ := json.Marshal(map[string]any{"name": ""})
	c, rec := echoCtx(http.MethodPost, "/api/user/api-keys", body, adminAuth("u1", "admin_user"))
	if err := h.CreateUserAPIKey(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code=%d want 400 (missing name)", rec.Code)
	}
}

func TestCreateUserAPIKey_InvalidExpiresAt_Returns400(t *testing.T) {
	h := buildHandler(&stubMeUserStore{}, &stubMeIAMStore{}, &stubMeTrafficStore{})
	body, _ := json.Marshal(map[string]any{"name": "my-key", "expiresAt": "not-a-date"})
	c, rec := echoCtx(http.MethodPost, "/api/user/api-keys", body, adminAuth("u1", "admin_user"))
	if err := h.CreateUserAPIKey(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code=%d want 400 (invalid expiresAt)", rec.Code)
	}
}

func TestCreateUserAPIKey_DuplicateName_Returns409(t *testing.T) {
	pgErr := &pgconn.PgError{Code: "23505"}
	us := &stubMeUserStore{createKeyErr: pgErr}
	h := buildHandler(us, &stubMeIAMStore{}, &stubMeTrafficStore{})
	body, _ := json.Marshal(map[string]any{"name": "dup-key"})
	c, rec := echoCtx(http.MethodPost, "/api/user/api-keys", body, adminAuth("u1", "admin_user"))
	if err := h.CreateUserAPIKey(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusConflict {
		t.Errorf("code=%d want 409 (duplicate name)", rec.Code)
	}
}

func TestCreateUserAPIKey_StoreError_Returns500(t *testing.T) {
	us := &stubMeUserStore{createKeyErr: errors.New("db")}
	h := buildHandler(us, &stubMeIAMStore{}, &stubMeTrafficStore{})
	body, _ := json.Marshal(map[string]any{"name": "my-key"})
	c, rec := echoCtx(http.MethodPost, "/api/user/api-keys", body, adminAuth("u1", "admin_user"))
	if err := h.CreateUserAPIKey(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500", rec.Code)
	}
}

func TestCreateUserAPIKey_Success_Returns201(t *testing.T) {
	uid := "u1"
	expAt := time.Now().Add(24 * time.Hour)
	us := &stubMeUserStore{createKey: &userstore.AdminAPIKey{
		ID:          "k-new",
		Name:        "my-key",
		OwnerUserID: &uid,
		ExpiresAt:   &expAt,
	}}
	h := buildHandler(us, &stubMeIAMStore{}, &stubMeTrafficStore{})
	body, _ := json.Marshal(map[string]any{
		"name":      "my-key",
		"expiresAt": expAt.Format(time.RFC3339),
	})
	c, rec := echoCtx(http.MethodPost, "/api/user/api-keys", body, adminAuth("u1", "admin_user"))
	if err := h.CreateUserAPIKey(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("code=%d want 201; body=%s", rec.Code, rec.Body)
	}
}

func TestDeleteUserAPIKey_NoAuth_Returns401(t *testing.T) {
	h := buildHandler(&stubMeUserStore{}, &stubMeIAMStore{}, &stubMeTrafficStore{})
	c, rec := echoCtx(http.MethodDelete, "/api/user/api-keys/k1", nil, nil)
	c.SetParamNames("id")
	c.SetParamValues("k1")
	if err := h.DeleteUserAPIKey(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("code=%d want 401", rec.Code)
	}
}

func TestDeleteUserAPIKey_NotFound_Returns404(t *testing.T) {
	us := &stubMeUserStore{getKey: nil}
	h := buildHandler(us, &stubMeIAMStore{}, &stubMeTrafficStore{})
	c, rec := echoCtx(http.MethodDelete, "/api/user/api-keys/missing", nil, adminAuth("u1", "admin_user"))
	c.SetParamNames("id")
	c.SetParamValues("missing")
	if err := h.DeleteUserAPIKey(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("code=%d want 404", rec.Code)
	}
}

func TestDeleteUserAPIKey_WrongOwner_Returns403(t *testing.T) {
	otherUID := "other-user"
	us := &stubMeUserStore{getKey: &userstore.AdminAPIKey{ID: "k1", OwnerUserID: &otherUID}}
	h := buildHandler(us, &stubMeIAMStore{}, &stubMeTrafficStore{})
	c, rec := echoCtx(http.MethodDelete, "/api/user/api-keys/k1", nil, adminAuth("u1", "admin_user"))
	c.SetParamNames("id")
	c.SetParamValues("k1")
	if err := h.DeleteUserAPIKey(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("code=%d want 403 (wrong owner)", rec.Code)
	}
}

func TestDeleteUserAPIKey_DeleteError_Returns500(t *testing.T) {
	uid := "u1"
	us := &stubMeUserStore{
		getKey:       &userstore.AdminAPIKey{ID: "k1", OwnerUserID: &uid},
		deleteKeyErr: errors.New("db"),
	}
	h := buildHandler(us, &stubMeIAMStore{}, &stubMeTrafficStore{})
	c, rec := echoCtx(http.MethodDelete, "/api/user/api-keys/k1", nil, adminAuth("u1", "admin_user"))
	c.SetParamNames("id")
	c.SetParamValues("k1")
	if err := h.DeleteUserAPIKey(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500", rec.Code)
	}
}

func TestDeleteUserAPIKey_Success_Returns200(t *testing.T) {
	uid := "u1"
	us := &stubMeUserStore{getKey: &userstore.AdminAPIKey{ID: "k1", OwnerUserID: &uid}}
	h := buildHandler(us, &stubMeIAMStore{}, &stubMeTrafficStore{})
	c, rec := echoCtx(http.MethodDelete, "/api/user/api-keys/k1", nil, adminAuth("u1", "admin_user"))
	c.SetParamNames("id")
	c.SetParamValues("k1")
	if err := h.DeleteUserAPIKey(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200; body=%s", rec.Code, rec.Body)
	}
}

func TestRegenerateUserAPIKey_NoAuth_Returns401(t *testing.T) {
	h := buildHandler(&stubMeUserStore{}, &stubMeIAMStore{}, &stubMeTrafficStore{})
	c, rec := echoCtx(http.MethodPost, "/api/user/api-keys/k1/regenerate", nil, nil)
	c.SetParamNames("id")
	c.SetParamValues("k1")
	if err := h.RegenerateUserAPIKey(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("code=%d want 401", rec.Code)
	}
}

func TestRegenerateUserAPIKey_NotFound_Returns404(t *testing.T) {
	us := &stubMeUserStore{getKey: nil}
	h := buildHandler(us, &stubMeIAMStore{}, &stubMeTrafficStore{})
	c, rec := echoCtx(http.MethodPost, "/api/user/api-keys/missing/regenerate", nil, adminAuth("u1", "admin_user"))
	c.SetParamNames("id")
	c.SetParamValues("missing")
	if err := h.RegenerateUserAPIKey(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("code=%d want 404", rec.Code)
	}
}

func TestRegenerateUserAPIKey_WrongOwner_Returns403(t *testing.T) {
	otherUID := "other-user"
	us := &stubMeUserStore{getKey: &userstore.AdminAPIKey{ID: "k1", OwnerUserID: &otherUID}}
	h := buildHandler(us, &stubMeIAMStore{}, &stubMeTrafficStore{})
	c, rec := echoCtx(http.MethodPost, "/api/user/api-keys/k1/regenerate", nil, adminAuth("u1", "admin_user"))
	c.SetParamNames("id")
	c.SetParamValues("k1")
	if err := h.RegenerateUserAPIKey(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("code=%d want 403 (wrong owner)", rec.Code)
	}
}

func TestRegenerateUserAPIKey_RegenError_Returns500(t *testing.T) {
	uid := "u1"
	us := &stubMeUserStore{
		getKey:      &userstore.AdminAPIKey{ID: "k1", OwnerUserID: &uid},
		regenKeyErr: errors.New("db"),
	}
	h := buildHandler(us, &stubMeIAMStore{}, &stubMeTrafficStore{})
	c, rec := echoCtx(http.MethodPost, "/api/user/api-keys/k1/regenerate", nil, adminAuth("u1", "admin_user"))
	c.SetParamNames("id")
	c.SetParamValues("k1")
	if err := h.RegenerateUserAPIKey(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500", rec.Code)
	}
}

func TestRegenerateUserAPIKey_Success_Returns200(t *testing.T) {
	uid := "u1"
	us := &stubMeUserStore{getKey: &userstore.AdminAPIKey{ID: "k1", OwnerUserID: &uid}}
	h := buildHandler(us, &stubMeIAMStore{}, &stubMeTrafficStore{})
	c, rec := echoCtx(http.MethodPost, "/api/user/api-keys/k1/regenerate", nil, adminAuth("u1", "admin_user"))
	c.SetParamNames("id")
	c.SetParamValues("k1")
	if err := h.RegenerateUserAPIKey(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200; body=%s", rec.Code, rec.Body)
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if _, ok := resp["key"]; !ok {
		t.Error("response missing 'key' field")
	}
}

// New + helpers

func TestNew_NilPool_DoesNotPanic(t *testing.T) {
	h := New(Deps{
		Pool:   nil,
		Hub:    nil,
		Audit:  noopAudit(),
		Logger: silentLogger(),
	})
	if h == nil {
		t.Fatal("New returned nil")
		return
	}
	if h.users != nil || h.iam != nil || h.traffic != nil {
		t.Error("expected nil stores with nil pool")
	}
}

func TestNew_NilLogger_UsesDefault(t *testing.T) {
	h := New(Deps{Pool: nil, Audit: noopAudit()})
	if h.logger == nil {
		t.Error("expected non-nil logger when Logger not provided")
	}
}

func TestNew_WithPool_InitializesStores(t *testing.T) {
	// pgxpool.NewWithConfig creates a pool object without establishing a connection
	// — construction is lazy. This exercises the pool != nil branch in New().
	cfg, err := pgxpool.ParseConfig("postgres://nouser:nopass@127.0.0.1:1/nodb?connect_timeout=1")
	if err != nil {
		t.Fatalf("parse pool config: %v", err)
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Skip("cannot construct pgxpool (network unavailable):", err)
	}
	defer pool.Close()

	h := New(Deps{Pool: pool, Audit: noopAudit(), Logger: silentLogger()})
	if h.users == nil || h.iam == nil || h.traffic == nil {
		t.Error("expected non-nil stores when pool is provided")
	}
}

func TestErrJSON_Shape(t *testing.T) {
	e := errJSON("test msg", "test_type", "test_code")
	inner, ok := e["error"].(map[string]any)
	if !ok {
		t.Fatal("errJSON missing 'error' map")
	}
	if inner["message"] != "test msg" {
		t.Errorf("message=%v want 'test msg'", inner["message"])
	}
}

func TestParsePagination_Defaults(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	e := echo.New()
	c := e.NewContext(req, httptest.NewRecorder())
	pg := parsePagination(c)
	if pg.Limit != 50 {
		t.Errorf("limit=%d want 50", pg.Limit)
	}
	if pg.Offset != 0 {
		t.Errorf("offset=%d want 0", pg.Offset)
	}
}

func TestParsePagination_CapAt1000(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/?limit=9999&offset=5", nil)
	e := echo.New()
	c := e.NewContext(req, httptest.NewRecorder())
	pg := parsePagination(c)
	if pg.Limit != 1000 {
		t.Errorf("limit=%d want 1000 (capped)", pg.Limit)
	}
	if pg.Offset != 5 {
		t.Errorf("offset=%d want 5", pg.Offset)
	}
}

func TestParsePagination_InvalidValues_UsesDefaults(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/?limit=abc&offset=-1", nil)
	e := echo.New()
	c := e.NewContext(req, httptest.NewRecorder())
	pg := parsePagination(c)
	if pg.Limit != 50 {
		t.Errorf("limit=%d want 50 (default on invalid)", pg.Limit)
	}
	if pg.Offset != 0 {
		t.Errorf("offset=%d want 0 (default on negative)", pg.Offset)
	}
}

func TestParseRFC3339Flexible_NanoAndBase(t *testing.T) {
	nanoStr := "2026-01-01T00:00:00.000000001Z"
	if _, ok := parseRFC3339Flexible(nanoStr); !ok {
		t.Errorf("parseRFC3339Flexible failed for nano: %s", nanoStr)
	}
	baseStr := "2026-01-01T00:00:00Z"
	if _, ok := parseRFC3339Flexible(baseStr); !ok {
		t.Errorf("parseRFC3339Flexible failed for base: %s", baseStr)
	}
	if _, ok := parseRFC3339Flexible("not-a-date"); ok {
		t.Error("parseRFC3339Flexible should fail on invalid input")
	}
}

func TestParseTimeRange_ValidRange(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/?startTime=2026-01-01T00:00:00Z&endTime=2026-12-31T23:59:59Z", nil)
	e := echo.New()
	c := e.NewContext(req, httptest.NewRecorder())
	start, end := parseTimeRange(c)
	if start == nil {
		t.Error("expected non-nil start")
	}
	if end == nil {
		t.Error("expected non-nil end")
	}
}

func TestIsDuplicateKeyError_MatchesCode23505(t *testing.T) {
	pgErr := &pgconn.PgError{Code: "23505"}
	if !isDuplicateKeyError(pgErr) {
		t.Error("expected true for 23505")
	}
	if isDuplicateKeyError(errors.New("other")) {
		t.Error("expected false for non-pg error")
	}
}

func TestDeleteUserAPIKey_GetKeyError_Returns404(t *testing.T) {
	us := &stubMeUserStore{getKeyErr: errors.New("db")}
	h := buildHandler(us, &stubMeIAMStore{}, &stubMeTrafficStore{})
	c, rec := echoCtx(http.MethodDelete, "/api/user/api-keys/k1", nil, adminAuth("u1", "admin_user"))
	c.SetParamNames("id")
	c.SetParamValues("k1")
	if err := h.DeleteUserAPIKey(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("code=%d want 404 on get error", rec.Code)
	}
}

func TestRegenerateUserAPIKey_GetKeyError_Returns404(t *testing.T) {
	us := &stubMeUserStore{getKeyErr: errors.New("db")}
	h := buildHandler(us, &stubMeIAMStore{}, &stubMeTrafficStore{})
	c, rec := echoCtx(http.MethodPost, "/api/user/api-keys/k1/regenerate", nil, adminAuth("u1", "admin_user"))
	c.SetParamNames("id")
	c.SetParamValues("k1")
	if err := h.RegenerateUserAPIKey(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("code=%d want 404 on get error", rec.Code)
	}
}

func TestDeleteUserAPIKey_NilOwner_Returns403(t *testing.T) {
	// OwnerUserID == nil → treated as wrong owner
	us := &stubMeUserStore{getKey: &userstore.AdminAPIKey{ID: "k1", OwnerUserID: nil}}
	h := buildHandler(us, &stubMeIAMStore{}, &stubMeTrafficStore{})
	c, rec := echoCtx(http.MethodDelete, "/api/user/api-keys/k1", nil, adminAuth("u1", "admin_user"))
	c.SetParamNames("id")
	c.SetParamValues("k1")
	if err := h.DeleteUserAPIKey(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("code=%d want 403 (nil owner)", rec.Code)
	}
}

func TestRegenerateUserAPIKey_NilOwner_Returns403(t *testing.T) {
	// OwnerUserID == nil → treated as wrong owner
	us := &stubMeUserStore{getKey: &userstore.AdminAPIKey{ID: "k1", OwnerUserID: nil}}
	h := buildHandler(us, &stubMeIAMStore{}, &stubMeTrafficStore{})
	c, rec := echoCtx(http.MethodPost, "/api/user/api-keys/k1/regenerate", nil, adminAuth("u1", "admin_user"))
	c.SetParamNames("id")
	c.SetParamValues("k1")
	if err := h.RegenerateUserAPIKey(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("code=%d want 403 (nil owner)", rec.Code)
	}
}

func TestActorFromContext_WithAuth(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	e := echo.New()
	c := e.NewContext(req, httptest.NewRecorder())
	aa := &authn.AdminAuth{KeyID: "u1", KeyName: "Alice"}
	middleware.WithAdminAuth(c, aa)
	actor := actorFromContext(c)
	if actor.UserID != "u1" || actor.Name != "Alice" {
		t.Errorf("actorFromContext=%+v want {u1 Alice}", actor)
	}
}

func TestActorFromContext_NoAuth(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	e := echo.New()
	c := e.NewContext(req, httptest.NewRecorder())
	actor := actorFromContext(c)
	if actor.UserID != "" || actor.Name != "" {
		t.Errorf("expected empty actor without auth, got %+v", actor)
	}
}

func TestParseAdminAuditParams_WithQueryParams(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/?action=test&entityType=user&limit=10&offset=5", nil)
	e := echo.New()
	c := e.NewContext(req, httptest.NewRecorder())
	p := parseAdminAuditParams(c)
	if p.Action != "test" {
		t.Errorf("action=%q want 'test'", p.Action)
	}
	if p.EntityType != "user" {
		t.Errorf("entityType=%q want 'user'", p.EntityType)
	}
	if p.Limit != 10 {
		t.Errorf("limit=%d want 10", p.Limit)
	}
}

// Additional coverage for lower-than-100% functions

func TestInternalServerError_Returns500(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	e := echo.New()
	c := e.NewContext(req, rec)
	err := internalServerError(c, "test error")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code=%d want 500", rec.Code)
	}
}

func TestRegisterUserAPIKeyRoutes_MountsRoutes(t *testing.T) {
	h := buildHandler(&stubMeUserStore{}, &stubMeIAMStore{}, &stubMeTrafficStore{})
	e := echo.New()
	g := e.Group("/api/user")
	h.RegisterUserAPIKeyRoutes(g)
	if len(e.Routes()) == 0 {
		t.Error("RegisterUserAPIKeyRoutes did not mount any routes")
	}
}

func TestRegisterMyRoutes_MountsRoutes(t *testing.T) {
	// h.pool is nil — virtualkey.New accepts nil pool; routes still mount.
	h := buildHandler(&stubMeUserStore{}, &stubMeIAMStore{}, &stubMeTrafficStore{})
	e := echo.New()
	g := e.Group("/api/my")
	h.RegisterMyRoutes(g)
	if len(e.Routes()) == 0 {
		t.Error("RegisterMyRoutes did not mount any routes")
	}
}

func TestParseRFC3339Flexible_NanoFirst(t *testing.T) {
	// Hits the RFC3339Nano branch (with sub-second precision).
	s := "2026-05-01T12:00:00.123456789Z"
	if _, ok := parseRFC3339Flexible(s); !ok {
		t.Errorf("parseRFC3339Flexible failed for nano format: %s", s)
	}
}

func TestParseTimeRange_InvalidStrings_ReturnsNils(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/?startTime=bad&endTime=bad", nil)
	e := echo.New()
	c := e.NewContext(req, httptest.NewRecorder())
	start, end := parseTimeRange(c)
	if start != nil || end != nil {
		t.Error("expected nil start/end for invalid time strings")
	}
}

func TestUpdateMyProfile_CorrectPassword_ChangesPassword(t *testing.T) {
	hash, err := authn.HashPassword("old-pass")
	if err != nil {
		t.Fatal(err)
	}
	email := "u1@example.com"
	safe := &userstore.NexusUserSafe{ID: "u1", Email: &email, Status: "active"}
	us := &stubMeUserStore{
		findResult:   &userstore.NexusUser{ID: "u1", Email: &email, Source: "local", PasswordHash: &hash},
		updateResult: safe,
	}
	h := buildHandler(us, &stubMeIAMStore{}, &stubMeTrafficStore{})
	body, _ := json.Marshal(map[string]any{
		"currentPassword": "old-pass",
		"newPassword":     "new-pass-secure",
	})
	c, rec := echoCtx(http.MethodPatch, "/api/my/profile", body, adminAuth("u1", "admin_user"))
	if err := h.UpdateMyProfile(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d want 200 (correct password change); body=%s", rec.Code, rec.Body)
	}
}

func TestCreateUserAPIKey_SuccessNoExpiresAt_Returns201(t *testing.T) {
	uid := "u1"
	us := &stubMeUserStore{createKey: &userstore.AdminAPIKey{
		ID:          "k-bare",
		Name:        "bare-key",
		OwnerUserID: &uid,
	}}
	h := buildHandler(us, &stubMeIAMStore{}, &stubMeTrafficStore{})
	body, _ := json.Marshal(map[string]any{"name": "bare-key"})
	c, rec := echoCtx(http.MethodPost, "/api/user/api-keys", body, adminAuth("u1", "admin_user"))
	if err := h.CreateUserAPIKey(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("code=%d want 201; body=%s", rec.Code, rec.Body)
	}
}

package iam

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	authstore "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/store"
)

// fakeOAuthClientStore is an in-memory iamOAuthClientStore for the handler
// tests. Each field is a behaviour configurator — set the per-method err or
// the corresponding row(s) to drive the handler's branches.
type fakeOAuthClientStore struct {
	listResult []*authstore.OAuthClient
	listErr    error

	getResult *authstore.OAuthClient
	getErr    error

	createResult *authstore.OAuthClient
	createErr    error
	createCalls  []authstore.CreateInput

	updateResult *authstore.OAuthClient
	updateErr    error
	updateCalls  []authstore.UpdateInput

	deleteErr   error
	deleteCalls []string

	rotateResult *authstore.OAuthClient
	rotateErr    error
	rotateCalls  int

	countResult int
	countErr    error
}

func (f *fakeOAuthClientStore) List(_ context.Context) ([]*authstore.OAuthClient, error) {
	return f.listResult, f.listErr
}
func (f *fakeOAuthClientStore) GetByID(_ context.Context, _ string) (*authstore.OAuthClient, error) {
	return f.getResult, f.getErr
}
func (f *fakeOAuthClientStore) Create(_ context.Context, in authstore.CreateInput) (*authstore.OAuthClient, error) {
	f.createCalls = append(f.createCalls, in)
	if f.createErr != nil {
		return nil, f.createErr
	}
	// Default behaviour: echo the input as a persisted row so tests that
	// don't set createResult still get a round-trip.
	if f.createResult != nil {
		return f.createResult, nil
	}
	return &authstore.OAuthClient{
		ID: in.ID, Name: in.Name, Type: in.Type,
		RedirectURIs: in.RedirectURIs, AllowedScopes: in.AllowedScopes,
		AccessTTLSeconds:  in.AccessTTLSeconds,
		RefreshTTLSeconds: in.RefreshTTLSeconds,
		ClientSecretHash:  in.SecretHash,
	}, nil
}
func (f *fakeOAuthClientStore) Update(_ context.Context, _ string, in authstore.UpdateInput) (*authstore.OAuthClient, error) {
	f.updateCalls = append(f.updateCalls, in)
	if f.updateErr != nil {
		return nil, f.updateErr
	}
	return f.updateResult, nil
}
func (f *fakeOAuthClientStore) Delete(_ context.Context, id string) error {
	f.deleteCalls = append(f.deleteCalls, id)
	return f.deleteErr
}
func (f *fakeOAuthClientStore) RotateSecret(_ context.Context, _ string, _ []byte) (*authstore.OAuthClient, error) {
	f.rotateCalls++
	if f.rotateErr != nil {
		return nil, f.rotateErr
	}
	return f.rotateResult, nil
}
func (f *fakeOAuthClientStore) CountActiveRefreshTokens(_ context.Context, _ string) (int, error) {
	return f.countResult, f.countErr
}

func newOAuthHandler(oauth *fakeOAuthClientStore) *Handler {
	return &Handler{
		oauth:  oauth,
		audit:  noopAudit(),
		logger: silentLogger(),
	}
}

// callJSON exec's an Echo context against the handler with a JSON body.
func callJSON(t *testing.T, method, target, idParam string, body any, fn func(echo.Context) error) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	req := httptest.NewRequest(method, target, &buf)
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)
	if idParam != "" {
		c.SetParamNames("id")
		c.SetParamValues(idParam)
	}
	if err := fn(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	var parsed map[string]any
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &parsed)
	}
	return rec, parsed
}

func sampleOAuthClient() *authstore.OAuthClient {
	now := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	hash := "stored-hash"
	return &authstore.OAuthClient{
		ID: "c1", Name: "Sample", Type: "confidential",
		RedirectURIs:      []string{"https://x/cb"},
		AllowedScopes:     []string{"openid"},
		AccessTTLSeconds:  3600,
		RefreshTTLSeconds: 86400,
		ClientSecretHash:  &hash,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
}

// validCreateBody returns a body that passes every validator. Tests mutate
// it to trip individual rules.
func validCreateBody() map[string]any {
	return map[string]any{
		"id":            "valid-id",
		"name":          "Valid",
		"type":          "confidential",
		"redirectUris":  []string{"https://x/cb"},
		"allowedScopes": []string{"openid"},
	}
}

func TestRegisterOAuthClientRoutes_DoesNotPanic(t *testing.T) {
	h := newOAuthHandler(&fakeOAuthClientStore{})
	g := echo.New().Group("")
	h.RegisterOAuthClientRoutes(g, noopIAMMW)
}

func TestListOAuthClients_HappyPath(t *testing.T) {
	h := newOAuthHandler(&fakeOAuthClientStore{
		listResult: []*authstore.OAuthClient{sampleOAuthClient()},
	})
	rec, body := callJSON(t, http.MethodGet, "/oauth-clients", "", nil, h.ListOAuthClients)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d, want 200", rec.Code)
	}
	data, _ := body["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("data len=%d, want 1", len(data))
	}
	if strings.Contains(rec.Body.String(), "stored-hash") {
		t.Fatal("clientSecretHash must NOT appear in list response")
	}
	if strings.Contains(rec.Body.String(), "clientSecret") {
		t.Fatal("clientSecret plaintext must NOT appear in list response")
	}
}

func TestListOAuthClients_StoreError(t *testing.T) {
	h := newOAuthHandler(&fakeOAuthClientStore{listErr: errors.New("db")})
	rec, _ := callJSON(t, http.MethodGet, "/oauth-clients", "", nil, h.ListOAuthClients)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code=%d, want 500", rec.Code)
	}
}

func TestGetOAuthClient_HappyPathEmbedsRefreshCount(t *testing.T) {
	h := newOAuthHandler(&fakeOAuthClientStore{
		getResult: sampleOAuthClient(), countResult: 7,
	})
	rec, body := callJSON(t, http.MethodGet, "/oauth-clients/c1", "c1", nil, h.GetOAuthClient)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d, want 200", rec.Code)
	}
	data := body["data"].(map[string]any)
	if data["activeRefreshTokenCount"].(float64) != 7 {
		t.Fatalf("activeRefreshTokenCount=%v, want 7", data["activeRefreshTokenCount"])
	}
	if _, ok := data["clientSecretHash"]; ok {
		t.Fatal("clientSecretHash must NOT appear in detail response")
	}
}

func TestGetOAuthClient_NotFound(t *testing.T) {
	h := newOAuthHandler(&fakeOAuthClientStore{getErr: authstore.ErrClientNotFound})
	rec, _ := callJSON(t, http.MethodGet, "/oauth-clients/x", "x", nil, h.GetOAuthClient)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d, want 404", rec.Code)
	}
}

func TestGetOAuthClient_GetGenericError(t *testing.T) {
	h := newOAuthHandler(&fakeOAuthClientStore{getErr: errors.New("db")})
	rec, _ := callJSON(t, http.MethodGet, "/oauth-clients/x", "x", nil, h.GetOAuthClient)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code=%d, want 500", rec.Code)
	}
}

func TestGetOAuthClient_CountErrorStillReturns200(t *testing.T) {
	// Activity card degrades to 0 rather than blocking the detail page.
	h := newOAuthHandler(&fakeOAuthClientStore{
		getResult: sampleOAuthClient(), countErr: errors.New("count down"),
	})
	rec, body := callJSON(t, http.MethodGet, "/oauth-clients/c1", "c1", nil, h.GetOAuthClient)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d, want 200", rec.Code)
	}
	data := body["data"].(map[string]any)
	if data["activeRefreshTokenCount"].(float64) != 0 {
		t.Fatalf("count fallback expected 0, got %v", data["activeRefreshTokenCount"])
	}
}

func TestCreateOAuthClient_ConfidentialEmitsPlaintextSecretOnce(t *testing.T) {
	store := &fakeOAuthClientStore{}
	h := newOAuthHandler(store)
	rec, body := callJSON(t, http.MethodPost, "/oauth-clients", "", validCreateBody(), h.CreateOAuthClient)
	if rec.Code != http.StatusCreated {
		t.Fatalf("code=%d, want 201; body=%s", rec.Code, rec.Body)
	}
	data := body["data"].(map[string]any)
	secret, ok := data["clientSecret"].(string)
	if !ok || !strings.HasPrefix(secret, "nx_cs_") {
		t.Fatalf("expected nx_cs_-prefixed secret in create response, got %v", data["clientSecret"])
	}
	if len(store.createCalls) != 1 || store.createCalls[0].SecretHash == nil {
		t.Fatal("confidential create must store a non-nil secret hash")
	}
}

func TestCreateOAuthClient_PublicNoSecret(t *testing.T) {
	store := &fakeOAuthClientStore{}
	h := newOAuthHandler(store)
	body := validCreateBody()
	body["type"] = "public"
	rec, parsed := callJSON(t, http.MethodPost, "/oauth-clients", "", body, h.CreateOAuthClient)
	if rec.Code != http.StatusCreated {
		t.Fatalf("code=%d, want 201", rec.Code)
	}
	data := parsed["data"].(map[string]any)
	if _, has := data["clientSecret"]; has {
		t.Fatal("public client create must NOT include clientSecret in response")
	}
	if store.createCalls[0].SecretHash != nil {
		t.Fatal("public client create must NOT store a secret hash")
	}
}

func TestCreateOAuthClient_AppliesSpecDefaults(t *testing.T) {
	store := &fakeOAuthClientStore{}
	h := newOAuthHandler(store)
	body := map[string]any{
		"id":           "min-id",
		"name":         "Min",
		"redirectUris": []string{"https://x/cb"},
	}
	rec, _ := callJSON(t, http.MethodPost, "/oauth-clients", "", body, h.CreateOAuthClient)
	if rec.Code != http.StatusCreated {
		t.Fatalf("code=%d, want 201", rec.Code)
	}
	got := store.createCalls[0]
	if got.Type != "confidential" ||
		got.AccessTTLSeconds != 3600 || got.RefreshTTLSeconds != 86400 ||
		len(got.AllowedScopes) != 3 || got.AllowedScopes[0] != "openid" {
		t.Fatalf("spec defaults not applied: %+v", got)
	}
}

func TestCreateOAuthClient_InvalidID(t *testing.T) {
	h := newOAuthHandler(&fakeOAuthClientStore{})
	body := validCreateBody()
	body["id"] = "BAD-CASE"
	rec, _ := callJSON(t, http.MethodPost, "/oauth-clients", "", body, h.CreateOAuthClient)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d, want 400", rec.Code)
	}
}

func TestCreateOAuthClient_InvalidName(t *testing.T) {
	h := newOAuthHandler(&fakeOAuthClientStore{})
	body := validCreateBody()
	body["name"] = ""
	rec, _ := callJSON(t, http.MethodPost, "/oauth-clients", "", body, h.CreateOAuthClient)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d, want 400", rec.Code)
	}
}

func TestCreateOAuthClient_InvalidType(t *testing.T) {
	h := newOAuthHandler(&fakeOAuthClientStore{})
	body := validCreateBody()
	body["type"] = "weird"
	rec, _ := callJSON(t, http.MethodPost, "/oauth-clients", "", body, h.CreateOAuthClient)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d, want 400", rec.Code)
	}
}

func TestCreateOAuthClient_InvalidRedirectURI(t *testing.T) {
	h := newOAuthHandler(&fakeOAuthClientStore{})
	body := validCreateBody()
	body["redirectUris"] = []string{"http://evil.example/cb"}
	rec, _ := callJSON(t, http.MethodPost, "/oauth-clients", "", body, h.CreateOAuthClient)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d, want 400", rec.Code)
	}
}

func TestCreateOAuthClient_LocalhostRedirectAccepted(t *testing.T) {
	h := newOAuthHandler(&fakeOAuthClientStore{})
	body := validCreateBody()
	body["redirectUris"] = []string{"http://localhost:8080/cb", "http://127.0.0.1:9000/cb"}
	rec, _ := callJSON(t, http.MethodPost, "/oauth-clients", "", body, h.CreateOAuthClient)
	if rec.Code != http.StatusCreated {
		t.Fatalf("code=%d, want 201; body=%s", rec.Code, rec.Body)
	}
}

func TestCreateOAuthClient_LoopbackPortWildcardAccepted(t *testing.T) {
	// Regression: the "tui" CLI client registers http://127.0.0.1:*/callback
	// (RFC 8252 §7.3 loopback port wildcard). The old validator's bare
	// url.Parse rejected ":*" as an invalid port, so Create/Update 400'd and
	// the admin form's Save stayed disabled.
	h := newOAuthHandler(&fakeOAuthClientStore{})
	body := validCreateBody()
	body["redirectUris"] = []string{"http://127.0.0.1:*/callback", "http://[::1]:*/callback"}
	rec, _ := callJSON(t, http.MethodPost, "/oauth-clients", "", body, h.CreateOAuthClient)
	if rec.Code != http.StatusCreated {
		t.Fatalf("code=%d, want 201; body=%s", rec.Code, rec.Body)
	}
}

func TestCreateOAuthClient_LocalhostPortWildcardAccepted(t *testing.T) {
	// localhost + ":*" is honored by the authorize-time matchLoopback, so
	// registration accepts it too (RFC 8252 prefers IP literals, but tooling
	// that registers a localhost callback is common).
	h := newOAuthHandler(&fakeOAuthClientStore{})
	body := validCreateBody()
	body["redirectUris"] = []string{"http://localhost:*/callback"}
	rec, _ := callJSON(t, http.MethodPost, "/oauth-clients", "", body, h.CreateOAuthClient)
	if rec.Code != http.StatusCreated {
		t.Fatalf("code=%d, want 201; body=%s", rec.Code, rec.Body)
	}
}

func TestCreateOAuthClient_RedirectURIsEmptyRejected(t *testing.T) {
	h := newOAuthHandler(&fakeOAuthClientStore{})
	body := validCreateBody()
	body["redirectUris"] = []string{}
	rec, _ := callJSON(t, http.MethodPost, "/oauth-clients", "", body, h.CreateOAuthClient)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d, want 400", rec.Code)
	}
}

func TestCreateOAuthClient_InvalidScopeName(t *testing.T) {
	h := newOAuthHandler(&fakeOAuthClientStore{})
	body := validCreateBody()
	body["allowedScopes"] = []string{"NOT lowercase"}
	rec, _ := callJSON(t, http.MethodPost, "/oauth-clients", "", body, h.CreateOAuthClient)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d, want 400", rec.Code)
	}
}

func TestCreateOAuthClient_InvalidAccessTTL(t *testing.T) {
	h := newOAuthHandler(&fakeOAuthClientStore{})
	body := validCreateBody()
	body["accessTtlSeconds"] = 30
	rec, _ := callJSON(t, http.MethodPost, "/oauth-clients", "", body, h.CreateOAuthClient)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d, want 400", rec.Code)
	}
}

func TestCreateOAuthClient_InvalidRefreshTTL(t *testing.T) {
	h := newOAuthHandler(&fakeOAuthClientStore{})
	body := validCreateBody()
	body["refreshTtlSeconds"] = 30
	rec, _ := callJSON(t, http.MethodPost, "/oauth-clients", "", body, h.CreateOAuthClient)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d, want 400", rec.Code)
	}
}

func TestCreateOAuthClient_DuplicateID(t *testing.T) {
	h := newOAuthHandler(&fakeOAuthClientStore{createErr: authstore.ErrClientIDExists})
	rec, _ := callJSON(t, http.MethodPost, "/oauth-clients", "", validCreateBody(), h.CreateOAuthClient)
	if rec.Code != http.StatusConflict {
		t.Fatalf("code=%d, want 409", rec.Code)
	}
}

func TestCreateOAuthClient_StoreGenericError(t *testing.T) {
	h := newOAuthHandler(&fakeOAuthClientStore{createErr: errors.New("db")})
	rec, _ := callJSON(t, http.MethodPost, "/oauth-clients", "", validCreateBody(), h.CreateOAuthClient)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code=%d, want 500", rec.Code)
	}
}

func TestCreateOAuthClient_BadJSON(t *testing.T) {
	h := newOAuthHandler(&fakeOAuthClientStore{})
	req := httptest.NewRequest(http.MethodPost, "/oauth-clients", strings.NewReader("not-json"))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)
	_ = h.CreateOAuthClient(c)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d, want 400", rec.Code)
	}
}

func TestUpdateOAuthClient_HappyPath(t *testing.T) {
	store := &fakeOAuthClientStore{updateResult: sampleOAuthClient()}
	h := newOAuthHandler(store)
	rec, _ := callJSON(t, http.MethodPatch, "/oauth-clients/c1", "c1",
		map[string]any{"name": "Renamed"}, h.UpdateOAuthClient)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d, want 200; body=%s", rec.Code, rec.Body)
	}
	if len(store.updateCalls) != 1 || store.updateCalls[0].Name == nil {
		t.Fatal("UpdateInput.Name should be set")
	}
}

func TestUpdateOAuthClient_NotFound(t *testing.T) {
	h := newOAuthHandler(&fakeOAuthClientStore{updateErr: authstore.ErrClientNotFound})
	rec, _ := callJSON(t, http.MethodPatch, "/oauth-clients/x", "x",
		map[string]any{"name": "X"}, h.UpdateOAuthClient)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d, want 404", rec.Code)
	}
}

func TestUpdateOAuthClient_ValidationError(t *testing.T) {
	h := newOAuthHandler(&fakeOAuthClientStore{})
	rec, _ := callJSON(t, http.MethodPatch, "/oauth-clients/c1", "c1",
		map[string]any{"name": ""}, h.UpdateOAuthClient)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d, want 400", rec.Code)
	}
}

func TestUpdateOAuthClient_GenericStoreError(t *testing.T) {
	h := newOAuthHandler(&fakeOAuthClientStore{updateErr: errors.New("db")})
	rec, _ := callJSON(t, http.MethodPatch, "/oauth-clients/c1", "c1",
		map[string]any{"name": "X"}, h.UpdateOAuthClient)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code=%d, want 500", rec.Code)
	}
}

func TestUpdateOAuthClient_BadJSON(t *testing.T) {
	h := newOAuthHandler(&fakeOAuthClientStore{})
	req := httptest.NewRequest(http.MethodPatch, "/oauth-clients/c1", strings.NewReader("not-json"))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)
	c.SetParamNames("id")
	c.SetParamValues("c1")
	_ = h.UpdateOAuthClient(c)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d, want 400", rec.Code)
	}
}

func TestUpdateOAuthClient_InvalidRedirectURI(t *testing.T) {
	h := newOAuthHandler(&fakeOAuthClientStore{})
	rec, _ := callJSON(t, http.MethodPatch, "/oauth-clients/c1", "c1",
		map[string]any{"redirectUris": []string{"http://evil/cb"}}, h.UpdateOAuthClient)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d, want 400", rec.Code)
	}
}

func TestUpdateOAuthClient_InvalidScope(t *testing.T) {
	h := newOAuthHandler(&fakeOAuthClientStore{})
	rec, _ := callJSON(t, http.MethodPatch, "/oauth-clients/c1", "c1",
		map[string]any{"allowedScopes": []string{"BAD"}}, h.UpdateOAuthClient)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d, want 400", rec.Code)
	}
}

func TestUpdateOAuthClient_InvalidAccessTTL(t *testing.T) {
	h := newOAuthHandler(&fakeOAuthClientStore{})
	rec, _ := callJSON(t, http.MethodPatch, "/oauth-clients/c1", "c1",
		map[string]any{"accessTtlSeconds": 1}, h.UpdateOAuthClient)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d, want 400", rec.Code)
	}
}

func TestUpdateOAuthClient_InvalidRefreshTTL(t *testing.T) {
	h := newOAuthHandler(&fakeOAuthClientStore{})
	rec, _ := callJSON(t, http.MethodPatch, "/oauth-clients/c1", "c1",
		map[string]any{"refreshTtlSeconds": 1}, h.UpdateOAuthClient)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d, want 400", rec.Code)
	}
}

func TestRotateOAuthClientSecret_HappyPath(t *testing.T) {
	store := &fakeOAuthClientStore{
		getResult: sampleOAuthClient(), rotateResult: sampleOAuthClient(),
	}
	h := newOAuthHandler(store)
	rec, body := callJSON(t, http.MethodPost, "/oauth-clients/c1/rotate-secret", "c1", nil, h.RotateOAuthClientSecret)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d, want 200", rec.Code)
	}
	data := body["data"].(map[string]any)
	if secret, _ := data["clientSecret"].(string); !strings.HasPrefix(secret, "nx_cs_") {
		t.Fatalf("expected nx_cs_-prefixed secret, got %v", data["clientSecret"])
	}
	if store.rotateCalls != 1 {
		t.Fatalf("rotate called %d times, want 1", store.rotateCalls)
	}
}

func TestRotateOAuthClientSecret_PublicClientRejected(t *testing.T) {
	pub := sampleOAuthClient()
	pub.Type = "public"
	h := newOAuthHandler(&fakeOAuthClientStore{getResult: pub})
	rec, _ := callJSON(t, http.MethodPost, "/oauth-clients/c1/rotate-secret", "c1", nil, h.RotateOAuthClientSecret)
	if rec.Code != http.StatusConflict {
		t.Fatalf("code=%d, want 409", rec.Code)
	}
}

func TestRotateOAuthClientSecret_LookupNotFound(t *testing.T) {
	h := newOAuthHandler(&fakeOAuthClientStore{getErr: authstore.ErrClientNotFound})
	rec, _ := callJSON(t, http.MethodPost, "/oauth-clients/x/rotate-secret", "x", nil, h.RotateOAuthClientSecret)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d, want 404", rec.Code)
	}
}

func TestRotateOAuthClientSecret_LookupGenericError(t *testing.T) {
	h := newOAuthHandler(&fakeOAuthClientStore{getErr: errors.New("db")})
	rec, _ := callJSON(t, http.MethodPost, "/oauth-clients/x/rotate-secret", "x", nil, h.RotateOAuthClientSecret)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code=%d, want 500", rec.Code)
	}
}

func TestRotateOAuthClientSecret_StoreNotFoundDuringRotation(t *testing.T) {
	// Concurrent delete between GetByID and RotateSecret.
	h := newOAuthHandler(&fakeOAuthClientStore{
		getResult: sampleOAuthClient(), rotateErr: authstore.ErrClientNotFound,
	})
	rec, _ := callJSON(t, http.MethodPost, "/oauth-clients/c1/rotate-secret", "c1", nil, h.RotateOAuthClientSecret)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d, want 404", rec.Code)
	}
}

func TestRotateOAuthClientSecret_StoreGenericError(t *testing.T) {
	h := newOAuthHandler(&fakeOAuthClientStore{
		getResult: sampleOAuthClient(), rotateErr: errors.New("db"),
	})
	rec, _ := callJSON(t, http.MethodPost, "/oauth-clients/c1/rotate-secret", "c1", nil, h.RotateOAuthClientSecret)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code=%d, want 500", rec.Code)
	}
}

func TestDeleteOAuthClient_HappyPath(t *testing.T) {
	store := &fakeOAuthClientStore{getResult: sampleOAuthClient()}
	h := newOAuthHandler(store)
	rec, _ := callJSON(t, http.MethodDelete, "/oauth-clients/c1", "c1", nil, h.DeleteOAuthClient)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("code=%d, want 204", rec.Code)
	}
	if len(store.deleteCalls) != 1 || store.deleteCalls[0] != "c1" {
		t.Fatalf("delete calls=%v", store.deleteCalls)
	}
}

func TestDeleteOAuthClient_NotFound(t *testing.T) {
	h := newOAuthHandler(&fakeOAuthClientStore{getErr: authstore.ErrClientNotFound})
	rec, _ := callJSON(t, http.MethodDelete, "/oauth-clients/x", "x", nil, h.DeleteOAuthClient)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d, want 404", rec.Code)
	}
}

func TestDeleteOAuthClient_LookupGenericError(t *testing.T) {
	h := newOAuthHandler(&fakeOAuthClientStore{getErr: errors.New("db")})
	rec, _ := callJSON(t, http.MethodDelete, "/oauth-clients/x", "x", nil, h.DeleteOAuthClient)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code=%d, want 500", rec.Code)
	}
}

func TestDeleteOAuthClient_DeleteNotFoundConcurrent(t *testing.T) {
	// Row vanished between snapshot Get and Delete.
	h := newOAuthHandler(&fakeOAuthClientStore{
		getResult: sampleOAuthClient(), deleteErr: authstore.ErrClientNotFound,
	})
	rec, _ := callJSON(t, http.MethodDelete, "/oauth-clients/c1", "c1", nil, h.DeleteOAuthClient)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d, want 404", rec.Code)
	}
}

func TestDeleteOAuthClient_DeleteGenericError(t *testing.T) {
	h := newOAuthHandler(&fakeOAuthClientStore{
		getResult: sampleOAuthClient(), deleteErr: errors.New("db"),
	})
	rec, _ := callJSON(t, http.MethodDelete, "/oauth-clients/c1", "c1", nil, h.DeleteOAuthClient)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code=%d, want 500", rec.Code)
	}
}

// TestOAuthClientPublic_NilLastRotated covers the nil-pointer branch of the
// projection helper. Without this the lastSecretRotatedAt key is left out
// vs. explicitly nil — the detail page distinguishes the two.
func TestOAuthClientPublic_NilLastRotated(t *testing.T) {
	row := sampleOAuthClient()
	row.LastSecretRotatedAt = nil
	body := oauthClientPublic(row)
	if v, ok := body["lastSecretRotatedAt"]; !ok || v != nil {
		t.Fatalf("expected explicit nil lastSecretRotatedAt key, got %v / present=%v", v, ok)
	}
}

func TestOAuthClientPublic_SetLastRotated(t *testing.T) {
	row := sampleOAuthClient()
	now := time.Now()
	row.LastSecretRotatedAt = &now
	body := oauthClientPublic(row)
	if _, ok := body["lastSecretRotatedAt"].(time.Time); !ok {
		t.Fatalf("expected time.Time value, got %T", body["lastSecretRotatedAt"])
	}
}

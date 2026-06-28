package login_test

import (
	"errors"
	"github.com/goccy/go-json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/login"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/store"
)

// newIdpsMockCtx builds an echo.Context wrapping GET /authserver/idps with
// the supplied authctx query param. Tests use it to keep recorder + ctx
// construction in a single helper.
func newIdpsMockCtx(authctx string) (echo.Context, *httptest.ResponseRecorder) {
	target := "/authserver/idps"
	if authctx != "" {
		target += "?authctx=" + authctx
	}
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rec := httptest.NewRecorder()
	return echo.New().NewContext(req, rec), rec
}

// newLivePending returns a PendingAuthzStore with one fresh entry under
// authctx. Tests close the store via t.Cleanup so the janitor goroutine
// does not leak across cases.
func newLivePending(t *testing.T, authctx string) *store.PendingAuthzStore {
	t.Helper()
	p := store.NewPendingAuthzStore()
	t.Cleanup(p.Close)
	p.Put(authctx, store.PendingAuthzEntry{
		ClientID:    "cli",
		RedirectURI: "http://127.0.0.1:9/cb",
		Scope:       "openid",
		ExpiresAt:   time.Now().Add(5 * time.Minute),
	})
	return p
}

// TestIdpsHandler_Mock_Success drives the happy path through pgxmock —
// proving the handler iterates rows and emits ID/Type/Name unchanged
// without leaking config/jitEnabled fields.
func TestIdpsHandler_Mock_Success(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("new mock: %v", err)
	}
	t.Cleanup(mock.Close)

	rows := pgxmock.NewRows([]string{
		"id", "type", "name", "enabled", "config", "defaultRole", "defaultControlPlaneAccess", "jitEnabled",
	}).
		AddRow("idp-local-1", "local", "Local IdP", true, []byte(`{}`), "developer", false, true).
		AddRow("idp-oidc-1", "oidc", "Corp Okta", true, []byte(`{"clientId":"abc"}`), "developer", false, true)
	mock.ExpectQuery(`SELECT id, type, name, enabled, config, "defaultRole", "defaultControlPlaneAccess", "jitEnabled"`).
		WillReturnRows(rows)

	authctx := "ctx-success-" + time.Now().Format("150405.000000000")
	deps := login.IdPsDeps{
		IdPs:    store.NewIdPStoreWithPool(mock),
		Pending: newLivePending(t, authctx),
	}

	c, rec := newIdpsMockCtx(authctx)
	if err := login.IdpsHandler(deps)(c); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}

	var resp login.IdpListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(resp.Providers) != 2 {
		t.Fatalf("providers: got %d, want 2", len(resp.Providers))
	}
	if resp.Providers[0].ID != "idp-local-1" || resp.Providers[0].Type != "local" || resp.Providers[0].Name != "Local IdP" {
		t.Fatalf("local row mismatch: %+v", resp.Providers[0])
	}
	if resp.Providers[1].ID != "idp-oidc-1" || resp.Providers[1].Type != "oidc" || resp.Providers[1].Name != "Corp Okta" {
		t.Fatalf("oidc row mismatch: %+v", resp.Providers[1])
	}

	// Sensitive: the SPA-facing response MUST NOT leak any field beyond
	// ID/Type/Name. Re-marshal a single entry and assert the JSON keys.
	raw, _ := json.Marshal(resp.Providers[0])
	var keys map[string]any
	_ = json.Unmarshal(raw, &keys)
	if _, ok := keys["enabled"]; ok {
		t.Fatal("response leaks `enabled` field")
	}
	if _, ok := keys["config"]; ok {
		t.Fatal("response leaks `config` field (contains secrets such as clientSecret)")
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestIdpsHandler_Mock_EmptyAuthctx asserts the security invariant that
// the IdP list is not enumerable without a live pending authorize handle.
// Without this check, an unauthenticated visitor could call
// /authserver/idps directly and learn which SSO providers are configured.
func TestIdpsHandler_Mock_EmptyAuthctx(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	t.Cleanup(mock.Close)
	deps := login.IdPsDeps{
		IdPs:    store.NewIdPStoreWithPool(mock),
		Pending: newLivePending(t, "ignored"),
	}

	c, rec := newIdpsMockCtx("")
	if err := login.IdpsHandler(deps)(c); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", rec.Code)
	}
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["error"] != "authctx_expired" {
		t.Fatalf("error: got %q, want authctx_expired", body["error"])
	}
	// The DB MUST NOT be queried when the gate fails — that is the
	// pre-condition of the gate. ExpectationsWereMet verifies no query
	// was issued.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("DB was queried despite missing authctx: %v", err)
	}
}

// TestIdpsHandler_Mock_UnknownAuthctx mirrors the no-authctx case for a
// non-empty but never-seen handle. Same 400/authctx_expired response,
// same no-DB-touch invariant.
func TestIdpsHandler_Mock_UnknownAuthctx(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	t.Cleanup(mock.Close)
	deps := login.IdPsDeps{
		IdPs:    store.NewIdPStoreWithPool(mock),
		Pending: newLivePending(t, "real-authctx"),
	}

	c, rec := newIdpsMockCtx("forged-authctx")
	if err := login.IdpsHandler(deps)(c); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", rec.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("DB was queried with forged authctx: %v", err)
	}
}

// TestIdpsHandler_Mock_ExpiredAuthctx puts an already-expired entry into
// Pending. Has should reject it as if it never existed.
func TestIdpsHandler_Mock_ExpiredAuthctx(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	t.Cleanup(mock.Close)

	pending := store.NewPendingAuthzStore()
	t.Cleanup(pending.Close)
	pending.Put("expired-ctx", store.PendingAuthzEntry{
		ClientID:    "cli",
		RedirectURI: "http://127.0.0.1:9/cb",
		ExpiresAt:   time.Now().Add(-time.Second),
	})

	deps := login.IdPsDeps{
		IdPs:    store.NewIdPStoreWithPool(mock),
		Pending: pending,
	}
	c, rec := newIdpsMockCtx("expired-ctx")
	if err := login.IdpsHandler(deps)(c); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400 (expired)", rec.Code)
	}
}

// TestIdpsHandler_Mock_StoreFailure exercises the 500 path: a DB error
// during ListEnabled must surface as `internal_error` without leaking
// the underlying SQL error to the response body.
func TestIdpsHandler_Mock_StoreFailure(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("new mock: %v", err)
	}
	t.Cleanup(mock.Close)
	mock.ExpectQuery(`SELECT id, type, name, enabled, config, "defaultRole", "defaultControlPlaneAccess", "jitEnabled"`).
		WillReturnError(errors.New("connection refused"))

	authctx := "ctx-err-" + time.Now().Format("150405.000000000")
	deps := login.IdPsDeps{
		IdPs:    store.NewIdPStoreWithPool(mock),
		Pending: newLivePending(t, authctx),
	}

	c, rec := newIdpsMockCtx(authctx)
	if err := login.IdpsHandler(deps)(c); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500", rec.Code)
	}
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["error"] != "internal_error" {
		t.Fatalf("error: got %q, want internal_error", body["error"])
	}
	// Crucial: the raw driver error must NOT bleed into the JSON body.
	if bytes := rec.Body.String(); bytesContains(bytes, "connection refused") {
		t.Fatalf("response leaks driver error: %q", bytes)
	}
}

func bytesContains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

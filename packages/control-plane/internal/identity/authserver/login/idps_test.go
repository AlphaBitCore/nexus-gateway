package login_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/login"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/store/storetest"
)

// seedIdpsFixture seeds two enabled IdPs (one local, one oidc) and returns
// a wired-up IdPsDeps plus a live authctx for the Has-check to succeed.
type idpsFixture struct {
	deps       login.IdPsDeps
	authctx    string
	localIdPID string
	oidcIdPID  string
}

func seedIdpsFixture(t *testing.T) idpsFixture {
	t.Helper()
	pool := storetest.Open(t)
	ctx := context.Background()
	suffix := time.Now().Format("150405.000000000")

	var localID, oidcID string
	err := pool.QueryRow(ctx,
		`INSERT INTO "IdentityProvider"(type,name,enabled,config,"defaultRole","jitEnabled","updatedAt")
		 VALUES ('local','test-local-idps-'||$1,TRUE,'{}'::jsonb,'developer',TRUE,NOW())
		 RETURNING id`, suffix).Scan(&localID)
	if err != nil {
		t.Fatalf("seed local idp: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM "IdentityProvider" WHERE id=$1`, localID) })

	err = pool.QueryRow(ctx,
		`INSERT INTO "IdentityProvider"(type,name,enabled,config,"defaultRole","jitEnabled","updatedAt")
		 VALUES ('oidc','test-oidc-idps-'||$1,TRUE,'{}'::jsonb,'developer',TRUE,NOW())
		 RETURNING id`, suffix).Scan(&oidcID)
	if err != nil {
		t.Fatalf("seed oidc idp: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM "IdentityProvider" WHERE id=$1`, oidcID) })

	idps := store.NewIdPStore(pool)
	pending := store.NewPendingAuthzStore()
	t.Cleanup(pending.Close)

	authctx := store.RandomOpaqueToken(16)
	pending.Put(authctx, store.PendingAuthzEntry{
		ClientID:    "test-client",
		RedirectURI: "http://127.0.0.1:9/cb",
		Scope:       "openid",
		ExpiresAt:   time.Now().Add(5 * time.Minute),
	})

	return idpsFixture{
		deps:       login.IdPsDeps{IdPs: idps, Pending: pending},
		authctx:    authctx,
		localIdPID: localID,
		oidcIdPID:  oidcID,
	}
}

func getIdps(authctx string) (echo.Context, *httptest.ResponseRecorder) {
	req := httptest.NewRequest(http.MethodGet, "/authserver/idps?authctx="+authctx, nil)
	rec := httptest.NewRecorder()
	return echo.New().NewContext(req, rec), rec
}

// TestIdpsHandler_ListsEnabledProviders seeds two enabled IdPs and confirms
// both come back in the response. Filtering is exercised by IdPStore tests;
// here we only verify the handler passes the data through correctly.
func TestIdpsHandler_ListsEnabledProviders(t *testing.T) {
	fx := seedIdpsFixture(t)

	c, rec := getIdps(fx.authctx)
	if err := login.IdpsHandler(fx.deps)(c); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200 (%q)", rec.Code, rec.Body.String())
	}

	var resp login.IdpListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v (%q)", err, rec.Body.String())
	}

	// The store returns rows from the shared test DB, so we cannot assert an
	// exact count — filter down to the rows this test seeded.
	types := map[string]bool{}
	for _, p := range resp.Providers {
		if p.ID == fx.localIdPID {
			types["local"] = true
			if p.Type != "local" {
				t.Fatalf("local entry: type=%q, want local", p.Type)
			}
		}
		if p.ID == fx.oidcIdPID {
			types["oidc"] = true
			if p.Type != "oidc" {
				t.Fatalf("oidc entry: type=%q, want oidc", p.Type)
			}
		}
	}
	if !types["local"] {
		t.Fatal("local IdP missing from response")
	}
	if !types["oidc"] {
		t.Fatal("oidc IdP missing from response")
	}
}

// TestIdpsHandler_RejectsMissingAuthctx must return 400 authctx_expired when
// the caller omits the query param entirely.
func TestIdpsHandler_RejectsMissingAuthctx(t *testing.T) {
	fx := seedIdpsFixture(t)

	c, rec := getIdps("")
	if err := login.IdpsHandler(fx.deps)(c); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["error"] != "authctx_expired" {
		t.Fatalf("error: got %q, want authctx_expired", body["error"])
	}
}

// TestIdpsHandler_RejectsUnknownAuthctx covers a non-empty but unmatched
// authctx — the same 400 error is returned so the SPA surfaces a clear
// "start again" message.
func TestIdpsHandler_RejectsUnknownAuthctx(t *testing.T) {
	fx := seedIdpsFixture(t)

	c, rec := getIdps("does-not-exist")
	if err := login.IdpsHandler(fx.deps)(c); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["error"] != "authctx_expired" {
		t.Fatalf("error: got %q, want authctx_expired", body["error"])
	}
}

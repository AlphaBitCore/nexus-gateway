package login

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/oidcdisco"
)

func oidcIdPRows(id, name string, cfg []byte) *pgxmock.Rows {
	return pgxmock.NewRows([]string{
		"id", "type", "name", "enabled", "config", "defaultRole", "defaultControlPlaneAccess", "jitEnabled",
	}).AddRow(id, "oidc", name, true, cfg, "developers", false, true)
}

func newLogoutCtx(idpID string) (echo.Context, *httptest.ResponseRecorder) {
	req := httptest.NewRequest(http.MethodGet, "/authserver/idp/"+idpID+"/logout", nil)
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)
	c.SetParamNames("idpId")
	c.SetParamValues(idpID)
	return c, rec
}

func TestLogoutHandler(t *testing.T) {
	t.Run("OIDC with end_session → IdP logout w/ post_logout_redirect_uri + client_id", func(t *testing.T) {
		disco := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]string{
				"authorization_endpoint": "https://idp/auth",
				"token_endpoint":         "https://idp/token",
				"jwks_uri":               "https://idp/jwks",
				"end_session_endpoint":   "https://idp/logout",
			})
		}))
		t.Cleanup(disco.Close)

		mock, _ := pgxmock.NewPool()
		t.Cleanup(mock.Close)
		cfg, _ := json.Marshal(map[string]any{"issuer": disco.URL, "clientId": "cli-1"})
		mock.ExpectQuery(idpRowQuery).WithArgs("idp-1").WillReturnRows(oidcIdPRows("idp-1", "Okta", cfg))

		d := StartDeps{IdPs: store.NewIdPStoreWithPool(mock), Issuer: "https://cp.test/", Resolver: oidcdisco.NewResolver()}
		c, rec := newLogoutCtx("idp-1")
		if err := LogoutHandler(d)(c); err != nil {
			t.Fatalf("handler: %v", err)
		}
		if rec.Code != http.StatusFound {
			t.Fatalf("status %d, want 302", rec.Code)
		}
		loc, _ := url.Parse(rec.Header().Get("Location"))
		if loc.Scheme+"://"+loc.Host+loc.Path != "https://idp/logout" {
			t.Errorf("redirect base = %q, want https://idp/logout", loc.String())
		}
		if got := loc.Query().Get("post_logout_redirect_uri"); got != "https://cp.test/login" {
			t.Errorf("post_logout_redirect_uri = %q", got)
		}
		if got := loc.Query().Get("client_id"); got != "cli-1" {
			t.Errorf("client_id = %q", got)
		}
	})

	t.Run("non-OIDC IdP → /login", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		t.Cleanup(mock.Close)
		mock.ExpectQuery(idpRowQuery).WithArgs("idp-2").
			WillReturnRows(samlIdPRows("idp-2", "Acme", true, true, []byte(`{}`)))
		d := StartDeps{IdPs: store.NewIdPStoreWithPool(mock), Issuer: "https://cp.test", Resolver: oidcdisco.NewResolver()}
		c, rec := newLogoutCtx("idp-2")
		_ = LogoutHandler(d)(c)
		if rec.Code != http.StatusFound || rec.Header().Get("Location") != loginPagePath {
			t.Errorf("want 302 → %s, got %d → %q", loginPagePath, rec.Code, rec.Header().Get("Location"))
		}
	})
}

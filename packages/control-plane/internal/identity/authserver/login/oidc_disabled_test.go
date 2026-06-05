package login_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/login"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/store"
)

// TestOIDCCallbackHandler_DisabledIdP_Rejected covers the !idp.Enabled guard in
// the OIDC callback (parity with the SAML ACS disabled-IdP test): disabling an
// IdP must invalidate in-flight logins, not just hide it from the picker. The
// IdP row is a valid OIDC config (non-empty TokenURL) but enabled=false, so the
// rejection is specifically the Enabled check, not the config-shape guard.
func TestOIDCCallbackHandler_DisabledIdP_Rejected(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	t.Cleanup(mock.Close)

	cfg := oidcConfigJSON("https://idp/authorize", "https://idp/token",
		"https://idp/jwks", "cid", "https://app/cb", "aud")
	mock.ExpectQuery(idpQuery).WithArgs("oidc-1").
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "type", "name", "enabled", "config", "defaultRole", "defaultControlPlaneAccess", "jitEnabled",
		}).AddRow("oidc-1", "oidc", "Okta", false, cfg, "developer", false, true))

	authctx := "ctx-disabled"
	pending := store.NewPendingAuthzStore()
	t.Cleanup(pending.Close)
	pending.Put(authctx, store.PendingAuthzEntry{
		ClientID: "cli", RedirectURI: "http://127.0.0.1/cb", IdPID: "oidc-1",
		ExpiresAt: time.Now().Add(5 * time.Minute),
	})

	deps := login.OIDCDeps{
		IdPs:      store.NewIdPStoreWithPool(mock),
		Federated: store.NewFederatedStoreWithPool(nil),
		Pending:   pending,
		AuthCodes: store.NewAuthCodeStore(5 * time.Minute),
	}
	t.Cleanup(deps.AuthCodes.Close)

	c, rec := newOIDCCallbackCtx("the-code", authctx)
	_ = login.OIDCCallbackHandler(deps)(c)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d (body=%q), want 400 for disabled IdP", rec.Code, rec.Body.String())
	}
}

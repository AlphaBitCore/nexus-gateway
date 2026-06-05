package login

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/store"
)

// --- resolveOrProvision branches (direct) ---

func TestResolveOrProvision(t *testing.T) {
	const findQ = `FROM "UserFederatedIdentity"`
	idp := func(jit bool) *store.IdentityProvider {
		return &store.IdentityProvider{ID: "idp-1", Type: "saml", Enabled: true, JITEnabled: jit}
	}

	t.Run("FindByIdPSubject error -> internal", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		t.Cleanup(mock.Close)
		mock.ExpectQuery(findQ).WillReturnError(context.DeadlineExceeded)
		d := SAMLDeps{Federated: store.NewFederatedStoreWithPool(mock)}
		_, errStr := d.resolveOrProvision(context.Background(), idp(true), "x", "", "x@y", nil)
		if errStr != errInternal {
			t.Fatalf("errStr = %q, want internal", errStr)
		}
	})

	t.Run("not found + JIT disabled -> user_not_provisioned", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		t.Cleanup(mock.Close)
		mock.ExpectQuery(findQ).WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).WillReturnRows(pgxmock.NewRows([]string{"id", "userId", "idpId", "externalSubject", "externalEmail", "rawClaims", "linkedAt", "lastLoginAt"}))
		d := SAMLDeps{Federated: store.NewFederatedStoreWithPool(mock)}
		_, errStr := d.resolveOrProvision(context.Background(), idp(false), "x", "", "x@y", nil)
		if errStr != "user_not_provisioned" {
			t.Fatalf("errStr = %q, want user_not_provisioned", errStr)
		}
	})

	t.Run("not found + JIT enabled + provision fails -> internal", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		t.Cleanup(mock.Close)
		mock.ExpectQuery(findQ).WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).WillReturnRows(pgxmock.NewRows([]string{"id", "userId", "idpId", "externalSubject", "externalEmail", "rawClaims", "linkedAt", "lastLoginAt"}))
		mock.ExpectBegin().WillReturnError(context.DeadlineExceeded)
		d := SAMLDeps{Federated: store.NewFederatedStoreWithPool(mock)}
		_, errStr := d.resolveOrProvision(context.Background(), idp(true), "x", "", "x@y", nil)
		if errStr != errInternal {
			t.Fatalf("errStr = %q, want internal", errStr)
		}
	})

	t.Run("not found + JIT enabled + provision succeeds -> user id", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		t.Cleanup(mock.Close)
		mock.ExpectQuery(findQ).WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).WillReturnRows(pgxmock.NewRows([]string{"id", "userId", "idpId", "externalSubject", "externalEmail", "rawClaims", "linkedAt", "lastLoginAt"}))
		mock.ExpectBegin()
		mock.ExpectQuery(`SELECT id FROM "Organization"`).
			WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("org-1"))
		// displayName (2nd arg) = the extracted name; source (4th arg) = "saml".
		mock.ExpectQuery(`INSERT INTO "NexusUser"`).WithArgs(pgxmock.AnyArg(), "Bob Acme", pgxmock.AnyArg(), "saml", pgxmock.AnyArg(), pgxmock.AnyArg()).
			WillReturnRows(pgxmock.NewRows([]string{"id", "displayName", "email", "status", "source"}).
				AddRow("user-9", "Bob Acme", nil, "active", "saml"))
		mock.ExpectQuery(`INSERT INTO "UserFederatedIdentity"`).WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
			WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("fi-9"))
		mock.ExpectCommit()
		d := SAMLDeps{Federated: store.NewFederatedStoreWithPool(mock)}
		userID, errStr := d.resolveOrProvision(context.Background(), idp(true), "bob@acme.test", "Bob Acme", "bob@acme.test", nil)
		if errStr != "" || userID != "user-9" {
			t.Fatalf("got (%q, %q), want (user-9, '')", userID, errStr)
		}
	})
}

// --- ACS error branches ---

func TestSAMLACSHandler_MoreFailures(t *testing.T) {
	kp := newTestIDPKeypair(t)
	const idpEntityID = "https://idp.acme.test/metadata"
	cfg := samlConfigJSON(idpEntityID, "https://idp.acme.test/sso", kp.CertPEM)
	sp, _ := buildSAMLServiceProvider(store.DecodeSAMLConfig(&store.IdentityProvider{Type: "saml", Config: map[string]any{
		"entityId": idpEntityID, "ssoUrl": "https://idp.acme.test/sso", "certificatePem": kp.CertPEM,
	}}), samlIssuer)

	mkACS := func(mock pgxmock.PgxPoolIface, authctx, reqID string) (SAMLDeps, *store.PendingAuthzStore, *store.SAMLRequestStore) {
		pending := store.NewPendingAuthzStore()
		reqs := store.NewSAMLRequestStore()
		pending.Put(authctx, store.PendingAuthzEntry{IdPID: "idp-1", RedirectURI: "http://127.0.0.1/cb", ExpiresAt: time.Now().Add(time.Minute)})
		reqs.Put(authctx, reqID)
		return SAMLDeps{
			IdPs: store.NewIdPStoreWithPool(mock), Federated: store.NewFederatedStoreWithPool(mock),
			Pending: pending, AuthCodes: store.NewAuthCodeStore(time.Minute), Requests: reqs, Issuer: samlIssuer,
		}, pending, reqs
	}

	t.Run("GetByID error -> 400", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		t.Cleanup(mock.Close)
		mock.ExpectQuery(idpRowQuery).WillReturnError(context.DeadlineExceeded)
		d, pending, reqs := mkACS(mock, "ctx-g", "id-g")
		t.Cleanup(pending.Close)
		t.Cleanup(reqs.Close)
		t.Cleanup(d.AuthCodes.Close)
		c, rec := newSAMLACSCtx("ctx-g", "x")
		_ = SAMLACSHandler(d)(c)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("got %d, want 400", rec.Code)
		}
	})

	t.Run("incomplete config -> 500 (cannot build SP)", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		t.Cleanup(mock.Close)
		mock.ExpectQuery(idpRowQuery).WithArgs("idp-1").WillReturnRows(samlIdPRows("idp-1", "Bad", true, true, samlConfigJSON("", "", "")))
		d, pending, reqs := mkACS(mock, "ctx-b", "id-b")
		t.Cleanup(pending.Close)
		t.Cleanup(reqs.Close)
		t.Cleanup(d.AuthCodes.Close)
		c, rec := newSAMLACSCtx("ctx-b", "x")
		_ = SAMLACSHandler(d)(c)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("got %d (body=%q), want 500", rec.Code, rec.Body.String())
		}
	})

	t.Run("valid signature but empty NameID -> 401", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		t.Cleanup(mock.Close)
		mock.ExpectQuery(idpRowQuery).WithArgs("idp-1").WillReturnRows(samlIdPRows("idp-1", "Acme", true, true, cfg))
		const reqID = "id-noname"
		d, pending, reqs := mkACS(mock, "ctx-nn", reqID)
		t.Cleanup(pending.Close)
		t.Cleanup(reqs.Close)
		t.Cleanup(d.AuthCodes.Close)
		resp := mintSignedSAMLResponse(t, kp, sp, idpEntityID, reqID, "", nil) // empty NameID
		c, rec := newSAMLACSCtx("ctx-nn", resp)
		_ = SAMLACSHandler(d)(c)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("got %d (body=%q), want 401", rec.Code, rec.Body.String())
		}
	})
}

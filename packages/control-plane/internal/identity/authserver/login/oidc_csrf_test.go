package login_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/login"
)

// newOIDCCallbackCtxWithCookie builds the callback echo context like
// newOIDCCallbackCtx but attaches the named cookie, so the CSRF tests can
// present (or withhold, or mismatch) the signed state cookie the handler reads.
func newOIDCCallbackCtxWithCookie(code, state string, cookie *http.Cookie) (echo.Context, *httptest.ResponseRecorder) {
	target := "/authserver/oidc/callback"
	v := url.Values{}
	if code != "" {
		v.Set("code", code)
	}
	if state != "" {
		v.Set("state", state)
	}
	if q := v.Encode(); q != "" {
		target += "?" + q
	}
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	return echo.New().NewContext(req, rec), rec
}

// assertCallbackStateRejected drives the handler with the given cookie and
// asserts it returns 400 state_cookie_mismatch and does NOT consume the pending
// handle (the rejection happens before Pending.Take).
func assertCallbackStateRejected(t *testing.T, fx callbackFixture, cookie *http.Cookie) {
	t.Helper()
	c, rec := newOIDCCallbackCtxWithCookie("auth-code-xyz", fx.authctx, cookie)
	if err := login.OIDCCallbackHandler(fx.deps)(c); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400 (body=%q)", rec.Code, rec.Body.String())
	}
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["error"] != login.ErrStateCookieMismatch {
		t.Fatalf("error: got %q, want %q", body["error"], login.ErrStateCookieMismatch)
	}
	// The single-use authctx must survive: a rejected CSRF callback must not
	// burn the legitimate browser's pending handle.
	if _, ok := fx.pending.Take(fx.authctx); !ok {
		t.Fatal("pending handle was consumed by a rejected callback")
	}
}

// TestOIDCCallback_NoStateCookie_Rejected — a callback that arrives without the
// signed cookie (the login-CSRF case: attacker forces the victim's browser to
// the callback URL, but the victim's browser carries no oidc_state cookie for
// the attacker's authctx) is rejected with 400.
func TestOIDCCallback_NoStateCookie_Rejected(t *testing.T) {
	fx := newCallbackFixture(t, false)
	signer, err := login.NewRandomStateSigner()
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	fx.deps.StateSigner = signer
	assertCallbackStateRejected(t, fx, nil)
}

// TestOIDCCallback_WrongAuthctxCookie_Rejected — the cookie is validly signed
// but binds a different authctx than the `state` query param (an attacker
// replaying their own cookie against a victim's state). Rejected.
func TestOIDCCallback_WrongAuthctxCookie_Rejected(t *testing.T) {
	fx := newCallbackFixture(t, false)
	signer, err := login.NewRandomStateSigner()
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	fx.deps.StateSigner = signer
	// Cookie binds "other-authctx", query state is fx.authctx → mismatch.
	cookie := &http.Cookie{Name: login.OIDCStateCookieName, Value: signer.SignStateForTest("other-authctx")}
	assertCallbackStateRejected(t, fx, cookie)
}

// TestOIDCCallback_TamperedCookie_Rejected — a cookie whose MAC was altered
// fails verification and is rejected.
func TestOIDCCallback_TamperedCookie_Rejected(t *testing.T) {
	fx := newCallbackFixture(t, false)
	signer, err := login.NewRandomStateSigner()
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	fx.deps.StateSigner = signer
	good := signer.SignStateForTest(fx.authctx)
	// Corrupt the first character of the MAC.
	bad := "0" + good[1:]
	if bad == good {
		bad = "1" + good[1:]
	}
	cookie := &http.Cookie{Name: login.OIDCStateCookieName, Value: bad}
	assertCallbackStateRejected(t, fx, cookie)
}

// TestOIDCCallback_ValidCookie_Success — the legitimate path: the browser
// carries a correctly-signed cookie binding the same authctx as `state`, so the
// CSRF gate passes and the login completes (302 with code+state). Also asserts
// the handler clears the state cookie on the way out.
func TestOIDCCallback_ValidCookie_Success(t *testing.T) {
	fx := newCallbackFixture(t, false)
	signer, err := login.NewRandomStateSigner()
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	fx.deps.StateSigner = signer

	expectGetByID(fx.mock, fx.idpID, fx.cfgJSON, false)
	fx.mock.ExpectQuery(`SELECT id, "userId", "idpId"`).
		WithArgs(fx.idpID, fx.subject).
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "userId", "idpId", "externalSubject", "externalEmail", "rawClaims", "linkedAt", "lastLoginAt",
		}).AddRow("fi-1", "user-real", fx.idpID, fx.subject, ptrStr("alice@example.com"), []byte(`{}`), time.Now(), (*time.Time)(nil)))
	fx.mock.ExpectExec(`UPDATE "UserFederatedIdentity" SET "rawClaims"`).
		WithArgs("fi-1", pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	tok := mintIDToken(t, fx.server, fx.subject, "alice@example.com", time.Now().Add(time.Hour))
	fx.server.SetIDToken(tok)

	cookie := &http.Cookie{Name: login.OIDCStateCookieName, Value: signer.SignStateForTest(fx.authctx)}
	c, rec := newOIDCCallbackCtxWithCookie("auth-code-xyz", fx.authctx, cookie)
	if err := login.OIDCCallbackHandler(fx.deps)(c); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if rec.Code != http.StatusFound {
		t.Fatalf("status: got %d, want 302 (body=%q)", rec.Code, rec.Body.String())
	}
	u, _ := url.Parse(rec.Header().Get("Location"))
	if u.Query().Get("code") == "" {
		t.Fatal("code missing from redirect on the valid-cookie path")
	}
	// The handler must expire the state cookie so it cannot be replayed.
	var cleared bool
	for _, ck := range rec.Result().Cookies() {
		if ck.Name == login.OIDCStateCookieName && ck.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatal("state cookie was not cleared after a successful callback")
	}
}

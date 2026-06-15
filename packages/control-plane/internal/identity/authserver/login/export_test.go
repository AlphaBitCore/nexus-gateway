package login

// export_test.go re-exports a few package internals to the external
// login_test package so the OIDC-callback CSRF tests (which live in
// login_test alongside the shared IdP-server fixtures) can mint a valid
// signed state-cookie value and reference the cookie name/error code without
// the production API exposing them. Test-only; not compiled into the binary.

// OIDCStateCookieName is the cookie name startOIDC sets and the callback reads.
const OIDCStateCookieName = oidcStateCookieName

// ErrStateCookieMismatch is the errorResponse code the callback returns when
// the signed state cookie is absent, malformed, or unbound to `state`.
const ErrStateCookieMismatch = errStateCookieMismatch

// SignStateForTest produces the signed cookie value the browser would carry,
// so the success-path callback test can present a cookie the signer accepts.
func (s *StateSigner) SignStateForTest(state string) string { return s.sign(state) }

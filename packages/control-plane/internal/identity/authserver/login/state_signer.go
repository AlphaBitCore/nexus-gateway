package login

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
)

// oidcStateCookieName is the HttpOnly cookie startOIDC sets and the OIDC
// callback verifies. It binds the IdP `state` (the authctx) to the browser that
// initiated the SSO flow: a login-CSRF attacker who replays a captured callback
// URL in a victim's browser cannot also forge this signed cookie, so the
// callback rejects the request (defense-in-depth on top of the single-use,
// 256-bit authctx and the SPA↔CP PKCE leg). Path-scoped to the callback route
// so it is never sent on unrelated requests.
const oidcStateCookieName = "oidc_state"

// oidcStateCookiePath scopes the state cookie to the OIDC callback endpoint:
// the browser only attaches it on the return leg, and clearing it must use the
// identical path or the Set-Cookie delete is ignored by the browser.
const oidcStateCookiePath = "/authserver/oidc/callback"

// oidcStateCookieMaxAge bounds the cookie lifetime to one IdP redirect
// round-trip (5 minutes), matching the pending-authz handle TTL so a stale
// cookie can never outlive its authctx.
const oidcStateCookieMaxAge = 300

// errStateCookieMismatch is the errorResponse code the OIDC callback returns
// when the signed state cookie is absent, malformed, or does not bind to the
// `state` query param — the login-CSRF rejection.
const errStateCookieMismatch = "state_cookie_mismatch"

// StateSigner is the exported alias the authserver mount uses to construct a
// signer and inject it into StartDeps + OIDCDeps. The concrete type stays
// unexported so the sign/verify methods are package-internal; mount only needs
// to mint one and pass it through.
type StateSigner = stateSigner

// NewRandomStateSigner mints a *StateSigner backed by a fresh 32-byte
// crypto/rand key, for the authserver mount to share between the SSO-start and
// OIDC-callback legs. Returns an error only if the system CSPRNG fails.
func NewRandomStateSigner() (*StateSigner, error) { return newRandomStateSigner() }

// stateSigner signs and verifies OIDC state cookies with HMAC-SHA256 under a
// per-process key. The key never leaves memory and is regenerated on restart;
// the only consequence of a restart mid-login is that the in-flight cookie no
// longer verifies and the user re-initiates — an acceptable failure mode for a
// 5-minute defense-in-depth control that needs no persistence.
type stateSigner struct{ key []byte }

// newStateSigner creates a signer with the given key. The key MUST be at least
// 32 bytes of cryptographically random data; callers use newRandomStateSigner
// in production wiring.
func newStateSigner(key []byte) *stateSigner { return &stateSigner{key: key} }

// newRandomStateSigner mints a signer with a fresh 32-byte crypto/rand key.
// Used by the authserver mount to bind state cookies for the process lifetime.
func newRandomStateSigner() (*stateSigner, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	return &stateSigner{key: key}, nil
}

// sign returns "hex(HMAC-SHA256(key, state)).state" — the value placed in the
// oidc_state cookie. The plaintext state rides alongside the MAC so verify can
// recompute the MAC over it without any server-side state.
func (s *stateSigner) sign(state string) string {
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(state))
	return hex.EncodeToString(mac.Sum(nil)) + "." + state
}

// verify returns the embedded state when the signed cookie is intact, or an
// error when the format is wrong or the MAC does not match. Comparison is
// constant-time (hmac.Equal) so a forged cookie cannot be probed byte-by-byte.
func (s *stateSigner) verify(cookie string) (string, error) {
	dot := strings.IndexByte(cookie, '.')
	if dot < 0 {
		return "", errors.New("invalid cookie format")
	}
	sig, state := cookie[:dot], cookie[dot+1:]
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(state))
	want := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(sig), []byte(want)) {
		return "", errors.New("cookie signature mismatch")
	}
	return state, nil
}

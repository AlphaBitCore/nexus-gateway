package login

import (
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/oidcdisco"
)

// loginPagePath is the SPA-hosted login picker. Every non-happy-path outcome of
// the SSO-start endpoint 302s here rather than returning JSON: the login UI
// (method picker + local password form) lives entirely in the front end, so a
// browser that navigated to start must always land back on that page, never on
// a raw server error body. The picker re-fetches the enabled IdP list on load,
// so a provider that was disabled or deleted between picker render and click
// simply disappears from the list on the bounce — no error loop.
const loginPagePath = "/login"

// ssoErrorPagePath is the SPA-hosted terminal page the OIDC callback sends the
// browser to when the IdP returns an error instead of a code. It shows the
// (bounded) OAuth error code and a button back to sign-in; it does not
// auto-redirect, so the operator can read the failure.
const ssoErrorPagePath = "/auth/sso-error"

// StartDeps carries the collaborators the unified SSO-start endpoint needs. It
// is intentionally the minimal initiation surface: resolving the IdP (IdPs),
// validating + stamping the pending authorize handle (Pending), and — for SAML
// — recording the outstanding AuthnRequest ID (Requests) plus the issuer the SP
// identity derives from. The Federated / AuthCode collaborators are absent on
// purpose: those come into play only on the return leg (OIDC callback / SAML
// ACS), after the external IdP has authenticated the user.
type StartDeps struct {
	IdPs     *store.IdPStore
	Pending  *store.PendingAuthzStore
	Requests *store.SAMLRequestStore
	Issuer   string
	// Resolver fills the OIDC authorize/token/jwks endpoints from the IdP
	// issuer's discovery document when the saved config omits them — the
	// admin Add-IdP form collects only the issuer and relies on discovery.
	// Shared with the OIDC callback so a document fetched here is reused on
	// the return leg. May be nil (tests that pin explicit endpoints).
	Resolver *oidcdisco.Resolver
	// StateSigner binds the OIDC `state` (authctx) to the initiating browser
	// via an HMAC-signed HttpOnly cookie set in startOIDC and verified on the
	// callback (login-CSRF defense-in-depth). Shared with OIDCDeps.StateSigner
	// so the same per-process key signs and verifies. May be nil (tests that
	// don't exercise the cookie binding); when nil, startOIDC skips the cookie.
	StateSigner *stateSigner
}

// StartHandler returns GET /authserver/idp/:idpId/start?authctx=<authctx>. It is
// the single browser entry point the SPA login picker navigates to for every
// external IdP, regardless of protocol — the SPA stays protocol-agnostic and
// this handler owns the OIDC-vs-SAML divergence:
//
//   - oidc → 302 redirect to the IdP's authorization endpoint.
//   - saml → 200 with an auto-submitting HTML POST form that delivers the
//     AuthnRequest to the IdP (HTTP-POST binding).
//   - local / unknown → bounce to the login page: the built-in local store
//     authenticates through the SPA's password form (POST /authserver/password),
//     never through SSO start.
//
// The browser reaches start as a full-page navigation (window.location), so
// every branch emits a browser-navigable response (a redirect or an HTML page),
// never JSON. The return leg lands on /authserver/oidc/callback (OIDC) or
// /authserver/saml/acs (SAML).
func StartHandler(d StartDeps) echo.HandlerFunc {
	return func(c echo.Context) error {
		ctx := c.Request().Context()
		authctx := c.QueryParam("authctx")
		if authctx == "" || !d.Pending.Has(authctx) {
			// No live authorize handle: send the browser to the login page with
			// no handle so the SPA re-bootstraps the OAuth dance from scratch.
			return c.Redirect(http.StatusFound, spaPath(c, loginPagePath))
		}

		idp, err := d.IdPs.GetByID(ctx, c.Param("idpId"))
		if err != nil || !idp.Enabled {
			// Missing or disabled IdP: a disabled IdP must not start a login
			// (parity with the callback / ACS Enabled guards). The handle is
			// still live, so keep it on the bounce — the reloaded picker drops
			// the stale provider and the user can pick another.
			return bounceToLogin(c, authctx)
		}

		switch idp.Type {
		case "oidc":
			return startOIDC(c, d, idp, authctx)
		case "saml":
			return startSAML(c, d, idp, authctx)
		default:
			// "local" and any unknown type are not SSO-startable.
			return bounceToLogin(c, authctx)
		}
	}
}

// bounceToLogin 302s the browser back to the SPA login picker, preserving the
// still-live authctx so the picker reloads against the same authorize handle.
func bounceToLogin(c echo.Context, authctx string) error {
	u := url.URL{Path: spaPath(c, loginPagePath), RawQuery: url.Values{"authctx": {authctx}}.Encode()}
	return c.Redirect(http.StatusFound, u.String())
}

// spaPath prepends X-Forwarded-Prefix to a SPA route so backend redirects
// land on the correct path when Nexus is proxied at a sub-path (e.g. /nexus/).
func spaPath(c echo.Context, path string) string {
	prefix := strings.TrimRight(c.Request().Header.Get("X-Forwarded-Prefix"), "/")
	return prefix + path
}

// oidcCallbackURI builds the absolute redirect_uri for the OIDC callback from
// the incoming request context. It uses X-Forwarded-Proto for the scheme,
// X-Forwarded-Host for the hostname (falls back to the Host header when the
// outermost proxy does not set it), and X-Forwarded-Prefix (if present) to
// prepend the sub-path when Nexus is proxied at e.g. /nexus/. The result is
// stored in the pending entry so the callback leg uses the same URI for the
// token exchange without re-deriving from a different (IdP-originated) request.
//
// X-Forwarded-Host is preferred over Host because a chained nginx proxy
// rewrites Host to the upstream hostname (e.g. nexus.stg.alphabitcore.io)
// by default; X-Forwarded-Host carries the original public hostname the
// browser used (e.g. prime5.stg.alphabitcore.io) and must be set by the
// outermost reverse proxy: proxy_set_header X-Forwarded-Host $http_host;
func oidcCallbackURI(c echo.Context) string {
	scheme := "https"
	if proto := c.Request().Header.Get("X-Forwarded-Proto"); proto != "" {
		scheme = proto
	} else if c.Request().TLS == nil {
		scheme = "http"
	}
	host := c.Request().Header.Get("X-Forwarded-Host")
	if host == "" {
		host = c.Request().Host
	}
	prefix := strings.TrimRight(c.Request().Header.Get("X-Forwarded-Prefix"), "/")
	return scheme + "://" + host + prefix + "/authserver/oidc/callback"
}

// startOIDC builds the IdP authorize URL, stamps the chosen IdP id onto the
// pending authorize entry so the callback loads the same config, and 302s the
// browser to the IdP. state carries the authctx back on the callback.
//
// CSRF binding: `state` is the authctx, which is itself a 256-bit
// crypto/rand opaque token (store.RandomOpaqueToken(32), minted in the OAuth
// authorize endpoint — see oauth/authorize.go). It is NOT a sequential or
// predictable identifier, so it already serves as the unguessable, single-use
// CSRF nonce the OAuth spec asks `state` to be: the callback resolves it via
// Pending.Take (a single-use server-side lookup) and rejects any value not
// present, so a forged/guessed state cannot drive the callback. Adding a second
// random component would not increase the (already 256-bit) entropy. A separate
// per-login `nonce` (set below) is bound into the ID token for OIDC §3.1.2.1
// replay defense — a distinct control from this CSRF state.
func startOIDC(c echo.Context, d StartDeps, idp *store.IdentityProvider, authctx string) error {
	cfg := store.DecodeOIDCConfig(idp)
	if cfg == nil || cfg.ClientID == "" {
		return bounceToLogin(c, authctx)
	}
	// The authorize endpoint is usually absent from the saved config (the
	// admin form collects only the issuer). Resolve it from the issuer's
	// discovery document; on failure bounce to the login page rather than
	// emit a server error body, matching every other non-happy path here.
	if cfg.AuthorizeURL == "" && d.Resolver != nil {
		eps, err := d.Resolver.Resolve(c.Request().Context(), cfg.Issuer, oidcdisco.Endpoints{
			AuthorizeURL: cfg.AuthorizeURL,
			TokenURL:     cfg.TokenURL,
			JwksURI:      cfg.JwksURI,
		})
		if err != nil {
			slog.Default().Warn("authserver: OIDC discovery failed at SSO start",
				"idp", idp.ID, "issuer", cfg.Issuer, "err", err)
			return bounceToLogin(c, authctx)
		}
		cfg.AuthorizeURL = eps.AuthorizeURL
		cfg.TokenURL = eps.TokenURL
		cfg.JwksURI = eps.JwksURI
	}
	if cfg.AuthorizeURL == "" {
		return bounceToLogin(c, authctx)
	}
	if !d.Pending.SetIdPID(authctx, idp.ID) {
		return c.Redirect(http.StatusFound, spaPath(c, loginPagePath))
	}
	// Derive the redirect_uri from the current request context so it reflects
	// the actual scheme, host, and sub-path prefix the browser used — works
	// correctly whether accessed directly or through a sub-path proxy.
	redirectURI := oidcCallbackURI(c)
	if !d.Pending.SetOIDCRedirectURI(authctx, redirectURI) {
		return c.Redirect(http.StatusFound, spaPath(c, loginPagePath))
	}
	// Single-use nonce bound to this login: sent to the IdP now and verified
	// against the ID token's `nonce` claim on the callback (OIDC Core §3.1.2.1),
	// defeating ID-token replay/injection.
	nonce := store.RandomOpaqueToken(32)
	if !d.Pending.SetIdPNonce(authctx, nonce) {
		return c.Redirect(http.StatusFound, spaPath(c, loginPagePath))
	}
	u, err := url.Parse(cfg.AuthorizeURL)
	if err != nil {
		return bounceToLogin(c, authctx)
	}
	q := u.Query()
	// Admin-configured extra params first (e.g. Auth0 `organization`), so the
	// reserved OAuth params below always win and can't be clobbered by config.
	for _, p := range cfg.AuthorizeParams {
		if p.Key == "" || reservedAuthorizeParams[p.Key] {
			continue
		}
		q.Set(p.Key, p.Value)
	}
	q.Set("response_type", "code")
	q.Set("client_id", cfg.ClientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("scope", "openid profile email")
	q.Set("state", authctx)
	q.Set("nonce", nonce)
	u.RawQuery = q.Encode()
	// Bind the callback to THIS browser: set a short-lived HMAC-signed HttpOnly
	// cookie carrying the authctx. The OIDC callback rejects any request whose
	// signed cookie is absent or does not bind to the `state` query param,
	// defeating login-CSRF (a forced callback in a victim's browser). Skipped
	// only when no signer is wired (test harnesses that don't exercise it).
	if d.StateSigner != nil {
		c.SetCookie(&http.Cookie{
			Name:     oidcStateCookieName,
			Value:    d.StateSigner.sign(authctx),
			Path:     spaPath(c, oidcStateCookiePath),
			HttpOnly: true,
			Secure:   true,
			SameSite: http.SameSiteLaxMode,
			MaxAge:   oidcStateCookieMaxAge,
		})
	}
	return c.Redirect(http.StatusFound, u.String())
}

// reservedAuthorizeParams are the OAuth params startOIDC owns; an IdP's
// extra-authorize-params config cannot override them.
var reservedAuthorizeParams = map[string]bool{
	"response_type": true,
	"client_id":     true,
	"redirect_uri":  true,
	"scope":         true,
	"state":         true,
	"nonce":         true,
}

// LogoutHandler returns GET /authserver/idp/:idpId/logout — RP-initiated logout.
// For an OIDC IdP whose discovery advertises an end_session_endpoint, it 302s the
// browser there with post_logout_redirect_uri back to the SPA login page, so the
// IdP ends its own session and then returns the user to /login. For a SAML or
// local IdP, or an OIDC IdP without end_session, it 302s straight to /login (the
// Nexus session is already dropped client-side). Best-effort: any resolution
// failure bounces to /login, so logout can never strand the user.
func LogoutHandler(d StartDeps) echo.HandlerFunc {
	return func(c echo.Context) error {
		ctx := c.Request().Context()
		idp, err := d.IdPs.GetByID(ctx, c.Param("idpId"))
		if err != nil || idp == nil || idp.Type != "oidc" || d.Resolver == nil {
			return c.Redirect(http.StatusFound, spaPath(c, loginPagePath))
		}
		cfg := store.DecodeOIDCConfig(idp)
		if cfg == nil {
			return c.Redirect(http.StatusFound, spaPath(c, loginPagePath))
		}
		eps, err := d.Resolver.Resolve(ctx, cfg.Issuer, oidcdisco.Endpoints{
			AuthorizeURL: cfg.AuthorizeURL,
			TokenURL:     cfg.TokenURL,
			JwksURI:      cfg.JwksURI,
		})
		if err != nil || eps.EndSessionURL == "" {
			return c.Redirect(http.StatusFound, spaPath(c, loginPagePath))
		}
		u, err := url.Parse(eps.EndSessionURL)
		if err != nil {
			return c.Redirect(http.StatusFound, spaPath(c, loginPagePath))
		}
		q := u.Query()
		q.Set("post_logout_redirect_uri", strings.TrimRight(d.Issuer, "/")+loginPagePath)
		if cfg.ClientID != "" {
			q.Set("client_id", cfg.ClientID)
		}
		u.RawQuery = q.Encode()
		return c.Redirect(http.StatusFound, u.String())
	}
}

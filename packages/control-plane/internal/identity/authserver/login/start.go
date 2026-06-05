package login

import (
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/crewjam/saml"
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
			return c.Redirect(http.StatusFound, loginPagePath)
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
	u := url.URL{Path: loginPagePath, RawQuery: url.Values{"authctx": {authctx}}.Encode()}
	return c.Redirect(http.StatusFound, u.String())
}

// startOIDC builds the IdP authorization URL, stamps the chosen IdP id onto the
// pending authorize entry so the callback loads the same config, and 302s the
// browser to the IdP. state carries the authctx back on the callback.
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
		return c.Redirect(http.StatusFound, loginPagePath)
	}
	// Single-use nonce bound to this login: sent to the IdP now and verified
	// against the ID token's `nonce` claim on the callback (OIDC Core §3.1.2.1),
	// defeating ID-token replay/injection.
	nonce := store.RandomOpaqueToken(32)
	if !d.Pending.SetIdPNonce(authctx, nonce) {
		return c.Redirect(http.StatusFound, loginPagePath)
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
	q.Set("redirect_uri", cfg.RedirectURI)
	q.Set("scope", "openid profile email")
	q.Set("state", authctx)
	q.Set("nonce", nonce)
	u.RawQuery = q.Encode()
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

// startSAML builds an SP-initiated AuthnRequest, records its ID against the
// authctx for InResponseTo validation on the ACS, stamps the IdP id onto the
// pending entry, and renders an auto-submitting POST form (HTTP-POST binding)
// that delivers the AuthnRequest to the IdP with RelayState=<authctx>.
func startSAML(c echo.Context, d StartDeps, idp *store.IdentityProvider, authctx string) error {
	cfg := store.DecodeSAMLConfig(idp)
	sp, err := buildSAMLServiceProvider(cfg, d.Issuer)
	if err != nil {
		return bounceToLogin(c, authctx)
	}
	// Append the IdP's configured extra SSO params to the destination URL (e.g.
	// Auth0 Organizations' required `organization`). crewjam posts the form to
	// the AuthnRequest Destination, so the params ride on the URL the browser
	// POSTs to while SAMLRequest / RelayState stay in the form body.
	dest, err := samlSSOURLWithParams(cfg.SSOURL, cfg.SSOParams)
	if err != nil {
		return bounceToLogin(c, authctx)
	}
	req, err := sp.MakeAuthenticationRequest(dest, saml.HTTPPostBinding, saml.HTTPPostBinding)
	if err != nil {
		return bounceToLogin(c, authctx)
	}
	if !d.Pending.SetIdPID(authctx, idp.ID) {
		return c.Redirect(http.StatusFound, loginPagePath)
	}
	d.Requests.Put(authctx, req.ID)
	return c.HTMLBlob(http.StatusOK, req.Post(authctx))
}

// reservedSAMLSSOParams are the SAML protocol query params the SP / crewjam
// own; an IdP's extra-SSO-params config cannot override them.
var reservedSAMLSSOParams = map[string]bool{
	"SAMLRequest": true,
	"RelayState":  true,
	"SigAlg":      true,
	"Signature":   true,
}

// samlSSOURLWithParams appends the IdP's configured extra query parameters to
// the SSO endpoint URL on the SP-initiated AuthnRequest — the SAML analogue of
// startOIDC's AuthorizeParams loop. Empty-key and reserved SAML protocol params
// are skipped. Returns the URL unchanged when no params are configured.
func samlSSOURLWithParams(ssoURL string, params []store.SAMLSSOParam) (string, error) {
	if len(params) == 0 {
		return ssoURL, nil
	}
	u, err := url.Parse(ssoURL)
	if err != nil {
		return "", err
	}
	q := u.Query()
	for _, p := range params {
		if p.Key == "" || reservedSAMLSSOParams[p.Key] {
			continue
		}
		q.Set(p.Key, p.Value)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
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
			return c.Redirect(http.StatusFound, loginPagePath)
		}
		cfg := store.DecodeOIDCConfig(idp)
		if cfg == nil {
			return c.Redirect(http.StatusFound, loginPagePath)
		}
		eps, err := d.Resolver.Resolve(ctx, cfg.Issuer, oidcdisco.Endpoints{
			AuthorizeURL: cfg.AuthorizeURL,
			TokenURL:     cfg.TokenURL,
			JwksURI:      cfg.JwksURI,
		})
		if err != nil || eps.EndSessionURL == "" {
			return c.Redirect(http.StatusFound, loginPagePath)
		}
		u, err := url.Parse(eps.EndSessionURL)
		if err != nil {
			return c.Redirect(http.StatusFound, loginPagePath)
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

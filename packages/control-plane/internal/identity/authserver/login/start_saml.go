package login

import (
	"net/http"
	"net/url"

	"github.com/crewjam/saml"
	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/store"
)

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
		return c.Redirect(http.StatusFound, spaPath(c, loginPagePath))
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

package login

import (
	"context"
	"encoding/xml"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/crewjam/saml"
	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
)

// SAMLDeps carries the collaborators for the SP-initiated SAML login handlers.
// It mirrors OIDCDeps plus a Requests store for InResponseTo tracking and the
// auth-server Issuer the SP entityID / ACS URL derive from.
type SAMLDeps struct {
	IdPs      *store.IdPStore
	Federated *store.FederatedStore
	Pending   *store.PendingAuthzStore
	AuthCodes *store.AuthCodeStore
	Requests  *store.SAMLRequestStore
	Issuer    string
	// Audit emits the admin.login.succeeded row for a SAML login, mirroring the
	// password path; nil-tolerant for test harnesses that don't assert audit.
	Audit *audit.Writer
}

// SAMLACSHandler returns POST /authserver/saml/acs — the Assertion Consumer
// Service. It consumes the signed SAMLResponse, validates it (signature,
// conditions, audience, destination, and InResponseTo against the outstanding
// AuthnRequest ID bound to the RelayState authctx), extracts the NameID and
// the email / groups attributes, then runs the shared match-or-JIT-provision
// path and mints an authorization code — rejoining the OAuth flow exactly as
// OIDC login does.
func SAMLACSHandler(d SAMLDeps) echo.HandlerFunc {
	return func(c echo.Context) error {
		ctx := c.Request().Context()

		authctx := c.FormValue("RelayState")
		if authctx == "" {
			return c.JSON(http.StatusBadRequest, errorResponse{Error: errAuthctxExpired})
		}
		// Take both the pending authorize request and the outstanding
		// AuthnRequest ID single-use. A response with no outstanding request
		// (replay, or an IdP-initiated response) cannot proceed.
		pending, ok := d.Pending.Take(authctx)
		if !ok || pending.IdPID == "" {
			return c.JSON(http.StatusBadRequest, errorResponse{Error: errAuthctxExpired})
		}
		requestID, ok := d.Requests.Take(authctx)
		if !ok {
			return c.JSON(http.StatusBadRequest, errorResponse{Error: errAuthctxExpired})
		}

		idp, err := d.IdPs.GetByID(ctx, pending.IdPID)
		if err != nil || !idp.Enabled {
			// Reject a missing or disabled IdP: disabling a SAML IdP must
			// invalidate in-flight logins, not just hide it from the picker.
			return c.JSON(http.StatusBadRequest, errorResponse{Error: "saml_not_configured"})
		}
		cfg := store.DecodeSAMLConfig(idp)
		sp, err := buildSAMLServiceProvider(cfg, d.Issuer)
		if err != nil {
			return c.JSON(http.StatusInternalServerError, errorResponse{Error: errInternal})
		}

		// ParseResponse validates the XML signature against the IdP cert, the
		// audience (SP entityID), the not-before / not-on-or-after window, the
		// destination (ACS URL), and InResponseTo against the supplied IDs.
		assertion, err := sp.ParseResponse(c.Request(), []string{requestID})
		if err != nil {
			// crewjam's InvalidResponseError.Error() is the opaque
			// "Authentication failed"; the actionable cause — fingerprint
			// mismatch, audience / issuer / InResponseTo mismatch, or a
			// clock-window violation — lives in PrivateErr. Surface it
			// server-side (mirroring the OIDC callback's slog.Warn) so an
			// admin debugging an IdP handshake isn't left with only the
			// bounded client code. The client still sees just the bounded
			// "saml_invalid_response".
			reason := err.Error()
			var ire *saml.InvalidResponseError
			if errors.As(err, &ire) && ire.PrivateErr != nil {
				reason = ire.PrivateErr.Error()
			}
			slog.Default().Warn("authserver: SAML ACS response validation failed",
				"idp", idp.ID, "reason", reason)
			return c.JSON(http.StatusUnauthorized, errorResponse{Error: "saml_invalid_response"})
		}
		subject := samlNameID(assertion)
		if subject == "" {
			return c.JSON(http.StatusUnauthorized, errorResponse{Error: "saml_invalid_response"})
		}
		email := samlEmailWithFallback(assertion, cfg.EmailAttr)
		groups := samlGroupsWithFallback(assertion, cfg.GroupsAttr)
		name := samlNameWithFallback(assertion, email)

		userID, provisionErr := d.resolveOrProvision(ctx, idp, subject, name, email, groups)
		if provisionErr != "" {
			return c.JSON(http.StatusUnauthorized, errorResponse{Error: provisionErr})
		}

		// Emit the login-succeeded audit row, mirroring the password path so
		// SSO logins appear in the admin audit log + login-rate SLO stream.
		if d.Audit != nil {
			d.Audit.LogObserved(ctx, audit.Entry{
				Action:     "admin.login.succeeded",
				ActorLabel: email,
				ActorID:    userID,
				SourceIP:   c.RealIP(),
				EntityType: "user",
				EntityID:   userID,
			})
		}

		authCode := store.RandomOpaqueToken(32)
		d.AuthCodes.Put(authCode, store.AuthCodeEntry{
			ClientID:      pending.ClientID,
			UserID:        userID,
			RedirectURI:   pending.RedirectURI,
			PKCEChallenge: pending.CodeChallenge,
			Scope:         pending.Scope,
			IdPID:         idp.ID,
			DeviceID:      pending.DeviceID,
			Nonce:         pending.Nonce,
			Email:         email,
			AMR:           []string{"sso"},
			ExpiresAt:     time.Now().Add(authCodeTTL),
		})
		redirect, err := buildRedirect(pending.RedirectURI, authCode, pending.State)
		if err != nil {
			return c.JSON(http.StatusInternalServerError, errorResponse{Error: errInternal})
		}
		return c.Redirect(http.StatusFound, redirect)
	}
}

// resolveOrProvision maps an external SAML subject to a Nexus user id, mirroring
// the OIDC callback: a known federated identity matches; otherwise the user is
// JIT-provisioned when the IdP allows it (resolving the groups attribute through
// IdpGroupMapping into IamGroupMembership), else the login is refused. Returns
// the user id, or a structured error string for the response body.
func (d SAMLDeps) resolveOrProvision(ctx context.Context, idp *store.IdentityProvider, subject, name, email string, groups []string) (string, string) {
	fi, found, err := d.Federated.FindByIdPSubject(ctx, idp.ID, subject)
	if err != nil {
		return "", errInternal
	}
	if found {
		_ = d.Federated.UpdateRawClaims(ctx, fi.ID, map[string]any{"sub": subject, "email": email})
		// Refresh the user's displayName + email on re-login: a real name the
		// IdP only began emitting after first JIT (or one corrected upstream)
		// now propagates. Empty values are ignored by RefreshUserProfile, so a
		// nameless assertion never blanks a good displayName. Best-effort —
		// a refresh failure must not block an otherwise-valid login.
		_ = d.Federated.RefreshUserProfile(ctx, fi.UserID, name, email)
		return fi.UserID, ""
	}
	if !idp.JITEnabled {
		return "", "user_not_provisioned"
	}
	displayName := name
	if displayName == "" {
		displayName = email
	}
	if displayName == "" {
		displayName = subject
	}
	u, _, jitErr := d.Federated.JITProvisionUser(ctx, store.JITProvisionParams{
		IdPID:                 idp.ID,
		ExternalSubject:       subject,
		Email:                 email,
		DisplayName:           displayName,
		Groups:                groups,
		DefaultRole:           idp.DefaultRole,
		CanAccessControlPlane: idp.DefaultControlPlaneAccess,
		CreatedBy:             "saml-jit",
		Source:                "saml",
	})
	if jitErr != nil {
		return "", errInternal
	}
	return u.ID, ""
}

// SAMLMetadataHandler returns GET /authserver/saml/metadata — the SP metadata
// (entityID + ACS URL) admins import into their IdP. It is IdP-independent:
// the SP identity derives solely from the auth-server issuer.
func SAMLMetadataHandler(d SAMLDeps) echo.HandlerFunc {
	return func(c echo.Context) error {
		base := strings.TrimRight(d.Issuer, "/")
		acsURL, err1 := url.Parse(base + samlACSPath)
		metaURL, err2 := url.Parse(base + samlMetadataPath)
		if err1 != nil || err2 != nil {
			return c.JSON(http.StatusInternalServerError, errorResponse{Error: errInternal})
		}
		sp := &saml.ServiceProvider{EntityID: metaURL.String(), AcsURL: *acsURL, MetadataURL: *metaURL}
		out, err := xml.MarshalIndent(sp.Metadata(), "", "  ")
		if err != nil {
			return c.JSON(http.StatusInternalServerError, errorResponse{Error: errInternal})
		}
		return c.Blob(http.StatusOK, "application/samlmetadata+xml", out)
	}
}

// samlNameID returns the assertion's NameID value (the external subject), or
// "" when the assertion carries no subject.
func samlNameID(a *saml.Assertion) string {
	if a == nil || a.Subject == nil || a.Subject.NameID == nil {
		return ""
	}
	return strings.TrimSpace(a.Subject.NameID.Value)
}

// samlAttrValues returns every non-empty value of the assertion attribute whose
// Name or FriendlyName matches name, across all attribute statements.
func samlAttrValues(a *saml.Assertion, name string) []string {
	if a == nil || name == "" {
		return nil
	}
	var out []string
	for _, stmt := range a.AttributeStatements {
		for _, attr := range stmt.Attributes {
			if attr.Name != name && attr.FriendlyName != name {
				continue
			}
			for _, v := range attr.Values {
				if s := strings.TrimSpace(v.Value); s != "" {
					out = append(out, s)
				}
			}
		}
	}
	return out
}

// samlFirstAttr returns the first value of the named attribute, or "".
func samlFirstAttr(a *saml.Assertion, name string) string {
	if vs := samlAttrValues(a, name); len(vs) > 0 {
		return vs[0]
	}
	return ""
}

// Well-known SAML attribute names / claim URIs / LDAP OIDs that IdPs use to
// carry the user's email and group memberships. These are probed at runtime
// only when the IdP's configured attribute name (default "email" / "groups")
// matches nothing on the assertion — so a tenant whose IdP (Auth0, ADFS,
// Azure AD, Shibboleth) emits the value under a long claim URI still resolves an
// email + groups without the admin having to rename claims on the IdP or
// hand-tune the attribute fields. The configured name stays authoritative;
// fallback never overrides a value the configured attribute already produced.
//
// This is a fixed protocol constant, deliberately in code rather than admin
// config or a Hub-pushed shadow blob: the set of standardized SAML email/groups
// claim names is bounded and provider-agnostic, and a genuinely novel attribute
// name is already covered by the admin setting the explicit attribute field
// (which always wins). Do not turn this into a configurable list.
var samlEmailFallbackAttrs = []string{
	"email",
	"mail",
	"emailAddress",
	"http://schemas.xmlsoap.org/ws/2005/05/identity/claims/emailaddress",
	"urn:oid:0.9.2342.19200300.100.1.3",
}

var samlGroupsFallbackAttrs = []string{
	"groups",
	"memberOf",
	"Group",
	"http://schemas.xmlsoap.org/claims/Group",
	"http://schemas.microsoft.com/ws/2008/06/identity/claims/groups",
}

// samlEmailWithFallback resolves the user's email: the IdP's configured email
// attribute wins; if it is absent on the assertion the well-known alternatives
// are probed in order; as a last resort a NameID already in emailAddress format
// is the subject's email. Returns "" when nothing yields an email.
func samlEmailWithFallback(a *saml.Assertion, configured string) string {
	if v := samlFirstAttr(a, configured); v != "" {
		return v
	}
	for _, name := range samlEmailFallbackAttrs {
		if name == configured {
			continue
		}
		if v := samlFirstAttr(a, name); v != "" {
			return v
		}
	}
	if emailFormatNameID(a) {
		return samlNameID(a)
	}
	return ""
}

// samlGroupsWithFallback resolves the user's group memberships: the configured
// groups attribute wins; if it is absent the well-known alternatives are probed
// in order. Returns nil when no group attribute is present (the user is then
// provisioned with no IdP-derived groups).
func samlGroupsWithFallback(a *saml.Assertion, configured string) []string {
	if vs := samlAttrValues(a, configured); len(vs) > 0 {
		return vs
	}
	for _, name := range samlGroupsFallbackAttrs {
		if name == configured {
			continue
		}
		if vs := samlAttrValues(a, name); len(vs) > 0 {
			return vs
		}
	}
	return nil
}

// emailFormatNameID reports whether the assertion's NameID is declared in SAML
// emailAddress format — the only case where the subject itself is a safe email
// source. A NameID in any other format (persistent, transient, unspecified) is
// not treated as an email, to avoid provisioning a user with a non-email
// identifier.
func emailFormatNameID(a *saml.Assertion) bool {
	if a == nil || a.Subject == nil || a.Subject.NameID == nil {
		return false
	}
	return a.Subject.NameID.Format == string(saml.EmailAddressNameIDFormat)
}

// Well-known SAML attribute names IdPs use to carry a human display name and
// its given/family parts. Probed (zero-config) to populate the user's display
// name — cosmetic enrichment only, so there is no admin attribute field for it
// (unlike email/groups): the standardized set below covers the common IdPs and
// a miss simply falls back to email, which is harmless.
var (
	samlFullNameAttrs = []string{
		"http://schemas.xmlsoap.org/ws/2005/05/identity/claims/name",
		"displayName",
		"cn",
		"name",
	}
	samlGivenNameAttrs = []string{
		"http://schemas.xmlsoap.org/ws/2005/05/identity/claims/givenname",
		"givenName",
		"given_name",
		"urn:oid:2.5.4.42",
	}
	samlFamilyNameAttrs = []string{
		"http://schemas.xmlsoap.org/ws/2005/05/identity/claims/surname",
		"sn",
		"family_name",
		"urn:oid:2.5.4.4",
	}
	// samlNicknameAttrs carry a short human handle (e.g. "steve.chen") IdPs emit
	// when they have no real given/family name. Probed only after the full-name
	// and given+family attrs yield nothing usable, so a login whose only "name"
	// value is the email address shows the handle instead of the whole address.
	samlNicknameAttrs = []string{
		"http://schemas.auth0.com/nickname",
		"nickname",
		"http://schemas.auth0.com/username",
		"username",
		"preferred_username",
	}
)

// emailish reports whether s looks like an email address. Used to reject a
// "name" value that is really just the email — several IdPs (notably Auth0 when
// the profile has no real name) populate the name/displayName claim with the
// address, which would surface the whole "steve.chen@corp.com" in the UI where
// a readable name reads better.
func emailish(s string) bool {
	return strings.Contains(s, "@")
}

// humanizeHandle turns a login handle or email local-part ("steve.chen",
// "steve_chen", "steve.chen@corp.com") into a readable display name
// ("steve chen"): it drops any "@domain" tail, splits on the separators IdPs
// use to join name parts (. _ - +), drops empty and purely-numeric segments
// (e.g. the "42" in "john.doe.42"), and joins with spaces. Case is preserved
// from the source — an all-lowercase handle stays lowercase. Returns "" when no
// usable word remains.
func humanizeHandle(s string) string {
	if at := strings.IndexByte(s, '@'); at >= 0 {
		s = s[:at]
	}
	s = strings.NewReplacer(".", " ", "_", " ", "-", " ", "+", " ").Replace(s)
	var words []string
	for _, f := range strings.Fields(s) {
		if isAllDigits(f) {
			continue
		}
		words = append(words, f)
	}
	return strings.Join(words, " ")
}

// isAllDigits reports whether s is non-empty and every rune is an ASCII digit.
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// samlNameWithFallback derives a human display name from the assertion: a full-
// name attribute wins; otherwise given + family are composed. Any candidate that
// is itself an email address is skipped (the address must not double as the
// display name). Failing a real name, a short nickname/username handle — or the
// email local-part — is humanized into a readable name ("steve.chen" →
// "steve chen"). Returns "" when nothing usable remains (the caller then falls
// back to email, then subject).
func samlNameWithFallback(a *saml.Assertion, email string) string {
	for _, name := range samlFullNameAttrs {
		if v := samlFirstAttr(a, name); v != "" && !emailish(v) {
			return v
		}
	}
	given, family := "", ""
	for _, n := range samlGivenNameAttrs {
		if v := samlFirstAttr(a, n); v != "" {
			given = v
			break
		}
	}
	for _, n := range samlFamilyNameAttrs {
		if v := samlFirstAttr(a, n); v != "" {
			family = v
			break
		}
	}
	if composed := strings.TrimSpace(given + " " + family); composed != "" && !emailish(composed) {
		return composed
	}
	for _, n := range samlNicknameAttrs {
		if v := samlFirstAttr(a, n); v != "" {
			if h := humanizeHandle(v); h != "" {
				return h
			}
		}
	}
	return humanizeHandle(email)
}

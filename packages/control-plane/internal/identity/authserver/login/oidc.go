package login

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/oidcdisco"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	nexushttp "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/http"
)

// OIDCDeps carries the collaborators for the OIDC begin + callback handlers.
// Config lives on per-IdP rows in the `IdentityProvider` table and is loaded
// via `IdPs.GetByID`. The chosen IdP id is threaded through
// `PendingAuthzEntry.IdPID` so the callback can read it back regardless of
// how many IdPs exist.
type OIDCDeps struct {
	IdPs      *store.IdPStore
	Federated *store.FederatedStore
	Pending   *store.PendingAuthzStore
	AuthCodes *store.AuthCodeStore
	// Resolver fills the token + JWKS endpoints from the IdP issuer's
	// discovery document when the saved config omits them, mirroring the
	// SSO-start leg. Shared with StartDeps.Resolver so the document fetched
	// at start is reused here. May be nil (tests that pin explicit endpoints).
	Resolver *oidcdisco.Resolver
	// Audit emits the admin.login.succeeded row for an OIDC login, mirroring the
	// password path; nil-tolerant for test harnesses that don't assert audit.
	Audit *audit.Writer
}

// OIDCCallbackHandler returns GET /authserver/oidc/callback?code=<code>&state=<authctx>.
// It exchanges the code for an ID token, verifies it against the IdP-
// specific JWKS, JIT-provisions the user if needed, mints an
// authorization code, and redirects back to the client.
func OIDCCallbackHandler(d OIDCDeps) echo.HandlerFunc {
	return func(c echo.Context) error {
		ctx := c.Request().Context()

		code := c.QueryParam("code")
		authctx := c.QueryParam("state")

		// The IdP can redirect back with an error instead of a code — e.g. Auth0
		// "parameter organization is required for this client" when the app
		// requires an org the authorize request omitted. Log the full reason
		// server-side and send the browser to the SPA SSO-error page carrying
		// ONLY the bounded OAuth error code (never the free-text description,
		// which is attacker-influenceable and would be reflected on an
		// unauthenticated page). The page is terminal: it shows the failure and
		// waits for the operator to click back to sign-in.
		if idpErr := c.QueryParam("error"); idpErr != "" {
			slog.Default().Warn("authserver: OIDC IdP returned an error at callback",
				"error", idpErr,
				"error_description", c.QueryParam("error_description"),
				"authctx_present", authctx != "")
			u := url.URL{Path: ssoErrorPagePath, RawQuery: url.Values{"code": {idpErr}}.Encode()}
			return c.Redirect(http.StatusFound, u.String())
		}

		if code == "" || authctx == "" {
			return c.JSON(http.StatusBadRequest, errorResponse{Error: errAuthctxExpired})
		}

		pending, ok := d.Pending.Take(authctx)
		if !ok {
			return c.JSON(http.StatusBadRequest, errorResponse{Error: errAuthctxExpired})
		}
		if pending.IdPID == "" {
			// The OIDC begin handler stamps IdPID into pending; if it's
			// missing the user reached the callback without going through
			// begin (or pending was racy). Refuse explicitly.
			return c.JSON(http.StatusBadRequest, errorResponse{Error: "oidc_not_configured"})
		}

		idp, err := d.IdPs.GetByID(ctx, pending.IdPID)
		if err != nil {
			return c.JSON(http.StatusInternalServerError, errorResponse{Error: errInternal})
		}
		cfg := store.DecodeOIDCConfig(idp)
		// Reject a disabled IdP: disabling it must invalidate in-flight logins,
		// not just hide it from the picker (parity with the SAML ACS handler).
		if cfg == nil || !idp.Enabled {
			return c.JSON(http.StatusBadRequest, errorResponse{Error: "oidc_not_configured"})
		}
		// The token + JWKS endpoints are usually absent from the saved config
		// (issuer-only Add-IdP form). Resolve them from the issuer's discovery
		// document — the same Resolver the SSO-start leg used, so this is a
		// cache hit. On failure the code below treats the still-empty endpoints
		// as oidc_not_configured.
		if (cfg.TokenURL == "" || cfg.JwksURI == "") && d.Resolver != nil {
			if eps, err := d.Resolver.Resolve(ctx, cfg.Issuer, oidcdisco.Endpoints{
				AuthorizeURL: cfg.AuthorizeURL,
				TokenURL:     cfg.TokenURL,
				JwksURI:      cfg.JwksURI,
			}); err != nil {
				slog.Default().Warn("authserver: OIDC discovery failed at callback",
					"idp", idp.ID, "issuer", cfg.Issuer, "err", err)
			} else {
				cfg.AuthorizeURL = eps.AuthorizeURL
				cfg.TokenURL = eps.TokenURL
				cfg.JwksURI = eps.JwksURI
			}
		}
		if cfg.TokenURL == "" {
			return c.JSON(http.StatusBadRequest, errorResponse{Error: "oidc_not_configured"})
		}

		// Exchange authorization code for tokens.
		idToken, err := exchangeOIDCCode(ctx, cfg, code)
		if err != nil {
			return c.JSON(http.StatusUnauthorized, errorResponse{Error: "oidc_exchange_failed"})
		}

		// Verify the ID token against THIS IdP's JWKS. NewJWKSCache requires a
		// non-nil *slog.Logger — passing nil here would nil-deref on the first
		// refresh's Debug/Warn calls (slog.(*Logger).Debug derefs the receiver).
		// Use slog.Default() so the cache logs through whatever sink main has
		// installed, matching every other NewJWKSCache call site in this repo.
		jwksCache := middleware.NewJWKSCache(cfg.JwksURI, slog.Default())
		// An OIDC ID token's `aud` MUST contain the client_id (OIDC Core §2). The
		// `audience` config field is for an optional API audience; when the admin
		// leaves it blank (the common case), validate against the client_id so a
		// standard ID token verifies instead of failing an empty-audience check.
		audience := cfg.Audience
		if audience == "" {
			audience = cfg.ClientID
		}
		mwCfg := &middleware.OidcConfig{
			Issuer:     cfg.Issuer,
			JwksUri:    cfg.JwksURI,
			Audience:   audience,
			EmailClaim: cfg.EmailClaim,
			GroupClaim: cfg.GroupClaim,
		}
		claims, err := middleware.ValidateJWT(idToken, mwCfg, jwksCache)
		if err != nil {
			slog.Default().Warn("authserver: OIDC ID token validation failed",
				"idp", idp.ID, "err", err)
			return c.JSON(http.StatusUnauthorized, errorResponse{Error: "oidc_token_invalid"})
		}
		// Bind the token to this login: the ID token's `nonce` must echo the
		// single-use nonce the SSO-start handler sent (OIDC Core §3.1.2.1),
		// defeating ID-token replay/injection. Enforced whenever a nonce was
		// stamped on the pending entry (always, for OIDC logins started after
		// this shipped; skipped only for an in-flight pre-upgrade handle).
		if pending.IdPNonce != "" && claims.Nonce != pending.IdPNonce {
			slog.Default().Warn("authserver: OIDC ID token nonce mismatch", "idp", idp.ID)
			return c.JSON(http.StatusUnauthorized, errorResponse{Error: "oidc_token_invalid"})
		}
		// Observability for IdP group wiring: shows exactly which groups the
		// token carried (empty means the IdP isn't emitting a groups claim, so
		// only the IdP defaultRole baseline applies). Keyed on the opaque `sub`,
		// not email — the address is PII and already captured in the audit log.
		slog.Default().Info("authserver: OIDC ID token validated",
			"idp", idp.ID, "sub", claims.Subject,
			"group_claim", cfg.GroupClaim, "groups", claims.Groups)

		// Look up or JIT provision the user — keyed on (idp.ID, claims.Subject).
		var userID string
		var fiID string
		fi, found, err := d.Federated.FindByIdPSubject(ctx, idp.ID, claims.Subject)
		if err != nil {
			slog.Default().Error("authserver: OIDC callback FindByIdPSubject failed",
				"idp", idp.ID, "sub", claims.Subject, "err", err)
			return c.JSON(http.StatusInternalServerError, errorResponse{Error: errInternal})
		}

		switch {
		case found:
			userID = fi.UserID
			fiID = fi.ID
			_ = d.Federated.UpdateRawClaims(ctx, fiID, map[string]any{
				"sub":   claims.Subject,
				"email": claims.Email,
				"iss":   claims.Issuer,
			})
			// Refresh displayName + email on re-login so a profile name the IdP
			// only began emitting after first JIT (or one corrected upstream)
			// propagates. oidcProfileName omits the opaque-subject last resort, so
			// it returns "" when no real name exists; RefreshUserProfile then
			// leaves the stored displayName untouched. A re-login can thus sharpen
			// the name but never blank it or overwrite it with the subject.
			// Best-effort — a refresh failure must not block a valid login.
			_ = d.Federated.RefreshUserProfile(ctx, userID, oidcProfileName(idToken, claims.Email), claims.Email)
		case idp.JITEnabled:
			displayName := oidcDisplayName(idToken, claims.Email, claims.Subject)
			if displayName == "" {
				displayName = claims.Subject
			}
			u, id, jitErr := d.Federated.JITProvisionUser(ctx, store.JITProvisionParams{
				IdPID:           idp.ID,
				ExternalSubject: claims.Subject,
				Email:           claims.Email,
				DisplayName:     displayName,
				// Groups carries the JWT "groups" claim straight through
				// to the JIT store which resolves it via IdpGroupMapping
				// and stamps IamGroupMembership rows in the same tx.
				// Unmapped externals are silently skipped server-side.
				Groups: claims.Groups,
				// Baseline role + CP-access default come from the IdP row so
				// a federated user is provisioned with usable authority.
				DefaultRole:           idp.DefaultRole,
				CanAccessControlPlane: idp.DefaultControlPlaneAccess,
				CreatedBy:             "oidc-jit",
				Source:                "oidc",
			})
			if jitErr != nil {
				slog.Default().Error("authserver: OIDC JIT provisioning failed",
					"idp", idp.ID, "sub", claims.Subject, "default_role", idp.DefaultRole,
					"groups", claims.Groups, "err", jitErr)
				return c.JSON(http.StatusInternalServerError, errorResponse{Error: errInternal})
			}
			userID = u.ID
			fiID = id
		default:
			return c.JSON(http.StatusUnauthorized, errorResponse{Error: "user_not_provisioned"})
		}
		_ = fiID

		// Emit the login-succeeded audit row, mirroring the password path so
		// SSO logins appear in the admin audit log + login-rate SLO stream.
		if d.Audit != nil {
			d.Audit.LogObserved(ctx, audit.Entry{
				Action:     "admin.login.succeeded",
				ActorLabel: claims.Email,
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
			Email:         claims.Email,
			AMR:           []string{"sso"},
			ExpiresAt:     time.Now().Add(authCodeTTL),
		})

		redirect, err := buildRedirect(pending.RedirectURI, authCode, pending.State)
		if err != nil {
			slog.Default().Error("authserver: OIDC callback buildRedirect failed",
				"idp", idp.ID, "redirect_uri", pending.RedirectURI, "err", err)
			return c.JSON(http.StatusInternalServerError, errorResponse{Error: errInternal})
		}
		return c.Redirect(http.StatusFound, redirect)
	}
}

// oidcDisplayName derives a human display name for the federated user,
// preferring the standard OIDC profile claims on the (already signature-
// validated) ID token, then a readable form of the email local-part, and only
// as a last resort the opaque subject. No per-IdP config: these are OIDC Core
// standard claims and we already request the `profile` scope, so this is best-
// effort enrichment with sensible defaults.
func oidcDisplayName(idToken, email, subject string) string {
	if n := oidcProfileName(idToken, email); n != "" {
		return n
	}
	return subject
}

// oidcProfileName is oidcDisplayName without the opaque-subject last resort: the
// best human name we can derive from the token's profile claims or the email
// local-part, or "" when none exists. The refresh-on-re-login path uses it so a
// re-login can sharpen a stored displayName but never overwrite it with the
// subject.
func oidcProfileName(idToken, email string) string {
	if n := nameFromIDToken(idToken); n != "" {
		return n
	}
	return humanizeHandle(email)
}

// nameFromIDToken re-reads the ID token's (already-trusted) payload for the
// standard profile name claims. Returns "" when none are present.
func nameFromIDToken(idToken string) string {
	parts := strings.Split(idToken, ".")
	if len(parts) < 2 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var c struct {
		Name              string `json:"name"`
		GivenName         string `json:"given_name"`
		FamilyName        string `json:"family_name"`
		PreferredUsername string `json:"preferred_username"`
		Nickname          string `json:"nickname"`
	}
	if json.Unmarshal(payload, &c) != nil {
		return ""
	}
	// A real `name` or given+family wins as-is. Each is skipped when it is itself
	// an email address: several IdPs (notably Auth0 when the profile has no real
	// name) set `name` to the email, and surfacing the whole address reads worse
	// than a readable handle. preferred_username / nickname are humanized
	// ("steve.chen" → "steve chen") since they carry a login handle, not a name.
	if n := strings.TrimSpace(c.Name); n != "" && !emailish(n) {
		return n
	}
	if g := strings.TrimSpace(strings.TrimSpace(c.GivenName) + " " + strings.TrimSpace(c.FamilyName)); g != "" && !emailish(g) {
		return g
	}
	if h := humanizeHandle(strings.TrimSpace(c.PreferredUsername)); h != "" {
		return h
	}
	return humanizeHandle(strings.TrimSpace(c.Nickname))
}

// exchangeOIDCCode exchanges an authorization code for an ID token by
// calling the IdP's token endpoint.
func exchangeOIDCCode(ctx context.Context, cfg *store.OIDCConfig, code string) (string, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", cfg.RedirectURI)
	form.Set("client_id", cfg.ClientID)
	if cfg.ClientSecret != "" {
		form.Set("client_secret", cfg.ClientSecret)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.TokenURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := nexushttp.New(nexushttp.Config{}).Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token endpoint returned %d: %s", resp.StatusCode, body)
	}

	var tokenResp struct {
		IDToken string `json:"id_token"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return "", fmt.Errorf("parse token response: %w", err)
	}
	if tokenResp.IDToken == "" {
		return "", fmt.Errorf("token endpoint did not return id_token")
	}
	return tokenResp.IDToken, nil
}

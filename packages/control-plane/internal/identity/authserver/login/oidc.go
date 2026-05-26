package login

import (
	"context"
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
}

// resolveOIDCIdP picks the IdentityProvider row this login flow targets.
// Priority:
//  1. Explicit `idp_id` query param (multi-IdP friendly — the method-
//     picker step on the SPA passes whichever row the user clicked).
//  2. Sole enabled OIDC row (back-compat for single-IdP deployments,
//     no UX disruption when there's only one).
//
// Returns a structured error string suitable for the response body
// when no resolution is possible.
func resolveOIDCIdP(ctx context.Context, idps *store.IdPStore, idpID string) (*store.IdentityProvider, string) {
	if idpID != "" {
		idp, err := idps.GetByID(ctx, idpID)
		if err != nil {
			return nil, "oidc_not_configured"
		}
		if idp.Type != "oidc" || !idp.Enabled {
			return nil, "oidc_not_configured"
		}
		return idp, ""
	}
	list, err := idps.ListEnabled(ctx)
	if err != nil {
		return nil, errInternal
	}
	var oidcs []store.IdentityProvider
	for i := range list {
		if list[i].Type == "oidc" {
			oidcs = append(oidcs, list[i])
		}
	}
	switch len(oidcs) {
	case 0:
		return nil, "oidc_not_configured"
	case 1:
		return &oidcs[0], ""
	default:
		return nil, "idp_id_required"
	}
}

// OIDCBeginHandler returns GET /authserver/oidc/begin?authctx=<authctx>&idp_id=<idp_id>.
// It validates the pending authctx, resolves the chosen IdP, stamps its
// id onto the pending entry so the callback can look up the same
// config, then returns the upstream authorization URL.
func OIDCBeginHandler(d OIDCDeps) echo.HandlerFunc {
	return func(c echo.Context) error {
		ctx := c.Request().Context()
		authctx := c.QueryParam("authctx")
		if authctx == "" || !d.Pending.Has(authctx) {
			return c.JSON(http.StatusBadRequest, errorResponse{Error: errAuthctxExpired})
		}

		idp, resolveErr := resolveOIDCIdP(ctx, d.IdPs, c.QueryParam("idp_id"))
		if resolveErr != "" {
			return c.JSON(http.StatusBadRequest, errorResponse{Error: resolveErr})
		}
		cfg := store.DecodeOIDCConfig(idp)
		if cfg == nil || cfg.AuthorizeURL == "" || cfg.ClientID == "" {
			return c.JSON(http.StatusBadRequest, errorResponse{Error: "oidc_not_configured"})
		}

		if !d.Pending.SetIdPID(authctx, idp.ID) {
			return c.JSON(http.StatusBadRequest, errorResponse{Error: errAuthctxExpired})
		}

		u, err := url.Parse(cfg.AuthorizeURL)
		if err != nil {
			return c.JSON(http.StatusInternalServerError, errorResponse{Error: errInternal})
		}
		q := u.Query()
		q.Set("response_type", "code")
		q.Set("client_id", cfg.ClientID)
		q.Set("redirect_uri", cfg.RedirectURI)
		q.Set("scope", "openid profile email")
		q.Set("state", authctx)
		u.RawQuery = q.Encode()

		return c.JSON(http.StatusOK, map[string]string{"authorizationUrl": u.String()})
	}
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
		if cfg == nil || cfg.TokenURL == "" {
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
		mwCfg := &middleware.OidcConfig{
			Issuer:     cfg.Issuer,
			JwksUri:    cfg.JwksURI,
			Audience:   cfg.Audience,
			EmailClaim: cfg.EmailClaim,
		}
		claims, err := middleware.ValidateJWT(idToken, mwCfg, jwksCache)
		if err != nil {
			return c.JSON(http.StatusUnauthorized, errorResponse{Error: "oidc_token_invalid"})
		}

		// Look up or JIT provision the user — keyed on (idp.ID, claims.Subject).
		var userID string
		var fiID string
		fi, found, err := d.Federated.FindByIdPSubject(ctx, idp.ID, claims.Subject)
		if err != nil {
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
		case idp.JITEnabled:
			displayName := claims.Email
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
				Groups:    claims.Groups,
				CreatedBy: "oidc-jit",
			})
			if jitErr != nil {
				return c.JSON(http.StatusInternalServerError, errorResponse{Error: errInternal})
			}
			userID = u.ID
			fiID = id
		default:
			return c.JSON(http.StatusUnauthorized, errorResponse{Error: "user_not_provisioned"})
		}
		_ = fiID

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
			return c.JSON(http.StatusInternalServerError, errorResponse{Error: errInternal})
		}
		return c.Redirect(http.StatusFound, redirect)
	}
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

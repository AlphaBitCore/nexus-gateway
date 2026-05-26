package oauth

import (
	"context"
	"io"
	"log/slog"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/token"
)

// revocationChecker is the minimum surface IntrospectHandler needs from
// the revocation Store. *revocation.Store satisfies this in production;
// tests pass a fake to avoid spinning up the DB just to assert the
// not-revoked branch.
type revocationChecker interface {
	IsAccessTokenRevoked(ctx context.Context, jti, userID, deviceID, sessionID string) (bool, error)
}

// IntrospectDeps carries the collaborators the introspect handler needs.
// Introspection is now revocation-aware: after the standard signature +
// issuer + exp checks, the handler queries the revocation store to find
// any matching unexpired row. A hit funnels to writeInactive per RFC
// 7662 §2.2.
type IntrospectDeps struct {
	// Issuer is the expected "iss" claim. An empty value disables the check so
	// callers that do not pin an issuer (e.g. ad-hoc tests) still work.
	Issuer string
	// Keystore resolves kid → RSA public key. Required.
	Keystore *token.Keystore
	// Revocation, when non-nil, makes introspect revocation-aware. RFC
	// 7009 §2 + RFC 7662 §2.2 together require active=false on a revoked
	// token; we satisfy that by consulting the revocation store here.
	// nil disables the check (legacy mode for tests that don't wire it).
	Revocation revocationChecker
	// Logger receives non-fatal parse/verify errors. Nil logs to io.Discard so
	// handlers stay safe to use without upfront wiring.
	Logger *slog.Logger
}

func (d IntrospectDeps) logger() *slog.Logger {
	if d.Logger != nil {
		return d.Logger
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// introspectResponse is the RFC 7662 §2.2 body. All optional fields are
// omitempty so negative responses serialise to `{"active":false}` exactly and
// positive responses stay compact when a given claim is absent from the token.
type introspectResponse struct {
	Active    bool     `json:"active"`
	Issuer    string   `json:"iss,omitempty"`
	Subject   string   `json:"sub,omitempty"`
	Audience  []string `json:"aud,omitempty"`
	Expiry    int64    `json:"exp,omitempty"`
	IssuedAt  int64    `json:"iat,omitempty"`
	JTI       string   `json:"jti,omitempty"`
	ClientID  string   `json:"client_id,omitempty"`
	Scope     string   `json:"scope,omitempty"`
	TokenType string   `json:"token_type,omitempty"`
	Username  string   `json:"username,omitempty"`
}

// writeInactive emits the §2.2 `{"active":false}` response with the mandatory
// no-store header. RFC 7662 §2.2 forbids clients from caching introspection
// responses because they leak token lifetime and subject data.
func writeInactive(c echo.Context) error {
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusOK, introspectResponse{Active: false})
}

// IntrospectHandler implements RFC 7662 §2 (OAuth 2.0 Token Introspection).
// The endpoint consumes application/x-www-form-urlencoded and always returns
// HTTP 200 with a JSON body. Every negative outcome (missing / malformed /
// expired / bad signature / unknown kid / non-RS256 alg / wrong issuer / not
// an access token) funnels to `{"active": false}` per RFC 7662 §2.2's
// anti-enumeration rule: callers must not be able to distinguish "token never
// existed" from "token expired" from "wrong shape" by observing the response.
//
// The token_type_hint form parameter is accepted for RFC compatibility but
// ignored — only access-token introspection is supported.
func IntrospectHandler(d IntrospectDeps) echo.HandlerFunc {
	return func(c echo.Context) error {
		tok := c.FormValue("token")
		if tok == "" {
			return writeInactive(c)
		}

		if d.Keystore == nil {
			d.logger().Error("introspect: no keystore configured")
			return writeInactive(c)
		}

		// Delegate signature + issuer + client_id validation to token.VerifyLocal
		// so the same alg-confusion defence applies to /oauth/introspect and
		// /oauth/revoke without maintaining two copies of the logic.
		claims, err := token.VerifyLocal(d.Keystore, d.Issuer, tok)
		if err != nil {
			d.logger().Debug("introspect: token rejected", slog.Any("err", err))
			return writeInactive(c)
		}

		// Revocation check — RFC 7009 §2 + RFC 7662 §2.2 require
		// active=false on a token whose JTI / session / user / device has
		// been revoked. The check is best-effort: a DB error is logged
		// and the handler proceeds as if the token were valid, on the
		// principle that a revocation DB outage shouldn't lock every
		// authenticated user out of the system.
		if d.Revocation != nil {
			revoked, rErr := d.Revocation.IsAccessTokenRevoked(
				c.Request().Context(),
				claims.ID,
				claims.Subject,
				claims.DeviceID,
				claims.SessionID,
			)
			if rErr != nil {
				d.logger().Error("introspect: revocation lookup", slog.Any("err", rErr))
			} else if revoked {
				return writeInactive(c)
			}
		}

		resp := introspectResponse{
			Active:    true,
			Issuer:    claims.Issuer,
			Subject:   claims.Subject,
			Audience:  []string(claims.Audience),
			JTI:       claims.ID,
			ClientID:  claims.ClientID,
			Scope:     claims.Scope,
			TokenType: "Bearer",
		}
		if claims.ExpiresAt != nil {
			resp.Expiry = claims.ExpiresAt.Unix()
		}
		if claims.IssuedAt != nil {
			resp.IssuedAt = claims.IssuedAt.Unix()
		}
		// §2.2 treats username as a human-readable subject hint. We expose it
		// only when Email is present so callers do not mistake sub for it.
		if claims.Email != "" {
			resp.Username = claims.Email
		}

		c.Response().Header().Set("Cache-Control", "no-store")
		return c.JSON(http.StatusOK, resp)
	}
}

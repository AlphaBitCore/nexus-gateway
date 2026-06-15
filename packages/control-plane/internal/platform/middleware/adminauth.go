// Package middleware provides Echo middleware for the control-plane admin API.
package middleware

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authn"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/jwt"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/users/apikeystore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/metrics"
)

const adminAuthContextKey = "adminAuth"

// AdminAuthFromContext extracts the AdminAuth from an Echo context.
func AdminAuthFromContext(c echo.Context) *auth.AdminAuth {
	if v := c.Get(adminAuthContextKey); v != nil {
		return v.(*auth.AdminAuth)
	}
	return nil
}

// WithAdminAuth attaches an AdminAuth to the Echo context under the same key
// the middleware uses. Exported for tests that exercise handlers downstream of
// AdminAuth without mounting the full middleware stack.
func WithAdminAuth(c echo.Context, aa *auth.AdminAuth) {
	c.Set(adminAuthContextKey, aa)
}

// AdminAPIKeyLookup is called by the auth middleware to find an API key by hash.
type AdminAPIKeyLookup interface {
	FindByKeyHash(ctx context.Context, keyHash string) (*apikeystore.APIKeyWithOwner, error)
}

// AdminAPIKeyRehasher lazy-migrates an admin key's stored hash to the current
// HMAC keyring version after it authenticated under an OLDER version (the
// one-way-HMAC rotation path: an HMAC can't be re-sealed offline, so it
// migrates on next use). It updates ONLY keyHash + key_version, never the
// rotation-lifecycle columns (status/rotatedAt/rotatedFromId). matchedHash is
// the stored hash that admitted the key; the implementation compare-and-swaps
// on it so a key regenerated between admission and this write is never
// overwritten (which would resurrect the superseded key). Optional: a nil
// rehasher means try-all-versions still admits, but old-version keys are not
// migrated (they keep working under try-all).
type AdminAPIKeyRehasher interface {
	UpdateKeyHashAndVersion(ctx context.Context, id, keyHash, keyVersion, matchedHash string) error
}

// AdminAuthConfig holds dependencies for the admin auth middleware.
//
// JWTVerifier is required; AdminAuth panics at construction time if it is nil
// because the JWT path is the primary credential surface and silently failing
// open would break the entire admin UI.
type AdminAuthConfig struct {
	// JWTVerifier verifies access tokens minted by the auth server. Required.
	JWTVerifier *jwtverifier.Verifier
	// APIKeyLookup resolves an API key hash to a row plus owner fields. May be
	// nil when the DB is not wired; the x-admin-key path will then 401.
	APIKeyLookup AdminAPIKeyLookup
	// APIKeyRehasher lazy-migrates an admin key to the current HMAC keyring
	// version after it authenticates under an older one.
	// Optional; nil disables lazy migration (try-all-versions still admits).
	APIKeyRehasher AdminAPIKeyRehasher
	Logger         *slog.Logger
}

// AdminAuth returns Echo middleware that authenticates admin requests via one
// of exactly two credential surfaces, in this order:
//
//  1. Authorization: Bearer <jwt> — verified against the auth-server JWKS
//     (issuer, audience "cp-admin", signature, expiry, revocation). On success
//     the attached AdminAuth is derived from the JWT claims (sub → KeyID,
//     email → KeyName, type "admin_user").
//  2. x-admin-key: <raw-key> — HMAC-SHA256 hashed and looked up in api_keys.
//     On success the attached AdminAuth comes from auth.EffectivePrincipal,
//     which honours owner delegation.
//
// Missing credentials, invalid JWTs, and unknown/disabled API keys all return
// 401 with a JSON error envelope. JWT failures additionally set a 6750-style
// WWW-Authenticate header; API-key failures do not (API keys are a non-OAuth
// surface). Credentials are mutually exclusive: when Authorization: Bearer is
// present, the API-key header is ignored — clients should not send both.
func AdminAuth(cfg AdminAuthConfig) echo.MiddlewareFunc {
	if cfg.JWTVerifier == nil {
		panic("middleware.AdminAuth: JWTVerifier is required")
	}
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			authzHeader := c.Request().Header.Get("Authorization")
			if bearerTok, ok := extractBearer(authzHeader); ok {
				return authenticateJWT(c, cfg, bearerTok, next)
			}

			if apiKey := c.Request().Header.Get("x-admin-key"); apiKey != "" {
				return authenticateAPIKey(c, cfg, apiKey, next)
			}

			if metrics.AuthAttemptsTotal != nil {
				metrics.AuthAttemptsTotal.With("missing", "none").Inc()
			}
			return c.JSON(http.StatusUnauthorized, errorResp(
				"Authentication required", "authentication_error", "AUTH_REQUIRED",
			))
		}
	}
}

// authenticateJWT verifies the bearer token and, on success, attaches an
// AdminAuth derived from the JWT claims and calls next. Failures emit the
// RFC 6750 WWW-Authenticate challenge alongside the JSON error body.
func authenticateJWT(c echo.Context, cfg AdminAuthConfig, raw string, next echo.HandlerFunc) error {
	claims, err := cfg.JWTVerifier.Verify(c.Request().Context(), raw)
	if err != nil {
		if metrics.AuthAttemptsTotal != nil {
			metrics.AuthAttemptsTotal.With("invalid_jwt", "jwt").Inc()
		}
		c.Response().Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
		return c.JSON(http.StatusUnauthorized, errorResp(
			"Invalid or expired access token", "authentication_error", "INVALID_TOKEN",
		))
	}
	if metrics.AuthAttemptsTotal != nil {
		metrics.AuthAttemptsTotal.With("success", "jwt").Inc()
	}
	c.Set(adminAuthContextKey, &auth.AdminAuth{
		KeyID:             claims.Subject,
		KeyName:           firstNonEmpty(claims.Email, claims.Subject),
		AuthPrincipalType: "admin_user",
	})
	return next(c)
}

// authenticateAPIKey hashes the raw key, looks it up, and on success attaches
// the EffectivePrincipal (honouring owner delegation) and calls next.
func authenticateAPIKey(c echo.Context, cfg AdminAuthConfig, rawKey string, next echo.HandlerFunc) error {
	if cfg.APIKeyLookup == nil {
		if metrics.AuthAttemptsTotal != nil {
			metrics.AuthAttemptsTotal.With("invalid_api_key", "apikey").Inc()
		}
		return c.JSON(http.StatusUnauthorized, errorResp(
			"Invalid credentials", "authentication_error", "INVALID_CREDENTIALS",
		))
	}
	ctx := c.Request().Context()
	// Try every HMAC keyring version, current first. The
	// steady-state common case is a one-hash hit under the current version; older
	// versions are tried only after a rotation, until the key migrates.
	currentVersion := auth.CurrentKeyVersion()
	var ak *apikeystore.APIKeyWithOwner
	var matchedVersion, matchedHash string
	for _, vh := range auth.HashAPIKeyVersions(rawKey) {
		found, err := cfg.APIKeyLookup.FindByKeyHash(ctx, vh.Hash)
		if err != nil {
			cfg.Logger.Error("API key lookup failed", "error", err)
			if metrics.AuthAttemptsTotal != nil {
				metrics.AuthAttemptsTotal.With("error", "apikey").Inc()
			}
			return c.JSON(http.StatusInternalServerError, errorResp(
				"Authentication service error", "server_error", "AUTH_SERVICE_ERROR",
			))
		}
		if found != nil {
			ak = found
			matchedVersion = vh.Version
			matchedHash = vh.Hash
			break
		}
	}
	if ak == nil || !ak.Enabled {
		if metrics.AuthAttemptsTotal != nil {
			metrics.AuthAttemptsTotal.With("invalid_api_key", "apikey").Inc()
		}
		return c.JSON(http.StatusUnauthorized, errorResp(
			"Invalid or disabled API key", "authentication_error", "INVALID_API_KEY",
		))
	}
	// Lazy re-hash an enabled admin key admitted under an OLDER keyring version
	// up to the current version (CP owns the table, so the write side is
	// available — unlike the ai-gw VK path). Best-effort: a failed UPDATE never
	// blocks admission (the key still authenticates and migrates on a later
	// auth), and the UPDATE touches ONLY keyHash + key_version, never the
	// rotation-lifecycle columns. matchedHash makes the write a compare-and-swap:
	// if an admin regenerated the key between admission and this write, the
	// UPDATE matches no rows instead of resurrecting the superseded key.
	if matchedVersion != currentVersion && cfg.APIKeyRehasher != nil {
		newHash := auth.HashAPIKey(rawKey)
		if err := cfg.APIKeyRehasher.UpdateKeyHashAndVersion(ctx, ak.ID, newHash, currentVersion, matchedHash); err != nil {
			cfg.Logger.Warn("admin key lazy re-hash failed (non-fatal; key still admitted, migrates on next auth)",
				"error", err, "keyId", ak.ID, "fromVersion", matchedVersion, "toVersion", currentVersion)
		}
	}
	if metrics.AuthAttemptsTotal != nil {
		metrics.AuthAttemptsTotal.With("success", "apikey").Inc()
	}
	c.Set(adminAuthContextKey, auth.EffectivePrincipal(ak))
	return next(c)
}

// extractBearer returns the token from an Authorization: Bearer <token>
// header, stripping surrounding whitespace. The boolean reports whether a
// non-empty bearer value was found.
func extractBearer(authz string) (string, bool) {
	const prefix = "Bearer "
	if !strings.HasPrefix(authz, prefix) {
		return "", false
	}
	tok := strings.TrimSpace(authz[len(prefix):])
	if tok == "" {
		return "", false
	}
	return tok, true
}

// firstNonEmpty returns the first non-empty string from the provided values.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func errorResp(message, errType, code string) map[string]any {
	return map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    errType,
			"code":    code,
		},
	}
}

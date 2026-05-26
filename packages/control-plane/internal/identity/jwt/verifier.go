package jwtverifier

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Config wires a Verifier. Callers provide the auth server's issuer URL and
// JWKS URL, the expected audience for the resource server, and an optional
// revocation checker. A nil RevCheck falls back to AlwaysAllow{} in New —
// callers that want real revocation checks must opt in by setting RevCheck
// explicitly.
type Config struct {
	Issuer    string
	JWKSURL   string
	Audience  string
	ClockSkew time.Duration     // default 5 min when zero
	RevCheck  RevocationChecker // default AlwaysAllow{} when nil — opt-in required for real checks
	Logger    *slog.Logger
}

// Verifier validates access tokens against the cached JWKS and configured
// claim expectations. Safe for concurrent use; all state is immutable after
// New.
type Verifier struct {
	cfg  Config
	jwks *JWKSCache
}

// New constructs a Verifier. ClockSkew defaults to 5 minutes when zero.
// RevCheck defaults to AlwaysAllow{} when nil; pass a real RevocationChecker
// to enable revocation enforcement.
func New(cfg Config) *Verifier {
	if cfg.ClockSkew == 0 {
		cfg.ClockSkew = 5 * time.Minute
	}
	if cfg.RevCheck == nil {
		cfg.RevCheck = AlwaysAllow{}
	}
	return &Verifier{cfg: cfg, jwks: NewJWKSCache(cfg.JWKSURL)}
}

// Verify parses and validates the raw JWT. On success returns the typed
// Claims; failures map to the ErrXxx sentinels so callers can surface a
// precise WWW-Authenticate description.
func (v *Verifier) Verify(ctx context.Context, raw string) (*Claims, error) {
	tok, err := jwt.Parse(raw, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		return v.jwks.KeyByKID(ctx, kid)
	}, jwt.WithValidMethods([]string{"RS256"}), jwt.WithLeeway(v.cfg.ClockSkew))
	if err != nil {
		return nil, mapParseError(err)
	}
	mc, ok := tok.Claims.(jwt.MapClaims)
	if !ok {
		return nil, ErrMalformed
	}
	c := claimsFromMap(mc)
	if c.Issuer != v.cfg.Issuer {
		return nil, ErrWrongIssuer
	}
	if !contains(c.Audience, v.cfg.Audience) {
		return nil, ErrWrongAudience
	}
	// Reject structurally valid but principal-less tokens: an empty `sub`
	// would propagate "" into every downstream IAM/DB lookup as a ghost
	// principal id. The verifier is the trust boundary, so enforce
	// completeness here rather than trusting the minter.
	if c.Subject == "" {
		return nil, ErrMalformed
	}
	// Populate Raw before the revocation check so that RevocationChecker
	// implementations (e.g. MQRevocationChecker.introspect) can forward the
	// compact JWT to /oauth/introspect without re-serializing.
	c.Raw = raw
	revoked, err := v.cfg.RevCheck.IsRevoked(ctx, c)
	if err != nil {
		return nil, err
	}
	if revoked {
		return nil, ErrRevoked
	}
	return c, nil
}

// mapParseError converts jwt/v5 error sentinels and our JWKS error to the
// package-local error set. Unknown causes fall through to ErrInvalidSignature
// because jwt.Parse runs the signature check first — a non-sentinel error
// here almost always means the signature didn't verify.
func mapParseError(err error) error {
	switch {
	case errors.Is(err, ErrJWKSUnavailable):
		return ErrJWKSUnavailable
	case errors.Is(err, jwt.ErrTokenMalformed):
		return ErrMalformed
	case errors.Is(err, jwt.ErrTokenExpired):
		return ErrExpired
	case errors.Is(err, jwt.ErrTokenNotValidYet):
		return ErrNotYetValid
	}
	return ErrInvalidSignature
}

// claimsFromMap reads the subset of wire claims we surface. Missing optional
// claims leave zero values. Audience is tolerant of both []string and a bare
// string form per RFC 7519.
func claimsFromMap(m jwt.MapClaims) *Claims {
	c := &Claims{}
	c.Issuer, _ = m["iss"].(string)
	c.Subject, _ = m["sub"].(string)
	c.JTI, _ = m["jti"].(string)
	c.Email, _ = m["email"].(string)
	c.IDP, _ = m["idp"].(string)
	c.AuthMode, _ = m["auth_mode"].(string)
	c.ClientID, _ = m["client_id"].(string)
	c.Scope, _ = m["scope"].(string)
	c.DeviceID, _ = m["device_id"].(string)
	c.SessionID, _ = m["session_id"].(string)

	// Numeric claims arrive as float64 via encoding/json. jwt/v5 does not use
	// json.Number by default, so we only handle float64 and int64 here.
	c.ExpiresAt = claimInt64(m, "exp")
	c.IssuedAt = claimInt64(m, "iat")
	c.NotBefore = claimInt64(m, "nbf")

	c.Audience = claimStringSlice(m, "aud")
	c.AMR = claimStringSlice(m, "amr")
	return c
}

func claimInt64(m jwt.MapClaims, key string) int64 {
	switch v := m[key].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	}
	return 0
}

func claimStringSlice(m jwt.MapClaims, key string) []string {
	switch v := m[key].(type) {
	case []any:
		out := make([]string, 0, len(v))
		for _, x := range v {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case string:
		if v == "" {
			return nil
		}
		return []string{v}
	}
	return nil
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

package token

import (
	"errors"
	"fmt"

	"github.com/golang-jwt/jwt/v5"
)

// ErrInvalidAccessToken is returned by VerifyLocal when the token fails any
// of the structural, signature, issuer, or client_id checks. Callers funnel
// this to their own anti-enumeration response (inactive / silent 200). The
// error text is intentionally opaque -- we do not leak which check failed.
var ErrInvalidAccessToken = errors.New("token: invalid access token")

// VerifyLocal parses an RS256-signed access token against the supplied
// Keystore, enforcing the same defences the OAuth introspection endpoint
// applies:
//
//   - non-RS256 algorithms are rejected (alg-confusion defence)
//   - the kid header must resolve in Keystore.ByKID
//   - jwt.ParseWithClaims validates exp / nbf / iat
//   - when issuer is non-empty it must equal the claim's iss
//   - a missing client_id claim is treated as "not our token"
//
// It returns the decoded AccessClaims on success. The helper is shared by the
// RFC 7662 introspection handler and the RFC 7009 revocation handler so the
// same validation rules apply to both endpoints.
func VerifyLocal(ks *Keystore, issuer, raw string) (*AccessClaims, error) {
	if ks == nil {
		return nil, fmt.Errorf("%w: no keystore", ErrInvalidAccessToken)
	}
	if raw == "" {
		return nil, fmt.Errorf("%w: empty token", ErrInvalidAccessToken)
	}

	var claims AccessClaims
	parsed, err := jwt.ParseWithClaims(raw, &claims, func(jt *jwt.Token) (any, error) {
		if _, ok := jt.Method.(*jwt.SigningMethodRSA); !ok {
			return nil, jwt.ErrTokenSignatureInvalid
		}
		if jt.Method.Alg() != jwt.SigningMethodRS256.Alg() {
			return nil, jwt.ErrTokenSignatureInvalid
		}
		kid, _ := jt.Header["kid"].(string)
		if kid == "" {
			return nil, jwt.ErrTokenUnverifiable
		}
		k, ok := ks.ByKID(kid)
		if !ok {
			return nil, jwt.ErrTokenUnverifiable
		}
		return &k.Priv.PublicKey, nil
	})
	if err != nil || parsed == nil || !parsed.Valid {
		return nil, fmt.Errorf("%w: %w", ErrInvalidAccessToken, err)
	}

	// Issuer pinning: when configured, a mismatched iss is indistinguishable
	// from a forged token so we reject without echoing the foreign issuer.
	if issuer != "" && claims.Issuer != issuer {
		return nil, fmt.Errorf("%w: issuer mismatch", ErrInvalidAccessToken)
	}

	// A signed JWT lacking client_id is not an access token we minted
	// (IssueAccess always stamps it).
	if claims.ClientID == "" {
		return nil, fmt.Errorf("%w: missing client_id", ErrInvalidAccessToken)
	}

	return &claims, nil
}

package token

import (
	"errors"

	"github.com/golang-jwt/jwt/v5"
)

// Signer issues RS256-signed JWTs using the active key in a Keystore.
type Signer struct{ ks *Keystore }

// NewSigner constructs a Signer bound to ks.
func NewSigner(ks *Keystore) *Signer { return &Signer{ks: ks} }

// Sign returns a compact-serialized JWT signed with the keystore's active key.
// It fails if the keystore has no keys.
func (s *Signer) Sign(claims jwt.Claims) (string, error) {
	kid := s.ks.ActiveKID()
	if kid == "" {
		return "", errors.New("token: no active signing key")
	}
	key, ok := s.ks.ByKID(kid)
	if !ok {
		return "", errors.New("token: active kid not resolvable")
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = kid
	return tok.SignedString(key.Priv)
}

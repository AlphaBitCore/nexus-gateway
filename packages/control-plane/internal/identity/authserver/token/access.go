package token

import (
	"crypto/rand"
	"encoding/base64"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// AccessInput carries the claim set IssueAccess needs to mint an access token.
// Zero-valued optional fields (DeviceID, AMR, ...) are omitted from the
// serialised payload so the JSON representation stays compact. Audience is
// the fixed resource-server identifier ("cp-admin" in the current model);
// it does not vary per client.
type AccessInput struct {
	Issuer    string
	Subject   string // end-user id
	Audience  []string
	ClientID  string
	Scope     string
	SessionID string
	DeviceID  string
	Email     string
	IdPID     string
	AMR       []string
	TTL       time.Duration
}

// AccessClaims is the RFC 7519 claim set the auth server issues for Bearer
// access tokens. Keeping the struct concrete (rather than jwt.MapClaims) lets
// callers and tests round-trip claims with static typing.
type AccessClaims struct {
	jwt.RegisteredClaims
	ClientID  string   `json:"client_id,omitempty"`
	Scope     string   `json:"scope,omitempty"`
	SessionID string   `json:"sid,omitempty"`
	DeviceID  string   `json:"device_id,omitempty"`
	Email     string   `json:"email,omitempty"`
	IdPID     string   `json:"idp,omitempty"`
	AMR       []string `json:"amr,omitempty"`
}

// IssueAccess mints a signed RS256 access token. The returned jti matches
// claims.ID so callers can correlate the access token with adjacent artefacts
// (e.g. the refresh row that was just inserted) without re-parsing the JWT.
func IssueAccess(s *Signer, in AccessInput) (string, string, error) {
	jti := newJTI()
	now := time.Now()
	claims := AccessClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    in.Issuer,
			Subject:   in.Subject,
			Audience:  jwt.ClaimStrings(in.Audience),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(in.TTL)),
			ID:        jti,
		},
		ClientID:  in.ClientID,
		Scope:     in.Scope,
		SessionID: in.SessionID,
		DeviceID:  in.DeviceID,
		Email:     in.Email,
		IdPID:     in.IdPID,
		AMR:       in.AMR,
	}
	tok, err := s.Sign(claims)
	if err != nil {
		return "", "", err
	}
	return tok, jti, nil
}

// newJTI returns a 128-bit base64url-encoded random identifier suitable for
// RFC 7519 §4.1.7. A crypto/rand failure is treated as fatal because a
// predictable jti would let attackers guess and replay tokens.
func newJTI() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("token: crypto/rand failed: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}

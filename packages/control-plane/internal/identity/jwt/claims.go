// Package jwtverifier verifies access tokens minted by the Nexus auth server.
// It provides a Verifier that resource servers (Hub, AI Gateway, Compliance
// Proxy) can mount as Echo middleware.
package jwtverifier

// Claims is the access-token payload emitted by the Nexus auth server.
// Field tags use wire names; all fields are RFC 7519 / OIDC + platform extras.
type Claims struct {
	Issuer    string   `json:"iss"`
	Subject   string   `json:"sub"`
	Audience  []string `json:"aud"`
	ExpiresAt int64    `json:"exp"`
	IssuedAt  int64    `json:"iat"`
	NotBefore int64    `json:"nbf"`
	JTI       string   `json:"jti"`

	ClientID  string   `json:"client_id"`
	Scope     string   `json:"scope"`
	DeviceID  string   `json:"device_id,omitempty"`
	SessionID string   `json:"session_id,omitempty"`
	Email     string   `json:"email"`
	IDP       string   `json:"idp"`
	AuthMode  string   `json:"auth_mode,omitempty"`
	AMR       []string `json:"amr"`

	// Raw is the compact-serialized JWT that produced these claims. Populated
	// by Verifier.Verify on success so downstream revocation checkers (notably
	// MQRevocationChecker.introspect) can forward the token to /oauth/introspect
	// without re-serializing. No JSON tag: Raw is set by the verifier itself,
	// never read from the token payload.
	Raw string `json:"-"`
}

// Package idp abstracts identity-provider adapters used by the auth server
// during interactive login. Each adapter (local, OIDC, SAML) implements the
// IdP interface and returns a normalized AuthResult that the login handlers
// use to mint authorization codes.
//
// Role resolution is deliberately NOT the adapter's job: NexusUser carries
// no role column, and roles are resolved from IamPolicy at token-issuance
// time. Adapters only authenticate and surface the IdP-local identity.
package idp

import (
	"context"
	"errors"
)

// ErrInvalidCredentials is returned when an authentication attempt fails for
// any user-facing reason (wrong password, unknown email, malformed input).
// Adapters MUST NOT leak the distinction between "user not found" and
// "password mismatch" to callers — both map to this sentinel so the HTTP
// response does not enable user enumeration.
var ErrInvalidCredentials = errors.New("idp: invalid credentials")

// AuthResult is the normalized authentication outcome produced by every IdP
// adapter. Fields are the minimum the downstream authorization-code + token
// flow needs to build an ID token (Task 1.11); additional claims are
// resolved at token-issuance time from IamPolicy.
type AuthResult struct {
	UserID string   // NexusUser.id
	IdPID  string   // IdentityProvider.id (UUID string)
	Email  string   // lowercased, trimmed; may be empty when the IdP omits it
	AMR    []string // RFC 8176 authentication method references, e.g. []string{"pwd"}
}

// IdP is the adapter contract. Input is a flat string map so the same
// signature covers password-based and SSO-callback flows; adapters document
// the keys they consume.
type IdP interface {
	Authenticate(ctx context.Context, input map[string]string) (*AuthResult, error)
}

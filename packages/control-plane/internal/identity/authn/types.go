// Package auth provides admin-principal identity (AdminAuth), password
// hashing, and API key hashing + lookup for the control-plane admin API.
//
// Admin authentication runs exclusively through two middleware-enforced
// credential surfaces: OAuth access tokens (verified by
// packages/control-plane/internal/jwtverifier) and admin API keys (hashed here and looked
// up in store.APIKeyWithOwner). Session cookies and CSRF double-submit
// tokens are no longer part of the auth model.
package auth

// AdminAuth holds the authenticated principal identity attached to each
// admin request by the auth middleware.
type AdminAuth struct {
	KeyID                 string
	KeyName               string
	AuthPrincipalType     string // "admin_user", "api_key"
	DelegatedFromAPIKeyID string // non-empty when API key owner delegates to user
}

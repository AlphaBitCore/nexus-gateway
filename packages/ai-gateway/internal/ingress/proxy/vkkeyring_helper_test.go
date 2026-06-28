package proxy

import "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/hmackeyring"

// vkKeyring builds a single-version (v1) HMAC keyring for tests — the
// non-rotating ADMIN_KEY_HMAC_SECRET path (Layer A). Mirrors the
// boot-time keyring the AI Gateway injects into vkauth.NewAuthenticator.
func vkKeyring(secret string) *hmackeyring.Keyring {
	kr, err := hmackeyring.Single(secret)
	if err != nil {
		panic(err)
	}
	return kr
}

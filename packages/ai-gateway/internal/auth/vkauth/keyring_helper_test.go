package vkauth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/hmackeyring"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/keyderive"
)

// mustKeyring builds a single-version (v1) HMAC keyring for tests — the
// non-rotating ADMIN_KEY_HMAC_SECRET path. Panics on the (impossible for a
// non-empty secret) error so call sites stay terse.
func mustKeyring(secret string) *hmackeyring.Keyring {
	kr, err := hmackeyring.Single(secret)
	if err != nil {
		panic(err)
	}
	return kr
}

// mustKeyringMap builds a multi-version keyring from a "[*]vN:secret" map for the
// try-all-versions tests.
func mustKeyringMap(keyMap string) *hmackeyring.Keyring {
	kr, err := hmackeyring.New(keyMap)
	if err != nil {
		panic(err)
	}
	return kr
}

// vkHashFor is the test oracle for the VK hash the authenticator computes under a
// given keyring secret — derive the VK-domain sub-key, then HMAC the token. Used
// to seed GetVirtualKeyByHash fakes with the hash a real admission would look up.
func vkHashFor(secret, token string) string {
	sub := keyderive.DeriveSubkey([]byte(secret), keyderive.ClassAPIKeyVirtualKey)
	mac := hmac.New(sha256.New, sub[:])
	mac.Write([]byte(token))
	return hex.EncodeToString(mac.Sum(nil))
}

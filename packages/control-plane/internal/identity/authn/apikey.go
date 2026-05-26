package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"os"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/users/apikeystore"
)

const (
	apiKeyPrefix    = "nxk_"
	apiKeyBytes     = 32
	hmacDevFallback = "nexus-gateway-default-hmac-secret"
)

// randRead is the entropy source used by GenerateAPIKey. Indirected through a
// package-level var (defaulting to crypto/rand.Read) so unit tests can exercise
// the rand-failure branch without monkey-patching the standard library.
// Production callers MUST NOT reassign this; the default is crypto/rand.
var randRead = rand.Read

// HMACSecret returns the HMAC key used for API key hashing.
// In non-production, falls back to a dev default.
func HMACSecret() string {
	if s := os.Getenv("ADMIN_KEY_HMAC_SECRET"); s != "" {
		return s
	}
	return hmacDevFallback
}

// ValidateHMACSecret checks that ADMIN_KEY_HMAC_SECRET is set in production.
// Call at startup to prevent running with the dev fallback in production.
func ValidateHMACSecret() error {
	if os.Getenv("ADMIN_KEY_HMAC_SECRET") != "" {
		return nil
	}
	if os.Getenv("NODE_ENV") == "production" {
		return fmt.Errorf("ADMIN_KEY_HMAC_SECRET is required in production")
	}
	return nil
}

// HashAPIKey hashes an API key using HMAC-SHA256, matching the Node.js format.
func HashAPIKey(key string) string {
	mac := hmac.New(sha256.New, []byte(HMACSecret()))
	mac.Write([]byte(key))
	return hex.EncodeToString(mac.Sum(nil))
}

// GenerateAPIKey creates a new API key with prefix, hash, and display prefix.
func GenerateAPIKey() (key, hash, prefix string, err error) {
	raw := make([]byte, apiKeyBytes)
	if _, err := randRead(raw); err != nil {
		return "", "", "", fmt.Errorf("generate key: %w", err)
	}
	key = apiKeyPrefix + hex.EncodeToString(raw)
	hash = HashAPIKey(key)
	prefix = key[:12]
	return key, hash, prefix, nil
}

// TimingSafeEqual compares two strings in constant time.
func TimingSafeEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// EffectivePrincipal resolves whether an API key authenticates as itself
// or delegates to its owner user.
func EffectivePrincipal(ak *apikeystore.APIKeyWithOwner) *AdminAuth {
	if ak.OwnerUserID != nil && ak.OwnerID != nil &&
		ak.OwnerEnabled != nil && *ak.OwnerEnabled {
		return &AdminAuth{
			KeyID:                 *ak.OwnerID,
			KeyName:               deref(ak.OwnerDisplayName),
			AuthPrincipalType:     "admin_user",
			DelegatedFromAPIKeyID: ak.ID,
		}
	}
	return &AdminAuth{
		KeyID:             ak.ID,
		KeyName:           ak.Name,
		AuthPrincipalType: "api_key",
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

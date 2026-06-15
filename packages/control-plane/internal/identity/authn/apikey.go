package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/users/apikeystore"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/hmackeyring"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/keyderive"
)

const (
	apiKeyPrefix = "nxk_"
	apiKeyBytes  = 32
)

// randRead is the entropy source used by GenerateAPIKey. Indirected through a
// package-level var (defaulting to crypto/rand.Read) so unit tests can exercise
// the rand-failure branch without monkey-patching the standard library.
// Production callers MUST NOT reassign this; the default is crypto/rand.
var randRead = rand.Read

// injectedKeyring is the versioned HMAC keyring used for API key + virtual-key
// hashing, installed by InitHMACKeyring at boot before any
// authentication path runs. It supersedes the prior single injected secret: the
// keyring's *current version hashes NEWLY issued keys, and every version is
// tried on admission so the HMAC secret can rotate without a fleet lockstep flip.
//
// The per-version secret is still resolved through the SecretCustody loader
// — under provider "command" the ADMIN_KEY_HMAC_KEY_MAP /
// ADMIN_KEY_HMAC_SECRET env carries a wrapped blob unwrapped once into config —
// so the UNWRAPPED plaintext (never the blob) keys every hash. There is NO
// in-code fallback. Set ONCE at boot and read-only thereafter
// (config.validate hard-fails an empty secret; InitHMACKeyring rejects nil), so
// the concurrent request-path reads need no lock.
var injectedKeyring *hmackeyring.Keyring

// InitHMACKeyring installs the boot-resolved HMAC keyring into the hashing
// layer. It MUST be called during boot, before any admin/VK authentication or
// key generation runs — the Control Plane calls it from wiring.InitBootstrap
// right after config.Load resolves custody. A nil keyring is rejected
// defensively (the config layer already builds a non-nil keyring or fails boot).
func InitHMACKeyring(kr *hmackeyring.Keyring) error {
	if kr == nil {
		return fmt.Errorf("ADMIN_KEY_HMAC keyring is required (no dev fallback; must match the AI Gateway)")
	}
	injectedKeyring = kr
	return nil
}

// VersionedHash pairs a keyring version id with the key_hash computed under that
// version. Admission tries these in order (current first).
type VersionedHash struct {
	Version string
	Hash    string
}

// hashKeyForClassWith HMAC-SHA256s key under a class-separated sub-key derived
// from the given keyring secret. The admin-API-key domain and the
// virtual-key domain use DISTINCT derived sub-keys (keyderive classes), so a
// forgery oracle or leak scoped to one domain cannot mint a credential in the
// other. The derivation is the shared keyderive contract, so the AI Gateway
// VK-admission side computes an identical VK hash from the same version's secret.
func hashKeyForClassWith(secret []byte, key, class string) string {
	sub := keyderive.DeriveSubkey(secret, class)
	mac := hmac.New(sha256.New, sub[:])
	mac.Write([]byte(key))
	return hex.EncodeToString(mac.Sum(nil))
}

// hashKeyForClass hashes key under the keyring's CURRENT version — the hash
// stamped on a newly issued key.
func hashKeyForClass(key, class string) string {
	_, secret := injectedKeyring.Current()
	return hashKeyForClassWith(secret, key, class)
}

// CurrentKeyVersion returns the keyring's current version id, stamped on the
// key_version column of every newly issued admin/user API key + virtual key.
func CurrentKeyVersion() string {
	return injectedKeyring.CurrentVersion()
}

// HashAPIKey hashes an admin/user API key under the admin domain sub-key of the
// keyring's CURRENT version (the hash recorded at issue time).
func HashAPIKey(key string) string {
	return hashKeyForClass(key, keyderive.ClassAPIKeyAdmin)
}

// HashVirtualKey hashes a virtual key under the virtual-key domain sub-key of
// the keyring's CURRENT version. [MUST MATCH] the AI Gateway's vkauth, which
// derives the same sub-key from the same version's secret.
func HashVirtualKey(key string) string {
	return hashKeyForClass(key, keyderive.ClassAPIKeyVirtualKey)
}

// HashAPIKeyVersions returns the admin-domain key_hash under EVERY keyring
// version, current first — the try-all-versions admission sequence. The caller
// looks each hash up newest-first; on a hit under a non-current version it
// lazy-rehashes the row to the current version (the one-way-HMAC rotation path).
func HashAPIKeyVersions(key string) []VersionedHash {
	entries := injectedKeyring.All()
	out := make([]VersionedHash, 0, len(entries))
	for _, e := range entries {
		out = append(out, VersionedHash{
			Version: e.Version,
			Hash:    hashKeyForClassWith(e.Secret, key, keyderive.ClassAPIKeyAdmin),
		})
	}
	return out
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

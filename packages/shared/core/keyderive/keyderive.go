// Package keyderive is the single source of truth for the HKDF key-derivation
// and AAD-construction contract shared by every service that seals or admits a
// secret. It exists so the Control Plane and the AI Gateway — which run
// SEPARATE copies of the credential-encryption and API-key-hashing code — derive
// byte-identical sub-keys and build byte-identical Additional Authenticated Data.
//
// Why it must be shared: each master secret in the
// fleet currently keys two independent trust domains at once, so one leak
// compromises both. The fix derives a DISTINCT sub-key per domain via
// HKDF-SHA256 (so a domain-scoped leak cannot forge the other domain) and binds
// each credential ciphertext to its own row identity via AAD (so a
// cross-credential ciphertext swap fails authentication instead of silently
// yielding the wrong upstream key). The derivation and the AAD bytes are a
// cross-service [MUST MATCH] wire contract: if the CP seal side and the ai-gw
// open side disagree by one byte, every decrypt fails. Centralizing the contract
// here makes "[MUST MATCH]" a compile-time guarantee, not a convention.
package keyderive

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/hkdf"
)

// Class info strings select an independent derived sub-key per trust domain.
// Each carries an explicit /vN scheme version: bumping it re-derives that
// class's key (and thus invalidates ciphertext/hashes sealed under the old
// version), so a scheme change is deliberate and the old/new keys are
// distinguishable. NEVER reuse a class string across domains — that would
// re-merge the blast radius the split exists to separate.
const (
	// ClassProviderCredential — AES-GCM sub-key for provider upstream API keys
	// (CP Vault seal ↔ ai-gw Decryptor open). [MUST MATCH] CP ↔ ai-gw.
	ClassProviderCredential = "nexus/cred/provider-api-key/v1"
	// ClassAlertChannelSecret — AES-GCM sub-key for Hub alert-channel secrets.
	ClassAlertChannelSecret = "nexus/cred/alert-channel-secret/v1"
	// ClassAPIKeyAdmin — HMAC sub-key for admin/user API-key admission (CP).
	ClassAPIKeyAdmin = "nexus/apikey/admin/v1"
	// ClassAPIKeyVirtualKey — HMAC sub-key for virtual-key admission (ai-gw).
	ClassAPIKeyVirtualKey = "nexus/apikey/virtual-key/v1"
)

// ErrEmptyMaster is returned when the master key material is empty.
var ErrEmptyMaster = errors.New("keyderive: empty master key")

// DeriveKey32 derives a 32-byte sub-key from master via HKDF-SHA256 with the
// given class as the HKDF info and an empty (nil) salt. It is deterministic: the
// same (master, class) pair always yields the same sub-key, so independent
// services derive matching keys with no coordination. Distinct class strings
// yield cryptographically independent sub-keys.
//
// salt is intentionally empty: the master is already a high-entropy 32-byte
// value (validated by core/keycheck), so HKDF-Extract with a zero salt is
// sufficient for domain separation; the security comes from the per-class info,
// not a salt. A nil/empty master or class is rejected (fail-closed) rather than
// producing a silently-weak key.
func DeriveKey32(master []byte, class string) ([32]byte, error) {
	var out [32]byte
	if len(master) == 0 {
		return out, ErrEmptyMaster
	}
	if class == "" {
		return out, fmt.Errorf("keyderive: empty class info")
	}
	r := hkdf.New(sha256.New, master, nil, []byte(class))
	// A 32-byte read from HKDF-SHA256 never short-reads: the reader only
	// errors past 255*32 bytes of total output.
	_, _ = io.ReadFull(r, out[:])
	return out, nil
}

// DeriveSubkey is the total (never-erroring) variant of DeriveKey32 for the
// API-key / virtual-key HMAC path. Hashing an API key must be a pure
// function that always returns a value — the empty-secret protection lives in
// each service's boot gate (`ValidateHMACSecret` on the Control Plane,
// `config.validate()` on the AI Gateway), not here, so this does NOT reject an
// empty master (HKDF-Extract is well-defined for empty input material). It still
// gives byte-identical, class-separated sub-keys, so the CP key-minting side and
// the AI Gateway VK-admission side agree by construction. Credential SEALING
// uses DeriveKey32 (strict, fail-closed) instead; do not cross the two.
func DeriveSubkey(master []byte, class string) [32]byte {
	var out [32]byte
	r := hkdf.New(sha256.New, master, nil, []byte(class))
	_, _ = io.ReadFull(r, out[:]) // a 32-byte read from HKDF never short-reads
	return out
}

// ProviderCredentialAAD binds a provider-credential ciphertext to its own
// credential id and provider id. The CP seal side and the ai-gw open
// side MUST construct this identically, so a ciphertext copied from credential A
// into credential B's row fails GCM authentication on open instead of yielding
// A's (possibly higher-privilege) upstream key under B's identity. The leading
// scheme tag namespaces the AAD so it can never collide with another class's.
func ProviderCredentialAAD(credentialID, providerID string) []byte {
	return []byte("nexus/cred/v1|cred:" + credentialID + "|provider:" + providerID)
}

// AlertChannelAAD binds a Hub alert-channel secret ciphertext to its channel id,
// so an alert-channel blob cannot be swapped between channels.
func AlertChannelAAD(channelID string) []byte {
	return []byte("nexus/alert/v1|channel:" + channelID)
}

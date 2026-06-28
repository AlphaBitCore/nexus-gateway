// Package creddecrypt implements AES-256-GCM credential decryption for the
// AI gateway. It is a pure-crypto leaf package with no dependency on the
// credential manager or any gateway runtime state.
package creddecrypt

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/keycheck"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/keyderive"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/keymap"
)

var (
	// ErrKeyNotInitialized indicates the encryption key has not been loaded.
	ErrKeyNotInitialized = errors.New("credentials: encryption key not initialized")
	// ErrDecryptFailed indicates AES-GCM decryption failed (bad key, corrupted data, or wrong tag).
	ErrDecryptFailed = errors.New("credentials: decryption failed")
)

// Decryptor handles AES-256-GCM decryption of stored credentials.
type Decryptor struct {
	gcm cipher.AEAD
}

// NewDecryptor creates a Decryptor from a hex-encoded 256-bit key.
func NewDecryptor(keyHex string) (*Decryptor, error) {
	key, err := hex.DecodeString(keyHex)
	if err != nil {
		return nil, fmt.Errorf("credentials: invalid hex key: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("credentials: key must be 32 bytes, got %d", len(key))
	}
	// Refuse a degenerate / known-constant master key (symmetric with
	// the Control Plane minting side — the same [MUST MATCH] secret).
	if err := keycheck.ValidateMasterKey(key); err != nil {
		return nil, fmt.Errorf("credentials: %w", err)
	}

	// Derive the provider-credential class sub-key from the master via
	// HKDF — the SAME derivation the Control Plane seal side performs — so the raw
	// master is never the AEAD key and the provider-credential class is separated
	// from the alert-channel class that shares the env value. [MUST MATCH] CP.
	sub, err := keyderive.DeriveKey32(key, keyderive.ClassProviderCredential)
	if err != nil {
		return nil, fmt.Errorf("credentials: derive provider key: %w", err)
	}
	block, err := aes.NewCipher(sub[:])
	if err != nil {
		return nil, fmt.Errorf("credentials: aes.NewCipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("credentials: cipher.NewGCM: %w", err)
	}

	return &Decryptor{gcm: gcm}, nil
}

// Decrypt decrypts a credential stored as hex-encoded ciphertext, IV, and tag.
// The IV must be 12 bytes (96 bits) and the tag must be 16 bytes (128 bits).
// aad is the row-identity binding — keyderive.ProviderCredentialAAD(
// credentialID, providerID) — and MUST match the value the Control Plane sealed
// with, or GCM authentication fails (so a ciphertext swapped from another
// credential's row is rejected instead of yielding the wrong upstream key).
func (d *Decryptor) Decrypt(ciphertextHex, ivHex, tagHex string, aad []byte) (string, error) {
	if d == nil {
		return "", ErrKeyNotInitialized
	}

	ciphertext, err := hex.DecodeString(ciphertextHex)
	if err != nil {
		return "", fmt.Errorf("credentials: invalid ciphertext hex: %w", err)
	}
	iv, err := hex.DecodeString(ivHex)
	if err != nil {
		return "", fmt.Errorf("credentials: invalid iv hex: %w", err)
	}
	tag, err := hex.DecodeString(tagHex)
	if err != nil {
		return "", fmt.Errorf("credentials: invalid tag hex: %w", err)
	}

	if len(iv) != 12 {
		return "", fmt.Errorf("credentials: iv must be 12 bytes, got %d", len(iv))
	}
	if len(tag) != 16 {
		return "", fmt.Errorf("credentials: tag must be 16 bytes, got %d", len(tag))
	}

	// Go's GCM expects tag appended to ciphertext. Allocate a fresh slice
	// rather than `append(ciphertext, tag...)` so a backing-array overlap
	// can never mutate the caller's ciphertext slice (gocritic appendAssign).
	sealed := make([]byte, 0, len(ciphertext)+len(tag))
	sealed = append(sealed, ciphertext...)
	sealed = append(sealed, tag...)

	plaintext, err := d.gcm.Open(nil, iv, sealed, aad)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrDecryptFailed, err)
	}

	return string(plaintext), nil
}

// MultiDecryptor supports decryption with multiple key versions.
type MultiDecryptor struct {
	keys map[string]*Decryptor
}

// NewMultiDecryptor creates a MultiDecryptor from a comma-separated key map in
// the format "[*]keyID1:hexKey1,keyID2:hexKey2,...".
//
// The wire parse (comma-split, first-colon id:value split, leading
// "*" current-marker STRIP, blank-entry skip, dup-id / empty-map fail-closed) is
// delegated to the shared keymap.Parse — the SAME leaf the Control Plane uses to
// mint via crypto.NewMultiVault. The "*" current-marker MUST be stripped: an
// operator's documented "*v2:" current-marker would otherwise store the key
// under literal id "*v2" while the CP stamps ciphertext id "v2", so every
// decrypt would 404 with "unknown key ID". Stripping here upholds the
// CREDENTIAL_KEY_MAP [MUST MATCH] contract: the gateway keys
// by the same stripped id the CP stamps. The gateway has no "current" concept
// (it opens by the stamped id), so the parsed currentID is intentionally
// ignored; only the stripped-id → value entries matter.
func NewMultiDecryptor(keyMap string) (*MultiDecryptor, error) {
	entries, _, _, _, err := keymap.Parse(keyMap, nil)
	if err != nil {
		// Preserve the package's error prefix for callers/log greps.
		return nil, fmt.Errorf("credentials: %w", err)
	}
	md := &MultiDecryptor{keys: make(map[string]*Decryptor, len(entries))}
	for id, hexKey := range entries {
		d, derr := NewDecryptor(hexKey)
		if derr != nil {
			return nil, fmt.Errorf("credentials: key %q: %w", id, derr)
		}
		md.keys[id] = d
	}
	return md, nil
}

// Decrypt decrypts using the key identified by keyID. aad is the
// row-identity binding (see Decryptor.Decrypt).
func (md *MultiDecryptor) Decrypt(keyID, ciphertextHex, ivHex, tagHex string, aad []byte) (string, error) {
	d, ok := md.keys[keyID]
	if !ok {
		return "", fmt.Errorf("credentials: unknown key ID: %q", keyID)
	}
	return d.Decrypt(ciphertextHex, ivHex, tagHex, aad)
}

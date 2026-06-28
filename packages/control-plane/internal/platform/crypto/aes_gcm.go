// Package crypto provides AES-256-GCM encryption/decryption compatible with
// the Node.js credential vault format (hex-encoded ciphertext, IV, and tag).
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/keycheck"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/keyderive"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/keymap"
)

// VaultConfig holds the parameters needed to initialize the credential vault.
type VaultConfig struct {
	EncryptionKey string
	Production    bool // true when running in production mode
}

const (
	ivLength  = 12 // 96-bit IV recommended for GCM
	tagLength = 16 // 128-bit auth tag
)

// randReader is the entropy source used by Encrypt to generate per-call IVs.
// It is a package-level variable solely so tests can substitute a failing
// reader and exercise the IV-generation error branch; production code never
// reassigns it. Default: crypto/rand.Reader.
var randReader io.Reader = rand.Reader

// newGCM constructs a GCM AEAD from the given block cipher. It is a
// package-level variable solely so tests can substitute a failing factory
// and exercise the "create GCM" error branch in Encrypt/Decrypt; production
// code never reassigns it.
var newGCM = func(block cipher.Block) (cipher.AEAD, error) {
	return cipher.NewGCMWithTagSize(block, tagLength)
}

// Vault holds the provider-credential AES-256-GCM sub-key and provides
// encrypt/decrypt. The sub-key is HKDF-derived from the configured master at
// construction: the raw master is NEVER used as an AEAD key
// directly, so the provider-credential domain is cryptographically separated
// from the other class (alert-channel secrets) that shares the same master env
// value. Every Encrypt/Decrypt also binds row-identity AAD.
type Vault struct {
	key []byte // HKDF(master, ClassProviderCredential) — NOT the raw master
}

// newProviderVault derives the provider-credential sub-key from a validated
// 32-byte master and returns a Vault. Centralizes the derivation so every
// construction path (InitVault / NewVault / NewMultiVault) is identical.
func newProviderVault(master []byte) (*Vault, error) {
	sub, err := keyderive.DeriveKey32(master, keyderive.ClassProviderCredential)
	if err != nil {
		return nil, fmt.Errorf("derive provider-credential key: %w", err)
	}
	dst := make([]byte, 32)
	copy(dst, sub[:])
	return &Vault{key: dst}, nil
}

// EncryptResult holds hex-encoded ciphertext, IV, and authentication tag.
type EncryptResult struct {
	Ciphertext string
	IV         string
	Tag        string
}

// InitVault initializes the credential vault from the provided config.
// Returns (nil, nil) when encryption is unavailable in non-production mode.
//
// Key custody (KMS/HSM): the AES-256-GCM master credential key is loaded from
// the CREDENTIAL_ENCRYPTION_KEY env variable. Generate a key with:
//
//	openssl rand -hex 32
//
// There is intentionally no KMS/HSM integration (no AWS KMS / GCP Cloud KMS /
// Azure Key Vault / PKCS#11 HSM) wired here: the key lives in process memory,
// sourced from the environment. Operators running in production must supply
// CREDENTIAL_ENCRYPTION_KEY from a secrets manager / systemd EnvironmentFile /
// K8s Secret rather than a checked-in value.
func InitVault(vcfg VaultConfig, logger *slog.Logger) (*Vault, error) {
	keyHex := vcfg.EncryptionKey

	if keyHex == "" {
		if vcfg.Production {
			return nil, errors.New(
				"CREDENTIAL_ENCRYPTION_KEY is required in production",
			)
		}
		logger.Warn("CREDENTIAL_ENCRYPTION_KEY not set — credential vault unavailable")
		return nil, nil
	}

	if len(keyHex) != 64 {
		return nil, errors.New("CREDENTIAL_ENCRYPTION_KEY must be exactly 64 hex characters (32 bytes)")
	}

	key, err := hex.DecodeString(keyHex)
	if err != nil {
		return nil, fmt.Errorf("CREDENTIAL_ENCRYPTION_KEY is not valid hex: %w", err)
	}
	if len(key) != 32 {
		return nil, errors.New("CREDENTIAL_ENCRYPTION_KEY decoded to wrong length; must be 32 bytes")
	}
	// Refuse an obviously weak/known key (all-zeros, a committed
	// example, a degenerate byte set) at boot, so a misconfigured deployment
	// fails closed instead of encrypting every credential under a guessable key.
	if err := keycheck.ValidateMasterKey(key); err != nil {
		return nil, fmt.Errorf("CREDENTIAL_ENCRYPTION_KEY rejected: %w", err)
	}

	logger.Info("Credential encryption key loaded successfully")
	return newProviderVault(key)
}

// NewVault creates a Vault from a raw 32-byte master key (for testing). The
// provider-credential sub-key is HKDF-derived from it, same as production.
func NewVault(key []byte) (*Vault, error) {
	if len(key) != 32 {
		return nil, errors.New("key must be exactly 32 bytes")
	}
	return newProviderVault(key)
}

// Encrypt encrypts plaintext and returns hex-encoded ciphertext, IV, and tag.
// aad binds the ciphertext to its row identity — e.g.
// keyderive.ProviderCredentialAAD(credentialID, providerID). It MUST be passed
// identically to Decrypt or GCM authentication fails. A nil/empty aad is
// permitted only for callers with no row identity (none on the credential path).
func (v *Vault) Encrypt(plaintext string, aad []byte) (*EncryptResult, error) {
	block, err := aes.NewCipher(v.key)
	if err != nil {
		return nil, fmt.Errorf("create cipher: %w", err)
	}

	aead, err := newGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create GCM: %w", err)
	}

	iv := make([]byte, ivLength)
	if _, err := io.ReadFull(randReader, iv); err != nil {
		return nil, fmt.Errorf("generate IV: %w", err)
	}

	// Seal appends ciphertext+tag; aad is authenticated but not encrypted.
	sealed := aead.Seal(nil, iv, []byte(plaintext), aad)

	// Split: ciphertext is everything except the last tagLength bytes
	ct := sealed[:len(sealed)-tagLength]
	tag := sealed[len(sealed)-tagLength:]

	return &EncryptResult{
		Ciphertext: hex.EncodeToString(ct),
		IV:         hex.EncodeToString(iv),
		Tag:        hex.EncodeToString(tag),
	}, nil
}

// Decrypt decrypts hex-encoded ciphertext using the given IV and tag. aad must
// match the value passed to Encrypt (the row-identity binding); a
// mismatch (e.g. a ciphertext swapped from another credential's row) fails GCM
// authentication and returns an error instead of yielding the wrong plaintext.
func (v *Vault) Decrypt(ciphertextHex, ivHex, tagHex string, aad []byte) (string, error) {
	ct, err := hex.DecodeString(ciphertextHex)
	if err != nil {
		return "", fmt.Errorf("decode ciphertext: %w", err)
	}
	iv, err := hex.DecodeString(ivHex)
	if err != nil {
		return "", fmt.Errorf("decode IV: %w", err)
	}
	// GCM panics if the nonce is the wrong length — guard at the boundary
	// so a corrupt Credential.ivHex row returns a clean error instead of
	// crashing the goroutine. Same defense for the tag: a wrong-length tag
	// would be silently appended to ciphertext and produce a misleading
	// "decrypt" error from Open; an explicit length check makes the failure
	// reason obvious in logs.
	if len(iv) != ivLength {
		return "", fmt.Errorf("invalid IV length: got %d bytes, want %d", len(iv), ivLength)
	}
	tag, err := hex.DecodeString(tagHex)
	if err != nil {
		return "", fmt.Errorf("decode tag: %w", err)
	}
	if len(tag) != tagLength {
		return "", fmt.Errorf("invalid tag length: got %d bytes, want %d", len(tag), tagLength)
	}

	block, err := aes.NewCipher(v.key)
	if err != nil {
		return "", fmt.Errorf("create cipher: %w", err)
	}

	aead, err := newGCM(block)
	if err != nil {
		return "", fmt.Errorf("create GCM: %w", err)
	}

	// Reconstruct sealed = ciphertext + tag (what Open expects)
	sealed := make([]byte, len(ct)+len(tag))
	copy(sealed, ct)
	copy(sealed[len(ct):], tag)

	plaintext, err := aead.Open(nil, iv, sealed, aad)
	if err != nil {
		return "", fmt.Errorf("decrypt: %w", err)
	}

	return string(plaintext), nil
}

// MultiVault supports multiple encryption keys identified by version IDs.
type MultiVault struct {
	current string
	keys    map[string]*Vault
}

// NewMultiVault parses a comma-separated key map of "id:hexkey" pairs and
// returns a MultiVault.
//
// Current-key selection: the key used for NEW encryptions is
// chosen explicitly by prefixing exactly one entry's id with "*", e.g.
//
//	CREDENTIAL_KEY_MAP="v1:<hex>,*v2:<hex>,v3:<hex>"  → current = v2
//
// This decouples "which key is active" from the textual ordering, so an
// operator can prepend or append rotation keys without silently changing the
// encryption key. If NO entry is marked "*", the LAST entry wins (documented
// fallback, preserves the historical default for single-key and append-only
// maps). Marking more than one entry "*" is a configuration error.
func NewMultiVault(keyMap string, logger *slog.Logger) (*MultiVault, error) {
	// Delegate the "[*]id:hexkey" wire parse + current-key selection (the
	// "*"-strip, single-"*", dup-id, empty-map, last-wins-fallback rules) to the
	// shared leaf keymap.Parse. The credential vault's value rule — exactly 64
	// hex chars, valid hex, not a degenerate/known-weak master — is
	// supplied as the validator so a malformed key reports the same per-id error
	// at parse time. The same leaf parses CREDENTIAL_KEY_MAP on the AI Gateway
	// open side (creddecrypt.NewMultiDecryptor), so both stamp/strip the id
	// identically — the [MUST MATCH] contract.
	entries, current, _, currentExplicit, err := keymap.Parse(keyMap, func(_, hexKey string) error {
		if len(hexKey) != 64 {
			return fmt.Errorf("must be 64 hex chars")
		}
		key, derr := hex.DecodeString(hexKey)
		if derr != nil {
			return fmt.Errorf("invalid hex: %w", derr)
		}
		// Reject a degenerate master before deriving from it.
		if verr := keycheck.ValidateMasterKey(key); verr != nil {
			return fmt.Errorf("rejected: %w", verr)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	mv := &MultiVault{current: current, keys: make(map[string]*Vault, len(entries))}
	for id, hexKey := range entries {
		// hex.DecodeString cannot fail here — the validator already proved the
		// value decodes; re-decode to build the per-key vault.
		key, _ := hex.DecodeString(hexKey)
		vault, verr := newProviderVault(key)
		if verr != nil {
			return nil, fmt.Errorf("key %q: %w", id, verr)
		}
		mv.keys[id] = vault
	}
	logger.Info("multi-key vault loaded", "keyCount", len(mv.keys), "current", mv.current, "currentExplicit", currentExplicit)
	return mv, nil
}

// Encrypt encrypts plaintext using the current key and returns the result
// along with the key ID used. aad binds the ciphertext to its row identity
// (see Vault.Encrypt).
func (mv *MultiVault) Encrypt(plaintext string, aad []byte) (*EncryptResult, string, error) {
	v := mv.keys[mv.current]
	result, err := v.Encrypt(plaintext, aad)
	return result, mv.current, err
}

// Decrypt decrypts ciphertext using the key identified by keyID. aad must match
// the value passed to Encrypt (the row-identity binding).
func (mv *MultiVault) Decrypt(keyID, ciphertextHex, ivHex, tagHex string, aad []byte) (string, error) {
	v, ok := mv.keys[keyID]
	if !ok {
		return "", fmt.Errorf("unknown encryption key ID: %q", keyID)
	}
	return v.Decrypt(ciphertextHex, ivHex, tagHex, aad)
}

// CurrentKeyID returns the ID of the key used for new encryptions.
func (mv *MultiVault) CurrentKeyID() string { return mv.current }

// Package crypto provides AES-256-GCM encryption/decryption compatible with
// the Node.js credential vault format (hex-encoded ciphertext, IV, and tag).
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"golang.org/x/crypto/hkdf"
)

// VaultConfig holds the parameters needed to initialize the credential vault.
type VaultConfig struct {
	EncryptionKey        string
	EncryptionPassphrase string
	EncryptionSalt       string
	Production           bool // true when running in production mode
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

// Vault holds the AES-256-GCM master key and provides encrypt/decrypt.
type Vault struct {
	masterKey []byte
}

// EncryptResult holds hex-encoded ciphertext, IV, and authentication tag.
type EncryptResult struct {
	Ciphertext string
	IV         string
	Tag        string
}

// InitVault initializes the credential vault from the provided config.
// Returns (nil, nil) when encryption is unavailable in non-production mode.
func InitVault(vcfg VaultConfig, logger *slog.Logger) (*Vault, error) {
	keyHex := vcfg.EncryptionKey

	if keyHex == "" {
		// Try HKDF derivation from passphrase
		passphrase := vcfg.EncryptionPassphrase
		if passphrase != "" {
			salt := vcfg.EncryptionSalt
			if salt == "" {
				salt = "nexus-gateway-default-salt"
			}
			derived, err := deriveKey(passphrase, salt)
			if err != nil {
				return nil, fmt.Errorf("derive encryption key: %w", err)
			}
			logger.Info("Credential encryption key derived via HKDF from passphrase")
			return &Vault{masterKey: derived}, nil
		}

		if vcfg.Production {
			return nil, errors.New(
				"CREDENTIAL_ENCRYPTION_KEY or CREDENTIAL_ENCRYPTION_PASSPHRASE is required in production",
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

	logger.Info("Credential encryption key loaded successfully")
	return &Vault{masterKey: key}, nil
}

// NewVault creates a Vault from a raw 32-byte key (for testing).
func NewVault(key []byte) (*Vault, error) {
	if len(key) != 32 {
		return nil, errors.New("key must be exactly 32 bytes")
	}
	dst := make([]byte, 32)
	copy(dst, key)
	return &Vault{masterKey: dst}, nil
}

// Encrypt encrypts plaintext and returns hex-encoded ciphertext, IV, and tag.
func (v *Vault) Encrypt(plaintext string) (*EncryptResult, error) {
	block, err := aes.NewCipher(v.masterKey)
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

	// Seal appends ciphertext+tag
	sealed := aead.Seal(nil, iv, []byte(plaintext), nil)

	// Split: ciphertext is everything except the last tagLength bytes
	ct := sealed[:len(sealed)-tagLength]
	tag := sealed[len(sealed)-tagLength:]

	return &EncryptResult{
		Ciphertext: hex.EncodeToString(ct),
		IV:         hex.EncodeToString(iv),
		Tag:        hex.EncodeToString(tag),
	}, nil
}

// Decrypt decrypts hex-encoded ciphertext using the given IV and tag.
func (v *Vault) Decrypt(ciphertextHex, ivHex, tagHex string) (string, error) {
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

	block, err := aes.NewCipher(v.masterKey)
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

	plaintext, err := aead.Open(nil, iv, sealed, nil)
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
// returns a MultiVault. The last entry in the map becomes the current key
// used for new encryptions.
func NewMultiVault(keyMap string, logger *slog.Logger) (*MultiVault, error) {
	mv := &MultiVault{keys: make(map[string]*Vault)}
	for _, pair := range strings.Split(keyMap, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		parts := strings.SplitN(pair, ":", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid key map entry: %q", pair)
		}
		id, hexKey := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
		if len(hexKey) != 64 {
			return nil, fmt.Errorf("key %q must be 64 hex chars", id)
		}
		key, err := hex.DecodeString(hexKey)
		if err != nil {
			return nil, fmt.Errorf("key %q: invalid hex: %w", id, err)
		}
		mv.keys[id] = &Vault{masterKey: key}
		mv.current = id
	}
	if len(mv.keys) == 0 {
		return nil, errors.New("empty key map")
	}
	logger.Info("multi-key vault loaded", "keyCount", len(mv.keys), "current", mv.current)
	return mv, nil
}

// Encrypt encrypts plaintext using the current key and returns the result
// along with the key ID used.
func (mv *MultiVault) Encrypt(plaintext string) (*EncryptResult, string, error) {
	v := mv.keys[mv.current]
	result, err := v.Encrypt(plaintext)
	return result, mv.current, err
}

// Decrypt decrypts ciphertext using the key identified by keyID.
func (mv *MultiVault) Decrypt(keyID, ciphertextHex, ivHex, tagHex string) (string, error) {
	v, ok := mv.keys[keyID]
	if !ok {
		return "", fmt.Errorf("unknown encryption key ID: %q", keyID)
	}
	return v.Decrypt(ciphertextHex, ivHex, tagHex)
}

// CurrentKeyID returns the ID of the key used for new encryptions.
func (mv *MultiVault) CurrentKeyID() string { return mv.current }

func deriveKey(passphrase, salt string) ([]byte, error) {
	r := hkdf.New(sha256.New, []byte(passphrase), []byte(salt), []byte("nexus-credential-vault"))
	key := make([]byte, 32)
	if _, err := io.ReadFull(r, key); err != nil {
		return nil, err
	}
	return key, nil
}

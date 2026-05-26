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
	"strings"
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

	block, err := aes.NewCipher(key)
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
// This matches Node's AES-256-GCM implementation (separate tag).
func (d *Decryptor) Decrypt(ciphertextHex, ivHex, tagHex string) (string, error) {
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

	plaintext, err := d.gcm.Open(nil, iv, sealed, nil)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrDecryptFailed, err)
	}

	return string(plaintext), nil
}

// MultiDecryptor supports decryption with multiple key versions.
type MultiDecryptor struct {
	keys map[string]*Decryptor
}

// NewMultiDecryptor creates a MultiDecryptor from a comma-separated key map
// in the format "keyID1:hexKey1,keyID2:hexKey2,...".
func NewMultiDecryptor(keyMap string) (*MultiDecryptor, error) {
	md := &MultiDecryptor{keys: make(map[string]*Decryptor)}
	for _, pair := range strings.Split(keyMap, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		parts := strings.SplitN(pair, ":", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("credentials: invalid key entry: %q", pair)
		}
		id := strings.TrimSpace(parts[0])
		hexKey := strings.TrimSpace(parts[1])
		d, err := NewDecryptor(hexKey)
		if err != nil {
			return nil, fmt.Errorf("credentials: key %q: %w", id, err)
		}
		md.keys[id] = d
	}
	if len(md.keys) == 0 {
		return nil, errors.New("credentials: empty key map")
	}
	return md, nil
}

// Decrypt decrypts using the key identified by keyID.
func (md *MultiDecryptor) Decrypt(keyID, ciphertextHex, ivHex, tagHex string) (string, error) {
	d, ok := md.keys[keyID]
	if !ok {
		return "", fmt.Errorf("credentials: unknown key ID: %q", keyID)
	}
	return d.Decrypt(ciphertextHex, ivHex, tagHex)
}

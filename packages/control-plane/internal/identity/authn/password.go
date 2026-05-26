package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"strings"

	"golang.org/x/crypto/scrypt"
)

const (
	scryptSaltLen = 32
	scryptKeyLen  = 64
	scryptN       = 16384
	scryptR       = 8
	scryptP       = 1
)

// randRead and scryptKey are indirected through package-level vars so unit
// tests can exercise the error branches of HashPassword/VerifyPassword without
// monkey-patching the standard library. Defaults point at crypto/rand.Read and
// golang.org/x/crypto/scrypt.Key; production callers MUST NOT reassign these.
var (
	pwRandRead  = rand.Read
	pwScryptKey = scrypt.Key
)

// HashPassword hashes a plaintext password using scrypt.
// Returns "salt_hex:hash_hex" matching the Node.js format.
func HashPassword(password string) (string, error) {
	salt := make([]byte, scryptSaltLen)
	if _, err := pwRandRead(salt); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}
	hash, err := pwScryptKey([]byte(password), salt, scryptN, scryptR, scryptP, scryptKeyLen)
	if err != nil {
		return "", fmt.Errorf("scrypt: %w", err)
	}
	return hex.EncodeToString(salt) + ":" + hex.EncodeToString(hash), nil
}

// VerifyPassword checks a plaintext password against a stored "salt_hex:hash_hex" string.
func VerifyPassword(password, stored string) bool {
	parts := strings.SplitN(stored, ":", 2)
	if len(parts) != 2 {
		return false
	}
	salt, err := hex.DecodeString(parts[0])
	if err != nil {
		return false
	}
	storedHash, err := hex.DecodeString(parts[1])
	if err != nil {
		return false
	}
	derived, err := pwScryptKey([]byte(password), salt, scryptN, scryptR, scryptP, scryptKeyLen)
	if err != nil {
		return false
	}
	if len(derived) != len(storedHash) {
		return false
	}
	return subtle.ConstantTimeCompare(derived, storedHash) == 1
}

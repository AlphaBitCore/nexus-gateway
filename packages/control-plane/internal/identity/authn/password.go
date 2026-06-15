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
	// scryptN is the CPU/memory cost parameter. Set to 2^17 per OWASP Password
	// Storage guidance (scrypt N=2^17, r=8, p=1). MUST stay in lockstep with
	// the seed hasher (tools/db-migrate/seed/lib.ts SCRYPT_OPTIONS.N): the
	// stored "salt:hash" format does not encode N, so a verifier using a
	// different N than the producer can never match.
	scryptN = 1 << 17
	scryptR = 8
	scryptP = 1
	// legacyScryptN is the pre-2026-06 cost parameter. The stored "salt:hash"
	// format does NOT encode N, so a hash produced before the 2^17 bump only
	// verifies under the old N. VerifyPassword falls back to this so a cost
	// increase can never lock out pre-existing password users; HashPassword
	// always writes the current scryptN, so new/changed passwords upgrade.
	legacyScryptN = 1 << 14 // 16384
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
	// Try the current cost first, then the legacy cost. The stored hash omits
	// N, so after a cost bump (16384 -> 2^17, 2026-06) a pre-bump hash only
	// verifies under the old N; the fallback prevents the parameter change from
	// locking out every existing password user. Both branches run on a wrong
	// password, so the timing profile stays uniform for the dummy-hash burns in
	// the login path (idp/local.go).
	for _, n := range [2]int{scryptN, legacyScryptN} {
		derived, err := pwScryptKey([]byte(password), salt, n, scryptR, scryptP, scryptKeyLen)
		if err != nil {
			continue
		}
		if len(derived) == len(storedHash) && subtle.ConstantTimeCompare(derived, storedHash) == 1 {
			return true
		}
	}
	return false
}

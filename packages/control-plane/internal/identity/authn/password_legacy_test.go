package auth

import (
	"encoding/hex"
	"testing"

	"golang.org/x/crypto/scrypt"
)

// TestVerifyPassword_LegacyScryptN guards the 2026-06 prod regression: the scrypt
// cost was bumped 16384 -> 2^17 in BOTH the Go verifier and the seed hasher, but
// the stored "salt:hash" omits N, so every password hashed at the old cost
// (admin@nexus.ai and the other seeded prod users) stopped verifying — a
// fleet-wide lockout for the correct password. VerifyPassword must fall back to
// the legacy cost so those users keep working, while never accepting a wrong
// password and still verifying current-cost hashes.
func TestVerifyPassword_LegacyScryptN(t *testing.T) {
	const pw = "admin123"
	salt := make([]byte, scryptSaltLen)
	for i := range salt {
		salt[i] = byte(i * 7)
	}
	legacy, err := scrypt.Key([]byte(pw), salt, legacyScryptN, scryptR, scryptP, scryptKeyLen)
	if err != nil {
		t.Fatalf("legacy scrypt: %v", err)
	}
	stored := hex.EncodeToString(salt) + ":" + hex.EncodeToString(legacy)

	if !VerifyPassword(pw, stored) {
		t.Fatal("legacy-N (16384) hash failed to verify — existing prod password users would be locked out after the 2^17 bump")
	}
	if VerifyPassword("wrong-password", stored) {
		t.Fatal("legacy-N fallback accepted a wrong password — auth bypass")
	}

	// A current-cost hash must still verify (no regression on fresh/changed passwords).
	cur, err := HashPassword(pw)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if !VerifyPassword(pw, cur) {
		t.Fatal("current-N (2^17) hash failed to verify")
	}
	if VerifyPassword("wrong-password", cur) {
		t.Fatal("current-N path accepted a wrong password — auth bypass")
	}
}

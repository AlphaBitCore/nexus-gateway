package auth

import (
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

// TestVerifyPasswordRejectsMalformedHash covers every rejection branch
// inside VerifyPassword. Each variant MUST return false; a true here would
// constitute an authentication bypass.
func TestVerifyPasswordRejectsMalformedHash(t *testing.T) {
	cases := []struct {
		name   string
		stored string
	}{
		{"empty", ""},
		{"single segment, no colon", "abcdef"},
		{"colon but salt is not hex", "not-hex-salt:00ff"},
		{"colon but hash is not hex", "00ff:not-hex-hash"},
		{"both non-hex", "zz:zz"},
		{"empty salt", ":00ff"},
		{"empty hash", "00ff:"},
		// Right shape but wrong derived-key length — odd-length salt, valid-hex hash that, after derivation,
		// will not match a length-64 derived key because scrypt-derived length is fixed at 64 bytes.
		{"hash too short", hex.EncodeToString(make([]byte, scryptSaltLen)) + ":00"},
		{"hash too long", hex.EncodeToString(make([]byte, scryptSaltLen)) + ":" + strings.Repeat("00", scryptKeyLen+1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if VerifyPassword("any-password", tc.stored) {
				t.Errorf("VerifyPassword accepted malformed stored hash %q — auth bypass", tc.stored)
			}
		})
	}
}

// TestHashPasswordFormat asserts the exact "salt_hex:hash_hex" shape so that
// stored hashes are wire-compatible with the legacy Node.js verifier (and so
// downstream tooling that splits on ':' doesn't break silently).
func TestHashPasswordFormat(t *testing.T) {
	stored, err := HashPassword("pw")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	parts := strings.SplitN(stored, ":", 2)
	if len(parts) != 2 {
		t.Fatalf("stored = %q; want salt:hash with one colon", stored)
	}
	salt, err := hex.DecodeString(parts[0])
	if err != nil {
		t.Fatalf("salt part not hex: %v", err)
	}
	if len(salt) != scryptSaltLen {
		t.Errorf("salt length = %d bytes; want %d", len(salt), scryptSaltLen)
	}
	hash, err := hex.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("hash part not hex: %v", err)
	}
	if len(hash) != scryptKeyLen {
		t.Errorf("hash length = %d bytes; want %d", len(hash), scryptKeyLen)
	}
}

// TestHashPasswordSaltUniqueness — successive hashes of the same password
// must differ because of fresh salt. A regression here would mean two users
// with the same password share the same stored hash (rainbow-table risk).
func TestHashPasswordSaltUniqueness(t *testing.T) {
	const pw = "shared-password"
	a, err := HashPassword(pw)
	if err != nil {
		t.Fatalf("HashPassword a: %v", err)
	}
	b, err := HashPassword(pw)
	if err != nil {
		t.Fatalf("HashPassword b: %v", err)
	}
	if a == b {
		t.Error("two HashPassword calls for the same password produced identical hashes — salt not random")
	}
}

// TestVerifyPasswordCaseSensitive ensures passwords are compared byte-for-byte,
// not case-insensitively (a common implementation bug class).
func TestVerifyPasswordCaseSensitive(t *testing.T) {
	stored, err := HashPassword("CorrectHorseBattery")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if VerifyPassword("correcthorsebattery", stored) {
		t.Error("VerifyPassword treated password as case-insensitive")
	}
	if !VerifyPassword("CorrectHorseBattery", stored) {
		t.Error("VerifyPassword rejected the exact original password")
	}
}

// TestHashPasswordRandError exercises the rand.Read failure branch via the
// pwRandRead indirection. A failure must surface (wrapped); the function must
// NOT silently fall back to an all-zero salt.
func TestHashPasswordRandError(t *testing.T) {
	prev := pwRandRead
	t.Cleanup(func() { pwRandRead = prev })

	sentinel := errors.New("entropy unavailable")
	pwRandRead = func(b []byte) (int, error) { return 0, sentinel }

	got, err := HashPassword("any")
	if err == nil {
		t.Fatal("HashPassword returned nil error on rand failure — risk of all-zero salt")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("error %v does not wrap rand sentinel %v", err, sentinel)
	}
	if got != "" {
		t.Errorf("expected empty hash on error; got %q", got)
	}
}

// TestHashPasswordScryptError exercises the scrypt.Key failure branch. A
// failure must surface (wrapped) rather than emit an empty-hash-but-nil-error.
func TestHashPasswordScryptError(t *testing.T) {
	prev := pwScryptKey
	t.Cleanup(func() { pwScryptKey = prev })

	sentinel := errors.New("scrypt blew up")
	pwScryptKey = func(password, salt []byte, N, r, p, keyLen int) ([]byte, error) {
		return nil, sentinel
	}

	got, err := HashPassword("any")
	if err == nil {
		t.Fatal("HashPassword returned nil error when scrypt failed")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("error %v does not wrap scrypt sentinel %v", err, sentinel)
	}
	if got != "" {
		t.Errorf("expected empty hash on scrypt error; got %q", got)
	}
}

// TestVerifyPasswordScryptError exercises VerifyPassword's scrypt.Key failure
// branch. A scrypt failure during verify must produce false (treat as a
// rejection) — returning true here would be an auth bypass under fault injection.
func TestVerifyPasswordScryptError(t *testing.T) {
	// Build a well-formed stored hash first under the real scrypt.
	stored, err := HashPassword("p")
	if err != nil {
		t.Fatalf("seed HashPassword: %v", err)
	}

	prev := pwScryptKey
	t.Cleanup(func() { pwScryptKey = prev })
	pwScryptKey = func(password, salt []byte, N, r, p, keyLen int) ([]byte, error) {
		return nil, errors.New("scrypt verify failure")
	}

	if VerifyPassword("p", stored) {
		t.Error("VerifyPassword returned true while scrypt.Key was failing — fault-injection auth bypass")
	}
}

// TestVerifyPasswordEmpty — corner case: empty plaintext should hash to a
// real digest and only verify against its own hash, not against arbitrary hashes.
func TestVerifyPasswordEmpty(t *testing.T) {
	stored, err := HashPassword("")
	if err != nil {
		t.Fatalf("HashPassword(\"\"): %v", err)
	}
	if !VerifyPassword("", stored) {
		t.Error("empty password did not verify against its own stored hash")
	}
	if VerifyPassword("not-empty", stored) {
		t.Error("non-empty password verified against empty-password hash")
	}
}

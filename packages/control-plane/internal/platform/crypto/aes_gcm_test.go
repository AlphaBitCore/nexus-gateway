package crypto

import (
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
)

// discardLogger returns a slog.Logger that drops all output, suitable for
// tests where we only care about return values, not log lines.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	return key
}

func TestNewVault_InvalidKeyLength(t *testing.T) {
	_, err := NewVault([]byte("short"))
	if err == nil {
		t.Fatal("expected error for short key")
	}
}

func TestEncryptDecrypt_RoundTrip(t *testing.T) {
	v, err := NewVault(testKey(t))
	if err != nil {
		t.Fatal(err)
	}

	plaintext := "sk-test-api-key-1234567890"
	enc, err := v.Encrypt(plaintext)
	if err != nil {
		t.Fatal("encrypt:", err)
	}

	if enc.Ciphertext == "" || enc.IV == "" || enc.Tag == "" {
		t.Fatal("encrypted result has empty fields")
	}

	// Verify hex encoding
	if _, err := hex.DecodeString(enc.Ciphertext); err != nil {
		t.Fatal("ciphertext not valid hex")
	}
	if _, err := hex.DecodeString(enc.IV); err != nil {
		t.Fatal("IV not valid hex")
	}
	if _, err := hex.DecodeString(enc.Tag); err != nil {
		t.Fatal("tag not valid hex")
	}

	// IV should be 12 bytes = 24 hex chars
	if len(enc.IV) != 24 {
		t.Fatalf("IV length: got %d, want 24 hex chars", len(enc.IV))
	}
	// Tag should be 16 bytes = 32 hex chars
	if len(enc.Tag) != 32 {
		t.Fatalf("tag length: got %d, want 32 hex chars", len(enc.Tag))
	}

	// Decrypt
	got, err := v.Decrypt(enc.Ciphertext, enc.IV, enc.Tag)
	if err != nil {
		t.Fatal("decrypt:", err)
	}
	if got != plaintext {
		t.Fatalf("decrypt: got %q, want %q", got, plaintext)
	}
}

func TestDecrypt_WrongKey(t *testing.T) {
	v1, _ := NewVault(testKey(t))
	v2, _ := NewVault(testKey(t))

	enc, _ := v1.Encrypt("secret")

	_, err := v2.Decrypt(enc.Ciphertext, enc.IV, enc.Tag)
	if err == nil {
		t.Fatal("expected decrypt error with wrong key")
	}
}

func TestDecrypt_TamperedCiphertext(t *testing.T) {
	v, _ := NewVault(testKey(t))
	enc, _ := v.Encrypt("secret")

	// Flip a byte in the ciphertext
	ct, _ := hex.DecodeString(enc.Ciphertext)
	ct[0] ^= 0xff
	tampered := hex.EncodeToString(ct)

	_, err := v.Decrypt(tampered, enc.IV, enc.Tag)
	if err == nil {
		t.Fatal("expected decrypt error with tampered ciphertext")
	}
}

func TestDecrypt_TamperedTag(t *testing.T) {
	v, _ := NewVault(testKey(t))
	enc, _ := v.Encrypt("secret")

	// Flip a byte in the tag
	tag, _ := hex.DecodeString(enc.Tag)
	tag[0] ^= 0xff
	tampered := hex.EncodeToString(tag)

	_, err := v.Decrypt(enc.Ciphertext, enc.IV, tampered)
	if err == nil {
		t.Fatal("expected decrypt error with tampered tag")
	}
}

func TestEncrypt_UniqueIV(t *testing.T) {
	v, _ := NewVault(testKey(t))

	enc1, _ := v.Encrypt("same plaintext")
	enc2, _ := v.Encrypt("same plaintext")

	if enc1.IV == enc2.IV {
		t.Fatal("IVs should be unique across encryptions")
	}
	if enc1.Ciphertext == enc2.Ciphertext {
		t.Fatal("ciphertexts should differ due to different IVs")
	}
}

func TestEncrypt_EmptyString(t *testing.T) {
	v, _ := NewVault(testKey(t))

	enc, err := v.Encrypt("")
	if err != nil {
		t.Fatal("encrypt empty string:", err)
	}

	got, err := v.Decrypt(enc.Ciphertext, enc.IV, enc.Tag)
	if err != nil {
		t.Fatal("decrypt empty string:", err)
	}
	if got != "" {
		t.Fatalf("got %q, want empty string", got)
	}
}

func TestEncrypt_LongPlaintext(t *testing.T) {
	v, _ := NewVault(testKey(t))

	// 4KB plaintext
	long := make([]byte, 4096)
	for i := range long {
		long[i] = byte(i % 256)
	}
	plaintext := string(long)

	enc, err := v.Encrypt(plaintext)
	if err != nil {
		t.Fatal("encrypt long:", err)
	}

	got, err := v.Decrypt(enc.Ciphertext, enc.IV, enc.Tag)
	if err != nil {
		t.Fatal("decrypt long:", err)
	}
	if got != plaintext {
		t.Fatal("long plaintext roundtrip failed")
	}
}

func testHexKey(t *testing.T) string {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(key)
}

func TestNewMultiVault_Basic(t *testing.T) {
	k1 := testHexKey(t)
	k2 := testHexKey(t)
	keyMap := "v1:" + k1 + ",v2:" + k2

	mv, err := NewMultiVault(keyMap, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if mv.CurrentKeyID() != "v2" {
		t.Fatalf("current key: got %q, want %q", mv.CurrentKeyID(), "v2")
	}
}

func TestNewMultiVault_EmptyMap(t *testing.T) {
	_, err := NewMultiVault("", slog.Default())
	if err == nil {
		t.Fatal("expected error for empty key map")
	}
}

func TestNewMultiVault_InvalidEntry(t *testing.T) {
	_, err := NewMultiVault("badentry", slog.Default())
	if err == nil {
		t.Fatal("expected error for invalid entry")
	}
}

func TestNewMultiVault_ShortKey(t *testing.T) {
	_, err := NewMultiVault("v1:abcd", slog.Default())
	if err == nil {
		t.Fatal("expected error for short key")
	}
}

func TestNewMultiVault_InvalidHex(t *testing.T) {
	// 64 chars but not valid hex
	badHex := "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"
	_, err := NewMultiVault("v1:"+badHex, slog.Default())
	if err == nil {
		t.Fatal("expected error for invalid hex")
	}
}

func TestMultiVault_EncryptDecrypt_RoundTrip(t *testing.T) {
	k1 := testHexKey(t)
	k2 := testHexKey(t)
	mv, err := NewMultiVault("v1:"+k1+",v2:"+k2, slog.Default())
	if err != nil {
		t.Fatal(err)
	}

	plaintext := "sk-secret-key-12345"
	enc, keyID, err := mv.Encrypt(plaintext)
	if err != nil {
		t.Fatal("encrypt:", err)
	}
	if keyID != "v2" {
		t.Fatalf("encrypt key ID: got %q, want %q", keyID, "v2")
	}

	got, err := mv.Decrypt(keyID, enc.Ciphertext, enc.IV, enc.Tag)
	if err != nil {
		t.Fatal("decrypt:", err)
	}
	if got != plaintext {
		t.Fatalf("decrypt: got %q, want %q", got, plaintext)
	}
}

func TestMultiVault_DecryptWithOldKey(t *testing.T) {
	k1 := testHexKey(t)
	k2 := testHexKey(t)

	// Create a vault with only v1 to encrypt
	mv1, err := NewMultiVault("v1:"+k1, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	enc, keyID, err := mv1.Encrypt("old-secret")
	if err != nil {
		t.Fatal(err)
	}
	if keyID != "v1" {
		t.Fatalf("expected keyID v1, got %q", keyID)
	}

	// Create a vault with both keys; current is v2
	mv2, err := NewMultiVault("v1:"+k1+",v2:"+k2, slog.Default())
	if err != nil {
		t.Fatal(err)
	}

	// Should decrypt with v1 key
	got, err := mv2.Decrypt("v1", enc.Ciphertext, enc.IV, enc.Tag)
	if err != nil {
		t.Fatal("decrypt with old key:", err)
	}
	if got != "old-secret" {
		t.Fatalf("got %q, want %q", got, "old-secret")
	}
}

func TestMultiVault_DecryptUnknownKeyID(t *testing.T) {
	k1 := testHexKey(t)
	mv, err := NewMultiVault("v1:"+k1, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	_, err = mv.Decrypt("v99", "aa", "bb", "cc")
	if err == nil {
		t.Fatal("expected error for unknown key ID")
	}
}

func TestDeriveKey(t *testing.T) {
	key1, err := deriveKey("my-passphrase", "salt1")
	if err != nil {
		t.Fatal(err)
	}
	if len(key1) != 32 {
		t.Fatalf("derived key length: got %d, want 32", len(key1))
	}

	// Same passphrase + salt = same key
	key2, _ := deriveKey("my-passphrase", "salt1")
	if hex.EncodeToString(key1) != hex.EncodeToString(key2) {
		t.Fatal("same inputs should produce same key")
	}

	// Different salt = different key
	key3, _ := deriveKey("my-passphrase", "salt2")
	if hex.EncodeToString(key1) == hex.EncodeToString(key3) {
		t.Fatal("different salts should produce different keys")
	}
}

// InitVault — exhaustive branch coverage.
//
// Contract:
//   - Explicit 64-hex EncryptionKey wins and decodes to a 32-byte master key.
//   - Non-64-char key string → error before hex decode.
//   - Invalid hex 64-char key → error from hex decode.
//   - Empty key + passphrase → HKDF-derived key (default salt or provided salt).
//   - Empty key + empty passphrase + Production=true → hard error.
//   - Empty key + empty passphrase + Production=false → (nil, nil), allowing
//     the caller to run without credential vault in dev.

func TestInitVault_ExplicitHexKey_Success(t *testing.T) {
	keyHex := testHexKey(t)
	v, err := InitVault(VaultConfig{EncryptionKey: keyHex}, discardLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v == nil {
		t.Fatal("expected non-nil Vault for valid hex key")
		return
	}

	// Round-trip to confirm the decoded master key actually drives AES-GCM.
	enc, err := v.Encrypt("payload")
	if err != nil {
		t.Fatalf("Encrypt failed: %v", err)
	}
	got, err := v.Decrypt(enc.Ciphertext, enc.IV, enc.Tag)
	if err != nil {
		t.Fatalf("Decrypt failed: %v", err)
	}
	if got != "payload" {
		t.Fatalf("round-trip: got %q, want %q", got, "payload")
	}

	// The Vault must hold the exact bytes we passed in.
	want, _ := hex.DecodeString(keyHex)
	if hex.EncodeToString(v.masterKey) != hex.EncodeToString(want) {
		t.Fatal("masterKey bytes do not match decoded hex input")
	}
}

func TestInitVault_HexKey_WrongLength(t *testing.T) {
	_, err := InitVault(VaultConfig{EncryptionKey: "deadbeef"}, discardLogger())
	if err == nil {
		t.Fatal("expected error for 8-char key")
	}
	if !strings.Contains(err.Error(), "64 hex characters") {
		t.Fatalf("error message should mention 64 hex chars, got: %v", err)
	}
}

func TestInitVault_HexKey_InvalidHex(t *testing.T) {
	// 64 chars but not valid hex — passes length check, fails hex decode.
	badHex := strings.Repeat("z", 64)
	_, err := InitVault(VaultConfig{EncryptionKey: badHex}, discardLogger())
	if err == nil {
		t.Fatal("expected error for non-hex 64-char key")
	}
	if !strings.Contains(err.Error(), "not valid hex") {
		t.Fatalf("error should mention invalid hex, got: %v", err)
	}
}

func TestInitVault_Passphrase_DefaultSalt(t *testing.T) {
	v, err := InitVault(VaultConfig{EncryptionPassphrase: "correct-horse-battery-staple"}, discardLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v == nil {
		t.Fatal("expected non-nil Vault for valid passphrase")
		return
	}

	// The default salt is "nexus-gateway-default-salt"; verify by deriving
	// the same key directly and comparing master-key bytes.
	want, derr := deriveKey("correct-horse-battery-staple", "nexus-gateway-default-salt")
	if derr != nil {
		t.Fatal(derr)
	}
	if hex.EncodeToString(v.masterKey) != hex.EncodeToString(want) {
		t.Fatal("masterKey must equal HKDF(passphrase, defaultSalt)")
	}

	// Encrypt/Decrypt round-trip — proves the key is actually usable.
	enc, err := v.Encrypt("hello")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	got, err := v.Decrypt(enc.Ciphertext, enc.IV, enc.Tag)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if got != "hello" {
		t.Fatalf("round-trip: got %q", got)
	}
}

func TestInitVault_Passphrase_CustomSalt(t *testing.T) {
	v, err := InitVault(VaultConfig{
		EncryptionPassphrase: "secret-pass",
		EncryptionSalt:       "my-custom-salt",
	}, discardLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v == nil {
		t.Fatal("expected non-nil Vault")
		return
	}

	// Must equal HKDF(passphrase, customSalt), not the default-salt derivation.
	want, _ := deriveKey("secret-pass", "my-custom-salt")
	if hex.EncodeToString(v.masterKey) != hex.EncodeToString(want) {
		t.Fatal("masterKey should be HKDF(passphrase, customSalt)")
	}
	withDefault, _ := deriveKey("secret-pass", "nexus-gateway-default-salt")
	if hex.EncodeToString(v.masterKey) == hex.EncodeToString(withDefault) {
		t.Fatal("custom salt must yield a different key than the default salt")
	}
}

func TestInitVault_NoKeyNoPassphrase_NonProduction_ReturnsNil(t *testing.T) {
	v, err := InitVault(VaultConfig{Production: false}, discardLogger())
	if err != nil {
		t.Fatalf("non-production with no key should not error, got: %v", err)
	}
	if v != nil {
		t.Fatal("non-production with no key should return nil Vault (vault unavailable)")
	}
}

func TestInitVault_NoKeyNoPassphrase_Production_Errors(t *testing.T) {
	_, err := InitVault(VaultConfig{Production: true}, discardLogger())
	if err == nil {
		t.Fatal("production with no key and no passphrase must error")
	}
	if !strings.Contains(err.Error(), "required in production") {
		t.Fatalf("error should mention production requirement, got: %v", err)
	}
}

// Encrypt / Decrypt — error-path coverage.
//
// These tests reach into the package-private masterKey to construct vaults
// with intentionally invalid key sizes — the only realistic way to exercise
// aes.NewCipher / cipher.NewGCMWithTagSize failure branches without an
// elaborate cipher-injection seam in production code.

// failingReader is a test-only io.Reader that always returns an error.
// It is used to drive Encrypt's IV-generation error branch without changing
// production behavior.
type failingReader struct{ err error }

func (f failingReader) Read(_ []byte) (int, error) { return 0, f.err }

func TestEncrypt_GCMFactoryFailure(t *testing.T) {
	// Swap the package's GCM factory for one that always errors.
	prev := newGCM
	t.Cleanup(func() { newGCM = prev })
	sentinel := errors.New("synthetic gcm failure")
	newGCM = func(_ cipher.Block) (cipher.AEAD, error) { return nil, sentinel }

	v, _ := NewVault(testKey(t))
	_, err := v.Encrypt("payload")
	if err == nil {
		t.Fatal("expected error when GCM factory fails")
	}
	if !strings.Contains(err.Error(), "create GCM") {
		t.Fatalf("error should mention create GCM, got: %v", err)
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("wrapped error should match sentinel, got: %v", err)
	}
}

func TestDecrypt_GCMFactoryFailure(t *testing.T) {
	v, _ := NewVault(testKey(t))
	// Generate a real ciphertext first so the hex-decode branches all pass.
	enc, err := v.Encrypt("payload")
	if err != nil {
		t.Fatal(err)
	}

	// Now swap the GCM factory so Decrypt's GCM step fails.
	prev := newGCM
	t.Cleanup(func() { newGCM = prev })
	sentinel := errors.New("synthetic gcm failure")
	newGCM = func(_ cipher.Block) (cipher.AEAD, error) { return nil, sentinel }

	_, err = v.Decrypt(enc.Ciphertext, enc.IV, enc.Tag)
	if err == nil {
		t.Fatal("expected error when GCM factory fails")
	}
	if !strings.Contains(err.Error(), "create GCM") {
		t.Fatalf("error should mention create GCM, got: %v", err)
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("wrapped error should match sentinel, got: %v", err)
	}
}

func TestEncrypt_IVGenerationFailure(t *testing.T) {
	// Swap the package's entropy source for one that always errors.
	prev := randReader
	t.Cleanup(func() { randReader = prev })
	sentinel := errors.New("synthetic entropy failure")
	randReader = failingReader{err: sentinel}

	v, _ := NewVault(testKey(t))
	_, err := v.Encrypt("payload")
	if err == nil {
		t.Fatal("expected error when entropy source fails")
	}
	if !strings.Contains(err.Error(), "generate IV") {
		t.Fatalf("error should mention generate IV, got: %v", err)
	}
	// Ensure the original sentinel propagates via wrapping (so callers can
	// observe the underlying entropy fault).
	if !errors.Is(err, sentinel) {
		t.Fatalf("wrapped error should match sentinel, got: %v", err)
	}
}

func TestEncrypt_InvalidMasterKeyLength_FailsCipher(t *testing.T) {
	// 17-byte key — AES requires 16/24/32, so NewCipher returns an error.
	v := &Vault{masterKey: make([]byte, 17)}
	_, err := v.Encrypt("payload")
	if err == nil {
		t.Fatal("expected error when masterKey has invalid AES length")
	}
	if !strings.Contains(err.Error(), "create cipher") {
		t.Fatalf("error should mention create cipher, got: %v", err)
	}
}

func TestDecrypt_InvalidMasterKeyLength_FailsCipher(t *testing.T) {
	v := &Vault{masterKey: make([]byte, 17)}
	// Inputs hex-valid AND length-valid (IV=12B, tag=16B) so we reach
	// the cipher init path; the 17-byte master key is what aes.NewCipher
	// rejects.
	_, err := v.Decrypt("aabb", strings.Repeat("01", 12), strings.Repeat("02", 16))
	if err == nil {
		t.Fatal("expected error when masterKey has invalid AES length")
	}
	if !strings.Contains(err.Error(), "create cipher") {
		t.Fatalf("error should mention create cipher, got: %v", err)
	}
}

func TestDecrypt_InvalidCiphertextHex(t *testing.T) {
	v, _ := NewVault(testKey(t))
	_, err := v.Decrypt("zz", "0102030405060708090a0b0c", strings.Repeat("ab", 16))
	if err == nil {
		t.Fatal("expected error for non-hex ciphertext")
	}
	if !strings.Contains(err.Error(), "decode ciphertext") {
		t.Fatalf("error should mention decode ciphertext, got: %v", err)
	}
}

func TestDecrypt_InvalidIVHex(t *testing.T) {
	v, _ := NewVault(testKey(t))
	_, err := v.Decrypt("aabb", "zz", strings.Repeat("ab", 16))
	if err == nil {
		t.Fatal("expected error for non-hex IV")
	}
	if !strings.Contains(err.Error(), "decode IV") {
		t.Fatalf("error should mention decode IV, got: %v", err)
	}
}

func TestDecrypt_InvalidTagHex(t *testing.T) {
	v, _ := NewVault(testKey(t))
	_, err := v.Decrypt("aabb", "0102030405060708090a0b0c", "zz")
	if err == nil {
		t.Fatal("expected error for non-hex tag")
	}
	if !strings.Contains(err.Error(), "decode tag") {
		t.Fatalf("error should mention decode tag, got: %v", err)
	}
}

// TestDecrypt_WrongIVLength verifies that a Credential row whose ivHex
// decodes to a length other than ivLength (12 bytes) returns a clean
// error instead of panicking inside aead.Open. Before the fix this
// path crashed the goroutine with "crypto/cipher: incorrect nonce
// length given to GCM". Source of corrupt IV: any DB row written by
// an out-of-spec process or a hand-edited Credential.
func TestDecrypt_WrongIVLength(t *testing.T) {
	v, _ := NewVault(testKey(t))
	cases := []struct {
		name string
		iv   string // hex
	}{
		{"too short — 6 bytes", "0102030405060708090a0b"}, // 11 hex chars → 5 bytes actually? Force even.
		{"too short — 8 bytes", strings.Repeat("ab", 8)},
		{"too long — 16 bytes", strings.Repeat("cd", 16)},
		{"empty", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := v.Decrypt("aabbccdd", tc.iv, strings.Repeat("ef", 16))
			if err == nil {
				t.Fatalf("expected error for IV of length %d, got nil", len(tc.iv)/2)
			}
			if !strings.Contains(err.Error(), "invalid IV length") {
				t.Fatalf("error should mention 'invalid IV length'; got: %v", err)
			}
		})
	}
}

// TestDecrypt_WrongTagLength verifies that a Credential row whose tagHex
// decodes to a length other than tagLength (16 bytes) returns a clean
// error. Before the fix aead.Open would receive a wrong-length tag
// silently concatenated to ciphertext and surface a misleading "decrypt"
// error; the explicit length check makes the failure reason obvious.
func TestDecrypt_WrongTagLength(t *testing.T) {
	v, _ := NewVault(testKey(t))
	cases := []struct {
		name string
		tag  string // hex
	}{
		{"too short — 8 bytes", strings.Repeat("ab", 8)},
		{"too long — 24 bytes", strings.Repeat("cd", 24)},
		{"empty", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := v.Decrypt("aabbccdd", strings.Repeat("12", 12), tc.tag)
			if err == nil {
				t.Fatalf("expected error for tag of length %d, got nil", len(tc.tag)/2)
			}
			if !strings.Contains(err.Error(), "invalid tag length") {
				t.Fatalf("error should mention 'invalid tag length'; got: %v", err)
			}
		})
	}
}

// MultiVault — additional edge cases not already covered.

// TestNewMultiVault_SkipsBlankEntries verifies that whitespace-only and empty
// comma-separated entries are silently ignored rather than treated as
// malformed pairs. This matches the "tolerate trailing commas in env vars"
// intent of the parser.
func TestNewMultiVault_SkipsBlankEntries(t *testing.T) {
	k1 := testHexKey(t)
	// Note the surrounding whitespace and the doubled comma.
	keyMap := "  ,  ,v1:" + k1 + ",  ,"
	mv, err := NewMultiVault(keyMap, discardLogger())
	if err != nil {
		t.Fatalf("expected blank entries to be skipped, got: %v", err)
	}
	if mv.CurrentKeyID() != "v1" {
		t.Fatalf("current key id: got %q, want v1", mv.CurrentKeyID())
	}
	if len(mv.keys) != 1 {
		t.Fatalf("key count: got %d, want 1", len(mv.keys))
	}
}

// TestNewMultiVault_TrimsWhitespace verifies that id/key whitespace around
// the ":" separator is trimmed — important for keys pasted from env files
// with stray spaces.
func TestNewMultiVault_TrimsWhitespace(t *testing.T) {
	k1 := testHexKey(t)
	mv, err := NewMultiVault("  v1  :  "+k1+"  ", discardLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mv.CurrentKeyID() != "v1" {
		t.Fatalf("current key id should be trimmed to 'v1', got %q", mv.CurrentKeyID())
	}

	// And the key actually works for round-trip.
	enc, _, err := mv.Encrypt("payload")
	if err != nil {
		t.Fatal(err)
	}
	got, err := mv.Decrypt("v1", enc.Ciphertext, enc.IV, enc.Tag)
	if err != nil {
		t.Fatal(err)
	}
	if got != "payload" {
		t.Fatalf("round-trip: got %q", got)
	}
}

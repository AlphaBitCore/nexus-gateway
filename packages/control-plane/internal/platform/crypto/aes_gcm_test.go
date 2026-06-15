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

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/keyderive"
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

// TestEncryptDecrypt_AADBinding_DefeatsCrossCredentialSwap is the SEC-C1-02
// regression: a ciphertext sealed under credential A's row-identity AAD must NOT
// decrypt under credential B's AAD. This is exactly the confused-deputy a DB-write
// attacker tries — copying A's encrypted columns into B's row to make B's identity
// drive A's (higher-privilege) upstream key. With AAD binding the swap fails GCM
// authentication instead of silently yielding A's plaintext.
func TestEncryptDecrypt_AADBinding_DefeatsCrossCredentialSwap(t *testing.T) {
	v, err := NewVault(testKey(t))
	if err != nil {
		t.Fatal(err)
	}
	aadA := keyderive.ProviderCredentialAAD("cred-A", "prov-A")
	aadB := keyderive.ProviderCredentialAAD("cred-B", "prov-B")

	enc, err := v.Encrypt("sk-high-privilege-key", aadA)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	// Correct AAD opens it.
	got, err := v.Decrypt(enc.Ciphertext, enc.IV, enc.Tag, aadA)
	if err != nil || got != "sk-high-privilege-key" {
		t.Fatalf("same-AAD decrypt must succeed: got %q err=%v", got, err)
	}
	// The swap: same ciphertext, a DIFFERENT credential's AAD → must be rejected.
	if _, err := v.Decrypt(enc.Ciphertext, enc.IV, enc.Tag, aadB); err == nil {
		t.Fatal("SEC-C1-02 broken: ciphertext sealed for cred-A decrypted under cred-B's AAD (cross-credential swap not blocked)")
	}
	// A nil-AAD open (the pre-fix behavior) must also fail now.
	if _, err := v.Decrypt(enc.Ciphertext, enc.IV, enc.Tag, nil); err == nil {
		t.Fatal("SEC-C1-02 broken: AAD-bound ciphertext opened with nil AAD")
	}
}

func TestEncryptDecrypt_RoundTrip(t *testing.T) {
	v, err := NewVault(testKey(t))
	if err != nil {
		t.Fatal(err)
	}

	plaintext := "sk-test-api-key-1234567890"
	enc, err := v.Encrypt(plaintext, nil)
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
	got, err := v.Decrypt(enc.Ciphertext, enc.IV, enc.Tag, nil)
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

	enc, _ := v1.Encrypt("secret", nil)

	_, err := v2.Decrypt(enc.Ciphertext, enc.IV, enc.Tag, nil)
	if err == nil {
		t.Fatal("expected decrypt error with wrong key")
	}
}

func TestDecrypt_TamperedCiphertext(t *testing.T) {
	v, _ := NewVault(testKey(t))
	enc, _ := v.Encrypt("secret", nil)

	// Flip a byte in the ciphertext
	ct, _ := hex.DecodeString(enc.Ciphertext)
	ct[0] ^= 0xff
	tampered := hex.EncodeToString(ct)

	_, err := v.Decrypt(tampered, enc.IV, enc.Tag, nil)
	if err == nil {
		t.Fatal("expected decrypt error with tampered ciphertext")
	}
}

func TestDecrypt_TamperedTag(t *testing.T) {
	v, _ := NewVault(testKey(t))
	enc, _ := v.Encrypt("secret", nil)

	// Flip a byte in the tag
	tag, _ := hex.DecodeString(enc.Tag)
	tag[0] ^= 0xff
	tampered := hex.EncodeToString(tag)

	_, err := v.Decrypt(enc.Ciphertext, enc.IV, tampered, nil)
	if err == nil {
		t.Fatal("expected decrypt error with tampered tag")
	}
}

func TestEncrypt_UniqueIV(t *testing.T) {
	v, _ := NewVault(testKey(t))

	enc1, _ := v.Encrypt("same plaintext", nil)
	enc2, _ := v.Encrypt("same plaintext", nil)

	if enc1.IV == enc2.IV {
		t.Fatal("IVs should be unique across encryptions")
	}
	if enc1.Ciphertext == enc2.Ciphertext {
		t.Fatal("ciphertexts should differ due to different IVs")
	}
}

func TestEncrypt_EmptyString(t *testing.T) {
	v, _ := NewVault(testKey(t))

	enc, err := v.Encrypt("", nil)
	if err != nil {
		t.Fatal("encrypt empty string:", err)
	}

	got, err := v.Decrypt(enc.Ciphertext, enc.IV, enc.Tag, nil)
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

	enc, err := v.Encrypt(plaintext, nil)
	if err != nil {
		t.Fatal("encrypt long:", err)
	}

	got, err := v.Decrypt(enc.Ciphertext, enc.IV, enc.Tag, nil)
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

// TestNewMultiVault_ExplicitCurrentMarker is the F-0090 regression: an
// entry prefixed with "*" is the current (encryption) key regardless of
// its position. Here v1 is marked current even though v3 is last, so a
// new encryption must use v1 — proving current selection is no longer
// order-dependent.
func TestNewMultiVault_ExplicitCurrentMarker(t *testing.T) {
	k1, k2, k3 := testHexKey(t), testHexKey(t), testHexKey(t)
	mv, err := NewMultiVault("*v1:"+k1+",v2:"+k2+",v3:"+k3, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if mv.CurrentKeyID() != "v1" {
		t.Fatalf("current key: got %q, want v1 (explicit '*' marker must win over last-entry)", mv.CurrentKeyID())
	}
	_, keyID, err := mv.Encrypt("secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	if keyID != "v1" {
		t.Fatalf("encrypt used key %q, want v1 (marked current)", keyID)
	}
}

// TestNewMultiVault_MarkerWinsOverPosition pins that prepending a new key
// without re-marking does NOT change the current key when an explicit
// marker is present — the operator hazard F-0090 describes.
func TestNewMultiVault_MarkerWinsOverPosition(t *testing.T) {
	k1, k2 := testHexKey(t), testHexKey(t)
	// New key knew prepended; the old key v_old is still marked current.
	mv, err := NewMultiVault("knew:"+k1+",*vold:"+k2, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if mv.CurrentKeyID() != "vold" {
		t.Fatalf("current=%q want vold — prepending knew must not steal current from the marked key", mv.CurrentKeyID())
	}
}

// TestNewMultiVault_NoMarkerLastWins documents the fallback: with no "*"
// marker the last entry remains current (historical default).
func TestNewMultiVault_NoMarkerLastWins(t *testing.T) {
	k1, k2 := testHexKey(t), testHexKey(t)
	mv, err := NewMultiVault("v1:"+k1+",v2:"+k2, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if mv.CurrentKeyID() != "v2" {
		t.Fatalf("current=%q want v2 (last-wins fallback)", mv.CurrentKeyID())
	}
}

// TestNewMultiVault_MultipleMarkersError rejects an ambiguous config with
// more than one "*"-marked entry.
func TestNewMultiVault_MultipleMarkersError(t *testing.T) {
	k1, k2 := testHexKey(t), testHexKey(t)
	_, err := NewMultiVault("*v1:"+k1+",*v2:"+k2, slog.Default())
	if err == nil {
		t.Fatal("expected error for two '*'-marked current keys")
	}
}

// TestNewMultiVault_DuplicateIDError rejects a map with the same id twice
// (would silently overwrite a key otherwise).
func TestNewMultiVault_DuplicateIDError(t *testing.T) {
	k1, k2 := testHexKey(t), testHexKey(t)
	_, err := NewMultiVault("v1:"+k1+",v1:"+k2, slog.Default())
	if err == nil {
		t.Fatal("expected error for duplicate key id")
	}
}

// TestNewMultiVault_EmptyMarkedIDError rejects a bare "*" with no id.
func TestNewMultiVault_EmptyMarkedIDError(t *testing.T) {
	k1 := testHexKey(t)
	_, err := NewMultiVault("*:"+k1, slog.Default())
	if err == nil {
		t.Fatal("expected error for empty key id after '*'")
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
	enc, keyID, err := mv.Encrypt(plaintext, nil)
	if err != nil {
		t.Fatal("encrypt:", err)
	}
	if keyID != "v2" {
		t.Fatalf("encrypt key ID: got %q, want %q", keyID, "v2")
	}

	got, err := mv.Decrypt(keyID, enc.Ciphertext, enc.IV, enc.Tag, nil)
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
	enc, keyID, err := mv1.Encrypt("old-secret", nil)
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
	got, err := mv2.Decrypt("v1", enc.Ciphertext, enc.IV, enc.Tag, nil)
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
	_, err = mv.Decrypt("v99", "aa", "bb", "cc", nil)
	if err == nil {
		t.Fatal("expected error for unknown key ID")
	}
}

// InitVault — exhaustive branch coverage.
//
// Contract:
//   - Explicit 64-hex EncryptionKey wins and decodes to a 32-byte master key.
//   - Non-64-char key string → error before hex decode.
//   - Invalid hex 64-char key → error from hex decode.
//   - Empty key + Production=true → hard error.
//   - Empty key + Production=false → (nil, nil), allowing
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
	enc, err := v.Encrypt("payload", nil)
	if err != nil {
		t.Fatalf("Encrypt failed: %v", err)
	}
	got, err := v.Decrypt(enc.Ciphertext, enc.IV, enc.Tag, nil)
	if err != nil {
		t.Fatalf("Decrypt failed: %v", err)
	}
	if got != "payload" {
		t.Fatalf("round-trip: got %q, want %q", got, "payload")
	}

	// SEC-W2-03: the Vault holds the HKDF-derived provider-credential sub-key,
	// NOT the raw master — derived identically to the ai-gw open side.
	masterBytes, _ := hex.DecodeString(keyHex)
	wantSub, _ := keyderive.DeriveKey32(masterBytes, keyderive.ClassProviderCredential)
	if hex.EncodeToString(v.key) != hex.EncodeToString(wantSub[:]) {
		t.Fatal("Vault must hold the HKDF-derived provider sub-key, not the raw master")
	}
}

// TestInitVault_RejectsWeakKey locks SEC-M2-02: a valid-hex, correct-length but
// degenerate master key (all-zeros / a committed example) must fail closed at
// boot rather than encrypting every credential under a guessable value.
func TestInitVault_RejectsWeakKey(t *testing.T) {
	weak := map[string]string{
		"all-zero":      strings.Repeat("0", 64),
		"committed-dev": "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		"single-repeat": strings.Repeat("ab", 32),
	}
	for name, keyHex := range weak {
		if _, err := InitVault(VaultConfig{EncryptionKey: keyHex}, discardLogger()); err == nil {
			t.Errorf("%s: expected weak-key rejection, got nil", name)
		}
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

func TestInitVault_NoKey_NonProduction_ReturnsNil(t *testing.T) {
	v, err := InitVault(VaultConfig{Production: false}, discardLogger())
	if err != nil {
		t.Fatalf("non-production with no key should not error, got: %v", err)
	}
	if v != nil {
		t.Fatal("non-production with no key should return nil Vault (vault unavailable)")
	}
}

func TestInitVault_NoKey_Production_Errors(t *testing.T) {
	_, err := InitVault(VaultConfig{Production: true}, discardLogger())
	if err == nil {
		t.Fatal("production with no key must error")
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
	_, err := v.Encrypt("payload", nil)
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
	enc, err := v.Encrypt("payload", nil)
	if err != nil {
		t.Fatal(err)
	}

	// Now swap the GCM factory so Decrypt's GCM step fails.
	prev := newGCM
	t.Cleanup(func() { newGCM = prev })
	sentinel := errors.New("synthetic gcm failure")
	newGCM = func(_ cipher.Block) (cipher.AEAD, error) { return nil, sentinel }

	_, err = v.Decrypt(enc.Ciphertext, enc.IV, enc.Tag, nil)
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
	_, err := v.Encrypt("payload", nil)
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
	v := &Vault{key: make([]byte, 17)}
	_, err := v.Encrypt("payload", nil)
	if err == nil {
		t.Fatal("expected error when masterKey has invalid AES length")
	}
	if !strings.Contains(err.Error(), "create cipher") {
		t.Fatalf("error should mention create cipher, got: %v", err)
	}
}

func TestDecrypt_InvalidMasterKeyLength_FailsCipher(t *testing.T) {
	v := &Vault{key: make([]byte, 17)}
	// Inputs hex-valid AND length-valid (IV=12B, tag=16B) so we reach
	// the cipher init path; the 17-byte master key is what aes.NewCipher
	// rejects.
	_, err := v.Decrypt("aabb", strings.Repeat("01", 12), strings.Repeat("02", 16), nil)
	if err == nil {
		t.Fatal("expected error when masterKey has invalid AES length")
	}
	if !strings.Contains(err.Error(), "create cipher") {
		t.Fatalf("error should mention create cipher, got: %v", err)
	}
}

func TestDecrypt_InvalidCiphertextHex(t *testing.T) {
	v, _ := NewVault(testKey(t))
	_, err := v.Decrypt("zz", "0102030405060708090a0b0c", strings.Repeat("ab", 16), nil)
	if err == nil {
		t.Fatal("expected error for non-hex ciphertext")
	}
	if !strings.Contains(err.Error(), "decode ciphertext") {
		t.Fatalf("error should mention decode ciphertext, got: %v", err)
	}
}

func TestDecrypt_InvalidIVHex(t *testing.T) {
	v, _ := NewVault(testKey(t))
	_, err := v.Decrypt("aabb", "zz", strings.Repeat("ab", 16), nil)
	if err == nil {
		t.Fatal("expected error for non-hex IV")
	}
	if !strings.Contains(err.Error(), "decode IV") {
		t.Fatalf("error should mention decode IV, got: %v", err)
	}
}

func TestDecrypt_InvalidTagHex(t *testing.T) {
	v, _ := NewVault(testKey(t))
	_, err := v.Decrypt("aabb", "0102030405060708090a0b0c", "zz", nil)
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
			_, err := v.Decrypt("aabbccdd", tc.iv, strings.Repeat("ef", 16), nil)
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
			_, err := v.Decrypt("aabbccdd", strings.Repeat("12", 12), tc.tag, nil)
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
	enc, _, err := mv.Encrypt("payload", nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := mv.Decrypt("v1", enc.Ciphertext, enc.IV, enc.Tag, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != "payload" {
		t.Fatalf("round-trip: got %q", got)
	}
}

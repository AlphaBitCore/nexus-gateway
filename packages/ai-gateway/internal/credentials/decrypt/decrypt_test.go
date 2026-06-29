package creddecrypt

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/keycheck"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/keyderive"
)

// testEncrypt encrypts plaintext with the given hex MASTER key, returning hex
// ciphertext, iv, tag. It HKDF-derives the provider-credential
// sub-key from the master, mirroring the Decryptor's open side, so the
// round-trip exercises the real derivation rather than the raw master.
func testEncrypt(t *testing.T, keyHex, plaintext string) (string, string, string) {
	t.Helper()
	master, _ := hex.DecodeString(keyHex)
	sub, err := keyderive.DeriveKey32(master, keyderive.ClassProviderCredential)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := aes.NewCipher(sub[:])
	gcm, _ := cipher.NewGCM(block)

	iv := make([]byte, 12)
	if _, err := rand.Read(iv); err != nil {
		t.Fatal(err)
	}

	sealed := gcm.Seal(nil, iv, []byte(plaintext), nil)
	// sealed = ciphertext + tag (last 16 bytes).
	ct := sealed[:len(sealed)-16]
	tag := sealed[len(sealed)-16:]

	return hex.EncodeToString(ct), hex.EncodeToString(iv), hex.EncodeToString(tag)
}

const testKeyHex = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"

func TestDecryptor_RoundTrip(t *testing.T) {
	d, err := NewDecryptor(testKeyHex)
	if err != nil {
		t.Fatal(err)
	}

	plaintext := "sk-openai-secret-key-12345"
	ctHex, ivHex, tagHex := testEncrypt(t, testKeyHex, plaintext)

	got, err := d.Decrypt(ctHex, ivHex, tagHex, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != plaintext {
		t.Errorf("got %q, want %q", got, plaintext)
	}
}

func TestDecryptor_WrongKey(t *testing.T) {
	d, err := NewDecryptor(testKeyHex)
	if err != nil {
		t.Fatal(err)
	}

	// Encrypt with a different key.
	otherKey := "202122232425262728292a2b2c2d2e2f303132333435363738393a3b3c3d3e3f"
	ctHex, ivHex, tagHex := testEncrypt(t, otherKey, "secret")

	_, err = d.Decrypt(ctHex, ivHex, tagHex, nil)
	if !errors.Is(err, ErrDecryptFailed) {
		t.Errorf("expected ErrDecryptFailed, got %v", err)
	}
}

func TestDecryptor_InvalidKeyLength(t *testing.T) {
	_, err := NewDecryptor("0123456789abcdef") // 8 bytes, not 32
	if err == nil {
		t.Fatal("expected error for short key")
	}
}

func TestDecryptor_InvalidHex(t *testing.T) {
	d, err := NewDecryptor(testKeyHex)
	if err != nil {
		t.Fatal(err)
	}

	// Bad ciphertext hex.
	_, err = d.Decrypt("not-hex", "000000000000000000000000", "00000000000000000000000000000000", nil)
	if err == nil {
		t.Fatal("expected error for invalid ciphertext hex")
	}
}

func TestDecryptor_InvalidIVHex(t *testing.T) {
	d, err := NewDecryptor(testKeyHex)
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.Decrypt("aabb", "not-hex", "00000000000000000000000000000000", nil)
	if err == nil {
		t.Fatal("expected error for invalid IV hex")
	}
	if !strings.Contains(err.Error(), "invalid iv hex") {
		t.Errorf("expected wrap mentioning iv hex, got %v", err)
	}
}

func TestDecryptor_InvalidTagHex(t *testing.T) {
	d, err := NewDecryptor(testKeyHex)
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.Decrypt("aabb", "000000000000000000000000", "not-hex", nil)
	if err == nil {
		t.Fatal("expected error for invalid tag hex")
	}
	if !strings.Contains(err.Error(), "invalid tag hex") {
		t.Errorf("expected wrap mentioning tag hex, got %v", err)
	}
}

// TestNewDecryptor_RejectsWeakKey locks the weak-key guard on the AI Gateway open side:
// a valid-hex, correct-length but degenerate master (all-zeros / a committed
// example / a single repeated byte) must fail closed at construction — symmetric
// with the Control Plane minting side's keycheck.ValidateMasterKey gate. A
// gateway that silently accepted a guessable master would decrypt every
// credential under it.
func TestNewDecryptor_RejectsWeakKey(t *testing.T) {
	weak := map[string]string{
		"all-zero":      strings.Repeat("0", 64),
		"committed-dev": "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		"single-repeat": strings.Repeat("ab", 32),
	}
	for name, keyHex := range weak {
		if _, err := NewDecryptor(keyHex); err == nil {
			t.Errorf("%s: expected weak-key rejection, got nil", name)
		}
	}
}

func TestNewDecryptor_InvalidHex(t *testing.T) {
	_, err := NewDecryptor("not-hex-at-all-zz") // non-hex characters
	if err == nil {
		t.Fatal("expected error for non-hex key")
	}
	if !strings.Contains(err.Error(), "invalid hex key") {
		t.Errorf("expected 'invalid hex key' in error, got %v", err)
	}
}

func TestDecryptor_NilReceiver(t *testing.T) {
	var d *Decryptor
	_, err := d.Decrypt("aa", "bb", "cc", nil)
	if !errors.Is(err, ErrKeyNotInitialized) {
		t.Errorf("expected ErrKeyNotInitialized, got %v", err)
	}
}

func TestDecryptor_WrongIVLength(t *testing.T) {
	d, err := NewDecryptor(testKeyHex)
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.Decrypt("aabb", "aabb", "00000000000000000000000000000000", nil)
	if err == nil {
		t.Fatal("expected error for wrong IV length")
	}
}

func TestDecryptor_WrongTagLength(t *testing.T) {
	d, err := NewDecryptor(testKeyHex)
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.Decrypt("aabb", "000000000000000000000000", "aabb", nil)
	if err == nil {
		t.Fatal("expected error for wrong tag length")
	}
}

func TestNewDecryptor_RejectsDegenerateKey(t *testing.T) {
	// A correctly sized (32-byte) but degenerate master key — a single repeated
	// byte, never the product of a random generator — must be refused at
	// construction so the service fails closed at boot instead of protecting
	// credentials at rest with a publicly guessable constant.
	cases := []struct {
		name   string
		keyHex string
	}{
		{"single repeated byte", strings.Repeat("aa", 32)},
		{"all zeros", strings.Repeat("00", 32)},
		{"tiny byte set", strings.Repeat("0123", 16)}, // only 4 distinct bytes
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, err := NewDecryptor(tc.keyHex)
			if err == nil {
				t.Fatal("expected degenerate master key to be rejected")
			}
			if !errors.Is(err, keycheck.ErrWeakMasterKey) {
				t.Errorf("expected ErrWeakMasterKey, got %v", err)
			}
			if d != nil {
				t.Error("expected nil decryptor when the key is rejected")
			}
		})
	}
}

func TestNewDecryptor_ValidKey(t *testing.T) {
	d, err := NewDecryptor(testKeyHex)
	if err != nil {
		t.Fatal(err)
	}
	if d == nil {
		t.Error("expected non-nil decryptor for valid key")
	}
}

// --- MultiDecryptor tests ---

const testKeyHex2 = "202122232425262728292a2b2c2d2e2f303132333435363738393a3b3c3d3e3f"

func TestMultiDecryptor_RoundTrip(t *testing.T) {
	keyMap := "v1:" + testKeyHex + ",v2:" + testKeyHex2
	md, err := NewMultiDecryptor(keyMap)
	if err != nil {
		t.Fatal(err)
	}

	// Encrypt with key v1.
	plain1 := "secret-for-v1"
	ct1, iv1, tag1 := testEncrypt(t, testKeyHex, plain1)
	got, err := md.Decrypt("v1", ct1, iv1, tag1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != plain1 {
		t.Errorf("v1: got %q, want %q", got, plain1)
	}

	// Encrypt with key v2.
	plain2 := "secret-for-v2"
	ct2, iv2, tag2 := testEncrypt(t, testKeyHex2, plain2)
	got, err = md.Decrypt("v2", ct2, iv2, tag2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != plain2 {
		t.Errorf("v2: got %q, want %q", got, plain2)
	}
}

// TestMultiDecryptor_StarMarkerStripped is the regression: when an
// operator uses the documented "*vN:" current-marker syntax in CREDENTIAL_KEY_MAP
// (recommended in .env.example), the gateway must store that key under the
// STRIPPED id "vN" — the same id the Control Plane stamps onto ciphertext — not
// under the literal "*vN". Before the fix this side did not strip the "*", so a
// row stamped "v2" missed the lookup and EVERY credential decrypt failed with
// "unknown key ID". Here we seal under the v2 master (mirroring the CP seal) and
// require Decrypt("v2", ...) to succeed; we also assert the literal "*v2" lookup
// fails, proving the marker is not retained in the stored id.
func TestMultiDecryptor_StarMarkerStripped(t *testing.T) {
	keyMap := "v1:" + testKeyHex + ",*v2:" + testKeyHex2
	md, err := NewMultiDecryptor(keyMap)
	if err != nil {
		t.Fatal(err)
	}

	plain := "sk-current-version-secret"
	ct, iv, tag := testEncrypt(t, testKeyHex2, plain) // sealed under v2's master

	// The CP stamps the ciphertext key id "v2" (the stripped, "*"-marked id).
	got, err := md.Decrypt("v2", ct, iv, tag, nil)
	if err != nil {
		t.Fatalf("F-0390 regression: decrypt under stamped id \"v2\" failed: %v", err)
	}
	if got != plain {
		t.Fatalf("got %q, want %q", got, plain)
	}

	// The "*" must NOT survive into the stored id.
	if _, err := md.Decrypt("*v2", ct, iv, tag, nil); err == nil {
		t.Fatal("F-0390 regression: gateway still keys under literal \"*v2\" (marker not stripped)")
	}
}

func TestMultiDecryptor_UnknownKeyID(t *testing.T) {
	keyMap := "v1:" + testKeyHex
	md, err := NewMultiDecryptor(keyMap)
	if err != nil {
		t.Fatal(err)
	}
	ct, iv, tag := testEncrypt(t, testKeyHex, "secret")
	_, err = md.Decrypt("v99", ct, iv, tag, nil)
	if err == nil {
		t.Fatal("expected error for unknown key ID")
	}
}

func TestMultiDecryptor_EmptyKeyMap(t *testing.T) {
	_, err := NewMultiDecryptor("")
	if err == nil {
		t.Fatal("expected error for empty key map")
	}
}

func TestMultiDecryptor_InvalidEntry(t *testing.T) {
	_, err := NewMultiDecryptor("bad-entry-no-colon")
	if err == nil {
		t.Fatal("expected error for invalid entry format")
	}
}

func TestMultiDecryptor_InvalidKey(t *testing.T) {
	_, err := NewMultiDecryptor("v1:tooshort")
	if err == nil {
		t.Fatal("expected error for invalid key hex")
	}
}

func TestMultiDecryptor_WhitespaceHandling(t *testing.T) {
	keyMap := " v1 : " + testKeyHex + " , v2 : " + testKeyHex2 + " "
	md, err := NewMultiDecryptor(keyMap)
	if err != nil {
		t.Fatal(err)
	}
	plain := "test-whitespace"
	ct, iv, tag := testEncrypt(t, testKeyHex, plain)
	got, err := md.Decrypt("v1", ct, iv, tag, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != plain {
		t.Errorf("got %q, want %q", got, plain)
	}
}

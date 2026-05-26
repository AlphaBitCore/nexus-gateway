package creddecrypt

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

// testEncrypt encrypts plaintext with the given hex key, returning hex ciphertext, iv, tag.
func testEncrypt(t *testing.T, keyHex, plaintext string) (string, string, string) {
	t.Helper()
	key, _ := hex.DecodeString(keyHex)
	block, _ := aes.NewCipher(key)
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

const testKeyHex = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestDecryptor_RoundTrip(t *testing.T) {
	d, err := NewDecryptor(testKeyHex)
	if err != nil {
		t.Fatal(err)
	}

	plaintext := "sk-openai-secret-key-12345"
	ctHex, ivHex, tagHex := testEncrypt(t, testKeyHex, plaintext)

	got, err := d.Decrypt(ctHex, ivHex, tagHex)
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
	otherKey := "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	ctHex, ivHex, tagHex := testEncrypt(t, otherKey, "secret")

	_, err = d.Decrypt(ctHex, ivHex, tagHex)
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
	_, err = d.Decrypt("not-hex", "000000000000000000000000", "00000000000000000000000000000000")
	if err == nil {
		t.Fatal("expected error for invalid ciphertext hex")
	}
}

func TestDecryptor_InvalidIVHex(t *testing.T) {
	d, err := NewDecryptor(testKeyHex)
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.Decrypt("aabb", "not-hex", "00000000000000000000000000000000")
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
	_, err = d.Decrypt("aabb", "000000000000000000000000", "not-hex")
	if err == nil {
		t.Fatal("expected error for invalid tag hex")
	}
	if !strings.Contains(err.Error(), "invalid tag hex") {
		t.Errorf("expected wrap mentioning tag hex, got %v", err)
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
	_, err := d.Decrypt("aa", "bb", "cc")
	if !errors.Is(err, ErrKeyNotInitialized) {
		t.Errorf("expected ErrKeyNotInitialized, got %v", err)
	}
}

func TestDecryptor_WrongIVLength(t *testing.T) {
	d, err := NewDecryptor(testKeyHex)
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.Decrypt("aabb", "aabb", "00000000000000000000000000000000")
	if err == nil {
		t.Fatal("expected error for wrong IV length")
	}
}

func TestDecryptor_WrongTagLength(t *testing.T) {
	d, err := NewDecryptor(testKeyHex)
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.Decrypt("aabb", "000000000000000000000000", "aabb")
	if err == nil {
		t.Fatal("expected error for wrong tag length")
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

const testKeyHex2 = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"

func TestMultiDecryptor_RoundTrip(t *testing.T) {
	keyMap := "v1:" + testKeyHex + ",v2:" + testKeyHex2
	md, err := NewMultiDecryptor(keyMap)
	if err != nil {
		t.Fatal(err)
	}

	// Encrypt with key v1.
	plain1 := "secret-for-v1"
	ct1, iv1, tag1 := testEncrypt(t, testKeyHex, plain1)
	got, err := md.Decrypt("v1", ct1, iv1, tag1)
	if err != nil {
		t.Fatal(err)
	}
	if got != plain1 {
		t.Errorf("v1: got %q, want %q", got, plain1)
	}

	// Encrypt with key v2.
	plain2 := "secret-for-v2"
	ct2, iv2, tag2 := testEncrypt(t, testKeyHex2, plain2)
	got, err = md.Decrypt("v2", ct2, iv2, tag2)
	if err != nil {
		t.Fatal(err)
	}
	if got != plain2 {
		t.Errorf("v2: got %q, want %q", got, plain2)
	}
}

func TestMultiDecryptor_UnknownKeyID(t *testing.T) {
	keyMap := "v1:" + testKeyHex
	md, err := NewMultiDecryptor(keyMap)
	if err != nil {
		t.Fatal(err)
	}
	ct, iv, tag := testEncrypt(t, testKeyHex, "secret")
	_, err = md.Decrypt("v99", ct, iv, tag)
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
	got, err := md.Decrypt("v1", ct, iv, tag)
	if err != nil {
		t.Fatal(err)
	}
	if got != plain {
		t.Errorf("got %q, want %q", got, plain)
	}
}

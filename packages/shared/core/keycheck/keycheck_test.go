package keycheck

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"testing"
)

func TestValidateMasterKey_RejectsDegenerate(t *testing.T) {
	cases := map[string][]byte{
		"empty":             {},
		"all-zero":          make([]byte, 32),
		"single-repeat":     bytes.Repeat([]byte{0xAB}, 32),
		"committed-dev-key": mustHex(t, "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"), // 8 distinct
	}
	for name, key := range cases {
		if err := ValidateMasterKey(key); err == nil {
			t.Errorf("%s: expected rejection, got nil", name)
		} else if !errors.Is(err, ErrWeakMasterKey) {
			t.Errorf("%s: wrong error: %v", name, err)
		}
	}
}

func TestValidateMasterKey_AcceptsRandom(t *testing.T) {
	for range 100 {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			t.Fatal(err)
		}
		if err := ValidateMasterKey(key); err != nil {
			t.Fatalf("random key rejected: %v (key=%x)", err, key)
		}
	}
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

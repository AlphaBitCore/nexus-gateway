package spillupload

import (
	"errors"
	"testing"
	"time"
)

// helperStore returns a SecretStore with two epochs so tests can
// exercise both the happy path (active kid signs+verifies) and the
// rotation path (an older kid still verifies until expired).
func helperStore(t *testing.T) *SecretStore {
	t.Helper()
	return NewInMemorySecretStore("epoch-2", map[string][]byte{
		"epoch-1": []byte("a-very-old-secret-not-used-anymore"),
		"epoch-2": []byte("the-current-active-signing-secret"),
	})
}

func validClaims() Claims {
	return Claims{
		EventID:   "8e9f3c1a-7d4b-4a8c-92ab-3a8f0a7b2c11",
		Direction: DirectionRequest,
		Key:       "agent/2026-05-06/8e9f3c1a-request",
		SizeBytes: 1024,
		SHA256:    "5b4e0c8a3e2f1c7d0a6b9d5f4e3c2b1a0d9e8f7c6b5a4e3d2c1b0a9f8e7d6c5b",
		Backend:   "localfs",
		Mime:      "application/json",
	}
}

func TestSign_VerifyHappyPath(t *testing.T) {
	store := helperStore(t)
	tok, _, err := Sign(store, validClaims(), MaxTTL)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	got, err := Verify(store, tok, time.Now())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.EventID != "8e9f3c1a-7d4b-4a8c-92ab-3a8f0a7b2c11" || got.SizeBytes != 1024 {
		t.Errorf("claims roundtrip mismatch: %+v", got)
	}
	if got.KID != "epoch-2" {
		t.Errorf("KID: want epoch-2, got %q", got.KID)
	}
}

func TestSign_TTLClampedToMaxTTL(t *testing.T) {
	store := helperStore(t)
	before := time.Now()
	tok, _, err := Sign(store, validClaims(), 24*time.Hour)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	got, err := Verify(store, tok, time.Now())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if delta := got.ExpiresAt - before.Unix(); delta > int64(MaxTTL.Seconds())+2 {
		t.Errorf("expiry not clamped to MaxTTL: delta=%ds", delta)
	}
}

func TestVerify_RejectsExpired(t *testing.T) {
	store := helperStore(t)
	tok, _, err := Sign(store, validClaims(), 1*time.Second)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	_, err = Verify(store, tok, time.Now().Add(2*time.Second))
	if !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("want ErrTokenExpired, got %v", err)
	}
}

func TestVerify_RejectsTamperedPayload(t *testing.T) {
	store := helperStore(t)
	tok, _, err := Sign(store, validClaims(), MaxTTL)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	// Mutate one byte in the payload portion.
	mut := []byte(tok)
	mut[5] ^= 0x01
	_, err = Verify(store, string(mut), time.Now())
	if !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("want ErrTokenInvalid, got %v", err)
	}
}

func TestVerify_RejectsTamperedSignature(t *testing.T) {
	store := helperStore(t)
	tok, _, err := Sign(store, validClaims(), MaxTTL)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	mut := []byte(tok)
	// Mutate a character well inside the signature region. Flipping
	// a bit on the last RawURLEncoding character can land on "spare"
	// bits that don't affect the decoded HMAC; pick a position 10
	// characters from the end so we always sit on a fully-decoded byte.
	idx := len(mut) - 10
	if mut[idx] == 'a' {
		mut[idx] = 'b'
	} else {
		mut[idx] = 'a'
	}
	_, err = Verify(store, string(mut), time.Now())
	if !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("want ErrTokenInvalid, got %v", err)
	}
}

func TestVerify_RejectsUnknownKID(t *testing.T) {
	signer := NewInMemorySecretStore("epoch-2", map[string][]byte{
		"epoch-2": []byte("the-current-active-signing-secret"),
	})
	tok, _, err := Sign(signer, validClaims(), MaxTTL)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	verifier := NewInMemorySecretStore("epoch-3", map[string][]byte{
		// epoch-2 (the kid baked into the token) is gone — rotation has
		// progressed past the issuance window.
		"epoch-3": []byte("fresh-secret-after-rotation-cleanup"),
	})
	_, err = Verify(verifier, tok, time.Now())
	if !errors.Is(err, ErrUnknownKID) {
		t.Fatalf("want ErrUnknownKID, got %v", err)
	}
}

func TestSign_RejectsBadClaims(t *testing.T) {
	store := helperStore(t)
	cases := []struct {
		name    string
		mutate  func(*Claims)
		wantSub string
	}{
		{"missing eventId", func(c *Claims) { c.EventID = "" }, "eventId"},
		{"bad direction", func(c *Claims) { c.Direction = "sideways" }, "direction"},
		{"missing key", func(c *Claims) { c.Key = "" }, "key"},
		{"zero size", func(c *Claims) { c.SizeBytes = 0 }, "sizeBytes"},
		{"short sha256", func(c *Claims) { c.SHA256 = "deadbeef" }, "sha256"},
		{"upper-case sha256", func(c *Claims) { c.SHA256 = "ABCDEFABCDEFABCDEFABCDEFABCDEFABCDEFABCDEFABCDEFABCDEFABCDEFABCD" }, "sha256"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validClaims()
			tc.mutate(&c)
			_, _, err := Sign(store, c, MaxTTL)
			if !errors.Is(err, ErrTokenInvalid) {
				t.Errorf("want ErrTokenInvalid; got %v", err)
			}
		})
	}
}

func TestDedupKey_DeterministicForSameToken(t *testing.T) {
	store := helperStore(t)
	tok, _, err := Sign(store, validClaims(), MaxTTL)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	a := DedupKey(tok)
	b := DedupKey(tok)
	if a != b {
		t.Error("DedupKey must be deterministic for the same input")
	}
}

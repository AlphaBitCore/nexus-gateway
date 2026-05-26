package auth

import "testing"

func TestHashAndVerifyPassword(t *testing.T) {
	password := "test-password-123"
	hashed, err := HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}

	if !VerifyPassword(password, hashed) {
		t.Error("VerifyPassword returned false for correct password")
	}
	if VerifyPassword("wrong-password", hashed) {
		t.Error("VerifyPassword returned true for wrong password")
	}
	if VerifyPassword(password, "invalid-format") {
		t.Error("VerifyPassword returned true for malformed hash")
	}
}

func TestHashAPIKey(t *testing.T) {
	key := "nxk_test1234567890abcdef"
	hash1 := HashAPIKey(key)
	hash2 := HashAPIKey(key)
	if hash1 != hash2 {
		t.Error("HashAPIKey not deterministic")
	}
	if hash1 == "" {
		t.Error("HashAPIKey returned empty")
	}
	// Different key → different hash
	other := HashAPIKey("nxk_different")
	if hash1 == other {
		t.Error("Different keys produced same hash")
	}
}

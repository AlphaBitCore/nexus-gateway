package wiring

import (
	"testing"
)

// TestInitVKAuth_nonEmptySecret verifies that a non-empty HMAC secret
// succeeds and returns a non-nil Authenticator.
func TestInitVKAuth_nonEmptySecret(t *testing.T) {
	auth, err := InitVKAuth(nil, "some-secret", discardLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if auth == nil {
		t.Fatal("expected non-nil Authenticator")
	}
}

// TestInitVKAuth_emptySecretOutsideProduction verifies that an empty secret
// is accepted in non-production environment (no NODE_ENV=production set
// in test environment).
func TestInitVKAuth_emptySecretOutsideProduction(t *testing.T) {
	auth, err := InitVKAuth(nil, "", discardLogger())
	if err != nil {
		t.Fatalf("unexpected error outside production: %v", err)
	}
	if auth == nil {
		t.Fatal("expected non-nil Authenticator for empty secret in dev mode")
	}
}

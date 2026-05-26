package oauth_test

import (
	"crypto/sha256"
	"encoding/base64"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/oauth"
)

func TestVerifyPKCE_S256_Match(t *testing.T) {
	verifier := "abcdefghijklmnopqrstuvwxyz0123456789abcdefghij"
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	if err := oauth.VerifyPKCE(verifier, challenge, "S256"); err != nil {
		t.Fatalf("expected match, got %v", err)
	}
}

func TestVerifyPKCE_Mismatch(t *testing.T) {
	if err := oauth.VerifyPKCE("wrong", "abc", "S256"); err == nil {
		t.Fatal("expected error")
	}
}

func TestVerifyPKCE_RejectsPlain(t *testing.T) {
	if err := oauth.VerifyPKCE("x", "x", "plain"); err == nil {
		t.Fatal("plain method must be rejected")
	}
}

func TestVerifyPKCE_RejectsShortVerifier(t *testing.T) {
	if err := oauth.VerifyPKCE("short", "x", "S256"); err == nil {
		t.Fatal("RFC 7636 requires verifier length 43..128")
	}
}

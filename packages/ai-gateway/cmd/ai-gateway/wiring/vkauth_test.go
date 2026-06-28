package wiring

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/config"
)

// cfgWithHMAC builds a minimal Config carrying the given single HMAC secret.
func cfgWithHMAC(secret string) *config.Config {
	return &config.Config{Auth: config.AuthConfig{HMACSecret: secret}}
}

// TestInitVKAuth_nonEmptySecret verifies that a non-empty HMAC secret builds the
// (single-version) keyring and returns a non-nil Authenticator.
func TestInitVKAuth_nonEmptySecret(t *testing.T) {
	auth, err := InitVKAuth(nil, cfgWithHMAC("some-secret"), discardLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if auth == nil {
		t.Fatal("expected non-nil Authenticator")
	}
}

// TestInitVKAuth_KeyMap verifies the versioned-keyring path: an
// ADMIN_KEY_HMAC_KEY_MAP value builds a multi-version keyring and returns a
// non-nil Authenticator.
func TestInitVKAuth_KeyMap(t *testing.T) {
	cfg := &config.Config{Auth: config.AuthConfig{HMACKeyMap: "v1:old,*v2:current"}}
	auth, err := InitVKAuth(nil, cfg, discardLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if auth == nil {
		t.Fatal("expected non-nil Authenticator")
	}
}

// TestInitVKAuth_EmptyConfigErrors is the defense-in-depth check: with NEITHER a
// single secret NOR a keymap, the keyring build fails closed. config.validate()
// already guarantees at least one is set by the time wiring runs, so this never
// fires in production — but InitVKAuth must surface the error rather than build a
// keyring over an empty secret.
func TestInitVKAuth_EmptyConfigErrors(t *testing.T) {
	if _, err := InitVKAuth(nil, &config.Config{}, discardLogger()); err == nil {
		t.Fatal("expected an error building a keyring from an all-empty HMAC config")
	}
}

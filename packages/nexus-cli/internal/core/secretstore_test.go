package core

import (
	"errors"
	"testing"

	"github.com/zalando/go-keyring"
)

// TestKeyringStore_RoundTrip exercises the real KeyringStore against
// go-keyring's in-memory mock provider (no OS keychain touched).
func TestKeyringStore_RoundTrip(t *testing.T) {
	keyring.MockInit()
	var s KeyringStore

	if err := s.Set("local", SecretAccessToken, "tok-123"); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, err := s.Get("local", SecretAccessToken)
	if err != nil || got != "tok-123" {
		t.Fatalf("get after set: got %q err=%v", got, err)
	}
}

func TestKeyringStore_GetMissing(t *testing.T) {
	keyring.MockInit()
	var s KeyringStore
	_, err := s.Get("local", SecretRefreshToken)
	if !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("missing secret: want ErrSecretNotFound, got %v", err)
	}
}

func TestKeyringStore_Delete(t *testing.T) {
	keyring.MockInit()
	var s KeyringStore
	_ = s.Set("local", SecretAdminKey, "nxk_abc")
	if err := s.Delete("local", SecretAdminKey); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.Get("local", SecretAdminKey); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("after delete: want ErrSecretNotFound, got %v", err)
	}
	// Deleting an absent secret is a no-op.
	if err := s.Delete("local", SecretAdminKey); err != nil {
		t.Fatalf("delete absent should be no-op, got %v", err)
	}
}

func TestKeyringStore_EnvIsolation(t *testing.T) {
	keyring.MockInit()
	var s KeyringStore
	_ = s.Set("local", SecretAccessToken, "local-tok")
	_ = s.Set("prod", SecretAccessToken, "prod-tok")
	gotLocal, _ := s.Get("local", SecretAccessToken)
	gotProd, _ := s.Get("prod", SecretAccessToken)
	if gotLocal != "local-tok" || gotProd != "prod-tok" {
		t.Fatalf("env isolation broken: local=%q prod=%q", gotLocal, gotProd)
	}
}

func TestAccountComposition(t *testing.T) {
	if account("prod", SecretVKSecret) != "prod:vk_secret" {
		t.Fatalf("account composition wrong: %q", account("prod", SecretVKSecret))
	}
}

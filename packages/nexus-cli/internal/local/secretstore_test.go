package local

import (
	"errors"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/core"
)

// TestKeyringStore_RoundTrip exercises the real KeyringStore against
// go-keyring's in-memory mock provider (no OS keychain touched).
func TestKeyringStore_RoundTrip(t *testing.T) {
	keyring.MockInit()
	var s KeyringStore

	if err := s.Set("local", core.SecretAccessToken, "tok-123"); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, err := s.Get("local", core.SecretAccessToken)
	if err != nil || got != "tok-123" {
		t.Fatalf("get after set: got %q err=%v", got, err)
	}
}

func TestKeyringStore_GetMissing(t *testing.T) {
	keyring.MockInit()
	var s KeyringStore
	_, err := s.Get("local", core.SecretRefreshToken)
	if !errors.Is(err, core.ErrSecretNotFound) {
		t.Fatalf("missing secret: want ErrSecretNotFound, got %v", err)
	}
}

func TestKeyringStore_Delete(t *testing.T) {
	keyring.MockInit()
	var s KeyringStore
	_ = s.Set("local", core.SecretAdminKey, "nxk_abc")
	if err := s.Delete("local", core.SecretAdminKey); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.Get("local", core.SecretAdminKey); !errors.Is(err, core.ErrSecretNotFound) {
		t.Fatalf("after delete: want ErrSecretNotFound, got %v", err)
	}
	// Deleting an absent secret is a no-op.
	if err := s.Delete("local", core.SecretAdminKey); err != nil {
		t.Fatalf("delete absent should be no-op, got %v", err)
	}
}

func TestKeyringStore_EnvIsolation(t *testing.T) {
	keyring.MockInit()
	var s KeyringStore
	_ = s.Set("local", core.SecretAccessToken, "local-tok")
	_ = s.Set("prod", core.SecretAccessToken, "prod-tok")
	gotLocal, _ := s.Get("local", core.SecretAccessToken)
	gotProd, _ := s.Get("prod", core.SecretAccessToken)
	if gotLocal != "local-tok" || gotProd != "prod-tok" {
		t.Fatalf("env isolation broken: local=%q prod=%q", gotLocal, gotProd)
	}
}

func TestAccountComposition(t *testing.T) {
	if account("prod", core.SecretVKSecret) != "prod:vk_secret" {
		t.Fatalf("account composition wrong: %q", account("prod", core.SecretVKSecret))
	}
}

func TestKeyringStore_BackendErrors(t *testing.T) {
	backendErr := errors.New("keychain locked")
	keyring.MockInitWithError(backendErr)

	var s KeyringStore
	if _, err := s.Get("local", core.SecretAccessToken); err == nil || errors.Is(err, core.ErrSecretNotFound) {
		t.Errorf("Get backend error: want wrapped non-NotFound error, got %v", err)
	}
	if err := s.Set("local", core.SecretAccessToken, "v"); err == nil {
		t.Errorf("Set backend error: want error")
	}
	if err := s.Delete("local", core.SecretAccessToken); err == nil {
		t.Errorf("Delete backend error: want error")
	}
	keyring.MockInit() // restore clean mock for other tests
}

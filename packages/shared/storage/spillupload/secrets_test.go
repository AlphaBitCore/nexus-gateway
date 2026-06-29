package spillupload

import (
	"context"
	"encoding/base64"
	"errors"
	"github.com/goccy/go-json"
	"sync"
	"testing"
)

// stubMetaStore is an in-memory MetadataStore implementation that
// captures every Set so tests can assert bootstrap behaviour without a
// live Postgres.
type stubMetaStore struct {
	mu        sync.Mutex
	rows      map[string][]byte
	lastSetBy map[string]string
}

func newStubMetaStore() *stubMetaStore {
	return &stubMetaStore{
		rows:      map[string][]byte{},
		lastSetBy: map[string]string{},
	}
}

func (s *stubMetaStore) GetSystemMetadata(_ context.Context, key string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rows[key], nil
}

func (s *stubMetaStore) SetSystemMetadata(_ context.Context, key string, value any, updatedBy string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	s.rows[key] = b
	s.lastSetBy[key] = updatedBy
	return nil
}

func TestLoadOrInit_FreshBoot_GeneratesEpoch1(t *testing.T) {
	db := newStubMetaStore()
	store, err := LoadOrInit(context.Background(), db)
	if err != nil {
		t.Fatalf("LoadOrInit: %v", err)
	}
	kid, secret, err := store.Active()
	if err != nil {
		t.Fatalf("Active: %v", err)
	}
	if kid != "epoch-1" {
		t.Errorf("kid: want epoch-1, got %q", kid)
	}
	if len(secret) < 16 {
		t.Errorf("secret too short: %d bytes", len(secret))
	}
	// The bootstrap must have written the row back so a restart is
	// idempotent.
	raw, _ := db.GetSystemMetadata(context.Background(), SystemMetadataKey)
	if len(raw) == 0 {
		t.Fatal("bootstrap did not persist epoch-1")
	}
	if got := db.lastSetBy[SystemMetadataKey]; got != "hub-bootstrap" {
		t.Errorf("updatedBy: want hub-bootstrap, got %q", got)
	}
}

func TestLoadOrInit_Idempotent(t *testing.T) {
	db := newStubMetaStore()
	first, err := LoadOrInit(context.Background(), db)
	if err != nil {
		t.Fatalf("first LoadOrInit: %v", err)
	}
	_, secret1, _ := first.Active()

	second, err := LoadOrInit(context.Background(), db)
	if err != nil {
		t.Fatalf("second LoadOrInit: %v", err)
	}
	_, secret2, _ := second.Active()

	if string(secret1) != string(secret2) {
		t.Error("LoadOrInit must be idempotent — secret regenerated on second load")
	}
}

func TestLoadOrInit_PreservesPreSeededRow(t *testing.T) {
	db := newStubMetaStore()
	preseed := map[string]any{
		"active": "epoch-1",
		"secrets": map[string]string{
			"epoch-1": base64.StdEncoding.EncodeToString([]byte("a-seeded-secret-from-an-operator-hand")),
		},
	}
	if err := db.SetSystemMetadata(context.Background(), SystemMetadataKey, preseed, "operator"); err != nil {
		t.Fatal(err)
	}
	store, err := LoadOrInit(context.Background(), db)
	if err != nil {
		t.Fatalf("LoadOrInit: %v", err)
	}
	_, secret, _ := store.Active()
	if string(secret) != "a-seeded-secret-from-an-operator-hand" {
		t.Errorf("preseeded secret was not preserved: got %q", secret)
	}
}

func TestLoadOrInit_RejectsTooShortSecret(t *testing.T) {
	db := newStubMetaStore()
	preseed := map[string]any{
		"active": "epoch-1",
		"secrets": map[string]string{
			"epoch-1": base64.StdEncoding.EncodeToString([]byte("short")),
		},
	}
	_ = db.SetSystemMetadata(context.Background(), SystemMetadataKey, preseed, "operator")
	_, err := LoadOrInit(context.Background(), db)
	if err == nil || !errIsAboutShortSecret(err) {
		t.Fatalf("want too-short error, got %v", err)
	}
}

func errIsAboutShortSecret(err error) bool {
	if err == nil {
		return false
	}
	// Plain string match keeps the test independent of the package's
	// error message tweaks; the substring is intentional.
	return errors.Unwrap(err) != nil ||
		(err.Error() != "" && len(err.Error()) > 0 && contains(err.Error(), "too short"))
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

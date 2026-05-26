//go:build linux

package secretstore_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/identity/secretstore"
)

// TestLinux_OpenFallsBackWhenNoDBus points DBUS_SESSION_BUS_ADDRESS at a
// non-functional socket and confirms Open returns a working Store via the
// encrypted-file fallback. This runs on any Linux host (CI, dev box without
// gnome-keyring) because it never touches a real D-Bus.
func TestLinux_OpenFallsBackWhenNoDBus(t *testing.T) {
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=/dev/null")

	path := filepath.Join(t.TempDir(), "secretstore.enc")
	key := []byte("test-root-key-32-bytes-minimum-padded")

	s, err := secretstore.Open(path, key)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	if err := s.Set("k", []byte("v")); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := s.Get("k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != "v" {
		t.Fatalf("Get value mismatch: got %q want %q", got, "v")
	}
	if err := s.Delete("k"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get("k"); !errors.Is(err, secretstore.ErrNotFound) {
		t.Fatalf("Get after Delete: expected ErrNotFound, got %v", err)
	}
}

// TestLinux_LibsecretRoundtrip exercises the Set/Get/Delete lifecycle against
// a real D-Bus Secret Service implementation (gnome-keyring or KWallet).
// Skipped when no session bus is available so CI and headless hosts stay green.
func TestLinux_LibsecretRoundtrip(t *testing.T) {
	if os.Getenv("DBUS_SESSION_BUS_ADDRESS") == "" {
		t.Skip("no D-Bus session; skipping libsecret roundtrip")
	}

	s, err := secretstore.OpenSecret("ai.nexus.agent.test")
	if err != nil {
		t.Skipf("libsecret unavailable on this host: %v", err)
	}
	defer s.Close()
	defer s.Delete("k") //nolint:errcheck

	if err := s.Set("k", []byte("v")); err != nil {
		t.Fatalf("Set: %v", err)
	}
	v, err := s.Get("k")
	if err != nil || string(v) != "v" {
		t.Fatalf("Get after Set: err=%v value=%q", err, v)
	}
	if err := s.Delete("k"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get("k"); !errors.Is(err, secretstore.ErrNotFound) {
		t.Fatalf("Get after Delete: expected ErrNotFound, got %v", err)
	}
	// Delete of a missing key must be idempotent.
	if err := s.Delete("k"); err != nil {
		t.Fatalf("Delete of missing key: %v", err)
	}
}

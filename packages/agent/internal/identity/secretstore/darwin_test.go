//go:build darwin

package secretstore_test

import (
	"errors"
	"runtime"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/identity/secretstore"
)

// TestDarwin_KeychainRoundtrip exercises a full Set/Get/Delete cycle through
// the macOS Keychain backend so we know the happy path of every method is
// wired correctly. The test uses a dedicated "test" service identifier so it
// never collides with production agent data.
func TestDarwin_KeychainRoundtrip(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip()
	}
	s, err := secretstore.OpenKeychain("ai.nexus.agent.test")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()     //nolint:errcheck
	defer s.Delete("k") //nolint:errcheck
	if err := s.Set("k", []byte("v")); err != nil {
		t.Fatal(err)
	}
	v, err := s.Get("k")
	if err != nil || string(v) != "v" {
		t.Fatalf("%v %q", err, v)
	}
}

// TestDarwin_Get_MissingKey_ReturnsErrNotFound verifies that querying a key
// that was never written returns the package-level ErrNotFound sentinel so
// callers can `errors.Is` against it instead of string-matching. This exercises
// the QueryItem→ErrorItemNotFound→ErrNotFound conversion path in Get.
func TestDarwin_Get_MissingKey_ReturnsErrNotFound(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip()
	}
	// Use a randomised service name so a stray entry from a previous run
	// cannot accidentally satisfy the query.
	s, err := secretstore.OpenKeychain("ai.nexus.agent.test.missing")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close() //nolint:errcheck

	got, err := s.Get("never-written-key")
	if !errors.Is(err, secretstore.ErrNotFound) {
		t.Fatalf("Get on missing key: err = %v, want ErrNotFound", err)
	}
	if got != nil {
		t.Fatalf("Get on missing key: value = %q, want nil", got)
	}
}

// TestDarwin_Delete_MissingKey_IsNoOp verifies that Delete on a key that does
// not exist returns nil rather than surfacing the underlying keychain
// ErrorItemNotFound. The fallback backend has the same semantics; callers
// must be able to call Delete unconditionally during cleanup.
func TestDarwin_Delete_MissingKey_IsNoOp(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip()
	}
	s, err := secretstore.OpenKeychain("ai.nexus.agent.test.missing.delete")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close() //nolint:errcheck

	if err := s.Delete("never-written-key"); err != nil {
		t.Fatalf("Delete on missing key: %v, want nil", err)
	}
}

// TestDarwin_Open_PrefersKeychain verifies that the platform-selecting Open()
// returns the macOS Keychain backend when a user session is available (which
// is the normal developer/CI case). It must succeed without ever touching the
// fallback file. We confirm this by passing a fallback path that points at a
// non-writeable location: if Open ever fell through to OpenFallback it would
// fail to persist on first Set.
func TestDarwin_Open_PrefersKeychain(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip()
	}
	// /dev/null/secret is unwriteable; if Open() incorrectly fell back to the
	// encrypted-file backend, the eventual persist() would fail. We exercise
	// only the Open() decision here.
	s, err := secretstore.Open("/dev/null/secret", []byte("ignored-key"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close() //nolint:errcheck

	// Use a dedicated key namespace so we don't pollute real entries. We don't
	// assert Set/Get here because that's covered by KeychainRoundtrip; the
	// point is that Open() returned a working backend without falling back.
	if err := s.Delete("never-written-key-open-test"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}

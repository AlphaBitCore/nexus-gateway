//go:build darwin

package secretstore

// darwin_seam_test.go drives the keychain-error branches in darwin.go and
// open_darwin.go via the package-level seams. The real Keychain library
// rarely surfaces these errors on a healthy macOS host, so injection is the
// only way to verify the wrapping + classification logic.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/keybase/go-keychain"
)

// TestSeam_Darwin_Set_AddItemError verifies that Set wraps an AddItem
// failure with "keychain add:" context so callers can pinpoint the failed
// stage. AddItem failures are how the OS reports keychain DB corruption or
// access-control mismatches; we must never swallow them.
func TestSeam_Darwin_Set_AddItemError(t *testing.T) {
	sentinel := errors.New("forced add error")
	prev := keychainAddItemFn
	keychainAddItemFn = func(item keychain.Item) error { return sentinel }
	defer func() { keychainAddItemFn = prev }()

	s, err := OpenKeychain("ai.nexus.agent.test.seam")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close() //nolint:errcheck

	err = s.Set("k", []byte("v"))
	if err == nil {
		t.Fatal("expected AddItem error, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want chain containing sentinel", err)
	}
	if !strings.Contains(err.Error(), "keychain add") {
		t.Fatalf("err = %q, want 'keychain add' wrap context", err.Error())
	}
}

// TestSeam_Darwin_Get_QueryItemGenericError verifies that a non-NotFound
// QueryItem error is wrapped with "keychain query:" context (NOT swallowed
// as ErrNotFound). This is the security-critical distinction: an opaque
// keychain failure must NOT look like "key doesn't exist" to callers,
// otherwise they would re-issue a Set and overwrite valid data.
func TestSeam_Darwin_Get_QueryItemGenericError(t *testing.T) {
	sentinel := errors.New("forced query error")
	prev := keychainQueryItemFn
	keychainQueryItemFn = func(item keychain.Item) ([]keychain.QueryResult, error) {
		return nil, sentinel
	}
	defer func() { keychainQueryItemFn = prev }()

	s, err := OpenKeychain("ai.nexus.agent.test.seam.query")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close() //nolint:errcheck

	_, err = s.Get("k")
	if err == nil {
		t.Fatal("expected QueryItem error, got nil")
	}
	if errors.Is(err, ErrNotFound) {
		t.Fatal("generic QueryItem error must NOT be reclassified as ErrNotFound")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want chain containing sentinel", err)
	}
	if !strings.Contains(err.Error(), "keychain query") {
		t.Fatalf("err = %q, want 'keychain query' wrap context", err.Error())
	}
}

// TestSeam_Darwin_Get_QueryItemNotFound verifies that the ErrorItemNotFound
// keychain error is normalised to the package-level ErrNotFound sentinel so
// callers can `errors.Is` against it. Without injection, QueryItem returns
// (nil, nil) for a missing key — never the typed error — leaving this
// classification branch uncovered.
func TestSeam_Darwin_Get_QueryItemNotFound(t *testing.T) {
	prev := keychainQueryItemFn
	keychainQueryItemFn = func(item keychain.Item) ([]keychain.QueryResult, error) {
		return nil, keychain.ErrorItemNotFound
	}
	defer func() { keychainQueryItemFn = prev }()

	s, err := OpenKeychain("ai.nexus.agent.test.seam.query.nf")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close() //nolint:errcheck

	got, err := s.Get("k")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if got != nil {
		t.Fatalf("value = %q, want nil", got)
	}
}

// TestSeam_Darwin_Delete_NonNotFoundError verifies that a generic DeleteItem
// failure is wrapped with "keychain delete:" context. The other branch
// (ErrorItemNotFound becomes nil) is also exercised here as a control case
// via the missing-key Delete test in darwin_test.go.
func TestSeam_Darwin_Delete_NonNotFoundError(t *testing.T) {
	sentinel := errors.New("forced delete error")
	prev := keychainDeleteItemFn
	keychainDeleteItemFn = func(item keychain.Item) error { return sentinel }
	defer func() { keychainDeleteItemFn = prev }()

	s, err := OpenKeychain("ai.nexus.agent.test.seam.delete")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close() //nolint:errcheck

	err = s.Delete("k")
	if err == nil {
		t.Fatal("expected DeleteItem error, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want chain containing sentinel", err)
	}
	if !strings.Contains(err.Error(), "keychain delete") {
		t.Fatalf("err = %q, want 'keychain delete' wrap context", err.Error())
	}
}

// TestSeam_Open_FallsBackWhenKeychainFails verifies that Open() correctly
// degrades to the encrypted-file backend when OpenKeychain returns an error
// (e.g., headless macOS without a user session). The seam injects a failing
// OpenKeychain; the resulting Store must come from OpenFallback and survive
// a real Set/Get round-trip.
func TestSeam_Open_FallsBackWhenKeychainFails(t *testing.T) {
	prev := openKeychainFn
	openKeychainFn = func(service string) (Store, error) {
		return nil, errors.New("forced keychain unavailable")
	}
	defer func() { openKeychainFn = prev }()

	path := filepath.Join(t.TempDir(), "s.enc")
	s, err := Open(path, []byte("test-root-key"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close() //nolint:errcheck

	// Verify it really is the file fallback by writing + reading something
	// and checking the file appears on disk where Keychain would never put it.
	if err := s.Set("rt", []byte("rt-value")); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := s.Get("rt")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != "rt-value" {
		t.Fatalf("Get = %q, want %q", got, "rt-value")
	}
	// File must exist (fallback persists to disk); Keychain backend never
	// writes to this path.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat fallback file: %v", err)
	}
	if info.Size() == 0 {
		t.Fatal("fallback file is empty; fallback backend did not persist")
	}
}

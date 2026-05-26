package secretstore_test

// fallback_seam_test.go drives the unreachable error branches in fallback.go
// via the test seams exposed in export_test.go. Each test injects a single
// failure into the constructor or persist() pipeline and asserts the
// resulting wrapped error surfaces (security-critical: we must never silently
// continue past a crypto/syscall failure).

import (
	"crypto/cipher"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/identity/secretstore"
)

// TestSeam_OpenFallback_NewCipherError surfaces a cipher-construction
// failure as a wrapped "init cipher" error. We swap aes.NewCipher with a
// function that always errors; this exercises the line that would otherwise
// only fire if AES rejected a 32-byte key, which it never does.
func TestSeam_OpenFallback_NewCipherError(t *testing.T) {
	sentinel := errors.New("forced cipher error")
	restore := secretstore.SetNewCipherFn(func(key []byte) (cipher.Block, error) {
		return nil, sentinel
	})
	defer restore()

	_, err := secretstore.OpenFallback(filepath.Join(t.TempDir(), "s.enc"), []byte("k"))
	if err == nil {
		t.Fatal("expected init cipher error, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want chain containing sentinel", err)
	}
	if !strings.Contains(err.Error(), "init cipher") {
		t.Fatalf("err = %q, want 'init cipher' wrap context", err.Error())
	}
}

// TestSeam_OpenFallback_NewGCMError surfaces an AEAD-construction failure
// as a wrapped "init GCM" error. cipher.NewGCM never fails on a real AES
// block, so this branch is only reachable via injection.
func TestSeam_OpenFallback_NewGCMError(t *testing.T) {
	sentinel := errors.New("forced GCM error")
	restore := secretstore.SetNewGCMFn(func(b cipher.Block) (cipher.AEAD, error) {
		return nil, sentinel
	})
	defer restore()

	_, err := secretstore.OpenFallback(filepath.Join(t.TempDir(), "s.enc"), []byte("k"))
	if err == nil {
		t.Fatal("expected init GCM error, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want chain containing sentinel", err)
	}
	if !strings.Contains(err.Error(), "init GCM") {
		t.Fatalf("err = %q, want 'init GCM' wrap context", err.Error())
	}
}

// TestSeam_Persist_RandReadError surfaces a nonce-generation failure as a
// wrapped "generate nonce" error on Set. rand.Read on /dev/urandom never
// errors in practice; this branch only fires when entropy is exhausted in
// a way the kernel surfaces, which a unit test cannot induce naturally.
func TestSeam_Persist_RandReadError(t *testing.T) {
	s, err := secretstore.OpenFallback(filepath.Join(t.TempDir(), "s.enc"), []byte("k"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close() //nolint:errcheck

	sentinel := errors.New("forced rand error")
	restore := secretstore.SetRandReadFn(func(b []byte) (int, error) {
		return 0, sentinel
	})
	defer restore()

	err = s.Set("k", []byte("v"))
	if err == nil {
		t.Fatal("expected generate nonce error, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want chain containing sentinel", err)
	}
	if !strings.Contains(err.Error(), "generate nonce") {
		t.Fatalf("err = %q, want 'generate nonce' wrap context", err.Error())
	}
}

// newFakeCreateTemp installs a fake CreateTemp seam returning the supplied
// FakeFile. Tests configure which of {Chmod, Write, Sync, Close} errors and
// then exercise s.Set to drive the matching error branch in persist().
func newFakeCreateTemp(t *testing.T, ff *secretstore.FakeFile) func() {
	t.Helper()
	// Bind a real path so the cleanup's os.Remove(tmpName) is harmless.
	ff.NameVal = filepath.Join(t.TempDir(), ".secretstore-fake")
	return secretstore.SetCreateTempFnFake(ff)
}

// TestSeam_Persist_ChmodError exercises the "chmod temp fallback file"
// error branch. tempfile Chmod on a freshly-created file rarely fails, so
// injection is the only way to verify the error is wrapped + the file is
// closed + removed (cleanup) rather than left dangling at default perms.
func TestSeam_Persist_ChmodError(t *testing.T) {
	s, err := secretstore.OpenFallback(filepath.Join(t.TempDir(), "s.enc"), []byte("k"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close() //nolint:errcheck

	sentinel := errors.New("forced chmod error")
	ff := &secretstore.FakeFile{ChmodErr: sentinel}
	restore := newFakeCreateTemp(t, ff)
	defer restore()

	err = s.Set("k", []byte("v"))
	if err == nil {
		t.Fatal("expected chmod error, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want chain containing sentinel", err)
	}
	if !strings.Contains(err.Error(), "chmod temp") {
		t.Fatalf("err = %q, want 'chmod temp' wrap context", err.Error())
	}
	if !ff.CloseDone {
		t.Fatal("Chmod failure must Close the temp file (resource leak)")
	}
}

// TestSeam_Persist_WriteError exercises the "write temp fallback file"
// error branch. A partial-write or full-write failure must result in the
// file being closed AND removed; never left half-written on disk.
func TestSeam_Persist_WriteError(t *testing.T) {
	s, err := secretstore.OpenFallback(filepath.Join(t.TempDir(), "s.enc"), []byte("k"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close() //nolint:errcheck

	sentinel := errors.New("forced write error")
	ff := &secretstore.FakeFile{WriteErr: sentinel}
	restore := newFakeCreateTemp(t, ff)
	defer restore()

	err = s.Set("k", []byte("v"))
	if err == nil {
		t.Fatal("expected write error, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want chain containing sentinel", err)
	}
	if !strings.Contains(err.Error(), "write temp") {
		t.Fatalf("err = %q, want 'write temp' wrap context", err.Error())
	}
	if !ff.CloseDone {
		t.Fatal("Write failure must Close the temp file (resource leak)")
	}
}

// TestSeam_Persist_SyncError exercises the "sync temp fallback file" branch.
// A failed fsync(2) means the bytes are not durably on disk; we must surface
// the error so callers know the secret is not persisted (avoid claiming
// success on data the kernel has buffered).
func TestSeam_Persist_SyncError(t *testing.T) {
	s, err := secretstore.OpenFallback(filepath.Join(t.TempDir(), "s.enc"), []byte("k"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close() //nolint:errcheck

	sentinel := errors.New("forced sync error")
	ff := &secretstore.FakeFile{SyncErr: sentinel}
	restore := newFakeCreateTemp(t, ff)
	defer restore()

	err = s.Set("k", []byte("v"))
	if err == nil {
		t.Fatal("expected sync error, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want chain containing sentinel", err)
	}
	if !strings.Contains(err.Error(), "sync temp") {
		t.Fatalf("err = %q, want 'sync temp' wrap context", err.Error())
	}
	if !ff.CloseDone {
		t.Fatal("Sync failure must Close the temp file (resource leak)")
	}
}

// TestSeam_Persist_CloseError exercises the "close temp fallback file"
// branch. Even though the data is already written + fsynced, a failed
// close(2) on POSIX can indicate write errors deferred until close, so we
// must NOT rename and MUST remove the tempfile.
func TestSeam_Persist_CloseError(t *testing.T) {
	s, err := secretstore.OpenFallback(filepath.Join(t.TempDir(), "s.enc"), []byte("k"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close() //nolint:errcheck

	sentinel := errors.New("forced close error")
	ff := &secretstore.FakeFile{CloseErr: sentinel}
	restore := newFakeCreateTemp(t, ff)
	defer restore()

	err = s.Set("k", []byte("v"))
	if err == nil {
		t.Fatal("expected close error, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want chain containing sentinel", err)
	}
	if !strings.Contains(err.Error(), "close temp") {
		t.Fatalf("err = %q, want 'close temp' wrap context", err.Error())
	}
}

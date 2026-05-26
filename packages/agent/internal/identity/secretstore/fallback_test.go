package secretstore_test

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"golang.org/x/crypto/hkdf"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/identity/secretstore"
)

// TestFallback_RoundTripAcrossReopen verifies that Set values persist across
// a Close + re-Open cycle using the same root key and path.
func TestFallback_RoundTripAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.enc")
	key := []byte("device-root-key-32-bytes-or-any-len")

	s1, err := secretstore.OpenFallback(path, key)
	if err != nil {
		t.Fatalf("open (first): %v", err)
	}
	if err := s1.Set("refresh_token", []byte("rt-value-1")); err != nil {
		t.Fatalf("set refresh_token: %v", err)
	}
	if err := s1.Set("session_id", []byte("sid-value-2")); err != nil {
		t.Fatalf("set session_id: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("close (first): %v", err)
	}

	s2, err := secretstore.OpenFallback(path, key)
	if err != nil {
		t.Fatalf("open (second): %v", err)
	}
	defer s2.Close() //nolint:errcheck

	got, err := s2.Get("refresh_token")
	if err != nil {
		t.Fatalf("get refresh_token after reopen: %v", err)
	}
	if string(got) != "rt-value-1" {
		t.Fatalf("refresh_token = %q, want %q", got, "rt-value-1")
	}

	got, err = s2.Get("session_id")
	if err != nil {
		t.Fatalf("get session_id after reopen: %v", err)
	}
	if string(got) != "sid-value-2" {
		t.Fatalf("session_id = %q, want %q", got, "sid-value-2")
	}

	if err := s2.Delete("refresh_token"); err != nil {
		t.Fatalf("delete refresh_token: %v", err)
	}
	if _, err := s2.Get("refresh_token"); !errors.Is(err, secretstore.ErrNotFound) {
		t.Fatalf("after delete, get refresh_token err = %v, want ErrNotFound", err)
	}
}

// TestFallback_TamperedCiphertext_Rejected verifies that flipping a byte in
// the on-disk ciphertext causes OpenFallback to fail loudly (never silent),
// and the error is not ErrNotFound.
func TestFallback_TamperedCiphertext_Rejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.enc")
	key := []byte("device-root-key")

	s, err := secretstore.OpenFallback(path, key)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := s.Set("k", []byte("v")); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if len(raw) < 16 {
		t.Fatalf("file too short to tamper: %d bytes", len(raw))
	}
	// Flip a byte in the ciphertext portion (after a 12-byte AES-GCM nonce).
	raw[len(raw)-1] ^= 0xff
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write tampered file: %v", err)
	}

	_, err = secretstore.OpenFallback(path, key)
	if err == nil {
		t.Fatal("expected error opening tampered store, got nil")
	}
	if errors.Is(err, secretstore.ErrNotFound) {
		t.Fatalf("expected decryption error, got ErrNotFound")
	}
}

// TestFallback_WrongRootKey_Rejected verifies that opening an existing store
// with a different root key fails (cannot derive the same AES key, AEAD open
// rejects the ciphertext).
func TestFallback_WrongRootKey_Rejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.enc")

	s, err := secretstore.OpenFallback(path, []byte("first-key"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := s.Set("k", []byte("v")); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if _, err := secretstore.OpenFallback(path, []byte("second-key")); err == nil {
		t.Fatal("expected error opening with wrong key, got nil")
	}
}

// TestFallback_FileMode_0600_OnPOSIX verifies that after a Set the on-disk
// file has mode 0600. Windows file modes are not POSIX; skip there.
func TestFallback_FileMode_0600_OnPOSIX(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file mode semantics differ on Windows")
	}
	path := filepath.Join(t.TempDir(), "s.enc")

	s, err := secretstore.OpenFallback(path, []byte("k"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close() //nolint:errcheck
	if err := s.Set("a", []byte("b")); err != nil {
		t.Fatalf("set: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("file mode = %o, want 0600", mode)
	}
}

// TestFallback_EmptyStore_NoFile verifies Open on a non-existent path
// succeeds (empty store) and Get returns ErrNotFound.
func TestFallback_EmptyStore_NoFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.enc")

	s, err := secretstore.OpenFallback(path, []byte("k"))
	if err != nil {
		t.Fatalf("open empty: %v", err)
	}
	defer s.Close() //nolint:errcheck

	if _, err := s.Get("any"); !errors.Is(err, secretstore.ErrNotFound) {
		t.Fatalf("get on empty store err = %v, want ErrNotFound", err)
	}
}

// TestFallback_ParentDirCreated verifies that a missing parent directory is
// created with restrictive permissions on first Set.
func TestFallback_ParentDirCreated(t *testing.T) {
	base := t.TempDir()
	path := filepath.Join(base, "nested", "deeper", "s.enc")

	s, err := secretstore.OpenFallback(path, []byte("k"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close() //nolint:errcheck
	if err := s.Set("x", []byte("y")); err != nil {
		t.Fatalf("set: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("stat created file: %v", err)
	}
}

// TestFallback_EmptyFile_TreatedAsEmptyStore verifies that an existing
// zero-byte file is treated as an empty store (not as a malformed file).
// This matters because tempfile + rename atomicity means we should never see
// a zero-byte file in practice, but if one does appear (e.g. an admin
// truncated it) load() must not panic or surface a bogus decryption error.
func TestFallback_EmptyFile_TreatedAsEmptyStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.enc")
	if err := os.WriteFile(path, []byte{}, 0o600); err != nil {
		t.Fatalf("seed empty file: %v", err)
	}

	s, err := secretstore.OpenFallback(path, []byte("k"))
	if err != nil {
		t.Fatalf("open on empty file: %v", err)
	}
	defer s.Close() //nolint:errcheck

	if _, err := s.Get("any"); !errors.Is(err, secretstore.ErrNotFound) {
		t.Fatalf("get on empty-file store err = %v, want ErrNotFound", err)
	}
}

// TestFallback_MalformedFile_TooShort verifies that an on-disk file shorter
// than `nonce + tag` length is rejected loudly. This is the boundary between
// "no file / empty file" and "we have ciphertext"; without this check a
// malicious truncation could silently zero out the store.
func TestFallback_MalformedFile_TooShort(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.enc")
	// 4 bytes is well under the 12-byte nonce + 16-byte GCM tag minimum.
	if err := os.WriteFile(path, []byte{0x01, 0x02, 0x03, 0x04}, 0o600); err != nil {
		t.Fatalf("seed short file: %v", err)
	}

	_, err := secretstore.OpenFallback(path, []byte("k"))
	if err == nil {
		t.Fatal("expected error on too-short file, got nil")
	}
	if errors.Is(err, secretstore.ErrNotFound) {
		t.Fatalf("expected malformed-file error, got ErrNotFound")
	}
}

// TestFallback_ReadFileError_SurfacedAsError verifies that an os.ReadFile
// failure that is NOT fs.ErrNotExist is surfaced as an error. We trigger this
// by pointing the store path at a directory: ReadFile on a directory returns
// an "is a directory" error, which is neither ErrNotExist nor a decryption
// failure, so load() must wrap and return it.
func TestFallback_ReadFileError_SurfacedAsError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory-as-path read semantics differ on Windows")
	}
	dir := t.TempDir()
	// Use the temp dir itself as the "file" path; ReadFile will fail.
	_, err := secretstore.OpenFallback(dir, []byte("k"))
	if err == nil {
		t.Fatal("expected error opening directory as fallback file, got nil")
	}
	if errors.Is(err, secretstore.ErrNotFound) {
		t.Fatalf("expected ReadFile error, got ErrNotFound")
	}
}

// TestFallback_Delete_MissingKey_IsNoOp verifies that Delete on an absent key
// returns nil AND does NOT touch the on-disk file. The latter matters because
// a no-op Delete should not bump the file's mtime or trigger an unnecessary
// fsync, which would be visible as a write-amplification regression.
func TestFallback_Delete_MissingKey_IsNoOp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.enc")
	s, err := secretstore.OpenFallback(path, []byte("k"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close() //nolint:errcheck

	// Seed one entry so the file exists with a known mtime.
	if err := s.Set("present", []byte("v")); err != nil {
		t.Fatalf("set: %v", err)
	}
	infoBefore, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat before: %v", err)
	}

	// Sleep a hair so a fresh write would visibly bump mtime if it occurred.
	// On filesystems with second-granularity mtime we'd need longer, but the
	// test only requires "no error" — the mtime check is a stretch goal that
	// catches regressions on nanosecond-mtime filesystems (ext4, APFS, ZFS).
	if err := s.Delete("never-written"); err != nil {
		t.Fatalf("Delete on missing key: %v, want nil", err)
	}

	infoAfter, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat after: %v", err)
	}
	if !infoAfter.ModTime().Equal(infoBefore.ModTime()) {
		t.Fatalf("Delete on missing key bumped file mtime: before=%v after=%v",
			infoBefore.ModTime(), infoAfter.ModTime())
	}

	// And the surviving entry must still be there.
	got, err := s.Get("present")
	if err != nil {
		t.Fatalf("get surviving entry: %v", err)
	}
	if string(got) != "v" {
		t.Fatalf("surviving entry = %q, want %q", got, "v")
	}
}

// TestFallback_MkdirAllError_SurfacedAsError verifies that persist() returns a
// wrapped error when the parent directory cannot be created. We force this by
// making an ancestor directory read-only (mode 0o500): MkdirAll fails when it
// tries to mkdir(2) the missing nested child. The store opens cleanly because
// load() returns nil on a missing file, then Set triggers persist() which
// tries to create the unreachable parent.
func TestFallback_MkdirAllError_SurfacedAsError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX directory-mode semantics; Windows uses ACLs")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses POSIX directory permissions")
	}
	base := t.TempDir()
	// `base/locked` is a read-only directory; MkdirAll trying to create
	// `base/locked/nested` will fail with EACCES.
	locked := filepath.Join(base, "locked")
	if err := os.Mkdir(locked, 0o500); err != nil {
		t.Fatalf("mkdir locked: %v", err)
	}
	// Restore mode at end so t.TempDir() can clean up.
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })

	path := filepath.Join(locked, "nested", "s.enc")
	s, err := secretstore.OpenFallback(path, []byte("k"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close() //nolint:errcheck

	err = s.Set("k", []byte("v"))
	if err == nil {
		t.Fatal("expected MkdirAll error on Set, got nil")
	}
	if errors.Is(err, secretstore.ErrNotFound) {
		t.Fatalf("expected MkdirAll error, got ErrNotFound")
	}
}

// TestFallback_CreateTempError_SurfacedAsError verifies that persist() returns
// a wrapped error when CreateTemp fails — the failure mode you hit when the
// parent directory exists (so MkdirAll is a no-op) but is not writeable. The
// store still opens successfully (load reads no file from a directory that
// exists), then the first Set triggers CreateTemp in a read-only parent.
func TestFallback_CreateTempError_SurfacedAsError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX directory-mode semantics; Windows uses ACLs")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses POSIX directory permissions")
	}
	base := t.TempDir()
	// Parent already exists so MkdirAll is a no-op; mode 0o500 prevents
	// CreateTemp from creating the tempfile inside it.
	parent := filepath.Join(base, "ro")
	if err := os.Mkdir(parent, 0o500); err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })

	path := filepath.Join(parent, "s.enc")
	s, err := secretstore.OpenFallback(path, []byte("k"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close() //nolint:errcheck

	err = s.Set("k", []byte("v"))
	if err == nil {
		t.Fatal("expected CreateTemp error on Set, got nil")
	}
	if errors.Is(err, secretstore.ErrNotFound) {
		t.Fatalf("expected CreateTemp error, got ErrNotFound")
	}
}

// TestFallback_NonJSONPlaintext_RejectedAsDecodeError verifies that a file
// whose AEAD plaintext is NOT a JSON object is rejected with a decode error
// rather than silently leaving the in-memory map empty. We construct this
// pathological file by deriving the same AES-GCM key the production code
// would derive, then sealing garbage plaintext with a fresh nonce — the exact
// shape of a corruption a buggy writer could leave behind.
func TestFallback_NonJSONPlaintext_RejectedAsDecodeError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.enc")
	rootKey := []byte("device-root-key-for-decode-error-test")

	// Mirror the production HKDF→AES-GCM key derivation. The hkdfInfo string
	// is "nexus-agent-secretstore/v1"; keep this in sync with fallback.go.
	h := hkdf.New(sha256.New, rootKey, nil, []byte("nexus-agent-secretstore/v1"))
	derived := make([]byte, 32)
	if _, err := io.ReadFull(h, derived); err != nil {
		t.Fatalf("derive: %v", err)
	}
	block, err := aes.NewCipher(derived)
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("gcm: %v", err)
	}
	nonce := make([]byte, aead.NonceSize())
	// Use a fixed nonce here (zeros) — the test never reuses this key for
	// genuine writes so there is no nonce-reuse risk to a real store.
	plaintext := []byte("this is not json")
	sealed := aead.Seal(nonce, nonce, plaintext, nil)
	if err := os.WriteFile(path, sealed, 0o600); err != nil {
		t.Fatalf("write sealed: %v", err)
	}

	_, err = secretstore.OpenFallback(path, rootKey)
	if err == nil {
		t.Fatal("expected decode error on non-JSON plaintext, got nil")
	}
	if errors.Is(err, secretstore.ErrNotFound) {
		t.Fatalf("expected decode error, got ErrNotFound")
	}
}

// TestFallback_RenameError_SurfacedAsError verifies that persist() returns a
// wrapped error when os.Rename fails. We force this by making the target path
// a non-empty directory: rename(tempfile, dir) fails on POSIX systems because
// you cannot replace a directory with a file. This exercises the final
// atomic-commit step of persist().
func TestFallback_RenameError_SurfacedAsError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("rename-onto-directory semantics differ on Windows")
	}
	base := t.TempDir()
	// Make the store "path" a directory containing a child, so a rename onto
	// it cannot succeed.
	path := filepath.Join(base, "s.enc")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	if err := os.WriteFile(filepath.Join(path, "child"), []byte("x"), 0o600); err != nil {
		t.Fatalf("seed child: %v", err)
	}

	// OpenFallback tries to ReadFile the path; reading a directory fails, so
	// the store will refuse to open. That itself is a covered behaviour
	// (ReadFileError test), so we can't use Set to exercise the rename path
	// via this exact construction. Use a different layout: store path is
	// inside a directory we'll keep writeable, but a sibling file at the
	// store path's tempfile location collides... too fragile. Instead, use
	// OpenFallback against a fresh path, then `os.Rename` the resulting
	// regular file into a directory and persist again.

	good := filepath.Join(base, "good.enc")
	s, err := secretstore.OpenFallback(good, []byte("k"))
	if err != nil {
		t.Fatalf("open good: %v", err)
	}
	defer s.Close() //nolint:errcheck
	if err := s.Set("k", []byte("v")); err != nil {
		t.Fatalf("seed write: %v", err)
	}
	// Replace the real file with a non-empty directory so the next persist's
	// Rename(tmpName, path) fails. On Linux this returns EISDIR / ENOTEMPTY.
	if err := os.Remove(good); err != nil {
		t.Fatalf("remove seed file: %v", err)
	}
	if err := os.Mkdir(good, 0o700); err != nil {
		t.Fatalf("mkdir over seed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(good, "block"), []byte("x"), 0o600); err != nil {
		t.Fatalf("seed block child: %v", err)
	}

	err = s.Set("k", []byte("v2"))
	if err == nil {
		t.Fatal("expected Rename error on Set with directory target, got nil")
	}
	if errors.Is(err, secretstore.ErrNotFound) {
		t.Fatalf("expected Rename error, got ErrNotFound")
	}
}

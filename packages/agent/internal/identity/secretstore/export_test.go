package secretstore

import (
	"crypto/cipher"
	"hash"
	"os"
)

// This file exposes a controlled set of internal test seams to fault-injection
// tests in fallback_seam_test.go. The seams default to the standard-library
// implementations on package init; tests substitute failing implementations
// to exercise error branches that are unreachable through normal filesystem
// or syscall behaviour. Mirrors the established pattern in
// packages/control-plane/internal/crypto/aes_gcm.go.

// SetSHA256Fn replaces the HKDF hash factory. Returns a restorer; tests
// `defer restore()` to ensure global state is reset even on test failure.
func SetSHA256Fn(f func() hash.Hash) func() {
	prev := sha256Fn
	sha256Fn = f
	return func() { sha256Fn = prev }
}

// SetNewCipherFn replaces the aes.NewCipher seam.
func SetNewCipherFn(f func(key []byte) (cipher.Block, error)) func() {
	prev := newCipherFn
	newCipherFn = f
	return func() { newCipherFn = prev }
}

// SetNewGCMFn replaces the cipher.NewGCM seam.
func SetNewGCMFn(f func(b cipher.Block) (cipher.AEAD, error)) func() {
	prev := newGCMFn
	newGCMFn = f
	return func() { newGCMFn = prev }
}

// SetRandReadFn replaces the rand.Read seam used for nonce generation.
func SetRandReadFn(f func(b []byte) (int, error)) func() {
	prev := randReadFn
	randReadFn = f
	return func() { randReadFn = prev }
}

// FakeFile is a test-only osFile that lets tests choose which of the
// four post-CreateTemp operations (Chmod, Write, Sync, Close) returns
// an error. A zero FakeFile behaves like a successful write.
type FakeFile struct {
	NameVal   string
	ChmodErr  error
	WriteErr  error
	SyncErr   error
	CloseErr  error
	WriteN    int
	CloseDone bool
}

// Name implements osFile by returning a configured (or empty) path.
func (f *FakeFile) Name() string { return f.NameVal }

// Chmod implements osFile; returns the configured error.
func (f *FakeFile) Chmod(mode os.FileMode) error { return f.ChmodErr }

// Write implements osFile; returns the configured error or len(p) bytes.
func (f *FakeFile) Write(p []byte) (int, error) {
	if f.WriteErr != nil {
		return 0, f.WriteErr
	}
	f.WriteN = len(p)
	return len(p), nil
}

// Sync implements osFile; returns the configured error.
func (f *FakeFile) Sync() error { return f.SyncErr }

// Close implements osFile; sets CloseDone and returns the configured error.
func (f *FakeFile) Close() error {
	f.CloseDone = true
	return f.CloseErr
}

// SetCreateTempFnFake installs createTempFn to return the supplied FakeFile.
// This is a convenience wrapper that hides the unexported osFile interface
// from black-box `_test` callers, who otherwise cannot construct a value
// satisfying it.
func SetCreateTempFnFake(ff *FakeFile) func() {
	prev := createTempFn
	createTempFn = func(dir, pattern string) (osFile, error) {
		return ff, nil
	}
	return func() { createTempFn = prev }
}

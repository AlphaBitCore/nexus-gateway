package secretstore

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/crypto/hkdf"
)

// hkdfInfo is the HKDF info string that binds the derived AES key to this
// package and its on-disk format version. Changing this value invalidates
// all existing fallback files. Treat it as a versioning hook.
const hkdfInfo = "nexus-agent-secretstore/v1"

// sha256Fn is the hash constructor used by HKDF. It is a package-level
// variable solely so tests can substitute a failing hash factory and
// exercise the HKDF read-error branch in OpenFallback; production code never
// reassigns it.
var sha256Fn = sha256.New

// The following package-level variables exist solely as test seams: tests
// substitute failing implementations to exercise the error branches in
// OpenFallback (cipher/AEAD construction) and persist() (atomic-write
// syscalls). Production code never reassigns them. This mirrors the
// `randReader`/`newGCM` pattern in packages/control-plane/internal/crypto.
var (
	newCipherFn = aes.NewCipher
	newGCMFn    = cipher.NewGCM
	randReadFn  = rand.Read
	mkdirAllFn  = os.MkdirAll
	createTempFn = func(dir, pattern string) (osFile, error) {
		return os.CreateTemp(dir, pattern)
	}
	renameFn = os.Rename
)

// osFile is the subset of *os.File methods persist() uses, named as an
// interface so test seams can return a mock implementation. Production code
// receives the real *os.File from os.CreateTemp.
type osFile interface {
	Name() string
	Chmod(mode os.FileMode) error
	Write(p []byte) (int, error)
	Sync() error
	Close() error
}

// fallbackStore is an encrypted-file secret store. Contents are serialized as
// JSON, sealed with AES-256-GCM, and written as nonce||ciphertext in a single
// file. The AES key is derived from the caller-supplied root key via
// HKDF-SHA256 with a fixed info string (hkdfInfo) and nil salt.
type fallbackStore struct {
	mu   sync.Mutex
	path string
	aead cipher.AEAD
	data map[string][]byte
}

// OpenFallback forces the encrypted-file backend. The rootKey supplies keying
// material for HKDF-SHA256; callers should pass a stable device-bound secret.
// If path does not exist, an empty store is returned and the file is created
// on the first write.
func OpenFallback(path string, rootKey []byte) (Store, error) {
	h := hkdf.New(sha256Fn, rootKey, nil, []byte(hkdfInfo))
	derived := make([]byte, 32)
	if _, err := io.ReadFull(h, derived); err != nil {
		return nil, fmt.Errorf("secretstore: derive key: %w", err)
	}
	block, err := newCipherFn(derived)
	if err != nil {
		return nil, fmt.Errorf("secretstore: init cipher: %w", err)
	}
	aead, err := newGCMFn(block)
	if err != nil {
		return nil, fmt.Errorf("secretstore: init GCM: %w", err)
	}
	s := &fallbackStore{
		path: path,
		aead: aead,
		data: make(map[string][]byte),
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// load reads and decrypts the on-disk file into s.data. A missing file is
// treated as an empty store. Any decryption failure is surfaced as an error;
// we never silently reset to empty on AEAD open failure.
func (s *fallbackStore) load() error {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("secretstore: read fallback file: %w", err)
	}
	if len(raw) == 0 {
		return nil
	}
	ns := s.aead.NonceSize()
	if len(raw) < ns+s.aead.Overhead() {
		return errors.New("secretstore: malformed fallback file")
	}
	nonce, ct := raw[:ns], raw[ns:]
	plain, err := s.aead.Open(nil, nonce, ct, nil)
	if err != nil {
		return fmt.Errorf("secretstore: decryption failed: %w", err)
	}
	if err := json.Unmarshal(plain, &s.data); err != nil {
		return fmt.Errorf("secretstore: decode fallback file: %w", err)
	}
	return nil
}

// persist marshals, seals, and atomically writes s.data to disk. The write
// uses tempfile + fsync + rename to ensure a crash mid-persist never leaves a
// half-written file in place.
func (s *fallbackStore) persist() error {
	plain, err := json.Marshal(s.data)
	if err != nil {
		return fmt.Errorf("secretstore: encode fallback file: %w", err)
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := randReadFn(nonce); err != nil {
		return fmt.Errorf("secretstore: generate nonce: %w", err)
	}
	sealed := s.aead.Seal(nonce, nonce, plain, nil)

	dir := filepath.Dir(s.path)
	if dir != "" && dir != "." {
		if err := mkdirAllFn(dir, 0o700); err != nil {
			return fmt.Errorf("secretstore: create fallback dir: %w", err)
		}
	}
	tmp, err := createTempFn(dir, ".secretstore-*")
	if err != nil {
		return fmt.Errorf("secretstore: create temp fallback file: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("secretstore: chmod temp fallback file: %w", err)
	}
	if _, err := tmp.Write(sealed); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("secretstore: write temp fallback file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("secretstore: sync temp fallback file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("secretstore: close temp fallback file: %w", err)
	}
	if err := renameFn(tmpName, s.path); err != nil {
		cleanup()
		return fmt.Errorf("secretstore: rename fallback file: %w", err)
	}
	return nil
}

// Set stores value under key, persisting to disk atomically.
func (s *fallbackStore) Set(key string, value []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	cp := make([]byte, len(value))
	copy(cp, value)
	s.data[key] = cp
	return s.persist()
}

// Get returns the value for key, or ErrNotFound if absent.
func (s *fallbackStore) Get(key string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	v, ok := s.data[key]
	if !ok {
		return nil, ErrNotFound
	}
	out := make([]byte, len(v))
	copy(out, v)
	return out, nil
}

// Delete removes key; deleting a missing key is a no-op.
func (s *fallbackStore) Delete(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.data[key]; !ok {
		return nil
	}
	delete(s.data, key)
	return s.persist()
}

// Close releases resources. Writes are flushed on every Set/Delete, so there
// is nothing to flush here.
func (s *fallbackStore) Close() error {
	return nil
}

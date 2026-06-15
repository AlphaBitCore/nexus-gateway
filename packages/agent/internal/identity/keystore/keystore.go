// Package keystore provides a platform-specific secret storage abstraction
// for retrieving device-bound encryption keys (e.g. for SQLCipher).
package keystore

import (
	"crypto/rand"
	"fmt"
	"sync"
)

// Store provides platform-specific secret storage.
type Store interface {
	// Get retrieves a secret by key name. Returns nil if not found.
	Get(key string) ([]byte, error)
	// Set stores a secret under the given key name.
	Set(key string, value []byte) error
	// Delete removes a secret by key name.
	Delete(key string) error
}

const dbKeyName = "nexus-agent-audit-db-key"
const dbKeyLen = 32 // AES-256

// AttestationKeyName is the platform-keystore label for the agent's
// Ed25519 attestation private key. Holding this key in the
// keystore — a macOS Keychain generic-password item, a Windows
// DPAPI-protected blob, or (Linux best-effort) a 0600 file — instead of a
// plaintext PEM on disk means a host/backup filesystem read no longer
// hands an attacker the signing key that, alone, forges traffic
// attestation and bypasses compliance inspection. The enrollment manager
// Sets it; the attestation signer Gets it.
const AttestationKeyName = "nexus-agent-attestation-key"

// memoryStore is an ephemeral in-process Store. It persists nothing across
// process restarts and provides no at-rest protection, so it is NOT a
// production custody backend — it exists so tests (and any future
// explicitly-ephemeral mode) can exercise the keystore-backed code paths
// without touching the real platform Keychain/DPAPI, which would be
// non-hermetic and could prompt for OS access.
type memoryStore struct {
	mu sync.RWMutex
	m  map[string][]byte
}

// NewMemoryStore returns an ephemeral in-process Store (see memoryStore).
func NewMemoryStore() Store {
	return &memoryStore{m: make(map[string][]byte)}
}

func (s *memoryStore) Get(key string) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.m[key]
	if !ok {
		return nil, nil
	}
	// Return a copy so a caller mutating the slice cannot corrupt the store.
	out := make([]byte, len(v))
	copy(out, v)
	return out, nil
}

func (s *memoryStore) Set(key string, value []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := make([]byte, len(value))
	copy(v, value)
	s.m[key] = v
	return nil
}

func (s *memoryStore) Delete(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, key)
	return nil
}

// GetOrCreateDBKey retrieves the audit DB encryption key from the platform
// keystore, generating a new random 32-byte key if none exists.
func GetOrCreateDBKey(store Store) ([]byte, error) {
	key, err := store.Get(dbKeyName)
	if err != nil {
		return nil, fmt.Errorf("keystore get: %w", err)
	}
	if key != nil {
		return key, nil
	}

	// Generate new random key
	key = make([]byte, dbKeyLen)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate db key: %w", err)
	}
	if err := store.Set(dbKeyName, key); err != nil {
		return nil, fmt.Errorf("keystore set: %w", err)
	}
	return key, nil
}

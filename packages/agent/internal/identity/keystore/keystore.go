// Package keystore provides a platform-specific secret storage abstraction
// for retrieving device-bound encryption keys (e.g. for SQLCipher).
package keystore

import (
	"crypto/rand"
	"fmt"
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

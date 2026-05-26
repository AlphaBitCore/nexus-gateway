// Package secretstore stores agent secrets (refresh tokens, session ids,
// clock offsets) using platform-native vaults where available, falling back
// to an encrypted file.
package secretstore

import "errors"

// ErrNotFound is returned by Get when the requested key does not exist.
var ErrNotFound = errors.New("secretstore: not found")

// Store is the platform-agnostic interface for secret persistence.
type Store interface {
	Set(key string, value []byte) error
	Get(key string) ([]byte, error)
	Delete(key string) error
	Close() error
}

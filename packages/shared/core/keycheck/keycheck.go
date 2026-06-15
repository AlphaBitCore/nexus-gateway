// Package keycheck provides a minimal entropy sanity gate for symmetric master
// keys loaded from the environment (CREDENTIAL_ENCRYPTION_KEY and friends).
//
// It does NOT replace a real KDF/HSM — it only refuses the keys that are never
// the product of `openssl rand`: all-zeros, a single repeated byte, or any of
// the committed example/dev keys whose byte set is tiny. A genuine 32-byte
// random key has ~32 distinct bytes; a degenerate key (<16 distinct) is a
// misconfiguration (a CI copied a fixed example into prod, an operator pasted a
// committed dev fallback) and must fail the service closed at boot rather than
// silently collapsing at-rest credential protection to a publicly-known value.
// The threshold is unconditional (dev and prod alike) because every
// legitimate dev key is now generated randomly (openssl / /dev/urandom).
package keycheck

import (
	"crypto/subtle"
	"errors"
	"fmt"
)

// minDistinctBytes is the floor for a 32-byte master key. A uniform random key
// effectively never lands below this; a value that does is a known/degenerate
// constant, not entropy.
const minDistinctBytes = 16

// ErrWeakMasterKey is returned by ValidateMasterKey for a degenerate key.
var ErrWeakMasterKey = errors.New("master key fails entropy sanity check (degenerate / known-constant)")

// ValidateMasterKey rejects an obviously weak symmetric master key: all-zeros
// or fewer than minDistinctBytes distinct byte values. Returns nil for any key
// with adequate byte diversity. Caller passes the already hex-decoded key.
func ValidateMasterKey(key []byte) error {
	if len(key) == 0 {
		return fmt.Errorf("%w: empty", ErrWeakMasterKey)
	}
	// All-zeros (constant-time, avoids leaking via early exit).
	zeros := make([]byte, len(key))
	if subtle.ConstantTimeCompare(key, zeros) == 1 {
		return fmt.Errorf("%w: all-zero", ErrWeakMasterKey)
	}
	var seen [256]bool
	distinct := 0
	for _, b := range key {
		if !seen[b] {
			seen[b] = true
			distinct++
		}
	}
	if distinct < minDistinctBytes {
		return fmt.Errorf("%w: only %d distinct bytes (min %d) — looks like a fixed/example key, not random",
			ErrWeakMasterKey, distinct, minDistinctBytes)
	}
	return nil
}

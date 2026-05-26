//go:build windows

package secretstore

import (
	"errors"
	"fmt"

	"github.com/danieljoos/wincred"
)

// winCred is the Windows Credential Manager-backed Store. Items are stored as
// generic credentials under the target name "<service>/<key>" and persisted
// with PersistLocalMachine, which keeps the credential available across logon
// sessions for the current user on this machine without requiring admin
// rights. Credential blobs are DPAPI-protected by the operating system.
type winCred struct {
	service string
}

// OpenWinCred opens the Windows Credential Manager backend, using the service
// identifier as the target-name prefix for each stored secret. Credentials are
// stored as generic credentials persisted to the local user profile.
func OpenWinCred(service string) (Store, error) {
	return &winCred{service: service}, nil
}

// targetName builds the "<service>/<key>" target name used to isolate entries
// that belong to this service from any other credentials in the vault.
func (w *winCred) targetName(key string) string {
	return fmt.Sprintf("%s/%s", w.service, key)
}

// Set stores value under key. wincred's Write is an upsert against the
// Windows Credential Manager, so there is no need to delete first.
func (w *winCred) Set(key string, value []byte) error {
	cred := wincred.NewGenericCredential(w.targetName(key))
	cred.CredentialBlob = value
	cred.Persist = wincred.PersistLocalMachine
	if err := cred.Write(); err != nil {
		return fmt.Errorf("secretstore: wincred write: %w", err)
	}
	return nil
}

// Get returns the value stored under key, or ErrNotFound if absent. The
// returned slice is a copy of the credential blob so mutations by the caller
// do not affect the underlying wincred structure.
func (w *winCred) Get(key string) ([]byte, error) {
	cred, err := wincred.GetGenericCredential(w.targetName(key))
	if err != nil {
		if errors.Is(err, wincred.ErrElementNotFound) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("secretstore: wincred read: %w", err)
	}
	out := make([]byte, len(cred.CredentialBlob))
	copy(out, cred.CredentialBlob)
	return out, nil
}

// Delete removes the entry for key. Deleting a missing key is a no-op, matching
// the fallback backend's semantics.
func (w *winCred) Delete(key string) error {
	cred, err := wincred.GetGenericCredential(w.targetName(key))
	if err != nil {
		if errors.Is(err, wincred.ErrElementNotFound) {
			return nil
		}
		return fmt.Errorf("secretstore: wincred read-for-delete: %w", err)
	}
	if err := cred.Delete(); err != nil {
		if errors.Is(err, wincred.ErrElementNotFound) {
			return nil
		}
		return fmt.Errorf("secretstore: wincred delete: %w", err)
	}
	return nil
}

// Close releases resources. The wincred API is stateless from our caller's
// perspective, so this is a no-op.
func (w *winCred) Close() error {
	return nil
}

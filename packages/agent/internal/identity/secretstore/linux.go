//go:build linux

package secretstore

import (
	"errors"
	"fmt"

	"github.com/zalando/go-keyring"
)

// linuxKeyring is the Linux Secret Service-backed Store. Items are stored via
// the D-Bus Secret Service API (libsecret, compatible with gnome-keyring and
// KWallet). The service identifier supplied to OpenSecret is used as the
// libsecret "service" attribute; the Store key is used as the "user" attribute.
//
// go-keyring exposes Set/Get as string-valued; we round-trip []byte through
// string. This is safe because the Store is used for OAuth refresh tokens
// (base64url printable ASCII) and other opaque token material that survives
// the UTF-8 string round-trip.
type linuxKeyring struct {
	service string
}

// OpenSecret opens the Linux Secret Service (libsecret via D-Bus) backend for
// the given service identifier. Each stored item uses the service as the
// libsecret "service" attribute and the Store key as the "user" attribute.
//
// OpenSecret probes the D-Bus session bus by issuing a benign Get against a
// non-existent key. A keyring.ErrNotFound from the probe confirms the bus is
// reachable; any other error indicates no session bus (headless host, missing
// DBUS_SESSION_BUS_ADDRESS) and is surfaced to the caller. Callers typically
// wrap this in Open(), which falls back to the encrypted-file backend.
func OpenSecret(service string) (Store, error) {
	if _, err := keyring.Get(service, "__nexus_probe__"); err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return nil, fmt.Errorf("secretstore: libsecret probe failed: %w", err)
	}
	return &linuxKeyring{service: service}, nil
}

// Set stores value under key. keyring.Set is an upsert in the Secret Service
// API (items with matching service+user attributes are replaced), so there is
// no need to delete first.
func (l *linuxKeyring) Set(key string, value []byte) error {
	if err := keyring.Set(l.service, key, string(value)); err != nil {
		return fmt.Errorf("secretstore: libsecret set: %w", err)
	}
	return nil
}

// Get returns the value stored under key, or ErrNotFound if absent.
func (l *linuxKeyring) Get(key string) ([]byte, error) {
	v, err := keyring.Get(l.service, key)
	if errors.Is(err, keyring.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("secretstore: libsecret get: %w", err)
	}
	return []byte(v), nil
}

// Delete removes the entry for key. Deleting a missing key is a no-op, matching
// the fallback backend's semantics.
func (l *linuxKeyring) Delete(key string) error {
	if err := keyring.Delete(l.service, key); err != nil {
		if errors.Is(err, keyring.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("secretstore: libsecret delete: %w", err)
	}
	return nil
}

// Close releases resources. go-keyring opens a fresh D-Bus connection per call
// and closes it internally, so this is a no-op.
func (l *linuxKeyring) Close() error {
	return nil
}

//go:build darwin

package secretstore

import (
	"errors"
	"fmt"

	"github.com/keybase/go-keychain"
)

// macKeychain is the macOS Keychain-backed Store. Items are stored as generic
// passwords under the configured service identifier, accessible only while the
// device is unlocked, and never synced across devices via iCloud.
type macKeychain struct {
	service string
}

// Package-level seams over the keychain library calls. Production code never
// reassigns these; tests substitute failing implementations to exercise the
// keychain-error branches that the real library cannot produce on a healthy
// host. Mirrors the pattern in fallback.go.
var (
	keychainAddItemFn    = keychain.AddItem
	keychainQueryItemFn  = keychain.QueryItem
	keychainDeleteItemFn = keychain.DeleteItem
)

// OpenKeychain opens the macOS Keychain backend for the given service
// identifier. Items are stored as generic passwords, accessible while the
// device is unlocked, and not synced across devices.
func OpenKeychain(service string) (Store, error) {
	return &macKeychain{service: service}, nil
}

// newItem builds a keychain.Item with the service/account pair and the
// device-unlocked, non-synchronizable attributes we use for every write.
func (m *macKeychain) newItem(key string, value []byte) keychain.Item {
	item := keychain.NewGenericPassword(m.service, key, "", value, "")
	item.SetSynchronizable(keychain.SynchronizableNo)
	item.SetAccessible(keychain.AccessibleWhenUnlockedThisDeviceOnly)
	return item
}

// Set stores value under key, upserting any existing entry by deleting first
// and then adding. The delete error is intentionally ignored because a missing
// prior entry is the common case.
func (m *macKeychain) Set(key string, value []byte) error {
	item := m.newItem(key, value)
	_ = keychainDeleteItemFn(item)
	if err := keychainAddItemFn(item); err != nil {
		return fmt.Errorf("secretstore: keychain add: %w", err)
	}
	return nil
}

// Get returns the value stored under key, or ErrNotFound if absent.
func (m *macKeychain) Get(key string) ([]byte, error) {
	query := keychain.NewItem()
	query.SetSecClass(keychain.SecClassGenericPassword)
	query.SetService(m.service)
	query.SetAccount(key)
	query.SetMatchLimit(keychain.MatchLimitOne)
	query.SetReturnData(true)

	results, err := keychainQueryItemFn(query)
	if err != nil {
		if errors.Is(err, keychain.ErrorItemNotFound) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("secretstore: keychain query: %w", err)
	}
	if len(results) == 0 {
		return nil, ErrNotFound
	}
	return results[0].Data, nil
}

// Delete removes the entry for key. Deleting a missing key is a no-op, matching
// the fallback backend's semantics.
func (m *macKeychain) Delete(key string) error {
	query := keychain.NewItem()
	query.SetSecClass(keychain.SecClassGenericPassword)
	query.SetService(m.service)
	query.SetAccount(key)

	if err := keychainDeleteItemFn(query); err != nil {
		if errors.Is(err, keychain.ErrorItemNotFound) {
			return nil
		}
		return fmt.Errorf("secretstore: keychain delete: %w", err)
	}
	return nil
}

// Close releases resources. The Keychain API is stateless from our caller's
// perspective, so this is a no-op.
func (m *macKeychain) Close() error {
	return nil
}

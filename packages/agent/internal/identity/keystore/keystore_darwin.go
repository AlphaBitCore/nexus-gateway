//go:build darwin

// Uses Security.framework via go-keychain rather than the /usr/bin/security
// CLI subprocess. The CLI variant required passing the password on argv
// (visible to ps for the lifetime of the subprocess), which leaked the
// SQLCipher DB key on multi-user macOS hosts. The framework path is also
// substantially faster (no subprocess fork per op).

package keystore

import (
	"errors"
	"fmt"

	"github.com/keybase/go-keychain"
)

const keychainService = "com.nexus-gateway.agent"

// DarwinKeychain stores values as generic passwords in the macOS Keychain
// using Security.framework directly.
type DarwinKeychain struct{}

// NewPlatformStore returns a macOS Keychain-backed Store.
func NewPlatformStore() Store {
	return &DarwinKeychain{}
}

// newItem builds a keychain.Item with the service/account pair and the
// device-unlocked, non-synchronizable attributes used for every write.
func (k *DarwinKeychain) newItem(key string, value []byte) keychain.Item {
	item := keychain.NewGenericPassword(keychainService, key, "", value, "")
	item.SetSynchronizable(keychain.SynchronizableNo)
	item.SetAccessible(keychain.AccessibleWhenUnlockedThisDeviceOnly)
	return item
}

func (k *DarwinKeychain) Get(key string) ([]byte, error) {
	query := keychain.NewItem()
	query.SetSecClass(keychain.SecClassGenericPassword)
	query.SetService(keychainService)
	query.SetAccount(key)
	query.SetMatchLimit(keychain.MatchLimitOne)
	query.SetReturnData(true)

	results, err := keychain.QueryItem(query)
	if err != nil {
		if errors.Is(err, keychain.ErrorItemNotFound) {
			return nil, nil // surface as cache miss, matching the prior contract
		}
		return nil, fmt.Errorf("keychain query: %w", err)
	}
	if len(results) == 0 {
		return nil, nil
	}
	return results[0].Data, nil
}

func (k *DarwinKeychain) Set(key string, value []byte) error {
	item := k.newItem(key, value)
	// Delete existing item first; missing-prior is the common case so the
	// error is intentionally ignored, matching the secretstore pattern.
	_ = keychain.DeleteItem(item)
	if err := keychain.AddItem(item); err != nil {
		return fmt.Errorf("keychain add: %w", err)
	}
	return nil
}

func (k *DarwinKeychain) Delete(key string) error {
	query := keychain.NewItem()
	query.SetSecClass(keychain.SecClassGenericPassword)
	query.SetService(keychainService)
	query.SetAccount(key)

	if err := keychain.DeleteItem(query); err != nil {
		if errors.Is(err, keychain.ErrorItemNotFound) {
			return nil
		}
		return fmt.Errorf("keychain delete: %w", err)
	}
	return nil
}

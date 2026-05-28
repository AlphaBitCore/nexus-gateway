package core

import (
	"errors"
	"fmt"

	"github.com/zalando/go-keyring"
)

// Secret keys stored per environment. These are the only secrets the toolkit
// holds; they never touch the on-disk config (NFR-1).
const (
	SecretAccessToken  = "access_token"
	SecretRefreshToken = "refresh_token"
	SecretAdminKey     = "admin_key"
	SecretVKSecret     = "vk_secret"
)

// keyringService is the OS-keychain service name under which all secrets live.
const keyringService = "nexus-cli"

// ErrSecretNotFound is returned by SecretStore.Get when no secret is stored for
// the (env, key) pair.
var ErrSecretNotFound = errors.New("secret not found")

// SecretStore persists per-environment secrets in the OS keychain. The account
// under keyringService is "<env>:<key>".
type SecretStore interface {
	Get(env, key string) (string, error)
	Set(env, key, val string) error
	Delete(env, key string) error
}

// account composes the keychain account for an (env, key) pair.
func account(env, key string) string { return env + ":" + key }

// KeyringStore is the production SecretStore backed by go-keyring (macOS
// Keychain / Linux libsecret / Windows Credential Manager).
type KeyringStore struct{}

// Get returns the stored secret, or ErrSecretNotFound when absent.
func (KeyringStore) Get(env, key string) (string, error) {
	v, err := keyring.Get(keyringService, account(env, key))
	if errors.Is(err, keyring.ErrNotFound) {
		return "", ErrSecretNotFound
	}
	if err != nil {
		return "", fmt.Errorf("keyring get %s/%s: %w", env, key, err)
	}
	return v, nil
}

// Set stores (or overwrites) the secret.
func (KeyringStore) Set(env, key, val string) error {
	if err := keyring.Set(keyringService, account(env, key), val); err != nil {
		return fmt.Errorf("keyring set %s/%s: %w", env, key, err)
	}
	return nil
}

// Delete removes the secret; deleting an absent secret is a no-op.
func (KeyringStore) Delete(env, key string) error {
	err := keyring.Delete(keyringService, account(env, key))
	if err == nil || errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	return fmt.Errorf("keyring delete %s/%s: %w", env, key, err)
}

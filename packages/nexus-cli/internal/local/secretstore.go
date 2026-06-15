// Package local holds the nexus-cli's local-machine implementations of the shared
// agent-core kernel seams — the OS-keychain SecretStore, the on-disk config file, and
// the interactive PKCE login. They live in the CLI module (not the shared
// nexus-agent-core kernel) so the kernel — and every server that embeds it, including
// the web assistant — does not carry the keychain / toml / browser-login
// dependencies it never uses. The web face supplies its own bearer-token source and
// has no local config file or keychain (M5).
package local

import (
	"errors"
	"fmt"

	"github.com/zalando/go-keyring"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/core"
)

// keyringService is the OS-keychain service name under which all secrets live.
const keyringService = "nexus-cli"

// account composes the keychain account for an (env, key) pair.
func account(env, key string) string { return env + ":" + key }

// KeyringStore is the production core.SecretStore backed by go-keyring (macOS
// Keychain / Linux libsecret / Windows Credential Manager). It satisfies
// core.SecretStore structurally; a missing secret maps to core.ErrSecretNotFound.
type KeyringStore struct{}

// Get returns the stored secret, or core.ErrSecretNotFound when absent.
func (KeyringStore) Get(env, key string) (string, error) {
	v, err := keyring.Get(keyringService, account(env, key))
	if errors.Is(err, keyring.ErrNotFound) {
		return "", core.ErrSecretNotFound
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

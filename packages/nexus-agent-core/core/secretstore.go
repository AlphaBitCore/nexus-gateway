package core

import (
	"errors"
)

// Secret keys stored per environment. These are the only secrets the toolkit
// holds; they never touch the on-disk config.
const (
	SecretAccessToken  = "access_token"
	SecretRefreshToken = "refresh_token"
	SecretAdminKey     = "admin_key"
	SecretVKSecret     = "vk_secret"
)

// ErrSecretNotFound is returned by SecretStore.Get when no secret is stored for
// the (env, key) pair.
var ErrSecretNotFound = errors.New("secret not found")

// SecretStore persists per-environment secrets. The production implementation is
// the OS-keychain-backed local.KeyringStore in the nexus-cli module; the kernel
// keeps only this interface so server embedders (the web assistant) are not
// forced to carry the keychain dependency (M5).
type SecretStore interface {
	Get(env, key string) (string, error)
	Set(env, key, val string) error
	Delete(env, key string) error
}

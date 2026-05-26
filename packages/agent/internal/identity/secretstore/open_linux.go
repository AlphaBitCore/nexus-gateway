//go:build linux

package secretstore

import "log/slog"

// linuxService is the libsecret "service" attribute used for the production
// agent's Secret Service entries. Tests use a distinct namespace so they
// never collide with real agent data.
const linuxService = "ai.nexus.agent"

// Open chooses the best backend for the current platform. On linux it prefers
// the Secret Service (libsecret via D-Bus, compatible with gnome-keyring and
// KWallet) and falls back to the HKDF+AES-GCM encrypted file when no session
// bus is available (e.g. headless hosts, systemd services without a user
// bus). A fallback event is logged at WARN level so ops can spot the posture
// downgrade.
//
// `fallbackPath` and `fallbackKey` are consumed only by OpenFallback; the key
// should be derived from the device private key via HKDF-SHA256 by the caller.
func Open(fallbackPath string, fallbackKey []byte) (Store, error) {
	s, err := OpenSecret(linuxService)
	if err == nil {
		return s, nil
	}
	slog.Default().Warn(
		"secretstore: libsecret unavailable, using encrypted file fallback",
		"err", err,
	)
	return OpenFallback(fallbackPath, fallbackKey)
}

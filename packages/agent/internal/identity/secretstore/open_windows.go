//go:build windows

package secretstore

import "log/slog"

// windowsService is the generic-credential target-name prefix used for the
// production agent's Windows Credential Manager entries. Tests use a distinct
// namespace so they never collide with real agent data.
const windowsService = "ai.nexus.agent"

// Open chooses the best backend for the current platform. On windows it prefers
// the Credential Manager (DPAPI-backed generic credentials) and falls back to
// the HKDF+AES-GCM encrypted file when the vault is unavailable (e.g. a
// service-account logon session without access to the user's vault). A
// fallback event is logged at WARN level so ops can spot the posture
// downgrade.
//
// `fallbackPath` and `fallbackKey` are consumed only by OpenFallback; the key
// should be derived from the device private key via HKDF-SHA256 by the caller.
func Open(fallbackPath string, fallbackKey []byte) (Store, error) {
	s, err := OpenWinCred(windowsService)
	if err == nil {
		return s, nil
	}
	slog.Default().Warn(
		"secretstore: wincred unavailable, using encrypted file fallback",
		"err", err,
	)
	return OpenFallback(fallbackPath, fallbackKey)
}

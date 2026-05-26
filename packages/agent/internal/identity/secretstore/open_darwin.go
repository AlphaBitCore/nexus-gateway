//go:build darwin

package secretstore

import "log/slog"

// darwinService is the generic-password service identifier used for the
// production agent's Keychain items. Tests use a distinct namespace so they
// never collide with real agent data.
const darwinService = "ai.nexus.agent"

// openKeychainFn is the seam for the platform-Keychain constructor used by
// Open(). Production code never reassigns it; tests substitute a failing
// implementation to exercise the file-fallback branch which would otherwise
// be unreachable (the real OpenKeychain never errors on darwin hosts).
var openKeychainFn = OpenKeychain

// Open chooses the best backend for the current platform. On darwin it prefers
// the Keychain backend and falls back to the HKDF+AES-GCM encrypted file when
// the Keychain is unavailable (e.g. headless macOS without a user session or a
// broken Keychain database). A fallback event is logged at WARN level so ops
// can spot the posture downgrade.
//
// `fallbackPath` and `fallbackKey` are consumed only by OpenFallback; the key
// should be derived from the device private key via HKDF-SHA256 by the caller.
func Open(fallbackPath string, fallbackKey []byte) (Store, error) {
	s, err := openKeychainFn(darwinService)
	if err == nil {
		return s, nil
	}
	slog.Default().Warn(
		"secretstore: keychain unavailable, using encrypted file fallback",
		"err", err,
	)
	return OpenFallback(fallbackPath, fallbackKey)
}

// Package auth holds the agent's on-disk enrollment credentials —
// the per-device token cookie that's exchanged during SSO enrollment
// and consumed on subsequent Hub calls. This is the surviving
// surface after a dead-code sweep retired the
// OAuth-from-shadow Bootstrap/TokenManager/Authenticator machinery
// (PR-0 deleted the receiver wiring; this follow-up deleted the
// rest of the never-wired-publisher-side helpers).
package auth

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// LoadDeviceToken reads the device token from certDir/device-token.
func LoadDeviceToken(certDir string) (string, error) {
	path := filepath.Join(certDir, "device-token")
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("load device token: %w (is agent enrolled?)", err)
	}
	token := strings.TrimSpace(string(data))
	if len(token) != 64 {
		return "", fmt.Errorf("load device token: invalid length %d (expected 64)", len(token))
	}
	return token, nil
}

// ClearEnrollment removes the on-disk credentials that mark this device
// as enrolled: device-token + thing-id. Used by the Dashboard's Sign
// Out flow — once these are gone, launchd-respawned agents will
// re-enter the onboarding flow on next launch. Missing files are not
// errors (already-cleared state is success).
//
// NOTE: the device client certificate + key are intentionally LEFT in
// place; they are bound to the org PKI and a stale enrollment that
// keeps them simplifies re-enrollment with the same machine identity.
// A full PKI scrub belongs in a separate "factory reset" surface.
func ClearEnrollment(certDir string) error {
	for _, name := range []string{"device-token", "thing-id"} {
		path := filepath.Join(certDir, name)
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("clear enrollment: remove %s: %w", name, err)
		}
	}
	return nil
}

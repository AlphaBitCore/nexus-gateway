// Package auth holds the agent's on-disk enrollment credentials —
// the per-device token cookie that's exchanged during SSO enrollment
// and consumed on subsequent Hub calls. This is the only credential
// surface: enrollment yields the device token, and there is no
// OAuth-from-shadow Bootstrap/TokenManager/Authenticator machinery.
package auth

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DeviceTokenExpiresFile is the on-disk file holding the RFC3339 expiry of the
// current device token. Written alongside `device-token` at enrollment and on
// every rotation; read by the renewal scheduler to decide when to rotate.
// Kept as a sibling file rather than embedded in the token so the
// token file stays a single opaque secret.
const DeviceTokenExpiresFile = "device-token-expires"

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

// LoadDeviceTokenExpiry reads the device token's expiry from
// certDir/device-token-expires. A missing or unparseable file returns the zero
// time and an error; callers treat that as "renew now" so a legacy enrollment
// without a recorded expiry self-heals on the next renewal tick rather than
// running on an unbounded token.
func LoadDeviceTokenExpiry(certDir string) (time.Time, error) {
	path := filepath.Join(certDir, DeviceTokenExpiresFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}, fmt.Errorf("load device token expiry: %w", err)
	}
	exp, err := time.Parse(time.RFC3339, strings.TrimSpace(string(data)))
	if err != nil {
		return time.Time{}, fmt.Errorf("load device token expiry: parse %q: %w", strings.TrimSpace(string(data)), err)
	}
	return exp, nil
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
	for _, name := range []string{"device-token", DeviceTokenExpiresFile, "thing-id"} {
		path := filepath.Join(certDir, name)
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("clear enrollment: remove %s: %w", name, err)
		}
	}
	return nil
}

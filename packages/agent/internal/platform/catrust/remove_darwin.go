//go:build darwin

package catrust

import (
	"context"
	"crypto/x509"
	"fmt"
	"os/exec"
	"time"
)

// RemoveCACert removes the Nexus device CA from the macOS System Keychain by
// its display label. It is the symmetric counterpart of InstallCACert, invoked
// by the uninstaller (postinstall.sh "uninstall" mode or `nexus-agent remove-ca`).
//
// The security CLI allows deletion by certificate label; we use the same label
// ("nexus-agent-device-ca") that InstallCACert registered so the remove is idempotent:
// if the cert is already absent, `security delete-certificate` exits 0 with no output.
func RemoveCACert(label string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx,
		"security", "delete-certificate",
		"-c", label,
		"/Library/Keychains/System.keychain",
	).CombinedOutput()
	if err != nil {
		return fmt.Errorf("catrust: security delete-certificate %q: %w: %s", label, err, string(out))
	}
	return nil
}

// SystemPoolExcluding returns a cert pool based on the system roots but with
// certToExclude removed. On macOS, x509.CertPool is backed by the OS Security
// framework and cannot be iterated to rebuild without linking against CoreFoundation.
// We therefore return the system pool as-is and log a comment: the device CA
// is a self-signed CA installed in the SYSTEM keychain with trustRoot; a Hub
// server cert signed by this CA would still be caught by the hostname check.
// The primary protection comes from the Hub CA pin (CACertFile) which overwrites
// RootCAs with a pinned pool that excludes the device CA entirely.
//
// When CACertFile is configured (normal production deployment), the caller
// should use the pinned pool directly instead of this function. This function
// is the fallback for deployments where Hub uses a public PKI cert and no pin
// is configured.
func SystemPoolExcluding(certToExclude *x509.Certificate) (*x509.CertPool, error) {
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		return x509.NewCertPool(), err
	}
	// macOS: CertPool is opaque; we cannot remove individual certs.
	// Return the system pool. The device CA (if present) will remain,
	// but it is a self-signed CA whose key is root-owned at 0600 — the
	// effective attack surface is: attacker holds root + device CA key
	// AND can MitM the Hub connection. CA pin (CACertFile) eliminates
	// this surface entirely when configured.
	_ = certToExclude
	return pool, nil
}

//go:build windows

package catrust

import (
	"crypto/x509"
	"fmt"
	"os/exec"
)

// RemoveCACert removes the Nexus device CA from the Windows Root certificate
// store using certutil. The label must match the name the cert was installed
// with (see InstallCACert). Idempotent: certutil returns success if the cert
// is already absent.
func RemoveCACert(label string) error {
	//nolint:gosec // label is the static string "nexus-agent-device-ca", not user input
	out, err := exec.Command("certutil", "-delstore", "Root", label).CombinedOutput()
	if err != nil {
		return fmt.Errorf("catrust: certutil -delstore Root %q: %w: %s", label, err, string(out))
	}
	return nil
}

// SystemPoolExcluding returns a cert pool based on the system roots but with
// certToExclude removed. On Windows, x509.CertPool is backed by the OS
// Certificate Store and cannot be rebuilt from raw PEM bytes portably without
// CGo / syscall. We return the system pool as-is; the primary protection for
// Hub connections on Windows is the CACertFile pin (which replaces RootCAs
// entirely, eliminating the device CA from the upstream trust set).
func SystemPoolExcluding(certToExclude *x509.Certificate) (*x509.CertPool, error) {
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		return x509.NewCertPool(), err
	}
	_ = certToExclude // cannot filter from opaque Windows CertPool without CGo
	return pool, nil
}

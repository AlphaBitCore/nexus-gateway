//go:build linux

package catrust

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"os/exec"
	"time"
)

// linuxCACertBundles lists the known CA bundle paths in order of preference.
// We try each in turn and use the first that exists on this distro.
var linuxCACertBundles = []string{
	"/etc/ssl/certs/ca-certificates.crt",                // Debian / Ubuntu / Kali / Alpine
	"/etc/pki/tls/certs/ca-bundle.crt",                  // RHEL / CentOS / Fedora
	"/etc/ssl/ca-bundle.pem",                            // OpenSUSE
	"/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem", // RHEL with update-ca-trust
}

// RemoveCACert removes the Nexus device CA cert file from the distro-specific
// trust store directory and refreshes the OS bundle. It is the symmetric
// counterpart of InstallCACert, invoked by the uninstaller script.
// Passing the same label ("nexus-agent-device-ca") that InstallCACert used
// makes this idempotent: if the cert is already absent, the function returns nil.
func RemoveCACert(label string) error {
	candidates := []struct {
		dir string
		cmd []string
	}{
		{"/usr/local/share/ca-certificates", []string{"update-ca-certificates"}},
		{"/etc/pki/ca-trust/source/anchors", []string{"update-ca-trust", "extract"}},
		{"/etc/ca-certificates/trust-source/anchors", []string{"update-ca-trust"}},
	}

	for _, c := range candidates {
		certFile := c.dir + "/" + label + ".crt"
		if _, err := os.Stat(certFile); os.IsNotExist(err) {
			continue
		}
		if err := os.Remove(certFile); err != nil {
			return fmt.Errorf("catrust: remove %s: %w", certFile, err)
		}
		if len(c.cmd) > 0 {
			// Bound the trust-store refresh so a wedged update-ca-* binary cannot
			// hang the uninstaller. Mirrors the darwin RemoveCACert timeout.
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			//nolint:gosec // args are static strings, not user input
			out, err := exec.CommandContext(ctx, c.cmd[0], c.cmd[1:]...).CombinedOutput()
			cancel()
			if err != nil {
				return fmt.Errorf("catrust: %s: %w: %s", c.cmd[0], err, string(out))
			}
		}
		return nil
	}
	// Cert file not found in any known location — treat as already removed.
	return nil
}

// SystemPoolExcluding returns an x509.CertPool containing the system roots
// but with certToExclude omitted. On Linux the CA bundle is a concatenated PEM
// file; we rebuild the pool PEM-block by PEM-block, skipping any block whose
// DER bytes match certToExclude.Raw. This ensures the Nexus device CA, which
// install-ca adds to the system trust store, is NOT trusted for the upstream
// Hub connection — preventing a compromised device CA from being used to forge
// Hub server certificates.
func SystemPoolExcluding(certToExclude *x509.Certificate) (*x509.CertPool, error) {
	pool := x509.NewCertPool()
	excludeRaw := certToExclude.Raw

	for _, bundlePath := range linuxCACertBundles {
		data, err := os.ReadFile(bundlePath)
		if err != nil {
			continue // try next candidate
		}
		rest := data
		for len(rest) > 0 {
			var block *pem.Block
			block, rest = pem.Decode(rest)
			if block == nil {
				break
			}
			if block.Type != "CERTIFICATE" {
				continue
			}
			// Skip the cert we're excluding.
			if bytes.Equal(block.Bytes, excludeRaw) {
				continue
			}
			cert, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				continue // skip malformed certs gracefully
			}
			pool.AddCert(cert)
		}
		// Successfully parsed a bundle — stop searching.
		return pool, nil
	}

	// No bundle file found — fall back to the opaque system pool (best effort).
	// On failure we MUST surface the error: the caller (hub/client.go) treats a
	// returned error as "use the unfiltered system pool", which is the safe
	// fallback. Returning (emptyPool, nil) instead would silently hand the
	// caller a pool that trusts NO roots, breaking every upstream Hub TLS
	// handshake. An empty pool is never an acceptable success result here.
	sysPool, err := x509.SystemCertPool()
	if err != nil {
		return nil, fmt.Errorf("catrust: load system cert pool: %w", err)
	}
	if sysPool == nil {
		return nil, fmt.Errorf("catrust: system cert pool is empty")
	}
	return sysPool, nil
}

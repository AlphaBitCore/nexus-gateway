//go:build linux

package catrust

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// trustCandidate is one distro's CA anchor directory plus the refresh
// command that rebuilds the system trust bundle from it. A nil cmd means
// no post-write command is needed.
type trustCandidate struct {
	dir string
	cmd []string
}

// linuxTrustCandidates lists the known anchor dir + refresh command per
// distro family, in priority order. Probed first-match-wins by
// selectTrustCandidate.
func linuxTrustCandidates() []trustCandidate {
	return []trustCandidate{
		// Debian/Ubuntu/Kali
		{dir: "/usr/local/share/ca-certificates", cmd: []string{"update-ca-certificates"}},
		// RHEL/Fedora/CentOS/Amazon Linux
		{dir: "/etc/pki/ca-trust/source/anchors", cmd: []string{"update-ca-trust", "extract"}},
		// Arch Linux
		{dir: "/etc/ca-certificates/trust-source/anchors", cmd: []string{"update-ca-trust"}},
		// Alpine
		{dir: "/usr/local/share/ca-certificates", cmd: []string{"update-ca-certificates"}},
	}
}

// selectTrustCandidate returns the first candidate whose anchor directory
// exists according to dirExists. ok=false means no known trust store was
// found on this host. Split out (with an injectable dirExists) so the
// distro-detection logic is unit-testable against temp dirs without root.
func selectTrustCandidate(candidates []trustCandidate, dirExists func(string) bool) (trustCandidate, bool) {
	for _, c := range candidates {
		if dirExists(c.dir) {
			return c, true
		}
	}
	return trustCandidate{}, false
}

// writeCAToTrustDir writes certPEM into dir as "<label>.crt" (0644) and
// returns the path written. Separated from the refresh exec so tests can
// assert the file placement without invoking the privileged update tool.
func writeCAToTrustDir(dir, label string, certPEM []byte) (string, error) {
	destPath := filepath.Join(dir, label+".crt")
	if err := os.WriteFile(destPath, certPEM, 0o644); err != nil {
		return "", fmt.Errorf("catrust: write cert to %s: %w", destPath, err)
	}
	return destPath, nil
}

// dirExists reports whether path is an existing directory.
func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// InstallCACert installs the given PEM-encoded CA certificate into the OS
// trust store on Linux. It auto-detects the distro layout (Debian/Ubuntu,
// RHEL/Fedora/Amazon Linux, Arch, Alpine), writes the cert into the right
// anchor directory, and runs that distro's refresh command so every
// userspace HTTPS client on the host trusts intercepted TLS. Requires
// root (the anchor dirs + refresh tools are root-only).
func InstallCACert(certPEM []byte, label string) error {
	c, ok := selectTrustCandidate(linuxTrustCandidates(), dirExists)
	if !ok {
		return fmt.Errorf("catrust: no known CA certificate directory found on this Linux distro")
	}
	if _, err := writeCAToTrustDir(c.dir, label, certPEM); err != nil {
		return err
	}
	if len(c.cmd) > 0 {
		//nolint:gosec // args are static strings, not user input
		out, err := exec.CommandContext(context.Background(), c.cmd[0], c.cmd[1:]...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("catrust: %s: %w: %s", c.cmd[0], err, string(out))
		}
	}
	return nil
}

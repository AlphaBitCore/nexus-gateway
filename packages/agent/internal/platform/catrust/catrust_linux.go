//go:build linux

package catrust

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// InstallCACert installs the given PEM-encoded CA certificate into the OS
// trust store on Linux. It tries the common distro-specific paths/commands
// and logs a best-effort warning on failure rather than aborting startup.
func InstallCACert(certPEM []byte, label string) error {
	// Determine the CA bundle directory and update command for this distro.
	type candidate struct {
		dir string
		cmd []string // nil means no post-install command needed
	}
	candidates := []candidate{
		// Debian/Ubuntu/Kali
		{dir: "/usr/local/share/ca-certificates", cmd: []string{"update-ca-certificates"}},
		// RHEL/Fedora/CentOS/Amazon Linux
		{dir: "/etc/pki/ca-trust/source/anchors", cmd: []string{"update-ca-trust", "extract"}},
		// Arch Linux
		{dir: "/etc/ca-certificates/trust-source/anchors", cmd: []string{"update-ca-trust"}},
		// Alpine
		{dir: "/usr/local/share/ca-certificates", cmd: []string{"update-ca-certificates"}},
	}

	for _, c := range candidates {
		if _, err := os.Stat(c.dir); os.IsNotExist(err) {
			continue
		}
		destPath := filepath.Join(c.dir, label+".crt")
		if err := os.WriteFile(destPath, certPEM, 0o644); err != nil {
			return fmt.Errorf("catrust: write cert to %s: %w", destPath, err)
		}
		if len(c.cmd) > 0 {
			//nolint:gosec // args are static strings, not user input
			out, err := exec.Command(c.cmd[0], c.cmd[1:]...).CombinedOutput()
			if err != nil {
				return fmt.Errorf("catrust: %s: %w: %s", c.cmd[0], err, string(out))
			}
		}
		return nil
	}
	return fmt.Errorf("catrust: no known CA certificate directory found on this Linux distro")
}

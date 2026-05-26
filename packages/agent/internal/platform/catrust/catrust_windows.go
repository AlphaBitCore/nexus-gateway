//go:build windows

package catrust

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// InstallCACert installs the given PEM-encoded CA certificate into the Windows
// Root certificate store using certutil. A temporary file is used to avoid
// passing PEM bytes via command line arguments.
func InstallCACert(certPEM []byte, label string) error {
	tmp, err := os.CreateTemp("", label+"-*.crt")
	if err != nil {
		return fmt.Errorf("catrust: create temp cert file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) //nolint:errcheck

	if _, err := tmp.Write(certPEM); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("catrust: write temp cert: %w", err)
	}
	_ = tmp.Close()

	// certutil -addstore "Root" <cert.pem> installs to the LocalMachine Root store.
	out, err := exec.Command("certutil", "-addstore", "-f", "Root", filepath.Clean(tmpPath)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("catrust: certutil -addstore: %w: %s", err, string(out))
	}
	return nil
}

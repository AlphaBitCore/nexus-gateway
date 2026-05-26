//go:build darwin

package catrust

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// InstallCACert installs the given PEM-encoded CA certificate into the macOS
// System keychain as a trusted root using the security CLI tool.
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

	// security add-trusted-cert -d (system keychain) -r trustRoot -k /Library/Keychains/System.keychain
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx,
		"security", "add-trusted-cert",
		"-d", "-r", "trustRoot",
		"-k", "/Library/Keychains/System.keychain",
		filepath.Clean(tmpPath),
	).CombinedOutput()
	if err != nil {
		return fmt.Errorf("catrust: security add-trusted-cert: %w: %s", err, string(out))
	}
	return nil
}

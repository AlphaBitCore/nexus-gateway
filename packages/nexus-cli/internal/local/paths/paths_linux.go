//go:build linux

package paths

import (
	"fmt"
	"os"
	"path/filepath"
)

// Linux per-user logs follow the XDG Base Directory Specification: state data
// that should persist but is not config (logs, history) lives under
// $XDG_STATE_HOME, falling back to ~/.local/state when the variable is unset.
// The CLI writes to $XDG_STATE_HOME/nexus (or ~/.local/state/nexus).
func userLogDir() (string, error) {
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		return filepath.Join(dir, "nexus"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, ".local", "state", "nexus"), nil
}

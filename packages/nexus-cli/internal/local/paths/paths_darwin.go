//go:build darwin

package paths

import (
	"fmt"
	"os"
	"path/filepath"
)

// macOS per-user logs follow Apple's File System Programming Guide: user-visible
// application logs live under ~/Library/Logs/ in a directory named for the app.
// The CLI uses ~/Library/Logs/nexus so an operator (or `Console.app`) can find
// nexus-cli.log without root.
func userLogDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, "Library", "Logs", "nexus"), nil
}

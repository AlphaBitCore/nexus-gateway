//go:build windows

package paths

import (
	"fmt"
	"os"
	"path/filepath"
)

// Windows per-user logs follow the Application Data conventions: machine-local
// (non-roaming) application data lives under %LocalAppData%. Logs should not
// roam between machines, so the CLI writes to %LocalAppData%\nexus\Logs. When
// LocalAppData is unset (rare), fall back to os.UserCacheDir(), which resolves
// to the same LocalAppData root on Windows.
func userLogDir() (string, error) {
	if dir := os.Getenv("LocalAppData"); dir != "" {
		return filepath.Join(dir, "nexus", "Logs"), nil
	}
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("resolve user cache dir: %w", err)
	}
	return filepath.Join(cacheDir, "nexus", "Logs"), nil
}

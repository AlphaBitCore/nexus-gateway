package runtime

import (
	"fmt"
	"os"
	"path/filepath"
)

// The default on-disk locations for the agent's per-operator state, all under
// ~/.config/nexus — one place to resolve every nexus config path.

// DefaultMemoryDir is ~/.config/nexus/memory — the base of the agent's learning
// memory. The store splits it into global/ (operator preferences + procedures, which
// apply everywhere) and <env>/ (baselines + named entities, which are per-env) so a
// prod session never recalls a local stack's baselines while still carrying the
// operator's global preferences.
func DefaultMemoryDir() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve user config dir: %w", err)
	}
	return filepath.Join(dir, "nexus", "memory"), nil
}

// DefaultSessionDir is ~/.config/nexus/sessions/<env> — where the agent persists
// resumable conversation sessions, partitioned per environment for the same
// isolation reason as the memory file.
func DefaultSessionDir(env string) (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve user config dir: %w", err)
	}
	return filepath.Join(dir, "nexus", "sessions", env), nil
}

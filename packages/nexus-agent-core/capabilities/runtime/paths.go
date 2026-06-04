package runtime

import (
	"fmt"
	"os"
	"path/filepath"
)

// The default on-disk locations for the agent's per-operator state, all under
// ~/.config/nexus. They live in the capabilities root (not the skills sub-package)
// because the cli wires the memory + session dirs alongside the skill dir when it
// builds an agent — one place to resolve every nexus config path.

// DefaultSkillDir is ~/.config/nexus/skills.
func DefaultSkillDir() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve user config dir: %w", err)
	}
	return filepath.Join(dir, "nexus", "skills"), nil
}

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

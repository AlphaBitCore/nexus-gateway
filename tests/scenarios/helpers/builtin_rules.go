package helpers

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

// ParseGoBuiltinRuleIDs extracts every `ID: "..."` literal from
// packages/nexus-hub/internal/alerts/engine/rules/builtin.go. The unused
// repoRoot argument is accepted for future flexibility; today we walk
// up from CWD looking for go.work so the helper works whether tests
// run from tests/scenarios/ or the repo root.
func ParseGoBuiltinRuleIDs(_ string) ([]string, error) {
	root, err := findRepoRoot()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(root, "packages", "nexus-hub", "internal", "alerting", "rules", "builtin.go")
	buf, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	re := regexp.MustCompile(`(?m)^\s+ID:\s*"([^"]+)"`)
	matches := re.FindAllStringSubmatch(string(buf), -1)
	ids := make([]string, 0, len(matches))
	for _, m := range matches {
		ids = append(ids, m[1])
	}
	return ids, nil
}

func findRepoRoot() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	dir := cwd
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", fmt.Errorf("repo root (go.work) not found from %s", cwd)
}

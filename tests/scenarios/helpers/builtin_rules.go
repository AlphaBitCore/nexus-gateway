package helpers

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
)

// ParseGoBuiltinRuleIDs extracts every `ID: "..."` literal from the Hub's
// builtin alert-rule table. The unused repoRoot argument is accepted for future
// flexibility; today we walk up from CWD looking for go.work so the helper
// works whether tests run from tests/scenarios/ or the repo root.
//
// The file is LOCATED rather than hardcoded. tests/scenarios is its own module
// and cannot import a nexus-hub internal/ package, so reading the source is the
// only way to get these ids — but a hardcoded path is silently invalidated by
// any package rename, and that is exactly what happened: the package moved from
// internal/alerting/rules to internal/alerts/engine/rules, this helper's doc
// comment was updated to the new path, and the filepath.Join one line below it
// was not. Nothing enforced the comment, so S-091 failed on a missing file
// instead of on a seed drift. Locating the file survives the next rename and
// still fails loudly if it genuinely disappears.
func ParseGoBuiltinRuleIDs(_ string) ([]string, error) {
	root, err := findRepoRoot()
	if err != nil {
		return nil, err
	}
	path, err := findBuiltinRulesFile(root)
	if err != nil {
		return nil, err
	}
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

// findBuiltinRulesFile locates the single `rules/builtin.go` under the Hub
// package. Ambiguity is an error rather than a guess: two candidates would mean
// the caller cannot know which table it parsed, and a silently-picked wrong one
// would turn this lockstep check into a comparison against the wrong set.
func findBuiltinRulesFile(root string) (string, error) {
	hub := filepath.Join(root, "packages", "nexus-hub")
	var found []string
	err := filepath.WalkDir(hub, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && d.Name() == "builtin.go" && filepath.Base(filepath.Dir(path)) == "rules" {
			found = append(found, path)
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("walk %s: %w", hub, err)
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return "", fmt.Errorf("no rules/builtin.go under %s — the builtin alert-rule table moved or was deleted", hub)
	default:
		return "", fmt.Errorf("ambiguous rules/builtin.go under %s: %v", hub, found)
	}
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

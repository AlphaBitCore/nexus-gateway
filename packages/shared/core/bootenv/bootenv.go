// Package bootenv loads .env files at process startup so local development
// gets the same env-var contract production gets from systemd EnvironmentFile
// or Kubernetes Secrets — without the application itself having to know about
// the file path.
//
// Design constraints:
//
//   - Application code reads secrets via os.Getenv ONLY. The .env file is a
//     boot-time convenience; it never appears in the read path.
//   - Variables already present in the process environment win. godotenv's
//     non-overload Load() is used so a systemd EnvironmentFile= or a
//     `MY_VAR=x ./svc` invocation always trumps .env. The same rule lets a
//     developer override one variable on the command line without editing
//     the shared .env.
//   - Loader is silent when .env is absent. Production typically has no
//     .env file (env vars come from systemd / K8s); silence keeps the
//     boot log clean. Errors parsing an existing .env are surfaced via
//     the supplied logger.
//   - Repo-root discovery walks up from cwd looking for the `.git/` marker.
//     Each of the four services (Hub / CP / AI Gateway / Compliance Proxy)
//     runs from its own packages/<svc>/ working directory; find-up locates
//     the shared repo-root .env regardless of which package the binary was
//     launched from.
package bootenv

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/joho/godotenv"
)

// repoRootMarker is the directory entry whose presence identifies the
// repository root. `.git` is checked as either a file or directory because
// `git worktree add` creates it as a file rather than a directory.
// Stops at filesystem root.
const repoRootMarker = ".git"

// FindRepoRoot walks up from start (or cwd if empty) until it finds a
// directory containing the repoRootMarker. Returns ("", false) when the walk
// hits the filesystem root without a match.
//
// Exported for tests and for callers that need to anchor paths to repo root
// for reasons other than .env loading (e.g. fixtures).
func FindRepoRoot(start string) (string, bool) {
	if start == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", false
		}
		start = cwd
	}
	dir := start
	for {
		if _, err := os.Stat(filepath.Join(dir, repoRootMarker)); err == nil {
			return dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false // hit filesystem root
		}
		dir = parent
	}
}

// LoadFromRepoRoot finds the repo root from cwd and loads <root>/.env into
// the process environment if the file exists. Existing env vars are NOT
// overridden — the contract is "boot defaults", not "force override".
//
// Returns the absolute path that was loaded (or "") and an error if the file
// existed but failed to parse. A missing .env returns ("", nil).
//
// `logger` may be nil; when present, the loader emits one INFO line on
// success (with the path) so boot logs make the configuration source
// obvious.
func LoadFromRepoRoot(logger *slog.Logger) (string, error) {
	root, ok := FindRepoRoot("")
	if !ok {
		return "", nil // running outside a git checkout; nothing to do
	}
	return LoadFile(filepath.Join(root, ".env"), logger)
}

// LoadFile loads a specific .env file. Exists for tests and for callers that
// need to point at a non-default path. Missing-file is not an error.
func LoadFile(path string, logger *slog.Logger) (string, error) {
	if path == "" {
		return "", nil
	}
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("bootenv stat %q: %w", path, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("bootenv: %q is a directory, expected a .env file", path)
	}
	// godotenv.Load (non-overload) leaves existing env vars untouched.
	// This makes systemd EnvironmentFile= / docker --env-file / shell
	// `VAR=x ./svc` always win over the file's defaults.
	if err := godotenv.Load(path); err != nil {
		return "", fmt.Errorf("bootenv load %q: %w", path, err)
	}
	if logger != nil {
		logger.Info("bootenv: loaded defaults from .env",
			"path", path,
			"note", "process env vars override file values")
	}
	return path, nil
}

package bootenv

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// helper: build an isolated fake repo dir layout for each test.
func tempRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	return root
}

func TestFindRepoRoot_FromNestedSubdir(t *testing.T) {
	root := tempRepo(t)
	nested := filepath.Join(root, "packages", "svc", "cmd", "svc")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	got, ok := FindRepoRoot(nested)
	if !ok {
		t.Fatalf("expected to find repo root from %s", nested)
	}
	if got != root {
		t.Errorf("FindRepoRoot = %q, want %q", got, root)
	}
}

func TestFindRepoRoot_NoMarkerReturnsFalse(t *testing.T) {
	// t.TempDir without an explicit .git marker.
	dir := t.TempDir()
	got, ok := FindRepoRoot(dir)
	if ok {
		t.Errorf("expected no repo root, got %q", got)
	}
}

func TestLoadFile_MissingFileIsSilent(t *testing.T) {
	path, err := LoadFile(filepath.Join(t.TempDir(), "does-not-exist.env"), nil)
	if err != nil {
		t.Errorf("missing file should not error, got %v", err)
	}
	if path != "" {
		t.Errorf("path on missing file = %q, want \"\"", path)
	}
}

func TestLoadFile_RejectsDirectoryAtPath(t *testing.T) {
	dir := t.TempDir()
	_, err := LoadFile(dir, nil)
	if err == nil {
		t.Errorf("expected error when path is a directory")
	}
}

func TestLoadFile_ExistingEnvWinsOverFile(t *testing.T) {
	// Create a .env with two keys.
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	contents := "BOOTENV_TEST_NEW=from-file\nBOOTENV_TEST_PRESET=from-file\n"
	if err := os.WriteFile(envPath, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}

	// Preset one var in the process env BEFORE Load — file value must
	// NOT override it.
	t.Setenv("BOOTENV_TEST_PRESET", "from-process")
	// Make sure the other var is unset so we can observe it loading.
	_ = os.Unsetenv("BOOTENV_TEST_NEW")
	t.Cleanup(func() { _ = os.Unsetenv("BOOTENV_TEST_NEW") })

	got, err := LoadFile(envPath, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got != envPath {
		t.Errorf("returned path = %q, want %q", got, envPath)
	}
	if v := os.Getenv("BOOTENV_TEST_PRESET"); v != "from-process" {
		t.Errorf("existing env var should win, got %q", v)
	}
	if v := os.Getenv("BOOTENV_TEST_NEW"); v != "from-file" {
		t.Errorf("file value should fill in for absent var, got %q", v)
	}
}

func TestLoadFile_MalformedReturnsError(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	// Missing = sign on a non-comment line; godotenv rejects this.
	if err := os.WriteFile(envPath, []byte("THIS_IS_NOT_VALID_KEY_VALUE\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadFile(envPath, nil)
	if err == nil {
		t.Errorf("expected parse error on malformed .env")
	}
}

func TestLoadFromRepoRoot_NoEnvFileIsSilent(t *testing.T) {
	// Make a fake repo with no .env. cd into it so FindRepoRoot succeeds
	// but LoadFile gets ENOENT.
	root := tempRepo(t)
	t.Chdir(root)
	path, err := LoadFromRepoRoot(nil)
	if err != nil {
		t.Errorf("missing .env in repo should not error, got %v", err)
	}
	if path != "" {
		t.Errorf("path on missing .env = %q, want \"\"", path)
	}
}

func TestLoadFile_StatErrorOtherThanMissing(t *testing.T) {
	// Stat returns ENOTDIR when a path component is a regular file. This
	// is a non-ENOENT stat error — the loader must surface it rather than
	// silently skip.
	dir := t.TempDir()
	regular := filepath.Join(dir, "blocker")
	if err := os.WriteFile(regular, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	pathThroughFile := filepath.Join(regular, "deeper.env")
	_, err := LoadFile(pathThroughFile, nil)
	if err == nil {
		t.Errorf("expected stat error on path through a regular file")
	}
}

func TestLoadFile_EmptyPathSilent(t *testing.T) {
	path, err := LoadFile("", nil)
	if err != nil || path != "" {
		t.Errorf("empty path = (%q,%v), want (\"\",nil)", path, err)
	}
}

func TestLoadFile_LoggerEmitsOneLineOnSuccess(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	if err := os.WriteFile(envPath, []byte("BOOTENV_LOG_TEST=x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Unsetenv("BOOTENV_LOG_TEST") })
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	if _, err := LoadFile(envPath, logger); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !strings.Contains(buf.String(), "bootenv: loaded defaults") {
		t.Errorf("expected success log line, got: %s", buf.String())
	}
}

func TestLoadFromRepoRoot_OutsideRepoIsSilent(t *testing.T) {
	// cd to a directory that has no .git ancestor (t.TempDir is created
	// under the OS temp root, which on macOS lives outside any repo).
	// LoadFromRepoRoot must early-return without error.
	dir := t.TempDir()
	t.Chdir(dir)
	path, err := LoadFromRepoRoot(nil)
	if err != nil {
		t.Errorf("outside repo should not error, got %v", err)
	}
	if path != "" {
		t.Errorf("path outside repo = %q, want \"\"", path)
	}
}

func TestLoadFromRepoRoot_LoadsRepoRootEnvFile(t *testing.T) {
	root := tempRepo(t)
	envPath := filepath.Join(root, ".env")
	if err := os.WriteFile(envPath,
		[]byte("BOOTENV_LFR_TEST=loaded\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Run from a nested cwd to ensure find-up actually walks.
	nested := filepath.Join(root, "packages", "svc")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(nested)
	_ = os.Unsetenv("BOOTENV_LFR_TEST")
	t.Cleanup(func() { _ = os.Unsetenv("BOOTENV_LFR_TEST") })

	got, err := LoadFromRepoRoot(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got != envPath {
		t.Errorf("LoadFromRepoRoot path = %q, want %q", got, envPath)
	}
	if v := os.Getenv("BOOTENV_LFR_TEST"); v != "loaded" {
		t.Errorf("value not loaded, got %q", v)
	}
}

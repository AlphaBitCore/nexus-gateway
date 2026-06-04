package cli

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/local"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/local/paths"
)

// TestEnsureConfig_WiresLogger drives ensureConfig with HOME pointed at a temp
// dir so the diagnostic log lands under the test's sandbox, then asserts:
//   - a.Log is built and the default HTTP client wraps the LoggingTransport,
//   - the log file exists with the "cli session start" line, and
//   - Close releases the file.
//
// This is the integration of every piece (paths → OpenLogger → LoggingTransport
// → session banner). It skips on Windows, where HOME does not redirect the
// AppData-based paths.
func TestEnsureConfig_WiresLogger(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("HOME does not redirect %AppData%/%LocalAppData% on windows")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "cfg"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))

	a := &App{Store: fakeStore{m: map[string]string{}}}
	if err := a.ensureConfig(); err != nil {
		t.Fatalf("ensureConfig: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })

	if a.Log == nil {
		t.Fatal("a.Log is nil after ensureConfig")
	}
	// The default client must carry the LoggingTransport over the kernel's
	// widened transport.
	lt, ok := a.HTTP.Transport.(*local.LoggingTransport)
	if !ok {
		t.Fatalf("a.HTTP.Transport = %T, want *local.LoggingTransport", a.HTTP.Transport)
	}
	if lt.Base == nil {
		t.Error("LoggingTransport.Base is nil; want the kernel widened transport")
	}

	// The session-start line landed in the file at the resolved log path.
	p, err := paths.DefaultPaths()
	if err != nil {
		t.Fatalf("DefaultPaths: %v", err)
	}
	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	data, err := os.ReadFile(p.LogFile)
	if err != nil {
		t.Fatalf("read log file %s: %v", p.LogFile, err)
	}
	if !bytes.Contains(data, []byte("cli session start")) {
		t.Errorf("log missing session-start line; got %q", data)
	}
	if !bytes.Contains(data, []byte("log_file=")) {
		t.Errorf("session-start line missing log_file attr; got %q", data)
	}
}

// TestEnsureConfig_KeepsInjectedHTTP asserts a test-injected HTTP client is not
// overwritten (so test network stubs keep working) while the logger is still
// built.
func TestEnsureConfig_KeepsInjectedHTTP(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("HOME does not redirect %AppData%/%LocalAppData% on windows")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "cfg"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))

	injected := &http.Client{}
	a := &App{Store: fakeStore{m: map[string]string{}}, HTTP: injected}
	if err := a.ensureConfig(); err != nil {
		t.Fatalf("ensureConfig: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })

	if a.HTTP != injected {
		t.Error("injected HTTP client was overwritten by ensureConfig")
	}
}

// TestEnsureConfig_PathsErrorFallsBackToDiscard asserts that when the log path
// cannot be resolved (HOME unset → paths.DefaultPaths fails) the CLI still runs:
// a.Log is a non-nil discard logger and no log file is created. ConfigPath is
// preset so config loading itself does not fail on the unset HOME.
func TestEnsureConfig_PathsErrorFallsBackToDiscard(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the HOME-unset recipe is unix-specific")
	}
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")

	a := &App{ConfigPath: cfgPath, Store: fakeStore{m: map[string]string{}}}
	if err := a.ensureConfig(); err != nil {
		t.Fatalf("ensureConfig should not fail just because logging cannot resolve a path: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	if a.Log == nil {
		t.Fatal("a.Log nil; want a discard logger fallback")
	}
	// No real file backs the discard logger, so Close is a no-op.
	if err := a.Close(); err != nil {
		t.Errorf("Close on discard logger = %v, want nil", err)
	}
}

// TestEnsureConfig_OpenLoggerErrorFallsBackToDiscard makes the resolved log
// directory un-creatable (a regular file occupies its parent path) so
// OpenLogger fails, and asserts the CLI still gets a non-nil discard logger.
func TestEnsureConfig_OpenLoggerErrorFallsBackToDiscard(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("uses the darwin ~/Library/Logs layout to block the log dir")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	// Occupy $HOME/Library/Logs with a regular FILE so MkdirAll of
	// $HOME/Library/Logs/nexus inside OpenLogger fails.
	library := filepath.Join(home, "Library")
	if err := os.MkdirAll(library, 0o700); err != nil {
		t.Fatalf("seed Library: %v", err)
	}
	if err := os.WriteFile(filepath.Join(library, "Logs"), []byte("x"), 0o600); err != nil {
		t.Fatalf("seed Logs-as-file: %v", err)
	}

	a := &App{Store: fakeStore{m: map[string]string{}}}
	if err := a.ensureConfig(); err != nil {
		t.Fatalf("ensureConfig should tolerate a log-open failure: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	if a.Log == nil {
		t.Fatal("a.Log nil; want a discard logger fallback")
	}
}

// TestApp_Close_NoLogger asserts Close is a safe no-op when no logger was ever
// opened (a test that preset a.Log, or a command that never ran ensureConfig).
func TestApp_Close_NoLogger(t *testing.T) {
	a := &App{}
	if err := a.Close(); err != nil {
		t.Errorf("Close with no logger = %v, want nil", err)
	}
}

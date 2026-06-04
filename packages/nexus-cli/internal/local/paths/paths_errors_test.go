package paths

import (
	"errors"
	"runtime"
	"testing"
)

// TestDefaultPaths_HomeUnsetErrors forces os.UserConfigDir / os.UserHomeDir to
// fail by clearing the env vars they depend on, proving DefaultPaths surfaces the
// resolution error rather than returning a bogus path. The mechanism differs per
// OS (HOME on unix, AppData/LocalAppData on windows), so the test targets the
// current GOOS.
func TestDefaultPaths_HomeUnsetErrors(t *testing.T) {
	switch runtime.GOOS {
	case "darwin", "linux":
		// os.UserConfigDir() needs $HOME on unix; clearing it makes the very
		// first resolution in DefaultPaths fail. On linux, also clear the XDG
		// override so userLogDir falls through to the $HOME branch.
		t.Setenv("HOME", "")
		t.Setenv("XDG_CONFIG_HOME", "")
		t.Setenv("XDG_STATE_HOME", "")
	case "windows":
		// os.UserConfigDir() needs %AppData% on windows.
		t.Setenv("AppData", "")
		t.Setenv("LocalAppData", "")
	default:
		t.Skipf("no env-clearing recipe for GOOS %q", runtime.GOOS)
	}

	if _, err := DefaultPaths(); err == nil {
		t.Fatal("expected DefaultPaths to error with home/appdata unset, got nil")
	}
}

// TestDefaultPaths_ConfigDirError forces the config-dir resolution to fail (via
// the userConfigDir seam) while the log dir would succeed, proving DefaultPaths
// surfaces that specific error and stops.
func TestDefaultPaths_ConfigDirError(t *testing.T) {
	orig := userConfigDir
	t.Cleanup(func() { userConfigDir = orig })
	sentinel := errors.New("no config dir")
	userConfigDir = func() (string, error) { return "", sentinel }

	_, err := DefaultPaths()
	if !errors.Is(err, sentinel) {
		t.Fatalf("DefaultPaths err = %v, want it to wrap the config-dir error", err)
	}
}

// TestDefaultPaths_LogDirError forces the log-dir resolution to fail (config dir
// succeeds), covering the second error branch that env manipulation alone cannot
// reach on platforms where config + log share a home source.
func TestDefaultPaths_LogDirError(t *testing.T) {
	origCfg, origLog := userConfigDir, resolveLogDir
	t.Cleanup(func() { userConfigDir, resolveLogDir = origCfg, origLog })
	userConfigDir = func() (string, error) { return "/tmp/cfg", nil }
	sentinel := errors.New("no log dir")
	resolveLogDir = func() (string, error) { return "", sentinel }

	_, err := DefaultPaths()
	if !errors.Is(err, sentinel) {
		t.Fatalf("DefaultPaths err = %v, want it to wrap the log-dir error", err)
	}
}

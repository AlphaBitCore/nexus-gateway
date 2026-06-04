package local

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestDefaultConfigPath_DelegatesToPaths asserts DefaultConfigPath returns the
// same location paths.DefaultPaths resolves (one source of truth) — the path
// ends in nexus/config.toml.
func TestDefaultConfigPath_DelegatesToPaths(t *testing.T) {
	got, err := DefaultConfigPath()
	if err != nil {
		t.Fatalf("DefaultConfigPath: %v", err)
	}
	want := filepath.Join("nexus", "config.toml")
	if !strings.HasSuffix(got, want) {
		t.Errorf("DefaultConfigPath = %q, want suffix %q", got, want)
	}
}

// TestDefaultConfigPath_Error surfaces the resolution error when the home /
// appdata source is unset, proving the error from paths.DefaultPaths is
// propagated rather than swallowed.
func TestDefaultConfigPath_Error(t *testing.T) {
	switch runtime.GOOS {
	case "darwin", "linux":
		t.Setenv("HOME", "")
		t.Setenv("XDG_CONFIG_HOME", "")
	case "windows":
		t.Setenv("AppData", "")
	default:
		t.Skipf("no env-clearing recipe for GOOS %q", runtime.GOOS)
	}
	if _, err := DefaultConfigPath(); err == nil {
		t.Fatal("expected DefaultConfigPath to error with home/appdata unset, got nil")
	}
}

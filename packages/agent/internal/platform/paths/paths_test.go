package paths

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestDefaultPaths verifies the per-OS path layout: every field is populated,
// the derived paths are consistent with their parents, and the values follow
// the documented OS convention for the current GOOS.
func TestDefaultPaths(t *testing.T) {
	p := DefaultPaths()

	// All fields must be populated — a blank path silently breaks config
	// defaulting / the installer layout.
	fields := map[string]string{
		"StateDir": p.StateDir, "ConfigDir": p.ConfigDir, "ConfigFile": p.ConfigFile,
		"LogDir": p.LogDir, "SocketPath": p.SocketPath, "FlagsDir": p.FlagsDir,
		"UserQuitFlagPath": p.UserQuitFlagPath, "DaemonUnitPath": p.DaemonUnitPath,
	}
	for name, v := range fields {
		if v == "" {
			t.Errorf("%s is empty", name)
		}
		if !filepath.IsAbs(v) {
			t.Errorf("%s = %q is not absolute", name, v)
		}
	}

	// Derived-path invariants the daemon and GUI rely on agreeing about.
	// filepath.Join keeps the comparison separator-agnostic so it holds on
	// Windows (\) as well as macOS/Linux (/).
	if p.ConfigFile != filepath.Join(p.ConfigDir, "agent.yaml") {
		t.Errorf("ConfigFile %q must be ConfigDir/agent.yaml", p.ConfigFile)
	}
	if p.UserQuitFlagPath != filepath.Join(p.FlagsDir, "user-quit") {
		t.Errorf("UserQuitFlagPath %q must be FlagsDir/user-quit", p.UserQuitFlagPath)
	}
	if !strings.HasPrefix(p.FlagsDir, p.StateDir) {
		t.Errorf("FlagsDir %q must be under StateDir %q", p.FlagsDir, p.StateDir)
	}

	// Per-OS convention spot-checks.
	switch runtime.GOOS {
	case "darwin":
		if !strings.Contains(p.StateDir, "/Library/Application Support/com.nexus-gateway.agent") {
			t.Errorf("darwin StateDir wrong: %q", p.StateDir)
		}
		if !strings.HasSuffix(p.DaemonUnitPath, ".plist") {
			t.Errorf("darwin DaemonUnitPath must be a launchd plist: %q", p.DaemonUnitPath)
		}
	case "linux":
		if !strings.Contains(p.StateDir, "nexus-agent") {
			t.Errorf("linux StateDir wrong: %q", p.StateDir)
		}
	case "windows":
		if !strings.Contains(strings.ToLower(p.StateDir), "nexusagent") {
			t.Errorf("windows StateDir wrong: %q", p.StateDir)
		}
	}
}

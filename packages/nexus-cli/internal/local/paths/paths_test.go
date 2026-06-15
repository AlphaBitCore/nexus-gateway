package paths

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestDefaultPaths_Shape asserts the resolved paths land under the right
// user-scoped roots and that LogFile is the named file inside LogDir. The exact
// home prefix is environment-dependent, so the assertions check structure
// (suffixes, containment) rather than a hardcoded absolute path.
func TestDefaultPaths_Shape(t *testing.T) {
	p, err := DefaultPaths()
	if err != nil {
		t.Fatalf("DefaultPaths: %v", err)
	}

	// ConfigFile is always <userConfigDir>/nexus/config.toml.
	wantCfgSuffix := filepath.Join("nexus", "config.toml")
	if !strings.HasSuffix(p.ConfigFile, wantCfgSuffix) {
		t.Errorf("ConfigFile = %q, want suffix %q", p.ConfigFile, wantCfgSuffix)
	}

	// LogFile must be exactly LogDir/nexus-cli.log so the logger and the
	// startup banner agree.
	wantLogFile := filepath.Join(p.LogDir, "nexus-cli.log")
	if p.LogFile != wantLogFile {
		t.Errorf("LogFile = %q, want %q", p.LogFile, wantLogFile)
	}

	// Per-OS log dir convention. Verify the directory the current GOOS uses,
	// since the build-tagged userLogDir compiled into this binary is the one
	// under test.
	switch runtime.GOOS {
	case "darwin":
		wantSuffix := filepath.Join("Library", "Logs", "nexus")
		if !strings.HasSuffix(p.LogDir, wantSuffix) {
			t.Errorf("darwin LogDir = %q, want suffix %q", p.LogDir, wantSuffix)
		}
	case "linux":
		// Either $XDG_STATE_HOME/nexus or ~/.local/state/nexus; both end in
		// .../nexus and the state segment distinguishes them from config.
		if !strings.HasSuffix(p.LogDir, "nexus") {
			t.Errorf("linux LogDir = %q, want suffix nexus", p.LogDir)
		}
		if !strings.Contains(p.LogDir, "state") {
			t.Errorf("linux LogDir = %q, want it under an XDG_STATE-style state dir", p.LogDir)
		}
	case "windows":
		wantSuffix := filepath.Join("nexus", "Logs")
		if !strings.HasSuffix(p.LogDir, wantSuffix) {
			t.Errorf("windows LogDir = %q, want suffix %q", p.LogDir, wantSuffix)
		}
	}
}

// TestDefaultPaths_ConfigAndLogDiffer guards the user-scoped split: config and
// logs must not collapse to the same directory (that would, on Linux, put logs
// in $XDG_CONFIG_HOME instead of $XDG_STATE_HOME).
func TestDefaultPaths_ConfigAndLogDiffer(t *testing.T) {
	p, err := DefaultPaths()
	if err != nil {
		t.Fatalf("DefaultPaths: %v", err)
	}
	cfgDir := filepath.Dir(p.ConfigFile)
	if cfgDir == p.LogDir {
		t.Errorf("config dir and log dir collapsed to %q; logs must be user-scoped state, not config", cfgDir)
	}
}

var errStub = errStubT("no config dir")

type errStubT string

func (e errStubT) Error() string { return string(e) }

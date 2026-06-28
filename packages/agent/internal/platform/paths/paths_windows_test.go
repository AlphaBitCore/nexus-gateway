//go:build windows

package paths

import "testing"

// TestDefaultPaths_ProgramDataFallback covers the defaultPaths fallback that
// hard-defaults to C:\ProgramData when the ProgramData environment variable is
// empty — e.g. a service launched with a stripped environment. The standard
// TestDefaultPaths runs with ProgramData set, so it never exercises this arm.
func TestDefaultPaths_ProgramDataFallback(t *testing.T) {
	t.Setenv("ProgramData", "")
	p := DefaultPaths()
	if p.StateDir != `C:\ProgramData\NexusAgent` {
		t.Errorf("StateDir with empty ProgramData = %q, want C:\\ProgramData\\NexusAgent", p.StateDir)
	}
	if p.ConfigFile != `C:\ProgramData\NexusAgent\agent.yaml` {
		t.Errorf("ConfigFile with empty ProgramData = %q, want C:\\ProgramData\\NexusAgent\\agent.yaml", p.ConfigFile)
	}
}

package runtime

import "testing"

// TestWebRegistryExcludesHostTools is the code-enforced AC-5 guard: the web/server
// profile (EnableSystem/EnableCanvas false — the zero value) must NEVER register
// the host shell/file tools or the TUI canvas. run_command is TierAuto (no confirm
// gate), so leaking it server-side is a direct RCE on the CP host. This test is the
// grep-replacement the spec asks for: it fails the build the moment a host tool
// becomes reachable from a web registry.
func TestWebRegistryExcludesHostTools(t *testing.T) {
	reg := NewAgentRegistry(&fakeGateway{}, nil, AgentOptions{EnableMitigate: true})
	for _, banned := range []string{"run_command", "read_file", "write_file", "navigate", "show_event", "highlight"} {
		if _, ok := reg.Get(banned); ok {
			t.Fatalf("web profile must NOT register %q (host/TUI tool) — binding invariant: no run_command server-side", banned)
		}
	}
	// It must still expose the gateway read tools (the web assistant's read surface).
	if _, ok := reg.Get("observe_health"); !ok {
		t.Fatal("web profile must still register the gateway read tools (observe_health)")
	}
}

// TestCLIRegistryIncludesHostTools confirms the CLI/TUI profile, which explicitly
// opts in, still gets the host + canvas tools (so the kernel extraction did not
// silently strip CLI capability).
func TestCLIRegistryIncludesHostTools(t *testing.T) {
	reg := NewAgentRegistry(&fakeGateway{}, &fakeCanvas{}, AgentOptions{EnableSystem: true, EnableCanvas: true})
	for _, want := range []string{"run_command", "navigate"} {
		if _, ok := reg.Get(want); !ok {
			t.Fatalf("CLI profile (EnableSystem+EnableCanvas) must register %q", want)
		}
	}
}

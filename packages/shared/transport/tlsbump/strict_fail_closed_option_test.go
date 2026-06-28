package tlsbump

import "testing"

// TestWithStrictFailClosed_OptionPlumbing is the plumbing regression:
// the compliance-proxy opts into strict fail-closed; the agent NE path (no
// option) defaults to fail-open. The flag is threaded to BuildPipeline as
// bo.strictFailClosed at the four tlsbump build sites; the build-time gate
// itself is exercised in shared/policy/pipeline/strict_fail_closed_test.go.
func TestWithStrictFailClosed_OptionPlumbing(t *testing.T) {
	var def bumpOptions
	if def.strictFailClosed {
		t.Error("default bumpOptions.strictFailClosed must be false (agent NE host-packet-path fail-open)")
	}
	var cp bumpOptions
	WithStrictFailClosed()(&cp)
	if !cp.strictFailClosed {
		t.Error("WithStrictFailClosed() must set strictFailClosed=true (compliance-proxy appliance refuses)")
	}
}

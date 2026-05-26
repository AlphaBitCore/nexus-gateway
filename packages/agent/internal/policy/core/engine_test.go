package core

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/policy/exemption"
)

// TestEvaluate_DefaultAction covers the case where neither the
// exemption store nor the interception_domains fallback matches: the
// engine returns its configured defaultAction with Matched=false.
func TestEvaluate_DefaultAction(t *testing.T) {
	e := NewEngine("passthrough")
	r := e.Evaluate("unknown.example.com")
	if r.Action != "passthrough" {
		t.Errorf("expected passthrough default, got %s", r.Action)
	}
	if r.Matched {
		t.Error("default-action result must report Matched=false")
	}
}

// TestEvaluate_DefaultActionCustom covers a non-passthrough default
// (admin-configured "deny" for strict deployments).
func TestEvaluate_DefaultActionCustom(t *testing.T) {
	e := NewEngine("deny")
	r := e.Evaluate("unknown.example.com")
	if r.Action != "deny" {
		t.Errorf("expected default deny, got %s", r.Action)
	}
	if r.Matched {
		t.Error("should not be matched")
	}
}

// TestSetExemptionStore_PassthroughWhenExempt pins that an attached
// exemption store short-circuits the evaluation: exempt hosts return
// passthrough with a synthetic MatchedPattern of "exempt:<reason>"
// regardless of any other state.
func TestSetExemptionStore_PassthroughWhenExempt(t *testing.T) {
	es := exemption.NewStore(exemption.DefaultConfig())
	es.Add("exempt.example.com", "manual", exemption.SourceAdmin, 0)

	e := NewEngine("passthrough")
	e.SetExemptionStore(es)

	r := e.Evaluate("exempt.example.com")
	if r.Action != "passthrough" {
		t.Errorf("exempted host must short-circuit to passthrough; got %s", r.Action)
	}
	if r.MatchedPattern != "exempt:manual" {
		t.Errorf("matched pattern should be exempt:reason; got %q", r.MatchedPattern)
	}
	if r.MatchedIndex != -1 {
		t.Errorf("exempt match should use index -1; got %d", r.MatchedIndex)
	}
}

// TestSetInterceptionHostsFn_FallbackToInspect covers the
// interception_domains fallback: hosts matching the Hub-pushed
// interception list return "inspect" so the daemon TLS-bumps the flow.
// Without this, the entire interception_domains config is dead weight
// (without this fallback, pushed domains produce 0 inspected flows).
func TestSetInterceptionHostsFn_FallbackToInspect(t *testing.T) {
	e := NewEngine("passthrough")
	e.SetInterceptionHostsFn(func() []string {
		return []string{"*.openai.com", "", "api.anthropic.com"} // include empty to drive the skip branch
	})

	got := e.Evaluate("chat.openai.com")
	if got.Action != "inspect" {
		t.Errorf("wildcard interception match must yield inspect; got %s", got.Action)
	}
	if got.MatchedPattern != "interception_domain:*.openai.com" {
		t.Errorf("matched pattern: got %q", got.MatchedPattern)
	}
	if got.MatchedIndex != -1 {
		t.Errorf("interception match index should be -1; got %d", got.MatchedIndex)
	}

	got = e.Evaluate("api.anthropic.com")
	if got.Action != "inspect" {
		t.Errorf("exact interception match must yield inspect; got %s", got.Action)
	}

	got = e.Evaluate("unrelated.example.com")
	if got.Action != "passthrough" {
		t.Errorf("non-matching host must fall through to defaultAction; got %s", got.Action)
	}
}

// TestSetInterceptionHostsFn_NilClears covers the nil-clear branch.
// Production wiring un-installs the hook when the shadow key empties.
func TestSetInterceptionHostsFn_NilClears(t *testing.T) {
	e := NewEngine("passthrough")
	e.SetInterceptionHostsFn(func() []string { return []string{"*.openai.com"} })
	// Re-install nil — should disable the fallback.
	e.SetInterceptionHostsFn(nil)

	got := e.Evaluate("chat.openai.com")
	if got.Action != "passthrough" {
		t.Errorf("after nil-clear, host must fall through to defaultAction; got %s", got.Action)
	}
}

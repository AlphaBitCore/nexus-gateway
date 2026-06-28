package thingtype

import (
	"sort"
	"testing"
)

func TestIsKnown_CanonicalValues(t *testing.T) {
	for _, name := range []string{Agent, AIGateway, ComplianceProxy, ControlPlane, NexusHub} {
		if !IsKnown(name) {
			t.Errorf("IsKnown(%q) = false; canonical value rejected", name)
		}
	}
}

func TestIsKnown_RejectsCommonTypos(t *testing.T) {
	// Real values pulled from the [[agent-desktop-type-mismatch-bug]] memory
	// — typos that historically slipped past compile-time checks because
	// the consumer side compared against a literal.
	cases := []string{
		"",
		"agent-desktop", // memory mentions this exact typo
		"agent-mobile",
		"Agent",        // case sensitivity
		"AI_GATEWAY",   // underscored
		"compliance",   // truncated
		"hub",          // truncated
		" agent",       // whitespace
		"agent ",       // trailing whitespace
		"ai_gateway",   // alt punctuation
		"controlplane", // no hyphen
	}
	for _, c := range cases {
		if IsKnown(c) {
			t.Errorf("IsKnown(%q) = true; typo accepted", c)
		}
	}
}

func TestIsBackendService_ServiceTypesOnly(t *testing.T) {
	// The four internal service types authenticate to Hub with the shared
	// service token and must be accepted.
	for _, name := range []string{AIGateway, ComplianceProxy, ControlPlane, NexusHub} {
		if !IsBackendService(name) {
			t.Errorf("IsBackendService(%q) = false; service type rejected", name)
		}
	}
	// Agent authenticates with a per-device token — it must NOT be treated as a
	// backend service: a service-token caller must not impersonate an
	// agent Thing.
	if IsBackendService(Agent) {
		t.Errorf("IsBackendService(%q) = true; agent must not count as a backend service", Agent)
	}
	// Unknown / malformed types fail closed.
	for _, c := range []string{"", "agent-desktop", "AI_GATEWAY", "hub", " agent", "evil"} {
		if IsBackendService(c) {
			t.Errorf("IsBackendService(%q) = true; non-service type accepted", c)
		}
	}
}

func TestAll_ContainsEveryCanonical(t *testing.T) {
	got := All()
	want := map[string]bool{
		Agent: true, AIGateway: true, ComplianceProxy: true,
		ControlPlane: true, NexusHub: true,
	}
	if len(got) != len(want) {
		t.Errorf("All() len = %d, want %d", len(got), len(want))
	}
	for _, t1 := range got {
		if !want[t1] {
			t.Errorf("All() contains unexpected %q", t1)
		}
		delete(want, t1)
	}
	if len(want) != 0 {
		// Anything left in want was missing from All().
		missing := make([]string, 0, len(want))
		for k := range want {
			missing = append(missing, k)
		}
		sort.Strings(missing)
		t.Errorf("All() missing %v", missing)
	}
}

func TestAll_ReturnedSliceIsNotShared(t *testing.T) {
	// The doc string promises mutation of the returned slice does not
	// affect the policy — pin that contract.
	a := All()
	if len(a) == 0 {
		t.Fatal("All() returned empty slice")
	}
	a[0] = "MUTATED"
	if !IsKnown(Agent) {
		t.Errorf("mutating All()[0] silently corrupted the package state")
	}
	b := All()
	for _, v := range b {
		if v == "MUTATED" {
			t.Errorf("All() returned a shared slice; mutation leaked: %+v", b)
		}
	}
}

package builtins

import "testing"

// TestDefaultRegistry_AllExpectedFactoriesPresent verifies the global Registry
// contains all documented built-in factories.
func TestDefaultRegistry_AllExpectedFactoriesPresent(t *testing.T) {
	expected := []string{
		"keyword-filter", "pii-detector", "content-safety",
		"rate-limiter", "request-size-validator", "ip-access-filter",
		"data-residency", "rulepack-engine", "noop",
		"webhook-forward", "quality-checker",
	}
	for _, id := range expected {
		if Registry.Get(id) == nil {
			t.Errorf("default Registry missing %q", id)
		}
	}
}

package rules_test

import (
	"encoding/json"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/engine/rules"
)

func TestRegistry_Lookup(t *testing.T) {
	reg := rules.NewRegistry(rules.BuiltinRules)

	d, ok := reg.Lookup("quota.threshold")
	if !ok {
		t.Fatal("expected quota.threshold in registry")
	}
	if d.SourceType != "quota" {
		t.Errorf("sourceType=%q", d.SourceType)
	}
	if d.CooldownSec != 300 {
		t.Errorf("cooldownSec=%d", d.CooldownSec)
	}
	if !d.RequiresAck {
		t.Errorf("requiresAck=%v want true", d.RequiresAck)
	}
}

func TestRegistry_LookupUnknown(t *testing.T) {
	reg := rules.NewRegistry(rules.BuiltinRules)
	if _, ok := reg.Lookup("does.not.exist"); ok {
		t.Fatal("expected unknown rule lookup to return false")
	}
}

func TestRegistry_All(t *testing.T) {
	reg := rules.NewRegistry(rules.BuiltinRules)
	all := reg.All()
	if len(all) != 30 {
		t.Fatalf("got %d rules, want 30", len(all))
	}
}


// TestBuiltinRulesHaveValidParamsSchema makes sure every ParamsSchema parses
// as valid JSON (not just that mustJSON didn't panic — we also want to see
// that the default Params is itself valid against the schema, or at least
// that both parse).
func TestBuiltinRulesHaveValidParamsSchema(t *testing.T) {
	for _, d := range rules.BuiltinRules {
		var params, schema map[string]any
		if err := json.Unmarshal(d.Params, &params); err != nil {
			t.Errorf("%s: params invalid JSON: %v", d.ID, err)
		}
		if err := json.Unmarshal(d.ParamsSchema, &schema); err != nil {
			t.Errorf("%s: paramsSchema invalid JSON: %v", d.ID, err)
		}
	}
}

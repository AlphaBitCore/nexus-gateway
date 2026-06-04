package agent

import (
	"encoding/json"
	"testing"
)

func TestRegistrySchemasAndDispatch(t *testing.T) {
	r := NewRegistry()
	r.Register(&stubTool{name: "observe_cost", tier: TierAuto, schema: json.RawMessage(`{"type":"object","properties":{"window":{"type":"string"}}}`)})
	r.Register(&stubTool{name: "mitigate_kill", tier: TierConfirm})

	if _, ok := r.Get("observe_cost"); !ok {
		t.Fatal("Get should find a registered tool")
	}
	if _, ok := r.Get("nope"); ok {
		t.Fatal("Get should miss an unknown tool")
	}

	// Names returns registration order.
	names := r.Names()
	if len(names) != 2 || names[0] != "observe_cost" || names[1] != "mitigate_kill" {
		t.Fatalf("Names should return registration order, got %v", names)
	}

	// nil allow-list → all tools, with their real schemas/descriptions.
	all := r.Schemas(nil)
	if len(all) != 2 {
		t.Fatalf("Schemas(nil) should expose all tools, got %d", len(all))
	}
	var cost *ToolSchema
	for i := range all {
		if all[i].Name == "observe_cost" {
			cost = &all[i]
		}
	}
	if cost == nil || string(cost.Parameters) == "" || cost.Description == "" {
		t.Fatalf("schema should carry name+description+parameters, got %+v", cost)
	}

	// Narrowed allow-list → only the named subset (skill narrowing).
	narrow := r.Schemas([]string{"observe_cost"})
	if len(narrow) != 1 || narrow[0].Name != "observe_cost" {
		t.Fatalf("Schemas(allow) should narrow, got %+v", narrow)
	}
	// Unknown name in allow-list is skipped, not invented.
	if got := r.Schemas([]string{"observe_cost", "ghost"}); len(got) != 1 {
		t.Fatalf("unknown allow entry must be skipped, got %+v", got)
	}

	// Re-registering the same name replaces without duplicating order.
	r.Register(&stubTool{name: "observe_cost", tier: TierAuto})
	if len(r.Names()) != 2 {
		t.Fatalf("re-register must not duplicate order, got %v", r.Names())
	}
}

// TestRegistryRemove covers Remove: a removed tool is gone from Get, Names, and
// the schema set, the order of the survivors is preserved, and removing an
// unknown name is a no-op. Hosts rely on this to enforce a governance policy that
// withholds a capability (e.g. the assistant's raw-body read tools).
func TestRegistryRemove(t *testing.T) {
	r := NewRegistry()
	r.Register(&stubTool{name: "a", tier: TierAuto})
	r.Register(&stubTool{name: "b", tier: TierAuto})
	r.Register(&stubTool{name: "c", tier: TierAuto})

	r.Remove("nope") // no-op on an unknown name
	r.Remove("b")

	if _, ok := r.Get("b"); ok {
		t.Error("Get must not return a removed tool")
	}
	if got := r.Names(); len(got) != 2 || got[0] != "a" || got[1] != "c" {
		t.Errorf("Names after Remove = %v; want [a c] (order preserved)", got)
	}
	for _, s := range r.Schemas(nil) {
		if s.Name == "b" {
			t.Error("Schemas must not advertise a removed tool")
		}
	}
	// a and c still resolve.
	if _, ok := r.Get("a"); !ok {
		t.Error("Remove must not affect other tools")
	}
}

package matcher

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
)

// The product's model-access rule, and the reason this filter survived the
// removal of the stage-0 narrowing it used to share a function with: a routed
// model must be on the virtual key's allow list.
//
// These two cases carry over verbatim from the narrowing engine's tests. They
// are the ones that had to keep passing through the deletion — anyone removing
// the old Filter by name would have taken this with it.
func TestVKAccessFilter_KeepsOnlyAllowedModels(t *testing.T) {
	targets := []core.RoutingTarget{
		{ModelID: "m-allowed", ProviderModelID: "gpt-4", ProviderID: "openai"},
		{ModelID: "m-other", ProviderModelID: "claude-3", ProviderID: "anthropic"},
	}
	rctx := &core.RoutingContext{
		VirtualKey: &core.VKContext{
			// The ref must name the provider too — ModelMatchesAllowedRefs skips
			// refs whose ProviderID does not match.
			AllowedModels: []store.AllowedModelRef{{ModelID: "m-allowed", ProviderID: "openai"}},
		},
	}
	kept := VKAccessFilter{}.Keep(targets, rctx)
	if len(kept) != 1 || kept[0].ModelID != "m-allowed" {
		t.Errorf("expected only the allowed target, got %v", kept)
	}
}

// An empty allow list means unrestricted, not "nothing". Reading it the other
// way would make every key with no explicit list unable to route at all.
func TestVKAccessFilter_NoRestrictionKeepsEverything(t *testing.T) {
	targets := []core.RoutingTarget{
		{ModelID: "m1", ProviderModelID: "gpt-4", ProviderID: "openai"},
		{ModelID: "m2", ProviderModelID: "claude-3", ProviderID: "anthropic"},
	}
	for name, rctx := range map[string]*core.RoutingContext{
		"no virtual key":   {VirtualKey: nil},
		"empty allow list": {VirtualKey: &core.VKContext{AllowedModels: nil}},
		"nil context":      nil,
	} {
		t.Run(name, func(t *testing.T) {
			if kept := (VKAccessFilter{}).Keep(targets, rctx); len(kept) != 2 {
				t.Errorf("expected both targets, got %d", len(kept))
			}
		})
	}
}

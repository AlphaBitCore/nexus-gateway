package routing

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// TestFilterByModality verifies the resolver's modality guard drops every
// target whose catalog model type cannot serve the request's endpoint kind,
// keeps compatible targets in order, and treats an empty ModelType as
// unconstrained (fail-open) so an unpopulated target is never dropped.
func TestFilterByModality(t *testing.T) {
	targets := []core.RoutingTarget{
		{ModelID: "chat-a", ModelType: "chat"},
		{ModelID: "img-b", ModelType: "image"},
		{ModelID: "chat-c", ModelType: "chat"},
		{ModelID: "unknown-d", ModelType: ""}, // fail-open: always kept
	}

	t.Run("chat endpoint keeps chat + unknown, drops image", func(t *testing.T) {
		got, _ := filterByModality(append([]core.RoutingTarget(nil), targets...), typology.EndpointKindChat)
		ids := targetIDs(got)
		want := []string{"chat-a", "chat-c", "unknown-d"}
		if !equalStrings(ids, want) {
			t.Fatalf("chat filter = %v, want %v", ids, want)
		}
	})

	t.Run("image endpoint keeps image + unknown, drops chat", func(t *testing.T) {
		got, _ := filterByModality(append([]core.RoutingTarget(nil), targets...), typology.EndpointKindImageGeneration)
		ids := targetIDs(got)
		want := []string{"img-b", "unknown-d"}
		if !equalStrings(ids, want) {
			t.Fatalf("image filter = %v, want %v", ids, want)
		}
	})

	t.Run("guardrail endpoint keeps everything (no model binding)", func(t *testing.T) {
		got, _ := filterByModality(append([]core.RoutingTarget(nil), targets...), typology.EndpointKindGuardrail)
		if len(got) != len(targets) {
			t.Fatalf("guardrail filter dropped targets: got %d, want %d", len(got), len(targets))
		}
	})

	t.Run("all-mismatch yields empty", func(t *testing.T) {
		onlyChat := []core.RoutingTarget{{ModelID: "c1", ModelType: "chat"}, {ModelID: "c2", ModelType: "chat"}}
		got, _ := filterByModality(onlyChat, typology.EndpointKindImageGeneration)
		if len(got) != 0 {
			t.Fatalf("expected empty result when every target mismatches, got %v", targetIDs(got))
		}
	})
}

func targetIDs(ts []core.RoutingTarget) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.ModelID)
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

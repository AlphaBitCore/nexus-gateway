package routing

import (
	"context"
	"github.com/goccy/go-json"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
)

// TestResolver_RequestNeedsCanonical locks the Phase-E gate signal: the resolver
// reports true iff any enabled rule's strategy tree contains a content-reading
// node (smart), detected recursively, and fails SAFE (true) on a rule-fetch
// error so smart routing is never starved of the canonical.
func TestResolver_RequestNeedsCanonical(t *testing.T) {
	smartCfg, err := json.Marshal(core.StrategyNode{
		Type:    "fallback",
		Targets: []core.StrategyNode{{Type: "single"}, {Type: "smart"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	plainCfg, err := json.Marshal(core.StrategyNode{Type: "single", ProviderID: "p", ModelID: "m"})
	if err != nil {
		t.Fatal(err)
	}
	// A smart rule scoped to requestedModelLiterals=["auto"] — the canonical is
	// needed ONLY for model "auto", never for concrete models.
	autoOnly, err := json.Marshal(core.MatchConditions{RequestedModelLiterals: []string{"auto"}})
	if err != nil {
		t.Fatal(err)
	}
	// A smart rule scoped to a Models set — not cheaply evaluable pre-canonical,
	// so the gate is conservative (could match any model).
	modelSet, err := json.Marshal(core.MatchConditions{Models: []string{"gpt-4o"}})
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name  string
		store routingStore
		model string
		want  bool
	}{
		{"no rules", &coverageFakeStore{}, "gpt-4o", false},
		{"rule with empty config", &coverageFakeStore{rules: []store.RoutingRule{{Config: nil}}}, "gpt-4o", false},
		{"only non-smart rule", &coverageFakeStore{rules: []store.RoutingRule{{Config: plainCfg}}}, "gpt-4o", false},
		{"smart catch-all matches any model", &coverageFakeStore{rules: []store.RoutingRule{{Config: plainCfg}, {Config: smartCfg}}}, "gpt-4o", true},
		{"smart scoped to auto: concrete model does NOT match", &coverageFakeStore{rules: []store.RoutingRule{{Config: smartCfg, MatchConditions: autoOnly}}}, "mock-gpt-4o-mini", false},
		{"smart scoped to auto: auto matches", &coverageFakeStore{rules: []store.RoutingRule{{Config: smartCfg, MatchConditions: autoOnly}}}, "auto", true},
		{"smart scoped by Models set: conservative match", &coverageFakeStore{rules: []store.RoutingRule{{Config: smartCfg, MatchConditions: modelSet}}}, "anything", true},
		{"fetch error fails safe (compute)", &errRulesStore{}, "gpt-4o", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &Resolver{db: tc.store}
			if got := r.RequestNeedsCanonical(context.Background(), tc.model); got != tc.want {
				t.Fatalf("RequestNeedsCanonical = %v, want %v", got, tc.want)
			}
		})
	}

	// Memoization: a repeated call over the same Config returns the same answer
	// (exercises the contentCache hit path).
	r := &Resolver{db: &coverageFakeStore{rules: []store.RoutingRule{{Config: smartCfg}}}}
	first := r.RequestNeedsCanonical(context.Background(), "m")
	second := r.RequestNeedsCanonical(context.Background(), "m") // memoized contentCache hit
	if !first || !second {
		t.Fatal("memoized RequestNeedsCanonical should stay true on both the cold and cached call")
	}
}

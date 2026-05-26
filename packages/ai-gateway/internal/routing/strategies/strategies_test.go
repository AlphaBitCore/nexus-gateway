package strategies

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
)

func contains(haystack, needle string) bool { return strings.Contains(haystack, needle) }

var errStub = errors.New("stub lookup failure")

func mockLookup(_ context.Context, providerID, modelID string) (*core.RoutingTarget, error) {
	return &core.RoutingTarget{
		ProviderID:      providerID,
		ProviderName:    providerID + "-name",
		ModelID:         modelID,
		ProviderModelID: modelID,
		BaseURL:         "https://api.example.com",
	}, nil
}

func newTestRegistry() *StrategyRegistry {
	reg := NewStrategyRegistry()
	RegisterAllStrategies(reg, mockLookup, nil)
	return reg
}

func TestSingleStrategy(t *testing.T) {
	reg := newTestRegistry()
	var trace []core.TraceEntry
	targets, err := reg.Evaluate(
		context.Background(),
		core.StrategyNode{Type: "single", ProviderID: "openai", ModelID: "gpt-4"},
		&core.RoutingContext{},
		&trace,
		0,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 {
		t.Fatalf("expected 1 target, got %d", len(targets))
	}
	if targets[0].ProviderID != "openai" || targets[0].ModelID != "gpt-4" {
		t.Error("wrong target")
	}
}

func TestFallbackStrategy(t *testing.T) {
	reg := newTestRegistry()
	var trace []core.TraceEntry
	targets, err := reg.Evaluate(
		context.Background(),
		core.StrategyNode{
			Type: "fallback",
			Targets: []core.StrategyNode{
				{Type: "single", ProviderID: "openai", ModelID: "gpt-4"},
				{Type: "single", ProviderID: "anthropic", ModelID: "claude-3"},
			},
		},
		&core.RoutingContext{},
		&trace,
		0,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 2 {
		t.Fatalf("expected 2 targets, got %d", len(targets))
	}
}

func TestLoadbalanceStrategy(t *testing.T) {
	reg := newTestRegistry()

	// Run multiple times to verify randomness works.
	hits := map[string]int{}
	for range 100 {
		var trace []core.TraceEntry
		targets, err := reg.Evaluate(
			context.Background(),
			core.StrategyNode{
				Type: "loadbalance",
				Weighted: []core.WeightedTarget{
					{Weight: 1, Node: core.StrategyNode{Type: "single", ProviderID: "openai", ModelID: "gpt-4"}},
					{Weight: 1, Node: core.StrategyNode{Type: "single", ProviderID: "anthropic", ModelID: "claude-3"}},
				},
			},
			&core.RoutingContext{},
			&trace,
			0,
		)
		if err != nil {
			t.Fatal(err)
		}
		if len(targets) != 1 {
			t.Fatal("expected 1 target from loadbalance")
		}
		hits[targets[0].ProviderID]++
	}

	if hits["openai"] == 0 || hits["anthropic"] == 0 {
		t.Errorf("expected both providers to be hit, got %v", hits)
	}
}

// TestLoadbalance_JSONRoundTrip guards against the field-name drift between
// UI/DB (key "weightedTargets") and the Go core.StrategyNode (tag json:"weightedTargets").
// Regression: a prior mismatch stored the array under "targets", which Go's
// encoder silently spilled into core.StrategyNode.Targets (fallback slot), leaving
// Weighted empty so LoadbalanceStrategy.Evaluate returned nil with no trace.
func TestLoadbalance_JSONRoundTrip(t *testing.T) {
	raw := `{
	  "type": "loadbalance",
	  "weightedTargets": [
	    {"weight": 1, "node": {"type": "single", "providerId": "openai",    "modelId": "gpt-4"}},
	    {"weight": 1, "node": {"type": "single", "providerId": "anthropic", "modelId": "claude-3"}}
	  ]
	}`

	var node core.StrategyNode
	if err := json.Unmarshal([]byte(raw), &node); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(node.Weighted) != 2 {
		t.Fatalf("weightedTargets JSON key did not populate Weighted: len=%d (Targets len=%d)", len(node.Weighted), len(node.Targets))
	}

	reg := newTestRegistry()
	var trace []core.TraceEntry
	targets, err := reg.Evaluate(context.Background(), node, &core.RoutingContext{}, &trace, 0)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if len(targets) != 1 {
		t.Fatalf("expected 1 target from loadbalance, got %d", len(targets))
	}
	if len(trace) == 0 {
		t.Error("expected a trace entry from loadbalance.Evaluate")
	}
}

// TestLoadbalance_BuggyKeyStaysBroken pins the contract: the legacy key
// "targets" on a loadbalance node must NOT accidentally start working without
// a deliberate schema/code change. If this test starts failing because someone
// added an UnmarshalJSON that accepts both keys, update this test and the UI
// emitter together.
func TestLoadbalance_BuggyKeyStaysBroken(t *testing.T) {
	raw := `{
	  "type": "loadbalance",
	  "targets": [
	    {"weight": 1, "node": {"type": "single", "providerId": "openai", "modelId": "gpt-4"}}
	  ]
	}`

	var node core.StrategyNode
	if err := json.Unmarshal([]byte(raw), &node); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(node.Weighted) != 0 {
		t.Fatalf("legacy key 'targets' unexpectedly populated Weighted (len=%d) — update this test if that was intentional", len(node.Weighted))
	}
}

// TestLoadbalance_EmptyWeighted_EmitsTrace pins the rule that a misconfigured
// loadbalance node (no weightedTargets) must still surface a trace entry, so
// the simulate endpoint can explain "0 targets" to operators. Regression guard
// for the silent-fail-then-empty-trace bug observed when the seed stored a
// loadbalance payload under the wrong JSON key.
func TestLoadbalance_EmptyWeighted_EmitsTrace(t *testing.T) {
	reg := newTestRegistry()
	var trace []core.TraceEntry
	targets, err := reg.Evaluate(
		context.Background(),
		core.StrategyNode{Type: "loadbalance"}, // zero Weighted
		&core.RoutingContext{},
		&trace,
		0,
	)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if len(targets) != 0 {
		t.Errorf("expected 0 targets, got %d", len(targets))
	}
	if len(trace) != 1 || trace[0].StrategyType != "loadbalance" {
		t.Fatalf("expected exactly one loadbalance trace entry, got %+v", trace)
	}
	if !contains(trace[0].Decision, "no weightedTargets") {
		t.Errorf("trace decision should explain why: got %q", trace[0].Decision)
	}
}

// TestLoadbalance_ZeroTotalWeight_EmitsTrace pins the rule that a
// loadbalance node with all-zero weights does not silently return nil.
func TestLoadbalance_ZeroTotalWeight_EmitsTrace(t *testing.T) {
	reg := newTestRegistry()
	var trace []core.TraceEntry
	_, err := reg.Evaluate(
		context.Background(),
		core.StrategyNode{
			Type: "loadbalance",
			Weighted: []core.WeightedTarget{
				{Weight: 0, Node: core.StrategyNode{Type: "single", ProviderID: "p", ModelID: "m"}},
				{Weight: 0, Node: core.StrategyNode{Type: "single", ProviderID: "q", ModelID: "n"}},
			},
		},
		&core.RoutingContext{},
		&trace,
		0,
	)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if len(trace) != 1 || !contains(trace[0].Decision, "total weight is 0") {
		t.Fatalf("expected zero-weight trace, got %+v", trace)
	}
}

func TestConditionalStrategy(t *testing.T) {
	reg := newTestRegistry()
	ctx := &core.RoutingContext{
		RequestedModel: core.RequestedModel{Type: "chat"},
	}
	var trace []core.TraceEntry

	targets, err := reg.Evaluate(
		context.Background(),
		core.StrategyNode{
			Type: "conditional",
			Conditions: []core.ConditionalBranch{
				{
					When: map[string]any{"requestedModel.type": "embedding"},
					Then: core.StrategyNode{Type: "single", ProviderID: "openai", ModelID: "text-embedding"},
				},
				{
					When: map[string]any{"requestedModel.type": "chat"},
					Then: core.StrategyNode{Type: "single", ProviderID: "anthropic", ModelID: "claude-3"},
				},
			},
		},
		ctx,
		&trace,
		0,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || targets[0].ProviderID != "anthropic" {
		t.Errorf("expected anthropic, got %v", targets)
	}
}

func TestConditionalStrategy_Default(t *testing.T) {
	reg := newTestRegistry()
	ctx := &core.RoutingContext{
		RequestedModel: core.RequestedModel{Type: "audio"},
	}
	var trace []core.TraceEntry

	defaultNode := core.StrategyNode{Type: "single", ProviderID: "deepseek", ModelID: "ds-chat"}
	targets, err := reg.Evaluate(
		context.Background(),
		core.StrategyNode{
			Type: "conditional",
			Conditions: []core.ConditionalBranch{
				{
					When: map[string]any{"requestedModel.type": "chat"},
					Then: core.StrategyNode{Type: "single", ProviderID: "openai", ModelID: "gpt-4"},
				},
			},
			Default: &defaultNode,
		},
		ctx,
		&trace,
		0,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || targets[0].ProviderID != "deepseek" {
		t.Errorf("expected default target, got %v", targets)
	}
}

func TestABSplitStrategy(t *testing.T) {
	reg := newTestRegistry()
	var trace []core.TraceEntry

	targets, err := reg.Evaluate(
		context.Background(),
		core.StrategyNode{
			Type: "ab_split",
			ABTargets: []core.ABTarget{
				{ProviderID: "openai", ModelID: "gpt-4", Weight: 100},
			},
		},
		&core.RoutingContext{},
		&trace,
		0,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || targets[0].ProviderID != "openai" {
		t.Error("expected openai from ab_split")
	}
}

// TestABSplit_EmptyTargets_EmitsTrace pins the rule that a misconfigured
// ab_split node surfaces a trace entry instead of returning silent nil.
func TestABSplit_EmptyTargets_EmitsTrace(t *testing.T) {
	reg := newTestRegistry()
	var trace []core.TraceEntry
	targets, err := reg.Evaluate(
		context.Background(),
		core.StrategyNode{Type: "ab_split"}, // zero ABTargets
		&core.RoutingContext{},
		&trace,
		0,
	)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if len(targets) != 0 {
		t.Errorf("expected 0 targets, got %d", len(targets))
	}
	if len(trace) != 1 || !contains(trace[0].Decision, "no abTargets") {
		t.Fatalf("expected empty-targets trace, got %+v", trace)
	}
}

// TestABSplit_ZeroTotalWeight_EmitsTrace pins the zero-weight path.
func TestABSplit_ZeroTotalWeight_EmitsTrace(t *testing.T) {
	reg := newTestRegistry()
	var trace []core.TraceEntry
	_, err := reg.Evaluate(
		context.Background(),
		core.StrategyNode{
			Type: "ab_split",
			ABTargets: []core.ABTarget{
				{ProviderID: "p", ModelID: "m", Weight: 0},
				{ProviderID: "q", ModelID: "n", Weight: 0},
			},
		},
		&core.RoutingContext{},
		&trace,
		0,
	)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if len(trace) != 1 || !contains(trace[0].Decision, "total weight is 0") {
		t.Fatalf("expected zero-weight trace, got %+v", trace)
	}
}

// TestABSplit_LookupFailure_EmitsTrace pins the rule that a provider lookup
// error does not disappear silently — the simulator should be able to explain
// why ab_split returned 0 targets.
func TestABSplit_LookupFailure_EmitsTrace(t *testing.T) {
	reg := NewStrategyRegistry()
	failingLookup := func(_ context.Context, providerID, modelID string) (*core.RoutingTarget, error) {
		return nil, errStub
	}
	RegisterAllStrategies(reg, failingLookup, nil)

	var trace []core.TraceEntry
	targets, err := reg.Evaluate(
		context.Background(),
		core.StrategyNode{
			Type:      "ab_split",
			ABTargets: []core.ABTarget{{ProviderID: "p", ModelID: "m", Weight: 1}},
		},
		&core.RoutingContext{},
		&trace,
		0,
	)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if len(targets) != 0 {
		t.Errorf("expected 0 targets on lookup failure, got %d", len(targets))
	}
	if len(trace) != 1 || !contains(trace[0].Decision, "lookup failed") {
		t.Fatalf("expected lookup-failure trace, got %+v", trace)
	}
}

func TestPolicyStrategy(t *testing.T) {
	reg := newTestRegistry()
	var trace []core.TraceEntry

	targets, err := reg.Evaluate(
		context.Background(),
		core.StrategyNode{Type: "policy", AllowModelIDs: []string{"gpt-4"}},
		&core.RoutingContext{},
		&trace,
		0,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 0 {
		t.Error("policy should return no targets")
	}
}

func TestModelMatchesAllowedRefs(t *testing.T) {
	refs := []store.AllowedModelRef{
		{ProviderID: "openai", ModelID: "gpt-*"},
		{ProviderID: "anthropic", ModelID: "claude-3-sonnet"},
	}

	tests := []struct {
		modelID, providerModelID, providerID string
		want                                 bool
	}{
		{"gpt-4", "gpt-4-0613", "openai", true},
		{"gpt-3.5-turbo", "gpt-3.5-turbo", "openai", true},
		{"claude-3-sonnet", "claude-3-sonnet-20240229", "anthropic", true},
		{"claude-3-haiku", "claude-3-haiku-20240307", "anthropic", false},
		{"llama-3", "llama-3-70b", "meta", false},
	}

	for _, tt := range tests {
		if got := core.ModelMatchesAllowedRefs(tt.modelID, tt.providerModelID, tt.providerID, refs); got != tt.want {
			t.Errorf("core.ModelMatchesAllowedRefs(%s, %s, %s) = %v, want %v",
				tt.modelID, tt.providerModelID, tt.providerID, got, tt.want)
		}
	}

	// Empty refs = unrestricted.
	if !core.ModelMatchesAllowedRefs("anything", "anything", "any", nil) {
		t.Error("empty refs should be unrestricted")
	}
}

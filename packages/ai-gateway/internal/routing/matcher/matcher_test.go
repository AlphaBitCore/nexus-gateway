package matcher

import (
	"net/http"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
)

// Test fixture UUIDs — chosen to keep tests readable while still
// matching the contract where core.MatchConditions.Models holds
// Model.id UUIDs (not codes).
const (
	testModelGPT4ID  = "11111111-1111-1111-1111-111111111111"
	testModelClaude3 = "22222222-2222-2222-2222-222222222222"
)

func testCtx() *core.RoutingContext {
	return &core.RoutingContext{
		RequestedModel: core.RequestedModel{
			ID:              "gpt-4",
			Type:            "chat",
			ProviderID:      "openai",
			ProviderModelID: "gpt-4-0613",
			CandidateIDs:    []string{testModelGPT4ID},
		},
		EndpointType: "chat",
		VirtualKey: &core.VKContext{
			ID:               "vk-1",
			Name:             "engineering-openai",
			OrganizationID:   "org-child",
			OrganizationPath: []string{"org-parent"},
			ProjectID:        "proj-1",
			SourceApp:        "web",
		},
		Headers: core.NewSafeHeaders(http.Header{"X-Custom": []string{"value"}}),
	}
}

func TestEvaluateExpression_Eq(t *testing.T) {
	ctx := testCtx()
	expr := map[string]any{"endpointType": "chat"}
	if !EvaluateExpression(expr, ctx) {
		t.Error("$eq should match")
	}
	expr = map[string]any{"endpointType": "embeddings"}
	if EvaluateExpression(expr, ctx) {
		t.Error("$eq should not match")
	}
}

func TestEvaluateExpression_DotNotation(t *testing.T) {
	ctx := testCtx()
	expr := map[string]any{"requestedModel.type": "chat"}
	if !EvaluateExpression(expr, ctx) {
		t.Error("dot notation should resolve")
	}
	expr = map[string]any{"virtualKey.name": "engineering-openai"}
	if !EvaluateExpression(expr, ctx) {
		t.Error("virtualKey.name should match")
	}
	expr = map[string]any{"virtualKey.organizationId": "org-child"}
	if !EvaluateExpression(expr, ctx) {
		t.Error("virtualKey.organizationId should match")
	}
}

func TestEvaluateExpression_Operators(t *testing.T) {
	ctx := testCtx()

	tests := []struct {
		name string
		expr map[string]any
		want bool
	}{
		{"$ne match", map[string]any{"endpointType": map[string]any{"$ne": "embeddings"}}, true},
		{"$ne fail", map[string]any{"endpointType": map[string]any{"$ne": "chat"}}, false},
		{"$in match", map[string]any{"requestedModel.type": map[string]any{"$in": []any{"chat", "embedding"}}}, true},
		{"$in miss", map[string]any{"requestedModel.type": map[string]any{"$in": []any{"image"}}}, false},
		{"$nin match", map[string]any{"requestedModel.type": map[string]any{"$nin": []any{"image"}}}, true},
		{"$nin miss", map[string]any{"requestedModel.type": map[string]any{"$nin": []any{"chat"}}}, false},
		{"$regex match", map[string]any{"virtualKey.name": map[string]any{"$regex": "^engineering-.*"}}, true},
		{"$regex miss", map[string]any{"virtualKey.name": map[string]any{"$regex": "^admin-.*"}}, false},
		{"$not", map[string]any{"endpointType": map[string]any{"$not": map[string]any{"$eq": "embeddings"}}}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := EvaluateExpression(tt.expr, ctx); got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEvaluateExpression_LogicalAnd(t *testing.T) {
	ctx := testCtx()
	expr := map[string]any{
		"$and": []any{
			map[string]any{"endpointType": "chat"},
			map[string]any{"requestedModel.type": "chat"},
		},
	}
	if !EvaluateExpression(expr, ctx) {
		t.Error("$and should match when all conditions match")
	}

	expr = map[string]any{
		"$and": []any{
			map[string]any{"endpointType": "chat"},
			map[string]any{"requestedModel.type": "image"},
		},
	}
	if EvaluateExpression(expr, ctx) {
		t.Error("$and should fail when any condition fails")
	}
}

func TestEvaluateExpression_LogicalOr(t *testing.T) {
	ctx := testCtx()
	expr := map[string]any{
		"$or": []any{
			map[string]any{"requestedModel.type": "image"},
			map[string]any{"requestedModel.type": "chat"},
		},
	}
	if !EvaluateExpression(expr, ctx) {
		t.Error("$or should match when any condition matches")
	}

	expr = map[string]any{
		"$or": []any{
			map[string]any{"requestedModel.type": "image"},
			map[string]any{"requestedModel.type": "audio"},
		},
	}
	if EvaluateExpression(expr, ctx) {
		t.Error("$or should fail when none match")
	}
}

func TestEvaluateExpression_RegexDoSProtection(t *testing.T) {
	ctx := testCtx()
	longPattern := make([]byte, maxRegexLen+1)
	for i := range longPattern {
		longPattern[i] = 'a'
	}
	expr := map[string]any{"virtualKey.name": map[string]any{"$regex": string(longPattern)}}
	if EvaluateExpression(expr, ctx) {
		t.Error("regex over max length should not match")
	}
}

func TestMatchGlob(t *testing.T) {
	tests := []struct {
		pattern, value string
		want           bool
	}{
		{"*", "anything", true},
		{"gpt-*", "gpt-4", true},
		{"gpt-*", "claude-3", false},
		{"exact", "exact", true},
		{"exact", "other", false},
		{"*-sonnet", "claude-3-sonnet", true},
		{"*-sonnet", "claude-3-haiku", false},
	}
	for _, tt := range tests {
		if got := MatchGlob(tt.pattern, tt.value); got != tt.want {
			t.Errorf("MatchGlob(%q, %q) = %v, want %v", tt.pattern, tt.value, got, tt.want)
		}
	}
}

func TestRuleMatchesContext(t *testing.T) {
	ctx := testCtx()

	// nil conditions = match all.
	if !RuleMatchesContext(nil, "gpt-4", ctx) {
		t.Error("nil conditions should match")
	}

	// Model match: core.MatchConditions.Models holds Model.id UUIDs and is
	// intersected against ctx.RequestedModel.CandidateIDs.
	if !RuleMatchesContext(&core.MatchConditions{Models: []string{testModelGPT4ID}}, "gpt-4", ctx) {
		t.Error("model UUID in candidates should match")
	}
	if RuleMatchesContext(&core.MatchConditions{Models: []string{testModelClaude3}}, "gpt-4", ctx) {
		t.Error("model UUID not in candidates should not match")
	}

	// VirtualKey glob.
	if !RuleMatchesContext(&core.MatchConditions{VirtualKeys: []string{"engineering-*"}}, "gpt-4", ctx) {
		t.Error("VK glob should match")
	}
	if RuleMatchesContext(&core.MatchConditions{VirtualKeys: []string{"admin-*"}}, "gpt-4", ctx) {
		t.Error("VK glob should not match")
	}

	// Projects: compared against VK.ProjectID.
	if !RuleMatchesContext(&core.MatchConditions{Projects: []string{"proj-1"}}, "gpt-4", ctx) {
		t.Error("project should match VK.ProjectID")
	}
	if RuleMatchesContext(&core.MatchConditions{Projects: []string{"proj-2"}}, "gpt-4", ctx) {
		t.Error("non-matching project should not match")
	}
	if !RuleMatchesContext(&core.MatchConditions{Projects: []string{"proj-1", "proj-2"}}, "gpt-4", ctx) {
		t.Error("project list containing match should match")
	}

	// Projects: empty conditions match all.
	if !RuleMatchesContext(&core.MatchConditions{Projects: nil}, "gpt-4", ctx) {
		t.Error("nil projects should match")
	}

	// Projects: no VK on context → cannot match.
	ctxNoVK := testCtx()
	ctxNoVK.VirtualKey = nil
	if RuleMatchesContext(&core.MatchConditions{Projects: []string{"proj-1"}}, "gpt-4", ctxNoVK) {
		t.Error("project filter should not match when VK is absent")
	}
}

// TestRuleMatchesContext_ModelsByCandidates locks the contract:
// core.MatchConditions.Models is a UUID set intersected against the request's
// hydrated CandidateIDs. Multiple candidates (alias hit) are honored;
// empty candidates with non-empty Models cannot match.
func TestRuleMatchesContext_ModelsByCandidates(t *testing.T) {
	const uuidA = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	const uuidB = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	const uuidC = "cccccccc-cccc-cccc-cccc-cccccccccccc"

	ctx := testCtx()
	ctx.RequestedModel.CandidateIDs = []string{uuidA, uuidB}

	if !RuleMatchesContext(&core.MatchConditions{Models: []string{uuidA}}, "gpt-4", ctx) {
		t.Error("Models intersecting candidates on uuidA should match")
	}
	if !RuleMatchesContext(&core.MatchConditions{Models: []string{uuidC, uuidB}}, "gpt-4", ctx) {
		t.Error("Models intersecting candidates on uuidB should match (alias hit)")
	}
	if RuleMatchesContext(&core.MatchConditions{Models: []string{uuidC}}, "gpt-4", ctx) {
		t.Error("Models with no candidate overlap should not match")
	}

	ctxEmpty := testCtx()
	ctxEmpty.RequestedModel.CandidateIDs = nil
	if RuleMatchesContext(&core.MatchConditions{Models: []string{uuidA}}, "gpt-4", ctxEmpty) {
		t.Error("non-empty Models with empty candidates should not match")
	}
}

// TestRuleMatchesContext_RequestedModelLiterals covers the
// core.MatchConditions.RequestedModelLiterals dimension. It is the only
// place that compares against the raw request string, used by the
// smart-auto-routing rule for the "auto" sentinel.
func TestRuleMatchesContext_RequestedModelLiterals(t *testing.T) {
	ctx := testCtx()

	if !RuleMatchesContext(&core.MatchConditions{RequestedModelLiterals: []string{"auto"}}, "auto", ctx) {
		t.Error("literal 'auto' should match request modelID 'auto'")
	}
	if RuleMatchesContext(&core.MatchConditions{RequestedModelLiterals: []string{"auto"}}, "gpt-4", ctx) {
		t.Error("literal 'auto' should not match request modelID 'gpt-4'")
	}
	if !RuleMatchesContext(&core.MatchConditions{RequestedModelLiterals: nil}, "anything", ctx) {
		t.Error("empty literals should not constrain matching")
	}
}

// TestRuleMatchesContext_BothFields_AreANDed: when both Models and
// RequestedModelLiterals are set, both must match.
func TestRuleMatchesContext_BothFields_AreANDed(t *testing.T) {
	conds := &core.MatchConditions{
		Models:                 []string{testModelGPT4ID},
		RequestedModelLiterals: []string{"auto"},
	}

	// AND happy path — candidates contain the UUID AND request string == "auto".
	ctxBoth := testCtx()
	ctxBoth.RequestedModel.CandidateIDs = []string{testModelGPT4ID}
	if !RuleMatchesContext(conds, "auto", ctxBoth) {
		t.Error("both Models (via candidates) and literal ('auto') match — should pass")
	}

	// AND fails when the literal does not match even though Models does.
	if RuleMatchesContext(conds, "gpt-4", ctxBoth) {
		t.Error("Models matches via candidates but literal 'auto' missing from request 'gpt-4' — should fail")
	}

	// AND fails when Models has no candidate overlap even though the literal matches.
	ctxNoCand := testCtx()
	ctxNoCand.RequestedModel.CandidateIDs = nil
	if RuleMatchesContext(conds, "auto", ctxNoCand) {
		t.Error("literal 'auto' matches but Models has no candidate overlap — should fail")
	}
}

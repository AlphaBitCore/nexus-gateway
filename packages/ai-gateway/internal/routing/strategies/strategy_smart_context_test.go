package strategies

import (
	"context"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/llm"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// The context-window hard filter drops candidates whose declared
// MaxContextTokens cannot hold the estimated request plus the output
// reserve, BEFORE the catalog is rendered for the router LLM — so the
// router can never select a model the request provably overflows.
//
// Token math used by these tests (inputstaging.EstimateTokensConservative,
// the upper-bound estimate the fit filter uses): ASCII counts 0.5
// tokens/char, so strings.Repeat("a", N) estimates to ceil(0.5*N) tokens.
// The default output reserve is 1024 tokens when the request carries no
// maxTokens. The boundaries below have wide margins so the exact rate is
// not load-bearing; TestSmart_ContextFilter_ConservativeSizing pins the
// rate directly.

func ctxRctx(msgs []normalize.Message, maxTokens *int, tools []normalize.ToolDef) *core.RoutingContext {
	p := &normalize.NormalizedPayload{
		Kind:     normalize.KindAIChat,
		Messages: msgs,
		Tools:    tools,
	}
	if maxTokens != nil {
		p.Params = &normalize.SamplingParam{MaxTokens: maxTokens}
	}
	return &core.RoutingContext{Request: p}
}

func textMsg(role normalize.Role, text string) normalize.Message {
	return normalize.Message{Role: role, Content: []normalize.ContentBlock{{Type: normalize.ContentText, Text: text}}}
}

func intPtr(v int) *int { return &v }

// TestSmart_ContextFilter_DropsModelTooSmallForHistory is the
// incident-shaped case: the user turn is tiny but the assistant history
// is huge. The small-context model must be filtered out of the catalog
// even though the router LLM only ever sees the (tiny) user content.
func TestSmart_ContextFilter_DropsModelTooSmallForHistory(t *testing.T) {
	small := core.SmartModelRow{ModelID: "m-small", ModelCode: "tiny-ctx", ProviderID: "p1", MaxContextTokens: intPtr(1000)}
	big := core.SmartModelRow{ModelID: "m-big", ModelCode: "grande-ctx", ProviderID: "p1", MaxContextTokens: intPtr(100000)}
	decider := &fakeDecider{decision: llm.Decision{ModelID: "grande-ctx", Reason: "fits"}}
	fx := newSmartFixture(t, decider, []core.SmartModelRow{small, big})
	node := core.StrategyNode{RouterProviderID: "p-router", RouterModelID: "m-router"}
	trace := []core.TraceEntry{}
	strat := &SmartStrategy{deps: fx.deps()}

	// 8000 ASCII chars of assistant history ≈ 2000 tokens; + 1024 reserve
	// = 3024 required > 1000 (small) but ≤ 100000 (big).
	rctx := ctxRctx([]normalize.Message{
		textMsg(normalize.RoleAssistant, strings.Repeat("a", 8000)),
		textMsg(normalize.RoleUser, "continue"),
	}, nil, nil)

	out, err := strat.Evaluate(context.Background(), node, rctx, &trace, 0, nil)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(out) != 1 || out[0].ModelID != "m-big" {
		t.Fatalf("unexpected targets: %+v", out)
	}
	if strings.Contains(decider.lastReq.SystemPrompt, "tiny-ctx") {
		t.Errorf("catalog still contains the too-small model")
	}
	if !strings.Contains(decider.lastReq.SystemPrompt, "grande-ctx") {
		t.Errorf("catalog lost the fitting model")
	}
	found := false
	for _, e := range trace {
		if strings.Contains(e.Decision, "context filter") {
			found = true
		}
	}
	if !found {
		t.Errorf("no context-filter trace entry recorded: %+v", trace)
	}
}

// TestSmart_ContextFilter_ConservativeSizing is the #35 regression guard:
// a request sized between the average-case estimate (0.25/char) and the
// conservative estimate (0.5/char) must drop a model that only the average
// estimate would have admitted. 10000 ASCII chars estimate to 2500 tokens
// under 0.25 (required 2600 ≤ 3000 → boundary WOULD survive) but 5000
// tokens under 0.5 (required 5100 > 3000 → boundary DROPPED). Under-counting
// here is exactly the mis-route that 400s upstream.
func TestSmart_ContextFilter_ConservativeSizing(t *testing.T) {
	boundary := core.SmartModelRow{ModelID: "m-boundary", ModelCode: "boundary-ctx", ProviderID: "p1", MaxContextTokens: intPtr(3000)}
	big := core.SmartModelRow{ModelID: "m-big", ModelCode: "grande-ctx", ProviderID: "p1", MaxContextTokens: intPtr(100000)}
	decider := &fakeDecider{decision: llm.Decision{ModelID: "grande-ctx", Reason: "fits"}}
	fx := newSmartFixture(t, decider, []core.SmartModelRow{boundary, big})
	node := core.StrategyNode{RouterProviderID: "p-router", RouterModelID: "m-router"}
	trace := []core.TraceEntry{}
	strat := &SmartStrategy{deps: fx.deps()}

	// maxTokens=100 → reserve=100. 10000 ASCII chars: 2500 tok (avg) vs
	// 5000 tok (conservative @ 0.5/char). required = est + 100.
	rctx := ctxRctx([]normalize.Message{
		textMsg(normalize.RoleUser, strings.Repeat("a", 10000)),
	}, intPtr(100), nil)

	out, err := strat.Evaluate(context.Background(), node, rctx, &trace, 0, nil)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(out) != 1 || out[0].ModelID != "m-big" {
		t.Fatalf("conservative sizing must drop the boundary model that only the average estimate admits; got %+v", out)
	}
	if strings.Contains(decider.lastReq.SystemPrompt, "boundary-ctx") {
		t.Errorf("catalog still contains the boundary model the average estimate would have kept")
	}
}

// TestSmart_ContextFilter_UnknownMaxCtxKept: a candidate without
// declared MaxContextTokens must survive the filter (fail-open) — the
// filter only acts on data the catalog actually has.
func TestSmart_ContextFilter_UnknownMaxCtxKept(t *testing.T) {
	unknown := core.SmartModelRow{ModelID: "m-unknown", ModelCode: "mystery", ProviderID: "p1"}
	small := core.SmartModelRow{ModelID: "m-small", ModelCode: "tiny-ctx", ProviderID: "p1", MaxContextTokens: intPtr(1000)}
	decider := &fakeDecider{decision: llm.Decision{ModelID: "mystery", Reason: "only fit"}}
	fx := newSmartFixture(t, decider, []core.SmartModelRow{unknown, small})
	node := core.StrategyNode{RouterProviderID: "p-router", RouterModelID: "m-router"}
	trace := []core.TraceEntry{}
	strat := &SmartStrategy{deps: fx.deps()}

	rctx := ctxRctx([]normalize.Message{
		textMsg(normalize.RoleAssistant, strings.Repeat("a", 8000)),
		textMsg(normalize.RoleUser, "go on"),
	}, nil, nil)

	out, err := strat.Evaluate(context.Background(), node, rctx, &trace, 0, nil)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(out) != 1 || out[0].ModelID != "m-unknown" {
		t.Fatalf("unexpected targets: %+v", out)
	}
	if !strings.Contains(decider.lastReq.SystemPrompt, "mystery") {
		t.Errorf("unknown-MaxCtx candidate was filtered out")
	}
	if strings.Contains(decider.lastReq.SystemPrompt, "tiny-ctx") {
		t.Errorf("too-small candidate survived the filter")
	}
}

// TestSmart_ContextFilter_NothingFits_KeepsLargest: when every declared
// context window is too small, the filter keeps the largest-context
// candidate(s) instead of emptying the pool — the largest model has the
// best chance given the estimate is coarse, and silently falling back to
// a (typically small) default would guarantee the overflow.
func TestSmart_ContextFilter_NothingFits_KeepsLargest(t *testing.T) {
	small := core.SmartModelRow{ModelID: "m-small", ModelCode: "tiny-ctx", ProviderID: "p1", MaxContextTokens: intPtr(1000)}
	mid := core.SmartModelRow{ModelID: "m-mid", ModelCode: "mid-ctx", ProviderID: "p1", MaxContextTokens: intPtr(2000)}
	decider := &fakeDecider{decision: llm.Decision{ModelID: "mid-ctx", Reason: "largest"}}
	fx := newSmartFixture(t, decider, []core.SmartModelRow{small, mid})
	node := core.StrategyNode{RouterProviderID: "p-router", RouterModelID: "m-router"}
	trace := []core.TraceEntry{}
	strat := &SmartStrategy{deps: fx.deps()}

	// 20000 ASCII chars ≈ 5000 tokens; + 1024 reserve > both candidates.
	rctx := ctxRctx([]normalize.Message{
		textMsg(normalize.RoleUser, strings.Repeat("a", 20000)),
	}, nil, nil)

	out, err := strat.Evaluate(context.Background(), node, rctx, &trace, 0, nil)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(out) != 1 || out[0].ModelID != "m-mid" {
		t.Fatalf("unexpected targets: %+v", out)
	}
	if strings.Contains(decider.lastReq.SystemPrompt, "tiny-ctx") {
		t.Errorf("smaller candidate should not survive when nothing fits")
	}
	if !strings.Contains(decider.lastReq.SystemPrompt, "mid-ctx") {
		t.Errorf("largest candidate must be kept when nothing fits")
	}
	found := false
	for _, e := range trace {
		if strings.Contains(e.Decision, "no candidate fits") {
			found = true
		}
	}
	if !found {
		t.Errorf("no overflow-risk trace entry recorded: %+v", trace)
	}
}

// TestSmart_ContextFilter_RespectsRequestedMaxTokens: the output reserve
// is the request's maxTokens when present, the built-in 1024 otherwise.
// Same input, same candidate — only maxTokens flips the verdict.
func TestSmart_ContextFilter_RespectsRequestedMaxTokens(t *testing.T) {
	small := core.SmartModelRow{ModelID: "m-small", ModelCode: "tiny-ctx", ProviderID: "p1", MaxContextTokens: intPtr(2000)}
	big := core.SmartModelRow{ModelID: "m-big", ModelCode: "grande-ctx", ProviderID: "p1", MaxContextTokens: intPtr(100000)}
	msgs := []normalize.Message{textMsg(normalize.RoleUser, strings.Repeat("a", 100))} // ≈ 25 tokens

	t.Run("default reserve keeps the small model", func(t *testing.T) {
		decider := &fakeDecider{decision: llm.Decision{ModelID: "tiny-ctx", Reason: "cheap"}}
		fx := newSmartFixture(t, decider, []core.SmartModelRow{small, big})
		trace := []core.TraceEntry{}
		strat := &SmartStrategy{deps: fx.deps()}
		node := core.StrategyNode{RouterProviderID: "p-router", RouterModelID: "m-router"}

		// 25 + 1024 = 1049 ≤ 2000 → small stays routable. The larger
		// candidate rides along as the armed context-upgrade escape.
		out, err := strat.Evaluate(context.Background(), node, ctxRctx(msgs, nil, nil), &trace, 0, nil)
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if len(out) != 2 || out[0].ModelID != "m-small" || out[0].ContextUpgradeOnly {
			t.Fatalf("unexpected targets: %+v", out)
		}
		if out[1].ModelID != "m-big" || !out[1].ContextUpgradeOnly {
			t.Fatalf("expected armed upgrade target: %+v", out[1])
		}
		if !strings.Contains(decider.lastReq.SystemPrompt, "tiny-ctx") {
			t.Errorf("small model must stay in catalog under default reserve")
		}
	})

	t.Run("requested maxTokens drops the small model", func(t *testing.T) {
		decider := &fakeDecider{decision: llm.Decision{ModelID: "grande-ctx", Reason: "fits"}}
		fx := newSmartFixture(t, decider, []core.SmartModelRow{small, big})
		trace := []core.TraceEntry{}
		strat := &SmartStrategy{deps: fx.deps()}
		node := core.StrategyNode{RouterProviderID: "p-router", RouterModelID: "m-router"}

		// 25 + 2000 = 2025 > 2000 → small is provably too small.
		out, err := strat.Evaluate(context.Background(), node, ctxRctx(msgs, intPtr(2000), nil), &trace, 0, nil)
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if len(out) != 1 || out[0].ModelID != "m-big" {
			t.Fatalf("unexpected targets: %+v", out)
		}
		if strings.Contains(decider.lastReq.SystemPrompt, "tiny-ctx") {
			t.Errorf("small model must be dropped when maxTokens overflows it")
		}
	})
}

// TestSmart_ContextFilter_CountsAllRequestContent: the estimate must
// cover every channel that consumes upstream context — assistant text,
// tool-use input JSON, tool-result output, and tool definitions — not
// just the role=user text the router LLM is shown.
func TestSmart_ContextFilter_CountsAllRequestContent(t *testing.T) {
	bulk := strings.Repeat("a", 8000) // ≈ 2000 tokens, overflowing a 2000-token window with any reserve
	cases := []struct {
		name string
		msgs []normalize.Message
		tool []normalize.ToolDef
	}{
		{
			name: "assistant text",
			msgs: []normalize.Message{textMsg(normalize.RoleAssistant, bulk), textMsg(normalize.RoleUser, "hi")},
		},
		{
			name: "tool-use input",
			msgs: []normalize.Message{
				{Role: normalize.RoleAssistant, Content: []normalize.ContentBlock{{Type: normalize.ContentToolUse, ToolUse: &normalize.ToolUse{Name: "search", Input: map[string]any{"data": bulk}}}}},
				textMsg(normalize.RoleUser, "hi"),
			},
		},
		{
			name: "tool-result output",
			msgs: []normalize.Message{
				{Role: normalize.RoleUser, Content: []normalize.ContentBlock{{Type: normalize.ContentToolResult, ToolResult: &normalize.ToolResult{Output: bulk}}}},
				textMsg(normalize.RoleUser, "hi"),
			},
		},
		{
			name: "tool definitions",
			msgs: []normalize.Message{textMsg(normalize.RoleUser, "hi")},
			tool: []normalize.ToolDef{{Name: "search", Description: bulk}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			small := core.SmartModelRow{ModelID: "m-small", ModelCode: "tiny-ctx", ProviderID: "p1", MaxContextTokens: intPtr(2000)}
			big := core.SmartModelRow{ModelID: "m-big", ModelCode: "grande-ctx", ProviderID: "p1", MaxContextTokens: intPtr(100000)}
			decider := &fakeDecider{decision: llm.Decision{ModelID: "grande-ctx", Reason: "fits"}}
			fx := newSmartFixture(t, decider, []core.SmartModelRow{small, big})
			trace := []core.TraceEntry{}
			strat := &SmartStrategy{deps: fx.deps()}
			node := core.StrategyNode{RouterProviderID: "p-router", RouterModelID: "m-router"}

			out, err := strat.Evaluate(context.Background(), node, ctxRctx(tc.msgs, nil, tc.tool), &trace, 0, nil)
			if err != nil {
				t.Fatalf("Evaluate: %v", err)
			}
			if len(out) != 1 || out[0].ModelID != "m-big" {
				t.Fatalf("unexpected targets: %+v", out)
			}
			if strings.Contains(decider.lastReq.SystemPrompt, "tiny-ctx") {
				t.Errorf("%s content was not counted against the context window", tc.name)
			}
		})
	}
}

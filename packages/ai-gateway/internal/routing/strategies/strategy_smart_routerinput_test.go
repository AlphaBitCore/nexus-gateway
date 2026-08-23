package strategies

import (
	"context"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/llm"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// Router-input contract: the smart strategy hands the Decider the recent
// user+assistant conversation (client system messages excluded), the
// router model's real context window, and a request-metadata line in the
// system prompt so the router can match capability needs it cannot see
// from text alone.

func TestSmart_RouterInput_IncludesAssistantExcludesSystem(t *testing.T) {
	decider := &fakeDecider{decision: llm.Decision{ModelID: "m-claude", Reason: "ok"}}
	candidates := []core.SmartModelRow{{ModelID: "m-claude", ModelCode: "claude", ProviderID: "p1"}}
	fx := newSmartFixture(t, decider, candidates)
	node := core.StrategyNode{RouterProviderID: "p-router", RouterModelID: "m-router"}
	trace := []core.TraceEntry{}
	strat := &SmartStrategy{deps: fx.deps()}

	rctx := &core.RoutingContext{Request: &normalize.NormalizedPayload{
		Kind: normalize.KindAIChat,
		Messages: []normalize.Message{
			textMsg(normalize.RoleSystem, "you are a haiku bot"),
			textMsg(normalize.RoleUser, "write a haiku"),
			textMsg(normalize.RoleAssistant, "cherry petals drift"),
			textMsg(normalize.RoleUser, "another one"),
		},
	}}

	if _, err := strat.Evaluate(context.Background(), node, fx.pool(rctx), &trace); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	var roles []string
	for _, m := range decider.lastReq.Messages {
		roles = append(roles, string(m.Role))
	}
	want := []string{"user", "assistant", "user"}
	if len(roles) != len(want) {
		t.Fatalf("decider received roles %v, want %v (assistant in, system out)", roles, want)
	}
	for i := range want {
		if roles[i] != want[i] {
			t.Fatalf("decider received roles %v, want %v", roles, want)
		}
	}
}

func TestSmart_RouterInput_PassesRouterModelContextLimit(t *testing.T) {
	decider := &fakeDecider{decision: llm.Decision{ModelID: "m-claude", Reason: "ok"}}
	// The router model is among enabled chat models but NOT in the VK
	// allowlist — its window must still be found (lookup runs pre-VK-filter).
	candidates := []core.SmartModelRow{
		{ModelID: "m-claude", ModelCode: "claude", ProviderID: "p1"},
		{ModelID: "m-router", ModelCode: "router-mini", ProviderID: "p-router", MaxContextTokens: intPtr(16384)},
	}
	fx := newSmartFixture(t, decider, candidates)
	node := core.StrategyNode{RouterProviderID: "p-router", RouterModelID: "m-router"}
	trace := []core.TraceEntry{}
	strat := &SmartStrategy{deps: fx.deps()}

	rctx := aiChatRctx()
	rctx.VirtualKey = &core.VKContext{AllowedModels: []store.AllowedModelRef{{ProviderID: "p1", ModelID: "m-claude"}}}

	if _, err := strat.Evaluate(context.Background(), node, fx.pool(rctx), &trace); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if decider.lastReq.RouterContextLimit != 16384 {
		t.Errorf("RouterContextLimit = %d, want 16384 (router model's declared window)", decider.lastReq.RouterContextLimit)
	}
}

func TestSmart_RouterInput_UndeclaredRouterWindowStaysZero(t *testing.T) {
	decider := &fakeDecider{decision: llm.Decision{ModelID: "m-claude", Reason: "ok"}}
	candidates := []core.SmartModelRow{{ModelID: "m-claude", ModelCode: "claude", ProviderID: "p1"}}
	fx := newSmartFixture(t, decider, candidates)
	node := core.StrategyNode{RouterProviderID: "p-router", RouterModelID: "m-unknown-router"}
	trace := []core.TraceEntry{}
	strat := &SmartStrategy{deps: fx.deps()}

	if _, err := strat.Evaluate(context.Background(), node, fx.pool(aiChatRctx()), &trace); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if decider.lastReq.RouterContextLimit != 0 {
		t.Errorf("RouterContextLimit = %d, want 0 (undeclared → prompt builder falls back)", decider.lastReq.RouterContextLimit)
	}
}

func TestSmart_RouterInput_SystemPromptCarriesRequestMetadataLine(t *testing.T) {
	decider := &fakeDecider{decision: llm.Decision{ModelID: "m-claude", Reason: "ok"}}
	candidates := []core.SmartModelRow{{ModelID: "m-claude", ModelCode: "claude", ProviderID: "p1"}}
	fx := newSmartFixture(t, decider, candidates)
	node := core.StrategyNode{RouterProviderID: "p-router", RouterModelID: "m-router"}
	trace := []core.TraceEntry{}
	strat := &SmartStrategy{deps: fx.deps()}

	rctx := &core.RoutingContext{Request: &normalize.NormalizedPayload{
		Kind: normalize.KindAIChat,
		Messages: []normalize.Message{
			{Role: normalize.RoleUser, Content: []normalize.ContentBlock{
				{Type: normalize.ContentText, Text: "describe these"},
				{Type: normalize.ContentMedia, MediaRef: &normalize.MediaRef{Modality: normalize.ModalityImage, SizeBytes: 10, Mime: "image/png", SHA256: "a"}},
				{Type: normalize.ContentMedia, MediaRef: &normalize.MediaRef{Modality: normalize.ModalityImage, SizeBytes: 10, Mime: "image/png", SHA256: "b"}},
			}},
		},
		Tools: []normalize.ToolDef{{Name: "search"}, {Name: "calc"}, {Name: "fetch"}},
	}}

	if _, err := strat.Evaluate(context.Background(), node, fx.pool(rctx), &trace); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	sp := decider.lastReq.SystemPrompt
	if !strings.Contains(sp, "2 images") {
		t.Errorf("system prompt missing image count metadata: %q", tail(sp))
	}
	if !strings.Contains(sp, "3 tool definitions") {
		t.Errorf("system prompt missing tool count metadata: %q", tail(sp))
	}
	if !strings.Contains(sp, "input tokens") {
		t.Errorf("system prompt missing estimated token metadata: %q", tail(sp))
	}
}

// tail returns the last 200 bytes of s for compact failure messages.
func tail(s string) string {
	if len(s) <= 200 {
		return s
	}
	return "..." + s[len(s)-200:]
}

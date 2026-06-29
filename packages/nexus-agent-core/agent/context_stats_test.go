package agent

import (
	"context"
	"github.com/goccy/go-json"
	"strings"
	"testing"
)

func TestEstimateTokens(t *testing.T) {
	cases := map[string]int{"": 0, "abcd": 1, "abcde": 2, "12345678": 2}
	for s, want := range cases {
		if got := estimateTokens(s); got != want {
			t.Errorf("estimateTokens(%q) = %d, want %d", s, got, want)
		}
	}
}

func TestEstimateMessagesAndSchemas(t *testing.T) {
	msgs := []Message{{Role: RoleUser, Blocks: []Block{
		{Type: BlockText, Text: strings.Repeat("a", 40)},                      // 10
		{Type: BlockToolUse, Input: json.RawMessage(strings.Repeat("x", 40))}, // 10
	}}}
	if got := estimateMessages(msgs); got != 20 {
		t.Fatalf("estimateMessages = %d, want 20", got)
	}
	tools := []ToolSchema{{Name: "abcd", Description: strings.Repeat("d", 40), Parameters: json.RawMessage(strings.Repeat("p", 40))}}
	if got := estimateSchemas(tools); got != 1+10+10 {
		t.Fatalf("estimateSchemas = %d, want 21", got)
	}
}

func TestContextStatsCalibration(t *testing.T) {
	system := strings.Repeat("s", 160)                                                                      // 40
	tools := []ToolSchema{{Name: "t", Parameters: json.RawMessage(strings.Repeat("p", 160))}}               // ~41
	bundle := strings.Repeat("b", 40)                                                                       // 10
	conv := []Message{{Role: RoleUser, Blocks: []Block{{Type: BlockText, Text: strings.Repeat("a", 400)}}}} // 100

	// Calibrated to the exact Used; parts sum exactly.
	cs := contextStats(system, tools, bundle, conv, &Usage{PromptTokens: 2000, CachedTokens: 1500})
	if cs.Used != 2000 || cs.Cached != 1500 {
		t.Fatalf("exact totals wrong: %+v", cs)
	}
	if cs.System+cs.Tools+cs.History+cs.Bundle != cs.Used {
		t.Fatalf("calibrated parts must sum to Used: %+v", cs)
	}
	if cs.System <= 0 || cs.History <= 0 {
		t.Fatalf("components should be positive: %+v", cs)
	}

	// No usage → raw (uncalibrated) estimates, Used == 0.
	raw := contextStats(system, tools, bundle, conv, nil)
	if raw.Used != 0 || raw.System != estimateTokens(system) {
		t.Fatalf("raw estimates expected without usage: %+v", raw)
	}

	// History clamps at 0 when the bundle estimate exceeds the messages estimate.
	clamp := contextStats("", nil, strings.Repeat("b", 4000), []Message{{Role: RoleUser, Blocks: []Block{{Text: "a"}}}}, nil)
	if clamp.History < 0 {
		t.Fatalf("history must clamp >= 0, got %d", clamp.History)
	}
}

func agentWithOnContext(t *testing.T, model Model, reg *Registry, onCtx func(ContextStats)) *Agent {
	t.Helper()
	dir := t.TempDir()
	return New(Config{
		Model:     model,
		Registry:  reg,
		Gate:      NewGate(NewCommandClassifier(), nil, false),
		Memory:    OpenMemoryStore(dir, "local"),
		Store:     openStoreAt(dir),
		Compactor: NewCompactor(model, 0),
		Situation: fakeSituation{s: Situation{Health: "ok"}},
		Env:       "local",
		Session:   NewSession("local"),
		OnContext: onCtx,
	})
}

func TestAgentTurnFiresOnContext(t *testing.T) {
	fm := newFakeModel(&ModelResponse{
		Message: TextMessage(RoleAssistant, "ok"), StopReason: StopEndTurn,
		Usage: &Usage{PromptTokens: 1200, CachedTokens: 900},
	})
	var got *ContextStats
	a := agentWithOnContext(t, fm, NewRegistry(), func(cs ContextStats) { got = &cs })
	if _, err := a.Turn(context.Background(), "hello", "Mission Control"); err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if got == nil {
		t.Fatal("OnContext must fire after a turn with usage")
	}
	if got.Used != 1200 || got.Cached != 900 {
		t.Fatalf("exact totals: %+v", got)
	}
	if got.System+got.Tools+got.History+got.Bundle != got.Used {
		t.Fatalf("calibrated parts must sum to Used: %+v", got)
	}

	// A turn whose model reports no usage must not fire OnContext (keeps the prior value).
	fm2 := newFakeModel(&ModelResponse{Message: TextMessage(RoleAssistant, "ok"), StopReason: StopEndTurn})
	fired := false
	a2 := agentWithOnContext(t, fm2, NewRegistry(), func(ContextStats) { fired = true })
	if _, err := a2.Turn(context.Background(), "hi", ""); err != nil {
		t.Fatal(err)
	}
	if fired {
		t.Fatal("OnContext must not fire when the model reported no usage")
	}
}

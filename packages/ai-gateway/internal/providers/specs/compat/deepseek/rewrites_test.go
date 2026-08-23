package deepseek

import "testing"

// The thinking family is the whole deepseek-v4 family plus deepseek-reasoner
// — every model that imposes the reasoning_content back-fill and forced-
// tool_choice quirks. deepseek-v4-flash is INCLUDED: it 400'd on a real
// tool-loop history whose assistant turns dropped reasoning_content, exactly
// because an exact-suffix denylist had listed only -pro and -reasoner.
// Non-v4 chat/coder families keep the caller's body untouched.
func TestIsThinkingModel(t *testing.T) {
	for _, m := range []string{"deepseek-reasoner", "deepseek-v4-pro", "deepseek-v4-pro-0610", "deepseek-v4-flash", "deepseek-v4-flash-0725"} {
		if !IsThinkingModel(m) {
			t.Fatalf("%s must be a thinking model", m)
		}
	}
	// Generations past 4 that do not exist yet. A rule frozen at today's
	// generation reaches its first caller as the 400 it was written to
	// prevent — which is exactly how deepseek-v4-flash reached callers when
	// the rule named only -pro and -reasoner.
	for _, m := range []string{"deepseek-v5", "deepseek-v5-pro", "deepseek-v6-flash", "deepseek-v12"} {
		if !IsThinkingModel(m) {
			t.Fatalf("%s is a later generation of the thinking line and must be covered", m)
		}
	}
	// Below the evidenced generation, and ids that are not a generation at
	// all: untouched, because a speculative structural rewrite is forbidden
	// (§3a Rule 7) and these have no observed 400.
	for _, m := range []string{"deepseek-chat", "deepseek-coder", "deepseek-v3.1-thinking", "deepseek-v3", "deepseek-vision", "deepseek-v", ""} {
		if IsThinkingModel(m) {
			t.Fatalf("%s must NOT be a thinking model", m)
		}
	}
}

// TestApplyRewrites (live 400: "Thinking mode does not support this
// tool_choice"): a forced tool_choice — "required" or a named function — is
// stripped on thinking models (tools stay; default behavior still calls
// them), "auto"/"none" pass through, and non-thinking models are untouched.
func TestApplyThinkingRules(t *testing.T) {
	p := map[string]any{"tool_choice": "required", "tools": []any{"x"}}
	if got := applyThinkingRules(p, "deepseek-v4-pro"); len(got) != 1 || got[0] != "tool_choice→removed" {
		t.Fatalf("rewrites = %v", got)
	}
	if _, ok := p["tool_choice"]; ok {
		t.Fatal("forced tool_choice must be removed")
	}
	if _, ok := p["tools"]; !ok {
		t.Fatal("tools must survive — the model can still call them")
	}

	named := map[string]any{"tool_choice": map[string]any{"type": "function", "function": map[string]any{"name": "f"}}}
	if got := applyThinkingRules(named, "deepseek-reasoner"); len(got) != 1 {
		t.Fatalf("a named selection is forced; rewrites = %v", got)
	}

	auto := map[string]any{"tool_choice": "auto"}
	if got := applyThinkingRules(auto, "deepseek-v4-pro"); got != nil {
		t.Fatalf("auto must pass through, got %v", got)
	}
	if auto["tool_choice"] != "auto" {
		t.Fatal("auto must remain")
	}

	// The model gate lives on the contract's structural rule, not inside
	// the apply function: a non-thinking model never reaches it.
	rule := Contract().ChatStructural[0]
	if rule.Applies("deepseek-chat") {
		t.Fatal("deepseek-chat must not gate-match the thinking rules")
	}
	if !rule.Applies("deepseek-v4-pro") || !rule.Applies("deepseek-reasoner") {
		t.Fatal("the evidenced thinking families must gate-match")
	}

	if got := applyThinkingRules(map[string]any{"messages": []any{}}, "deepseek-v4-pro"); got != nil {
		t.Fatalf("absent tool_choice needs no rewrite, got %v", got)
	}
}

// In thinking mode DeepSeek 400s ("The `reasoning_content` in the thinking
// mode must be passed back to the API.") when an assistant turn carrying
// tool_calls comes back without reasoning_content — observed in prod on
// deepseek-v4-pro. Probing bounded the rule: only tool_calls turns are
// checked, and the upstream tests key PRESENCE, so "" satisfies it.
func assistantWithToolCalls(extra map[string]any) map[string]any {
	m := map[string]any{
		"role":    "assistant",
		"content": "",
		"tool_calls": []any{map[string]any{
			"id": "call_1", "type": "function",
			"function": map[string]any{"name": "f", "arguments": "{}"},
		}},
	}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func TestApplyThinkingRules_fillsReasoningContentOnToolCallTurns(t *testing.T) {
	payload := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": "hi"},
		assistantWithToolCalls(nil),
		map[string]any{"role": "tool", "tool_call_id": "call_1", "content": "ok"},
	}}
	got := applyThinkingRules(payload, "deepseek-v4-pro")

	msg := payload["messages"].([]any)[1].(map[string]any)
	rc, has := msg["reasoning_content"]
	if !has {
		t.Fatalf("assistant tool_calls turn must carry reasoning_content, got %v", msg)
	}
	if rc != "" {
		t.Errorf("filled value = %q, want the empty string (we must not fabricate reasoning)", rc)
	}
	// The coercion must be observable via x-nexus-coerced / traffic_event.
	want := "reasoning_content→filled_on_1_assistant_turns"
	if len(got) != 1 || got[0] != want {
		t.Errorf("rewrites = %v, want [%s]", got, want)
	}
}

func TestApplyThinkingRules_preservesCallerReasoningContent(t *testing.T) {
	payload := map[string]any{"messages": []any{
		assistantWithToolCalls(map[string]any{"reasoning_content": "the caller's real thinking"}),
	}}
	got := applyThinkingRules(payload, "deepseek-v4-pro")

	msg := payload["messages"].([]any)[0].(map[string]any)
	if msg["reasoning_content"] != "the caller's real thinking" {
		t.Errorf("a caller-supplied reasoning_content must never be overwritten, got %v", msg["reasoning_content"])
	}
	if len(got) != 0 {
		t.Errorf("nothing to fill → no rewrite, got %v", got)
	}
}

func TestApplyThinkingRules_leavesPlainAssistantTurnsAlone(t *testing.T) {
	// A plain assistant turn without reasoning_content is ACCEPTED upstream
	// (probed), so filling it would rewrite a body the upstream never objected
	// to — the rule is scoped to tool_calls turns.
	payload := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": "hi"},
		map[string]any{"role": "assistant", "content": "hello"},
	}}
	got := applyThinkingRules(payload, "deepseek-v4-pro")

	msg := payload["messages"].([]any)[1].(map[string]any)
	if _, has := msg["reasoning_content"]; has {
		t.Errorf("plain assistant turn must be left untouched, got %v", msg)
	}
	if len(got) != 0 {
		t.Errorf("rewrites = %v, want none", got)
	}
}

func TestContract_NonThinkingModel_NeverGates(t *testing.T) {
	// deepseek-chat / deepseek-coder never reach the structural apply:
	// the gate is the contract rule's Applies, and a non-matching model
	// keeps its forcing and its unfilled history.
	rule := Contract().ChatStructural[0]
	for _, m := range []string{"deepseek-chat", "deepseek-coder", ""} {
		if rule.Applies(m) {
			t.Fatalf("%q must not gate-match", m)
		}
	}
}

func TestApplyThinkingRules_fillsEveryToolCallTurnInAMultiTurnHistory(t *testing.T) {
	// An agent loop replays its whole history; every prior tool_calls turn
	// needs the key, not just the last one.
	payload := map[string]any{"messages": []any{
		assistantWithToolCalls(nil),
		map[string]any{"role": "tool", "tool_call_id": "call_1", "content": "ok"},
		assistantWithToolCalls(nil),
	}}
	got := applyThinkingRules(payload, "deepseek-v4-pro")

	for i, idx := range []int{0, 2} {
		msg := payload["messages"].([]any)[idx].(map[string]any)
		if _, has := msg["reasoning_content"]; !has {
			t.Errorf("turn %d (index %d) not filled: %v", i, idx, msg)
		}
	}
	want := "reasoning_content→filled_on_2_assistant_turns"
	if len(got) != 1 || got[0] != want {
		t.Errorf("rewrites = %v, want [%s]", got, want)
	}
}

// TestApplyThinkingRules_fillsEveryAssistantTurnWhenToolsArePresent pins the
// trigger the tool_calls gate missed: it is the REQUEST carrying `tools`, not
// the individual turn carrying `tool_calls`.
//
// Probed against api.taskforce10x.com → deepseek-v4-pro on 2026-08-19, three
// shapes, one variable at a time, reasoning_content absent unless stated:
//
//	D  plain assistant turns, NO tools                    → 200
//	E  plain assistant turns, WITH tools                  → 400 "The `reasoning_content`
//	                                                        in the thinking mode must be
//	                                                        passed back to the API."
//	F  plain assistant turns, WITH tools, rc:"" present   → 200
//
// D is why TestApplyThinkingRules_leavesPlainAssistantTurnsAlone is right and
// stays: with no tools the upstream never objects, so filling would rewrite a
// body it accepted. E is the staging failure — a 17-tool request whose
// assistant turns were plain text and had never called anything. F shows the
// empty presence marker is sufficient, so the fix still fabricates nothing.
func TestApplyThinkingRules_fillsEveryAssistantTurnWhenToolsArePresent(t *testing.T) {
	tools := []any{map[string]any{"type": "function", "function": map[string]any{"name": "get_time"}}}
	payload := map[string]any{
		"tools": tools,
		"messages": []any{
			map[string]any{"role": "system", "content": "be brief"},
			map[string]any{"role": "user", "content": "q"},
			map[string]any{"role": "assistant", "content": "first"},
			map[string]any{"role": "assistant", "content": "second"},
		},
	}
	got := applyThinkingRules(payload, "deepseek-v4-pro")

	msgs := payload["messages"].([]any)
	for _, i := range []int{2, 3} {
		msg := msgs[i].(map[string]any)
		rc, has := msg["reasoning_content"]
		if !has {
			t.Fatalf("messages[%d]: with tools present the upstream demands reasoning_content on "+
				"every assistant turn, not only tool_calls turns; got %v", i, msg)
		}
		if rc != "" {
			t.Errorf("messages[%d] filled value = %q, want the empty presence marker", i, rc)
		}
	}
	for _, i := range []int{0, 1} {
		if _, has := msgs[i].(map[string]any)["reasoning_content"]; has {
			t.Errorf("messages[%d] is not an assistant turn and must not be filled", i)
		}
	}
	if len(got) != 1 || got[0] != "reasoning_content→filled_on_2_assistant_turns" {
		t.Errorf("rewrites = %v, want [reasoning_content→filled_on_2_assistant_turns]", got)
	}
}

// TestApplyThinkingRules_emptyToolsArrayIsNotToolsPresent — an empty `tools`
// array is the no-tools case (shape D), so it must not widen the fill.
func TestApplyThinkingRules_emptyToolsArrayIsNotToolsPresent(t *testing.T) {
	payload := map[string]any{
		"tools": []any{},
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "assistant", "content": "hello"},
		},
	}
	if got := applyThinkingRules(payload, "deepseek-v4-pro"); len(got) != 0 {
		t.Errorf("rewrites = %v, want none for an empty tools array", got)
	}
	if _, has := payload["messages"].([]any)[1].(map[string]any)["reasoning_content"]; has {
		t.Error("an empty tools array must not trigger the fill")
	}
}

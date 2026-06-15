package deepseek

import "testing"

// The thinking family is exactly the models observed to reject a forced
// tool_choice; everything else (deepseek-chat, v4-flash) keeps the caller's
// forcing untouched.
func TestIsThinkingModel(t *testing.T) {
	for _, m := range []string{"deepseek-reasoner", "deepseek-v4-pro", "deepseek-v4-pro-0610"} {
		if !IsThinkingModel(m) {
			t.Fatalf("%s must be a thinking model", m)
		}
	}
	for _, m := range []string{"deepseek-chat", "deepseek-v4-flash", "deepseek-coder", "deepseek-v3.1-thinking"} {
		if IsThinkingModel(m) {
			t.Fatalf("%s must NOT be a thinking model", m)
		}
	}
}

// TestApplyRewrites (live 400: "Thinking mode does not support this
// tool_choice"): a forced tool_choice — "required" or a named function — is
// stripped on thinking models (tools stay; default behavior still calls
// them), "auto"/"none" pass through, and non-thinking models are untouched.
func TestApplyRewrites(t *testing.T) {
	p := map[string]any{"tool_choice": "required", "tools": []any{"x"}}
	if got := ApplyRewrites(p, "deepseek-v4-pro"); len(got) != 1 || got[0] != "tool_choice→removed" {
		t.Fatalf("rewrites = %v", got)
	}
	if _, ok := p["tool_choice"]; ok {
		t.Fatal("forced tool_choice must be removed")
	}
	if _, ok := p["tools"]; !ok {
		t.Fatal("tools must survive — the model can still call them")
	}

	named := map[string]any{"tool_choice": map[string]any{"type": "function", "function": map[string]any{"name": "f"}}}
	if got := ApplyRewrites(named, "deepseek-reasoner"); len(got) != 1 {
		t.Fatalf("a named selection is forced; rewrites = %v", got)
	}

	auto := map[string]any{"tool_choice": "auto"}
	if got := ApplyRewrites(auto, "deepseek-v4-pro"); got != nil {
		t.Fatalf("auto must pass through, got %v", got)
	}
	if auto["tool_choice"] != "auto" {
		t.Fatal("auto must remain")
	}

	chat := map[string]any{"tool_choice": "required"}
	if got := ApplyRewrites(chat, "deepseek-chat"); got != nil {
		t.Fatalf("non-thinking model must keep the caller's forcing, got %v", got)
	}

	if got := ApplyRewrites(map[string]any{"messages": []any{}}, "deepseek-v4-pro"); got != nil {
		t.Fatalf("absent tool_choice needs no rewrite, got %v", got)
	}
}

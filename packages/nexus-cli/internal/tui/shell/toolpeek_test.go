package shell

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestToolActionLabel(t *testing.T) {
	cases := []struct{ name, input, want string }{
		{"use_skill", `{"name":"incident-triage"}`, "loading skill incident-triage"},
		{"analyze_cost", `{"window":"7d","groupBy":"provider"}`, "analyze_cost · 7d, by provider"},
		{"analyze_slo", `{"window":"24h"}`, "analyze_slo · 24h"},
		{"resource_search", `{"query":"hooks"}`, "resource_search · hooks"},
		{"resource_read", `{"kind":"virtual-keys","operationId":"listVirtualKeys"}`, "resource_read · virtual-keys / listVirtualKeys"},
		{"mitigate_routing_rule_enabled", `{"rule":"smart-auto-routing","enabled":false}`, "routing rule smart-auto-routing → off"},
		{"mitigate_kill_switch", `{"engage":true}`, "kill switch → on"},
		{"mitigate_provider_enabled", `{"provider":"moonshot","enabled":false}`, "provider moonshot → off"},
		{"mitigate_vk_revoke", `{"vk":"eng"}`, "revoke virtual key eng"},
		{"mitigate_passthrough_global", `{"enabled":true}`, "global passthrough → on"},
		{"navigate", `{"view":"radar"}`, "navigate · radar"},
		{"show_event", `{"id":"ev1"}`, "show_event · ev1"},
		{"route_explain", `{"model":"claude"}`, "route_explain · claude"},
		{"simulate_request", `{"model":"gpt-4o"}`, "simulate_request · gpt-4o"},
		{"resource_describe", `{"kind":"hooks"}`, "resource_describe · hooks"},
		{"observe_health", `{}`, "observe_health"},
		{"observe_alerts", `{}`, "observe_alerts"}, // no args → bare name
	}
	for _, c := range cases {
		if got := toolActionLabel(c.name, []byte(c.input)); got != c.want {
			t.Errorf("toolActionLabel(%s, %s) = %q, want %q", c.name, c.input, got, c.want)
		}
	}
	// generic fallback leads with the name + compacted input.
	if got := toolActionLabel("unknown_tool", []byte(`{"x":  1}`)); !strings.HasPrefix(got, "unknown_tool · ") {
		t.Errorf("generic fallback should lead with the name: %q", got)
	}
}

func TestToolResultPeek(t *testing.T) {
	if got := toolResultPeek("resource_read", []byte(`{"data":[{"id":"a"},{"id":"b"},{"id":"c"}]}`), false); got != "3 rows" {
		t.Errorf("collection peek = %q, want '3 rows'", got)
	}
	if got := toolResultPeek("use_skill", []byte("Skill loaded. Follow…"), false); got != "playbook loaded" {
		t.Errorf("use_skill peek = %q", got)
	}
	if got := toolResultPeek("mitigate_x", []byte("boom: provider not found"), true); !strings.HasPrefix(got, "⚠ ") {
		t.Errorf("error peek should be flagged: %q", got)
	}
	if got := toolResultPeek("x", []byte(""), false); got != "(no output)" {
		t.Errorf("empty peek = %q", got)
	}
}

func TestRenderToolBlock(t *testing.T) {
	// running (no result yet): only the action line.
	running := convLine{tag: "tool", toolName: "analyze_cost", toolInput: []byte(`{"window":"7d"}`)}
	if out := renderToolBlock(running, false, 80); !strings.Contains(out, "analyze_cost · 7d") || strings.Contains(out, "└") {
		t.Errorf("running block should show the action, no peek: %q", out)
	}
	// done, collapsed: action + dim peek.
	done := convLine{tag: "tool", toolName: "resource_read", toolInput: []byte(`{"kind":"virtual-keys"}`),
		toolOutput: []byte(`{"data":[{"id":"a"}]}`), toolDone: true}
	collapsed := renderToolBlock(done, false, 80)
	if !strings.Contains(collapsed, "virtual-keys") || !strings.Contains(collapsed, "1 rows") {
		t.Errorf("collapsed block should show action + peek: %q", collapsed)
	}
	// verbose: shows the in/out block instead of the peek.
	verbose := renderToolBlock(done, true, 80)
	if !strings.Contains(verbose, "in ") || !strings.Contains(verbose, "out ") {
		t.Errorf("verbose block should show in/out: %q", verbose)
	}
}

func TestConversation_ToolBlockResultAndVerboseToggle(t *testing.T) {
	c := newConversation(testSessionLocal(), nil)
	c.bridge = newBridge(nil) // the msg handlers return c.bridge.drain()

	// Start → records an open tool block; result → fills it (FIFO match).
	c.Update(agentToolMsg{name: "analyze_cost", input: []byte(`{"window":"7d"}`)})
	c.Update(agentToolResultMsg{name: "analyze_cost", output: []byte(`{"data":[{"x":1}]}`), isError: false})
	if len(c.lines) != 1 || c.lines[0].tag != "tool" || !c.lines[0].toolDone {
		t.Fatalf("the tool block should be recorded and marked done: %+v", c.lines)
	}
	if out := c.View(100, 24); !strings.Contains(out, "analyze_cost · 7d") {
		t.Fatalf("collapsed transcript must show the input-forward action: %s", out)
	}

	// ctrl+t enables verbose (full I/O); /verbose toggles it back.
	c.handleKey(tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl})
	if !c.toolVerbose {
		t.Fatal("ctrl+t should enable verbose tool I/O")
	}
	if out := c.View(100, 24); !strings.Contains(out, "out ") {
		t.Fatalf("verbose transcript must show the output block: %s", out)
	}
	c.agentCommand("/verbose")
	if c.toolVerbose {
		t.Fatal("/verbose should toggle verbose back off")
	}
}

func TestToolResultPeek_MoreBranches(t *testing.T) {
	// bare JSON array → "N items"
	if got := toolResultPeek("x", []byte(`[1,2]`), false); got != "2 items" {
		t.Errorf("array peek = %q, want '2 items'", got)
	}
	// non-JSON, multi-line → first line, truncated
	if got := toolResultPeek("x", []byte("first line of output\nsecond"), false); got != "first line of output" {
		t.Errorf("first-line peek = %q", got)
	}
	// pretty-printed JSON object → one-line preview, NEVER a lone "{"
	pretty := "{\n  \"id\": \"867046cd\",\n  \"model\": \"gpt-4o\"\n}"
	got := toolResultPeek("observe_traffic_event", []byte(pretty), false)
	if got == "{" || strings.TrimSpace(got) == "{" {
		t.Errorf("object peek must not be a bare brace, got %q", got)
	}
	if !strings.Contains(got, "867046cd") || !strings.Contains(got, "model") {
		t.Errorf("object peek should preview real fields, got %q", got)
	}
	// empty object → (no output), not "{}"
	if got := toolResultPeek("x", []byte("{}"), false); got != "(no output)" {
		t.Errorf("empty-object peek = %q, want '(no output)'", got)
	}
}

func TestRenderToolBlock_ErrorAndCap(t *testing.T) {
	// error result renders the flagged peek.
	errLn := convLine{tag: "tool", toolName: "mitigate_x", toolInput: []byte(`{}`),
		toolOutput: []byte("provider not found"), toolErr: true, toolDone: true}
	if out := renderToolBlock(errLn, false, 80); !strings.Contains(out, "⚠") {
		t.Errorf("error block should flag the failure: %q", out)
	}
	// verbose output longer than the cap is truncated (… marker) so it can't flood.
	big := strings.Repeat(`{"provider":"anthropic","cost":12.34},`, 40)
	bigLn := convLine{tag: "tool", toolName: "resource_read", toolInput: []byte(`{"kind":"x"}`),
		toolOutput: []byte(big), toolDone: true}
	if out := renderToolBlock(bigLn, true, 200); !strings.Contains(out, "…") {
		t.Errorf("verbose over-cap output should be truncated with …: %q", out)
	}
}

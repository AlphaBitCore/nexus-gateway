package shell

import (
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/agent"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/styles"
)

func TestContextBar(t *testing.T) {
	// No usage, known window → the pre-first-turn placeholder.
	if got := contextBar(agent.ContextStats{}, 200000); !strings.Contains(got, "–/200k") {
		t.Fatalf("pre-first-turn bar: %q", got)
	}
	// No usage, no window → empty.
	if got := contextBar(agent.ContextStats{}, 0); got != "" {
		t.Fatalf("empty bar expected, got %q", got)
	}
	// Used + window → used/window + percent + a fill bar.
	got := contextBar(agent.ContextStats{Used: 48000, Cached: 42000}, 200000)
	for _, want := range []string{"48k/200k", "24%", "▕", "▏", "cache 87%"} {
		if !strings.Contains(got, want) {
			t.Fatalf("bar missing %q:\n%q", want, got)
		}
	}
	// Near the window → the ⚠ near-full marker.
	if got := contextBar(agent.ContextStats{Used: 190000}, 200000); !strings.Contains(got, "⚠") {
		t.Fatalf("a near-full bar should warn: %q", got)
	}
	// Window unknown but usage present → absolute used, no bar bracket.
	if got := contextBar(agent.ContextStats{Used: 12000}, 0); !strings.Contains(got, "12k") || strings.Contains(got, "▕") {
		t.Fatalf("window-unknown bar: %q", got)
	}
}

// TestContextBarSeededBeforeChat guards that the gauge shows the window from
// launch (seeded via Session.ContextWindow) before any turn reports usage.
func TestContextBarSeededBeforeChat(t *testing.T) {
	c := newConversation(Session{EnvName: "local", ContextWindow: 200000}, nil)
	if c.ctxWindow != 200000 {
		t.Fatalf("conversation should seed the window from the session, got %d", c.ctxWindow)
	}
	if got := c.contextBar(); !strings.Contains(got, "–/200k") {
		t.Fatalf("the gauge should show the window before any chat: %q", got)
	}
}

func TestCtxColorThresholds(t *testing.T) {
	if ctxColor(0.50) != styles.Green {
		t.Fatal("under amber → green")
	}
	if ctxColor(0.75) != styles.Amber {
		t.Fatal("amber band")
	}
	if ctxColor(0.95) != styles.Red {
		t.Fatal("red band")
	}
}

func TestCachePctClampsAtHundred(t *testing.T) {
	// Cached prompt tokens are a subset of Used; the percentage must never read >100%.
	if got := cachePct(agent.ContextStats{Used: 2000, Cached: 5000}); got != 100 {
		t.Fatalf("over-cache cachePct = %d, want clamped 100", got)
	}
	if got := cachePct(agent.ContextStats{Used: 2000, Cached: 1480}); got != 74 {
		t.Fatalf("cachePct = %d, want 74", got)
	}
	if got := cachePct(agent.ContextStats{Used: 0, Cached: 100}); got != 0 {
		t.Fatalf("cachePct with no prompt = %d, want 0", got)
	}
	if got := cachePct(agent.ContextStats{Used: 2000, Cached: 0}); got != 0 {
		t.Fatalf("cachePct with no cache = %d, want 0", got)
	}
}

// TestContextBarPercentConsistency guards that the used/window display and the percent
// are derived from the SAME values: a small prompt in a large window reads ~1%, never a
// mismatched figure — so "2k/256k 9%" is structurally impossible from this renderer.
func TestContextBarPercentConsistency(t *testing.T) {
	got := contextBar(agent.ContextStats{Used: 2000}, 256000)
	if !strings.Contains(got, "2k/256k 1%") {
		t.Fatalf("small prompt in a big window must read ~1%%: %q", got)
	}
	// An over-cache still clamps the suffix at 100%.
	if got := contextBar(agent.ContextStats{Used: 2000, Cached: 9000}, 256000); !strings.Contains(got, "cache 100%") {
		t.Fatalf("over-cache must clamp to 100%%: %q", got)
	}
}

func TestCtxFillAndMiniBar(t *testing.T) {
	// A full bar clamps to all-filled; over-fraction does not overflow.
	if got := ctxFillBar(2.0, 4); strings.Count(got, "█") != 4 || strings.Contains(got, "░") {
		t.Fatalf("over-full bar should clamp: %q", got)
	}
	if got := ctxFillBar(0, 4); strings.Count(got, "░") != 4 {
		t.Fatalf("empty bar: %q", got)
	}
	// Mini bar: proportion of total; zero total → empty string.
	if ctxMiniBar(5, 0, 10) != "" {
		t.Fatal("zero total → empty mini bar")
	}
	if got := ctxMiniBar(5, 10, 10); strings.Count(got, "█") != 5 {
		t.Fatalf("half mini bar: %q", got)
	}
}

func TestContextPanel(t *testing.T) {
	// No usage yet.
	if got := contextPanel(agent.ContextStats{}, 200000, "claude-sonnet-4-6"); !strings.Contains(got, "no usage yet") || !strings.Contains(got, "claude-sonnet-4-6") {
		t.Fatalf("empty panel: %q", got)
	}
	// Full breakdown.
	cs := agent.ContextStats{Used: 48000, Cached: 42000, System: 6000, Tools: 9000, History: 30000, Bundle: 3000, Messages: 14, CompactBudget: 140000}
	got := contextPanel(cs, 200000, "m")
	for _, want := range []string{"used", "48k / 200k", "cache hit 87%", "~system", "~history", "/clear", "auto-trim when the model view exceeds", "140k", "estimate"} {
		if !strings.Contains(got, want) {
			t.Fatalf("panel missing %q:\n%s", want, got)
		}
	}
	// Past the trim budget → "auto-trim active".
	cs.History = 150000
	if got := contextPanel(cs, 200000, "m"); !strings.Contains(got, "auto-trim active") {
		t.Fatalf("past-budget panel: %s", got)
	}
}

func TestConversationContextStatsAndPanel(t *testing.T) {
	c := newConversation(testSessionLocal(), nil)
	// A stats message updates the indicator state (nil bridge is tolerated).
	c.Update(contextStatsMsg{stats: agent.ContextStats{Used: 48000}, window: 200000})
	if c.ctxStats.Used != 48000 || c.ctxWindow != 200000 {
		t.Fatalf("contextStatsMsg should update state: %+v win=%d", c.ctxStats, c.ctxWindow)
	}
	if !strings.Contains(c.contextBar(), "48k/200k") {
		t.Fatalf("contextBar should reflect the stats: %q", c.contextBar())
	}
	// /context prints the breakdown into the transcript (like /help), so the chat
	// stays usable — it is not a modal that traps the view.
	c.agentCommand("context")
	c.flushReveal() // /context types out; snap it for the assertion
	out := c.View(80, 30)
	for _, want := range []string{"used", "~system", "~history"} {
		if !strings.Contains(out, want) {
			t.Fatalf("/context should print the breakdown into the transcript (missing %q):\n%s", want, out)
		}
	}
	// The prompt is still present — the conversation is not trapped in a panel.
	if !strings.Contains(out, convPromptPrefix) {
		t.Fatal("the chat prompt must remain after /context")
	}
}

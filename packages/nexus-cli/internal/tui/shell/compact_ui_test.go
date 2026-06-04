package shell

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/agent"
)

// noTurn is a convScript that ends a turn immediately with no output.
func noTurn(_, _ func(string), _ func(string, []byte), _ agent.ConfirmFunc) (string, error) {
	return "", nil
}

func TestConversationClearResetsContextBar(t *testing.T) {
	c, _ := newTestConv(testSessionLocal(), noTurn)
	// A prior turn populated the gauge.
	c.Update(contextStatsMsg{stats: agent.ContextStats{Used: 120000, History: 90000, Messages: 30}, window: 200000})
	if !strings.Contains(c.contextBar(), "120k/200k") {
		t.Fatalf("precondition: bar should show the prior usage, got %q", c.contextBar())
	}
	// /clear must reset the indicator, not strand the old number.
	c.input.SetValue("/clear")
	c.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	if c.ctxStats.Used != 0 {
		t.Fatalf("/clear must zero the context stats, got Used=%d", c.ctxStats.Used)
	}
	if !strings.Contains(c.contextBar(), "–/200k") {
		t.Fatalf("after /clear the bar must fall back to the empty gauge, got %q", c.contextBar())
	}
}

func TestConversationCompactSurfacesNoticeAndRefreshesBar(t *testing.T) {
	c, rr := newTestConv(testSessionLocal(), noTurn)
	// A prior turn left the gauge high.
	c.Update(contextStatsMsg{stats: agent.ContextStats{Used: 180000, History: 165000, Messages: 40}, window: 200000})
	// The runner fires OnCompact (as the real agent does inside Compact) then reports it acted.
	rr.compactFn = func() (agent.CompactStat, bool, error) {
		stat := agent.CompactStat{MessagesBefore: 40, MessagesAfter: 9, TokensBefore: 165000, TokensAfter: 22000}
		rr.onCompact(stat)
		return stat, true, nil
	}
	c.input.SetValue("/compact")
	pumpConv(c, c.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter}))

	if c.running {
		t.Fatal("compaction must finish (running cleared)")
	}
	// Footer dropped to the post-compaction estimate immediately.
	if c.ctxStats.Used != 22000 || c.ctxStats.Messages != 9 {
		t.Fatalf("compaction must refresh the gauge to the post-compaction estimate, got %+v", c.ctxStats)
	}
	if c.ctxStats.Cached != 0 {
		t.Fatalf("the rewritten prefix has no cache hit, got Cached=%d", c.ctxStats.Cached)
	}
	// A visible notice (like a tool call) lands in the transcript.
	out := c.View(80, 40)
	if !strings.Contains(out, "context compacted") || !strings.Contains(out, "40→9") {
		t.Fatalf("transcript must show the compaction notice, got:\n%s", out)
	}
}

func TestConversationCompactNothingToCompact(t *testing.T) {
	c, _ := newTestConv(testSessionLocal(), noTurn)
	c.Update(agentCompactMsg{acted: false})
	if !strings.Contains(c.View(80, 20), "nothing to compact") {
		t.Fatalf("a no-op compaction must say so, got:\n%s", c.View(80, 20))
	}
}

func TestConversationCompactRejectedWhileRunning(t *testing.T) {
	c, _ := newTestConv(testSessionLocal(), noTurn)
	c.running = true // a turn is in flight
	if cmd := c.startCompact(); cmd != nil {
		t.Fatal("/compact must be rejected while a turn runs (it rewrites the same session)")
	}
	if !strings.Contains(c.notice, "busy") {
		t.Fatalf("rejection must explain why, got notice %q", c.notice)
	}
}

func TestBridgeStartCompactEmitsDone(t *testing.T) {
	b := newBridge(&fakeRunner{compact: func(context.Context) (agent.CompactStat, bool, error) {
		return agent.CompactStat{MessagesAfter: 3}, true, nil
	}})
	msg := b.startCompact()() // drain the first event
	done, ok := msg.(agentDoneMsg)
	if !ok || done.err != nil {
		t.Fatalf("a successful compaction emits a clean done, got %#v", msg)
	}
	b.done()
}

func TestBridgeStartCompactReportsNothingToCompact(t *testing.T) {
	b := newBridge(&fakeRunner{compact: func(context.Context) (agent.CompactStat, bool, error) {
		return agent.CompactStat{}, false, nil // acted=false
	}})
	msg := b.startCompact()()
	cm, ok := msg.(agentCompactMsg)
	if !ok || cm.acted {
		t.Fatalf("a no-op compaction must emit agentCompactMsg{acted:false}, got %#v", msg)
	}
}

func TestModelRoutesCompactEventsSoCompactCompletes(t *testing.T) {
	// Regression: the root Model must route agentCompactMsg to the conversation. If it
	// drops it, the bridge drain chain breaks, the buffered agentDoneMsg is never read,
	// and /compact hangs forever (running stuck) — even on a tiny context. This drives
	// /compact THROUGH the root Update (not the bare conversation) to catch that.
	m, rr := newTestModel(testSessionLocal(), noTurn)
	rr.compactFn = func() (agent.CompactStat, bool, error) {
		rr.onCompact(agent.CompactStat{Kind: "summary", MessagesBefore: 10, MessagesAfter: 3, TokensBefore: 5000, TokensAfter: 1000})
		return agent.CompactStat{Kind: "summary", MessagesBefore: 10, MessagesAfter: 3, TokensBefore: 5000, TokensAfter: 1000}, true, nil
	}
	cmd := m.conv.startCompact() // returns the bridge drain cmd
	m = pumpModel(m, cmd)        // pump through the ROOT Update (exercises root routing)

	if m.conv.running {
		t.Fatal("/compact hung — root must route agentCompactMsg so the drain reaches agentDoneMsg")
	}
	if out := m.conv.View(80, 40); !strings.Contains(out, "context compacted") {
		t.Fatalf("the /compact notice must reach the transcript via root routing:\n%s", out)
	}
}

func TestModelRoutesNoOpCompactSoItCompletes(t *testing.T) {
	// The acted=false (nothing to compact) event must also route — else /compact on a
	// tiny context hangs (the exact prod symptom: "compacting…" then stuck).
	m, rr := newTestModel(testSessionLocal(), noTurn)
	rr.compactFn = func() (agent.CompactStat, bool, error) { return agent.CompactStat{}, false, nil }
	m = pumpModel(m, m.conv.startCompact())
	if m.conv.running {
		t.Fatal("a no-op /compact must complete, not hang")
	}
	if out := m.conv.View(80, 40); !strings.Contains(out, "nothing to compact") {
		t.Fatalf("a no-op /compact must say so:\n%s", out)
	}
}

func TestBridgeStartCompactRejectsDoubleStart(t *testing.T) {
	b := newBridge(&fakeRunner{})
	b.running = true
	if b.startCompact() != nil {
		t.Fatal("startCompact must reject while already running")
	}
}

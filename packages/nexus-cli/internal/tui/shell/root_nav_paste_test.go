package shell

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/kit"
)

// TestModel_NavPasteFooterCoverage exercises the nav/teardown + paste + footer paths
// the shell routes: jumpTop/drillTo/popNav (including leaveActive firing Leave on a
// leaver view), the three handlePaste branches, and the footer/badge/status render.
func TestModel_NavPasteFooterCoverage(t *testing.T) {
	m := NewModel(sampleGateway(), testSession())
	m.width, m.height = 120, 30

	// jump to Chat (a leaver), then away → leaveActive must call Leave on the leaver.
	mm, _ := m.jumpTop(m.indexOf("Chat"))
	m = mm.(Model)
	mm, _ = m.jumpTop(m.indexOf("Radar"))
	m = mm.(Model)

	// drill into Event (drillTo + leaveActive), then pop back up.
	em, _ := m.drillEvent(kit.OpenEventMsg{ID: "ev1"})
	m = em.(Model)
	if m.entries[m.active].name != "Event" {
		t.Fatalf("drillEvent should open Event, got %s", m.entries[m.active].name)
	}
	pm, _ := m.popNav()
	m = pm.(Model)

	// handlePaste: chat-focused → routed to the conversation.
	m.focus = focusChat
	m, _ = updateModel(m, tea.PasteMsg{Content: "pasted"})
	// slash open → paste dropped.
	m.slashOpen = true
	m, _ = updateModel(m, tea.PasteMsg{Content: "x"})
	m.slashOpen = false
	// canvas-focused, non-capturing view → paste dropped.
	m.focus = focusCanvas
	cm, _ := m.jumpTop(m.indexOf("Cost"))
	m = cm.(Model)
	m, _ = updateModel(m, tea.PasteMsg{Content: "y"})
	// canvas-focused, capturing view (Chat captures keystrokes) → routed to the view.
	chm, _ := m.jumpTop(m.indexOf("Chat"))
	m = chm.(Model)
	m, _ = updateModel(m, tea.PasteMsg{Content: "z"})

	// footer / badge / status render (non-prod).
	if m.footerBar(120) == "" {
		t.Fatal("footer should render")
	}
	_ = m.statusBar(120)
	_ = m.modelBadge()

	// prod model with a named VK: the status bar renders the full production banner.
	mp := NewModel(sampleGateway(), kit.Session{EnvName: "prod", IsProd: true, Model: "m", VKName: "engineering", VKSecret: "s"})
	mp.width, mp.height = 120, 30
	if !strings.Contains(mp.statusBar(120), "engineering") {
		t.Fatal("prod status bar should show the VK name")
	}
	_ = mp.View()
}

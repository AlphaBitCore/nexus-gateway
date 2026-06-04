package views

import (
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/kit"
)

// TestChat_SetSessionFollowsModelSwitch: a session-bearing view follows the shell's
// runtime model switch (the broadcast target of setChatModel's sessionSetter loop).
func TestChat_SetSessionFollowsModelSwitch(t *testing.T) {
	c := newChat(sampleGateway(), testSession())
	c.SetSession(kit.Session{Model: "brand-new-model", VKSecret: "x"})
	if c.session.Model != "brand-new-model" {
		t.Fatalf("chat must follow SetSession, got %q", c.session.Model)
	}
}

// TestAlerts_BackClosesDetail: the view's Back() consumes the first esc to close its
// own detail drawer (the shell's backHandler fall-through).
func TestAlerts_BackClosesDetail(t *testing.T) {
	g := sampleGateway()
	g.alerts = &core.AlertsResult{Alerts: []core.Alert{{TargetLabel: "ai-gateway", State: "firing", Message: "spike"}}}
	a := newAlerts(g)
	a.Update(a.Init()())
	a.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if !a.detail {
		t.Fatal("enter should open the alerts detail")
	}
	if !a.Back() {
		t.Fatal("Back should handle and close the open detail")
	}
	if a.detail {
		t.Fatal("Back should close the detail drawer")
	}
}

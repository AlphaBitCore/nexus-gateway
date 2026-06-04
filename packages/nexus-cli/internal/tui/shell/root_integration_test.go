package shell

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/kit"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/styles"
)

func TestModel_EventIndexFallback(t *testing.T) {
	// A model whose entries lack "Event" falls back to index 0 (defensive branch).
	m := Model{entries: []viewEntry{{name: "A"}, {name: "B"}}}
	if m.indexOf("Event") != 0 {
		t.Fatal("indexOf should fall back to 0 when Event is absent")
	}
}

// view tab indices (must track NewModel's registry order).
const (
	chatTabIndex = 5
	labTabIndex  = 6
	killTabIndex = 7
)

// TestModel_LocationIndicator verifies the bottom-right profile + address badge:
// it shows the env name and scheme-stripped address, reddens prod, hides when no
// env, and yields to the keybar in a too-narrow footer.
func TestModel_LocationIndicator(t *testing.T) {
	m := NewModel(sampleGateway(), testSession())
	m.width, m.height = 120, 30
	out := m.View().Content
	if !strings.Contains(out, "local") || !strings.Contains(out, "localhost:3001") {
		t.Fatalf("footer should show profile + scheme-stripped address:\n%s", out)
	}
	// scheme is stripped.
	if strings.Contains(m.locationIndicator(), "http://") {
		t.Fatalf("address should be scheme-stripped: %q", m.locationIndicator())
	}
	// the selected model leads the footer cluster (its own badge, not the location).
	if !strings.Contains(m.modelBadge(), "gpt-4o-mini") {
		t.Fatalf("model badge should show the model: %q", m.modelBadge())
	}
	if strings.Contains(m.locationIndicator(), "gpt-4o-mini") {
		t.Fatalf("the model belongs to its own badge, not the location indicator: %q", m.locationIndicator())
	}
	// the footer shows model, gauge, and location together, with separators.
	if !strings.Contains(out, "gpt-4o-mini") {
		t.Fatalf("footer should show the model:\n%s", out)
	}
	// no env → no indicator.
	empty := NewModel(sampleGateway(), kit.Session{})
	if empty.locationIndicator() != "" {
		t.Fatal("no env should yield no location indicator")
	}
	if empty.footerBar(120) != styles.HelpBar.Render(empty.helpText()) {
		t.Fatal("footer with no env should be the keybar alone")
	}
	// too-narrow footer keeps the keybar, drops the indicator.
	if got := m.footerBar(5); strings.Contains(got, "localhost") {
		t.Fatalf("a narrow footer should drop the indicator: %q", got)
	}
	// an open slash palette owns the whole footer line.
	m.slashOpen = true
	m.slash = newSlashPalette(defaultSlashCommands())
	if !strings.Contains(m.footerBar(120), m.slash.View()) {
		t.Fatal("open slash palette should own the footer")
	}
}

// TestModel_EscClosesViewDetailBeforePoppingNav verifies the root's esc handling
// routes through the active view's backHandler: esc first closes the view's own
// detail drawer (the view consumes it, root stays put), and only then pops the nav
// stack to the cockpit. Asserted observably via m.active; the view-side Back()
// behavior is covered by views.TestAlerts_BackClosesDetail.
func TestModel_EscClosesViewDetailBeforePoppingNav(t *testing.T) {
	g := sampleGateway()
	g.alerts = &core.AlertsResult{Alerts: []core.Alert{{TargetLabel: "ai-gateway", State: "firing", Message: "spike"}}}
	m := NewModel(g, testSession())
	m.focus = focusCanvas // row-drill is a canvas-focus interaction
	am, cmd := m.jumpTop(m.indexOf("Alerts"))
	m = am.(Model)
	if cmd != nil {
		m, _ = updateModel(m, cmd()) // populate the alerts list so a row is selectable
	}
	before := m.active

	// enter opens the in-view detail drawer (the view stays active — no navigation).
	m, _ = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.active != before {
		t.Fatalf("enter should open the in-view detail, not navigate (active=%d)", m.active)
	}
	// first esc: the view's Back() consumes it (closes the detail), so the root stays.
	m, _ = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEsc})
	if m.active != before {
		t.Fatalf("esc in a detail drawer should close it and stay in the view, active=%d", m.active)
	}
	// second esc: nothing left to back out of → the root pops to the cockpit.
	m, _ = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEsc})
	if m.active != 0 {
		t.Fatalf("esc at the list level should pop to the cockpit, active=%d", m.active)
	}
}

func TestRun_WiresShellWithAltScreen(t *testing.T) {
	orig := run
	defer func() { run = orig }()
	var sawShell bool
	run = func(m tea.Model, opts ...tea.ProgramOption) error {
		_, sawShell = m.(*Shell)
		return nil
	}
	deps := Deps{Gateway: sampleGateway(), HasSession: func() bool { return true },
		Session: kit.Session{EnvName: "local", Model: "m", VKSecret: "s"}}
	if err := Run(deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !sawShell {
		t.Fatal("Run should launch a Shell")
	}
	// In v2 the alt-screen is requested via the view (View.AltScreen), not a program
	// option — assert the shell's view sets it.
	if !NewShell(deps).View().AltScreen {
		t.Fatal("the shell view must request the alt-screen")
	}
}

func TestWizard_VKNavAndSecretTyping(t *testing.T) {
	r := newRecordingDeps(sampleGateway())
	r.hasSession = true
	w := newWizard(r.deps)
	w, _ = runStep(t, w, w.Init()) // → stageModel with one model
	// pick the model, load VKs
	w, cmd := w.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	w, _ = runStep(t, w, cmd)
	// give it two VKs to exercise nav clamping both ways
	w.vks = append(w.vks, w.vks[0])
	w.vks[1].Name = "second"
	w, _ = w.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	if w.vkCursor != 1 {
		t.Fatalf("down should move VK cursor to 1, got %d", w.vkCursor)
	}
	w, _ = w.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	if w.vkCursor != 0 {
		t.Fatalf("up should move VK cursor to 0, got %d", w.vkCursor)
	}
	// select VK → secret stage, then type into the (masked) secret field
	w, _ = w.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if w.stage != stageSecret {
		t.Fatalf("should be at secret stage, got %d", w.stage)
	}
	w, _ = w.Update(keyRunes("nvk_typed"))
	if w.secret.Value() != "nvk_typed" {
		t.Fatalf("typed secret not captured: %q", w.secret.Value())
	}
}

// TestModel_CaptureRoutesText verifies that while a canvas view captures text, the
// root suspends single-letter shortcuts (the key is typed), yet tab still toggles
// pane focus (never trapping the operator in a capturing view).
func TestModel_CaptureRoutesText(t *testing.T) {
	m := NewModel(sampleGateway(), testSession())
	m.focus = focusCanvas // interacting with a canvas view
	mm, _ := m.jumpTop(chatTabIndex)
	m = mm.(Model)
	if _, ok := m.views[m.active].(textCapturer); !ok || !m.views[m.active].(textCapturer).Capturing() {
		t.Fatal("chat view should be capturing text")
	}
	// a number key must NOT navigate while capturing (it's typed into the prompt)
	mm, _ = m.Update(keyRunes("2"))
	m = mm.(Model)
	if m.active != chatTabIndex {
		t.Fatalf("number key should be captured, active=%d", m.active)
	}
	// tab still toggles focus to the resident chat (does not trap the capture)
	mm, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	m = mm.(Model)
	if m.focus != focusChat {
		t.Fatalf("tab should toggle focus to the chat even while a view captures, got %v", m.focus)
	}
	// ctrl+c quits even while capturing
	mm, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if !mm.(Model).quitting || cmd == nil {
		t.Fatal("ctrl+c should quit while capturing")
	}
}

// TestModel_HelpTextPerView checks the bottom keybar defers to the active canvas
// view when the canvas is focused.
func TestModel_HelpTextPerView(t *testing.T) {
	const sloTabIndex = 3 // SLO supplies its own keybar (contains "1-9 jump")
	m := NewModel(sampleGateway(), testSession())
	m.focus = focusCanvas // view keybars show only when the canvas is focused
	mm0, _ := m.jumpTop(sloTabIndex)
	m = mm0.(Model)
	m2, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = m2.(Model)
	if !strings.Contains(m.View().Content, "1-9 jump") {
		t.Fatalf("SLO should show its own keybar with the jump hint:\n%s", m.View().Content)
	}
	mm, _ := m.jumpTop(killTabIndex)
	m = mm.(Model)
	m2, _ = m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	if !strings.Contains(m2.(Model).View().Content, "kill-switch on/off") {
		t.Fatalf("kill tab should show its own keybar:\n%s", m2.(Model).View().Content)
	}
}

func TestWizard_ViewAllStages(t *testing.T) {
	r := newRecordingDeps(sampleGateway())
	w := newWizard(r.deps)
	w.stage = stageLogin
	w.loggingIn = true
	if !strings.Contains(w.View(100, 24), "opening browser") {
		t.Fatal("login stage shows browser progress when logging in")
	}
	w.stage = stageModel
	w.models = []modelChoice{{code: "gpt-4o-mini", name: "GPT", provider: "OpenAI"}}
	if !strings.Contains(w.View(100, 24), "Step 2") {
		t.Fatal("model stage")
	}
	w.stage = stageVK
	w.vks = sampleGateway().vks
	out := w.View(100, 24)
	if !strings.Contains(out, "Step 3") || !strings.Contains(out, "engineering") {
		t.Fatalf("VK stage should list keys:\n%s", out)
	}
	w.stage = stageSecret
	w.session.VKName = "engineering"
	if !strings.Contains(w.View(100, 24), "Step 4") {
		t.Fatal("secret stage")
	}
	// empty-list placeholders
	w2 := newWizard(r.deps)
	w2.stage = stageModel
	if !strings.Contains(w2.View(100, 24), "no chat models") {
		t.Fatal("empty model list placeholder")
	}
	w2.stage = stageVK
	if !strings.Contains(w2.View(100, 24), "no enabled virtual keys") {
		t.Fatal("empty VK list placeholder")
	}
}

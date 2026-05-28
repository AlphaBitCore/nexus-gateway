package tui

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// chatTabIndex / labTabIndex / killTabIndex must track NewModel's tab order.
const (
	chatTabIndex = 5
	labTabIndex  = 6
	killTabIndex = 7
)

// TestModel_CaptureRoutesTextButTabNavigates verifies the root model suspends
// single-letter shortcuts while a view captures text, yet tab still navigates.
func TestModel_CaptureRoutesText(t *testing.T) {
	m := NewModel(sampleGateway(), testSession())
	mm, _ := m.switchTo(chatTabIndex)
	m = mm.(Model)
	if _, ok := m.views[m.active].(textCapturer); !ok || !m.views[m.active].(textCapturer).capturing() {
		t.Fatal("chat tab should be capturing text")
	}
	// a number key must NOT navigate while capturing (it's typed into the prompt)
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("2")})
	m = mm.(Model)
	if m.active != chatTabIndex {
		t.Fatalf("number key should be captured, active=%d", m.active)
	}
	// tab still navigates away
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = mm.(Model)
	if m.active != labTabIndex {
		t.Fatalf("tab should navigate while capturing, active=%d", m.active)
	}
	// shift+tab navigates back
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
	m = mm.(Model)
	if m.active != chatTabIndex {
		t.Fatalf("shift+tab should navigate back, active=%d", m.active)
	}
	// ctrl+c quits even while capturing
	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if !mm.(Model).quitting || cmd == nil {
		t.Fatal("ctrl+c should quit while capturing")
	}
}

// TestModel_HelpTextPerView checks the bottom keybar defers to the active view.
func TestModel_HelpTextPerView(t *testing.T) {
	const sloTabIndex = 3 // SLO has no help() → falls back to the default keybar
	m := NewModel(sampleGateway(), testSession())
	mm0, _ := m.switchTo(sloTabIndex)
	m = mm0.(Model)
	m2, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = m2.(Model)
	if !strings.Contains(m.View(), "tab/1-9") {
		t.Fatalf("SLO (no helpProvider) should show the default keybar:\n%s", m.View())
	}
	mm, _ := m.switchTo(killTabIndex)
	m = mm.(Model)
	m2, _ = m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	if !strings.Contains(m2.(Model).View(), "kill-switch on/off") {
		t.Fatalf("kill tab should show its own keybar:\n%s", m2.(Model).View())
	}
}

func TestViews_InitAndHelp(t *testing.T) {
	gw, s := sampleGateway(), testSession()
	if newChat(gw, s).Init() == nil {
		t.Fatal("chat Init returns the blink cmd")
	}
	if newLab(gw, s).Init() != nil {
		t.Fatal("lab Init has no startup command")
	}
	if newKill(gw, s).Init() == nil {
		t.Fatal("kill Init should fetch the current kill-switch + passthrough state")
	}
	if !strings.Contains(newChat(gw, s).help(), "enter send") {
		t.Fatal("ready chat help")
	}
	if !strings.Contains(newChat(gw, Session{}).help(), "no model/VK") {
		t.Fatal("not-ready chat help")
	}
	if !strings.Contains(newLab(gw, s).help(), "generator burst") {
		t.Fatal("lab idle help")
	}
	le := newLab(gw, s)
	le.editing = true
	if !strings.Contains(le.help(), "send lab request") {
		t.Fatal("lab edit help")
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

func TestSLOCost_LastGoodOnError(t *testing.T) {
	// SLO: a later error keeps the prior data with a stale note.
	s := newSLO(sampleGateway())
	v, _ := s.Update(s.Init()())
	sv := v.(*slo)
	sv.err = errors.New("blip")
	if !strings.Contains(sv.View(120, 30), "last-good") {
		t.Fatal("SLO should show last-good banner on transient error")
	}
	// SLO availability with no metrics.
	empty := newSLO(&fakeGateway{phases: sampleGateway().phases})
	v, _ = empty.Update(empty.Init()())
	if !strings.Contains(v.View(120, 30), "no metrics") {
		t.Fatal("SLO availability with no sparkline → 'no metrics'")
	}
	// Cost: last-good on error.
	c := newCost(sampleGateway())
	cv, _ := c.Update(c.Init()())
	cvv := cv.(*cost)
	cvv.err = errors.New("blip")
	if !strings.Contains(cvv.View(120, 30), "last-good") {
		t.Fatal("cost should show last-good banner on transient error")
	}
}

func TestSLOCost_EmptyPanels(t *testing.T) {
	s := newSLO(&fakeGateway{sp: sampleGateway().sp, phases: nil, fallbacks: nil})
	v, _ := s.Update(s.Init()())
	out := v.View(120, 30)
	if !strings.Contains(out, "(no data)") || !strings.Contains(out, "(none)") {
		t.Fatalf("SLO empty panels should render placeholders:\n%s", out)
	}
	c := newCost(&fakeGateway{roi: nil, cost: nil})
	cv, _ := c.Update(c.Init()())
	if !strings.Contains(cv.View(120, 30), "no spend") {
		t.Fatal("cost empty report → '(no spend)'")
	}
}

func TestLab_GenStatusAndPrettyJSON(t *testing.T) {
	// genStatus running / failed badge.
	l := newLab(sampleGateway(), testSession())
	l.genTotal, l.genOK, l.genFail, l.genRunning = 10, 3, 0, true
	if !strings.Contains(l.genStatus(), "running") {
		t.Fatal("running generator badge")
	}
	l.genRunning, l.genFail = false, 2
	if !strings.Contains(l.genStatus(), "5/10") || !strings.Contains(l.genStatus(), "failed 2") {
		t.Fatalf("finished generator counts wrong: %s", l.genStatus())
	}
	// prettyJSON: valid → indented, invalid → raw, empty → "".
	if got := prettyJSON(json.RawMessage(`{"a":1}`)); !strings.Contains(got, "\n  \"a\": 1") {
		t.Fatalf("prettyJSON should indent valid JSON: %q", got)
	}
	if got := prettyJSON(json.RawMessage(`not json`)); got != "not json" {
		t.Fatalf("prettyJSON should pass through invalid JSON: %q", got)
	}
	if prettyJSON(nil) != "" {
		t.Fatal("prettyJSON(nil) should be empty")
	}
}

func TestKill_ConfirmViewAndVerb(t *testing.T) {
	k := newKill(sampleGateway(), Session{EnvName: "prod", IsProd: true})
	k.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("e")}) // → confirming
	out := k.View(100, 12)
	if !strings.Contains(out, "ENGAGE") || !strings.Contains(out, "Type the environment name") {
		t.Fatalf("prod confirm view should prompt + name the verb:\n%s", out)
	}
	if engageVerb(true) != "ENGAGE" || engageVerb(false) != "DISENGAGE" {
		t.Fatal("engageVerb mapping wrong")
	}
}

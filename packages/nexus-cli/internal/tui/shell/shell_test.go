package shell

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestShell_NeedWizard(t *testing.T) {
	gw := sampleGateway()
	cases := []struct {
		name string
		deps Deps
		want bool
	}{
		{"nil HasSession", Deps{Gateway: gw}, true},
		{"no session", Deps{Gateway: gw, HasSession: func() bool { return false },
			Session: Session{Model: "m", VKSecret: "s"}}, true},
		{"missing model", Deps{Gateway: gw, HasSession: func() bool { return true },
			Session: Session{VKSecret: "s"}}, true},
		{"missing secret", Deps{Gateway: gw, HasSession: func() bool { return true },
			Session: Session{Model: "m"}}, true},
		{"all present", Deps{Gateway: gw, HasSession: func() bool { return true },
			Session: Session{Model: "m", VKSecret: "s"}}, false},
	}
	for _, c := range cases {
		if got := needWizard(c.deps); got != c.want {
			t.Errorf("%s: needWizard = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestShell_StraightToDashboard(t *testing.T) {
	r := newRecordingDeps(sampleGateway())
	r.hasSession = true
	r.deps.Session = Session{EnvName: "local", Model: "gpt-4o-mini", VKSecret: "nvk"}
	s := NewShell(r.deps)
	if s.inWizard {
		t.Fatal("valid stored selection should skip the wizard")
	}
	if s.Init() == nil {
		t.Fatal("dashboard shell Init should start the first view fetch")
	}
	m, _ := s.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	s = m.(*Shell)
	if !strings.Contains(s.View().Content, "Overview") {
		t.Fatalf("dashboard shell should render the tab row:\n%s", s.View().Content)
	}
	// ctrl+c quits from the dashboard from any focus.
	if _, cmd := s.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}); cmd == nil {
		t.Fatal("ctrl+c should quit")
	}
	// q quits only once the canvas owns the keyboard — while the chat is focused
	// (the launch default) q is literal text in the prompt, not a quit hotkey.
	mt, _ := s.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	s = mt.(*Shell)
	if _, cmd := s.Update(keyRunes("q")); cmd == nil {
		t.Fatal("q should quit from the canvas-focused dashboard")
	}
}

func TestShell_WizardToDashboard(t *testing.T) {
	r := newRecordingDeps(sampleGateway())
	r.hasSession = true
	r.deps.Session = Session{EnvName: "local"} // missing model+secret → wizard
	s := NewShell(r.deps)
	if !s.inWizard {
		t.Fatal("missing selection should start the wizard")
	}
	if s.Init() == nil {
		t.Fatal("wizard shell Init (HasSession) should fetch models")
	}
	s.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	// drive the wizard to its final step, then finish.
	s.wiz.stage = stageSecret
	s.wiz.session.Model = "gpt-4o-mini"
	s.wiz.session.VKID, s.wiz.session.VKName = "vk1", "engineering"
	s.wiz.secret.SetValue("nvk_x")
	// Enter fires the VK-validation probe (a chat completion); the cmd
	// resolves to a vkProbeMsg the wizard folds back to call persistSecret.
	m, cmd := s.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	s = m.(*Shell)
	if cmd == nil {
		t.Fatal("enter at stageSecret should return the probe cmd")
	}
	m, cmd = s.Update(cmd())
	s = m.(*Shell)
	if s.inWizard {
		t.Fatal("finishing the wizard should enter the dashboard")
	}
	if cmd == nil {
		t.Fatal("entering the dashboard should return Init+resize commands")
	}
	if !strings.Contains(s.View().Content, "Overview") {
		t.Fatalf("dashboard should render after the wizard:\n%s", s.View().Content)
	}
	// the dashboard inherits the last window size on the next resize.
	m, _ = s.Update(tea.WindowSizeMsg{Width: 90, Height: 25})
	s = m.(*Shell)
	if s.dash.width != 90 {
		t.Fatalf("dashboard should receive window size, got width=%d", s.dash.width)
	}
}

func TestShell_WizardCtrlCQuits(t *testing.T) {
	s := NewShell(Deps{Gateway: sampleGateway(), HasSession: func() bool { return false }, Session: Session{EnvName: "local"}})
	if !s.inWizard {
		t.Fatal("should start in wizard")
	}
	if _, cmd := s.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}); cmd == nil {
		t.Fatal("ctrl+c should quit even mid-wizard")
	}
}

func TestShell_WizardViewDefaultsHeight(t *testing.T) {
	s := NewShell(Deps{Gateway: sampleGateway(), HasSession: func() bool { return false }, Session: Session{EnvName: "local"}})
	// no WindowSizeMsg yet → View must still render (height defaulted).
	if !strings.Contains(s.View().Content, "nexus setup") {
		t.Fatalf("wizard view should render without a prior resize:\n%s", s.View().Content)
	}
}

// TestShell_EnvSwitchReentersWizard covers the /env round-trip: a
// wantEnvSwitchMsg dispatched while the dashboard is active must hand
// control back to a fresh wizard at the env picker, with the live env list
// visible so the operator can switch / edit / delete without quitting.
func TestShell_EnvSwitchReentersWizard(t *testing.T) {
	r := newRecordingDeps(sampleGateway()).withEnvStep([]string{"local", "staging"})
	r.hasSession = true
	r.deps.Session = Session{EnvName: "local", Model: "gpt-4o-mini", VKSecret: "nvk_x"}
	s := NewShell(r.deps)
	if s.inWizard {
		t.Fatal("a complete session should go straight to the dashboard")
	}
	s.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m, cmd := s.Update(wantEnvSwitchMsg{})
	s = m.(*Shell)
	if !s.inWizard {
		t.Fatal("wantEnvSwitchMsg must reopen the wizard")
	}
	if s.wiz.stage != stageEnv {
		t.Fatalf("reopened wizard should start at the env picker, got %v", s.wiz.stage)
	}
	if cmd == nil {
		t.Fatal("reopen should return a window-resize cmd so the env picker renders at the right size")
	}
	if !strings.Contains(s.View().Content, "staging") {
		t.Fatalf("reopened wizard should list configured envs:\n%s", s.View().Content)
	}
}

// TestSlash_EnvCommandRouting covers the dashboard's slash-palette wiring:
// selecting the /env command must emit wantEnvSwitchMsg so the Shell can
// catch it and reopen the wizard.
func TestSlash_EnvCommandRouting(t *testing.T) {
	m := NewModel(sampleGateway(), Session{EnvName: "local"}, nil)
	envCmd := slashCmd{name: "env", kind: slashShell}
	_, cmd := m.handleSlash(slashSelectedMsg{cmd: envCmd})
	if cmd == nil {
		t.Fatal("/env must return a cmd")
	}
	if _, ok := cmd().(wantEnvSwitchMsg); !ok {
		t.Fatalf("/env cmd must resolve to wantEnvSwitchMsg, got %T", cmd())
	}
}

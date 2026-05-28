package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
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
	if !strings.Contains(s.View(), "Overview") {
		t.Fatalf("dashboard shell should render the tab row:\n%s", s.View())
	}
	// ctrl+c quits from the dashboard.
	if _, cmd := s.Update(tea.KeyMsg{Type: tea.KeyCtrlC}); cmd == nil {
		t.Fatal("ctrl+c should quit")
	}
	// q also quits (dashboard sets quitting).
	if _, cmd := s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")}); cmd == nil {
		t.Fatal("q should quit from the dashboard")
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
	m, cmd := s.Update(tea.KeyMsg{Type: tea.KeyEnter})
	s = m.(*Shell)
	if s.inWizard {
		t.Fatal("finishing the wizard should enter the dashboard")
	}
	if cmd == nil {
		t.Fatal("entering the dashboard should return Init+resize commands")
	}
	if !strings.Contains(s.View(), "Overview") {
		t.Fatalf("dashboard should render after the wizard:\n%s", s.View())
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
	if _, cmd := s.Update(tea.KeyMsg{Type: tea.KeyCtrlC}); cmd == nil {
		t.Fatal("ctrl+c should quit even mid-wizard")
	}
}

func TestShell_WizardViewDefaultsHeight(t *testing.T) {
	s := NewShell(Deps{Gateway: sampleGateway(), HasSession: func() bool { return false }, Session: Session{EnvName: "local"}})
	// no WindowSizeMsg yet → View must still render (height defaulted).
	if !strings.Contains(s.View(), "nexus setup") {
		t.Fatalf("wizard view should render without a prior resize:\n%s", s.View())
	}
}

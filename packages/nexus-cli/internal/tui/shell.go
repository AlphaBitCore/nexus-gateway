package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// Shell is the top-level program model. It runs the entry wizard first when the
// stored selection is missing/invalid, then hands off to the dashboard. Once on
// the dashboard it is a thin pass-through.
type Shell struct {
	deps     Deps
	inWizard bool
	wiz      *wizard
	dash     Model

	width, height int
}

// needWizard reports whether the wizard must run: no usable session, or no
// remembered model + VK secret to skip it (FR-13).
func needWizard(d Deps) bool {
	if d.HasSession == nil || !d.HasSession() {
		return true
	}
	return d.Session.Model == "" || strings.TrimSpace(d.Session.VKSecret) == ""
}

// NewShell builds the entry shell. It either starts in the wizard or goes
// straight to the dashboard depending on the stored selection.
func NewShell(d Deps) *Shell {
	s := &Shell{deps: d, inWizard: needWizard(d)}
	if s.inWizard {
		s.wiz = newWizard(d)
	} else {
		s.dash = NewModel(d.Gateway, d.Session)
	}
	return s
}

func (s *Shell) Init() tea.Cmd {
	if s.inWizard {
		return s.wiz.Init()
	}
	return s.dash.Init()
}

func (s *Shell) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if sz, ok := msg.(tea.WindowSizeMsg); ok {
		s.width, s.height = sz.Width, sz.Height
	}
	// ctrl+c always quits, even mid-wizard.
	if k, ok := msg.(tea.KeyMsg); ok && k.String() == "ctrl+c" {
		return s, tea.Quit
	}
	if s.inWizard {
		next, cmd := s.wiz.Update(msg)
		s.wiz = next
		if s.wiz.done {
			return s.enterDashboard()
		}
		return s, cmd
	}
	dm, cmd := s.dash.Update(msg)
	s.dash = dm.(Model)
	if s.dash.quitting {
		return s, tea.Quit
	}
	return s, cmd
}

// enterDashboard builds the dashboard from the wizard's resolved session and
// (possibly env-switched) gateway, and re-sends the last window size so the
// first frame is laid out correctly.
func (s *Shell) enterDashboard() (tea.Model, tea.Cmd) {
	s.inWizard = false
	s.dash = NewModel(s.wiz.gateway, s.wiz.session)
	w, h := s.width, s.height
	return s, tea.Batch(s.dash.Init(), func() tea.Msg {
		return tea.WindowSizeMsg{Width: w, Height: h}
	})
}

func (s *Shell) View() string {
	if s.inWizard {
		h := s.height
		if h == 0 {
			h = 24
		}
		return s.wiz.View(s.width, h)
	}
	return s.dash.View()
}

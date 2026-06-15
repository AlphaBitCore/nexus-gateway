package shell

import (
	"context"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

// wantEnvSwitchMsg is emitted by the dashboard when the operator runs /env in
// the slash palette; the Shell catches it and rebuilds the wizard starting at
// stageEnv so they can switch / edit / delete an environment without
// quitting + relaunching.
type wantEnvSwitchMsg struct{}

// wantLoginMsg / wantLogoutMsg are emitted by the dashboard's /login and /logout
// slash commands. The Shell owns them because credentials live at the CLI layer
// (the keychain), not inside the dashboard model. /login re-runs the browser
// OAuth flow in place: on success the dashboard's existing gateway recovers on
// its next poll (the token source re-reads the freshly stored token), so an
// expired-session "(showing last-good data)" view heals without losing context.
// /logout clears the stored credentials and drops back to the entry wizard,
// which then requires a fresh login.
type wantLoginMsg struct{}
type wantLogoutMsg struct{}

// loginResultMsg carries the outcome of an in-place /login browser flow.
type loginResultMsg struct{ err error }

// reauthTimeout bounds an in-place /login so an abandoned browser flow cannot
// leave the loopback-callback goroutine waiting forever.
const reauthTimeout = 3 * time.Minute

// Shell is the top-level program model. It runs the entry wizard first when the
// stored selection is missing/invalid, then hands off to the dashboard. Once on
// the dashboard it is a thin pass-through.
type Shell struct {
	deps     Deps
	inWizard bool
	wiz      *wizard
	dash     Model

	// loggingIn is true while an in-place /login browser flow is outstanding; the
	// dashboard keeps polling underneath, but the View shows a re-auth notice so
	// the operator knows to finish the login in the opened browser tab.
	loggingIn bool

	width, height int
}

// needWizard reports whether the wizard must run: no usable session, or no
// remembered model + VK secret to skip it.
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
		s.dash = NewModel(d.Gateway, d.Session, d.BuildAgent)
		s.wireDash()
	}
	return s
}

// wireDash connects the dashboard's runtime-model-switch persistence seam to
// Deps.SaveSelection (nil-safe — tests / single-shot builds leave it unset) and
// hands the conversation the diagnostic logger so user-visible turn failures are
// mirrored into the file log.
func (s *Shell) wireDash() {
	if s.deps.SaveSelection != nil {
		s.dash.applyModel = s.deps.SaveSelection
	}
	if s.deps.OpenSessions != nil {
		s.dash.openSessions = s.deps.OpenSessions
	}
	if s.dash.conv != nil {
		s.dash.conv.log = s.deps.Log
	}
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
	// /env from the dashboard pops back to the wizard's env picker; the new
	// wizard starts at stageEnv with the live env list, so the operator can
	// switch / edit / delete without quitting.
	if _, ok := msg.(wantEnvSwitchMsg); ok && !s.inWizard {
		return s.reopenWizard()
	}
	// /login re-authenticates in place; /logout clears credentials + reopens the
	// wizard. Both are no-ops mid-wizard (it owns its own login step).
	if _, ok := msg.(wantLoginMsg); ok && !s.inWizard {
		return s.startReauth()
	}
	if _, ok := msg.(wantLogoutMsg); ok && !s.inWizard {
		if s.deps.Logout != nil {
			_ = s.deps.Logout()
		}
		return s.reopenWizard()
	}
	if r, ok := msg.(loginResultMsg); ok {
		s.loggingIn = false
		// On success nothing else is needed: the dashboard's gateway shares the
		// keychain-backed token source, so its next poll picks up the fresh token
		// and the "(showing last-good data)" banner clears. On failure the
		// dashboard's own unauthorized banner stays up; the operator can retry.
		_ = r.err
		return s, nil
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

// reopenWizard hands control from the dashboard back to a fresh wizard at the
// env picker, re-reading the deps' EnvNames so any out-of-band edits show up.
// The dashboard state is discarded — when the wizard finishes a new dashboard
// is built from the (possibly switched) gateway + session.
func (s *Shell) reopenWizard() (tea.Model, tea.Cmd) {
	s.inWizard = true
	s.wiz = newWizard(s.deps)
	w, h := s.width, s.height
	return s, tea.Batch(s.wiz.Init(), func() tea.Msg {
		return tea.WindowSizeMsg{Width: w, Height: h}
	})
}

// startReauth kicks off an in-place browser login. The dashboard is left intact
// (and keeps polling) so a successful re-auth resumes exactly where the operator
// was. A nil Login dep (single-env / test builds) is a no-op. The flow is bounded
// by reauthTimeout so an abandoned browser tab cannot wedge the callback.
func (s *Shell) startReauth() (tea.Model, tea.Cmd) {
	if s.deps.Login == nil {
		return s, nil
	}
	s.loggingIn = true
	login := s.deps.Login
	return s, func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), reauthTimeout)
		defer cancel()
		return loginResultMsg{err: login(ctx)}
	}
}

// enterDashboard builds the dashboard from the wizard's resolved session and
// (possibly env-switched) gateway, and re-sends the last window size so the
// first frame is laid out correctly.
func (s *Shell) enterDashboard() (tea.Model, tea.Cmd) {
	s.inWizard = false
	s.dash = NewModel(s.wiz.gateway, s.wiz.session, s.deps.BuildAgent)
	s.wireDash()
	w, h := s.width, s.height
	return s, tea.Batch(s.dash.Init(), func() tea.Msg {
		return tea.WindowSizeMsg{Width: w, Height: h}
	})
}

func (s *Shell) View() tea.View {
	if s.inWizard {
		h := s.height
		if h == 0 {
			h = 24
		}
		v := tea.NewView(s.wiz.View(s.width, h))
		v.AltScreen = true
		return v
	}
	if s.loggingIn {
		v := tea.NewView("\n  Re-authenticating…\n\n" +
			"  Complete the sign-in in the browser tab that just opened.\n" +
			"  This screen resumes the dashboard automatically once you're signed in.\n")
		v.AltScreen = true
		return v
	}
	return s.dash.View()
}

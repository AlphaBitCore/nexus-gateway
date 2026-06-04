package shell

import (
	tea "charm.land/bubbletea/v2"
)

// Run starts the operator console — the entry wizard (first run / invalid
// stored selection) followed by the dashboard — and blocks until the user
// quits. deps carries the gateway plus the auth/persistence callbacks.
func Run(deps Deps) error {
	// Alt-screen is set on the view (View.AltScreen) in v2, not as a program option.
	return run(NewShell(deps))
}

// run executes a program for model with the given options. It is a package var
// so tests can drive the loop headlessly (or stub it) without a real TTY.
var run = func(model tea.Model, opts ...tea.ProgramOption) error {
	_, err := tea.NewProgram(model, opts...).Run()
	return err
}

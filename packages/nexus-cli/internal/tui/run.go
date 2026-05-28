package tui

import (
	tea "github.com/charmbracelet/bubbletea"
)

// Run starts the operator console — the entry wizard (first run / invalid
// stored selection) followed by the dashboard — and blocks until the user
// quits. deps carries the gateway plus the auth/persistence callbacks.
func Run(deps Deps) error {
	return run(NewShell(deps), tea.WithAltScreen())
}

// run executes a program for model with the given options. It is a package var
// so tests can drive the loop headlessly (or stub it) without a real TTY.
var run = func(model tea.Model, opts ...tea.ProgramOption) error {
	_, err := tea.NewProgram(model, opts...).Run()
	return err
}

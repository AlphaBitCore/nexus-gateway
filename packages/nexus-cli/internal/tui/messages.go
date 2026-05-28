package tui

import (
	"context"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// Poll intervals: the radar refreshes fast; aggregate views slower (NFR-4).
const (
	pollFast = 2 * time.Second
	pollSlow = 5 * time.Second
)

// loginTimeout bounds the wizard's browser-login wait (the loopback listener
// closes when it fires so a never-completed login does not hang the wizard).
const loginTimeout = 3 * time.Minute

// tick schedules msg after d.
func tick(d time.Duration, msg tea.Msg) tea.Cmd {
	return tea.Tick(d, func(time.Time) tea.Msg { return msg })
}

// fetchCtx bounds a single data fetch so a hung gateway never freezes a view.
func fetchCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 10*time.Second)
}

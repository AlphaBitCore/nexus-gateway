package shell

import (
	tea "charm.land/bubbletea/v2"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/kit"
	viewpkg "github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/views"
)

// app_nav.go — the root model's navigation and drill plumbing: slash/agent
// view jumps, row drills (event), the breadcrumb stack,
// and the canvas-focus handoff. Split from app.go along the navigation seam
// so the root Update stays the message router, not the navigator.

// focusCanvasForView moves keyboard focus to the canvas (snapping the split) when a
// slash command drives a view, so the operator lands ready to navigate it.
func (m Model) focusCanvasForView() Model {
	m.focus = focusCanvas
	m.conv.blur()
	m.easeFrame = easeFrames
	return m
}

// jumpTop switches to a top-level view, resetting the drill path (a lateral jump
// starts a fresh breadcrumb rather than deepening the current trail). Leaving a
// streaming view tears its background stream down first.
func (m Model) jumpTop(i int) (tea.Model, tea.Cmd) {
	if i < 0 || i >= len(m.views) {
		return m, nil
	}
	m.leaveActive(i)
	m.nav.reset()
	m.active = i
	return m, m.views[i].Init()
}

// drillEvent pushes the current view onto the nav stack and opens the Event view
// on the given id (esc later pops back to where the operator drilled from).
func (m Model) drillEvent(msg kit.OpenEventMsg) (tea.Model, tea.Cmd) {
	idx := m.indexOf("Event")
	m = m.drillTo(idx)
	ev := m.views[m.active].(*viewpkg.EventView)
	if msg.Explain {
		ev.SetIDExplain(msg.ID)
	} else {
		ev.SetID(msg.ID)
	}
	return m, ev.Init()
}

// applyAgentNav is the agent's navigate canvas drive: open the named view
// (drilling so esc returns), applying a radar filter when one rode along, and
// keep pumping the bridge.
func (m Model) applyAgentNav(msg agentNavMsg) (tea.Model, tea.Cmd) {
	idx := resolveViewIndex(m.entries, msg.view)
	if idx < 0 {
		return m, m.conv.drainCmd()
	}
	m = m.drillTo(idx)
	if r, ok := m.views[m.active].(*viewpkg.Radar); ok {
		r.ApplyFilter(msg.filter)
	}
	return m, tea.Batch(m.views[m.active].Init(), m.conv.drainCmd())
}

// applyAgentShow is the agent's show_event canvas drive.
func (m Model) applyAgentShow(msg agentShowMsg) (tea.Model, tea.Cmd) {
	idx := m.indexOf("Event")
	m = m.drillTo(idx)
	ev := m.views[m.active].(*viewpkg.EventView)
	ev.SetID(msg.id)
	return m, tea.Batch(ev.Init(), m.conv.drainCmd())
}

// drillTo pushes the current view onto the nav stack and switches to i (a drill
// deepens the trail). It returns the updated model: the receiver is by value, so
// callers MUST use the result (m = m.drillTo(i)) — otherwise the active/nav
// mutations are lost on the copy.
func (m Model) drillTo(i int) Model {
	if i < 0 || i >= len(m.views) || i == m.active {
		return m
	}
	m.leaveActive(i)
	m.nav.push(m.active)
	m.active = i
	return m
}

// popNav walks one step back up the drill path; past the root it lands on the
// cockpit (index 0). A no-op when already at the cockpit with an empty stack.
func (m Model) popNav() (tea.Model, tea.Cmd) {
	idx, ok := m.nav.pop()
	if !ok && m.active == 0 {
		return m, nil
	}
	m.leaveActive(idx)
	m.active = idx
	return m, m.views[idx].Init()
}

// leaveActive tears down the active view's background stream when navigating to a
// different view, so a mid-stream switch never leaks the goroutine + connection.
func (m Model) leaveActive(next int) {
	if next == m.active {
		return
	}
	if l, ok := m.views[m.active].(leaver); ok {
		l.Leave()
	}
}

// indexOf resolves a registry entry name to its view index (0 if absent — the
// registry always carries the named core views, so this is a safe default).
func (m Model) indexOf(name string) int {
	for i, e := range m.entries {
		if e.name == name {
			return i
		}
	}
	return 0
}

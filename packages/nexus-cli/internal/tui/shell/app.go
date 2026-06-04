package shell

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/kit"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/resource"
	viewpkg "github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/views"
)

// Model is the root Bubble Tea model: an optional prod banner, a breadcrumb trail,
// a vertical split of the active view (top canvas) over the resident conversation
// pane, and a footer that carries the keybar/working-spinner or the open slash
// palette plus the bottom-right environment indicator.
type Model struct {
	session Session

	width, height int
	active        int
	entries       []viewEntry
	views         []viewModel
	nav           navStack
	quitting      bool

	slashOpen bool
	slash     slashPalette

	conv      *conversation
	focus     paneFocus // which pane owns the keyboard; zero value = focusChat
	easeFrame int       // split-resize ease progress; easeFrames = settled
	easing    bool      // a splitTick loop is in flight (guards against duplicate chains)

	// applyModel persists a runtime model switch (wired from Deps.SaveSelection);
	// nil in tests / when no persistence seam is available.
	applyModel func(model, vkID, vkName string) error

	gw Gateway
}

// splitTick advances the split-resize ease one frame after a focus change.
type splitTick struct{}

// NewModel builds the root dashboard over gw for the given session. Views and
// their registry entries are built in lockstep (index i aligns). An optional
// AgentBuildFunc wires the conversation's gateway agent; omitted (tests) the
// conversation reports the agent unavailable but the shell is fully navigable.
func NewModel(gw Gateway, s Session, build ...AgentBuildFunc) Model {
	views := []viewModel{
		viewpkg.NewCockpit(gw),
		viewpkg.NewRadar(gw),
		viewpkg.NewEvent(gw, s),
		viewpkg.NewSLO(gw, s),
		viewpkg.NewCost(gw, s),
		viewpkg.NewChat(gw, s),
		viewpkg.NewLab(gw, s),
		viewpkg.NewKill(gw, s),
		viewpkg.NewAlerts(gw),
		viewpkg.NewNodes(gw),
		viewpkg.NewCompliance(gw),
		viewpkg.NewJobs(gw),
		viewpkg.NewConfigSync(gw),
		viewpkg.NewModels(gw),
		viewpkg.NewVKs(gw, s),
		viewpkg.NewRouting(gw, s),
		resource.NewResource(gw, s),
	}
	entries := []viewEntry{
		{name: "Overview", aliases: []string{"ov", "home", "health", "cockpit"}},
		{name: "Radar", aliases: []string{"traffic", "rd", "live"}},
		{name: "Event", aliases: []string{"ev", "drill"}},
		{name: "SLO", aliases: []string{"perf", "latency", "availability"}},
		{name: "Cost", aliases: []string{"spend", "$", "money"}},
		{name: "Chat", aliases: []string{"raw", "playground"}},
		{name: "Lab", aliases: []string{"sim", "simulate", "route"}},
		{name: "Kill", aliases: []string{"killswitch", "ks", "passthrough"}},
		{name: "Alerts", aliases: []string{"al", "firing"}},
		{name: "Nodes", aliases: []string{"no", "fleet", "drift"}},
		{name: "Compliance", aliases: []string{"comp", "block", "governance"}},
		{name: "Jobs", aliases: []string{"job", "schedule", "cron"}},
		{name: "Sync", aliases: []string{"config-sync", "configsync", "outofsync"}},
		{name: "Models", aliases: []string{"model", "catalog", "mc"}},
		{name: "Keys", aliases: []string{"vk", "virtual-keys", "revoke", "regenerate"}},
		{name: "Rules", aliases: []string{"routing", "routing-rules", "rr", "toggle"}},
		{name: "Resource", aliases: []string{"resources", "res", "kind", "kinds", "admin"}},
	}
	var ba AgentBuildFunc
	if len(build) > 0 {
		ba = build[0]
	}
	conv := newConversation(s, ba)
	conv.input.Focus() // chat is the launch default (focus paneFocus zero value = focusChat)
	return Model{gw: gw, session: s, entries: entries, views: views, conv: conv, easeFrame: easeFrames}
}

// Init starts the first view, focuses the resident chat prompt (the launch default
// is chat focus), and kicks the conversation's animation clock (typewriter +
// working spinner), which then re-issues itself.
func (m Model) Init() tea.Cmd {
	return tea.Batch(m.views[m.active].Init(), m.conv.focus(), kit.Tick(kit.ConvAnimInterval, convTick{}))
}

// currentMode reports the interaction mode (design §3). Chat focus = INPUT (the
// whole keyboard, incl. IME, goes to the conversation). Canvas focus = NORMAL
// (single-key hotkeys, IME-guarded) unless the active view itself captures text.
// The slash palette always forces INPUT. In NORMAL the IME guard (isHotkey) drops
// multibyte/composed runes so an active CJK IME never misfires.
func (m Model) currentMode() inputMode {
	if m.slashOpen || m.focus == focusChat {
		return modeInput
	}
	if c, ok := m.views[m.active].(textCapturer); ok && c.Capturing() {
		return modeInput
	}
	return modeNormal
}

// Update folds global navigation + the slash/conversation/agent message streams
// and delegates everything else to the active view.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case splitTick:
		if m.easeFrame < easeFrames {
			m.easeFrame++
			return m, kit.Tick(kit.AnimInterval/8, splitTick{})
		}
		m.easing = false
		return m, nil
	case kit.JumpMsg:
		return m.jumpTop(msg.Index)
	case slashCloseMsg:
		m.slashOpen = false
		return m, nil
	case slashSelectedMsg:
		m.slashOpen = false
		return m.handleSlash(msg)
	case kit.SetModelMsg:
		return m.setChatModel(msg.Code), nil
	case kit.OpenEventMsg:
		return m.drillEvent(msg)
	case agentNavMsg:
		return m.applyAgentNav(msg)
	case agentShowMsg:
		return m.applyAgentShow(msg)
	case agentHighlightMsg:
		if h, ok := m.views[m.active].(highlighter); ok {
			h.Highlight(msg.ref)
		}
		return m, m.conv.drainCmd()
	case agentConfirmMsg:
		// A mitigation needs the operator's authorization on a visible surface.
		// The chat is always resident now; force focus to it so the Allow/Deny
		// confirm gate owns the keyboard.
		m.focus = focusChat
		m.easeFrame = easeFrames // snap (no ease) so the gate is immediately full-size
		return m, m.conv.Update(msg)
	case convTick:
		return m, m.conv.Update(msg)
	case agentTextMsg, agentReasoningMsg, agentToolMsg, agentToolResultMsg, confirmReplyMsg, agentDoneMsg, contextStatsMsg, agentCompactMsg:
		return m, m.conv.Update(msg)
	case tea.PasteMsg:
		return m.handlePaste(msg)
	case tea.KeyPressMsg:
		return m.handleKey(msg)
	}
	updated, cmd := m.views[m.active].Update(msg)
	m.views[m.active] = updated
	return m, cmd
}

// handlePaste routes bracketed-paste content (a tea.PasteMsg, distinct from a
// key) to whichever surface owns the keyboard: the resident chat when it is
// focused, otherwise a canvas view that is capturing text. A paste into a
// non-text surface (or the slash palette) is dropped rather than misapplied.
func (m Model) handlePaste(msg tea.PasteMsg) (tea.Model, tea.Cmd) {
	if m.slashOpen {
		return m, nil
	}
	if m.focus == focusChat {
		return m, m.conv.Update(msg)
	}
	if c, ok := m.views[m.active].(textCapturer); ok && c.Capturing() {
		updated, cmd := m.views[m.active].Update(msg)
		m.views[m.active] = updated
		return m, cmd
	}
	return m, nil
}

// handleKey routes a keystroke through the modal grammar: ctrl+c quits; the slash
// palette wins; tab is the global pane-focus toggle; then chat-focus routes the
// keyboard to the conversation and canvas-focus routes a text-capturing view or
// the NORMAL hotkeys (IME-guarded).
func (m Model) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "ctrl+c" {
		return m.quit()
	}
	if m.slashOpen {
		var cmd tea.Cmd
		m.slash, cmd = m.slash.Update(msg)
		return m, cmd
	}
	// tab toggles which pane owns the keyboard. Suppressed only while the confirm
	// gate is up (the operator is choosing Allow/Deny).
	if msg.String() == "tab" && !m.conv.cf.Capturing() {
		return m.toggleFocus()
	}
	if m.focus == focusChat {
		// Keep the agent's turn context current with whatever the canvas shows.
		m.conv.setActiveView(m.entries[m.active].name)
		// "/" on an empty prompt opens the shared command palette (Claude-Code style);
		// a non-empty prompt keeps "/" as a literal so it can appear mid-message.
		if msg.String() == "/" && strings.TrimSpace(m.conv.input.Value()) == "" &&
			!m.conv.running && !m.conv.cf.Capturing() {
			m.slashOpen = true
			m.slash = newSlashPalette(defaultSlashCommands())
			return m, nil
		}
		// Idle esc (no running turn, no gate) moves focus up to the canvas; otherwise
		// the conversation owns esc (interrupt a running turn / cancel the gate).
		if msg.String() == "esc" && !m.conv.running && !m.conv.cf.Capturing() {
			return m.focusCanvas()
		}
		return m, m.conv.Update(msg)
	}
	// Canvas focused.
	if c, ok := m.views[m.active].(textCapturer); ok && c.Capturing() {
		updated, cmd := m.views[m.active].Update(msg)
		m.views[m.active] = updated
		return m, cmd
	}
	// NORMAL mode: drop non-hotkeys (the IME guard) before dispatching.
	if m.currentMode() == modeNormal && !isHotkey(msg) {
		return m, nil
	}
	switch msg.String() {
	case "/":
		m.slashOpen = true
		m.slash = newSlashPalette(defaultSlashCommands())
		return m, nil
	case "esc", "left":
		// Back: `left` is an alias for `esc` (no view uses the arrow for in-view
		// navigation). Let the active view close its own inner level (a detail
		// drawer) first; only when it has nothing to back out of does the root pop
		// the nav stack (design §3: "esc/← back one level").
		if bh, ok := m.views[m.active].(backHandler); ok && bh.Back() {
			return m, nil
		}
		return m.popNav()
	case "q":
		return m.quit()
	case "1", "2", "3", "4", "5", "6", "7", "8", "9":
		if i := int(msg.String()[0] - '1'); i < len(m.views) {
			return m.jumpTop(i)
		}
		return m, nil
	}
	updated, cmd := m.views[m.active].Update(msg)
	m.views[m.active] = updated
	return m, cmd
}

// toggleFocus flips keyboard ownership between the chat and the canvas.
func (m Model) toggleFocus() (tea.Model, tea.Cmd) {
	if m.focus == focusChat {
		return m.focusCanvas()
	}
	return m.focusChatPane()
}

// focusCanvas hands the keyboard to the top view (NORMAL mode), blurs the chat
// prompt so single-key hotkeys resume, and starts the split-resize ease.
func (m Model) focusCanvas() (tea.Model, tea.Cmd) {
	m.focus = focusCanvas
	m.conv.blur()
	return m, m.startEase()
}

// focusChatPane hands the keyboard to the bottom conversation (INPUT mode),
// focuses its prompt, and starts the split-resize ease.
func (m Model) focusChatPane() (tea.Model, tea.Cmd) {
	m.focus = focusChat
	return m, tea.Batch(m.conv.focus(), m.startEase())
}

// startEase restarts the split-resize interpolation from frame 0 and returns a tick
// to advance it — but reuses an in-flight ease loop rather than spawning a second
// one, so rapid focus toggles never stack concurrent tick chains.
func (m *Model) startEase() tea.Cmd {
	m.easeFrame = 0
	if m.easing {
		return nil
	}
	m.easing = true
	return kit.Tick(kit.AnimInterval/8, splitTick{})
}

// quit tears down any in-flight agent turn (so the bridge goroutine never
// outlives the program) and signals the shell to exit.
func (m Model) quit() (tea.Model, tea.Cmd) {
	m.conv.stop()
	m.quitting = true
	return m, tea.Quit
}

// sessionSetter is implemented by views that hold the resolved Session (model/VK)
// so a runtime model switch propagates to their own gateway calls + display.
type sessionSetter interface{ SetSession(Session) }

// setChatModel switches the chat/agent model everywhere: persist (if wired), update
// the root + conversation session, reset the agent (next turn rebuilds with the new
// model), and broadcast to the session-bearing views.
func (m Model) setChatModel(code string) Model {
	if m.applyModel != nil {
		_ = m.applyModel(code, m.session.VKID, m.session.VKName)
	}
	m.session.Model = code
	m.conv.setModel(code)
	m.conv.session.Model = code
	for i, v := range m.views {
		if ss, ok := v.(sessionSetter); ok {
			ss.SetSession(m.session)
			m.views[i] = v
		}
	}
	return m
}

// handleSlash dispatches a selected slash command: a view command resolves to a
// top-level view (with any trailing arg, e.g. an event id); an agent command runs
// its control in the resident chat (focusing it).
func (m Model) handleSlash(msg slashSelectedMsg) (tea.Model, tea.Cmd) {
	if msg.cmd.kind == slashAgent {
		m.focus = focusChat
		m.easeFrame = easeFrames // snap to the chat-focused split
		m.conv.setActiveView(m.entries[m.active].name)
		ccmd := m.conv.agentCommand("/" + msg.cmd.name)
		return m, tea.Batch(m.conv.focus(), ccmd)
	}
	// /env hands the dashboard back to the entry wizard at the env picker so
	// the operator can switch/edit/delete environments without quitting +
	// relaunching. Shell intercepts wantEnvSwitchMsg.
	if msg.cmd.kind == slashShell && msg.cmd.name == "env" {
		return m, func() tea.Msg { return wantEnvSwitchMsg{} }
	}
	// /model switches the chat model: with an arg, directly (focus stays put — no
	// canvas to land on); without, opens the Models view in pick mode and focuses it.
	if msg.cmd.name == "model" {
		if arg := strings.TrimSpace(msg.arg); arg != "" {
			return m.setChatModel(arg), nil
		}
		m = m.focusCanvasForView()
		idx := m.indexOf("Models")
		m = m.drillTo(idx)
		mv := m.views[m.active].(*viewpkg.ModelsView)
		mv.EnterPick(m.session.Model)
		return m, mv.Init()
	}
	// /event <id> drills straight into the Event view with the id.
	if msg.cmd.name == "event" && strings.TrimSpace(msg.arg) != "" {
		m = m.focusCanvasForView()
		return m.drillEvent(kit.OpenEventMsg{ID: strings.TrimSpace(msg.arg)})
	}
	idx := resolveViewIndex(m.entries, msg.cmd.name)
	if idx < 0 {
		return m, nil
	}
	// A view command drives the top canvas — move focus there so the operator can
	// immediately navigate the result.
	m = m.focusCanvasForView()
	return m.jumpTop(idx)
}

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

// View composes the (prod-only) banner, breadcrumb, the vertical split (top canvas
// over the resident bottom chat), and the footer into a tea.View. It positions the
// real terminal cursor at the chat input when the chat is focused, so a CJK input
// method anchors its candidate window at the prompt (the IME fix).

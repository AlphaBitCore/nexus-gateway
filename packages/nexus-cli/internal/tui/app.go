package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/styles"
)

// Session is the resolved context the dashboard renders against: the active
// environment plus the remembered model/VK selection. VKSecret is held only in
// memory for the Chat + Lab views — it is never written to the on-disk config.
type Session struct {
	EnvName  string
	Addr     string // Control Plane base URL, shown in the location indicator
	IsProd   bool
	Model    string // selected model code/slug
	VKID     string
	VKName   string
	VKSecret string
}

// viewModel is one tab. Views are self-contained: Init starts their data
// fetch/poll, Update folds messages, View renders into the given content box.
type viewModel interface {
	Init() tea.Cmd
	Update(msg tea.Msg) (viewModel, tea.Cmd)
	View(width, height int) string
}

// openEventMsg is emitted by the radar when a row is selected (or by the Ask-Nexus
// explain intent); the app switches to the event view, loads the id, and — when
// explain is set — auto-starts the LLM explanation once the event loads.
type openEventMsg struct {
	id      string
	explain bool
}

// Model is the root Bubble Tea model: a status bar, a tab row, the active
// view, a help bar, and the command-palette overlay.
type Model struct {
	session Session

	width, height int
	active        int
	tabs          []string
	entries       []viewEntry
	views         []viewModel
	quitting      bool

	paletteOpen bool
	pal         palette

	gw      Gateway // retained so the `>` ask overlay can be built on demand
	askOpen bool
	ask     askBar
}

// NewModel builds the root dashboard model over gw for the given session. Views
// and their palette/tab entries are built in lockstep (index i aligns).
func NewModel(gw Gateway, s Session) Model {
	views := []viewModel{
		newOverview(gw),
		newRadar(gw),
		newEvent(gw, s),
		newSLO(gw, s),
		newCost(gw, s),
		newChat(gw, s),
		newLab(gw, s),
		newKill(gw, s),
		newAlerts(gw),
		newNodes(gw),
		newCompliance(gw),
		newJobs(gw),
		newConfigSync(gw),
		newModels(gw),
		newVKs(gw, s),
		newRouting(gw, s),
	}
	entries := []viewEntry{
		{name: "Overview", aliases: []string{"ov", "home", "health"}},
		{name: "Radar", aliases: []string{"traffic", "rd", "live"}},
		{name: "Event", aliases: []string{"ev", "drill"}},
		{name: "SLO", aliases: []string{"perf", "latency", "availability"}},
		{name: "Cost", aliases: []string{"spend", "$", "money"}},
		{name: "Chat", aliases: []string{"playground", "ask"}},
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
	}
	tabs := make([]string, len(entries))
	for i, e := range entries {
		tabs[i] = e.name
	}
	return Model{gw: gw, session: s, tabs: tabs, entries: entries, views: views}
}

// Init starts the first view.
func (m Model) Init() tea.Cmd { return m.views[m.active].Init() }

// Update folds global keys (tab/number/quit) and delegates everything else to
// the active view. Selecting a radar row switches to the Event view.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case jumpMsg:
		m.paletteOpen = false
		return m.switchTo(msg.index)
	case paletteCloseMsg:
		m.paletteOpen = false
		return m, nil
	case askCloseMsg:
		m.askOpen = false
		return m, nil
	case navigateMsg:
		m.askOpen = false
		return m.navigateTo(msg)
	case askRouteDeltaMsg, askRouteDoneMsg, askDataMsg, askAnswerDeltaMsg, askAnswerDoneMsg:
		if m.askOpen {
			var acmd tea.Cmd
			m.ask, acmd = m.ask.Update(msg)
			return m, acmd
		}
		return m, nil
	case openEventMsg:
		m.active = m.eventIndex()
		ev := m.views[m.active].(*eventView)
		if msg.explain {
			ev.setIDExplain(msg.id)
		} else {
			ev.setID(msg.id)
		}
		return m, ev.Init()
	case tea.KeyMsg:
		// While the command palette is open it owns every keystroke.
		if m.paletteOpen {
			if msg.String() == "ctrl+c" {
				m.quitting = true
				return m, tea.Quit
			}
			var pcmd tea.Cmd
			m.pal, pcmd = m.pal.Update(msg)
			return m, pcmd
		}
		// While the ask overlay is open it owns every keystroke (ctrl+c still quits).
		if m.askOpen {
			if msg.String() == "ctrl+c" {
				m.quitting = true
				return m, tea.Quit
			}
			var acmd tea.Cmd
			m.ask, acmd = m.ask.Update(msg)
			return m, acmd
		}
		// While a view is capturing text (chat prompt, lab editor, kill confirm),
		// global single-letter shortcuts must not steal the keystroke — but tab
		// still navigates (so the operator is never trapped) and ctrl+c quits.
		if c, ok := m.views[m.active].(textCapturer); ok && c.capturing() {
			switch msg.String() {
			case "ctrl+c":
				m.quitting = true
				return m, tea.Quit
			case "tab":
				return m.switchTo((m.active + 1) % len(m.views))
			case "shift+tab":
				return m.switchTo((m.active - 1 + len(m.views)) % len(m.views))
			}
			updated, cmd := m.views[m.active].Update(msg)
			m.views[m.active] = updated
			return m, cmd
		}
		switch msg.String() {
		case ":":
			m.paletteOpen = true
			m.pal = newPalette(m.entries)
			return m, nil
		case ">":
			m.askOpen = true
			m.ask = newAskBar(m.gw, m.session, m.entries)
			return m, nil
		case "q", "ctrl+c":
			m.quitting = true
			return m, tea.Quit
		case "tab", "right":
			return m.switchTo((m.active + 1) % len(m.views))
		case "shift+tab", "left":
			return m.switchTo((m.active - 1 + len(m.views)) % len(m.views))
		case "1", "2", "3", "4", "5", "6", "7", "8", "9":
			if i := int(msg.String()[0] - '1'); i < len(m.views) {
				return m.switchTo(i)
			}
			return m, nil
		}
	}
	updated, cmd := m.views[m.active].Update(msg)
	m.views[m.active] = updated
	return m, cmd
}

// switchTo activates view i and (re)starts its data fetch. Leaving a view that
// holds a background stream tears it down first, so a mid-stream tab-switch
// never leaks the goroutine + upstream connection.
func (m Model) switchTo(i int) (tea.Model, tea.Cmd) {
	if i != m.active {
		if l, ok := m.views[m.active].(leaver); ok {
			l.leave()
		}
	}
	m.active = i
	return m, m.views[i].Init()
}

// navigateTo handles an Ask-Nexus navigate intent: resolve the view name, switch
// to it (tearing down a streaming view first), and apply a Radar filter when one
// rode along. An unknown view name is a no-op (stay put).
func (m Model) navigateTo(msg navigateMsg) (tea.Model, tea.Cmd) {
	idx := resolveViewIndex(m.entries, msg.view)
	if idx < 0 {
		return m, nil
	}
	if l, ok := m.views[m.active].(leaver); ok && idx != m.active {
		l.leave()
	}
	m.active = idx
	if r, ok := m.views[idx].(*radar); ok && msg.filter != nil {
		r.applyFilter(filterFromIntent(msg.filter, time.Now()))
	}
	return m, m.views[idx].Init()
}

func (m Model) eventIndex() int {
	for i, t := range m.tabs {
		if t == "Event" {
			return i
		}
	}
	return 0
}

// View composes the status bar, tab row, active view, and help bar.
func (m Model) View() string {
	if m.quitting {
		return ""
	}
	w := m.width
	if w == 0 {
		w = 100
	}
	bar := m.statusBar(w)
	tabs := m.tabRow()
	footer := m.footerBar(w)
	contentH := m.height - lipgloss.Height(bar) - lipgloss.Height(tabs) - lipgloss.Height(footer) - 1
	if contentH < 3 {
		contentH = 3
	}
	body := m.views[m.active].View(w, contentH)
	return strings.Join([]string{bar, tabs, body, footer}, "\n")
}

// footerBar composes the bottom line: the keybar on the left and a profile +
// address indicator on the right, so the operator can always confirm which
// deployment they are acting on. When the command palette is open it owns the
// whole line. If the line is too narrow for both, the keybar wins.
func (m Model) footerBar(width int) string {
	if m.paletteOpen {
		return m.pal.View()
	}
	if m.askOpen {
		return m.ask.View()
	}
	help := styles.HelpBar.Render(m.helpText())
	loc := m.locationIndicator()
	if loc == "" {
		return help
	}
	gap := width - lipgloss.Width(help) - lipgloss.Width(loc)
	if gap < 1 {
		return help
	}
	return help + strings.Repeat(" ", gap) + loc
}

// locationIndicator is the bottom-right "which deployment am I on" badge: the
// active profile name and its (scheme-stripped, truncated) Control Plane
// address. Prod is reddened to reinforce the top banner.
func (m Model) locationIndicator() string {
	if m.session.EnvName == "" {
		return ""
	}
	label := m.session.EnvName
	if addr := stripScheme(m.session.Addr); addr != "" {
		label += " · " + clip(addr, 32)
	}
	style := styles.HelpBar
	if m.session.IsProd {
		style = lipgloss.NewStyle().Foreground(styles.Red).Bold(true)
	}
	return style.Render(label)
}

// stripScheme drops a leading http(s):// so the address reads compactly.
func stripScheme(u string) string {
	u = strings.TrimPrefix(u, "https://")
	return strings.TrimPrefix(u, "http://")
}

// helpText is the bottom keybar; it defers to a view that supplies its own.
func (m Model) helpText() string {
	if h, ok := m.views[m.active].(helpProvider); ok {
		return h.help()
	}
	return "tab/1-9 switch · : palette · > ask · / filter · ↑/↓ move · enter open · q quit"
}

// statusBar renders the env + model/VK indicator; prod shows a red banner.
func (m Model) statusBar(width int) string {
	sel := ""
	if m.session.Model != "" {
		sel = " · " + m.session.Model
		if m.session.VKName != "" {
			sel += " · vk:" + m.session.VKName
		}
	}
	if m.session.IsProd {
		return styles.ProdBanner.Width(width).Render(
			fmt.Sprintf("⚠ PROD %s — mutations require confirmation%s", m.session.EnvName, sel))
	}
	return styles.StatusBar.Width(width).Render("nexus · ENV " + m.session.EnvName + sel)
}

// tabRow renders the tab labels with the active one highlighted.
func (m Model) tabRow() string {
	parts := make([]string, len(m.tabs))
	for i, t := range m.tabs {
		if i == m.active {
			parts[i] = styles.ActiveTab.Render(t)
		} else {
			parts[i] = styles.Tab.Render(t)
		}
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, parts...)
}

// textCapturer is implemented by views that capture raw keystrokes (text input)
// so the root model can suspend its single-letter shortcuts while typing.
type textCapturer interface{ capturing() bool }

// helpProvider lets a view supply its own bottom keybar text.
type helpProvider interface{ help() string }

// leaver is implemented by views that hold a background stream/connection so the
// shell can tear it down when the operator navigates away. stream.go's stop()
// is the teardown; without this hook a mid-stream tab-switch would leak.
type leaver interface{ leave() }

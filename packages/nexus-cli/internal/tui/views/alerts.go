package views

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"image/color"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/kit"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/styles"
)

// alertsView lists the currently-firing alerts with a row cursor; enter opens a
// detail drawer of the selected alert.
type alertsView struct {
	gw      kit.Gateway
	res     *core.AlertsResult
	cursor  int
	detail  bool
	err     error
	loading bool
}

type alertsMsg struct {
	res *core.AlertsResult
	err error
}
type alertsTick struct{}

func newAlerts(gw kit.Gateway) *alertsView { return &alertsView{gw: gw, loading: true} }

func (a *alertsView) Init() tea.Cmd { return a.fetch() }

func (a *alertsView) fetch() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := kit.FetchCtx()
		defer cancel()
		res, err := a.gw.Alerts(ctx)
		return alertsMsg{res: res, err: err}
	}
}

func (a *alertsView) Update(msg tea.Msg) (kit.ViewModel, tea.Cmd) {
	switch msg := msg.(type) {
	case alertsMsg:
		a.loading = false
		a.err = msg.err
		if msg.res != nil {
			a.res = msg.res
		}
		a.clampCursor()
		return a, kit.Tick(kit.PollSlow, alertsTick{})
	case alertsTick:
		return a, a.fetch()
	case tea.KeyPressMsg:
		if a.detail {
			return a, nil // esc (→ Back()) is the only key out of the drawer
		}
		switch msg.String() {
		case "up", "k":
			if a.cursor > 0 {
				a.cursor--
			}
		case "down", "j":
			if a.cursor < len(a.firing())-1 {
				a.cursor++
			}
		case "enter":
			if _, ok := a.selected(); ok {
				a.detail = true
			}
		}
	}
	return a, nil
}

// back closes the detail drawer so esc returns to the list before the root pops
// the nav stack.
func (a *alertsView) Back() bool {
	if a.detail {
		a.detail = false
		return true
	}
	return false
}

func (a *alertsView) selected() (core.Alert, bool) {
	f := a.firing()
	if a.cursor < 0 || a.cursor >= len(f) {
		return core.Alert{}, false
	}
	return f[a.cursor], true
}

func (a *alertsView) clampCursor() {
	if n := len(a.firing()); a.cursor >= n {
		a.cursor = n - 1
	}
	if a.cursor < 0 {
		a.cursor = 0
	}
}

func (a *alertsView) Help() string {
	if a.detail {
		return "←/esc back · q quit"
	}
	return "↑/↓ select · enter open · ←/esc back · q quit"
}

func (a *alertsView) View(width, height int) string {
	if a.loading && a.res == nil {
		return styles.TileLabel.Render("loading alerts…")
	}
	if a.detail {
		return a.detailView(width)
	}
	var b strings.Builder
	b.WriteString(styles.TileValue.Render("Firing alerts"))
	if a.err != nil {
		b.WriteString("  " + lipgloss.NewStyle().Foreground(styles.Red).Render("⚠ "+a.err.Error()))
	}
	b.WriteString("\n")
	firing := a.firing()
	if len(firing) == 0 {
		b.WriteString(styles.TileLabel.Render("  no firing alerts"))
		return b.String()
	}
	b.WriteString(lipgloss.NewStyle().Foreground(styles.Sub).Render(
		fmt.Sprintf("  %-10s %-22s %5s  %s", "SEVERITY", "TARGET", "DUP", "MESSAGE")))
	b.WriteString("\n")
	var rows []string
	for i, al := range firing {
		sev := lipgloss.NewStyle().Bold(true).Foreground(severityColor(al.Severity)).Render(fmt.Sprintf("%-10s", al.Severity))
		cursor := "  "
		line := fmt.Sprintf("%s %-22s %5d  %s", sev, kit.Clip(al.TargetLabel, 22), al.DuplicateCount, al.Message)
		if i == a.cursor {
			cursor = lipgloss.NewStyle().Foreground(styles.Brand).Render("▸ ")
			line = lipgloss.NewStyle().Bold(true).Render(line)
		}
		rows = append(rows, cursor+line)
	}
	b.WriteString(strings.Join(rows, "\n"))
	return b.String()
}

// detailView renders the full record of the selected alert.
func (a *alertsView) detailView(width int) string {
	al, ok := a.selected()
	if !ok {
		return styles.TileLabel.Render("(no alert selected)")
	}
	var b strings.Builder
	b.WriteString(styles.TileValue.Render("Alert · " + al.TargetLabel))
	b.WriteString("\n\n")
	sev := lipgloss.NewStyle().Bold(true).Foreground(severityColor(al.Severity)).Render(al.Severity)
	b.WriteString(kit.DetailRow("Severity", sev) + "\n")
	b.WriteString(kit.DetailRow("State", kit.Dash(al.State)) + "\n")
	b.WriteString(kit.DetailRow("Fired at", kit.Dash(al.FiredAt)) + "\n")
	b.WriteString(kit.DetailRow("Duplicates", fmt.Sprintf("%d", al.DuplicateCount)) + "\n")
	b.WriteString(kit.DetailRow("Alert id", kit.Dash(al.ID)) + "\n\n")
	b.WriteString(styles.TileLabel.Render("  Message"))
	b.WriteString("\n")
	b.WriteString(kit.WrapText("  "+al.Message, width-2))
	return b.String()
}

// firing returns only the active alerts.
func (a *alertsView) firing() []core.Alert {
	if a.res == nil {
		return nil
	}
	out := make([]core.Alert, 0, len(a.res.Alerts))
	for _, al := range a.res.Alerts {
		if al.Firing() {
			out = append(out, al)
		}
	}
	return out
}

// severityColor maps an alert severity to a RAG color.
func severityColor(sev string) color.Color {
	switch strings.ToLower(sev) {
	case "critical", "error", "high":
		return styles.Red
	case "warning", "warn", "medium":
		return styles.Amber
	default:
		return styles.Brand
	}
}

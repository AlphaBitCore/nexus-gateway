package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/styles"
)

// alertsView lists the currently-firing alerts.
type alertsView struct {
	gw      Gateway
	res     *core.AlertsResult
	err     error
	loading bool
}

type alertsMsg struct {
	res *core.AlertsResult
	err error
}
type alertsTick struct{}

func newAlerts(gw Gateway) *alertsView { return &alertsView{gw: gw, loading: true} }

func (a *alertsView) Init() tea.Cmd { return a.fetch() }

func (a *alertsView) fetch() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := fetchCtx()
		defer cancel()
		res, err := a.gw.Alerts(ctx)
		return alertsMsg{res: res, err: err}
	}
}

func (a *alertsView) Update(msg tea.Msg) (viewModel, tea.Cmd) {
	switch msg := msg.(type) {
	case alertsMsg:
		a.loading = false
		a.err = msg.err
		if msg.res != nil {
			a.res = msg.res
		}
		return a, tick(pollSlow, alertsTick{})
	case alertsTick:
		return a, a.fetch()
	}
	return a, nil
}

func (a *alertsView) View(width, height int) string {
	if a.loading && a.res == nil {
		return styles.TileLabel.Render("loading alerts…")
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
	for _, al := range firing {
		sev := lipgloss.NewStyle().Bold(true).Foreground(severityColor(al.Severity)).Render(fmt.Sprintf("%-10s", al.Severity))
		target := al.TargetLabel
		if len(target) > 22 {
			target = target[:21] + "…"
		}
		rows = append(rows, fmt.Sprintf("  %s %-22s %5d  %s", sev, target, al.DuplicateCount, al.Message))
	}
	b.WriteString(strings.Join(rows, "\n"))
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
func severityColor(sev string) lipgloss.Color {
	switch strings.ToLower(sev) {
	case "critical", "error", "high":
		return styles.Red
	case "warning", "warn", "medium":
		return styles.Amber
	default:
		return styles.Brand
	}
}

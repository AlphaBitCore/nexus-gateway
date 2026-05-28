package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/styles"
)

// radar is the Live Traffic Radar: a polled, navigable list of recent events.
type radar struct {
	gw         Gateway
	list       *core.TrafficList
	err        error
	cursor     int
	errorsOnly bool
	base       core.TrafficFilter // externally-applied filter (Ask-Nexus navigate); zero value = none
	loading    bool
}

type radarMsg struct {
	list *core.TrafficList
	err  error
}
type radarTick struct{}

func newRadar(gw Gateway) *radar { return &radar{gw: gw, loading: true} }

func (r *radar) Init() tea.Cmd { return r.fetch() }

func (r *radar) fetch() tea.Cmd {
	f := r.base
	if f.Limit == 0 {
		f.Limit = 20
	}
	if r.errorsOnly {
		f.StatusRange = "error"
	}
	return func() tea.Msg {
		ctx, cancel := fetchCtx()
		defer cancel()
		list, err := r.gw.TrafficList(ctx, f)
		return radarMsg{list: list, err: err}
	}
}

// applyFilter sets an externally-supplied traffic filter (from the Ask-Nexus
// navigate intent) as the base of the next poll and resets the cursor. errorsOnly
// is synced so the header reflects an error-only navigate.
func (r *radar) applyFilter(f core.TrafficFilter) {
	if f.Limit == 0 {
		f.Limit = 20
	}
	r.base = f
	r.cursor = 0
	r.errorsOnly = strings.EqualFold(f.StatusRange, "error")
}

func (r *radar) Update(msg tea.Msg) (viewModel, tea.Cmd) {
	switch msg := msg.(type) {
	case radarMsg:
		r.loading = false
		r.err = msg.err
		if msg.list != nil {
			r.list = msg.list
		}
		r.clampCursor()
		return r, tick(pollFast, radarTick{})
	case radarTick:
		return r, r.fetch()
	case tea.KeyMsg:
		switch msg.String() {
		case "up", "k":
			if r.cursor > 0 {
				r.cursor--
			}
		case "down", "j":
			if r.cursor < r.rowCount()-1 {
				r.cursor++
			}
		case "f":
			r.errorsOnly = !r.errorsOnly
			r.cursor = 0
			return r, r.fetch()
		case "enter":
			if id := r.selectedID(); id != "" {
				return r, func() tea.Msg { return openEventMsg{id: id} }
			}
		}
	}
	return r, nil
}

func (r *radar) rowCount() int {
	if r.list == nil {
		return 0
	}
	return len(r.list.Data)
}

func (r *radar) clampCursor() {
	if r.cursor >= r.rowCount() {
		r.cursor = r.rowCount() - 1
	}
	if r.cursor < 0 {
		r.cursor = 0
	}
}

func (r *radar) selectedID() string {
	if r.cursor < 0 || r.cursor >= r.rowCount() {
		return ""
	}
	return r.list.Data[r.cursor].ID
}

func (r *radar) View(width, height int) string {
	filter := "all"
	if r.errorsOnly {
		filter = "errors only"
	} else if r.base.StatusRange != "" {
		filter = r.base.StatusRange
	}
	if r.base.Provider != "" {
		filter += " · provider=" + r.base.Provider
	}
	if !r.base.StartTime.IsZero() {
		filter += " · windowed"
	}
	var windowCost float64
	if r.list != nil {
		for _, e := range r.list.Data {
			windowCost += e.EstCostUSD
		}
	}
	header := styles.TileLabel.Render(fmt.Sprintf("Live traffic · %s · f=toggle filter", filter)) +
		"  " + lipgloss.NewStyle().Bold(true).Foreground(styles.BrandHi).Render(fmt.Sprintf("$%.5f", windowCost))
	if r.loading && r.list == nil {
		return header + "\n" + styles.TileLabel.Render("loading…")
	}
	var b strings.Builder
	b.WriteString(header)
	if r.err != nil {
		b.WriteString("  " + lipgloss.NewStyle().Foreground(styles.Red).Render("⚠ "+r.err.Error()))
	}
	b.WriteString("\n")
	b.WriteString(lipgloss.NewStyle().Foreground(styles.Sub).Render(
		fmt.Sprintf("  %-8s %-6s %-22s %8s %10s", "TIME", "STATUS", "MODEL", "TOKENS", "COST")))
	b.WriteString("\n")
	if r.rowCount() == 0 {
		b.WriteString(styles.TileLabel.Render("  (no events)"))
		return b.String()
	}
	for i, e := range r.list.Data {
		b.WriteString(r.row(i, e))
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func (r *radar) row(i int, e core.TrafficEvent) string {
	cursor := "  "
	if i == r.cursor {
		cursor = lipgloss.NewStyle().Foreground(styles.Brand).Render("▸ ")
	}
	status := lipgloss.NewStyle().Foreground(styles.StatusColor(e.StatusCode)).Render(fmt.Sprintf("%-6d", e.StatusCode))
	model := e.ModelName
	if len(model) > 22 {
		model = model[:21] + "…"
	}
	line := fmt.Sprintf("%-8s %s %-22s %8d %10.5f",
		e.Timestamp.Format("15:04:05"), status, model, e.TotalTokens, e.EstCostUSD)
	if i == r.cursor {
		line = lipgloss.NewStyle().Bold(true).Render(line)
	}
	if badges := eventBadges(e); badges != "" {
		line += "  " + badges
	}
	return cursor + line
}

// eventBadges renders the "fireworks" badges for a traffic row: a flashing
// BLOCKED (hook blocked), PII (hook redacted), and a cache HIT.
func eventBadges(e core.TrafficEvent) string {
	var parts []string
	// Worst outcome across both hook stages wins the badge.
	switch {
	case hookOutcome(e.RequestHookDecision) == "block" || hookOutcome(e.ResponseHookDecision) == "block":
		parts = append(parts, lipgloss.NewStyle().Bold(true).Foreground(styles.Red).Render("BLOCKED"))
	case hookOutcome(e.RequestHookDecision) == "redact" || hookOutcome(e.ResponseHookDecision) == "redact":
		parts = append(parts, lipgloss.NewStyle().Bold(true).Foreground(styles.Amber).Render("PII"))
	}
	if cacheHit(e) {
		parts = append(parts, lipgloss.NewStyle().Foreground(styles.Green).Render("HIT"))
	}
	return strings.Join(parts, " ")
}

// cacheHit reports whether either cache layer served this event.
func cacheHit(e core.TrafficEvent) bool {
	return strings.EqualFold(e.CacheStatus, "hit") || strings.EqualFold(e.GatewayCache, "hit")
}

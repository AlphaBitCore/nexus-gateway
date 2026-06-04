package views

import (
	"fmt"
	"sort"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"image/color"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/kit"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/styles"
)

const (
	waterfallRows  = 8  // recent traffic rows shown in the live waterfall
	cardTrendWidth = 16 // braille width of a hero card's trend
	leaderBarWidth = 16 // block-bar width of a provider leaderboard row
	leaderRows     = 5  // top providers shown
)

// cockpit is the Mission Control landing view (design §2): a live, animated
// dashboard — hero metric cards (value + ▲▼ delta + braille trend), a traffic
// waterfall, a provider leaderboard, and system status lights. It replaces the
// old Overview as view index 0.
type cockpit struct {
	gw      kit.Gateway
	sp      *core.SparklineResult
	inst    *core.InstancesResult
	traffic *core.TrafficList
	prov    *core.ByProviderResult
	alerts  *core.AlertsResult
	kill    *core.KillSwitchState
	pass    *core.PassthroughSnapshot
	err     error
	loading bool
	pulse   int // animation phase; advanced by cockpitPulse, never refetches
}

// cockpitData carries one poll's fetch results. Each source is independent: a
// failed source leaves the prior value in place so a single flaky endpoint never
// blanks the whole cockpit (design: "alive on first glance").
type cockpitData struct {
	sp      *core.SparklineResult
	inst    *core.InstancesResult
	traffic *core.TrafficList
	prov    *core.ByProviderResult
	alerts  *core.AlertsResult
	kill    *core.KillSwitchState
	pass    *core.PassthroughSnapshot
	err     error
}

type cockpitTick struct{}  // poll → refetch
type cockpitPulse struct{} // animation → advance the pulse phase only

func newCockpit(gw kit.Gateway) *cockpit { return &cockpit{gw: gw, loading: true} }

// Init starts the first fetch and the animation clock.
func (c *cockpit) Init() tea.Cmd {
	return tea.Batch(c.fetch(), kit.Tick(kit.AnimInterval, cockpitPulse{}))
}

func (c *cockpit) Help() string {
	return "tab chat · / commands · ↑/↓ move · enter open · ←/esc back · q quit"
}

// fetch gathers every cockpit source. Each call is independent: an error on one
// is recorded (first wins) but the others still populate, so one flaky endpoint
// never blanks the cockpit.
func (c *cockpit) fetch() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := kit.FetchCtx()
		defer cancel()
		var d cockpitData
		note := func(err error) {
			if err != nil && d.err == nil {
				d.err = err
			}
		}
		var err error
		d.sp, err = c.gw.Sparkline(ctx, nil)
		note(err)
		d.inst, err = c.gw.Instances(ctx)
		note(err)
		d.traffic, err = c.gw.TrafficList(ctx, core.TrafficFilter{Limit: waterfallRows})
		note(err)
		d.prov, err = c.gw.ByProvider(ctx, nil)
		note(err)
		d.alerts, err = c.gw.Alerts(ctx)
		note(err)
		d.kill, err = c.gw.KillSwitchStatus(ctx)
		note(err)
		d.pass, err = c.gw.PassthroughSnapshot(ctx)
		note(err)
		return d
	}
}

func (c *cockpit) Update(msg tea.Msg) (kit.ViewModel, tea.Cmd) {
	switch msg := msg.(type) {
	case cockpitData:
		c.loading = false
		c.err = msg.err
		if msg.sp != nil {
			c.sp = msg.sp
		}
		if msg.inst != nil {
			c.inst = msg.inst
		}
		if msg.traffic != nil {
			c.traffic = msg.traffic
		}
		if msg.prov != nil {
			c.prov = msg.prov
		}
		if msg.alerts != nil {
			c.alerts = msg.alerts
		}
		if msg.kill != nil {
			c.kill = msg.kill
		}
		if msg.pass != nil {
			c.pass = msg.pass
		}
		return c, kit.Tick(kit.PollSlow, cockpitTick{})
	case cockpitTick:
		return c, c.fetch()
	case cockpitPulse:
		c.pulse++
		return c, kit.Tick(kit.AnimInterval, cockpitPulse{})
	}
	return c, nil
}

func (c *cockpit) View(width, height int) string {
	if c.loading && c.sp == nil {
		return styles.TileLabel.Render("loading mission control…")
	}
	if width < 1 {
		width = kit.DefaultViewWidth
	}
	var b strings.Builder
	if c.err != nil {
		b.WriteString(lipgloss.NewStyle().Bold(true).Foreground(styles.Red).Render("⚠ " + c.err.Error()))
		b.WriteString("  ")
		b.WriteString(styles.TileLabel.Render("(showing last-good data)"))
		b.WriteString("\n")
	}
	b.WriteString(c.heroRow())
	b.WriteString("\n")
	b.WriteString(c.body(width, height))
	return b.String()
}

// heroRow renders the three big-number cards from the window totals.
func (c *cockpit) heroRow() string {
	totals := map[string]float64{}
	if c.sp != nil {
		totals = c.sp.Totals()
	}
	errs := totals[core.MetricStatus4xxCount] + totals[core.MetricStatus5xxCount]
	cards := []string{
		c.heroCard("Requests", fmt.Sprintf("%.0f", totals[core.MetricRequestCount]), core.MetricRequestCount, false),
		c.heroCard("Cost USD", fmt.Sprintf("$%.4f", totals[core.MetricEstimatedCostUSD]), core.MetricEstimatedCostUSD, false),
		c.heroCard("Errors", fmt.Sprintf("%.0f", errs), core.MetricStatus5xxCount, true),
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, cards...)
}

// heroCard renders one metric card: label, big value with a trend-delta arrow,
// and a small braille trend of the metric. higherIsWorse flips the delta color
// for "bad-when-rising" metrics (errors): a rising error count reads red.
func (c *cockpit) heroCard(label, value, metricKey string, higherIsWorse bool) string {
	delta := 0.0
	trend := ""
	if c.sp != nil {
		delta = deltaFor(c.sp.Series, metricKey)
		trend = kit.SparklineChart(c.sp.Series, metricKey, cardTrendWidth, 1)
	}
	glyph, col := deltaGlyph(delta, higherIsWorse)
	inner := styles.TileLabel.Render(label) + "\n" +
		styles.TileValue.Render(value) + " " + lipgloss.NewStyle().Foreground(col).Render(glyph)
	if trend != "" {
		inner += "\n" + lipgloss.NewStyle().Foreground(styles.Sub).Render(trend)
	}
	return styles.Tile.Render(inner)
}

// body lays the live traffic waterfall on the left and the provider leaderboard
// plus status lights on the right.
func (c *cockpit) body(width, height int) string {
	rows := height - 7
	if rows < 4 {
		rows = 4
	}
	left := c.waterfallPanel(rows)
	right := c.leaderboard() + "\n\n" + c.statusLights()
	_ = width
	return lipgloss.JoinHorizontal(lipgloss.Top, left, "    ", right)
}

// waterfallPanel renders the most recent traffic rows with a RAG status code and
// the shared event badges (⚡ HIT / BLOCKED / PII — empty hook decisions on a
// list row simply render no badge).
func (c *cockpit) waterfallPanel(rows int) string {
	var b strings.Builder
	b.WriteString(styles.TileValue.Render("Live traffic"))
	b.WriteString("\n")
	if c.traffic == nil || len(c.traffic.Data) == 0 {
		b.WriteString(styles.TileLabel.Render("(no recent traffic)"))
		return b.String()
	}
	n := len(c.traffic.Data)
	if n > rows {
		n = rows
	}
	for _, e := range c.traffic.Data[:n] {
		status := lipgloss.NewStyle().Foreground(styles.StatusColor(e.StatusCode)).Render(fmt.Sprintf("%-4d", e.StatusCode))
		line := fmt.Sprintf("%s %s %-18s", e.Timestamp.Format("15:04:05"), status, kit.Clip(e.ModelName, 18))
		if badges := eventBadges(e); badges != "" {
			line += "  " + badges
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// leaderboard ranks providers by request volume with a block bar; the friendly
// provider label is shown, never the bare id ([[feedback_surface_human_friendly_not_bare_id]]).
func (c *cockpit) leaderboard() string {
	var b strings.Builder
	b.WriteString(styles.TileValue.Render("Providers"))
	b.WriteString("\n")
	if c.prov == nil || len(c.prov.Data) == 0 {
		b.WriteString(styles.TileLabel.Render("(no provider traffic)"))
		return b.String()
	}
	rows := append([]core.ProviderUsageRow(nil), c.prov.Data...)
	sort.Slice(rows, func(i, j int) bool { return rows[i].RequestCount > rows[j].RequestCount })
	if len(rows) > leaderRows {
		rows = rows[:leaderRows]
	}
	max := rows[0].RequestCount
	if max < 1 {
		max = 1
	}
	for _, r := range rows {
		label := r.ProviderLabel
		if label == "" {
			label = r.Provider
		}
		filled := r.RequestCount * leaderBarWidth / max
		bar := lipgloss.NewStyle().Foreground(styles.Brand).Render(strings.Repeat("█", filled)) +
			lipgloss.NewStyle().Foreground(styles.Line).Render(strings.Repeat("░", leaderBarWidth-filled))
		b.WriteString(fmt.Sprintf("%-14s %s %d\n", kit.Clip(label, 14), bar, r.RequestCount))
	}
	return strings.TrimRight(b.String(), "\n")
}

// statusLights renders the system health lights: nodes/services, firing alerts,
// the kill-switch, and emergency passthrough. Problem states pulse (the dot
// alternates ●/○ each animation frame) so an active alert visibly blinks.
func (c *cockpit) statusLights() string {
	var b strings.Builder
	b.WriteString(styles.TileValue.Render("Status"))
	b.WriteString("\n")
	if c.inst != nil {
		b.WriteString(c.light(false, styles.Green, fmt.Sprintf("%d nodes · %d services", c.inst.Count, len(c.inst.Services))))
		b.WriteString("\n")
	}
	firing := 0
	if c.alerts != nil {
		for _, a := range c.alerts.Alerts {
			if a.Firing() {
				firing++
			}
		}
	}
	if firing > 0 {
		b.WriteString(c.light(true, styles.Red, fmt.Sprintf("%d alerts firing", firing)))
	} else {
		b.WriteString(c.light(false, styles.Green, "no alerts firing"))
	}
	b.WriteString("\n")
	if c.kill != nil && c.kill.Engaged {
		b.WriteString(c.light(true, styles.Red, "kill-switch ENGAGED"))
	} else {
		b.WriteString(c.light(false, styles.Green, "kill-switch armed"))
	}
	b.WriteString("\n")
	if c.pass != nil {
		ad, pr := c.pass.ActiveOverrides()
		switch {
		case c.pass.Global.Enabled:
			b.WriteString(c.light(true, styles.Amber, "passthrough GLOBAL active"))
		case ad+pr > 0:
			b.WriteString(c.light(true, styles.Amber, fmt.Sprintf("passthrough overrides: %d", ad+pr)))
		default:
			b.WriteString(c.light(false, styles.Green, "passthrough off"))
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// light renders one status dot + label. When pulsing (a problem state) the dot
// hollows out on odd animation frames so the line visibly blinks.
func (c *cockpit) light(pulsing bool, col color.Color, label string) string {
	dot := "●"
	if pulsing && c.pulse%2 == 1 {
		dot = "○"
	}
	return lipgloss.NewStyle().Foreground(col).Render(dot) + " " + styles.TileLabel.Render(label)
}

// deltaFor is the change in a metric across the last two buckets (last − prev).
// Fewer than two buckets is a flat zero.
func deltaFor(series []core.SparklineBucket, key string) float64 {
	if len(series) < 2 {
		return 0
	}
	return series[len(series)-1].Values[key] - series[len(series)-2].Values[key]
}

// deltaGlyph maps a delta to a trend arrow + color. For most metrics a rise is
// good (green ▲); higherIsWorse flips that (errors rising is red ▲). A flat
// delta is a muted dot.
func deltaGlyph(delta float64, higherIsWorse bool) (string, color.Color) {
	switch {
	case delta > 0:
		if higherIsWorse {
			return "▲", styles.Red
		}
		return "▲", styles.Green
	case delta < 0:
		if higherIsWorse {
			return "▼", styles.Green
		}
		return "▼", styles.Red
	default:
		return "·", styles.Sub
	}
}

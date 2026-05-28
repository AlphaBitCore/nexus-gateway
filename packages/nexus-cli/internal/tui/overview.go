package tui

import (
	"fmt"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/styles"
)

// overview is the Health Overview: big-number tiles, a braille trend chart,
// service/node health, and a backlog (DLQ) signal.
type overview struct {
	gw          Gateway
	sp          *core.SparklineResult
	inst        *core.InstancesResult
	dlq         *core.DLQResult
	err         error
	loading     bool
	chartMetric int // index into chartMetrics; c cycles it
}

type overviewMsg struct {
	sp   *core.SparklineResult
	inst *core.InstancesResult
	dlq  *core.DLQResult
	err  error
}
type overviewTick struct{}

func newOverview(gw Gateway) *overview { return &overview{gw: gw, loading: true} }

func (o *overview) Init() tea.Cmd { return o.fetch() }

func (o *overview) help() string {
	return "c cycle chart (" + chartMetrics[o.chartMetric].label + ") · : palette · tab switch · q quit"
}

func (o *overview) fetch() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := fetchCtx()
		defer cancel()
		sp, err := o.gw.Sparkline(ctx, nil)
		if err != nil {
			return overviewMsg{err: err}
		}
		inst, err := o.gw.Instances(ctx)
		if err != nil {
			return overviewMsg{sp: sp, err: err}
		}
		dlq, err := o.gw.DLQ(ctx)
		return overviewMsg{sp: sp, inst: inst, dlq: dlq, err: err}
	}
}

func (o *overview) Update(msg tea.Msg) (viewModel, tea.Cmd) {
	switch msg := msg.(type) {
	case overviewMsg:
		o.loading = false
		o.err = msg.err
		if msg.sp != nil {
			o.sp = msg.sp
		}
		if msg.inst != nil {
			o.inst = msg.inst
		}
		if msg.dlq != nil {
			o.dlq = msg.dlq
		}
		return o, tick(pollSlow, overviewTick{})
	case overviewTick:
		return o, o.fetch()
	case tea.KeyMsg:
		if msg.String() == "c" {
			o.chartMetric = (o.chartMetric + 1) % len(chartMetrics)
		}
	}
	return o, nil
}

func (o *overview) View(width, height int) string {
	if o.loading && o.sp == nil {
		return styles.TileLabel.Render("loading health…")
	}
	var b strings.Builder
	if o.err != nil {
		b.WriteString(styles.TileValue.Foreground(styles.Red).Render("⚠ " + o.err.Error()))
		b.WriteString("\n")
		if o.sp == nil {
			return b.String()
		}
		b.WriteString(styles.TileLabel.Render("(showing last-good data)\n"))
	}
	b.WriteString(o.tilesRow())
	b.WriteString("\n")
	b.WriteString(o.backlogRow())
	b.WriteString("\n")
	b.WriteString(o.chartPanel(width, height))
	b.WriteString("\n")
	b.WriteString(o.servicesPanel())
	return b.String()
}

// backlogRow shows the dead-letter backlog depth (a non-zero value is a signal
// that downstream consumers are falling behind).
func (o *overview) backlogRow() string {
	depth := 0
	if o.dlq != nil {
		depth = len(o.dlq.Rows)
	}
	c := styles.Green
	if depth > 0 {
		c = styles.Red
	}
	badge := lipgloss.NewStyle().Bold(true).Foreground(c).Render(fmt.Sprintf("%d", depth))
	return styles.TileLabel.Render("DLQ backlog ") + badge
}

// chartPanel renders the selected metric's braille trend over the window.
func (o *overview) chartPanel(width, height int) string {
	if o.sp == nil {
		return ""
	}
	m := chartMetrics[o.chartMetric]
	w := width - 4
	if w > 60 {
		w = 60
	}
	h := 5
	if height < 16 {
		h = 3
	}
	chart := sparklineChart(o.sp.Series, m.key, w, h)
	if chart == "" {
		return styles.TileLabel.Render("trend: (no series)")
	}
	return styles.TileLabel.Render("trend · "+m.label+" (c to cycle)") + "\n" + chart
}

// Health-tile metric keys (the snake_case names the analytics series uses).
const (
	mRequests  = core.MetricRequestCount
	mCostUSD   = core.MetricEstimatedCostUSD
	mTokens    = core.MetricTotalTokens
	mCacheHits = core.MetricCacheHitCount
	m4xx       = core.MetricStatus4xxCount
	m5xx       = core.MetricStatus5xxCount
)

func (o *overview) tilesRow() string {
	s := map[string]float64{}
	if o.sp != nil {
		s = o.sp.Totals()
	}
	errors := s[m4xx] + s[m5xx]
	tiles := []string{
		tile("Requests", fmt.Sprintf("%.0f", s[mRequests])),
		tile("Cost USD", fmt.Sprintf("$%.4f", s[mCostUSD])),
		tile("Tokens", fmt.Sprintf("%.0f", s[mTokens])),
		tile("Cache hits", fmt.Sprintf("%.0f", s[mCacheHits])),
		tile("Errors", fmt.Sprintf("%.0f", errors)),
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, tiles...)
}

// tile renders one bordered metric card.
func tile(label, value string) string {
	inner := styles.TileLabel.Render(label) + "\n" + styles.TileValue.Render(value)
	return styles.Tile.Render(inner)
}

func (o *overview) servicesPanel() string {
	if o.inst == nil || len(o.inst.Services) == 0 {
		return styles.TileLabel.Render("services: (none reported)")
	}
	names := make([]string, 0, len(o.inst.Services))
	for n := range o.inst.Services {
		names = append(names, n)
	}
	sort.Strings(names)
	var rows []string
	rows = append(rows, styles.TileValue.Render(fmt.Sprintf("Services (%d nodes)", o.inst.Count)))
	for _, n := range names {
		dot := lipgloss.NewStyle().Foreground(styles.Green).Render("●")
		rows = append(rows, fmt.Sprintf("%s %-18s %d", dot, n, o.inst.Services[n].Total))
	}
	return strings.Join(rows, "\n")
}

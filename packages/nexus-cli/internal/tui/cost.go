package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/styles"
)

// cost is the Cost view: per-provider spend (with latency), cache ROI, and a
// burn-rate + month projection.
type cost struct {
	gw      Gateway
	byProv  *core.ByProviderResult
	roi     *core.CacheROIResult
	err     error
	loading bool

	cf        confirm // prod-gated cache flush
	flushing  bool
	flushNote string
	flushErr  error
}

type costMsg struct {
	byProv *core.ByProviderResult
	roi    *core.CacheROIResult
	err    error
}
type costTick struct{}
type cacheFlushMsg struct{ err error }

// newCost builds the Cost view. The session (optional; the dashboard always
// passes it) drives the prod confirmation on the cache-flush write.
func newCost(gw Gateway, s ...Session) *cost {
	return &cost{gw: gw, loading: true, cf: newConfirm(optSession(s))}
}

func (c *cost) Init() tea.Cmd { return c.fetch() }

// capturing suspends the root's single-letter shortcuts while the prod
// confirmation field is focused.
func (c *cost) capturing() bool { return c.cf.capturing() }

func (c *cost) help() string {
	if c.cf.capturing() {
		return c.cf.helpHint()
	}
	return "f flush gateway cache · tab/1-9 switch · : palette · q quit"
}

func (c *cost) fetch() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := fetchCtx()
		defer cancel()
		bp, err := c.gw.ByProvider(ctx, nil)
		if err != nil {
			return costMsg{err: err}
		}
		roi, err := c.gw.CacheROI(ctx, nil)
		return costMsg{byProv: bp, roi: roi, err: err}
	}
}

func (c *cost) Update(msg tea.Msg) (viewModel, tea.Cmd) {
	switch msg := msg.(type) {
	case costMsg:
		c.loading = false
		c.err = msg.err
		if msg.byProv != nil {
			c.byProv = msg.byProv
		}
		if msg.roi != nil {
			c.roi = msg.roi
		}
		return c, tick(pollSlow, costTick{})
	case costTick:
		return c, c.fetch()
	case cacheFlushMsg:
		c.flushing = false
		c.flushErr = msg.err
		if msg.err == nil {
			c.flushNote = "gateway cache flushed"
		}
		return c, nil
	case tea.KeyMsg:
		if handled, cmd := c.cf.update(msg); handled {
			return c, cmd
		}
		if msg.String() == "f" && !c.flushing {
			c.flushNote = ""
			c.flushErr = nil
			return c, c.cf.begin("flush the gateway cache", c.flush)
		}
	}
	return c, nil
}

// flush fires the cache-flush write (after the confirm gate passes).
func (c *cost) flush() tea.Cmd {
	c.flushing = true
	return func() tea.Msg {
		ctx, cancel := fetchCtx()
		defer cancel()
		return cacheFlushMsg{err: c.gw.CacheFlush(ctx)}
	}
}

func (c *cost) View(width, height int) string {
	if c.cf.capturing() {
		return c.cf.view()
	}
	if c.loading && c.byProv == nil {
		return styles.TileLabel.Render("loading cost…")
	}
	var b strings.Builder
	if c.flushErr != nil {
		b.WriteString(lipgloss.NewStyle().Foreground(styles.Red).Render("⚠ flush: " + c.flushErr.Error()))
		b.WriteString("\n")
	} else if c.flushing {
		b.WriteString(styles.TileLabel.Render("flushing cache…"))
		b.WriteString("\n")
	} else if c.flushNote != "" {
		b.WriteString(lipgloss.NewStyle().Foreground(styles.Green).Render("✓ " + c.flushNote))
		b.WriteString("\n")
	}
	if c.err != nil {
		b.WriteString(lipgloss.NewStyle().Foreground(styles.Red).Render("⚠ " + c.err.Error()))
		b.WriteString("\n")
		if c.byProv == nil {
			return b.String()
		}
		b.WriteString(styles.TileLabel.Render("(showing last-good data)\n"))
	}
	b.WriteString(c.burnRateLine())
	b.WriteString("\n")
	b.WriteString(c.roiLine())
	b.WriteString("\n\n")
	b.WriteString(c.providerTable())
	return b.String()
}

// burnRateLine extrapolates the window spend to an hourly burn rate and a
// 30-day projection (pure client-side math over the cache-ROI window).
func (c *cost) burnRateLine() string {
	if c.roi == nil || c.roi.PeriodDays <= 0 {
		return styles.TileLabel.Render("burn rate: (no data)")
	}
	perHour := c.roi.TotalEstimatedCostUSD / float64(c.roi.PeriodDays*24)
	projection := perHour * 24 * 30
	badge := lipgloss.NewStyle().Bold(true).Foreground(styles.BrandHi).Render(fmt.Sprintf("$%.4f/hr", perHour))
	return styles.TileValue.Render("Burn rate ") + badge +
		styles.TileLabel.Render(fmt.Sprintf("  → ~$%.2f/30d (from $%.4f over %dd)",
			projection, c.roi.TotalEstimatedCostUSD, c.roi.PeriodDays))
}

// roiLine summarizes cache ROI: net savings, hit count, and savings as a share
// of total spend.
func (c *cost) roiLine() string {
	if c.roi == nil {
		return styles.TileLabel.Render("cache ROI: (no data)")
	}
	saved := c.roi.TotalCacheNetSavingsUSD
	share := 0.0
	if c.roi.TotalEstimatedCostUSD > 0 {
		share = saved / c.roi.TotalEstimatedCostUSD * 100
	}
	badge := lipgloss.NewStyle().Bold(true).Foreground(styles.Green).Render(fmt.Sprintf("$%.4f", saved))
	return styles.TileValue.Render("Cache saved ") + badge +
		styles.TileLabel.Render(fmt.Sprintf("  (%.1f%% of spend · %d hits over %dd)",
			share, c.roi.RequestsWithCacheHit, c.roi.PeriodDays))
}

// providerTable lists per-provider spend with average latency (top talkers).
func (c *cost) providerTable() string {
	var b strings.Builder
	b.WriteString(styles.TileValue.Render("Top providers"))
	b.WriteString("\n")
	b.WriteString(lipgloss.NewStyle().Foreground(styles.Sub).Render(
		fmt.Sprintf("  %-22s %8s %12s %10s %10s", "PROVIDER", "REQS", "TOKENS", "COST", "AVG LAT")))
	b.WriteString("\n")
	if c.byProv == nil || len(c.byProv.Data) == 0 {
		b.WriteString(styles.TileLabel.Render("  (no spend)"))
		return b.String()
	}
	var lines []string
	var total float64
	for _, r := range c.byProv.Data {
		label := r.ProviderLabel
		if label == "" {
			label = r.Provider
		}
		if len(label) > 22 {
			label = label[:21] + "…"
		}
		lines = append(lines, fmt.Sprintf("  %-22s %8d %12d %10s %10s",
			label, r.RequestCount, r.TotalTokens, fmt.Sprintf("$%.4f", r.TotalEstCostUSD), ms(int(r.AvgLatencyMs))))
		total += r.TotalEstCostUSD
	}
	b.WriteString(strings.Join(lines, "\n"))
	b.WriteString("\n")
	b.WriteString(styles.TileValue.Render(fmt.Sprintf("  total $%.4f across %d providers", total, len(c.byProv.Data))))
	return b.String()
}

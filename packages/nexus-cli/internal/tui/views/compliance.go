package views

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/kit"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/styles"
)

// complianceView shows the compliance KPI rollup (block rate, TLS coverage,
// hook error rate).
type complianceView struct {
	gw      kit.Gateway
	res     *core.ComplianceOverview
	err     error
	loading bool
}

type complianceMsg struct {
	res *core.ComplianceOverview
	err error
}
type complianceTick struct{}

func newCompliance(gw kit.Gateway) *complianceView { return &complianceView{gw: gw, loading: true} }

func (c *complianceView) Init() tea.Cmd { return c.fetch() }

func (c *complianceView) fetch() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := kit.FetchCtx()
		defer cancel()
		res, err := c.gw.ComplianceOverview(ctx, nil)
		return complianceMsg{res: res, err: err}
	}
}

func (c *complianceView) Update(msg tea.Msg) (kit.ViewModel, tea.Cmd) {
	switch msg := msg.(type) {
	case complianceMsg:
		c.loading = false
		c.err = msg.err
		if msg.res != nil {
			c.res = msg.res
		}
		return c, kit.Tick(kit.PollSlow, complianceTick{})
	case complianceTick:
		return c, c.fetch()
	}
	return c, nil
}

func (c *complianceView) View(width, height int) string {
	if c.loading && c.res == nil {
		return styles.TileLabel.Render("loading compliance…")
	}
	var b strings.Builder
	b.WriteString(styles.TileValue.Render("Compliance overview"))
	if c.err != nil {
		b.WriteString("  " + lipgloss.NewStyle().Foreground(styles.Red).Render("⚠ "+c.err.Error()))
	}
	b.WriteString("\n")
	if c.res == nil {
		return b.String()
	}
	k := c.res.KPIs
	blockRate := k.OverallBlockRate * 100
	rateColor := styles.Green
	if blockRate >= 5 {
		rateColor = styles.Red
	} else if blockRate >= 1 {
		rateColor = styles.Amber
	}
	tiles := []string{
		kit.Tile("Requests", fmt.Sprintf("%d", k.TotalRequests)),
		kit.Tile("Blocked", fmt.Sprintf("%d", k.TotalBlocked)),
		kit.Tile("Block rate", lipgloss.NewStyle().Foreground(rateColor).Render(fmt.Sprintf("%.2f%%", blockRate))),
		kit.Tile("TLS cov", fmt.Sprintf("%.0f%%", k.TLSCoveragePct)),
		kit.Tile("Hook err", fmt.Sprintf("%.2f%%", k.HookErrorRate*100)),
	}
	b.WriteString(lipgloss.JoinHorizontal(lipgloss.Top, tiles...))
	return b.String()
}

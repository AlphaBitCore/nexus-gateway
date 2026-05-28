package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/styles"
)

// nodesView lists the registered nodes with heartbeat, version, and config
// drift (target version != applied version).
type nodesView struct {
	gw      Gateway
	res     *core.NodesResult
	err     error
	loading bool
}

type nodesMsg struct {
	res *core.NodesResult
	err error
}
type nodesTick struct{}

func newNodes(gw Gateway) *nodesView { return &nodesView{gw: gw, loading: true} }

func (n *nodesView) Init() tea.Cmd { return n.fetch() }

func (n *nodesView) fetch() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := fetchCtx()
		defer cancel()
		res, err := n.gw.Nodes(ctx)
		return nodesMsg{res: res, err: err}
	}
}

func (n *nodesView) Update(msg tea.Msg) (viewModel, tea.Cmd) {
	switch msg := msg.(type) {
	case nodesMsg:
		n.loading = false
		n.err = msg.err
		if msg.res != nil {
			n.res = msg.res
		}
		return n, tick(pollSlow, nodesTick{})
	case nodesTick:
		return n, n.fetch()
	}
	return n, nil
}

func (n *nodesView) View(width, height int) string {
	if n.loading && n.res == nil {
		return styles.TileLabel.Render("loading nodes…")
	}
	var b strings.Builder
	b.WriteString(styles.TileValue.Render("Nodes"))
	if n.err != nil {
		b.WriteString("  " + lipgloss.NewStyle().Foreground(styles.Red).Render("⚠ "+n.err.Error()))
	}
	b.WriteString("\n")
	if n.res == nil || len(n.res.Nodes) == 0 {
		b.WriteString(styles.TileLabel.Render("  no nodes"))
		return b.String()
	}
	b.WriteString(lipgloss.NewStyle().Foreground(styles.Sub).Render(
		fmt.Sprintf("  %-20s %-16s %-9s %-10s %s", "NAME", "TYPE", "STATUS", "VERSION", "SYNC")))
	b.WriteString("\n")
	var rows []string
	for _, nd := range n.res.Nodes {
		dot := lipgloss.NewStyle().Foreground(nodeStatusColor(nd.Status)).Render("●")
		sync := lipgloss.NewStyle().Foreground(styles.Green).Render("in sync")
		if nd.Drifted() {
			sync = lipgloss.NewStyle().Foreground(styles.Amber).Render("out of sync")
		}
		name := nd.Name
		if len(name) > 20 {
			name = name[:19] + "…"
		}
		rows = append(rows, fmt.Sprintf("  %s %-18s %-16s %-9s %-10s %s",
			dot, name, nd.Type, nd.Status, dash(nd.Version), sync))
	}
	b.WriteString(strings.Join(rows, "\n"))
	return b.String()
}

// nodeStatusColor maps a node status to a RAG color.
func nodeStatusColor(status string) lipgloss.Color {
	switch strings.ToLower(status) {
	case "online", "active", "ready":
		return styles.Green
	case "degraded", "stale":
		return styles.Amber
	default:
		return styles.Red
	}
}

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

// nodesView lists the registered nodes with heartbeat, version, and config drift
// (target version != applied version), with a row cursor; enter opens a detail
// drawer of the selected node.
type nodesView struct {
	gw      kit.Gateway
	res     *core.NodesResult
	cursor  int
	detail  bool
	err     error
	loading bool
}

type nodesMsg struct {
	res *core.NodesResult
	err error
}
type nodesTick struct{}

func newNodes(gw kit.Gateway) *nodesView { return &nodesView{gw: gw, loading: true} }

func (n *nodesView) Init() tea.Cmd { return n.fetch() }

func (n *nodesView) fetch() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := kit.FetchCtx()
		defer cancel()
		res, err := n.gw.Nodes(ctx)
		return nodesMsg{res: res, err: err}
	}
}

func (n *nodesView) Update(msg tea.Msg) (kit.ViewModel, tea.Cmd) {
	switch msg := msg.(type) {
	case nodesMsg:
		n.loading = false
		n.err = msg.err
		if msg.res != nil {
			n.res = msg.res
		}
		n.clampCursor()
		return n, kit.Tick(kit.PollSlow, nodesTick{})
	case nodesTick:
		return n, n.fetch()
	case tea.KeyPressMsg:
		if n.detail {
			return n, nil
		}
		switch msg.String() {
		case "up", "k":
			if n.cursor > 0 {
				n.cursor--
			}
		case "down", "j":
			if n.cursor < n.count()-1 {
				n.cursor++
			}
		case "enter":
			if _, ok := n.selected(); ok {
				n.detail = true
			}
		}
	}
	return n, nil
}

func (n *nodesView) Back() bool {
	if n.detail {
		n.detail = false
		return true
	}
	return false
}

func (n *nodesView) count() int {
	if n.res == nil {
		return 0
	}
	return len(n.res.Nodes)
}

func (n *nodesView) selected() (core.Node, bool) {
	if n.res == nil || n.cursor < 0 || n.cursor >= len(n.res.Nodes) {
		return core.Node{}, false
	}
	return n.res.Nodes[n.cursor], true
}

func (n *nodesView) clampCursor() {
	if n.cursor >= n.count() {
		n.cursor = n.count() - 1
	}
	if n.cursor < 0 {
		n.cursor = 0
	}
}

func (n *nodesView) Help() string {
	if n.detail {
		return "←/esc back · q quit"
	}
	return "↑/↓ select · enter open · ←/esc back · q quit"
}

func (n *nodesView) View(width, height int) string {
	if n.loading && n.res == nil {
		return styles.TileLabel.Render("loading nodes…")
	}
	if n.detail {
		return n.detailView()
	}
	var b strings.Builder
	b.WriteString(styles.TileValue.Render("Nodes"))
	if n.err != nil {
		b.WriteString("  " + lipgloss.NewStyle().Foreground(styles.Red).Render("⚠ "+n.err.Error()))
	}
	b.WriteString("\n")
	if n.count() == 0 {
		b.WriteString(styles.TileLabel.Render("  no nodes"))
		return b.String()
	}
	b.WriteString(lipgloss.NewStyle().Foreground(styles.Sub).Render(
		fmt.Sprintf("  %-20s %-16s %-9s %-10s %s", "NAME", "TYPE", "STATUS", "VERSION", "SYNC")))
	b.WriteString("\n")
	var rows []string
	for i, nd := range n.res.Nodes {
		dot := lipgloss.NewStyle().Foreground(nodeStatusColor(nd.Status)).Render("●")
		sync := lipgloss.NewStyle().Foreground(styles.Green).Render("in sync")
		if nd.Drifted() {
			sync = lipgloss.NewStyle().Foreground(styles.Amber).Render("out of sync")
		}
		cursor := "  "
		line := fmt.Sprintf("%s %-18s %-16s %-9s %-10s %s",
			dot, kit.Clip(nd.Name, 18), nd.Type, nd.Status, kit.Dash(nd.Version), sync)
		if i == n.cursor {
			cursor = lipgloss.NewStyle().Foreground(styles.Brand).Render("▸ ")
			line = lipgloss.NewStyle().Bold(true).Render(line)
		}
		rows = append(rows, cursor+line)
	}
	b.WriteString(strings.Join(rows, "\n"))
	return b.String()
}

// detailView renders the full record of the selected node, foregrounding the
// config-drift fields (target vs applied version) since that is the operator's
// primary "is this node converged" question.
func (n *nodesView) detailView() string {
	nd, ok := n.selected()
	if !ok {
		return styles.TileLabel.Render("(no node selected)")
	}
	var b strings.Builder
	b.WriteString(styles.TileValue.Render("Node · " + nd.Name))
	b.WriteString("\n\n")
	statusDot := lipgloss.NewStyle().Foreground(nodeStatusColor(nd.Status)).Render("● " + kit.Dash(nd.Status))
	b.WriteString(kit.DetailRow("Status", statusDot) + "\n")
	b.WriteString(kit.DetailRow("Type", kit.Dash(nd.Type)) + "\n")
	b.WriteString(kit.DetailRow("Version", kit.Dash(nd.Version)) + "\n")
	sync := lipgloss.NewStyle().Foreground(styles.Green).Render(
		fmt.Sprintf("in sync (target %d = applied %d)", nd.TargetVersion, nd.AppliedVersion))
	if nd.Drifted() {
		sync = lipgloss.NewStyle().Foreground(styles.Amber).Render(
			fmt.Sprintf("out of sync (target %d ≠ applied %d)", nd.TargetVersion, nd.AppliedVersion))
	}
	b.WriteString(kit.DetailRow("Config sync", sync) + "\n")
	b.WriteString(kit.DetailRow("Last seen", kit.Dash(nd.LastSeenAt)) + "\n")
	b.WriteString(kit.DetailRow("Protocol", kit.Dash(nd.ConnProtocol)) + "\n")
	b.WriteString(kit.DetailRow("Physical id", kit.Dash(nd.PhysicalID)) + "\n")
	b.WriteString(kit.DetailRow("Node id", kit.Dash(nd.ID)))
	return b.String()
}

// nodeStatusColor maps a node status to a RAG color.
func nodeStatusColor(status string) color.Color {
	switch strings.ToLower(status) {
	case "online", "active", "ready":
		return styles.Green
	case "degraded", "stale":
		return styles.Amber
	default:
		return styles.Red
	}
}

package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/styles"
)

// configSyncView shows how many nodes have not yet applied the target config.
// The Nodes view shows per-node drift; this is the fleet-wide rollup.
type configSyncView struct {
	gw      Gateway
	res     *core.ConfigSyncResult
	err     error
	loading bool
}

type configSyncMsg struct {
	res *core.ConfigSyncResult
	err error
}
type configSyncTick struct{}

func newConfigSync(gw Gateway) *configSyncView { return &configSyncView{gw: gw, loading: true} }

func (s *configSyncView) Init() tea.Cmd { return s.fetch() }

func (s *configSyncView) fetch() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := fetchCtx()
		defer cancel()
		res, err := s.gw.ConfigSyncOutOfSync(ctx)
		return configSyncMsg{res: res, err: err}
	}
}

func (s *configSyncView) Update(msg tea.Msg) (viewModel, tea.Cmd) {
	switch msg := msg.(type) {
	case configSyncMsg:
		s.loading = false
		s.err = msg.err
		if msg.res != nil {
			s.res = msg.res
		}
		return s, tick(pollSlow, configSyncTick{})
	case configSyncTick:
		return s, s.fetch()
	}
	return s, nil
}

func (s *configSyncView) View(width, height int) string {
	if s.loading && s.res == nil {
		return styles.TileLabel.Render("loading config sync…")
	}
	var b strings.Builder
	b.WriteString(styles.TileValue.Render("Config sync"))
	if s.err != nil {
		b.WriteString("  " + lipgloss.NewStyle().Foreground(styles.Red).Render("⚠ "+s.err.Error()))
	}
	b.WriteString("\n")
	if s.res == nil {
		return b.String()
	}
	n := s.res.Total
	color := styles.Green
	state := "all nodes in sync"
	if n > 0 {
		color = styles.Amber
		state = fmt.Sprintf("%d node(s) out of sync — applied config lags target", n)
	}
	b.WriteString(lipgloss.NewStyle().Bold(true).Foreground(color).Render(state))
	b.WriteString("\n")
	b.WriteString(styles.TileLabel.Render("see the Nodes view for per-node drift"))
	return b.String()
}

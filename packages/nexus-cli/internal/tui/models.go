package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/styles"
)

// modelsView is the Models catalog browser: every configured model grouped by
// provider with its code, friendly name, type, context window, per-million
// pricing, and enabled state. It scrolls when the catalog exceeds the viewport.
type modelsView struct {
	gw      Gateway
	cat     *core.ModelCatalog
	offset  int
	err     error
	loading bool
}

type modelsMsg struct {
	cat *core.ModelCatalog
	err error
}
type modelsTick struct{}

func newModels(gw Gateway) *modelsView { return &modelsView{gw: gw, loading: true} }

func (v *modelsView) Init() tea.Cmd { return v.fetch() }

func (v *modelsView) fetch() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := fetchCtx()
		defer cancel()
		cat, err := v.gw.AdminModels(ctx)
		return modelsMsg{cat: cat, err: err}
	}
}

func (v *modelsView) Update(msg tea.Msg) (viewModel, tea.Cmd) {
	switch msg := msg.(type) {
	case modelsMsg:
		v.loading = false
		v.err = msg.err
		if msg.cat != nil {
			v.cat = msg.cat
		}
		return v, tick(pollSlow, modelsTick{})
	case modelsTick:
		return v, v.fetch()
	case tea.KeyMsg:
		switch msg.String() {
		case "up", "k":
			if v.offset > 0 {
				v.offset--
			}
		case "down", "j":
			v.offset++ // upper bound clamped in View against the viewport height
		}
	}
	return v, nil
}

func (v *modelsView) help() string {
	return "↑/↓ scroll · tab/1-9 switch · : palette · q quit"
}

func (v *modelsView) View(width, height int) string {
	if v.loading && v.cat == nil {
		return styles.TileLabel.Render("loading model catalog…")
	}
	head := styles.TileValue.Render("Model catalog")
	if v.err != nil {
		head += "  " + lipgloss.NewStyle().Foreground(styles.Red).Render("⚠ "+v.err.Error())
	}
	if v.cat == nil || len(v.cat.Data) == 0 {
		return head + "\n" + styles.TileLabel.Render("(no models)")
	}
	lines := v.catalogLines()
	budget := height - 2
	if budget < 3 {
		budget = 3
	}
	// Clamp the scroll offset to the last full page (down past the end is a
	// no-op; height is only known here, so the clamp lives in View).
	if max := len(lines) - budget; v.offset > max {
		v.offset = max
	}
	if v.offset < 0 {
		v.offset = 0
	}
	end := v.offset + budget
	if end > len(lines) {
		end = len(lines)
	}
	scroll := ""
	if len(lines) > budget {
		scroll = styles.TileLabel.Render(fmt.Sprintf("   (%d–%d of %d · ↑/↓)", v.offset+1, end, len(lines)))
	}
	return head + scroll + "\n" + strings.Join(lines[v.offset:end], "\n")
}

// catalogLines flattens the catalog into render lines: a provider header, a
// column header, then one row per model, with a blank separator between groups.
func (v *modelsView) catalogLines() []string {
	var lines []string
	for _, g := range v.cat.Data {
		lines = append(lines, lipgloss.NewStyle().Bold(true).Foreground(styles.Brand).Render(
			fmt.Sprintf("▸ %s (%d)", g.Provider.Label(), len(g.Models))))
		lines = append(lines, lipgloss.NewStyle().Foreground(styles.Sub).Render(
			fmt.Sprintf("    %-30s %-22s %7s %9s %9s %s", "CODE", "NAME", "CTX", "IN/M", "OUT/M", "")))
		for _, m := range g.Models {
			enabled := lipgloss.NewStyle().Foreground(styles.Green).Render("on")
			if !m.Enabled {
				enabled = lipgloss.NewStyle().Foreground(styles.Sub).Render("off")
			}
			lines = append(lines, fmt.Sprintf("    %-30s %-22s %7s %9s %9s %s",
				clip(m.Code, 30), clip(m.Name, 22), ktok(m.MaxContextTokens),
				fmt.Sprintf("$%.2f", m.InputPricePerMillion),
				fmt.Sprintf("$%.2f", m.OutputPricePerMillion), enabled))
		}
		lines = append(lines, "")
	}
	return lines
}

// clip truncates s to n runes with a trailing ellipsis (rune-safe so a
// multibyte label never splits mid-rune).
func clip(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// ktok renders a token count compactly (200000 → "200k", 1500000 → "1.5M").
func ktok(n int) string {
	switch {
	case n >= 1000000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1000:
		return fmt.Sprintf("%dk", n/1000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

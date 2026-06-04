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

// ModelsView is the Models catalog browser: every configured model grouped by
// provider with its code, friendly name, context window, per-million pricing,
// and enabled state. A row cursor selects a model; enter opens a detail drawer
// (the model's type + status, not shown in the list, plus the full pricing). The
// catalog auto-scrolls to keep the cursor visible.
type ModelsView struct {
	gw      kit.Gateway
	cat     *core.ModelCatalog
	cursor  int
	offset  int // first visible render line; auto-scrolled to keep the cursor in view
	detail  bool
	err     error
	loading bool

	pick    bool   // pick mode: enter selects the row's model for the chat (/model)
	current string // the chat's current model code, marked in the list
}

// EnterPick puts the view in model-pick mode and records the current selection so
// it can be marked. cur is the chat's current model code.
func (v *ModelsView) EnterPick(cur string) {
	v.pick = true
	v.current = cur
	v.detail = false
}

// catalogModel is one selectable row: a model plus its provider's friendly label
// (the cursor indexes a flat list of these across all provider groups).
type catalogModel struct {
	providerLabel string
	m             core.Model
}

type modelsMsg struct {
	cat *core.ModelCatalog
	err error
}
type modelsTick struct{}

func newModels(gw kit.Gateway) *ModelsView { return &ModelsView{gw: gw, loading: true} }

func (v *ModelsView) Init() tea.Cmd { return v.fetch() }

func (v *ModelsView) fetch() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := kit.FetchCtx()
		defer cancel()
		cat, err := v.gw.AdminModels(ctx)
		return modelsMsg{cat: cat, err: err}
	}
}

func (v *ModelsView) Update(msg tea.Msg) (kit.ViewModel, tea.Cmd) {
	switch msg := msg.(type) {
	case modelsMsg:
		v.loading = false
		v.err = msg.err
		if msg.cat != nil {
			v.cat = msg.cat
		}
		return v, kit.Tick(kit.PollSlow, modelsTick{})
	case modelsTick:
		return v, v.fetch()
	case tea.KeyPressMsg:
		if v.detail {
			return v, nil
		}
		switch msg.String() {
		case "up", "k":
			if v.cursor > 0 {
				v.cursor--
			}
		case "down", "j":
			if v.cursor < len(v.flatModels())-1 {
				v.cursor++
			}
		case "enter":
			if v.pick {
				if cm, ok := v.selected(); ok {
					return v, func() tea.Msg { return kit.SetModelMsg{Code: cm.m.Code} }
				}
				return v, nil
			}
			if _, ok := v.selected(); ok {
				v.detail = true
			}
		}
	}
	return v, nil
}

// back closes the detail drawer so esc returns to the catalog before the root
// pops the nav stack.
func (v *ModelsView) Back() bool {
	if v.detail {
		v.detail = false
		return true
	}
	return false
}

// flatModels is the cursor domain: every model across all provider groups, in
// display order, each tagged with its provider's friendly label.
func (v *ModelsView) flatModels() []catalogModel {
	if v.cat == nil {
		return nil
	}
	var out []catalogModel
	for _, g := range v.cat.Data {
		for _, m := range g.Models {
			out = append(out, catalogModel{providerLabel: g.Provider.Label(), m: m})
		}
	}
	return out
}

func (v *ModelsView) selected() (catalogModel, bool) {
	fm := v.flatModels()
	if v.cursor < 0 || v.cursor >= len(fm) {
		return catalogModel{}, false
	}
	return fm[v.cursor], true
}

func (v *ModelsView) Help() string {
	if v.detail {
		return "←/esc back · q quit"
	}
	if v.pick {
		return "↑/↓ select · enter use for chat · ←/esc back · q quit"
	}
	return "↑/↓ select · enter open · ←/esc back · q quit"
}

func (v *ModelsView) View(width, height int) string {
	if v.loading && v.cat == nil {
		return styles.TileLabel.Render("loading model catalog…")
	}
	if v.detail {
		return v.detailView()
	}
	head := styles.TileValue.Render("Model catalog")
	if v.err != nil {
		head += "  " + lipgloss.NewStyle().Foreground(styles.Red).Render("⚠ "+v.err.Error())
	}
	if v.cat == nil || len(v.cat.Data) == 0 {
		return head + "\n" + styles.TileLabel.Render("(no models)")
	}
	lines, cursorLine := v.catalogLines()
	budget := height - 2
	if budget < 3 {
		budget = 3
	}
	// Auto-scroll so the cursor's row stays within the visible window, then clamp
	// to valid bounds (height is only known here, so the scroll math lives in View).
	if cursorLine < v.offset {
		v.offset = cursorLine
	}
	if cursorLine >= v.offset+budget {
		v.offset = cursorLine - budget + 1
	}
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

// catalogLines flattens the catalog into render lines (provider header, column
// header, one row per model, blank separator between groups) and returns the
// render-line index of the cursor's model so View can scroll it into view.
func (v *ModelsView) catalogLines() (lines []string, cursorLine int) {
	mi := 0 // running model index, matched against the cursor
	for _, g := range v.cat.Data {
		lines = append(lines, lipgloss.NewStyle().Bold(true).Foreground(styles.Brand).Render(
			fmt.Sprintf("  %s (%d)", g.Provider.Label(), len(g.Models))))
		lines = append(lines, lipgloss.NewStyle().Foreground(styles.Sub).Render(
			fmt.Sprintf("    %-30s %-22s %7s %9s %9s %s", "CODE", "NAME", "CTX", "IN/M", "OUT/M", "")))
		for _, m := range g.Models {
			enabled := lipgloss.NewStyle().Foreground(styles.Green).Render("on")
			if !m.Enabled {
				enabled = lipgloss.NewStyle().Foreground(styles.Sub).Render("off")
			}
			row := fmt.Sprintf("%-30s %-22s %7s %9s %9s %s",
				kit.Clip(m.Code, 30), kit.Clip(m.Name, 22), kit.Ktok(m.MaxContextTokens),
				fmt.Sprintf("$%.2f", m.InputPricePerMillion),
				fmt.Sprintf("$%.2f", m.OutputPricePerMillion), enabled)
			lead := "    "
			if mi == v.cursor {
				lead = lipgloss.NewStyle().Foreground(styles.Brand).Render("  ▸ ")
				row = lipgloss.NewStyle().Bold(true).Render(row)
				cursorLine = len(lines)
			}
			if m.Code == v.current { // mark the chat's current model (pick mode)
				row += lipgloss.NewStyle().Foreground(styles.Green).Bold(true).Render(" ● current")
			}
			lines = append(lines, lead+row)
			mi++
		}
		lines = append(lines, "")
	}
	return lines, cursorLine
}

// detailView renders the full record of the selected model, foregrounding the
// type + status that the catalog list does not show.
func (v *ModelsView) detailView() string {
	cm, ok := v.selected()
	if !ok {
		return styles.TileLabel.Render("(no model selected)")
	}
	m := cm.m
	var b strings.Builder
	b.WriteString(styles.TileValue.Render("Model · " + m.Name))
	b.WriteString("\n\n")
	enabled := lipgloss.NewStyle().Foreground(styles.Green).Render("● enabled")
	if !m.Enabled {
		enabled = lipgloss.NewStyle().Foreground(styles.Sub).Render("○ disabled")
	}
	b.WriteString(kit.DetailRow("Provider", kit.Dash(cm.providerLabel)) + "\n")
	b.WriteString(kit.DetailRow("Code", kit.Dash(m.Code)) + "\n")
	b.WriteString(kit.DetailRow("Type", kit.Dash(m.Type)) + "\n")
	b.WriteString(kit.DetailRow("Status", kit.Dash(m.Status)) + "\n")
	b.WriteString(kit.DetailRow("Enabled", enabled) + "\n")
	b.WriteString(kit.DetailRow("Context", kit.Ktok(m.MaxContextTokens)+" tokens") + "\n")
	b.WriteString(kit.DetailRow("Input $/M", fmt.Sprintf("$%.2f", m.InputPricePerMillion)) + "\n")
	b.WriteString(kit.DetailRow("Output $/M", fmt.Sprintf("$%.2f", m.OutputPricePerMillion)))
	return b.String()
}

// ktok renders a token count compactly (200000 → "200k", 1500000 → "1.5M").

// Picking reports whether the view is in model-pick mode (the shell's /model drive).
func (v *ModelsView) Picking() bool { return v.pick }

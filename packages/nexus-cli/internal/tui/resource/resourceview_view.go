package resource

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/restable"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/kit"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/styles"
)

func (r *resourceView) View(width, height int) string {
	if r.cf.Capturing() {
		return r.cf.View()
	}
	if r.form != nil {
		return r.form.view()
	}
	if len(r.stack) == 0 {
		return r.kindView()
	}
	fr := r.top()
	switch fr.mode {
	case frameTable:
		return r.tableView(fr, width)
	case frameRecord:
		return r.recordView(fr, width)
	default:
		return r.menuView(fr)
	}
}

func (r *resourceView) noteLine() string {
	var b strings.Builder
	if r.err != nil {
		b.WriteString(lipgloss.NewStyle().Foreground(styles.Red).Render("⚠ "+r.err.Error()) + "\n")
	} else if r.note != "" {
		b.WriteString(lipgloss.NewStyle().Foreground(styles.Green).Render(r.note) + "\n")
	}
	return b.String()
}

func (r *resourceView) kindView() string {
	var b strings.Builder
	b.WriteString(styles.TileValue.Render("Resource — pick a kind"))
	b.WriteString("\n")
	b.WriteString(lipgloss.NewStyle().Foreground(styles.Brand).Render("filter ▸ ") + r.filter.View())
	b.WriteString("\n")
	if r.busy {
		b.WriteString(styles.TileLabel.Render("opening "+r.pendingKind+"…") + "\n")
	}
	b.WriteString(r.noteLine())
	f := r.filtered()
	if len(f) == 0 {
		b.WriteString(styles.TileLabel.Render("  (no matching kinds)"))
		return b.String()
	}
	var rows []string
	for i, k := range f {
		cursor := "  "
		summary := fmt.Sprintf("%d ops", k.OpCount)
		if len(k.Capabilities) > 0 {
			summary += " · " + strings.Join(k.Capabilities, " ")
		}
		name := fmt.Sprintf("%-28s ", k.Kind)
		if i == r.kindCur {
			cursor = lipgloss.NewStyle().Foreground(styles.Brand).Render("▸ ")
			name = lipgloss.NewStyle().Bold(true).Render(name)
		}
		rows = append(rows, cursor+name+styles.TileLabel.Render(summary))
	}
	b.WriteString(strings.Join(rows, "\n"))
	return b.String()
}

func (r *resourceView) tableView(fr *resFrame, width int) string {
	var b strings.Builder
	b.WriteString(styles.TileValue.Render(r.Crumb()))
	b.WriteString("\n")
	b.WriteString(r.noteLine())
	if r.busy && fr.rows == nil {
		b.WriteString(styles.TileLabel.Render("loading…"))
		return b.String()
	}
	page := restable.Paginate(fr.rows, fr.page, resPageSize)
	if len(fr.rows) == 0 {
		if len(fr.raw) > 0 {
			b.WriteString(styles.TileLabel.Render(kit.PrettyJSON(fr.raw)))
		} else {
			b.WriteString(styles.TileLabel.Render("  (empty)"))
		}
		return b.String()
	}
	widths := columnWidths(fr.cols, page.Rows, width)
	// header
	var hdr strings.Builder
	hdr.WriteString("  ")
	for i, c := range fr.cols {
		hdr.WriteString(fmt.Sprintf("%-*s ", widths[i], kit.Clip(strings.ToUpper(c), widths[i])))
	}
	b.WriteString(lipgloss.NewStyle().Foreground(styles.Sub).Render(hdr.String()))
	b.WriteString("\n")
	for ri, row := range page.Rows {
		cursor := "  "
		var line strings.Builder
		for i, c := range fr.cols {
			line.WriteString(fmt.Sprintf("%-*s ", widths[i], kit.Clip(restable.CellString(row[c]), widths[i])))
		}
		s := line.String()
		if ri == fr.cursor {
			cursor = lipgloss.NewStyle().Foreground(styles.Brand).Render("▸ ")
			s = lipgloss.NewStyle().Bold(true).Render(s)
		}
		b.WriteString(cursor + s + "\n")
	}
	b.WriteString(styles.TileLabel.Render(fmt.Sprintf("  page %d/%d · %d rows", page.PageIndex+1, page.PageCount, page.Total)))
	return b.String()
}

func (r *resourceView) recordView(fr *resFrame, width int) string {
	var b strings.Builder
	b.WriteString(styles.TileValue.Render(r.Crumb()))
	b.WriteString("\n")
	b.WriteString(r.noteLine())
	if r.busy && len(fr.raw) == 0 {
		b.WriteString(styles.TileLabel.Render("loading…"))
		return b.String()
	}
	b.WriteString(styles.TileLabel.Render(kit.PrettyJSON(fr.raw)))
	if len(fr.menu) > 0 {
		b.WriteString("\n\n")
		b.WriteString(lipgloss.NewStyle().Foreground(styles.Sub).Render("  operations"))
		b.WriteString("\n")
		b.WriteString(r.menuLines(fr))
	}
	return b.String()
}

func (r *resourceView) menuView(fr *resFrame) string {
	var b strings.Builder
	b.WriteString(styles.TileValue.Render(r.Crumb()))
	b.WriteString("\n")
	b.WriteString(r.noteLine())
	if len(fr.menu) == 0 {
		b.WriteString(styles.TileLabel.Render("  (no operations)"))
		return b.String()
	}
	b.WriteString(r.menuLines(fr))
	return b.String()
}

func (r *resourceView) menuLines(fr *resFrame) string {
	var rows []string
	for i, op := range fr.menu {
		cursor := "  "
		tag := lipgloss.NewStyle().Foreground(styles.Sub).Render(op.Method)
		if op.Mutating {
			tag = lipgloss.NewStyle().Foreground(styles.Amber).Render(op.Method)
		}
		// Prefer the operation's OpenAPI summary — "Set node config override"
		// reads better than a synthesized path tail; fall back to the label.
		text := op.Summary
		if text == "" {
			text = op.Label
		}
		if r := []rune(text); len(r) > 44 { // rune-safe truncation
			text = string(r[:43]) + "…"
		}
		label := fmt.Sprintf("%-44s", text)
		if i == fr.cursor {
			cursor = lipgloss.NewStyle().Foreground(styles.Brand).Render("▸ ")
			label = lipgloss.NewStyle().Bold(true).Render(label)
		}
		rows = append(rows, cursor+label+" "+tag)
	}
	return strings.Join(rows, "\n")
}

// columnWidths sizes each column to its widest cell (header included), capped so a
// wide value never pushes the table off-screen and the row still fits the pane.
func columnWidths(cols []string, rows []restable.Row, width int) []int {
	const maxCol = 24
	widths := make([]int, len(cols))
	for i, c := range cols {
		widths[i] = len(c)
	}
	for _, row := range rows {
		for i, c := range cols {
			if n := len(restable.CellString(row[c])); n > widths[i] {
				widths[i] = n
			}
		}
	}
	for i := range widths {
		if widths[i] > maxCol {
			widths[i] = maxCol
		}
		if widths[i] < 3 {
			widths[i] = 3
		}
	}
	return widths
}

func (r *resourceView) Crumb() string {
	parts := []string{"Resource"}
	for _, fr := range r.stack {
		parts = append(parts, fr.title)
	}
	return strings.Join(parts, " · ")
}

func (r *resourceView) Help() string {
	if r.cf.Capturing() {
		return r.cf.HelpHint()
	}
	if r.form != nil {
		return r.form.Help()
	}
	if len(r.stack) == 0 {
		return "type to filter · ↑/↓ select · enter open · esc home"
	}
	switch r.top().mode {
	case frameTable:
		return "↑/↓ row · enter drill · o operations · f filter · n/p page · ←/esc back · q quit"
	default:
		return "↑/↓ select · enter run · ←/esc back · q quit"
	}
}

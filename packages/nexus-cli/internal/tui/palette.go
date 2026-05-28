package tui

import (
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/styles"
)

// jumpMsg is emitted by the palette when the operator selects a view; the root
// model switches to that view index and closes the palette.
type jumpMsg struct{ index int }

// paletteCloseMsg is emitted when the palette is dismissed (esc).
type paletteCloseMsg struct{}

// palette is the k9s-style command overlay: type to fuzzy-filter the view
// registry, ↑/↓ to move, enter to jump, esc to dismiss.
type palette struct {
	input   textinput.Model
	entries []viewEntry
	matches []int // indices into entries that match the current query
	cursor  int
}

func newPalette(entries []viewEntry) palette {
	ti := textinput.New()
	ti.Placeholder = "jump to view…"
	ti.Prompt = ": "
	ti.Focus()
	p := palette{input: ti, entries: entries}
	p.recompute()
	return p
}

// recompute refreshes the match list for the current query and clamps cursor.
func (p *palette) recompute() {
	p.matches = matchEntries(p.entries, strings.TrimSpace(p.input.Value()))
	if p.cursor >= len(p.matches) {
		p.cursor = len(p.matches) - 1
	}
	if p.cursor < 0 {
		p.cursor = 0
	}
}

// Update folds one keystroke. enter emits jumpMsg for the highlighted match;
// esc emits paletteCloseMsg; everything else edits the query.
func (p palette) Update(msg tea.KeyMsg) (palette, tea.Cmd) {
	switch msg.String() {
	case "esc":
		return p, func() tea.Msg { return paletteCloseMsg{} }
	case "enter":
		if p.cursor >= 0 && p.cursor < len(p.matches) {
			idx := p.matches[p.cursor]
			return p, func() tea.Msg { return jumpMsg{index: idx} }
		}
		return p, nil
	case "up", "ctrl+k":
		if p.cursor > 0 {
			p.cursor--
		}
		return p, nil
	case "down", "ctrl+j":
		if p.cursor < len(p.matches)-1 {
			p.cursor++
		}
		return p, nil
	}
	var cmd tea.Cmd
	p.input, cmd = p.input.Update(msg)
	p.recompute()
	return p, cmd
}

// View renders the input line plus the filtered match list.
func (p palette) View() string {
	var b strings.Builder
	b.WriteString(styles.TileValue.Render("Command palette"))
	b.WriteString("\n")
	b.WriteString(p.input.View())
	b.WriteString("\n")
	if len(p.matches) == 0 {
		b.WriteString(styles.TileLabel.Render("  (no matching view)"))
		return styles.Panel.Render(b.String())
	}
	for i, idx := range p.matches {
		prefix := "  "
		name := p.entries[idx].name
		if i == p.cursor {
			prefix = lipgloss.NewStyle().Foreground(styles.Brand).Render("▸ ")
			name = lipgloss.NewStyle().Bold(true).Render(name)
		}
		b.WriteString(prefix + name + "\n")
	}
	return styles.Panel.Render(strings.TrimRight(b.String(), "\n"))
}

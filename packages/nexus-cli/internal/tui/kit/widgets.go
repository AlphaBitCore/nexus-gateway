package kit

import (
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/styles"
)

// Clip truncates s to at most n runes, appending an ellipsis when it overflows
// (rune-aware so a multibyte string is never cut mid-codepoint).
func Clip(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// WrapText soft-wraps s to width columns so long LLM responses (event explain,
// conversation replies) read as a paragraph instead of running off the right
// edge. Uses lipgloss so wrapping is rune-width aware; width<=0 is a no-op.
func WrapText(s string, width int) string {
	if width <= 0 {
		return s
	}
	return lipgloss.NewStyle().Width(width).Render(s)
}

// RenderCursorList renders a scrolling window of n rows around cursor within a
// height budget, marking the cursor row with a brand arrow + bold. row(i) renders
// row i. Shared by the wizard pickers and any cursor-driven list.
func RenderCursorList(budget, cursor, n int, row func(i int) string) string {
	if budget < 3 {
		budget = 3
	}
	start := 0
	if cursor >= budget {
		start = cursor - budget + 1
	}
	end := start + budget
	if end > n {
		end = n
	}
	var lines []string
	for i := start; i < end; i++ {
		prefix := "  "
		line := row(i)
		if i == cursor {
			prefix = lipgloss.NewStyle().Foreground(styles.Brand).Render("▸ ")
			line = lipgloss.NewStyle().Bold(true).Render(line)
		}
		lines = append(lines, prefix+line)
	}
	return strings.Join(lines, "\n")
}

// Package styles holds the one shared Lip Gloss palette + styles for the TUI,
// so every view renders from a single source of truth (no scattered hex).
// Accent is the AlphaBitCore brand blue, matching the web Control Plane theme.
package styles

import "github.com/charmbracelet/lipgloss"

// Palette. Brand accent is #3b518a (institutional blue). Text/sub adapt to the
// terminal background so the UI is legible in light and dark themes.
var (
	Brand   = lipgloss.Color("#3b518a")
	BrandHi = lipgloss.Color("#5a73c0")
	Green   = lipgloss.Color("#2e9e5b")
	Amber   = lipgloss.Color("#d39e00")
	Red     = lipgloss.Color("#c0392b")

	Text = lipgloss.AdaptiveColor{Light: "#1a1a2e", Dark: "#e6e9f0"}
	Sub  = lipgloss.AdaptiveColor{Light: "#5b6172", Dark: "#9aa2b1"}
	Line = lipgloss.AdaptiveColor{Light: "#c8cdda", Dark: "#3a4151"}
)

// Shared styles.
var (
	// StatusBar is the top bar; ProdBanner overrides it for prod environments.
	StatusBar  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#ffffff")).Background(Brand).Padding(0, 1)
	ProdBanner = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#ffffff")).Background(Red).Padding(0, 1)

	// Tabs.
	Tab       = lipgloss.NewStyle().Foreground(Sub).Padding(0, 1)
	ActiveTab = lipgloss.NewStyle().Bold(true).Foreground(Brand).Underline(true).Padding(0, 1)

	// Tile is a bordered "card" for a big-number metric.
	Tile      = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(Line).Padding(0, 1).Margin(0, 1, 0, 0)
	TileLabel = lipgloss.NewStyle().Foreground(Sub)
	TileValue = lipgloss.NewStyle().Bold(true).Foreground(Text)

	// HelpBar is the bottom keybinding bar.
	HelpBar = lipgloss.NewStyle().Foreground(Sub).Padding(0, 1)

	// Panel wraps a scrollable region (event bodies, etc.).
	Panel = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(Line).Padding(0, 1)
)

// StatusColor maps an HTTP status code to a semantic color (RAG).
func StatusColor(code int) lipgloss.Color {
	switch {
	case code >= 500:
		return Red
	case code >= 400:
		return Amber
	case code >= 200 && code < 300:
		return Green
	default:
		return Amber
	}
}

// DeltaColor returns green for non-negative deltas, red for negative.
func DeltaColor(delta float64) lipgloss.Color {
	if delta < 0 {
		return Red
	}
	return Green
}

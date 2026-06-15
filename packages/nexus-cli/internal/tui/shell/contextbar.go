package shell

import (
	"fmt"
	"image/color"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/agent"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/kit"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/styles"
)

// contextbar.go renders the conversation's context-usage indicator from the
// per-turn agent.ContextStats + the model's context window. The always-on bar
// (contextBar) is a fullness gauge coloured by how close the used tokens are to the
// window; the on-demand panel (contextPanel, toggled by /context) breaks the used
// tokens down by component. Totals (used / window / cache) are exact; the
// per-component split is an estimate, calibrated to the exact used total.

const (
	ctxBarCells  = 12
	ctxAmberFrac = 0.70 // window fullness past which the gauge turns amber
	ctxRedFrac   = 0.90 // ...and red (near the model's hard limit)
)

// contextBar is the always-on one-line indicator for the footer. It is empty when
// there is neither usage nor a known window, "ctx –/<window>" before the first
// turn, and the gauge + used/window/% + cache-hit once a turn has reported usage.
func contextBar(s agent.ContextStats, window int) string {
	if s.Used <= 0 {
		if window > 0 {
			return styles.TileLabel.Render("ctx –/" + kit.Ktok(window))
		}
		return ""
	}
	used := kit.Ktok(s.Used)
	if window <= 0 { // window unknown — show the absolute used + cache only
		return styles.TileLabel.Render("ctx "+used) + ctxCacheSuffix(s)
	}
	frac := float64(s.Used) / float64(window)
	if frac > 1 {
		frac = 1
	}
	pct := int(frac*100 + 0.5)
	warn := ""
	if frac >= ctxRedFrac {
		warn = lipgloss.NewStyle().Foreground(styles.Red).Render(" ⚠")
	}
	return styles.TileLabel.Render("ctx ") + ctxFillBar(frac, ctxBarCells) +
		styles.TileLabel.Render(fmt.Sprintf(" %s/%s %d%%", used, kit.Ktok(window), pct)) + warn + ctxCacheSuffix(s)
}

// cachePct is the cache-hit percentage of the prompt, clamped to [0,100]. Cached prompt
// tokens are a subset of Used (prompt_tokens) in the gateway's OpenAI-normalized usage,
// so the ratio is ≤100%; the clamp guards the display against any upstream accounting
// quirk that reports cached larger than the prompt (which would otherwise show >100%).
func cachePct(s agent.ContextStats) int {
	if s.Used <= 0 || s.Cached <= 0 {
		return 0
	}
	if p := s.Cached * 100 / s.Used; p < 100 {
		return p
	}
	return 100
}

// ctxCacheSuffix renders " · cache NN%" when any of the prompt was a cache hit.
func ctxCacheSuffix(s agent.ContextStats) string {
	p := cachePct(s)
	if p <= 0 {
		return ""
	}
	return styles.TileLabel.Render(fmt.Sprintf(" · cache %d%%", p))
}

// ctxFillBar renders a fixed-width fill bar; the filled portion is coloured by
// urgency (green → amber → red), the empty portion dim.
func ctxFillBar(frac float64, cells int) string {
	if cells < 1 {
		cells = 1
	}
	filled := int(frac*float64(cells) + 0.5)
	if filled > cells {
		filled = cells
	}
	if filled < 0 {
		filled = 0
	}
	full := lipgloss.NewStyle().Foreground(ctxColor(frac)).Render(strings.Repeat("█", filled))
	empty := lipgloss.NewStyle().Foreground(styles.Line).Render(strings.Repeat("░", cells-filled))
	return styles.TileLabel.Render("▕") + full + empty + styles.TileLabel.Render("▏")
}

func ctxColor(frac float64) color.Color {
	switch {
	case frac >= ctxRedFrac:
		return styles.Red
	case frac >= ctxAmberFrac:
		return styles.Amber
	default:
		return styles.Green
	}
}

// contextPanel is the on-demand breakdown: the exact used/window/cache, the
// per-component estimate with mini bars, the compaction progress, and a note that
// the split is estimated. model is the current model slug (may be empty).
func contextPanel(s agent.ContextStats, window int, model string) string {
	var b strings.Builder
	title := "Context"
	if model != "" {
		title += " · " + model
	}
	b.WriteString(styles.TileValue.Render(title) + "\n")
	if s.Used <= 0 {
		line := "no usage yet — send a message to populate"
		if window > 0 {
			line += "  (window " + kit.Ktok(window) + ")"
		}
		b.WriteString(styles.TileLabel.Render(line))
		return b.String()
	}
	win, pct := "?", 0
	if window > 0 {
		win = kit.Ktok(window)
		pct = int(float64(s.Used)/float64(window)*100 + 0.5)
	}
	cache := ""
	if p := cachePct(s); p > 0 {
		cache = fmt.Sprintf("   cache hit %d%%", p)
	}
	b.WriteString(styles.TileValue.Render(fmt.Sprintf("used   %s / %s  (%d%%)", kit.Ktok(s.Used), win, pct)) +
		styles.TileLabel.Render(cache) + "\n")

	for _, r := range []struct {
		label, note string
		v           int
	}{
		{"~system", "fixed", s.System},
		{"~tools", "fixed", s.Tools},
		{"~history", "← /clear to reset", s.History},
		{"~bundle", "situation + view + memory", s.Bundle},
	} {
		b.WriteString(fmt.Sprintf("%-9s %6s  %s  %s\n",
			r.label, kit.Ktok(r.v), ctxMiniBar(r.v, s.Used, 10), styles.TileLabel.Render(r.note)))
	}

	if s.CompactBudget > 0 {
		line := fmt.Sprintf("auto-trim when the model view exceeds ~%s tokens (now ~%s, %d msgs)",
			kit.Ktok(s.CompactBudget), kit.Ktok(s.History), s.Messages)
		if s.History >= s.CompactBudget {
			line = fmt.Sprintf("auto-trim active — model view ~%s tokens (%d msgs)", kit.Ktok(s.History), s.Messages)
		}
		b.WriteString(styles.TileLabel.Render(line) + "\n")
	}
	b.WriteString(styles.TileLabel.Render("totals exact · the ~split is an estimate"))
	return b.String()
}

// ctxMiniBar renders a small proportional bar for one component's share of total.
func ctxMiniBar(v, total, cells int) string {
	if total <= 0 || cells < 1 {
		return ""
	}
	filled := v * cells / total
	if filled > cells {
		filled = cells
	}
	if filled < 0 {
		filled = 0
	}
	return styles.TileLabel.Render("▕" + strings.Repeat("█", filled) + strings.Repeat(" ", cells-filled) + "▏")
}

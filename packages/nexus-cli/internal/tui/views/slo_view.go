package views

import (
	"fmt"
	"strings"

	"image/color"

	"charm.land/lipgloss/v2"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/kit"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/styles"
)

func (s *slo) View(width, height int) string {
	if s.inDetail {
		return s.detailView()
	}
	if s.loading && s.phases == nil {
		return styles.TileLabel.Render("loading SLO…")
	}
	var b strings.Builder
	if s.err != nil {
		b.WriteString(lipgloss.NewStyle().Foreground(styles.Red).Render("⚠ " + s.err.Error()))
		b.WriteString("\n")
		if s.phases == nil {
			return b.String()
		}
		b.WriteString(styles.TileLabel.Render("(showing last-good data)\n"))
	}
	b.WriteString(s.availabilityLine())
	b.WriteString("\n\n")
	b.WriteString(s.providerTable())
	b.WriteString("\n\n")
	b.WriteString(s.fallbackPanel())
	return b.String()
}

// detailView renders one provider's SLO detail: the friendly heading, the
// ProviderDetail summary (availability + cache + cost), and the latency
// percentiles of the row the operator drilled from.
func (s *slo) detailView() string {
	if s.cf.Capturing() {
		return s.cf.View()
	}
	var b strings.Builder
	b.WriteString(styles.TileValue.Render("Provider detail · " + s.friendlyName(s.detailRow)))
	if s.detailResolved {
		state := lipgloss.NewStyle().Foreground(styles.Green).Render("enabled")
		if !s.detailProvider.Enabled {
			state = lipgloss.NewStyle().Foreground(styles.Red).Render("disabled")
		}
		b.WriteString("  " + state + styles.TileLabel.Render("  (t: toggle · esc: back)"))
	} else {
		b.WriteString(styles.TileLabel.Render("   (esc: back)"))
	}
	b.WriteString("\n")
	if !s.detailResolved {
		b.WriteString(styles.TileLabel.Render("(no catalog match — availability detail unavailable)"))
		b.WriteString("\n")
	}
	if s.writeErr != nil {
		b.WriteString(lipgloss.NewStyle().Foreground(styles.Red).Render("⚠ " + s.writeErr.Error()))
		b.WriteString("\n")
	} else if s.writeNote != "" {
		b.WriteString(lipgloss.NewStyle().Foreground(styles.Green).Render("✓ " + s.writeNote))
		b.WriteString("\n")
	}
	if s.detailErr != nil {
		b.WriteString(lipgloss.NewStyle().Foreground(styles.Red).Render("⚠ " + s.detailErr.Error()))
		b.WriteString("\n")
	}
	if s.detailLoading && s.detail == nil {
		b.WriteString(styles.TileLabel.Render("loading provider detail…"))
		return b.String()
	}
	if s.detail != nil {
		sum := s.detail.Summary
		errColor := styles.Green
		switch {
		case sum.ErrorRate >= 0.05:
			errColor = styles.Red
		case sum.ErrorRate >= 0.01:
			errColor = styles.Amber
		}
		tiles := []string{
			kit.Tile("Requests", fmt.Sprintf("%d", sum.TotalRequests)),
			kit.Tile("Errors", fmt.Sprintf("%d", sum.ErrorCount)),
			kit.Tile("Error rate", lipgloss.NewStyle().Foreground(errColor).Render(fmt.Sprintf("%.2f%%", sum.ErrorRate*100))),
			kit.Tile("Cache hit", fmt.Sprintf("%.1f%%", sum.CacheHitRate*100)),
			kit.Tile("Avg latency", kit.Ms(int(sum.AvgLatencyMs))),
			kit.Tile("Avg TTFB", kit.Ms(int(sum.AvgUpstreamTTFBMs))),
			kit.Tile("Cost", fmt.Sprintf("$%.4f", sum.TotalEstCostUSD)),
		}
		b.WriteString(lipgloss.JoinHorizontal(lipgloss.Top, tiles...))
		b.WriteString("\n\n")
	}
	r := s.detailRow
	b.WriteString(styles.TileValue.Render("Latency percentiles (selected window)"))
	b.WriteString("\n")
	b.WriteString(lipgloss.NewStyle().Foreground(styles.Sub).Render(
		fmt.Sprintf("  %-10s %-10s %-10s %-12s %-12s", "p50", "p95", "p99", "TTFB p95", "UPSTREAM p95")))
	b.WriteString("\n")
	p95 := lipgloss.NewStyle().Foreground(sloLatencyColor(r.TotalP95Ms)).Render(fmt.Sprintf("%-10s", kit.Ms(r.TotalP95Ms)))
	b.WriteString(fmt.Sprintf("  %-10s %s %-10s %-12s %-12s",
		kit.Ms(r.TotalP50Ms), p95, kit.Ms(r.TotalP99Ms), kit.Ms(r.UpstreamTTFBP95Ms), kit.Ms(r.UpstreamTotalP95Ms)))
	return b.String()
}

// availabilityLine summarizes overall request volume + error rate from the
// sparkline summary (the only source with 4xx/5xx counts).
func (s *slo) availabilityLine() string {
	if s.sp == nil {
		return styles.TileLabel.Render("availability: (no metrics)")
	}
	sm := s.sp.Totals()
	reqs := sm[core.MetricRequestCount]
	errs := sm[core.MetricStatus4xxCount] + sm[core.MetricStatus5xxCount]
	rate := 0.0
	if reqs > 0 {
		rate = errs / reqs * 100
	}
	avail := 100 - rate
	color := styles.Green
	switch {
	case avail < 95:
		color = styles.Red
	case avail < 99:
		color = styles.Amber
	}
	badge := lipgloss.NewStyle().Bold(true).Foreground(color).Render(fmt.Sprintf("%.2f%%", avail))
	return styles.TileValue.Render("Availability ") + badge +
		styles.TileLabel.Render(fmt.Sprintf("   requests %.0f   errors %.0f", reqs, errs))
}

// providerTable renders per-provider latency percentiles, RAG-colored by p95.
// Rows show the friendly provider name; the selected row carries the cursor and
// enter drills into it.
func (s *slo) providerTable() string {
	var b strings.Builder
	b.WriteString(styles.TileValue.Render("Per-provider latency (7d)"))
	b.WriteString("\n")
	b.WriteString(lipgloss.NewStyle().Foreground(styles.Sub).Render(
		fmt.Sprintf("  %-18s %8s %10s %10s %10s", "PROVIDER", "REQS", "p50", "p95", "TTFB p95")))
	b.WriteString("\n")
	if s.phases == nil || len(s.phases.Rows) == 0 {
		b.WriteString(styles.TileLabel.Render("  (no data)"))
		return b.String()
	}
	var lines []string
	for i, r := range s.phases.Rows {
		label := kit.Clip(s.friendlyName(r), 18)
		p95 := lipgloss.NewStyle().Foreground(sloLatencyColor(r.TotalP95Ms)).Render(fmt.Sprintf("%8s", kit.Ms(r.TotalP95Ms)))
		cursor := "  "
		if i == s.cursor {
			cursor = lipgloss.NewStyle().Foreground(styles.Brand).Render("▸ ")
		}
		line := fmt.Sprintf("%-18s %8d %10s %s %10s",
			label, r.RequestCount, kit.Ms(r.TotalP50Ms), p95, kit.Ms(r.UpstreamTTFBP95Ms))
		if i == s.cursor {
			line = lipgloss.NewStyle().Bold(true).Render(line)
		}
		lines = append(lines, cursor+line)
	}
	b.WriteString(strings.Join(lines, "\n"))
	return b.String()
}

// fallbackPanel lists routing-fallback activity.
func (s *slo) fallbackPanel() string {
	var b strings.Builder
	b.WriteString(styles.TileValue.Render("Routing fallbacks"))
	b.WriteString("\n")
	if s.fallbacks == nil || len(s.fallbacks.Data) == 0 {
		b.WriteString(styles.TileLabel.Render("  (none)"))
		return b.String()
	}
	var lines []string
	for _, f := range s.fallbacks.Data {
		label := f.GroupLabel
		if label == "" {
			label = f.Group
		}
		lines = append(lines, fmt.Sprintf("  %-30s %6d", label, f.RequestCount))
	}
	b.WriteString(strings.Join(lines, "\n"))
	return b.String()
}

// sloLatencyColor RAG-grades a p95 latency in ms (chat workloads run seconds).
func sloLatencyColor(p95ms int) color.Color {
	switch {
	case p95ms >= 30000:
		return styles.Red
	case p95ms >= 8000:
		return styles.Amber
	default:
		return styles.Green
	}
}

// ms renders a millisecond count compactly (e.g. 1.2s above 1000ms).

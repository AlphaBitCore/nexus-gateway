package tui

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/styles"
)

// slo is the Performance/SLO view: per-provider latency percentiles (RAG by
// p95), overall error rate, and routing-fallback activity. Selecting a provider
// row (enter) drills into that provider's detail (availability + cache + cost).
//
// The latency-phases endpoint groups by the provider *name* (e.g. "openai"); the
// detail endpoint keys on the provider UUID. The view resolves name → UUID via
// the providers catalog so the drill is correct, and renders the friendly
// DisplayName ("OpenAI") — the UUID is never shown to the operator.
type slo struct {
	gw        Gateway
	phases    *core.LatencyPhasesResult
	fallbacks *core.FallbacksResult
	sp        *core.SparklineResult
	providers map[string]core.Provider // keyed by Provider.Name (= phase groupKey)
	err       error
	loading   bool

	cursor int // selected provider row in list mode

	// Detail-drill state. inDetail switches the view to one provider's detail;
	// detailRow is the list row we drilled from (its percentiles are shown).
	// detailProvider is the resolved catalog provider (zero if the row's name
	// has no catalog match, in which case detail is unavailable).
	inDetail       bool
	detailRow      core.LatencyPhaseRow
	detailProvider core.Provider
	detailResolved bool
	detail         *core.ProviderDetail
	detailErr      error
	detailLoading  bool

	cf        confirm // prod-gated provider enable/disable
	writeNote string
	writeErr  error
}

type sloMsg struct {
	phases    *core.LatencyPhasesResult
	fallbacks *core.FallbacksResult
	sp        *core.SparklineResult
	providers []core.Provider
	err       error
}
type sloTick struct{}

// providerDetailMsg carries the result of a ProviderDetail drill fetch. key is
// the provider UUID the fetch was issued for; it ties the result back to the
// row that requested it so a stale fetch (the operator backed out and drilled
// elsewhere) is ignored.
type providerDetailMsg struct {
	key    string
	detail *core.ProviderDetail
	err    error
}

// providerWriteMsg carries the result of an enable/disable provider write.
type providerWriteMsg struct {
	enabled bool
	err     error
}

// newSLO builds the SLO view. The session (optional; the dashboard always
// passes it) drives the prod confirmation on the provider enable/disable write.
func newSLO(gw Gateway, s ...Session) *slo {
	return &slo{gw: gw, loading: true, cf: newConfirm(optSession(s))}
}

// optSession returns the first session or a zero (non-prod) one.
func optSession(s []Session) Session {
	if len(s) > 0 {
		return s[0]
	}
	return Session{}
}

func (s *slo) Init() tea.Cmd { return s.fetch() }

// sloWindow returns the [7d ago, now] window the analytics endpoints require.
func sloWindow() url.Values {
	now := time.Now().UTC()
	return url.Values{
		"start": {now.AddDate(0, 0, -7).Format(time.RFC3339)},
		"end":   {now.Format(time.RFC3339)},
	}
}

func (s *slo) fetch() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := fetchCtx()
		defer cancel()
		phases, err := s.gw.LatencyPhases(ctx, "provider", sloWindow())
		if err != nil {
			return sloMsg{err: err}
		}
		fb, err := s.gw.RoutingFallbacks(ctx, sloWindow())
		if err != nil {
			return sloMsg{phases: phases, err: err}
		}
		sp, spErr := s.gw.Sparkline(ctx, nil)
		// The provider catalog only enriches labels and enables the drill, so its
		// failure must not blank the SLO view — fetch it best-effort.
		var provs []core.Provider
		if pr, perr := s.gw.Providers(ctx); perr == nil && pr != nil {
			provs = pr.Data
		}
		return sloMsg{phases: phases, fallbacks: fb, sp: sp, providers: provs, err: spErr}
	}
}

// fetchDetail loads one provider's SLO detail for the drill panel, keyed by UUID.
func (s *slo) fetchDetail(uuid string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := fetchCtx()
		defer cancel()
		d, err := s.gw.ProviderDetail(ctx, uuid, sloWindow())
		return providerDetailMsg{key: uuid, detail: d, err: err}
	}
}

func (s *slo) Update(msg tea.Msg) (viewModel, tea.Cmd) {
	switch msg := msg.(type) {
	case sloMsg:
		s.loading = false
		s.err = msg.err
		if msg.phases != nil {
			s.phases = msg.phases
		}
		if msg.fallbacks != nil {
			s.fallbacks = msg.fallbacks
		}
		if msg.sp != nil {
			s.sp = msg.sp
		}
		if msg.providers != nil {
			m := make(map[string]core.Provider, len(msg.providers))
			for _, p := range msg.providers {
				m[p.Name] = p
			}
			s.providers = m
		}
		s.clampCursor()
		return s, tick(pollSlow, sloTick{})
	case sloTick:
		return s, s.fetch()
	case providerDetailMsg:
		// Apply only if still viewing the provider that requested it.
		if s.inDetail && msg.key == s.detailProvider.ID {
			s.detailLoading = false
			s.detail = msg.detail
			s.detailErr = msg.err
		}
		return s, nil
	case providerWriteMsg:
		s.writeErr = msg.err
		if msg.err == nil {
			s.detailProvider.Enabled = msg.enabled // reflect the new state locally
			state := "disabled"
			if msg.enabled {
				state = "enabled"
			}
			s.writeNote = "provider " + state
		}
		return s, nil
	case tea.KeyMsg:
		if s.inDetail {
			if handled, cmd := s.cf.update(msg); handled {
				return s, cmd
			}
			switch msg.String() {
			case "esc", "backspace":
				s.inDetail = false
				s.detail = nil
				s.detailErr = nil
			case "t":
				if cmd := s.toggleProvider(); cmd != nil {
					return s, cmd
				}
			}
			return s, nil
		}
		switch msg.String() {
		case "up", "k":
			if s.cursor > 0 {
				s.cursor--
			}
		case "down", "j":
			if s.cursor < s.rowCount()-1 {
				s.cursor++
			}
		case "enter":
			if row, ok := s.selectedRow(); ok {
				return s, s.drill(row)
			}
		}
	}
	return s, nil
}

// drill enters the detail panel for row. If the row's provider name resolves to
// a catalog UUID, it fetches the detail; otherwise the panel shows that detail
// is unavailable (the percentiles we already have are still rendered).
func (s *slo) drill(row core.LatencyPhaseRow) tea.Cmd {
	s.inDetail = true
	s.detailRow = row
	s.detail = nil
	s.detailErr = nil
	p, found := s.providers[row.GroupKey]
	s.detailProvider = p
	s.detailResolved = found
	if !found {
		s.detailLoading = false
		return nil
	}
	s.detailLoading = true
	return s.fetchDetail(p.ID)
}

func (s *slo) rowCount() int {
	if s.phases == nil {
		return 0
	}
	return len(s.phases.Rows)
}

func (s *slo) clampCursor() {
	if s.cursor >= s.rowCount() {
		s.cursor = s.rowCount() - 1
	}
	if s.cursor < 0 {
		s.cursor = 0
	}
}

func (s *slo) selectedRow() (core.LatencyPhaseRow, bool) {
	if s.cursor < 0 || s.cursor >= s.rowCount() {
		return core.LatencyPhaseRow{}, false
	}
	return s.phases.Rows[s.cursor], true
}

// friendlyName returns the human label for a provider row, preferring the
// catalog DisplayName, then the catalog Name, then the phase group label/key.
// It never returns a UUID.
func (s *slo) friendlyName(r core.LatencyPhaseRow) string {
	if p, ok := s.providers[r.GroupKey]; ok {
		if p.DisplayName != "" {
			return p.DisplayName
		}
		if p.Name != "" {
			return p.Name
		}
	}
	if r.GroupLabel != "" {
		return r.GroupLabel
	}
	return r.GroupKey
}

// toggleProvider begins a prod-gated enable/disable of the drilled provider.
// Returns nil when the row has no catalog match (no provider id to act on).
func (s *slo) toggleProvider() tea.Cmd {
	if !s.detailResolved {
		return nil
	}
	p := s.detailProvider
	target := !p.Enabled
	verb := "disable"
	if target {
		verb = "enable"
	}
	s.writeNote = ""
	s.writeErr = nil
	return s.cf.begin(verb+" provider "+s.friendlyName(s.detailRow), func() tea.Cmd {
		return func() tea.Msg {
			ctx, cancel := fetchCtx()
			defer cancel()
			return providerWriteMsg{enabled: target, err: s.gw.SetProviderEnabled(ctx, p.ID, target)}
		}
	})
}

// capturing suspends the root's single-letter shortcuts while the prod
// confirmation field is focused.
func (s *slo) capturing() bool { return s.cf.capturing() }

// help is mode-aware so the drill's esc-to-return + provider toggle are
// discoverable.
func (s *slo) help() string {
	if s.cf.capturing() {
		return s.cf.helpHint()
	}
	if s.inDetail {
		return "t enable/disable provider · esc back · tab/1-9 switch · q quit"
	}
	return "↑/↓ select · enter provider detail · tab/1-9 switch · : palette · q quit"
}

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
	if s.cf.capturing() {
		return s.cf.view()
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
			tile("Requests", fmt.Sprintf("%d", sum.TotalRequests)),
			tile("Errors", fmt.Sprintf("%d", sum.ErrorCount)),
			tile("Error rate", lipgloss.NewStyle().Foreground(errColor).Render(fmt.Sprintf("%.2f%%", sum.ErrorRate*100))),
			tile("Cache hit", fmt.Sprintf("%.1f%%", sum.CacheHitRate*100)),
			tile("Avg latency", ms(int(sum.AvgLatencyMs))),
			tile("Avg TTFB", ms(int(sum.AvgUpstreamTTFBMs))),
			tile("Cost", fmt.Sprintf("$%.4f", sum.TotalEstCostUSD)),
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
	p95 := lipgloss.NewStyle().Foreground(sloLatencyColor(r.TotalP95Ms)).Render(fmt.Sprintf("%-10s", ms(r.TotalP95Ms)))
	b.WriteString(fmt.Sprintf("  %-10s %s %-10s %-12s %-12s",
		ms(r.TotalP50Ms), p95, ms(r.TotalP99Ms), ms(r.UpstreamTTFBP95Ms), ms(r.UpstreamTotalP95Ms)))
	return b.String()
}

// availabilityLine summarizes overall request volume + error rate from the
// sparkline summary (the only source with 4xx/5xx counts).
func (s *slo) availabilityLine() string {
	if s.sp == nil {
		return styles.TileLabel.Render("availability: (no metrics)")
	}
	sm := s.sp.Totals()
	reqs := sm[mRequests]
	errs := sm[m4xx] + sm[m5xx]
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
		label := clip(s.friendlyName(r), 18)
		p95 := lipgloss.NewStyle().Foreground(sloLatencyColor(r.TotalP95Ms)).Render(fmt.Sprintf("%8s", ms(r.TotalP95Ms)))
		cursor := "  "
		if i == s.cursor {
			cursor = lipgloss.NewStyle().Foreground(styles.Brand).Render("▸ ")
		}
		line := fmt.Sprintf("%-18s %8d %10s %s %10s",
			label, r.RequestCount, ms(r.TotalP50Ms), p95, ms(r.UpstreamTTFBP95Ms))
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
func sloLatencyColor(p95ms int) lipgloss.Color {
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
func ms(v int) string {
	if v >= 1000 {
		return fmt.Sprintf("%.1fs", float64(v)/1000)
	}
	return fmt.Sprintf("%dms", v)
}

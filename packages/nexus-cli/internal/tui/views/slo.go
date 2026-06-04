package views

import (
	"net/url"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/kit"
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
	gw        kit.Gateway
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

	cf        kit.Confirm // prod-gated provider enable/disable
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
func newSLO(gw kit.Gateway, s ...kit.Session) *slo {
	return &slo{gw: gw, loading: true, cf: kit.NewConfirm(kit.OptSession(s))}
}

// optSession returns the first session or a zero (non-prod) one.
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
		ctx, cancel := kit.FetchCtx()
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
		ctx, cancel := kit.FetchCtx()
		defer cancel()
		d, err := s.gw.ProviderDetail(ctx, uuid, sloWindow())
		return providerDetailMsg{key: uuid, detail: d, err: err}
	}
}

func (s *slo) Update(msg tea.Msg) (kit.ViewModel, tea.Cmd) {
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
		return s, kit.Tick(kit.PollSlow, sloTick{})
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
	case tea.KeyPressMsg:
		if s.inDetail {
			if handled, cmd := s.cf.Update(msg); handled {
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
	return s.cf.Begin(verb+" provider "+s.friendlyName(s.detailRow), func() tea.Cmd {
		return func() tea.Msg {
			ctx, cancel := kit.FetchCtx()
			defer cancel()
			return providerWriteMsg{enabled: target, err: s.gw.SetProviderEnabled(ctx, p.ID, target)}
		}
	})
}

// capturing suspends the root's single-letter shortcuts while the prod
// confirmation field is focused.
func (s *slo) Capturing() bool { return s.cf.Capturing() }

// back closes the provider detail panel so `esc` returns to the SLO list before
// the root pops the nav stack. Returns false at the list level (let the root walk
// up to the cockpit).
func (s *slo) Back() bool {
	if s.inDetail {
		s.inDetail = false
		s.detail = nil
		s.detailErr = nil
		return true
	}
	return false
}

// help is mode-aware so the drill's esc-to-return + provider toggle are
// discoverable.
func (s *slo) Help() string {
	if s.cf.Capturing() {
		return s.cf.HelpHint()
	}
	if s.inDetail {
		return "t enable/disable provider · ←/esc back · 1-9 jump · tab chat · q quit"
	}
	return "↑/↓ select · enter provider detail · 1-9 jump · / commands · tab chat · q quit"
}

package views

import (
	"bytes"
	"fmt"
	"github.com/goccy/go-json"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/kit"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/styles"
)

// routing is the Routing Rules view: the configured rules in priority order with
// their strategy, pipeline stage, and enabled state, plus one prod-gated
// mitigation write — toggle enabled (t). Disabling a misbehaving rule (or
// enabling a prepared failover) is a one-keystroke incident action; the rule's
// config blobs are intentionally not surfaced (edit those in the CP UI).
type routing struct {
	gw      kit.Gateway
	rules   []core.RoutingRule
	cursor  int
	detail  bool // enter opens a read-only detail drawer for the cursor row
	err     error
	loading bool

	cf        kit.Confirm // prod-gated enable/disable
	writeNote string
	writeErr  error
	busy      bool // a toggle is in flight — suppresses a second t until it lands
}

type routingMsg struct {
	rules []core.RoutingRule
	err   error
}
type routingTick struct{}

// routingWriteMsg carries the result of an enable/disable toggle.
type routingWriteMsg struct {
	enabled bool
	name    string
	err     error
}

// newRouting builds the Routing Rules view. The session (optional; the dashboard
// always passes it) drives the prod confirmation on the toggle write.
func newRouting(gw kit.Gateway, s ...kit.Session) *routing {
	return &routing{gw: gw, loading: true, cf: kit.NewConfirm(kit.OptSession(s))}
}

func (r *routing) Init() tea.Cmd { return r.fetch() }

func (r *routing) fetch() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := kit.FetchCtx()
		defer cancel()
		rules, err := r.gw.RoutingRules(ctx)
		return routingMsg{rules: rules, err: err}
	}
}

// capturing suspends the root's single-letter shortcuts while the prod
// confirmation field is focused.
func (r *routing) Capturing() bool { return r.cf.Capturing() }

func (r *routing) Update(msg tea.Msg) (kit.ViewModel, tea.Cmd) {
	switch msg := msg.(type) {
	case routingMsg:
		r.loading = false
		r.err = msg.err
		if msg.rules != nil {
			r.rules = msg.rules
		}
		r.clampCursor()
		return r, kit.Tick(kit.PollSlow, routingTick{})
	case routingTick:
		return r, r.fetch()
	case routingWriteMsg:
		r.busy = false
		r.writeErr = msg.err
		if msg.err == nil {
			state := "disabled"
			if msg.enabled {
				state = "enabled"
			}
			r.writeNote = state + " rule " + msg.name
			return r, r.fetch() // refresh enabled state after a write
		}
		return r, nil
	case tea.KeyPressMsg:
		if handled, cmd := r.cf.Update(msg); handled {
			return r, cmd
		}
		// The detail drawer is read-only; ←/esc (root → Back()) closes it.
		if r.detail {
			return r, nil
		}
		switch msg.String() {
		case "up", "k":
			if r.cursor > 0 {
				r.cursor--
			}
		case "down", "j":
			if r.cursor < len(r.rules)-1 {
				r.cursor++
			}
		case "enter":
			if _, ok := r.selected(); ok {
				r.detail = true
			}
		case "t":
			if r.busy {
				return r, nil
			}
			return r, r.toggle()
		}
	}
	return r, nil
}

// back closes the detail drawer so ←/esc returns to the list before the root
// pops the nav stack.
func (r *routing) Back() bool {
	if r.detail {
		r.detail = false
		return true
	}
	return false
}

func (r *routing) selected() (core.RoutingRule, bool) {
	if r.cursor < 0 || r.cursor >= len(r.rules) {
		return core.RoutingRule{}, false
	}
	return r.rules[r.cursor], true
}

func (r *routing) clampCursor() {
	if r.cursor >= len(r.rules) {
		r.cursor = len(r.rules) - 1
	}
	if r.cursor < 0 {
		r.cursor = 0
	}
}

// toggle begins a prod-gated enable/disable of the selected rule.
func (r *routing) toggle() tea.Cmd {
	rule, ok := r.selected()
	if !ok {
		return nil
	}
	target := !rule.Enabled
	verb := "disable"
	if target {
		verb = "enable"
	}
	id, name := rule.ID, rule.Name
	r.writeNote, r.writeErr = "", nil
	return r.cf.Begin(verb+" routing rule "+name, func() tea.Cmd {
		r.busy = true
		return func() tea.Msg {
			ctx, cancel := kit.FetchCtx()
			defer cancel()
			return routingWriteMsg{enabled: target, name: name, err: r.gw.SetRoutingRuleEnabled(ctx, id, target)}
		}
	})
}

func (r *routing) Help() string {
	if r.cf.Capturing() {
		return r.cf.HelpHint()
	}
	if r.detail {
		return "←/esc back · q quit"
	}
	return "↑/↓ select · enter open · t enable/disable · 1-9 jump · tab chat · q quit"
}

func (r *routing) View(width, height int) string {
	if r.cf.Capturing() {
		return r.cf.View()
	}
	if r.loading && r.rules == nil {
		return styles.TileLabel.Render("loading routing rules…")
	}
	if r.detail {
		return r.detailView()
	}
	var b strings.Builder
	if r.writeErr != nil {
		b.WriteString(lipgloss.NewStyle().Foreground(styles.Red).Render("⚠ " + r.writeErr.Error()))
		b.WriteString("\n")
	} else if r.writeNote != "" {
		b.WriteString(lipgloss.NewStyle().Foreground(styles.Green).Render("✓ " + r.writeNote))
		b.WriteString("\n")
	}
	if r.err != nil {
		b.WriteString(lipgloss.NewStyle().Foreground(styles.Red).Render("⚠ " + r.err.Error()))
		b.WriteString("\n")
		if r.rules == nil {
			return b.String()
		}
		b.WriteString(styles.TileLabel.Render("(showing last-good data)\n"))
	}
	b.WriteString(r.table())
	return b.String()
}

// detailView renders the full rule, foregrounding what the list omits: the
// description and the three JSON blobs an operator needs to read what the rule
// actually does (strategy config, match predicate, fallback chain) without
// leaving the TUI. The blobs are read-only here — edits still happen in the CP UI.
func (r *routing) detailView() string {
	rule, ok := r.selected()
	if !ok {
		return styles.TileLabel.Render("(no rule selected)")
	}
	var b strings.Builder
	b.WriteString(styles.TileValue.Render("Rule · " + rule.Name))
	b.WriteString("\n\n")
	enabled := lipgloss.NewStyle().Foreground(styles.Green).Render("on")
	if !rule.Enabled {
		enabled = lipgloss.NewStyle().Foreground(styles.Sub).Render("off")
	}
	b.WriteString(kit.DetailRow("Enabled", enabled) + "\n")
	b.WriteString(kit.DetailRow("Strategy", kit.Dash(rule.StrategyType)) + "\n")
	b.WriteString(kit.DetailRow("Priority", fmt.Sprintf("%d", rule.Priority)) + "\n")
	b.WriteString(kit.DetailRow("Pipeline stage", fmt.Sprintf("%d", rule.PipelineStage)) + "\n")
	if rule.Description != "" {
		b.WriteString(kit.DetailRow("Description", rule.Description) + "\n")
	}
	b.WriteString(kit.DetailRow("Created", kit.Dash(rule.CreatedAt)) + "\n")
	b.WriteString(kit.DetailRow("Updated", kit.Dash(rule.UpdatedAt)) + "\n")
	b.WriteString(kit.DetailRow("Rule id", kit.Dash(rule.ID)) + "\n")
	b.WriteString(jsonBlock("Config", rule.Config))
	b.WriteString(jsonBlock("Match conditions", rule.MatchConditions))
	b.WriteString(jsonBlock("Fallback chain", rule.FallbackChain))
	return b.String()
}

// jsonBlock renders a labelled, pretty-printed JSON blob for the detail drawer.
// Empty / null / unparseable blobs collapse to a dim "—" so a sparse rule reads
// cleanly. Returns "" for an entirely absent blob so no empty section is shown.
func jsonBlock(label string, raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	header := "\n" + lipgloss.NewStyle().Foreground(styles.Sub).Render(label) + "\n"
	if s == "" || s == "null" {
		return header + styles.TileLabel.Render("  —") + "\n"
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, []byte(s), "  ", "  "); err != nil {
		return header + styles.TileLabel.Render("  "+s) + "\n" // invalid JSON — show raw
	}
	return header + styles.TileLabel.Render("  "+pretty.String()) + "\n"
}

// table renders the routing rules with the cursor row; enabled is RAG-colored.
func (r *routing) table() string {
	var b strings.Builder
	b.WriteString(styles.TileValue.Render(fmt.Sprintf("Routing rules (%d)", len(r.rules))))
	b.WriteString("\n")
	b.WriteString(lipgloss.NewStyle().Foreground(styles.Sub).Render(
		fmt.Sprintf("  %-28s %-12s %8s %6s %s", "NAME", "STRATEGY", "PRIORITY", "STAGE", "ENABLED")))
	b.WriteString("\n")
	if len(r.rules) == 0 {
		b.WriteString(styles.TileLabel.Render("  (no routing rules)"))
		return b.String()
	}
	var lines []string
	for i, rule := range r.rules {
		enabled := lipgloss.NewStyle().Foreground(styles.Green).Render("on")
		if !rule.Enabled {
			enabled = lipgloss.NewStyle().Foreground(styles.Sub).Render("off")
		}
		cursor := "  "
		if i == r.cursor {
			cursor = lipgloss.NewStyle().Foreground(styles.Brand).Render("▸ ")
		}
		line := fmt.Sprintf("%-28s %-12s %8d %6d %s",
			kit.Clip(rule.Name, 28), kit.Clip(rule.StrategyType, 12), rule.Priority, rule.PipelineStage, enabled)
		if i == r.cursor {
			line = lipgloss.NewStyle().Bold(true).Render(line)
		}
		lines = append(lines, cursor+line)
	}
	b.WriteString(strings.Join(lines, "\n"))
	return b.String()
}

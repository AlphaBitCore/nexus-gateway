package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/styles"
)

// routing is the Routing Rules view: the configured rules in priority order with
// their strategy, pipeline stage, and enabled state, plus one prod-gated
// mitigation write — toggle enabled (t). Disabling a misbehaving rule (or
// enabling a prepared failover) is a one-keystroke incident action; the rule's
// config blobs are intentionally not surfaced (edit those in the CP UI).
type routing struct {
	gw      Gateway
	rules   []core.RoutingRule
	cursor  int
	err     error
	loading bool

	cf        confirm // prod-gated enable/disable
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
func newRouting(gw Gateway, s ...Session) *routing {
	return &routing{gw: gw, loading: true, cf: newConfirm(optSession(s))}
}

func (r *routing) Init() tea.Cmd { return r.fetch() }

func (r *routing) fetch() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := fetchCtx()
		defer cancel()
		rules, err := r.gw.RoutingRules(ctx)
		return routingMsg{rules: rules, err: err}
	}
}

// capturing suspends the root's single-letter shortcuts while the prod
// confirmation field is focused.
func (r *routing) capturing() bool { return r.cf.capturing() }

func (r *routing) Update(msg tea.Msg) (viewModel, tea.Cmd) {
	switch msg := msg.(type) {
	case routingMsg:
		r.loading = false
		r.err = msg.err
		if msg.rules != nil {
			r.rules = msg.rules
		}
		r.clampCursor()
		return r, tick(pollSlow, routingTick{})
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
	case tea.KeyMsg:
		if handled, cmd := r.cf.update(msg); handled {
			return r, cmd
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
		case "t":
			if r.busy {
				return r, nil
			}
			return r, r.toggle()
		}
	}
	return r, nil
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
	return r.cf.begin(verb+" routing rule "+name, func() tea.Cmd {
		r.busy = true
		return func() tea.Msg {
			ctx, cancel := fetchCtx()
			defer cancel()
			return routingWriteMsg{enabled: target, name: name, err: r.gw.SetRoutingRuleEnabled(ctx, id, target)}
		}
	})
}

func (r *routing) help() string {
	if r.cf.capturing() {
		return r.cf.helpHint()
	}
	return "↑/↓ select · t enable/disable · tab/1-9 switch · : palette · q quit"
}

func (r *routing) View(width, height int) string {
	if r.cf.capturing() {
		return r.cf.view()
	}
	if r.loading && r.rules == nil {
		return styles.TileLabel.Render("loading routing rules…")
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
			clip(rule.Name, 28), clip(rule.StrategyType, 12), rule.Priority, rule.PipelineStage, enabled)
		if i == r.cursor {
			line = lipgloss.NewStyle().Bold(true).Render(line)
		}
		lines = append(lines, cursor+line)
	}
	b.WriteString(strings.Join(lines, "\n"))
	return b.String()
}

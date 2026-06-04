package shell

import (
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/kit"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/styles"
)

// slashKind classifies a slash command for grouping + dispatch.
type slashKind int

const (
	slashView  slashKind = iota // open/navigate a resource view (feature command)
	slashAgent                  // an agent control handled by the conversation (/clear, /help)
	slashShell                  // a top-level shell action (/env reopens the wizard's env picker)
)

// slashCmd is one entry in the `/` palette — the vocabulary humans (press `/`) and
// the agent share (design §3).
type slashCmd struct {
	name    string // command token without the slash, e.g. "cost"
	desc    string // one-line description shown in the menu
	kind    slashKind
	aliases []string // extra fuzzy tokens
}

// matches reports a case-insensitive substring match over name + aliases + desc.
// A leading slash on the query is ignored; an empty query matches everything.
func (c slashCmd) matches(q string) bool {
	q = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(q), "/"))
	if q == "" {
		return true
	}
	if strings.Contains(strings.ToLower(c.name), q) {
		return true
	}
	for _, a := range c.aliases {
		if strings.Contains(strings.ToLower(a), q) {
			return true
		}
	}
	return strings.Contains(strings.ToLower(c.desc), q)
}

// defaultSlashCommands is the shared command vocabulary. View commands open a
// resource view (the agent/canvas can react); agent commands are handled by the
// conversation. Keep in lockstep with the view registry in app.go.
func defaultSlashCommands() []slashCmd {
	return []slashCmd{
		{name: "overview", desc: "Mission Control cockpit", kind: slashView, aliases: []string{"home", "cockpit"}},
		{name: "radar", desc: "live traffic waterfall", kind: slashView, aliases: []string{"traffic", "live"}},
		{name: "cost", desc: "spend, burn-rate, top talkers", kind: slashView, aliases: []string{"spend", "$"}},
		{name: "slo", desc: "latency + availability by provider", kind: slashView, aliases: []string{"perf", "latency"}},
		{name: "nodes", desc: "fleet + config-sync drift", kind: slashView, aliases: []string{"fleet", "drift"}},
		{name: "alerts", desc: "firing + recent alerts", kind: slashView, aliases: []string{"firing"}},
		{name: "compliance", desc: "blocks + governance", kind: slashView, aliases: []string{"block"}},
		{name: "jobs", desc: "scheduled jobs", kind: slashView, aliases: []string{"cron"}},
		{name: "sync", desc: "out-of-sync nodes", kind: slashView, aliases: []string{"config-sync"}},
		{name: "models", desc: "model catalog", kind: slashView, aliases: []string{"catalog"}},
		{name: "model", desc: "switch the chat model", kind: slashView, aliases: []string{"llm", "use"}},
		{name: "keys", desc: "virtual keys (revoke/regenerate)", kind: slashView, aliases: []string{"vk"}},
		{name: "rules", desc: "routing rules (toggle)", kind: slashView, aliases: []string{"routing"}},
		{name: "resource", desc: "browse any admin kind (cascade)", kind: slashView, aliases: []string{"res", "kind", "kinds"}},
		{name: "kill", desc: "kill-switch + passthrough", kind: slashView, aliases: []string{"killswitch", "passthrough"}},
		{name: "lab", desc: "request lab + routing dry-run", kind: slashView, aliases: []string{"sim", "simulate"}},
		{name: "event", desc: "open an event by id", kind: slashView, aliases: []string{"drill"}},
		{name: "clear", desc: "clear the conversation", kind: slashAgent},
		{name: "context", desc: "context-usage breakdown", kind: slashAgent, aliases: []string{"ctx"}},
		{name: "compact", desc: "summarize older turns to free context", kind: slashAgent, aliases: []string{"summarize", "shrink"}},
		{name: "help", desc: "key + command help", kind: slashAgent},
		{name: "env", desc: "switch · add · edit · delete the environment", kind: slashShell, aliases: []string{"envs", "environment", "switch"}},
	}
}

// matchSlash returns the commands matching q, preserving registry order.
func matchSlash(cmds []slashCmd, q string) []slashCmd {
	out := make([]slashCmd, 0, len(cmds))
	for _, c := range cmds {
		if c.matches(q) {
			out = append(out, c)
		}
	}
	return out
}

// slashSelectedMsg is emitted when the operator selects a command; arg carries any
// trailing argument (e.g. an event id for /event).
type slashSelectedMsg struct {
	cmd slashCmd
	arg string
}

// slashCloseMsg is emitted when the palette is dismissed (esc).
type slashCloseMsg struct{}

// slashPalette is the `/` command overlay: type to fuzzy-filter on the command
// token, ↑/↓ to move, enter to select (carrying any trailing arg), esc to dismiss.
type slashPalette struct {
	input   textinput.Model
	cmds    []slashCmd
	matches []slashCmd
	cursor  int
}

func newSlashPalette(cmds []slashCmd) slashPalette {
	ti := textinput.New()
	ti.Placeholder = "command…"
	ti.Prompt = "/ "
	ti.Focus()
	p := slashPalette{input: ti, cmds: cmds}
	p.recompute()
	return p
}

// recompute refreshes the match list for the current command token and clamps the
// cursor. It filters on the command token only, so a trailing arg never narrows
// the menu away (typing "event ev-1" still shows the /event row).
func (p *slashPalette) recompute() {
	cmdTok, _ := kit.SplitCmdArg(p.input.Value())
	p.matches = matchSlash(p.cmds, cmdTok)
	if p.cursor >= len(p.matches) {
		p.cursor = len(p.matches) - 1
	}
	if p.cursor < 0 {
		p.cursor = 0
	}
}

// Update folds one keystroke. enter emits slashSelectedMsg for the highlighted
// command with the parsed arg; esc emits slashCloseMsg; else edits the query.
func (p slashPalette) Update(msg tea.KeyPressMsg) (slashPalette, tea.Cmd) {
	switch msg.String() {
	case "esc":
		return p, func() tea.Msg { return slashCloseMsg{} }
	case "enter":
		if p.cursor >= 0 && p.cursor < len(p.matches) {
			cmd := p.matches[p.cursor]
			_, arg := kit.SplitCmdArg(p.input.Value())
			return p, func() tea.Msg { return slashSelectedMsg{cmd: cmd, arg: arg} }
		}
		return p, nil
	case "up":
		if p.cursor > 0 {
			p.cursor--
		}
		return p, nil
	case "down":
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

// View renders the input line plus the filtered command list with descriptions.
func (p slashPalette) View() string {
	var b strings.Builder
	b.WriteString(styles.TileValue.Render("Commands"))
	b.WriteString("\n")
	b.WriteString(p.input.View())
	b.WriteString("\n")
	if len(p.matches) == 0 {
		b.WriteString(styles.TileLabel.Render("  (no matching command)"))
		return styles.Panel.Render(b.String())
	}
	for i, c := range p.matches {
		prefix := "  "
		name := "/" + c.name
		if i == p.cursor {
			prefix = lipgloss.NewStyle().Foreground(styles.Brand).Render("▸ ")
			name = lipgloss.NewStyle().Bold(true).Render(name)
		}
		b.WriteString(prefix + name + styles.TileLabel.Render("  "+c.desc) + "\n")
	}
	return styles.Panel.Render(strings.TrimRight(b.String(), "\n"))
}

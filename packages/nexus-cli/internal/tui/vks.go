package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/styles"
)

// vks is the Virtual Keys view: the deployment's keys with their type, approval
// status, and enabled state, plus two prod-gated mitigation writes — revoke (r)
// and regenerate/rotate-secret (g). Revoke is offered only for keys in "active"
// status (the endpoint 404s otherwise), so the operator never burns a prod
// confirmation on a no-op. A regenerated secret is shown exactly once in a
// dismissible panel — the server keeps only a hash, so it is never persisted.
type vks struct {
	gw      Gateway
	keys    []core.VirtualKey
	cursor  int
	err     error
	loading bool

	cf        confirm // prod-gated revoke / regenerate
	writeNote string
	writeErr  error
	busy      bool // a write is in flight — suppresses a second r/g until it lands

	// regen holds the once-shown plaintext after a successful rotate; regenName
	// is the friendly key name it belongs to. While set, the view shows the
	// secret panel until the operator dismisses it (esc).
	regen     *core.RegeneratedVK
	regenName string
}

type vksMsg struct {
	keys []core.VirtualKey
	err  error
}
type vksTick struct{}

// vkWriteMsg carries a revoke/regenerate result. verb labels the action for the
// note; regen is non-nil only on a successful regenerate.
type vkWriteMsg struct {
	verb  string
	name  string
	regen *core.RegeneratedVK
	err   error
}

// newVKs builds the Virtual Keys view. The session (optional; the dashboard
// always passes it) drives the prod confirmation on the revoke/regenerate writes.
func newVKs(gw Gateway, s ...Session) *vks {
	return &vks{gw: gw, loading: true, cf: newConfirm(optSession(s))}
}

func (v *vks) Init() tea.Cmd { return v.fetch() }

func (v *vks) fetch() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := fetchCtx()
		defer cancel()
		keys, err := v.gw.VirtualKeys(ctx)
		return vksMsg{keys: keys, err: err}
	}
}

// capturing suspends the root's single-letter shortcuts while the prod
// confirmation field is focused.
func (v *vks) capturing() bool { return v.cf.capturing() }

func (v *vks) Update(msg tea.Msg) (viewModel, tea.Cmd) {
	switch msg := msg.(type) {
	case vksMsg:
		v.loading = false
		v.err = msg.err
		if msg.keys != nil {
			v.keys = msg.keys
		}
		v.clampCursor()
		return v, tick(pollSlow, vksTick{})
	case vksTick:
		return v, v.fetch()
	case vkWriteMsg:
		v.busy = false
		v.writeErr = msg.err
		if msg.err == nil {
			v.writeNote = msg.verb + " " + msg.name
			if msg.regen != nil {
				v.regen = msg.regen
				v.regenName = msg.name
			}
			return v, v.fetch() // refresh statuses after a write
		}
		return v, nil
	case tea.KeyMsg:
		// The confirm field owns keystrokes while a prod confirmation is in flight.
		if handled, cmd := v.cf.update(msg); handled {
			return v, cmd
		}
		// A shown secret panel takes over until dismissed.
		if v.regen != nil {
			if msg.String() == "esc" || msg.String() == "enter" {
				v.regen = nil
				v.regenName = ""
			}
			return v, nil
		}
		switch msg.String() {
		case "up", "k":
			if v.cursor > 0 {
				v.cursor--
			}
		case "down", "j":
			if v.cursor < len(v.keys)-1 {
				v.cursor++
			}
		case "r":
			if v.busy {
				return v, nil
			}
			return v, v.revoke()
		case "g":
			if v.busy {
				return v, nil
			}
			return v, v.regenerate()
		}
	}
	return v, nil
}

func (v *vks) selected() (core.VirtualKey, bool) {
	if v.cursor < 0 || v.cursor >= len(v.keys) {
		return core.VirtualKey{}, false
	}
	return v.keys[v.cursor], true
}

func (v *vks) clampCursor() {
	if v.cursor >= len(v.keys) {
		v.cursor = len(v.keys) - 1
	}
	if v.cursor < 0 {
		v.cursor = 0
	}
}

// revoke begins a prod-gated revoke of the selected key. Only "active" keys are
// revocable; for any other status it sets an explanatory note instead of
// starting the confirm (so a prod operator does not type the env name for a 404).
func (v *vks) revoke() tea.Cmd {
	k, ok := v.selected()
	if !ok {
		return nil
	}
	v.writeNote, v.writeErr = "", nil
	if !k.Revocable() {
		v.writeNote = fmt.Sprintf("only active keys can be revoked (%s is %s)", k.Name, k.Status())
		return nil
	}
	id, name := k.ID, k.Name
	return v.cf.begin("revoke virtual key "+name, func() tea.Cmd {
		v.busy = true
		return func() tea.Msg {
			ctx, cancel := fetchCtx()
			defer cancel()
			return vkWriteMsg{verb: "revoked", name: name, err: v.gw.RevokeVK(ctx, id)}
		}
	})
}

// regenerate begins a prod-gated secret rotation of the selected key. The server
// returns the new plaintext once; the view shows it in the dismissible panel.
func (v *vks) regenerate() tea.Cmd {
	k, ok := v.selected()
	if !ok {
		return nil
	}
	v.writeNote, v.writeErr = "", nil
	id, name := k.ID, k.Name
	return v.cf.begin("regenerate (rotate the secret of) virtual key "+name, func() tea.Cmd {
		v.busy = true
		return func() tea.Msg {
			ctx, cancel := fetchCtx()
			defer cancel()
			r, err := v.gw.RegenerateVK(ctx, id)
			return vkWriteMsg{verb: "rotated secret for", name: name, regen: r, err: err}
		}
	})
}

func (v *vks) help() string {
	if v.cf.capturing() {
		return v.cf.helpHint()
	}
	if v.regen != nil {
		return "copy the secret now — shown once · esc/enter dismiss · q quit"
	}
	return "↑/↓ select · r revoke · g regenerate secret · tab/1-9 switch · : palette · q quit"
}

func (v *vks) View(width, height int) string {
	if v.cf.capturing() {
		return v.cf.view()
	}
	if v.regen != nil {
		return v.secretPanel()
	}
	if v.loading && v.keys == nil {
		return styles.TileLabel.Render("loading virtual keys…")
	}
	var b strings.Builder
	if v.writeErr != nil {
		b.WriteString(lipgloss.NewStyle().Foreground(styles.Red).Render("⚠ " + v.writeErr.Error()))
		b.WriteString("\n")
	} else if v.writeNote != "" {
		b.WriteString(lipgloss.NewStyle().Foreground(styles.Green).Render("✓ " + v.writeNote))
		b.WriteString("\n")
	}
	if v.err != nil {
		b.WriteString(lipgloss.NewStyle().Foreground(styles.Red).Render("⚠ " + v.err.Error()))
		b.WriteString("\n")
		if v.keys == nil {
			return b.String()
		}
		b.WriteString(styles.TileLabel.Render("(showing last-good data)\n"))
	}
	b.WriteString(v.table())
	return b.String()
}

// secretPanel renders the once-only regenerated plaintext prominently. It stays
// until the operator dismisses it so the secret can never scroll away unseen.
func (v *vks) secretPanel() string {
	var b strings.Builder
	b.WriteString(lipgloss.NewStyle().Bold(true).Foreground(styles.Green).Render(
		"✓ rotated secret for " + v.regenName))
	b.WriteString("\n")
	b.WriteString(lipgloss.NewStyle().Foreground(styles.Amber).Render(
		"Save this key now — it will not be shown again:"))
	b.WriteString("\n\n")
	b.WriteString(lipgloss.NewStyle().Bold(true).Foreground(styles.BrandHi).Render("  " + v.regen.Key))
	b.WriteString("\n\n")
	b.WriteString(styles.TileLabel.Render("  prefix " + v.regen.KeyPrefix + "   ·   esc/enter to dismiss"))
	return b.String()
}

// table renders the virtual keys, RAG-colored by status, with the cursor row.
func (v *vks) table() string {
	var b strings.Builder
	b.WriteString(styles.TileValue.Render(fmt.Sprintf("Virtual keys (%d)", len(v.keys))))
	b.WriteString("\n")
	b.WriteString(lipgloss.NewStyle().Foreground(styles.Sub).Render(
		fmt.Sprintf("  %-26s %-14s %-12s %-10s %s", "NAME", "PREFIX", "TYPE", "STATUS", "ENABLED")))
	b.WriteString("\n")
	if len(v.keys) == 0 {
		b.WriteString(styles.TileLabel.Render("  (no virtual keys)"))
		return b.String()
	}
	var lines []string
	for i, k := range v.keys {
		status := lipgloss.NewStyle().Foreground(vkStatusColor(k.Status())).Render(fmt.Sprintf("%-10s", k.Status()))
		enabled := lipgloss.NewStyle().Foreground(styles.Green).Render("on")
		if !k.Enabled {
			enabled = lipgloss.NewStyle().Foreground(styles.Sub).Render("off")
		}
		cursor := "  "
		if i == v.cursor {
			cursor = lipgloss.NewStyle().Foreground(styles.Brand).Render("▸ ")
		}
		line := fmt.Sprintf("%-26s %-14s %-12s %s %s",
			clip(k.Name, 26), clip(k.KeyPrefix, 14), clip(vkType(k), 12), status, enabled)
		if i == v.cursor {
			line = lipgloss.NewStyle().Bold(true).Render(line)
		}
		lines = append(lines, cursor+line)
	}
	b.WriteString(strings.Join(lines, "\n"))
	return b.String()
}

// vkType is the key's type label ("application"/"personal"), or "—" when the
// nullable column is unset.
func vkType(k core.VirtualKey) string {
	if k.VKType != nil && *k.VKType != "" {
		return *k.VKType
	}
	return "—"
}

// vkStatusColor RAG-grades a VK approval status. It returns the TerminalColor
// interface so an AdaptiveColor (styles.Sub) and a plain Color can share it.
func vkStatusColor(status string) lipgloss.TerminalColor {
	switch status {
	case "active":
		return styles.Green
	case "pending":
		return styles.Amber
	case "revoked", "rejected":
		return styles.Red
	default:
		return styles.Sub
	}
}

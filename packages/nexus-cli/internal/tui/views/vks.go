package views

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"image/color"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/kit"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/styles"
)

// vks is the Virtual Keys view: the deployment's keys with their type, approval
// status, and enabled state, plus two prod-gated mitigation writes — revoke (r)
// and regenerate/rotate-secret (g). Revoke is offered only for keys in "active"
// status (the endpoint 404s otherwise), so the operator never burns a prod
// confirmation on a no-op. A regenerated secret is shown exactly once in a
// dismissible panel — the server keeps only a hash, so it is never persisted.
type vks struct {
	gw      kit.Gateway
	keys    []core.VirtualKey
	cursor  int
	detail  bool // enter opens a read-only detail drawer for the cursor row
	err     error
	loading bool

	cf        kit.Confirm // prod-gated revoke / regenerate
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
func newVKs(gw kit.Gateway, s ...kit.Session) *vks {
	return &vks{gw: gw, loading: true, cf: kit.NewConfirm(kit.OptSession(s))}
}

func (v *vks) Init() tea.Cmd { return v.fetch() }

func (v *vks) fetch() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := kit.FetchCtx()
		defer cancel()
		keys, err := v.gw.VirtualKeys(ctx)
		return vksMsg{keys: keys, err: err}
	}
}

// capturing suspends the root's single-letter shortcuts while the prod
// confirmation field is focused.
func (v *vks) Capturing() bool { return v.cf.Capturing() }

func (v *vks) Update(msg tea.Msg) (kit.ViewModel, tea.Cmd) {
	switch msg := msg.(type) {
	case vksMsg:
		v.loading = false
		v.err = msg.err
		if msg.keys != nil {
			v.keys = msg.keys
		}
		v.clampCursor()
		return v, kit.Tick(kit.PollSlow, vksTick{})
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
	case tea.KeyPressMsg:
		// The confirm field owns keystrokes while a prod confirmation is in flight.
		if handled, cmd := v.cf.Update(msg); handled {
			return v, cmd
		}
		// A shown secret panel takes over until dismissed (enter; ←/esc routes
		// through Back()).
		if v.regen != nil {
			if msg.String() == "enter" {
				v.regen = nil
				v.regenName = ""
			}
			return v, nil
		}
		// The detail drawer is read-only; ←/esc (root → Back()) closes it.
		if v.detail {
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
		case "enter":
			if _, ok := v.selected(); ok {
				v.detail = true
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

// back closes the once-shown secret panel or the detail drawer so ←/esc returns
// to the list before the root pops the nav stack.
func (v *vks) Back() bool {
	if v.regen != nil {
		v.regen = nil
		v.regenName = ""
		return true
	}
	if v.detail {
		v.detail = false
		return true
	}
	return false
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

// revoke begins a gated revoke of the selected key. Only "active" keys are
// revocable; for any other status it sets an explanatory note instead of
// starting the confirm (so an operator never authorizes a no-op that 404s).
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
	return v.cf.Begin("revoke virtual key "+name, func() tea.Cmd {
		v.busy = true
		return func() tea.Msg {
			ctx, cancel := kit.FetchCtx()
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
	return v.cf.Begin("regenerate (rotate the secret of) virtual key "+name, func() tea.Cmd {
		v.busy = true
		return func() tea.Msg {
			ctx, cancel := kit.FetchCtx()
			defer cancel()
			r, err := v.gw.RegenerateVK(ctx, id)
			return vkWriteMsg{verb: "rotated secret for", name: name, regen: r, err: err}
		}
	})
}

func (v *vks) Help() string {
	if v.cf.Capturing() {
		return v.cf.HelpHint()
	}
	if v.regen != nil {
		return "copy the secret now — shown once · ←/esc/enter dismiss · q quit"
	}
	if v.detail {
		return "←/esc back · q quit"
	}
	return "↑/↓ select · enter open · r revoke · g regenerate · 1-9 jump · tab chat · q quit"
}

func (v *vks) View(width, height int) string {
	if v.cf.Capturing() {
		return v.cf.View()
	}
	if v.regen != nil {
		return v.secretPanel()
	}
	if v.detail {
		return v.detailView()
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

// detailView renders the full record of the selected key, foregrounding the
// approval status (the operator's primary "can this key call" question) and the
// fields the list omits: source app, rate limit, owner / project scope, and id.
func (v *vks) detailView() string {
	k, ok := v.selected()
	if !ok {
		return styles.TileLabel.Render("(no key selected)")
	}
	var b strings.Builder
	b.WriteString(styles.TileValue.Render("Key · " + k.Name))
	b.WriteString("\n\n")
	status := lipgloss.NewStyle().Foreground(vkStatusColor(k.Status())).Render("● " + k.Status())
	b.WriteString(kit.DetailRow("Status", status) + "\n")
	enabled := lipgloss.NewStyle().Foreground(styles.Green).Render("on")
	if !k.Enabled {
		enabled = lipgloss.NewStyle().Foreground(styles.Sub).Render("off")
	}
	b.WriteString(kit.DetailRow("Enabled", enabled) + "\n")
	b.WriteString(kit.DetailRow("Type", vkType(k)) + "\n")
	b.WriteString(kit.DetailRow("Prefix", kit.Dash(k.KeyPrefix)) + "\n")
	b.WriteString(kit.DetailRow("Source app", kit.Dash(k.SourceApp)) + "\n")
	rate := "unlimited"
	if k.RateLimitRPM != nil {
		rate = fmt.Sprintf("%d rpm", *k.RateLimitRPM)
	}
	b.WriteString(kit.DetailRow("Rate limit", rate) + "\n")
	project := "— (personal / unscoped)"
	if k.ProjectID != nil && *k.ProjectID != "" {
		project = *k.ProjectID
	}
	b.WriteString(kit.DetailRow("Project", project) + "\n")
	b.WriteString(kit.DetailRow("Owner id", kit.Dash(k.OwnerID)) + "\n")
	b.WriteString(kit.DetailRow("Key id", kit.Dash(k.ID)))
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
			kit.Clip(k.Name, 26), kit.Clip(k.KeyPrefix, 14), kit.Clip(vkType(k), 12), status, enabled)
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
func vkStatusColor(status string) color.Color {
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

package kit

import (
	"fmt"
	"image/color"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/styles"
)

// confirmWhite is the high-contrast text on the selected (highlighted) choice chip.
var confirmWhite = lipgloss.Color("#ffffff")

// Confirm gates a mutation behind a Claude-style Allow/Deny choice, raised in EVERY
// environment — no write fires without the operator authorizing it. The operator picks
// Allow or Deny (arrow + enter, or y/n) before the action runs; the selection defaults
// to Deny, so a stray enter never applies a change. In prod the gate carries a red
// PRODUCTION banner + the env-named Apply button for extra gravity; off-prod it shows a
// neutral, lower-gravity prompt. One tested gate is shared by every mitigation write.
type Confirm struct {
	session Session
	active  bool
	prompt  string         // what is being confirmed (shown to the operator)
	allow   bool           // selection cursor: false = Deny (default), true = Allow
	run     func() tea.Cmd // the mutation, run once allowed
	err     error          // a cancel note (kept for parity; selection has no mismatch)
}

func NewConfirm(s Session) Confirm { return Confirm{session: s} }

// Begin starts confirming prompt in EVERY environment: it raises the Allow/Deny choice
// (defaulting to Deny) and returns no command. The mutation runs only when the operator
// selects Allow (or presses y). Gating is unconditional — there is no environment in
// which a write fires without confirmation.
func (c *Confirm) Begin(prompt string, run func() tea.Cmd) tea.Cmd {
	c.err = nil
	c.prompt = prompt
	c.run = run
	c.active = true
	c.allow = false // safe default — a stray enter denies, never applies
	return nil
}

// Capturing reports whether the choice owns keystrokes (so the root model suspends
// its single-letter shortcuts).
func (c *Confirm) Capturing() bool { return c.active }

// Cancel abandons an in-flight confirmation WITHOUT running its action — the
// teardown path when the turn that raised the gate ends. Clearing run makes a stale
// resolve a no-op. Idempotent.
func (c *Confirm) Cancel() {
	c.active = false
	c.run = nil
}

// Update handles a keystroke while confirming. handled is false when no
// confirmation is in progress, so the caller keeps the key for its own bindings.
// A resolved gate returns active=false; the caller maps a nil cmd to "denied".
func (c *Confirm) Update(msg tea.KeyPressMsg) (handled bool, cmd tea.Cmd) {
	if !c.active {
		return false, nil
	}
	switch msg.String() {
	case "left", "right", "up", "down", "tab", "h", "l":
		c.allow = !c.allow // move the selection
		return true, nil
	case "y", "Y": // quick allow
		c.active = false
		return true, c.run()
	case "n", "N", "esc": // quick deny / cancel
		c.active = false
		return true, nil
	case "enter":
		c.active = false
		if c.allow {
			return true, c.run()
		}
		return true, nil
	}
	return true, nil // swallow other keys while the gate is up
}

// View renders the Allow/Deny choice as a bordered modal box (Claude-Code style);
// empty when not confirming. A prod gate uses red PRODUCTION framing + the env-named
// Apply button; off-prod is a neutral amber box. Both are arrow/enter or y/n driven.
func (c *Confirm) View() string {
	if !c.active {
		return ""
	}
	if !c.session.IsProd {
		return c.box(styles.Amber, "Confirm", styles.Green,
			"←/→ choose · enter confirm · y allow · n/esc deny", " Apply ")
	}
	return c.box(styles.Red,
		fmt.Sprintf("⚠ PRODUCTION · %s", c.session.EnvName), styles.Red,
		"This applies to PRODUCTION.  ←/→ choose · enter confirm · esc deny",
		fmt.Sprintf(" Apply to %s ", c.session.EnvName))
}

// box renders the modal overlay: a rounded border in the accent color, a bold title,
// the prompt, a dim key hint, and the Deny/Apply chips with the current selection
// highlighted (Deny is the default, so a stray enter denies). allowBG colors the
// Apply chip when it is selected.
func (c *Confirm) box(accent color.Color, title string, allowBG color.Color, hint, allowLabel string) string {
	deny, allow := " Deny ", allowLabel
	if c.allow {
		deny = lipgloss.NewStyle().Foreground(styles.Sub).Render(deny)
		allow = lipgloss.NewStyle().Bold(true).Foreground(confirmWhite).Background(allowBG).Render(allow)
	} else {
		deny = lipgloss.NewStyle().Bold(true).Foreground(confirmWhite).Background(styles.Sub).Render(deny)
		allow = lipgloss.NewStyle().Foreground(allowBG).Render(allow)
	}
	var inner strings.Builder
	inner.WriteString(lipgloss.NewStyle().Bold(true).Foreground(accent).Render(title))
	inner.WriteString("\n")
	inner.WriteString(styles.TileValue.Render(c.prompt))
	inner.WriteString("\n\n")
	inner.WriteString(deny + "   " + allow)
	inner.WriteString("\n")
	inner.WriteString(styles.TileLabel.Render(hint))
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).BorderForeground(accent).
		Padding(0, 1).Render(inner.String())
}

// HelpHint is the keybar text shown while confirming.
func (c *Confirm) HelpHint() string {
	return "←/→ choose · enter confirm · y allow · n/esc deny"
}

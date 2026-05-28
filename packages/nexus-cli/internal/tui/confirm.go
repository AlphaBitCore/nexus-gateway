package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/styles"
)

// confirm gates a mutation behind a prod typed-confirmation. In a prod
// environment the operator must type the environment name before the action
// fires; elsewhere it fires immediately. One tested gate is shared by every
// mitigation write so the safety-critical confirm logic lives in one place.
type confirm struct {
	session Session
	input   textinput.Model
	active  bool
	prompt  string         // what is being confirmed (shown to the operator)
	run     func() tea.Cmd // the mutation, run once confirmed
	err     error          // a mismatch/cancel note
}

func newConfirm(s Session) confirm {
	ti := textinput.New()
	ti.Placeholder = s.EnvName
	ti.CharLimit = 40
	return confirm{session: s, input: ti}
}

// begin starts confirming prompt. In prod it focuses the typed-confirm field and
// returns its focus cmd; otherwise it runs the action immediately.
func (c *confirm) begin(prompt string, run func() tea.Cmd) tea.Cmd {
	c.err = nil
	c.prompt = prompt
	c.run = run
	if c.session.IsProd {
		c.active = true
		c.input.SetValue("")
		return c.input.Focus()
	}
	return run()
}

// capturing reports whether the typed-confirm field owns keystrokes (so the root
// model suspends its single-letter shortcuts).
func (c *confirm) capturing() bool { return c.active }

// update handles a keystroke while confirming. handled is false when no
// confirmation is in progress, so the caller keeps the key for its own bindings.
func (c *confirm) update(msg tea.KeyMsg) (handled bool, cmd tea.Cmd) {
	if !c.active {
		return false, nil
	}
	switch msg.String() {
	case "esc":
		c.active = false
		c.input.Blur()
		return true, nil
	case "enter":
		if strings.TrimSpace(c.input.Value()) != c.session.EnvName {
			c.err = fmt.Errorf("confirmation %q did not match env %q — not applied", c.input.Value(), c.session.EnvName)
			c.active = false
			c.input.Blur()
			return true, nil
		}
		c.active = false
		c.input.Blur()
		return true, c.run()
	}
	c.input, cmd = c.input.Update(msg)
	return true, cmd
}

// view renders the confirmation panel; empty when not confirming.
func (c *confirm) view() string {
	if !c.active {
		return ""
	}
	var b strings.Builder
	b.WriteString(lipgloss.NewStyle().Foreground(styles.Red).Bold(true).Render(
		fmt.Sprintf("⚠ PROD %s — confirm: %s", c.session.EnvName, c.prompt)))
	b.WriteString("\n")
	b.WriteString(fmt.Sprintf("Type the environment name (%s) to confirm:\n", c.session.EnvName))
	b.WriteString(c.input.View())
	return b.String()
}

// helpHint is the keybar text shown while confirming.
func (c *confirm) helpHint() string {
	return fmt.Sprintf("type %q + enter to confirm · esc cancel", c.session.EnvName)
}

// app_chrome.go renders the root model's chrome: the vertical split (canvas over
// the resident conversation), the prod banner, breadcrumb, footer (keybar + model +
// context gauge + env indicator), and the IME cursor anchoring. Split out of app.go
// so the root file holds the model + main loop, not the view composition.
package shell

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/kit"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/styles"
)

func (m Model) View() tea.View {
	if m.quitting {
		return tea.NewView("")
	}
	w := m.width
	if w == 0 {
		w = kit.DefaultViewWidth
	}
	parts := make([]string, 0, 4)
	bar := m.statusBar(w)
	barH := 0
	if bar != "" {
		parts = append(parts, bar)
		barH = lipgloss.Height(bar)
	}
	crumbs := m.crumbBar(w)
	footer := m.footerBar(w)
	contentH := m.height - barH - lipgloss.Height(crumbs) - lipgloss.Height(footer) - 1
	if contentH < minCanvasHeight+minChatHeight {
		contentH = minCanvasHeight + minChatHeight
	}
	parts = append(parts, crumbs, m.splitBody(w, contentH), footer)

	v := tea.NewView(strings.Join(parts, "\n"))
	v.AltScreen = true
	if cur := m.inputCursor(barH, lipgloss.Height(crumbs), contentH); cur != nil {
		v.Cursor = cur
	}
	return v
}

// inputCursor returns the real-terminal cursor position at the chat prompt when
// the chat owns the keyboard, so an IME candidate window anchors at the input.
// It returns nil when the canvas is focused or the prompt has no keyboard focus,
// leaving the cursor hidden. The prompt is always the last content line of the
// bottom chat panel, so its row derives from the layout heights.
func (m Model) inputCursor(barH, crumbsH, contentH int) *tea.Cursor {
	if m.focus != focusChat || !m.conv.input.Focused() {
		return nil
	}
	_, botH := easeHeights(contentH, m.focus, m.easeFrame)
	if botH <= 2 {
		return nil
	}
	topH := contentH - botH
	// Panel layout: row 0 is the top border, content fills rows 1..botH-2, the
	// prompt is the last content line. Columns: left border (1) + left padding (1)
	// then the rendered "you ▸ " prefix, then the DISPLAY width of the committed
	// text before the cursor — display width, not rune count, so a CJK cursor (each
	// char is two cells wide) anchors where the IME composition actually appears.
	// The input is unbounded (no SetWidth) so it never scrolls; display width of the
	// full pre-cursor text is the exact visual column.
	y := barH + crumbsH + topH + botH - 2
	runes := []rune(m.conv.input.Value())
	pos := m.conv.input.Position()
	if pos > len(runes) {
		pos = len(runes)
	}
	if pos < 0 {
		pos = 0
	}
	x := 2 + lipgloss.Width(convPromptPrefix) + lipgloss.Width(string(runes[:pos]))
	return tea.NewCursor(x, y)
}

// splitBody stacks the active view (top canvas) over the always-resident
// conversation (bottom), sized by focus via splitHeights. The conversation is
// wrapped in a panel whose border highlights when the chat is focused.
func (m Model) splitBody(w, contentH int) string {
	topH, botH := easeHeights(contentH, m.focus, m.easeFrame)
	parts := make([]string, 0, 2)
	if topH > 0 {
		// Pad/clip the canvas to exactly topH lines so the chat pane below it is
		// pinned to the bottom of the content area instead of floating mid-screen.
		top := m.views[m.active].View(w, topH)
		parts = append(parts, lipgloss.NewStyle().Height(topH).MaxHeight(topH).Render(top))
	}
	if botH > 2 {
		panel := styles.Panel
		if m.focus == focusChat {
			panel = styles.PanelFocused
		}
		// Force the panel to exactly botH lines (botH-2 content + 2 border) so the
		// conversation fills its half and its prompt sits at the very bottom. The panel
		// content width is w-6 — Width(w-2) minus the rounded border (2) AND the
		// horizontal padding (2); rendering conv.View at w-4 made full-width reasoning /
		// markdown lines wrap INSIDE the panel, overflowing its height (a covered prompt,
		// a cursor that drifted up, a doubled keybar). Match the content width exactly.
		parts = append(parts, panel.Width(w-2).Height(botH-2).MaxHeight(botH).Render(m.conv.View(w-6, botH-2)))
	}
	return strings.Join(parts, "\n")
}

// crumbBar renders the breadcrumb trail in place of the old tab row.
func (m Model) crumbBar(width int) string {
	labels := make([]string, 0, m.nav.depth()+1)
	for _, idx := range m.nav.stack {
		labels = append(labels, m.entries[idx].name)
	}
	active := m.entries[m.active].name
	if cp, ok := m.views[m.active].(crumbProvider); ok {
		if c := cp.Crumb(); c != "" {
			active = c
		}
	}
	labels = append(labels, active)
	left := styles.Crumb.Render(breadcrumbTrail(labels))
	// Fill the otherwise-unused top-right with the global-shortcut strip — but only
	// when the row is wide enough to hold it after the breadcrumb plus a gap. On a
	// narrow terminal it is dropped so it never collides with the breadcrumb (the
	// controls stay reachable via the bottom keybar and /help).
	hint := styles.HelpBar.Render(kit.GlobalHints())
	gap := width - lipgloss.Width(left) - lipgloss.Width(hint)
	if width <= 0 || gap < 2 {
		return left
	}
	return left + strings.Repeat(" ", gap) + hint
}

// footerBar composes the bottom line: the open slash palette owns it; otherwise
// the keybar on the left and the profile + address indicator on the right.
func (m Model) footerBar(width int) string {
	if m.slashOpen {
		return m.slash.View()
	}
	if m.sessionsOpen {
		return m.sessionPick.View()
	}
	// While a turn runs the working spinner + elapsed time replaces the keybar on
	// the left; the env indicator stays bottom-right.
	left := styles.HelpBar.Render(m.helpText())
	if s := m.conv.statusLine(); s != "" {
		left = styles.HelpBar.Render(s)
	}
	// Right cluster, in order: the selected model, the context gauge, then the env +
	// address badge — each a separate segment so they never run together.
	var segs []string
	if mdl := m.modelBadge(); mdl != "" {
		segs = append(segs, mdl)
	}
	if ctx := m.conv.contextBar(); ctx != "" {
		segs = append(segs, ctx)
	}
	if loc := m.locationIndicator(); loc != "" {
		segs = append(segs, loc)
	}
	right := strings.Join(segs, footerSep)
	if right == "" {
		return left
	}
	gap := width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		return left
	}
	return left + strings.Repeat(" ", gap) + right
}

// footerSep separates the footer-right segments so a value never abuts the next.
const footerSep = "   "

// modelBadge is the selected model slug, leading the footer-right cluster so the
// operator always sees what they are talking to. Reddened on prod like the env badge.
func (m Model) modelBadge() string {
	if m.session.Model == "" {
		return ""
	}
	style := styles.HelpBar
	if m.session.IsProd {
		style = lipgloss.NewStyle().Foreground(styles.Red).Bold(true)
	}
	return style.Render(kit.Clip(m.session.Model, 28))
}

// locationIndicator is the bottom-right "which deployment am I on" badge: the
// active profile name and its (scheme-stripped, truncated) Control Plane address.
// The selected model leads the footer cluster separately (modelBadge). Prod is
// reddened to reinforce the top banner.
func (m Model) locationIndicator() string {
	if m.session.EnvName == "" {
		return ""
	}
	label := m.session.EnvName
	if addr := stripScheme(m.session.Addr); addr != "" {
		label += " · " + kit.Clip(addr, 32)
	}
	style := styles.HelpBar
	if m.session.IsProd {
		style = lipgloss.NewStyle().Foreground(styles.Red).Bold(true)
	}
	return style.Render(label)
}

// stripScheme drops a leading http(s):// so the address reads compactly.
func stripScheme(u string) string {
	u = strings.TrimPrefix(u, "https://")
	return strings.TrimPrefix(u, "http://")
}

// helpText is the bottom keybar. When the chat owns the keyboard it supplies the
// keybar; otherwise a view with its own keybar wins, then the default.
func (m Model) helpText() string {
	if m.focus == focusChat {
		return m.conv.Help()
	}
	if h, ok := m.views[m.active].(helpProvider); ok {
		return h.Help()
	}
	return "tab chat · / commands · ↑/↓ move · enter open · ←/esc back · q quit"
}

// statusBar renders the prod safety banner only. In a non-prod env there is no top
// bar — the environment indicator lives bottom-right (locationIndicator) and the
// canvas breadcrumb is the top title — keeping the chrome minimal.
func (m Model) statusBar(width int) string {
	if !m.session.IsProd {
		return ""
	}
	sel := ""
	if m.session.Model != "" {
		sel = " · " + m.session.Model
		if m.session.VKName != "" {
			sel += " · vk:" + m.session.VKName
		}
	}
	return styles.ProdBanner.Width(width).Render(
		fmt.Sprintf("⚠ PROD %s — mutations require confirmation%s", m.session.EnvName, sel))
}

// textCapturer is implemented by views that capture raw keystrokes (text input)
// so the root suspends its single-letter shortcuts while typing.

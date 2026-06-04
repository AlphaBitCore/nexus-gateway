package shell

import (
	"fmt"
	"image/color"
	"strings"
	"time"

	"charm.land/lipgloss/v2"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/kit"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/styles"
)

func (c *conversation) statusLine() string {
	if !c.running {
		return ""
	}
	frame := spinnerFrames[c.spinnerPhase%len(spinnerFrames)]
	secs := int(time.Since(c.startedAt).Seconds())
	// After esc, the turn is tearing down (the cancel propagates through the loop's
	// ctx checks); say so rather than "working" so the interrupt reads as taken.
	if c.interrupted {
		return lipgloss.NewStyle().Foreground(styles.Amber).Render(
			fmt.Sprintf("%s interrupting…", frame))
	}
	return lipgloss.NewStyle().Foreground(styles.Brand).Render(
		fmt.Sprintf("%s working… %ds", frame, secs))
}

// contextBar renders the always-on context-usage indicator for the footer (empty
// until the first turn reports usage, or when there is no known window).
func (c *conversation) contextBar() string {
	return contextBar(c.ctxStats, c.ctxWindow)
}

// visibleAssistant returns the currently-shown trailing assistant text (test seam).
func (c *conversation) visibleAssistant() string {
	if n := len(c.lines); n > 0 && c.lines[n-1].tag == "asst" {
		return c.lines[n-1].text
	}
	return ""
}

// help is the keybar text shown while the chat is focused.
func (c *conversation) Help() string {
	if c.cf.Capturing() {
		return c.cf.HelpHint()
	}
	if c.scroll > 0 {
		return "scrolled back · ↓ newer · ↑ older · enter send (jumps to latest)"
	}
	if c.running {
		return "agent working… · enter queues · esc interrupts · ↑ scroll · ctrl+c quit"
	}
	return "enter send · ↑/↓ scroll · /help · tab views · ctrl+c quit"
}

// View renders the chat pane into the given box, filling the full height with the
// prompt pinned to the bottom: a title, the transcript bottom-aligned (newest just
// above the prompt, tail-trimmed so the latest exchange stays visible), the confirm
// gate when active, and the prompt as the last line.
func (c *conversation) View(width, height int) string {
	if width < 1 {
		width = 1
	}
	if height < 3 {
		height = 3
	}
	header := styles.TileValue.Render("Conversation")
	headLines := 1
	if c.notice != "" {
		header += "\n" + styles.TileLabel.Render(c.notice)
		headLines++
	}
	gate := c.cf.View()
	gateBlock := 0
	if gate != "" {
		gateBlock = lipgloss.Height(gate) + 1 // the gate plus a separator line
	}
	// Transcript fills everything between the header and the bottom-pinned prompt.
	trBudget := height - headLines - gateBlock - 1
	if trBudget < 1 {
		trBudget = 1
	}
	tr := c.transcript(width, trBudget)
	if pad := trBudget - lipgloss.Height(tr); pad > 0 {
		tr = strings.Repeat("\n", pad) + tr // bottom-align: blanks above the content
	}
	prompt := lipgloss.NewStyle().Foreground(styles.BrandHi).Bold(true).Render(convPromptPrefix) + c.input.View()

	var b strings.Builder
	b.WriteString(header)
	b.WriteString("\n")
	b.WriteString(tr)
	b.WriteString("\n")
	if gate != "" {
		b.WriteString(gate)
		b.WriteString("\n")
	}
	b.WriteString(prompt)
	return b.String()
}

// transcript renders the lines wrapped to width, tail-trimmed to budget lines so
// the newest content is always on screen. An empty transcript shows a hint.
func (c *conversation) transcript(width, budget int) string {
	if budget < 2 {
		budget = 2
	}
	if len(c.lines) == 0 {
		// Empty-state onboarding: a brand-new operator lands here, so name the few
		// controls that unlock everything else (chat, the / palette, /help, tab, scroll).
		return styles.TileLabel.Render(
			"Ask Nexus about your gateway — \"what's my most expensive provider?\", \"what's failing now?\"\n" +
				"— or describe a task and it will drive the views above for you.\n\n" +
				"New here?   type to chat   ·   /  commands   ·   /help  all keys   ·   tab  the views above   ·   ↑/↓  scroll")
	}
	var rendered []string
	last := len(c.lines) - 1
	for i := range c.lines {
		ln := c.lines[i]
		// A finalized assistant answer (or the /help reference) renders as markdown;
		// a line still being typed out stays raw so the typewriter reveal isn't
		// fighting glamour (the assistant stream while the turn runs, or a /help still
		// revealing).
		revealingThis := i == last && ln.tag == c.revealTag && c.revealing()
		skipMD := revealingThis || (ln.tag == "asst" && c.running && i == last)
		if (ln.tag == "asst" || ln.tag == "help") && !skipMD {
			if c.lines[i].md == "" || c.lines[i].mdW != width {
				c.lines[i].md = kit.RenderMarkdown(ln.text, width)
				c.lines[i].mdW = width
			}
			rendered = append(rendered, c.lines[i].md)
			continue
		}
		rendered = append(rendered, c.formatLine(ln, width))
	}
	flat := strings.Split(strings.Join(rendered, "\n"), "\n")
	// Record the geometry so the key handler can clamp scrollback, then window the
	// lines: the bottom of the window is `scroll` lines up from the newest line. The
	// window is exactly `budget` lines so it never overflows the pane (the scroll
	// state is surfaced in the help bar, not an extra transcript line).
	c.lastFlat, c.lastBudget = len(flat), budget
	c.clampScroll()
	if len(flat) > budget {
		end := len(flat) - c.scroll
		flat = flat[end-budget : end]
	}
	return strings.Join(flat, "\n")
}

// formatLine renders one transcript line as a colored role marker followed by
// neutral, terminal-default prose, word-wrapped to width. Color is reserved for
// the marker (who is speaking) and semantic state (amber notices) — the body
// stays neutral so the text you actually read is high-contrast and calm, the way
// a good CLI chat keeps long answers comfortable.
func (c *conversation) formatLine(ln convLine, width int) string {
	switch ln.tag {
	case "asst":
		// Streaming answer: a calm brand bullet + neutral prose. The body is left
		// terminal-default (no recolor) so the hand-off to the finalized markdown
		// render below doesn't visibly shift the text's color or weight.
		return markerLine("● ", styles.BrandHi, ln.text, width)
	case "think":
		// Reasoning/thinking channel — dim + italic so it reads as the model's
		// internal monologue, visually subordinate to the answer below it. Reasoning
		// is multi-line and long; wrap the PLAIN text first, then style — wrapping
		// already-styled multi-line ANSI (kit.WrapText(style.Render(…))) makes lipgloss
		// re-flow over the escape codes and garbles the pane.
		return lipgloss.NewStyle().Foreground(styles.Sub).Italic(true).
			Render(kit.WrapText("✱ "+ln.text, width)) // carries its own glyph
	case "tool":
		// Input-forward action line + dim result peek (or full I/O when verbose).
		return renderToolBlock(ln, c.toolVerbose, width)
	case "sys":
		// System notices carry their own glyph (⚠ …); amber, no role label so it
		// doesn't double-stamp ("sys ▸ ⚠ …").
		return kit.WrapText(lipgloss.NewStyle().Foreground(styles.Amber).Render(ln.text), width)
	case "queued":
		// A message typed while a turn runs, waiting its turn — dimmed + marked.
		return kit.WrapText(lipgloss.NewStyle().Foreground(styles.Sub).Italic(true).Render("⋯ "+ln.text), width)
	case "context", "help":
		// /context (pre-styled) and /help (markdown once revealed) are self-formatted
		// and multi-line; print as-is. Wrapping already-styled ANSI would garble them,
		// and /help carries its own rendered layout. During the reveal /help shows its
		// raw source typing out; the markdown branch renders it once the reveal lands.
		return ln.text
	}
	// User input echo: a brand caret + neutral, readable text.
	return markerLine("› ", styles.BrandHi, ln.text, width)
}

// markerLine renders "<marker><text>": the marker bold in markerColor, the body
// in the terminal's default foreground (neutral, high-contrast), word-wrapped to
// width. The marker is styled as a single inline token so lipgloss measures its
// visible width correctly; the body is left raw so a multi-line answer is never
// re-flowed over its own escape codes.
func markerLine(marker string, markerColor color.Color, text string, width int) string {
	head := lipgloss.NewStyle().Foreground(markerColor).Bold(true).Render(marker)
	return kit.WrapText(head+text, width)
}

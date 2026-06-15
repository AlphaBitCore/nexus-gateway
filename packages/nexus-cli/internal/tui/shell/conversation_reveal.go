package shell

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/kit"
)

func (c *conversation) appendLine(tag, text string) {
	if tag == "help" || tag == "context" {
		c.startReveal(tag, text)
		return
	}
	c.lines = append(c.lines, convLine{tag: tag, text: text})
}

// startReveal opens a fresh trailing line whose full text is typed out by the
// reveal clock (snapping any previously-animating line to its full text first).
func (c *conversation) startReveal(tag, text string) {
	c.flushReveal()
	c.streamFull = text
	c.reveal = 0
	c.revealTag = tag
	c.lines = append(c.lines, convLine{tag: tag, text: ""})
}

// revealing reports whether the typewriter still has hidden text on the trailing line.
func (c *conversation) revealing() bool { return c.reveal < visibleLen(c.streamFull) }

// revealANSI returns the first n VISIBLE runes of s, copying ANSI escape sequences
// verbatim (uncounted) so a styled string is never sliced mid-escape; a trailing
// reset is appended only when s carried escapes so a cut style cannot bleed. Raw
// text (the assistant stream, /help source) is just truncated.
//
// SECURITY — INTENTIONAL ANSI passthrough, do NOT sanitize here. Unlike
// the CLI's `restable.SanitizeTerminal` (which strips control sequences from
// server-supplied table cells and error bodies), this TUI conversation view
// deliberately renders ANSI for styling: the typewriter reveal of the assistant's
// own streamed answer relies on copying escape sequences through so styled output
// shows correctly. An operator who explicitly opens the interactive conversation
// view to watch AI traffic is knowingly rendering that styled content; stripping
// ANSI here would break the product's core display. The injection-hardening
// boundary is the non-interactive, scriptable surface (table cells, error prints),
// which IS sanitized — not this opt-in interactive view.
func revealANSI(s string, n int) string {
	if n < 0 {
		n = 0
	}
	var b strings.Builder
	hadEsc, count := false, 0
	rs := []rune(s)
	for i := 0; i < len(rs); {
		if rs[i] == 0x1b {
			hadEsc = true
			b.WriteRune(rs[i])
			i++
			for i < len(rs) {
				ch := rs[i]
				b.WriteRune(ch)
				i++
				if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') {
					break
				}
			}
			continue
		}
		if count >= n {
			break
		}
		b.WriteRune(rs[i])
		count++
		i++
	}
	if hadEsc {
		b.WriteString("\x1b[0m")
	}
	return b.String()
}

// visibleLen counts the visible runes of s (ANSI escape sequences excluded).
func visibleLen(s string) int {
	n := 0
	rs := []rune(s)
	for i := 0; i < len(rs); {
		if rs[i] == 0x1b {
			i++
			for i < len(rs) {
				ch := rs[i]
				i++
				if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') {
					break
				}
			}
			continue
		}
		n++
		i++
	}
	return n
}

// scrollStep is half a page (min 1) — a comfortable PgUp/PgDn jump.
func (c *conversation) scrollStep() int {
	if c.lastBudget > 2 {
		return c.lastBudget / 2
	}
	return 1
}

// clampScroll holds the scrollback offset within [0, hidden-lines] using the last
// render's line counts, so PgUp can't scroll past the oldest line.
func (c *conversation) clampScroll() {
	maxScroll := c.lastFlat - c.lastBudget
	if maxScroll < 0 {
		maxScroll = 0
	}
	if c.scroll > maxScroll {
		c.scroll = maxScroll
	}
	if c.scroll < 0 {
		c.scroll = 0
	}
}

// beginAssistant opens a fresh trailing assistant line for the turn's streamed
// text and resets the reveal buffer. It first snaps the previous assistant
// segment to its complete buffered text: when reasoning interleaves between two
// answer segments, the earlier asst line is no longer the trailing line, so the
// typewriter (which only advances the trailing asst line) would otherwise leave
// it frozen at a truncated prefix.
func (c *conversation) beginAssistant() {
	c.flushReveal()
	c.streamFull = ""
	c.reveal = 0
	c.revealTag = "asst"
	c.appendLine("asst", "")
}

// feed appends a streamed delta to the full buffer (not yet shown). The reveal
// clock (revealStep) uncovers it gradually for the typewriter effect.
func (c *conversation) feed(delta string) {
	if n := len(c.lines); n == 0 || c.lines[n-1].tag != "asst" {
		c.beginAssistant()
	}
	c.streamFull += delta
}

// feedReasoning grows the trailing "think" line with a streamed reasoning delta.
// Reasoning is shown live (no typewriter — thinking should scroll as it arrives)
// and rendered in a distinct dim style; it is display-only and is never persisted
// to the agent transcript. A non-think trailing line opens a fresh think block, so
// reasoning that resumes after assistant text appears as its own block in order.
func (c *conversation) feedReasoning(delta string) {
	if n := len(c.lines); n == 0 || c.lines[n-1].tag != "think" {
		c.appendLine("think", "")
	}
	n := len(c.lines)
	c.lines[n-1].text += delta
}

// revealStep uncovers up to revealRunes more runes of the buffered assistant text
// into the visible trailing line. Returns true while hidden text remains.
func (c *conversation) revealStep() bool {
	total := visibleLen(c.streamFull)
	if c.reveal >= total {
		return false
	}
	c.reveal += revealRunes
	if c.reveal > total {
		c.reveal = total
	}
	if n := len(c.lines); n > 0 && c.lines[n-1].tag == c.revealTag {
		c.lines[n-1].text = revealANSI(c.streamFull, c.reveal)
	}
	return c.reveal < total
}

// flushReveal shows all buffered text at once (turn end — the final answer is never
// withheld behind the typewriter cadence). It snaps the MOST RECENT assistant line
// (searching back past any trailing think/tool lines, so an answer segment that a
// reasoning block interleaved after still finalizes). The empty-buffer guard skips
// the snap when no text has streamed: a pure tool turn (nothing to flush), and
// — load-bearing — after /clear wipes the transcript and resets the buffer, so the
// next turn's first beginAssistant cannot snap onto a now-absent line. (streamFull
// persists across ordinary turns, so a back-to-back text turn re-snaps the prior
// asst line to its own unchanged full text: idempotent, not a wipe.)
func (c *conversation) flushReveal() {
	if c.streamFull == "" {
		return
	}
	c.reveal = visibleLen(c.streamFull)
	for i := len(c.lines) - 1; i >= 0; i-- {
		if c.lines[i].tag == c.revealTag {
			c.lines[i].text = c.streamFull
			return
		}
	}
}

// tickUpdate advances one animation frame: reveal more buffered text + spin the
// working glyph. It re-issues itself perpetually (kicked once by the root) so the
// typewriter and spinner stay live; revealStep is a no-op when nothing is buffered.
func (c *conversation) tickUpdate() tea.Cmd {
	c.revealStep()
	c.spinnerPhase++
	return kit.Tick(kit.ConvAnimInterval, convTick{})
}

// statusLine is the bottom-most status text (left side): the working spinner +
// elapsed seconds while a turn runs, empty when idle.

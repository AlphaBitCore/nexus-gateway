package shell

import (
	"fmt"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/agent"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/kit"
)

// conversation_commands.go — the conversation's typed slash controls (/clear,
// /compact, /sessions, /verbose, /help, /context) and the
// compaction they drive. Split from conversation.go along the command seam so
// the core file keeps only the turn lifecycle + transcript fold.

// startCompact runs a manual /compact: it builds the agent if needed and launches the
// one-shot summary call in the background (the footer spinner shows it running). It is
// rejected with a notice if a turn is already in flight, since compaction rewrites the
// same session the turn is reading.
func (c *conversation) startCompact() tea.Cmd {
	c.notice = ""
	if c.running {
		c.notice = "busy — finish the current turn first"
		return nil
	}
	if err := c.ensureAgent(); err != nil {
		c.appendLine("sys", "⚠ "+err.Error())
		return nil
	}
	// Immediate feedback: the summary is a model call that can take a while at high
	// context, so show it started rather than leaving the operator staring at a bare
	// spinner (the "/compact does nothing" report).
	c.appendLine("sys", "⊙ compacting context… (esc to cancel)")
	c.scroll = 0
	c.running = true
	c.startedAt = time.Now()
	return c.bridge.startCompact()
}

// applyCompact surfaces a compaction in the transcript (like a tool-call line) and
// refreshes the context indicator. On acted, it prints the message/token delta and
// drops the footer gauge to the post-compaction estimate immediately so the operator
// sees the freed context now; Used/History are estimates until the next turn reports
// the exact gateway usage. On a no-op it just notes there was nothing to compact.
func (c *conversation) applyCompact(msg agentCompactMsg) {
	c.flushReveal()
	if !msg.acted {
		c.appendLine("sys", "⊙ nothing to compact")
		c.scroll = 0
		return
	}
	s := msg.stat
	if s.Kind == "trim" {
		// Auto deterministic trim (mid-turn): elided old tool output to fit the window.
		// The turn's end-of-turn OnContext reports the exact gauge, so don't estimate here.
		c.appendLine("sys", fmt.Sprintf("⊙ context trimmed — elided %d old tool output(s) to fit the window (~%s → ~%s tokens)",
			s.Elided, kit.Ktok(s.TokensBefore), kit.Ktok(s.TokensAfter)))
		c.scroll = 0
		return
	}
	// Explicit /compact summary permanently rewrote the session → refresh the gauge now.
	c.appendLine("sys", fmt.Sprintf("⊙ context compacted — %d→%d messages (~%s → ~%s tokens)",
		s.MessagesBefore, s.MessagesAfter, kit.Ktok(s.TokensBefore), kit.Ktok(s.TokensAfter)))
	c.ctxStats.Used = s.TokensAfter
	c.ctxStats.History = s.TokensAfter
	c.ctxStats.Messages = s.MessagesAfter
	c.ctxStats.Cached = 0
	c.scroll = 0
}

// agentCommand handles the conversation's typed slash controls (/clear, /help).
// It is also the dispatch target for the slash palette's agent commands.
func (c *conversation) agentCommand(val string) tea.Cmd {
	name, _ := kit.SplitCmdArg(val)
	switch name {
	case "clear":
		// Clear the agent's context, not just the screen: cancel any in-flight turn,
		// then reset the agent so the next turn rebuilds with a fresh session
		// (capabilities.BuildAgent starts a new empty session — no carried history).
		// Finally wipe the transcript + queue + reveal buffer.
		if c.running {
			c.interrupted = true // suppress the cancelled turn's terminal noise
		}
		c.resetAgent()
		c.resume = nil // a resumed session is abandoned too — the next turn starts fresh
		c.lines = nil
		c.pending = nil
		c.streamFull = ""
		c.reveal = 0
		c.revealTag = ""
		c.scroll = 0
		// Reset the context indicator too: a fresh session has no used tokens, so the
		// footer gauge must fall back to "ctx –/<window>" rather than stranding the old
		// turn's number.
		c.ctxStats = agent.ContextStats{}
		c.notice = "conversation + context cleared"
	case "compact":
		// Manual compaction: summarize the older transcript now and permanently rewrite
		// the persisted session. Runs as a background one-shot (it makes a model call).
		return c.startCompact()
	case "sessions", "history":
		// The picker is a root-level overlay (it owns the footer like the slash
		// palette), so hand the request up rather than handling it here.
		return func() tea.Msg { return openSessionsMsg{} }
	case "verbose":
		// Toggle full tool-call I/O for the whole transcript (same as ctrl+t).
		c.toolVerbose = !c.toolVerbose
		if c.toolVerbose {
			c.notice = "tool calls: showing full input/output (ctrl+t or /verbose to hide)"
		} else {
			c.notice = "tool calls: showing the one-line peek (ctrl+t or /verbose for detail)"
		}
	case "help", "?":
		// Render the full reference into the transcript (markdown, scrollable) rather
		// than a one-line notice — /help is a complete cheat sheet.
		c.appendLine("help", kit.HelpReference())
		c.scroll = 0
		c.notice = ""
	case "context", "ctx":
		// Print the context-usage breakdown into the transcript (like /help) so it
		// scrolls with the conversation and the chat stays usable.
		c.appendLine("context", contextPanel(c.ctxStats, c.ctxWindow, c.session.Model))
		c.scroll = 0
		c.notice = ""
	default:
		// NOT a known command → send it to the assistant as a normal message.
		// An operator's pasted "/v1/messages returns 404" must never be eaten
		// by the router (typed input gets the live palette, but a paste lands
		// here directly — same rule as the web chat's slash router).
		return c.submit(val)
	}
	return nil
}

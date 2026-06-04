package shell

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/agent"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/kit"
)

// confirmReplyMsg carries the human's decision on a mitigation back to the agent
// loop. The conversation raises the shared confirm gate on agentConfirmMsg and
// emits this once the gate resolves (Allow → ok=true; Deny/esc-cancelled →
// ok=false). The root forwards nothing here;
// the conversation consumes it and calls bridge.reply.
type confirmReplyMsg struct{ ok bool }

// convLine is one rendered line in the transcript. tag is the speaker/kind badge
// ("you"/"asst"/"tool"/"sys"); a streaming assistant turn is the trailing "asst"
// line and is grown in place by each text delta.
type convLine struct {
	tag  string
	text string
	// md / mdW cache the markdown-rendered form of a finalized assistant line so
	// glamour runs once per width, not every animation frame. Empty until rendered.
	md  string
	mdW int

	// Tool-call transparency (tag == "tool"): the raw call so the renderer can show
	// an input-forward action line + a dim result peek, and expand the full I/O on
	// the ctrl+t / /verbose toggle. toolDone flips when the result arrives.
	toolName   string
	toolInput  []byte
	toolOutput []byte
	toolErr    bool
	toolDone   bool
}

// conversation is the resident bottom agent pane: the operator converses with the
// gateway agent and watches it drive the cockpit. It owns the
// bridge (built lazily via the AgentBuildFunc seam on first turn), folds the
// bridge's streamed text + tool-progress into a transcript, and routes a
// mitigation request through the shared Allow/Deny confirm gate before replying.
type conversation struct {
	session Session
	build   AgentBuildFunc // nil => agent unavailable (tests / unconfigured)

	// log records turn-level failures so a user-visible "⚠ …" line also lands in
	// the CLI's diagnostic file with context. nil-safe: tests and unconfigured
	// shells leave it nil and the helper at logTurnErr no-ops.
	log *slog.Logger

	bridge     *bridge
	buildErr   error
	agentBuilt bool

	input  textinput.Model
	cf     kit.Confirm
	lines  []convLine
	notice string

	running         bool
	awaitingConfirm bool
	interrupted     bool // current turn was cancelled by esc / clear; finish suppresses its terminal noise

	pending []string // messages typed while a turn runs, drained FIFO on finish

	// scroll is the transcript scrollback offset in lines from the bottom (0 =
	// pinned to the newest content). PgUp/PgDn move it; a new turn snaps back to 0.
	// lastFlat / lastBudget cache the most recent render's total + visible line
	// counts so the key handler can clamp scroll without re-rendering.
	scroll     int
	lastFlat   int
	lastBudget int

	streamFull   string    // full text of the trailing typed-out line (assistant stream, or /help · /context)
	reveal       int       // visible runes of streamFull currently shown in the trailing line
	revealTag    string    // tag of the line the typewriter is currently revealing ("asst", "help", "context")
	startedAt    time.Time // when the current turn began (drives the elapsed clock)
	spinnerPhase int       // working-spinner animation phase

	activeView string // the view name handed to the agent as turn context

	// toolVerbose expands every tool block from its one-line peek to the full
	// input/output (dim, capped). Toggled by ctrl+t and /verbose; a session-level
	// preference so the operator chooses how much tool detail the transcript shows.
	toolVerbose bool

	// ctxStats / ctxWindow are the latest context-usage stats + the model's window,
	// driving the always-on footer indicator (and the /context breakdown).
	ctxStats  agent.ContextStats
	ctxWindow int
}

// revealRunes is how many runes the typewriter uncovers per animation tick. Tuned
// so bursty SSE still reads as smooth, even typing.
const revealRunes = 3

// convPromptPrefix is the chat input's visible prefix — a brand caret matching the
// "› " marker on submitted user lines. Shared by the prompt render and the IME
// cursor-position calc so they stay in lockstep.
const convPromptPrefix = "› "

// convTick drives the conversation's animation: the typewriter reveal + the
// working spinner. It is kicked once by the root and re-issues itself forever.
type convTick struct{}

// spinnerFrames cycle the working glyph.
var spinnerFrames = []string{"✶", "✦", "✷", "✸", "✺", "✹"}

func newConversation(s Session, build AgentBuildFunc) *conversation {
	ti := textinput.New()
	ti.Placeholder = "Ask Nexus or describe a task…  (enter send · /clear · /help)"
	ti.CharLimit = 4000
	ti.Prompt = ""
	// Use the real terminal cursor (not the textinput's virtual block) so the root
	// can place the hardware cursor at the prompt — that is what a macOS/CJK IME
	// anchors its candidate window to. The root computes the position in inputCursor.
	ti.SetVirtualCursor(false)
	// Seed the context window from the session so the gauge shows "ctx –/<window>"
	// from launch, before the first turn reports usage.
	return &conversation{session: s, build: build, input: ti, cf: kit.NewConfirm(s), ctxWindow: s.ContextWindow}
}

// Init returns the input blink; the conversation fetches nothing on its own (the
// agent is built lazily on the first turn).
func (c *conversation) Init() tea.Cmd { return textinput.Blink }

// focus gives the prompt keyboard focus (called when the chat gains focus).
func (c *conversation) focus() tea.Cmd { return c.input.Focus() }

// blur releases keyboard focus (called when focus moves to the canvas).
func (c *conversation) blur() { c.input.Blur() }

// capturing reports whether the conversation owns keystrokes — the prompt is
// focused or the confirm gate is up — so the root suspends NORMAL hotkeys.
func (c *conversation) Capturing() bool { return c.input.Focused() || c.cf.Capturing() }

// setActiveView records the view the operator is on so the next turn carries it
// as context (the agent grounds answers in what the operator is looking at).
func (c *conversation) setActiveView(name string) { c.activeView = name }

// drainCmd re-issues the bridge drain so the root can keep pumping agent events
// after it handles a canvas drive (nav/show/highlight) itself. Nil when no agent.
func (c *conversation) drainCmd() tea.Cmd {
	if c.bridge == nil {
		return nil
	}
	return c.bridge.drain()
}

// stop tears down an in-flight turn (quit / teardown). Nil-safe.
func (c *conversation) stop() { c.bridge.stop() }

// ensureAgent builds the bridge + agent once, on the first turn. A nil seam or a
// build error is recorded and surfaced in the transcript; it is attempted once
// (a build failure is a persistent misconfiguration, not a transient error).
func (c *conversation) ensureAgent() error {
	if c.agentBuilt {
		return c.buildErr
	}
	c.agentBuilt = true
	if c.build == nil {
		c.buildErr = errors.New("agent unavailable (no model/VK configured)")
		return c.buildErr
	}
	b := newBridge(nil)
	onText := func(s string) { b.sendEv(agentTextMsg{delta: s}) }
	onReasoning := func(s string) { b.sendEv(agentReasoningMsg{delta: s}) }
	onTool := func(name string, input []byte) { b.sendEv(agentToolMsg{name: name, input: input}) }
	onToolEnd := func(name string, output []byte, isError bool) {
		b.sendEv(agentToolResultMsg{name: name, output: output, isError: isError})
	}
	onContext := func(stats agent.ContextStats, window int) { b.sendEv(contextStatsMsg{stats: stats, window: window}) }
	onCompact := func(stat agent.CompactStat) { b.sendEv(agentCompactMsg{stat: stat, acted: true}) }
	runner, err := c.build(b, b.confirm, onText, onReasoning, onTool, onToolEnd, onContext, onCompact)
	if err != nil {
		c.buildErr = err
		return err
	}
	b.agent = runner
	c.bridge = b
	return nil
}

// Update folds one message and returns the next command. Pointer receiver: it
// mutates in place (the root holds *conversation), returning only the cmd.
func (c *conversation) Update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case convTick:
		return c.tickUpdate()
	case agentTextMsg:
		c.feed(msg.delta)
		return c.bridge.drain()
	case agentReasoningMsg:
		c.feedReasoning(msg.delta)
		return c.bridge.drain()
	case agentToolMsg:
		// Start: record the call so the renderer can show an input-forward action
		// line + (once the result lands) a dim peek, expandable via ctrl+t.
		c.lines = append(c.lines, convLine{tag: "tool", toolName: msg.name, toolInput: msg.input})
		return c.bridge.drain()
	case agentToolResultMsg:
		// End: fill the first still-open tool block (results arrive in call order).
		for i := range c.lines {
			if c.lines[i].tag == "tool" && !c.lines[i].toolDone {
				c.lines[i].toolOutput = msg.output
				c.lines[i].toolErr = msg.isError
				c.lines[i].toolDone = true
				break
			}
		}
		return c.bridge.drain()
	case contextStatsMsg:
		c.ctxStats, c.ctxWindow = msg.stats, msg.window
		if c.bridge != nil {
			return c.bridge.drain()
		}
		return nil
	case agentCompactMsg:
		c.applyCompact(msg)
		if c.bridge != nil {
			return c.bridge.drain()
		}
		return nil
	case agentConfirmMsg:
		return c.beginConfirm(msg)
	case confirmReplyMsg:
		c.awaitingConfirm = false
		if c.bridge == nil {
			return nil
		}
		return c.bridge.reply(msg.ok) // forwards the decision + resumes draining
	case agentDoneMsg:
		return c.finish(msg)
	case tea.PasteMsg:
		// Pasted text edits the prompt (the textinput inserts it). The confirm gate
		// owns the keyboard when up, so don't leak a paste into the prompt then.
		if c.awaitingConfirm {
			return nil
		}
		var cmd tea.Cmd
		c.input, cmd = c.input.Update(msg)
		return cmd
	case tea.KeyPressMsg:
		return c.handleKey(msg)
	}
	return nil
}

// handleKey routes a keystroke: a live confirm gate wins; esc interrupts a
// running turn; enter sends/enqueues/handles-a-command; any other key edits the
// prompt — which stays editable even while a turn runs (continuous sending).
func (c *conversation) handleKey(k tea.KeyPressMsg) tea.Cmd {
	if c.awaitingConfirm {
		_, cmd := c.cf.Update(k)
		if !c.cf.Capturing() { // the gate just resolved
			c.awaitingConfirm = false
			if cmd == nil {
				// Deny / esc-cancel → decline the mitigation.
				return func() tea.Msg { return confirmReplyMsg{ok: false} }
			}
		}
		return cmd // approve path: cmd yields confirmReplyMsg{ok:true}
	}
	// Scrollback through the transcript (works while a turn streams too). The prompt
	// is single-line, so ↑/↓ are free for line scroll — the Mac-friendly option,
	// since PgUp/PgDn require Fn+↑/Fn+↓ on a MacBook. PgUp/PgDn jump a half page.
	switch k.String() {
	case "ctrl+t":
		// Toggle full tool-call I/O (peek ⇄ expanded) for the whole transcript.
		c.toolVerbose = !c.toolVerbose
		return nil
	case "up":
		c.scroll++
		c.clampScroll()
		return nil
	case "down":
		c.scroll--
		c.clampScroll()
		return nil
	case "pgup":
		c.scroll += c.scrollStep()
		c.clampScroll()
		return nil
	case "pgdown":
		c.scroll -= c.scrollStep()
		c.clampScroll()
		return nil
	}
	if k.Code == tea.KeyEsc && c.running {
		// Cancel the in-flight turn. running stays set so the turn's terminal
		// agentDoneMsg — still delivered by the outstanding drain — is the single
		// finalizer: it tears the bridge down and then drains the next queued message,
		// keeping turns strictly serial (no second drain, no cross-turn done). The
		// interrupted flag suppresses the redundant cancellation line in finish.
		// Queued messages are kept — the operator interrupted this turn, not the plan.
		c.bridge.stop()
		c.interrupted = true
		c.flushReveal()
		c.appendLine("sys", "⚠ interrupted")
		c.scroll = 0 // snap to the latest so the interrupt is visible at the prompt
		return nil
	}
	if k.Code == tea.KeyEnter {
		val := strings.TrimSpace(c.input.Value())
		switch {
		case val == "":
			return nil
		case strings.HasPrefix(val, "/"):
			c.input.SetValue("")
			return c.agentCommand(val)
		case c.running:
			// A turn is in flight — enqueue rather than block. The message shows as a
			// dim queued line and runs when the current turn finishes.
			c.input.SetValue("")
			c.appendLine("queued", val)
			c.pending = append(c.pending, val)
			return nil
		default:
			return c.submit(val)
		}
	}
	var cmd tea.Cmd
	c.input, cmd = c.input.Update(k)
	return cmd
}

// submit starts an agent turn for text. It builds the agent on first use; a build
// failure is surfaced as a sys line instead of a turn.
func (c *conversation) submit(text string) tea.Cmd {
	c.notice = ""
	if err := c.ensureAgent(); err != nil {
		c.appendLine("sys", "⚠ "+err.Error())
		c.logTurnErr("agent build failed", err)
		c.input.SetValue("")
		return nil
	}
	c.input.SetValue("")
	c.appendLine("you", text)
	c.scroll = 0 // a new exchange snaps the transcript back to the latest
	c.running = true
	c.startedAt = time.Now()
	return c.bridge.startTurn(text, c.activeView)
}

// beginConfirm raises the shared Allow/Deny confirm gate for a mitigation in every
// environment (awaitingConfirm routes keys to the gate); the reply rides back only
// when the operator allows. Prod adds the red banner; off-prod is a neutral prompt.
func (c *conversation) beginConfirm(msg agentConfirmMsg) tea.Cmd {
	prompt := msg.tool
	if msg.reason != "" {
		prompt += " — " + msg.reason
	}
	cmd := c.cf.Begin(prompt, func() tea.Cmd {
		return func() tea.Msg { return confirmReplyMsg{ok: true} }
	})
	c.awaitingConfirm = c.cf.Capturing() // the gate is up in every env until resolved
	return cmd
}

// finish closes the turn: clears running, releases the bridge's turn guard,
// surfaces the terminal text/error, then drains the next queued message (kept
// serial so the confirm-gate safety holds). A step-cap error is shown as a hint
// that the operator can ask the agent to continue.
func (c *conversation) finish(msg agentDoneMsg) tea.Cmd {
	interrupted := c.interrupted
	c.interrupted = false
	c.running = false
	// A turn that ends with a confirm still pending (timeout / teardown) must drop
	// the gate: otherwise the operator's later keystrokes could resolve the dead
	// turn's confirm onto the next turn, authorizing a mitigation never approved
	// (fail-safe — no carried-over authorization).
	c.awaitingConfirm = false
	c.cf.Cancel()
	if c.bridge != nil {
		c.bridge.done()
	}
	c.flushReveal() // never withhold the final answer behind the typewriter cadence
	// An interrupted turn already surfaced its own line; its terminal text/error is
	// just the cancellation, so don't double-message. Otherwise surface the final
	// answer when the agent streamed no assistant text this turn (a pure tool turn):
	// add it as its own line. When text was streamed the trailing assistant line
	// already holds it, so nothing is appended (no dup).
	if !interrupted {
		if msg.final != "" {
			if n := len(c.lines); n == 0 || c.lines[n-1].tag != "asst" {
				c.appendLine("asst", msg.final)
			}
		}
		if msg.err != nil {
			c.appendLine("sys", "⚠ "+msg.err.Error()+" — ask again to continue")
			c.logTurnErr("turn failed", msg.err)
		}
	}
	c.input.Focus()
	// Drain the next queued message as a fresh turn (serial: one turn at a time).
	if len(c.pending) > 0 {
		next := c.pending[0]
		c.pending = c.pending[1:]
		return c.startQueued(next)
	}
	return nil
}

// logTurnErr mirrors a user-visible "⚠ …" failure into the CLI's diagnostic file
// with the active env + view for context, so a reported "it just hangs / errors"
// can be matched against the per-request transport timings the LoggingTransport
// already wrote. nil-safe: an unconfigured shell (tests) leaves c.log nil and
// this no-ops.
func (c *conversation) logTurnErr(what string, err error) {
	if c.log == nil || err == nil {
		return
	}
	c.log.Error(what,
		"env", c.session.EnvName,
		"view", c.activeView,
		"err", err.Error(),
	)
}

// startQueued starts a turn for a message that was already shown as a queued line,
// so (unlike submit) it appends no new transcript line. Builds the agent if needed
// and stamps the animation clock.
func (c *conversation) startQueued(text string) tea.Cmd {
	if err := c.ensureAgent(); err != nil {
		c.appendLine("sys", "⚠ "+err.Error())
		return nil
	}
	c.scroll = 0 // the next queued exchange snaps back to the latest
	c.running = true
	c.startedAt = time.Now()
	return c.bridge.startTurn(text, c.activeView)
}

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

// resetAgent tears down the built agent so the next turn rebuilds it. Used by a
// model switch: the build seam reads the (now updated) model on the next turn.
func (c *conversation) resetAgent() {
	if c.bridge != nil {
		c.bridge.stop()
	}
	c.bridge = nil
	c.agentBuilt = false
	c.buildErr = nil
	c.running = false
}

// setModel switches the chat/agent model: cancel any in-flight turn, update the
// session, reset the agent (so the next turn rebuilds with the new model), and
// confirm in the transcript. Persisting the choice is the root's job (it owns the
// CLI seam); the conversation only changes what it builds against.
func (c *conversation) setModel(code string) {
	c.resetAgent()
	c.session.Model = code
	c.cf = kit.NewConfirm(c.session) // keep the confirm gate's env in sync
	c.appendLine("sys", "model → "+code)
	c.notice = ""
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
		c.notice = "unknown command /" + name + " — /help for commands"
	}
	return nil
}

// appendLine adds a transcript line. Command outputs (/help, /context) are typed
// out with the same typewriter as the assistant stream; everything else is added in

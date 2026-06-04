package shell

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/agent"
	capabilities "github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/capabilities/runtime"
)

// sgrRE matches ANSI SGR (color/style) escapes. The conversation markdown-renders
// finalized assistant answers via glamour, which styles them with SGR codes;
// stripping those lets a test assert on the rendered TEXT (words stay contiguous —
// glamour keeps spaces inside the styled spans) without matching escape bytes.
var sgrRE = regexp.MustCompile("\x1b\\[[0-9;]*m")

func stripSGR(s string) string { return sgrRE.ReplaceAllString(s, "") }

// convScript is one scripted agent turn: it receives the bridge's real streaming
// callbacks + confirm so a test can simulate text/reasoning/tool/confirm without a
// model.
type convScript func(onText, onReasoning func(string), onTool func(string, []byte), confirm agent.ConfirmFunc) (string, error)

// recordingRunner captures the args Turn was invoked with so a test can assert
// the active view is threaded as turn context.
type recordingRunner struct {
	script      convScript
	gotView     string
	gotText     string
	onText      func(string)
	onReasoning func(string)
	onTool      func(string, []byte)
	onCompact   func(agent.CompactStat)
	confirm     agent.ConfirmFunc
	canvasOK    bool
	compactFn   func() (agent.CompactStat, bool, error) // drives the manual /compact path
}

func (r *recordingRunner) Turn(_ context.Context, userText, activeView string) (string, error) {
	r.gotText, r.gotView = userText, activeView
	return r.script(r.onText, r.onReasoning, r.onTool, r.confirm)
}

func (r *recordingRunner) Compact(_ context.Context) (agent.CompactStat, bool, error) {
	if r.compactFn == nil {
		return agent.CompactStat{}, false, nil
	}
	return r.compactFn()
}

// newTestConv builds a conversation whose agent runs script. The returned runner
// exposes what Turn saw (active view / user text) for assertions.
func newTestConv(s Session, script convScript) (*conversation, *recordingRunner) {
	rr := &recordingRunner{script: script}
	build := func(canvas capabilities.Canvas, confirm agent.ConfirmFunc, onText, onReasoning func(string), onTool func(string, []byte), _ func(string, []byte, bool), _ func(agent.ContextStats, int), onCompact func(agent.CompactStat)) (AgentRunner, error) {
		rr.onText, rr.onReasoning, rr.onTool, rr.confirm = onText, onReasoning, onTool, confirm
		rr.onCompact = onCompact
		rr.canvasOK = canvas != nil
		return rr, nil
	}
	return newConversation(s, build), rr
}

func testSessionProd() Session  { return Session{EnvName: "prod", IsProd: true} }
func testSessionLocal() Session { return Session{EnvName: "local"} }

// pump drives a non-blocking agent flow to completion: it calls each returned cmd
// and feeds the resulting msg back into Update until the turn is done (or a safety
// cap). Confirm flows in a non-prod env auto-resolve through this loop.
func pumpConv(c *conversation, cmd tea.Cmd) {
	for i := 0; cmd != nil && i < 100; i++ {
		msg := cmd()
		if msg == nil {
			return
		}
		cmd = c.Update(msg)
		if _, done := msg.(agentDoneMsg); done {
			return
		}
	}
}

// keyRunes builds a printable key-press event for s (a v2 KeyPressMsg carries the
// characters in Text; a single rune also sets Code so String()/matching work).
func keyRunes(s string) tea.KeyPressMsg {
	r := []rune(s)
	k := tea.KeyPressMsg{Text: s}
	if len(r) == 1 {
		k.Code = r[0]
	}
	return k
}

func TestView_FillsHeightWithPromptPinnedToBottom(t *testing.T) {
	c := newConversation(testSession(), nil)
	c.appendLine("you", "hi")
	const h = 12
	out := c.View(60, h)
	lines := strings.Split(out, "\n")
	if len(lines) != h {
		t.Fatalf("the chat pane must fill its full height: got %d lines, want %d", len(lines), h)
	}
	last := lines[len(lines)-1]
	if !strings.Contains(last, convPromptPrefix) {
		t.Fatalf("the prompt must be the bottom-most line (pinned to the bottom), got %q", last)
	}
	// The short transcript bottom-aligns, so the line just above the prompt area is
	// not the title — there is blank padding between the header and the content.
	if strings.Contains(lines[1], "hi") {
		t.Fatalf("a short transcript should bottom-align (blank padding under the header), got line1=%q", lines[1])
	}
}

func TestTypewriter_RevealsGradually(t *testing.T) {
	c := newConversation(testSession(), nil)
	c.running = true
	c.beginAssistant()
	c.feed("hello world from nexus") // full text buffered, not yet shown
	if got := c.visibleAssistant(); len([]rune(got)) >= len([]rune("hello world from nexus")) {
		t.Fatalf("text must not be fully revealed before any tick, got %q", got)
	}
	for i := 0; i < 100; i++ {
		c.revealStep()
	}
	if got := c.visibleAssistant(); got != "hello world from nexus" {
		t.Fatalf("after enough ticks the full text must be revealed, got %q", got)
	}
}

func TestTypewriter_FinishFlushesRemainder(t *testing.T) {
	c := newConversation(testSession(), nil)
	c.running = true
	c.beginAssistant()
	c.feed("the quick brown fox")
	c.revealStep() // only partially revealed
	c.finish(agentDoneMsg{final: ""})
	if got := c.visibleAssistant(); got != "the quick brown fox" {
		t.Fatalf("finish must flush the unrevealed remainder, got %q", got)
	}
}

func containsLineTag(lines []convLine, tag string) bool {
	for _, l := range lines {
		if l.tag == tag {
			return true
		}
	}
	return false
}

func TestQueue_EnterWhileRunningEnqueues(t *testing.T) {
	c, _ := newTestConv(testSessionLocal(), func(_ func(string), _ func(string), _ func(string, []byte), _ agent.ConfirmFunc) (string, error) {
		return "ok", nil
	})
	c.submit("first") // starts a turn (running=true)
	c.input.SetValue("second")
	c.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter}) // running → must enqueue, not drop
	if len(c.pending) != 1 || c.pending[0] != "second" {
		t.Fatalf("a message sent while running must enqueue, got pending=%v", c.pending)
	}
	if !containsLineTag(c.lines, "queued") {
		t.Fatalf("queued message must show a queued transcript line: %+v", c.lines)
	}
}

func TestQueue_DrainsNextOnFinish(t *testing.T) {
	c, _ := newTestConv(testSessionLocal(), func(_ func(string), _ func(string), _ func(string, []byte), _ agent.ConfirmFunc) (string, error) {
		return "ok", nil
	})
	cmd := c.submit("first") // first turn (running)
	c.input.SetValue("second")
	c.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter}) // enqueue "second"
	// Drive the first turn to completion: pumpConv processes its agentDoneMsg, which
	// calls finish → drains "second" as a fresh turn. Pumping (rather than calling
	// finish by hand) guarantees the first turn's goroutine has fully returned before
	// the second starts, so turns stay strictly serial (no overlapping goroutines).
	pumpConv(c, cmd)
	if !c.running {
		t.Fatal("finishing a turn with a queued message must immediately start the next turn")
	}
	if len(c.pending) != 0 {
		t.Fatalf("the drained message must leave the queue empty, got %v", c.pending)
	}
}

func TestQueue_ClearEmptiesQueue(t *testing.T) {
	c, _ := newTestConv(testSessionLocal(), func(_ func(string), _ func(string), _ func(string, []byte), _ agent.ConfirmFunc) (string, error) {
		return "ok", nil
	})
	c.submit("first")
	c.input.SetValue("second")
	c.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	c.agentCommand("/clear")
	if len(c.pending) != 0 || len(c.lines) != 0 {
		t.Fatalf("/clear must empty the transcript and the queue, got lines=%d pending=%v", len(c.lines), c.pending)
	}
}

func TestStartQueued_BuildFailureSurfacesSys(t *testing.T) {
	c := newConversation(testSession(), nil) // nil build → agent unavailable
	cmd := c.startQueued("run me")
	if cmd != nil {
		t.Fatal("a queued message that cannot build the agent must not start a turn")
	}
	if c.running {
		t.Fatal("a failed startQueued must not leave the conversation running")
	}
	if !containsLineTag(c.lines, "sys") {
		t.Fatalf("a build failure draining the queue must surface a sys line: %+v", c.lines)
	}
}

func TestSetModel_ResetsAgentAndUpdatesSession(t *testing.T) {
	c, _ := newTestConv(testSessionLocal(), func(_ func(string), _ func(string), _ func(string, []byte), _ agent.ConfirmFunc) (string, error) {
		return "ok", nil
	})
	cmd := c.submit("hi")
	pumpConv(c, cmd) // build the agent + complete the turn (agentBuilt=true)
	if !c.agentBuilt {
		t.Fatal("precondition: the agent should be built after a turn")
	}
	c.setModel("new-model-x")
	if c.agentBuilt {
		t.Fatal("setModel must reset the agent so the next turn rebuilds with the new model")
	}
	if c.session.Model != "new-model-x" {
		t.Fatalf("setModel must update the conversation session model, got %q", c.session.Model)
	}
	if !containsLineTag(c.lines, "sys") {
		t.Fatal("setModel must surface a sys confirmation line")
	}
}

func TestInterrupt_EscCancelsCurrentTurnThenContinuesQueue(t *testing.T) {
	c, _ := newTestConv(testSessionLocal(), func(_ func(string), _ func(string), _ func(string, []byte), _ agent.ConfirmFunc) (string, error) {
		return "ok", nil
	})
	cmd := c.submit("first")
	c.input.SetValue("second")
	c.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter}) // enqueue
	c.handleKey(tea.KeyPressMsg{Code: tea.KeyEsc})   // interrupt the running turn
	if !c.interrupted {
		t.Fatal("esc while running must mark the current turn interrupted")
	}
	if len(c.pending) != 1 {
		t.Fatalf("interrupt must keep the queue (interrupted this turn, not the plan), got %v", c.pending)
	}
	if !containsLineTag(c.lines, "sys") {
		t.Fatal("interrupt must surface a sys 'interrupted' line")
	}
	// The cancelled turn's terminal message (delivered by the outstanding drain)
	// finalizes through finish — the single finalizer — which drains the kept queue
	// as the next turn. This keeps turns strictly serial (no second drain).
	pumpConv(c, cmd)
	if len(c.pending) != 0 {
		t.Fatalf("the kept queue must drain after the interrupted turn finalizes, got %v", c.pending)
	}
	sys := 0
	for _, l := range c.lines {
		if l.tag == "sys" {
			sys++
		}
	}
	if sys != 1 {
		t.Fatalf("interrupt must not double-message: want exactly 1 sys line, got %d (%+v)", sys, c.lines)
	}
}

func TestClear_ResetsAgentContext(t *testing.T) {
	c, _ := newTestConv(testSessionLocal(), func(_ func(string), _ func(string), _ func(string, []byte), _ agent.ConfirmFunc) (string, error) {
		return "ok", nil
	})
	if err := c.ensureAgent(); err != nil {
		t.Fatalf("ensureAgent: %v", err)
	}
	if !c.agentBuilt {
		t.Fatal("precondition: agent built")
	}
	c.agentCommand("/clear")
	if c.agentBuilt {
		t.Fatal("/clear must reset the agent so the next turn rebuilds with a fresh context (not just clear the UI)")
	}
}

func TestClear_WhileRunningTearsDownTurn(t *testing.T) {
	c, _ := newTestConv(testSessionLocal(), func(_ func(string), _ func(string), _ func(string, []byte), _ agent.ConfirmFunc) (string, error) {
		return "ok", nil
	})
	cmd := c.submit("first")
	c.input.SetValue("queued")
	c.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter}) // enqueue
	c.agentCommand("/clear")                         // clear while a turn runs
	if !c.interrupted {
		t.Fatal("/clear while running must cancel the in-flight turn")
	}
	if len(c.lines) != 0 || len(c.pending) != 0 {
		t.Fatalf("/clear must wipe transcript + queue, got lines=%d pending=%v", len(c.lines), c.pending)
	}
	// The cancelled turn finalizes through finish; with an empty queue the chat
	// returns to idle rather than leaving an orphaned running turn.
	pumpConv(c, cmd)
	if c.running {
		t.Fatal("after /clear the cancelled turn must finalize to idle, not stay running")
	}
	if len(c.pending) != 0 {
		t.Fatalf("queue stays empty after /clear, got %v", c.pending)
	}
}

func TestSpinner_ShowsElapsedWhileRunning(t *testing.T) {
	c := newConversation(testSession(), nil)
	c.startedAt = time.Now().Add(-3 * time.Second)
	c.running = true
	if got := c.statusLine(); !strings.Contains(got, "working") || !strings.Contains(got, "3s") {
		t.Fatalf("running status line must show a working spinner + elapsed seconds, got %q", got)
	}
}

func TestSpinner_EmptyWhenIdle(t *testing.T) {
	c := newConversation(testSession(), nil)
	if got := c.statusLine(); got != "" {
		t.Fatalf("idle status line must be empty, got %q", got)
	}
}

func TestConvTick_AdvancesRevealAndReissues(t *testing.T) {
	c := newConversation(testSession(), nil)
	c.running = true
	c.beginAssistant()
	c.feed("abcdefghij")
	cmd := c.tickUpdate()
	if c.visibleAssistant() == "" {
		t.Fatal("a convTick must advance the typewriter reveal")
	}
	if cmd == nil {
		t.Fatal("the conversation animation clock must re-issue the tick")
	}
}

func TestConversationSubmitStartsTurnAndShowsUser(t *testing.T) {
	c, rr := newTestConv(testSessionLocal(), func(onText func(string), _ func(string), _ func(string, []byte), _ agent.ConfirmFunc) (string, error) {
		onText("answer")
		return "answer", nil
	})
	c.setActiveView("Cost")
	c.input.SetValue("why is spend up?")
	cmd := c.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	if !c.running {
		t.Fatal("submit should mark the conversation running")
	}
	if !strings.Contains(c.View(60, 20), "why is spend up?") {
		t.Fatalf("transcript should show the user line:\n%s", c.View(60, 20))
	}
	pumpConv(c, cmd)
	if rr.gotView != "Cost" {
		t.Fatalf("turn should carry the active view as context, got %q", rr.gotView)
	}
	if rr.gotText != "why is spend up?" {
		t.Fatalf("turn should carry the user text, got %q", rr.gotText)
	}
	if c.running {
		t.Fatal("turn should clear running once done")
	}
}

func TestConversationStreamedTextAccumulates(t *testing.T) {
	c, _ := newTestConv(testSessionLocal(), func(onText func(string), _ func(string), _ func(string, []byte), _ agent.ConfirmFunc) (string, error) {
		onText("foo")
		onText("bar")
		return "foobar", nil
	})
	c.input.SetValue("hi")
	pumpConv(c, c.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter}))
	if !strings.Contains(c.View(80, 20), "foobar") {
		t.Fatalf("streamed deltas should accumulate into one assistant line:\n%s", c.View(80, 20))
	}
}

func TestConversationReasoningRendersDistinctThinkBlock(t *testing.T) {
	c, _ := newTestConv(testSessionLocal(), func(onText, onReasoning func(string), _ func(string, []byte), _ agent.ConfirmFunc) (string, error) {
		onReasoning("let me weigh the options")
		onText("the answer")
		return "the answer", nil
	})
	c.input.SetValue("decide")
	pumpConv(c, c.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter}))

	// Reasoning lands on its own "think" line, distinct from the assistant answer.
	var think, asst *convLine
	for i := range c.lines {
		switch c.lines[i].tag {
		case "think":
			think = &c.lines[i]
		case "asst":
			asst = &c.lines[i]
		}
	}
	if think == nil || !strings.Contains(think.text, "weigh the options") {
		t.Fatalf("reasoning must render on a distinct think line, got lines=%+v", c.lines)
	}
	if asst == nil || !strings.Contains(asst.text, "the answer") {
		t.Fatalf("the answer must still render on its own asst line, got lines=%+v", c.lines)
	}
	// The thinking block precedes the answer (subordinate, above the answer).
	out := stripSGR(c.View(60, 20))
	if !strings.Contains(out, "✱") {
		t.Fatalf("the think block must carry its distinct ✱ glyph:\n%s", out)
	}
	if ti, ai := strings.Index(out, "weigh"), strings.Index(out, "the answer"); ti < 0 || ai < 0 || ti > ai {
		t.Fatalf("reasoning should appear above the answer, got think@%d answer@%d:\n%s", ti, ai, out)
	}
}

func TestConversationReasoningInterleavedDoesNotTruncateAnswer(t *testing.T) {
	// Answer segment, then reasoning, then a second answer segment. The first asst
	// segment must finalize to its full text (not freeze at a typewriter prefix)
	// when the think block pushes it off the trailing line, and neither segment may
	// be lost or duplicated.
	c, _ := newTestConv(testSessionLocal(), func(onText, onReasoning func(string), _ func(string, []byte), _ agent.ConfirmFunc) (string, error) {
		onText("the first part")
		onReasoning("reconsidering")
		onText("the second part")
		return "the first partthe second part", nil
	})
	c.input.SetValue("go")
	pumpConv(c, c.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter}))

	var asst []string
	thinkBetween := false
	for _, ln := range c.lines {
		switch ln.tag {
		case "asst":
			asst = append(asst, ln.text)
		case "think":
			if len(asst) == 1 { // a think line landed after the first asst segment
				thinkBetween = true
			}
		}
	}
	if len(asst) != 2 || asst[0] != "the first part" || asst[1] != "the second part" {
		t.Fatalf("interleaved answer must keep both full segments in order, got asst=%q", asst)
	}
	if !thinkBetween {
		t.Fatalf("the reasoning block must sit between the two answer segments, got lines=%+v", c.lines)
	}
}

func TestConversationRendersMarkdownOnFinalAnswer(t *testing.T) {
	c, _ := newTestConv(testSessionLocal(), func(onText, _ func(string), _ func(string, []byte), _ agent.ConfirmFunc) (string, error) {
		onText("**bold** and more")
		return "**bold** and more", nil
	})
	c.input.SetValue("hi")
	pumpConv(c, c.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter}))
	out := c.View(80, 24)
	if !strings.Contains(out, "bold") {
		t.Fatalf("the finalized answer should still show its text:\n%s", out)
	}
	if strings.Contains(out, "**bold**") {
		t.Fatalf("markdown syntax should be rendered, not shown literally:\n%s", out)
	}
}

func TestConversationScrollback(t *testing.T) {
	c := newConversation(testSessionLocal(), nil)
	for i := 0; i < 40; i++ {
		c.appendLine("sys", fmt.Sprintf("line-%02d", i))
	}
	// Default render shows the newest tail, not the oldest line.
	if out := stripSGR(c.View(50, 12)); !strings.Contains(out, "line-39") || strings.Contains(out, "line-00") {
		t.Fatalf("default view must show the newest tail:\n%s", out)
	}
	// PgUp scrolls toward older content until the top is visible.
	for i := 0; i < 25; i++ {
		c.handleKey(tea.KeyPressMsg{Code: tea.KeyPgUp})
		c.View(50, 12) // re-render so geometry/clamp track each step
	}
	if out := stripSGR(c.View(50, 12)); !strings.Contains(out, "line-00") {
		t.Fatalf("PgUp must reveal the oldest content:\n%s", out)
	}
	// Clamp: another PgUp at the top is a no-op.
	prev := c.scroll
	c.handleKey(tea.KeyPressMsg{Code: tea.KeyPgUp})
	c.View(50, 12)
	if c.scroll != prev {
		t.Fatalf("scroll must clamp at the oldest line, got %d want %d", c.scroll, prev)
	}
	// PgDn returns to the bottom (scroll 0).
	for i := 0; i < 25; i++ {
		c.handleKey(tea.KeyPressMsg{Code: tea.KeyPgDown})
		c.View(50, 12)
	}
	if c.scroll != 0 {
		t.Fatalf("PgDn must return to the bottom, got scroll=%d", c.scroll)
	}
}

// TestConversationScrollKeysAndStatus covers ↑/↓ line scroll, the interrupting
// status line, and the help-string variants (scrolled / running / idle).
func TestConversationScrollKeysAndStatus(t *testing.T) {
	c := newConversation(testSessionLocal(), nil)
	for i := 0; i < 30; i++ {
		c.appendLine("sys", fmt.Sprintf("l%02d", i))
	}
	c.View(50, 10) // establish geometry
	// ↑ scrolls back a line; help reflects the scrolled state.
	c.handleKey(tea.KeyPressMsg{Code: tea.KeyUp})
	if c.scroll == 0 {
		t.Fatal("↑ should scroll the transcript back")
	}
	if !strings.Contains(c.Help(), "scrolled back") {
		t.Fatalf("scrolled help wrong: %q", c.Help())
	}
	// ↓ returns toward the bottom.
	for i := 0; i < 40; i++ {
		c.handleKey(tea.KeyPressMsg{Code: tea.KeyDown})
		c.View(50, 10)
	}
	if c.scroll != 0 {
		t.Fatalf("↓ should return to the bottom, got %d", c.scroll)
	}
	// Running help + the interrupting status line.
	c.running = true
	if !strings.Contains(c.Help(), "interrupts") {
		t.Fatalf("running help wrong: %q", c.Help())
	}
	if s := c.statusLine(); !strings.Contains(s, "working") {
		t.Fatalf("running status should say working: %q", s)
	}
	c.interrupted = true
	if s := c.statusLine(); !strings.Contains(s, "interrupting") {
		t.Fatalf("interrupted status should say interrupting: %q", s)
	}
}

func TestConversationToolStartRendersProgressLine(t *testing.T) {
	c, _ := newTestConv(testSessionLocal(), func(_ func(string), _ func(string), onTool func(string, []byte), _ agent.ConfirmFunc) (string, error) {
		onTool("observe_cost", nil)
		return "done", nil
	})
	c.input.SetValue("cost?")
	pumpConv(c, c.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter}))
	if !strings.Contains(c.View(60, 20), "▸ observe_cost") {
		t.Fatalf("a tool start should render a progress line:\n%s", c.View(60, 20))
	}
}

func TestConversationConfirmApproveNonProd(t *testing.T) {
	approved := make(chan bool, 1)
	c, _ := newTestConv(testSessionLocal(), func(_ func(string), _ func(string), _ func(string, []byte), confirm agent.ConfirmFunc) (string, error) {
		ok, _ := confirm(context.Background(), stubTool{name: "mitigate_cache_flush"}, nil, "authorize cache flush")
		approved <- ok
		return "flushed", nil
	})
	c.input.SetValue("flush the cache")
	cmd := c.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter}) // starts the turn
	// Non-prod ALSO raises the gate now (every mutation is confirmed) — drain until up.
	for range 10 {
		if c.awaitingConfirm {
			break
		}
		cmd = c.Update(cmd())
	}
	if !c.awaitingConfirm || !c.cf.Capturing() {
		t.Fatal("non-prod should ALSO raise the confirm gate, not auto-fire")
	}
	// allow (y) → approve and return true to the agent.
	approveCmd := c.handleKey(keyRunes("y"))
	if approveCmd == nil {
		t.Fatal("allow should produce the approve reply")
	}
	pumpConv(c, approveCmd)
	if !<-approved {
		t.Fatal("allowing the gate should approve the mitigation")
	}
}

func TestConversationConfirmProdTypedConfirm(t *testing.T) {
	decided := make(chan bool, 1)
	c, _ := newTestConv(testSessionProd(), func(_ func(string), _ func(string), _ func(string, []byte), confirm agent.ConfirmFunc) (string, error) {
		okv, _ := confirm(context.Background(), stubTool{name: "mitigate_vk_revoke"}, nil, "revoke vk")
		decided <- okv
		return "revoked", nil
	})
	c.input.SetValue("revoke that key")
	cmd := c.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter}) // starts the turn (drain cmd)

	// Drain until the confirm request raises the prod gate.
	for range 10 {
		if c.awaitingConfirm {
			break
		}
		cmd = c.Update(cmd())
	}
	if !c.awaitingConfirm || !c.cf.Capturing() {
		t.Fatal("prod env should raise the typed-confirm gate, not auto-fire")
	}
	// Quick-allow the prod gate (y) → approve.
	approveCmd := c.handleKey(keyRunes("y"))
	if approveCmd == nil {
		t.Fatal("allow should produce the approve reply")
	}
	pumpConv(c, approveCmd)
	if !<-decided {
		t.Fatal("allowing the gate should approve the mitigation")
	}
}

func TestConversationConfirmCancelDeclines(t *testing.T) {
	decided := make(chan bool, 1)
	c, _ := newTestConv(testSessionProd(), func(_ func(string), _ func(string), _ func(string, []byte), confirm agent.ConfirmFunc) (string, error) {
		okv, _ := confirm(context.Background(), stubTool{name: "mitigate_routing_rule_enabled"}, nil, "toggle rule")
		decided <- okv
		return "noop", nil
	})
	c.input.SetValue("disable that rule")
	cmd := c.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	for range 10 {
		if c.awaitingConfirm {
			break
		}
		cmd = c.Update(cmd())
	}
	if !c.awaitingConfirm {
		t.Fatal("prod gate should be awaiting confirm")
	}
	// esc cancels the gate → decline.
	declineCmd := c.handleKey(tea.KeyPressMsg{Code: tea.KeyEsc})
	pumpConv(c, declineCmd)
	if <-decided {
		t.Fatal("esc-cancelling the gate must decline the mitigation (false)")
	}
}

// TestConversationFinishDropsPendingConfirmGate guards the fail-open fix: a turn
// that ends while a prod confirm is still raised must drop the gate + awaiting
// flag, so a later keystroke cannot resolve the dead turn's confirm onto the next.
func TestConversationFinishDropsPendingConfirmGate(t *testing.T) {
	c, _ := newTestConv(testSessionProd(), nil)
	c.beginConfirm(agentConfirmMsg{tool: "mitigate_cache_flush", reason: "flush"})
	if !c.awaitingConfirm || !c.cf.Capturing() {
		t.Fatal("precondition: the prod confirm gate is raised")
	}
	c.finish(agentDoneMsg{final: "torn down"})
	if c.awaitingConfirm {
		t.Fatal("a finished turn must clear awaitingConfirm (no carried-over authorization)")
	}
	if c.cf.Capturing() {
		t.Fatal("a finished turn must drop the typed-confirm gate")
	}
}

func TestConversationDoneSurfacesError(t *testing.T) {
	c, _ := newTestConv(testSessionLocal(), func(_ func(string), _ func(string), _ func(string, []byte), _ agent.ConfirmFunc) (string, error) {
		return "partial", context.DeadlineExceeded
	})
	c.input.SetValue("do a thing")
	pumpConv(c, c.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter}))
	out := c.View(70, 20)
	if !strings.Contains(out, "ask again to continue") {
		t.Fatalf("a turn error should surface a continue hint:\n%s", out)
	}
	if c.running {
		t.Fatal("a turn error should still clear running")
	}
}

func TestConversationFinalFillsEmptyAssistantLine(t *testing.T) {
	// Agent returns a final answer without streaming any text (pure tool turn).
	c, _ := newTestConv(testSessionLocal(), func(_ func(string), _ func(string), onTool func(string, []byte), _ agent.ConfirmFunc) (string, error) {
		onTool("observe_alerts", nil)
		return "3 alerts are firing", nil
	})
	c.input.SetValue("any alerts?")
	pumpConv(c, c.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter}))
	if !strings.Contains(stripSGR(c.View(70, 20)), "3 alerts are firing") {
		t.Fatalf("final answer should fill the empty assistant line:\n%s", c.View(70, 20))
	}
}

func TestConversationSlashClearAndHelp(t *testing.T) {
	c, _ := newTestConv(testSessionLocal(), func(onText func(string), _ func(string), _ func(string, []byte), _ agent.ConfirmFunc) (string, error) {
		onText("hello")
		return "hello", nil
	})
	c.input.SetValue("hi")
	pumpConv(c, c.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter}))
	if len(c.lines) == 0 {
		t.Fatal("expected transcript lines before clear")
	}
	c.input.SetValue("/clear")
	c.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	if len(c.lines) != 0 {
		t.Fatalf("/clear should empty the transcript, got %d lines", len(c.lines))
	}
	if c.notice == "" {
		t.Fatal("/clear should set a notice")
	}
	c.input.SetValue("/help")
	c.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	c.flushReveal() // /help types out; snap it to the full reference for the assertion
	help := ""
	for _, ln := range c.lines {
		if ln.tag == "help" {
			help = ln.text
		}
	}
	for _, want := range []string{"keys & commands", "Slash commands", "/model", "tab"} {
		if !strings.Contains(help, want) {
			t.Fatalf("/help should render a full reference incl. %q, got %q", want, help)
		}
	}
	c.input.SetValue("/bogus")
	c.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	if !strings.Contains(c.notice, "unknown") {
		t.Fatalf("an unknown command should be reported, got %q", c.notice)
	}
}

func TestConversationAgentUnavailable(t *testing.T) {
	c := newConversation(testSessionLocal(), nil) // nil build seam → no agent
	c.input.SetValue("hello?")
	cmd := c.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd != nil {
		t.Fatal("with no agent, a submit should not start a turn")
	}
	if c.running {
		t.Fatal("with no agent, running must stay false")
	}
	if !strings.Contains(c.View(60, 20), "agent unavailable") {
		t.Fatalf("a missing agent should be surfaced:\n%s", c.View(60, 20))
	}
}

func TestConversationEmptyTranscriptHintAndCapturing(t *testing.T) {
	c, _ := newTestConv(testSessionLocal(), nil)
	if c.Capturing() {
		t.Fatal("an unfocused conversation should not capture keystrokes")
	}
	c.focus()
	if !c.Capturing() {
		t.Fatal("a focused conversation should capture keystrokes")
	}
	if !strings.Contains(c.View(60, 20), "Ask Nexus") {
		t.Fatalf("empty transcript should show the onboarding hint:\n%s", c.View(60, 20))
	}
	c.blur()
	if c.Capturing() {
		t.Fatal("a blurred conversation should release keystrokes")
	}
}

func TestConversationInitAndHelpStates(t *testing.T) {
	c, _ := newTestConv(testSessionProd(), nil)
	if c.Init() == nil {
		t.Fatal("Init should return the input blink cmd")
	}
	if !strings.Contains(c.Help(), "send") {
		t.Fatalf("idle help should mention send, got %q", c.Help())
	}
	c.running = true
	if !strings.Contains(c.Help(), "working") {
		t.Fatalf("running help should mention working, got %q", c.Help())
	}
	c.running = false
	// Raise the prod gate so help reflects the typed-confirm hint.
	c.beginConfirm(agentConfirmMsg{tool: "mitigate_cache_flush", reason: "r"})
	if !strings.Contains(c.Help(), "confirm") {
		t.Fatalf("confirming help should be the gate's allow/deny hint, got %q", c.Help())
	}
}

func TestConversationDrainCmdAndStopNilSafe(t *testing.T) {
	c, _ := newTestConv(testSessionLocal(), func(_ func(string), _ func(string), _ func(string, []byte), _ agent.ConfirmFunc) (string, error) {
		return "ok", nil
	})
	if c.drainCmd() != nil {
		t.Fatal("drainCmd should be nil before the agent is built")
	}
	c.stop() // nil bridge → must not panic
	if err := c.ensureAgent(); err != nil {
		t.Fatalf("ensureAgent should succeed with a build seam: %v", err)
	}
	if c.drainCmd() == nil {
		t.Fatal("drainCmd should be non-nil once the agent/bridge exists")
	}
	c.stop() // real bridge, no turn yet → still nil-safe
}

func TestConversationViewShowsConfirmGate(t *testing.T) {
	c, _ := newTestConv(testSessionProd(), nil)
	c.beginConfirm(agentConfirmMsg{tool: "mitigate_provider_enabled", reason: "disable openai"})
	out := c.View(70, 24)
	if !strings.Contains(out, "PROD") || !strings.Contains(out, "mitigate_provider_enabled") {
		t.Fatalf("View should render the prod allow/deny gate:\n%s", out)
	}
}

func TestConversationTranscriptTailTrims(t *testing.T) {
	c, _ := newTestConv(testSessionLocal(), nil)
	for i := range 40 {
		c.appendLine("sys", fmt.Sprintf("line-%d", i))
	}
	// A short box must drop the oldest lines and keep the newest.
	out := c.View(60, 10)
	if strings.Contains(out, "line-0") {
		t.Fatalf("oldest line should be trimmed in a short box:\n%s", out)
	}
	if !strings.Contains(out, "line-39") {
		t.Fatalf("newest line must stay visible:\n%s", out)
	}
}

func TestConversationHandleKeyEditsInput(t *testing.T) {
	c, _ := newTestConv(testSessionLocal(), nil)
	c.focus()
	c.handleKey(keyRunes("h"))
	c.handleKey(keyRunes("i"))
	if got := c.input.Value(); got != "hi" {
		t.Fatalf("typing should edit the prompt, got %q", got)
	}
}

func TestConversationEnsureAgentBuildError(t *testing.T) {
	boom := errors.New("skill dir unreadable")
	c := newConversation(testSessionLocal(), func(_ capabilities.Canvas, _ agent.ConfirmFunc, _, _ func(string), _ func(string, []byte), _ func(string, []byte, bool), _ func(agent.ContextStats, int), _ func(agent.CompactStat)) (AgentRunner, error) {
		return nil, boom
	})
	c.input.SetValue("hello")
	if cmd := c.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter}); cmd != nil {
		t.Fatal("a build error should not start a turn")
	}
	if !strings.Contains(c.View(60, 20), "skill dir unreadable") {
		t.Fatalf("a build error should be surfaced in the transcript:\n%s", c.View(60, 20))
	}
	// A second submit must not retry the build (attempted once).
	c.input.SetValue("again")
	c.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	if c.running {
		t.Fatal("a persistent build error should keep the conversation idle")
	}
}

func TestConversationConfirmReplyNilBridgeSafe(t *testing.T) {
	c, _ := newTestConv(testSessionLocal(), nil)
	// A confirm reply with no agent/bridge built must not panic and starts nothing.
	if cmd := c.Update(confirmReplyMsg{ok: true}); cmd != nil {
		t.Fatal("confirmReply with no bridge should be a safe no-op")
	}
	// An unrelated message type is ignored (the conversation only folds its own).
	if cmd := c.Update(tea.WindowSizeMsg{Width: 80, Height: 24}); cmd != nil {
		t.Fatal("an unrelated message should be a no-op for the conversation")
	}
}

func TestConversationViewClampAndNotice(t *testing.T) {
	c, _ := newTestConv(testSessionLocal(), nil)
	c.notice = "cleared"
	c.appendLine("you", "hi there")
	// width 0 clamps; tiny height clamps the transcript budget — neither panics.
	out := c.View(0, 3)
	if out == "" {
		t.Fatal("View should still render at clamped width/height")
	}
	if !strings.Contains(c.View(60, 20), "cleared") {
		t.Fatalf("a set notice should render:\n%s", c.View(60, 20))
	}
}

func TestConversationIgnoresEmptyAndTypingWhileRunning(t *testing.T) {
	c, _ := newTestConv(testSessionLocal(), func(_ func(string), _ func(string), _ func(string, []byte), _ agent.ConfirmFunc) (string, error) {
		return "ok", nil
	})
	// Empty enter is a no-op.
	if cmd := c.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter}); cmd != nil {
		t.Fatal("empty enter should be a no-op")
	}
	// Force running and assert typing is ignored.
	c.running = true
	if cmd := c.handleKey(keyRunes("x")); cmd != nil {
		t.Fatal("typing while the agent works should be ignored")
	}
}

package shell

import (
	"context"
	"encoding/json"

	tea "charm.land/bubbletea/v2"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/agent"
	capabilities "github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/capabilities/runtime"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/kit"
)

// AgentRunner is the bridge's view of a built agent. *agent.Agent satisfies it;
// tests inject a fake so the bridge plumbing runs without a model. It is exported
// so the CLI's BuildAgent seam (cli/root.go) can name it as the return type.
type AgentRunner interface {
	Turn(ctx context.Context, userText, activeView string) (string, error)
	// Compact summarizes the session's older transcript now (the manual /compact
	// path). It reports the stat, whether it acted (false = nothing to compact), and
	// any error.
	Compact(ctx context.Context) (agent.CompactStat, bool, error)
}

// AgentBuildFunc is the seam the CLI implements to construct the gateway agent
// the conversation drives. The TUI owns the bridge and hands the CLI the canvas
// (view-driving), the blocking confirm gate, and the streaming callbacks; the CLI
// supplies what only it knows (model / VK / env / on-disk memory + session paths)
// and calls capabilities.BuildAgent. A nil seam disables the conversation.
type AgentBuildFunc func(canvas capabilities.Canvas, confirm agent.ConfirmFunc, onText, onReasoning func(string), onToolStart func(name string, input []byte), onToolEnd func(name string, output []byte, isError bool), onContext func(stats agent.ContextStats, window int), onCompact func(agent.CompactStat)) (AgentRunner, error)

// --- messages the bridge emits into the Bubble Tea loop ---

// agentTextMsg is one streamed assistant text delta.
type agentTextMsg struct{ delta string }

// agentReasoningMsg is one streamed reasoning/thinking delta — display-only, shown
// in a distinct dim style and never persisted to the agent transcript.
type agentReasoningMsg struct{ delta string }

// agentToolMsg announces a tool call starting (name + input args).
type agentToolMsg struct {
	name  string
	input []byte
}

// agentToolResultMsg reports a tool call's result (raw output + error flag), for the
// transcript's dim result peek + the expandable full I/O.
type agentToolResultMsg struct {
	name    string
	output  []byte
	isError bool
}

// agentNavMsg / agentShowMsg / agentHighlightMsg are canvas drives: the agent
// asked to open a view / show an event / highlight a row. The root model applies
// them so the cockpit follows the agent live.
type agentNavMsg struct {
	view   string
	filter core.TrafficFilter
}
type agentShowMsg struct{ id string }
type agentHighlightMsg struct{ ref string }

// agentConfirmMsg asks the human to authorize a mitigation. The root model raises
// the confirm gate; the reply rides back on the bridge's reply channel.
type agentConfirmMsg struct {
	tool   string
	reason string
}

// contextStatsMsg carries the post-turn context-usage stats + the current model's
// context window to the conversation pane's context indicator.
type contextStatsMsg struct {
	stats  agent.ContextStats
	window int
}

// agentCompactMsg announces a compaction so the conversation can surface a visible
// notice (like a tool call) and refresh the context indicator. acted=false carries
// no stat — it is the manual /compact "nothing to compact" outcome.
type agentCompactMsg struct {
	stat  agent.CompactStat
	acted bool
}

// agentDoneMsg is the terminal result of one Turn (or a manual /compact).
type agentDoneMsg struct {
	final string
	err   error
}

// bridge runs one agent's turns on a background goroutine and pumps the agent's
// streaming callbacks (text/tool/canvas/confirm/done) into the Bubble Tea loop as
// messages. It owns the channels; the root model drains them with drain().
//
// Synchronization model: agent.Turn runs on its own goroutine and blocks on the
// confirm callback. The UI goroutine (Update) drains events and, on a confirm
// request, gathers the human's decision and sends it on replyCh — unblocking the
// agent goroutine. Every channel send the agent goroutine makes is on evCh; the
// only receive it does is replyCh. This is the standard "blocking callback over an
// async event loop" channel handshake.
type bridge struct {
	agent   AgentRunner
	evCh    chan tea.Msg    // text/tool/canvas/confirm/done, drained by drain()
	replyCh chan bool       // the human's confirm decision, read by confirmFunc
	turnCtx context.Context // the in-flight turn's context, read by confirm()
	cancel  context.CancelFunc
	running bool
}

// newBridge wires an AgentRunner. evCh is buffered so canvas/text sends from the
// loop goroutine never block the agent on a slow UI; replyCh is unbuffered (a
// confirm is a true handshake — the agent waits for exactly one reply). The agent
// may be nil at construction and assigned once built (it is needed only by
// startTurn), which resolves the bridge↔agent construction cycle: the agent is
// built with this bridge as its Canvas + Confirm before the bridge holds the agent.
func newBridge(a AgentRunner) *bridge {
	return &bridge{agent: a, evCh: make(chan tea.Msg, 256), replyCh: make(chan bool)}
}

// Navigate/ShowEvent/Highlight implement capabilities.Canvas. Each enqueues a
// canvas message and returns nil immediately — the design forbids a hang (§6), and
// the actual view switch happens when the UI goroutine drains the message.
func (b *bridge) Navigate(view string, filter core.TrafficFilter) error {
	b.sendEv(agentNavMsg{view: view, filter: filter})
	return nil
}
func (b *bridge) ShowEvent(id string) error  { b.sendEv(agentShowMsg{id: id}); return nil }
func (b *bridge) Highlight(ref string) error { b.sendEv(agentHighlightMsg{ref: ref}); return nil }

var _ capabilities.Canvas = (*bridge)(nil)

// sendEv is a non-blocking best-effort send: canvas/text events are UI sugar, so a
// momentarily full buffer drops the event rather than stalling the agent loop.
// Confirm + done are sent with a guaranteed (buffered) send so they are never lost.
func (b *bridge) sendEv(m tea.Msg) {
	select {
	case b.evCh <- m:
	default:
	}
}

// confirmFunc implements agent.ConfirmFunc. It MUST block until the human decides,
// so it raises the request then waits on replyCh. Fail-safe both ways: if the turn
// is already torn down it declines up front; if the context cancels while it waits
// for a decision it declines — a torn-down turn never silently mutates, matching
// the kernel's nil-Confirm rule. The request is a guaranteed (buffered) send so a
// mitigation prompt is never dropped; only one confirm is ever in flight (the loop
// runs confirm-tier tools sequentially), so the buffered send never deadlocks.
func (b *bridge) confirmFunc(ctx context.Context) agent.ConfirmFunc {
	return func(_ context.Context, tool agent.Tool, _ json.RawMessage, reason string) (bool, error) {
		if ctx.Err() != nil {
			return false, nil
		}
		b.evCh <- agentConfirmMsg{tool: tool.Name(), reason: reason}
		select {
		case ok := <-b.replyCh:
			// If the turn was torn down while the human's reply raced in, decline —
			// a cancelled turn must never act on a late authorization (fail-safe).
			if ctx.Err() != nil {
				return false, nil
			}
			return ok, nil
		case <-ctx.Done():
			return false, nil
		}
	}
}

// confirm is the agent.ConfirmFunc the built agent is wired with. The agent is
// constructed once but each Turn runs under its own context (set in startTurn);
// confirm binds to that live turn context so a torn-down turn (stop()/timeout)
// unblocks a parked confirm and declines — never leaking the loop goroutine on
// replyCh. Called with no turn in flight (turnCtx nil) it declines (fail-safe).
func (b *bridge) confirm(callCtx context.Context, tool agent.Tool, input json.RawMessage, reason string) (bool, error) {
	ctx := b.turnCtx
	if ctx == nil {
		return false, nil
	}
	return b.confirmFunc(ctx)(callCtx, tool, input, reason)
}

// startTurn launches one Turn on a background goroutine and returns a Cmd that
// blocks for the first event. Guards against a double-start (one turn at a time);
// a rejected start returns nil.
func (b *bridge) startTurn(userText, activeView string) tea.Cmd {
	if b.running {
		return nil
	}
	b.running = true
	ctx, cancel := context.WithTimeout(context.Background(), kit.AgentTurnTimeout)
	b.turnCtx, b.cancel = ctx, cancel
	go func() {
		final, err := b.agent.Turn(ctx, userText, activeView)
		cancel()
		b.evCh <- agentDoneMsg{final: final, err: err} // buffered send: done is never dropped
	}()
	return b.drain()
}

// startCompact launches a manual /compact on a background goroutine (it makes one
// summary model call, so it must not block the UI) and returns the drain Cmd. The
// acted=true notice rides in via the OnCompact callback during the call; the
// goroutine emits the acted=false notice when there was nothing to compact, then the
// terminal agentDoneMsg. Guards against a double-start like startTurn.
func (b *bridge) startCompact() tea.Cmd {
	if b.running {
		return nil
	}
	b.running = true
	ctx, cancel := context.WithTimeout(context.Background(), kit.AgentTurnTimeout)
	b.turnCtx, b.cancel = ctx, cancel
	go func() {
		_, acted, err := b.agent.Compact(ctx)
		cancel()
		if err == nil && !acted {
			b.evCh <- agentCompactMsg{acted: false}
		}
		b.evCh <- agentDoneMsg{err: err} // buffered send: done is never dropped
	}()
	return b.drain()
}

// drain reads one bridge event and returns it as a tea.Msg. The root model
// re-issues drain() after each non-terminal event; on agentDoneMsg it calls done()
// and stops. This is kit's ChatStreamer.Wait pattern over the agent stream.
func (b *bridge) drain() tea.Cmd {
	return func() tea.Msg { return <-b.evCh }
}

// reply forwards the human's confirm decision to the waiting agent goroutine and
// returns the drain Cmd so the loop keeps pumping. It delivers ONLY to a live
// turn: if the turn already ended (timeout / stop), its confirm has already
// declined via ctx-cancel, so the reply is dropped rather than parked forever on
// an unread channel or left as an orphan a later turn's confirm would wrongly
// consume — a dead turn must never auto-approve a mitigation (fail-safe).
func (b *bridge) reply(ok bool) tea.Cmd {
	ctx := b.turnCtx
	if ctx == nil {
		return nil
	}
	select {
	case b.replyCh <- ok:
		return b.drain()
	case <-ctx.Done():
		return nil
	}
}

// stop cancels an in-flight turn (teardown / quit). Idempotent + nil-safe.
func (b *bridge) stop() {
	if b != nil && b.cancel != nil {
		b.cancel()
	}
}

// done marks the turn finished (root model calls it on agentDoneMsg).
func (b *bridge) done() { b.running = false }

package shell

import (
	"context"
	"github.com/goccy/go-json"
	"sync/atomic"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/agent"
	capabilities "github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/capabilities/runtime"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/kit"
)

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
	// Idle-watchdog state: the turn cancels only when NO progress lands for
	// kit.AgentTurnIdleTimeout. lastActivity is stamped by every bridge event;
	// confirmWait pauses the clock while a confirm card waits on the human.
	lastActivity atomic.Int64
	confirmWait  atomic.Int32
	// idleTimeout overrides kit.AgentTurnIdleTimeout (zero = the default);
	// the watchdog tests inject a tiny window to run deterministically.
	idleTimeout time.Duration
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
	b.touch()
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
		// A confirm card waiting on the HUMAN is not a stuck turn: pause the
		// idle watchdog for the wait and stamp fresh activity when it resolves.
		b.confirmWait.Add(1)
		defer func() { b.confirmWait.Add(-1); b.touch() }()
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

// touch stamps watchdog activity (any streamed delta, tool event, canvas drive,
// or resolved confirm counts as progress).
func (b *bridge) touch() { b.lastActivity.Store(time.Now().UnixNano()) }

// watchIdle cancels the turn only when it makes NO progress for
// kit.AgentTurnIdleTimeout — a turn that is actively streaming, running tools,
// or waiting on a human confirm card is never killed, however long it runs.
// This replaces the old fixed total cap, which severed legitimate long
// multi-tool turns (slow models iterating draft→freeze) mid-stream.
func (b *bridge) watchIdle(ctx context.Context, cancel context.CancelFunc) {
	limit := b.idleTimeout
	if limit <= 0 {
		limit = kit.AgentTurnIdleTimeout
	}
	tick := limit / 60
	if tick < time.Millisecond {
		tick = time.Millisecond
	}
	if tick > 5*time.Second {
		tick = 5 * time.Second
	}
	b.touch()
	go func() {
		t := time.NewTicker(tick)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if b.confirmWait.Load() > 0 {
					continue
				}
				idle := time.Since(time.Unix(0, b.lastActivity.Load()))
				if idle > limit {
					cancel()
					return
				}
			}
		}
	}()
}

// startTurn launches one Turn on a background goroutine and returns a Cmd that
// blocks for the first event. Guards against a double-start (one turn at a time);
// a rejected start returns nil.
func (b *bridge) startTurn(userText, activeView string) tea.Cmd {
	if b.running {
		return nil
	}
	b.running = true
	ctx, cancel := context.WithCancel(context.Background())
	b.turnCtx, b.cancel = ctx, cancel
	b.watchIdle(ctx, cancel)
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
	ctx, cancel := context.WithCancel(context.Background())
	b.turnCtx, b.cancel = ctx, cancel
	b.watchIdle(ctx, cancel)
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
		// Delivery is refused (the dead turn's confirm already declined via
		// ctx-cancel) but the pump must resume: the terminal agentDoneMsg is
		// buffered with no outstanding drain to deliver it.
		return b.drain()
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

package assistant

import (
	"context"
	"encoding/json"
	"sync"
	"time"
)

// bus.go is the P2b command/data-stream split (e90-s2 T1/T2). Before P2b a single
// POST both started a turn AND was the SSE stream, so a dropped connection killed the
// turn and there was no reconnect. P2b detaches the two:
//   - POST /sessions/:id/chat starts a turn in a BACKGROUND goroutine (it outlives the
//     POST request) and returns immediately.
//   - GET  /sessions/:id/stream is a long-lived SSE channel that attaches to the turn's
//     event bus and can reconnect with ?lastSeq= to replay missed committed events.
//   - POST /sessions/:id/interrupt cancels the running turn (the Stop button).
//
// The bus is in-memory, per-pod (Redis has no pub/sub in this system). Cross-pod
// continuity is provided by the owner registry + LB session affinity (the GET stream
// and the POST land on the owning pod), NOT a shared bus.

const (
	// replayRingSize bounds the per-session committed-event ring used for reconnect
	// replay. A turn emits at most a few hundred deltas; 1024 comfortably covers a
	// turn so a reconnect within the same turn never hits a gap. A reconnect older
	// than the window gets the gap signalled (see Attach).
	replayRingSize = 1024

	// liveChanBuffer is the live-delivery channel depth. Generous so a momentarily
	// slow client never overflows mid-turn; on the rare overflow the subscriber is
	// force-closed and the client reconnects with ?lastSeq= (replaying from the ring),
	// turning backpressure into a reconnect rather than a lost event or a blocked turn.
	liveChanBuffer = 1024
)

// streamGrace is how long a detached turn keeps running after its SSE stream drops
// without an explicit interrupt — long enough for a transient network blip to
// reconnect and resume, short enough to bound system-VK billing on an abandoned turn.
// A deliberate Stop / popup-close sends POST /interrupt (immediate cancel); only an
// ungraceful drop relies on this grace. A var (not const) only so tests can shrink it.
var streamGrace = 30 * time.Second

// busEvent is one SSE event: a monotonic per-session sequence number, the event name,
// and its JSON payload. Seq lets a reconnecting stream request only events it missed.
type busEvent struct {
	Seq   int             `json:"seq"`
	Event string          `json:"event"`
	Data  json.RawMessage `json:"data"`
}

// liveSession is the per-session turn state: the committed-event ring (replay source
// of truth), the at-most-one live subscriber, and the in-flight turn's running flag +
// cancel func (for interrupt and the disconnect-grace cancel).
type liveSession struct {
	mu      sync.Mutex
	seq     int
	ring    []busEvent
	sub     chan busEvent // current subscriber's live channel; nil when no stream attached
	closed  bool          // the turn finished — a subscriber drains the ring then exits
	running bool          // a turn is in flight (serialization guard: no second concurrent chat)
	cancel  context.CancelFunc
	graceT  *time.Timer // cancels the turn if no stream reconnects within streamGrace
}

// sessionBus owns the live sessions, keyed by the isolation key (userID:sessionID) so
// one user's session can never be addressed by another (I3).
type sessionBus struct {
	mu       sync.Mutex
	sessions map[string]*liveSession
}

func newSessionBus() *sessionBus {
	return &sessionBus{sessions: make(map[string]*liveSession)}
}

// startTurn registers a turn as running for key and returns its liveSession + the
// turn context (cancelled by interrupt, the disconnect grace, or turnDeadline). It
// reports ok=false when a turn is ALREADY running for key — the serialization guard
// behind the "no new command while one is in flight" rule (enforced server-side as
// defense-in-depth on top of the disabled-input client guard). The caller runs the
// turn in a background goroutine and MUST call finishTurn(key) when it returns.
func (b *sessionBus) startTurn(key string, parent context.Context, deadline time.Duration) (*liveSession, context.Context, bool) {
	b.mu.Lock()
	ls := b.sessions[key]
	if ls == nil {
		ls = &liveSession{}
		b.sessions[key] = ls
	}
	b.mu.Unlock()

	ls.mu.Lock()
	defer ls.mu.Unlock()
	if ls.running {
		return nil, nil, false // a turn is already in flight for this session
	}
	ctx, cancel := context.WithTimeout(parent, deadline)
	ls.running = true
	ls.cancel = cancel
	ls.closed = false
	// A fresh turn resets the event stream: replay only applies within a single turn.
	ls.seq = 0
	ls.ring = ls.ring[:0]
	// Arm the disconnect grace immediately: if NO stream ever attaches (the client got
	// the 202 but died before opening GET /stream, or a non-streaming caller), the turn
	// would otherwise bill the system VK until turnDeadline. The first attach cancels
	// this timer; a later detach re-arms it. Bounds billing on the never-streamed case.
	ls.armGraceLocked()
	return ls, ctx, true
}

// armGraceLocked starts the disconnect-grace timer (caller holds ls.mu) if the turn is
// running, has a cancel, and no timer is already armed. When it fires (no stream
// attached/reconnected within streamGrace) it cancels the turn so an abandoned turn
// stops billing. Idempotent — attach Stops it, finishTurn/interrupt/drop clear it.
func (ls *liveSession) armGraceLocked() {
	if !ls.running || ls.cancel == nil || ls.graceT != nil {
		return
	}
	cancel := ls.cancel
	ls.graceT = time.AfterFunc(streamGrace, func() {
		cancel() // no stream within grace → cancel the abandoned turn (stop billing)
	})
}

// publish appends an event to the session's ring (replay source of truth) and, if a
// stream is attached, delivers it live. A full live channel (slow client) force-closes
// the subscriber so it reconnects with ?lastSeq= and replays from the ring — never
// blocking the turn goroutine and never silently dropping an event.
func (b *sessionBus) publish(key, event string, payload any) {
	b.mu.Lock()
	ls := b.sessions[key]
	b.mu.Unlock()
	if ls == nil {
		return
	}
	data, _ := json.Marshal(payload)
	ls.mu.Lock()
	ls.seq++
	ev := busEvent{Seq: ls.seq, Event: event, Data: data}
	ls.ring = append(ls.ring, ev)
	if len(ls.ring) > replayRingSize {
		ls.ring = ls.ring[len(ls.ring)-replayRingSize:]
	}
	if ls.sub != nil {
		select {
		case ls.sub <- ev:
		default:
			close(ls.sub) // subscriber too slow → force reconnect (replays from ring)
			ls.sub = nil
		}
	}
	ls.mu.Unlock()
}

// finishTurn marks the turn complete and wakes the subscriber to drain + close. The
// session entry is retained (with its ring) so a late reconnect can still replay the
// finished turn's tail; it is reclaimed by the next startTurn (reset) or by drop.
func (b *sessionBus) finishTurn(key string) {
	b.mu.Lock()
	ls := b.sessions[key]
	b.mu.Unlock()
	if ls == nil {
		return
	}
	ls.mu.Lock()
	ls.running = false
	ls.closed = true
	ls.cancel = nil
	if ls.graceT != nil {
		ls.graceT.Stop()
		ls.graceT = nil
	}
	if ls.sub != nil {
		close(ls.sub) // signal the stream the turn is done; it drains the ring then exits
		ls.sub = nil
	}
	ls.mu.Unlock()
}

// interrupt cancels the in-flight turn for key (the Stop button). Reports whether a
// running turn was found. The turn goroutine observes ctx cancellation, emits
// turn_aborted, and calls finishTurn.
func (b *sessionBus) interrupt(key string) bool {
	b.mu.Lock()
	ls := b.sessions[key]
	b.mu.Unlock()
	if ls == nil {
		return false
	}
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if !ls.running || ls.cancel == nil {
		return false
	}
	cancel := ls.cancel
	ls.cancel = nil // consume once: a second interrupt before finishTurn reports false
	if ls.graceT != nil {
		ls.graceT.Stop() // a pending disconnect-grace cancel is now redundant
		ls.graceT = nil
	}
	cancel()
	return true
}

// attachResult carries the replay snapshot a newly-attached stream must write before
// it starts consuming live events: the buffered events with Seq > lastSeq, plus a flag
// that the requested lastSeq is older than the ring window (a replay gap the client
// should treat as "history truncated").
type attachResult struct {
	replay  []busEvent
	live    <-chan busEvent
	closed  bool // the turn already finished; replay is all there is, then EOF
	gap     bool // lastSeq predates the ring — the client missed events it can't recover
	running bool // whether a turn is currently in flight (so the stream knows to expect live events)
}

// Attach registers ch as the session's live subscriber and returns the replay snapshot.
// At most one stream is attached at a time; a new attach supersedes (closes) the prior
// one (newest-stream-owns, matching the owner-registry's newest-turn-owns). A grace
// timer started by a prior detach is cancelled (the reconnect arrived in time).
func (b *sessionBus) attach(key string, lastSeq int, ch chan busEvent) (attachResult, bool) {
	b.mu.Lock()
	ls := b.sessions[key]
	b.mu.Unlock()
	if ls == nil {
		return attachResult{}, false // no such live session (turn never started / already reclaimed)
	}
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if ls.graceT != nil {
		ls.graceT.Stop()
		ls.graceT = nil
	}
	if ls.sub != nil {
		close(ls.sub) // supersede the prior stream
	}
	res := attachResult{closed: ls.closed, running: ls.running}
	// Replay buffered events the client has not seen. A gap exists if the oldest
	// retained event is newer than lastSeq+1 (the ring rotated past what they need).
	if len(ls.ring) > 0 && ls.ring[0].Seq > lastSeq+1 {
		res.gap = true
	}
	for _, ev := range ls.ring {
		if ev.Seq > lastSeq {
			res.replay = append(res.replay, ev)
		}
	}
	if ls.closed {
		ls.sub = nil // nothing more will be published; the stream writes replay then EOFs
	} else {
		ls.sub = ch
		res.live = ch
	}
	return res, true
}

// detach removes the current subscriber (the SSE stream ended) and, if the turn is
// still running, starts the grace timer: the turn is cancelled after streamGrace
// unless a stream reconnects (attach cancels the timer). onGrace runs the actual
// cancel when the timer fires (the bus does not import the turn ctx directly). A
// non-matching subscriber (already superseded) is a no-op.
func (b *sessionBus) detach(key string, ch chan busEvent) {
	b.mu.Lock()
	ls := b.sessions[key]
	b.mu.Unlock()
	if ls == nil {
		return
	}
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if ls.sub != ch {
		return // already superseded by a newer stream; nothing to do
	}
	ls.sub = nil
	ls.armGraceLocked() // no reconnect within grace → cancel the abandoned turn (stop billing)
}

// drop reclaims a session entry entirely (used when a session is deleted). Safe to
// call on an unknown key.
func (b *sessionBus) drop(key string) {
	b.mu.Lock()
	ls := b.sessions[key]
	delete(b.sessions, key)
	b.mu.Unlock()
	if ls == nil {
		return
	}
	ls.mu.Lock()
	if ls.graceT != nil {
		ls.graceT.Stop()
		ls.graceT = nil
	}
	if ls.cancel != nil {
		ls.cancel()
	}
	if ls.sub != nil {
		close(ls.sub)
		ls.sub = nil
	}
	ls.mu.Unlock()
}

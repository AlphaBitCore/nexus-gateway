package assistant

import (
	"context"
	"github.com/goccy/go-json"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// bus.go is the command/data-stream split. Previously a single
// POST both started a turn AND was the SSE stream, so a dropped connection killed the
// turn and there was no reconnect. The bus detaches the two:
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

// maxConcurrentTurnsPerUser caps how many of a user's sessions can have a turn in
// flight at the same time. All turns share ONE system virtual key, so without
// this cap a single user could open N distinct sessionIDs, fire N concurrent turns, and
// drain the shared system-VK budget — denying the assistant to every other user. Each
// turn can run up to StepCap tool rounds × tokens/step, so even a handful of concurrent
// turns is a meaningful share of the budget; 5 is generous for legitimate parallel use
// (a couple of chats open at once) yet tight enough that one user cannot monopolise the
// key. The concurrent-turn cap is the FIRST per-user spend gate: it bounds the MAXIMUM
// instantaneous per-user spend (≤ 5 × per-turn ceiling). A persistent per-user token/
// cost budget is a heavier, telemetry-backed second gate that can layer on top later;
// this cap stands on its own and requires no spend-tracking pipeline.
// A const, not a cfg knob: there is no demonstrated need for per-deployment divergence,
// and a sensible default beats an admin-facing field (less-is-more). Promote to a cfg
// field only when a concrete deployment justifies a different ceiling.
const maxConcurrentTurnsPerUser = 5

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

	// userID is the owning principal, parsed from the bus key (userID:sessionID) at
	// startTurn. It is the slot key for the per-user concurrent-turn cap.
	userID string
	// userSlotHeld records whether THIS session currently holds one of the owning
	// user's concurrent-turn slots. It makes slot release idempotent per liveSession:
	// the slot is claimed when running flips false→true and released exactly once on
	// the terminal path (finishTurn / drop), so a finishTurn-then-drop or a never-
	// started session can never double-decrement or under-flow the user counter.
	userSlotHeld bool
}

// sessionBus owns the live sessions, keyed by the isolation key (userID:sessionID) so
// one user's session can never be addressed by another.
type sessionBus struct {
	mu       sync.Mutex
	sessions map[string]*liveSession

	// userSlots maps userID → *atomic.Int32 tracking how many of that user's sessions
	// currently have a turn in flight (the per-user concurrent-turn cap). A sync.Map
	// because the key set is the (unbounded, churny) live user population; the counter
	// is atomic so claim/release never need the bus mutex. Entries are left in place
	// after they drain to zero — a bounded-cardinality leak (one small struct per user
	// ever seen on this pod) that avoids a delete/recreate race on the hot path.
	userSlots sync.Map
}

func newSessionBus() *sessionBus {
	return &sessionBus{sessions: make(map[string]*liveSession)}
}

// startResult distinguishes WHY startTurn refused, so the handler can map each to the
// correct HTTP status: a per-session in-flight turn is 409 (turn_in_progress), a per-
// user concurrency-cap breach is 429 (user_turn_limit_exceeded).
type startResult int

const (
	startOK           startResult = iota // a fresh turn was claimed
	startTurnInFlight                    // a turn is already running for THIS session (409)
	startUserLimitHit                    // the user is at the per-user concurrent-turn cap (429)
)

// userSlotCounter returns the per-user atomic counter, creating it on first use. The
// LoadOrStore makes concurrent first-claims for the same user converge on one counter.
func (b *sessionBus) userSlotCounter(userID string) *atomic.Int32 {
	if v, ok := b.userSlots.Load(userID); ok {
		return v.(*atomic.Int32)
	}
	v, _ := b.userSlots.LoadOrStore(userID, new(atomic.Int32))
	return v.(*atomic.Int32)
}

// claimUserSlot atomically reserves one of userID's concurrent-turn slots if the user
// is below maxConcurrentTurnsPerUser, returning true on success. It uses a CAS loop so
// that two simultaneous claims for the same user can never both succeed past the cap
// (a plain Add-then-check could transiently exceed the limit and admit an extra turn).
// An empty userID (pool-less / test paths with no authenticated principal) is never
// rate-limited — there is no per-user budget to protect.
func (b *sessionBus) claimUserSlot(userID string) bool {
	if userID == "" {
		return true
	}
	ctr := b.userSlotCounter(userID)
	for {
		cur := ctr.Load()
		if cur >= maxConcurrentTurnsPerUser {
			return false
		}
		if ctr.CompareAndSwap(cur, cur+1) {
			return true
		}
	}
}

// releaseUserSlot frees one of userID's concurrent-turn slots. It is only ever called
// against a slot a prior claimUserSlot reserved (guarded by liveSession.userSlotHeld),
// so the counter cannot under-flow; the >0 check is belt-and-braces. An empty userID
// never claimed a slot, so it is a no-op.
func (b *sessionBus) releaseUserSlot(userID string) {
	if userID == "" {
		return
	}
	ctr := b.userSlotCounter(userID)
	for {
		cur := ctr.Load()
		if cur <= 0 {
			return
		}
		if ctr.CompareAndSwap(cur, cur-1) {
			return
		}
	}
}

// startTurn registers a turn as running for key and returns its liveSession + the
// turn context (cancelled by interrupt, the disconnect grace, or turnDeadline). The
// startResult reports why a turn was refused:
//   - startTurnInFlight: a turn is ALREADY running for key — the serialization guard
//     behind the "no new command while one is in flight" rule (enforced server-side as
//     defense-in-depth on top of the disabled-input client guard).
//   - startUserLimitHit: the owning user already has maxConcurrentTurnsPerUser turns in
//     flight across distinct sessions — the per-user concurrent-turn cap that
//     stops one user monopolising the shared system VK.
//
// The owning user is parsed from key (userID:sessionID). The caller runs the turn in a
// background goroutine and MUST call finishTurn(key) when it returns, which releases the
// per-user slot this claim reserved.
func (b *sessionBus) startTurn(key string, parent context.Context, deadline time.Duration) (*liveSession, context.Context, startResult) {
	b.mu.Lock()
	ls := b.sessions[key]
	if ls == nil {
		ls = &liveSession{}
		b.sessions[key] = ls
	}
	b.mu.Unlock()

	// The userID is the slot key for the per-user cap. The bus key is "userID:sessionID";
	// a userID may itself contain ':' so we split on the FIRST separator only.
	userID, _, _ := strings.Cut(key, ":")

	ls.mu.Lock()
	defer ls.mu.Unlock()
	if ls.running {
		return nil, nil, startTurnInFlight // a turn is already in flight for this session
	}
	// Per-user concurrency gate: reserve one of the user's slots BEFORE flipping
	// running, so a refused turn leaves no state to unwind. claimUserSlot is the only
	// place the counter goes up; finishTurn/drop release it exactly once.
	if !b.claimUserSlot(userID) {
		return nil, nil, startUserLimitHit
	}
	ctx, cancel := context.WithTimeout(parent, deadline)
	ls.userID = userID
	ls.userSlotHeld = true
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
	ls.mu.Unlock()
	// Re-bind the entry under b.mu: a drop (session delete) racing between the
	// insert above and the claim removes the map entry, and the turn's eventual
	// finishTurn(key) would then miss it and leak the user's concurrent-turn
	// slot permanently. Re-inserting the SAME entry keeps finishTurn/drop
	// reachable for this already-authorized turn. Taken after ls.mu is released
	// so the b.mu → ls.mu lock order used everywhere else is never inverted.
	b.mu.Lock()
	if b.sessions[key] != ls {
		b.sessions[key] = ls
	}
	b.mu.Unlock()
	ls.mu.Lock()
	return ls, ctx, startOK
}

// releaseUserSlotLocked frees the per-user concurrent-turn slot this session holds, if
// any, and clears the held flag so a later terminal path (finishTurn then drop) cannot
// double-release. Caller holds ls.mu.
func (b *sessionBus) releaseUserSlotLocked(ls *liveSession) {
	if !ls.userSlotHeld {
		return
	}
	ls.userSlotHeld = false
	b.releaseUserSlot(ls.userID)
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
	b.releaseUserSlotLocked(ls) // the turn ended → free the user's concurrent-turn slot
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
	ls.running = false
	b.releaseUserSlotLocked(ls) // reclaiming a session frees any slot it still held
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

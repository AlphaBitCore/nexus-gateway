package assistant

import (
	"context"
	"strconv"
	"testing"
	"time"
)

// drainReplay reads the events the bus says a freshly-attached stream must replay.
func seqsOf(evs []busEvent) []int {
	out := make([]int, len(evs))
	for i, e := range evs {
		out[i] = e.Seq
	}
	return out
}

// TestBus_StartTurnSerializes is the "no concurrent turn" guard: a second startTurn for
// the same session while one is running is refused; after finishTurn it succeeds again.
func TestBus_StartTurnSerializes(t *testing.T) {
	b := newSessionBus()
	if _, _, sr := b.startTurn("u:s", context.Background(), time.Minute); sr != startOK {
		t.Fatalf("first startTurn must succeed, got %v", sr)
	}
	if _, _, sr := b.startTurn("u:s", context.Background(), time.Minute); sr != startTurnInFlight {
		t.Fatalf("a second concurrent startTurn for the same session must report startTurnInFlight, got %v", sr)
	}
	// A different session (same user) is independent — distinct sessionID, well under the cap.
	if _, _, sr := b.startTurn("u:other", context.Background(), time.Minute); sr != startOK {
		t.Fatalf("a different session must start independently, got %v", sr)
	}
	b.finishTurn("u:s")
	if _, _, sr := b.startTurn("u:s", context.Background(), time.Minute); sr != startOK {
		t.Fatalf("after finishTurn the session must accept a new turn, got %v", sr)
	}
}

// TestBus_UserConcurrentTurnCap is the F-0267 per-user gate: a single user may have at
// most maxConcurrentTurnsPerUser turns in flight across DISTINCT sessions; the next is
// refused with startUserLimitHit (distinct from the per-session startTurnInFlight). All
// turns share one system VK, so this bounds one user's instantaneous draw on the budget.
func TestBus_UserConcurrentTurnCap(t *testing.T) {
	b := newSessionBus()
	// Open exactly the cap's worth of distinct sessions for one user.
	for i := range maxConcurrentTurnsPerUser {
		key := "u:s" + strconv.Itoa(i)
		if _, _, sr := b.startTurn(key, context.Background(), time.Minute); sr != startOK {
			t.Fatalf("session %d within the cap must start, got %v", i, sr)
		}
	}
	// The (cap+1)th DISTINCT session for the same user is refused with the user-limit code,
	// NOT the per-session in-flight code (it is a brand-new session).
	if _, _, sr := b.startTurn("u:over", context.Background(), time.Minute); sr != startUserLimitHit {
		t.Fatalf("the (cap+1)th session for a user must report startUserLimitHit, got %v", sr)
	}
}

// TestBus_UserCapIsPerUser proves the cap is namespaced per user: a second user gets a
// fresh full allotment even while the first is saturated.
func TestBus_UserCapIsPerUser(t *testing.T) {
	b := newSessionBus()
	for i := range maxConcurrentTurnsPerUser {
		if _, _, sr := b.startTurn("alice:s"+strconv.Itoa(i), context.Background(), time.Minute); sr != startOK {
			t.Fatalf("alice session %d must start, got %v", i, sr)
		}
	}
	// Alice is saturated.
	if _, _, sr := b.startTurn("alice:more", context.Background(), time.Minute); sr != startUserLimitHit {
		t.Fatalf("alice must be at her cap, got %v", sr)
	}
	// Bob still gets his own full allotment.
	for i := range maxConcurrentTurnsPerUser {
		if _, _, sr := b.startTurn("bob:s"+strconv.Itoa(i), context.Background(), time.Minute); sr != startOK {
			t.Fatalf("bob session %d must start independently of alice, got %v", i, sr)
		}
	}
	if _, _, sr := b.startTurn("bob:more", context.Background(), time.Minute); sr != startUserLimitHit {
		t.Fatalf("bob must also have his own cap, got %v", sr)
	}
}

// TestBus_UserSlotFreedOnFinish proves a completed turn frees the user's slot so a new
// session is admitted: saturate, finish one, then a new session succeeds.
func TestBus_UserSlotFreedOnFinish(t *testing.T) {
	b := newSessionBus()
	for i := range maxConcurrentTurnsPerUser {
		b.startTurn("u:s"+strconv.Itoa(i), context.Background(), time.Minute)
	}
	if _, _, sr := b.startTurn("u:blocked", context.Background(), time.Minute); sr != startUserLimitHit {
		t.Fatalf("user must start saturated, got %v", sr)
	}
	b.finishTurn("u:s0") // free one slot
	if _, _, sr := b.startTurn("u:fresh", context.Background(), time.Minute); sr != startOK {
		t.Fatalf("after a turn finishes the freed slot must admit a new session, got %v", sr)
	}
}

// TestBus_UserSlotFreedOnDrop proves drop (session deletion) also frees the user's slot.
func TestBus_UserSlotFreedOnDrop(t *testing.T) {
	b := newSessionBus()
	for i := range maxConcurrentTurnsPerUser {
		b.startTurn("u:s"+strconv.Itoa(i), context.Background(), time.Minute)
	}
	b.drop("u:s0") // reclaim one session → frees its slot
	if _, _, sr := b.startTurn("u:fresh", context.Background(), time.Minute); sr != startOK {
		t.Fatalf("dropping a session must free its slot, got %v", sr)
	}
}

// TestBus_UserSlotNotDoubleReleased pins the exactly-once release invariant: finishTurn
// THEN drop on the same session must release the slot only once, so the user counter
// cannot under-flow and silently raise the effective cap. After the single release the
// user should be able to run exactly the cap again, no more.
func TestBus_UserSlotNotDoubleReleased(t *testing.T) {
	b := newSessionBus()
	b.startTurn("u:s0", context.Background(), time.Minute)
	b.finishTurn("u:s0") // release once
	b.drop("u:s0")       // must NOT release again (slot already freed)

	// The counter is back to 0 — confirm by saturating and then being blocked at exactly
	// the cap (a double-release would have driven the counter negative, admitting cap+1).
	for i := range maxConcurrentTurnsPerUser {
		if _, _, sr := b.startTurn("u:n"+strconv.Itoa(i), context.Background(), time.Minute); sr != startOK {
			t.Fatalf("session %d must start, got %v", i, sr)
		}
	}
	if _, _, sr := b.startTurn("u:extra", context.Background(), time.Minute); sr != startUserLimitHit {
		t.Fatalf("the cap must hold exactly (no under-flow from double release), got %v", sr)
	}
}

// TestBus_RestartedSessionRebalancesSlot proves a session that finishes and starts a new
// turn re-claims a slot: the slot accounting follows running turns, not session lifetime.
func TestBus_RestartedSessionRebalancesSlot(t *testing.T) {
	b := newSessionBus()
	// One session runs, finishes, runs again — net one slot held, never two.
	b.startTurn("u:s", context.Background(), time.Minute)
	b.finishTurn("u:s")
	b.startTurn("u:s", context.Background(), time.Minute) // re-claims a slot
	// Fill the rest of the cap with other sessions.
	for i := 1; i < maxConcurrentTurnsPerUser; i++ {
		if _, _, sr := b.startTurn("u:o"+strconv.Itoa(i), context.Background(), time.Minute); sr != startOK {
			t.Fatalf("session o%d within the cap must start, got %v", i, sr)
		}
	}
	if _, _, sr := b.startTurn("u:over", context.Background(), time.Minute); sr != startUserLimitHit {
		t.Fatalf("a restarted session must hold exactly one slot; the cap must hold, got %v", sr)
	}
}

// TestBus_EmptyUserNotRateLimited covers the pool-less / unauthenticated path: an empty
// userID has no per-user budget to protect and is never capped.
func TestBus_EmptyUserNotRateLimited(t *testing.T) {
	b := newSessionBus()
	// Keys with an empty userID prefix (":sessionID"). Far more than the cap.
	for i := range maxConcurrentTurnsPerUser + 3 {
		if _, _, sr := b.startTurn(":s"+strconv.Itoa(i), context.Background(), time.Minute); sr != startOK {
			t.Fatalf("an empty-userID session %d must never be rate-limited, got %v", i, sr)
		}
	}
}

// TestBus_ReleaseUserSlotNoUnderflow directly exercises the under-flow guard on the
// raw counter helper: releasing a never-claimed user is a safe no-op and the next claim
// still succeeds (the counter never went negative).
func TestBus_ReleaseUserSlotNoUnderflow(t *testing.T) {
	b := newSessionBus()
	b.releaseUserSlot("ghost") // never claimed
	if !b.claimUserSlot("ghost") {
		t.Fatal("a claim after a no-op release must succeed (counter must not be negative)")
	}
	b.releaseUserSlot("ghost")
	b.releaseUserSlot("") // empty userID release is a no-op
}

// TestBus_ReplayFromLastSeq covers reconnect: attaching with ?lastSeq=N replays only
// events newer than N, and lastSeq=0 replays the whole turn so far.
func TestBus_ReplayFromLastSeq(t *testing.T) {
	b := newSessionBus()
	b.startTurn("u:s", context.Background(), time.Minute)
	b.publish("u:s", "text", map[string]string{"delta": "a"})
	b.publish("u:s", "text", map[string]string{"delta": "b"})
	b.publish("u:s", "text", map[string]string{"delta": "c"})

	res, ok := b.attach("u:s", 0, make(chan busEvent, 8))
	if !ok {
		t.Fatal("attach must find the live session")
	}
	if got := seqsOf(res.replay); len(got) != 3 || got[0] != 1 || got[2] != 3 {
		t.Fatalf("lastSeq=0 must replay all 3 events, got seqs %v", got)
	}
	if !res.running || res.live == nil {
		t.Fatal("a running turn must expose a live channel")
	}

	res2, _ := b.attach("u:s", 2, make(chan busEvent, 8))
	if got := seqsOf(res2.replay); len(got) != 1 || got[0] != 3 {
		t.Fatalf("lastSeq=2 must replay only seq 3, got %v", got)
	}
}

// TestBus_AttachSupersedesPriorStream pins newest-stream-owns: a second attach closes
// the prior subscriber so a stale stream cannot keep consuming.
func TestBus_AttachSupersedesPriorStream(t *testing.T) {
	b := newSessionBus()
	b.startTurn("u:s", context.Background(), time.Minute)
	first := make(chan busEvent, 8)
	b.attach("u:s", 0, first)
	b.attach("u:s", 0, make(chan busEvent, 8)) // supersede
	if _, open := <-first; open {
		t.Fatal("the superseded stream's channel must be closed")
	}
}

// TestBus_LiveDelivery proves a published event reaches the attached subscriber live.
func TestBus_LiveDelivery(t *testing.T) {
	b := newSessionBus()
	b.startTurn("u:s", context.Background(), time.Minute)
	ch := make(chan busEvent, 8)
	b.attach("u:s", 0, ch)
	b.publish("u:s", "text", map[string]string{"delta": "live"})
	select {
	case ev := <-ch:
		if ev.Event != "text" || ev.Seq != 1 {
			t.Fatalf("got %+v, want a live text event seq 1", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("a published event must be delivered live to the attached subscriber")
	}
}

// TestBus_ReplayGap covers ring rotation: when more events are published than the ring
// holds, a reconnect from seq 0 reports a gap (history truncated) and replays the tail.
func TestBus_ReplayGap(t *testing.T) {
	b := newSessionBus()
	b.startTurn("u:s", context.Background(), time.Minute)
	for range replayRingSize + 50 {
		b.publish("u:s", "text", map[string]string{"delta": "x"})
	}
	res, _ := b.attach("u:s", 0, make(chan busEvent, 8))
	if !res.gap {
		t.Fatal("a lastSeq older than the ring window must report a gap")
	}
	if len(res.replay) != replayRingSize {
		t.Fatalf("replay must be capped at the ring size %d, got %d", replayRingSize, len(res.replay))
	}
}

// TestBus_Interrupt cancels the in-flight turn's context (the Stop button).
func TestBus_Interrupt(t *testing.T) {
	b := newSessionBus()
	_, ctx, _ := b.startTurn("u:s", context.Background(), time.Minute)
	if !b.interrupt("u:s") {
		t.Fatal("interrupt must report it stopped a running turn")
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("interrupt must cancel the turn context")
	}
	if b.interrupt("u:s") {
		t.Fatal("interrupting an already-stopped turn must report false")
	}
	if b.interrupt("u:nope") {
		t.Fatal("interrupting an unknown session must report false")
	}
}

// TestBus_StartTurnArmsGraceWhenNeverStreamed is the no-stream billing bound (Opus
// review fix): a turn that is started but whose stream is NEVER opened must still be
// cancelled after the grace window, not bill until turnDeadline.
func TestBus_StartTurnArmsGraceWhenNeverStreamed(t *testing.T) {
	orig := streamGrace
	streamGrace = 20 * time.Millisecond
	defer func() { streamGrace = orig }()

	b := newSessionBus()
	_, ctx, _ := b.startTurn("u:s", context.Background(), time.Minute)
	// No attach at all — the client got the 202 but never opened the stream.
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("a turn whose stream is never opened must be cancelled after the grace window")
	}
}

// TestBus_FirstAttachCancelsStartGrace: opening the stream within the grace window
// cancels the start-armed timer so a normally-streamed turn is not killed at 20ms.
func TestBus_FirstAttachCancelsStartGrace(t *testing.T) {
	orig := streamGrace
	streamGrace = 30 * time.Millisecond
	defer func() { streamGrace = orig }()

	b := newSessionBus()
	_, ctx, _ := b.startTurn("u:s", context.Background(), time.Minute)
	b.attach("u:s", 0, make(chan busEvent, 4)) // stream opens promptly
	select {
	case <-ctx.Done():
		t.Fatal("opening the stream must cancel the start-armed grace timer")
	case <-time.After(80 * time.Millisecond):
		// still alive — correct
	}
}

// TestBus_DetachGraceCancels is the disconnect-grace billing bound: an ungraceful
// stream drop (no reconnect) cancels the turn after the grace window.
func TestBus_DetachGraceCancels(t *testing.T) {
	orig := streamGrace
	streamGrace = 20 * time.Millisecond
	defer func() { streamGrace = orig }()

	b := newSessionBus()
	_, ctx, _ := b.startTurn("u:s", context.Background(), time.Minute)
	ch := make(chan busEvent, 8)
	b.attach("u:s", 0, ch)
	b.detach("u:s", ch) // stream dropped, no reconnect

	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("a detached turn with no reconnect must be cancelled after the grace window")
	}
}

// TestBus_ReattachCancelsGrace proves a reconnect within the grace window keeps the
// turn alive (the blip recovered).
func TestBus_ReattachCancelsGrace(t *testing.T) {
	orig := streamGrace
	streamGrace = 50 * time.Millisecond
	defer func() { streamGrace = orig }()

	b := newSessionBus()
	_, ctx, _ := b.startTurn("u:s", context.Background(), time.Minute)
	ch := make(chan busEvent, 8)
	b.attach("u:s", 0, ch)
	b.detach("u:s", ch)                        // drop
	b.attach("u:s", 0, make(chan busEvent, 8)) // reconnect within grace

	select {
	case <-ctx.Done():
		t.Fatal("a reconnect within the grace window must NOT cancel the turn")
	case <-time.After(100 * time.Millisecond):
		// still alive — correct
	}
}

// TestBus_DetachNonMatchingSubIsNoop ensures a detach from an already-superseded
// stream does not start a grace timer for the live one.
func TestBus_DetachNonMatchingSubIsNoop(t *testing.T) {
	orig := streamGrace
	streamGrace = 20 * time.Millisecond
	defer func() { streamGrace = orig }()

	b := newSessionBus()
	_, ctx, _ := b.startTurn("u:s", context.Background(), time.Minute)
	stale := make(chan busEvent, 8)
	b.attach("u:s", 0, stale)
	b.attach("u:s", 0, make(chan busEvent, 8)) // supersede stale
	b.detach("u:s", stale)                     // stale detaches — must be a no-op

	select {
	case <-ctx.Done():
		t.Fatal("a detach from a superseded stream must not cancel the live turn")
	case <-time.After(80 * time.Millisecond):
	}
}

// TestBus_FinishClosesSubscriber proves the turn finishing wakes the stream (closes
// the channel) so it drains and exits.
func TestBus_FinishClosesSubscriber(t *testing.T) {
	b := newSessionBus()
	b.startTurn("u:s", context.Background(), time.Minute)
	ch := make(chan busEvent, 8)
	b.attach("u:s", 0, ch)
	b.finishTurn("u:s")
	if _, open := <-ch; open {
		t.Fatal("finishTurn must close the subscriber channel")
	}
}

// TestBus_AttachAfterFinishReplaysClosed covers a late reconnect to a finished turn:
// attach returns the tail with closed=true and no live channel.
func TestBus_AttachAfterFinishReplaysClosed(t *testing.T) {
	b := newSessionBus()
	b.startTurn("u:s", context.Background(), time.Minute)
	b.publish("u:s", "text", map[string]string{"delta": "a"})
	b.publish("u:s", "done", map[string]any{"sessionId": "s"})
	b.finishTurn("u:s")

	res, ok := b.attach("u:s", 0, make(chan busEvent, 8))
	if !ok {
		t.Fatal("the finished session entry must still be attachable for a late replay")
	}
	if !res.closed || res.live != nil {
		t.Fatal("attaching to a finished turn must be closed with no live channel")
	}
	if len(res.replay) != 2 {
		t.Fatalf("a late reconnect must replay the finished turn's tail, got %d events", len(res.replay))
	}
}

// TestBus_AttachUnknownSession returns ok=false for a key that never started.
func TestBus_AttachUnknownSession(t *testing.T) {
	b := newSessionBus()
	if _, ok := b.attach("u:never", 0, make(chan busEvent, 8)); ok {
		t.Fatal("attaching to a session that never started must report not-found")
	}
}

// TestBus_Drop reclaims the entry, cancels the turn, and closes the subscriber.
func TestBus_Drop(t *testing.T) {
	b := newSessionBus()
	_, ctx, _ := b.startTurn("u:s", context.Background(), time.Minute)
	ch := make(chan busEvent, 8)
	b.attach("u:s", 0, ch)
	b.drop("u:s")

	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("drop must cancel the in-flight turn")
	}
	if _, open := <-ch; open {
		t.Fatal("drop must close the subscriber channel")
	}
	if _, ok := b.attach("u:s", 0, make(chan busEvent, 8)); ok {
		t.Fatal("drop must reclaim the session entry")
	}
	b.drop("u:unknown") // safe on an unknown key
}

// TestBus_NoopsOnUnknownKey: publish / finishTurn / detach / interrupt / drop on a key
// with no live session are safe no-ops (the nil-session early returns).
func TestBus_NoopsOnUnknownKey(t *testing.T) {
	b := newSessionBus()
	b.publish("nope", "text", map[string]string{"d": "x"})
	b.finishTurn("nope")
	b.detach("nope", make(chan busEvent, 1))
	if b.interrupt("nope") {
		t.Fatal("interrupt on unknown key must be false")
	}
	b.drop("nope")
}

// TestBus_FinishStopsPendingGrace: finishing a turn after the stream detached (grace
// timer armed) must stop the timer so it cannot cancel the already-finished turn.
func TestBus_FinishStopsPendingGrace(t *testing.T) {
	orig := streamGrace
	streamGrace = time.Hour // long, so the timer would not fire on its own during the test
	defer func() { streamGrace = orig }()

	b := newSessionBus()
	b.startTurn("u:s", context.Background(), time.Minute)
	ch := make(chan busEvent, 4)
	b.attach("u:s", 0, ch)
	b.detach("u:s", ch) // arms the grace timer
	b.finishTurn("u:s") // must Stop the armed timer

	ls := b.sessions["u:s"]
	ls.mu.Lock()
	armed := ls.graceT != nil
	ls.mu.Unlock()
	if armed {
		t.Fatal("finishTurn must stop the pending grace timer")
	}
}

// TestBus_PublishOverflowForcesReconnect covers backpressure: a subscriber whose
// channel is full is force-closed (so it reconnects + replays from the ring) rather
// than blocking the turn or dropping an event.
func TestBus_PublishOverflowForcesReconnect(t *testing.T) {
	b := newSessionBus()
	b.startTurn("u:s", context.Background(), time.Minute)
	ch := make(chan busEvent, 1) // tiny buffer to force overflow
	b.attach("u:s", 0, ch)
	b.publish("u:s", "text", map[string]string{"delta": "1"}) // fits
	b.publish("u:s", "text", map[string]string{"delta": "2"}) // overflow → close

	// The channel was closed by the overflow; draining yields the one buffered event
	// then the closed signal. The ring still holds both for a reconnect.
	drained := 0
	for range ch {
		drained++
	}
	res, _ := b.attach("u:s", 0, make(chan busEvent, 8))
	if len(res.replay) != 2 {
		t.Fatalf("the ring must retain both events for reconnect after overflow, got %d", len(res.replay))
	}
}

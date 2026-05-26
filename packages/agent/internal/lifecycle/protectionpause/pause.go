// Package protectionpause manages the user-initiated "Pause Protection"
// feature exposed through the local IPC. It composes the existing
// killswitch so the connection bridge already respects the paused state
// without further plumbing.
//
// Three behaviours are layered on top of the bare killswitch:
//
//  1. An explicit duration: Pause(seconds=N) auto-resumes after N
//     seconds via a one-shot timer. The deadline is exposed via
//     ResumesAt so the menu bar can render a countdown.
//  2. Resume() cancels any pending timer atomically.
//  3. A "paused-by-user" actor string is recorded on the killswitch
//     so admin shadow pushes ("hub-shadow") remain distinguishable
//     in the snapshot — operators can see whether the device is
//     paused locally or globally.
//
// The package owns no I/O of its own; the daemon's connection bridge
// (which already checks killswitch.IsEngaged) does all of the work.
package protectionpause

import (
	"sync"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/lifecycle/killswitch"
)

// ActorUserPaused is the audit-trail string written into the killswitch
// snapshot when the user initiates a pause from the menu bar.
const ActorUserPaused = "user-paused"

// ActorUserResumed is the audit-trail string written into the
// killswitch snapshot when the user resumes protection (manually or
// via the auto-resume timer).
const ActorUserResumed = "user-resumed"

// Pauser orchestrates the user-pause feature. Safe for concurrent use.
type Pauser struct {
	ks *killswitch.Switch

	mu        sync.Mutex
	timer     *time.Timer
	resumesAt time.Time

	// now is the time source. Overridable in tests.
	now func() time.Time
}

// New constructs a Pauser that toggles the given killswitch.
func New(ks *killswitch.Switch) *Pauser {
	return &Pauser{ks: ks, now: time.Now}
}

// Pause engages protection-pause via the killswitch. When seconds > 0
// an internal timer auto-resumes after that interval; seconds == 0
// pauses indefinitely until Resume is called.
//
// Calling Pause again replaces any pending timer (the new deadline
// wins). Returns the absolute resume time when seconds > 0; the
// zero value when paused indefinitely.
func (p *Pauser) Pause(seconds int) time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.timer != nil {
		p.timer.Stop()
		p.timer = nil
	}
	// Engage the kill switch to pause protection. Wire semantic is the
	// canonical one — engaged=true means pause/passthrough.
	p.ks.Toggle(true, ActorUserPaused)

	if seconds <= 0 {
		p.resumesAt = time.Time{}
		return time.Time{}
	}
	d := time.Duration(seconds) * time.Second
	p.resumesAt = p.now().Add(d)
	p.timer = time.AfterFunc(d, p.autoResume)
	return p.resumesAt
}

// Resume disengages the user-pause and cancels any pending auto-
// resume timer. Safe to call when no pause is active.
func (p *Pauser) Resume() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cancelTimerLocked()
	p.resumesAt = time.Time{}
	// Disengage the kill switch to resume normal interception.
	p.ks.Toggle(false, ActorUserResumed)
}

// ResumesAt returns the scheduled auto-resume time and true when a
// finite pause is currently active, or the zero value + false when
// no pause is active (or the pause is indefinite).
func (p *Pauser) ResumesAt() (time.Time, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.resumesAt.IsZero() {
		return time.Time{}, false
	}
	return p.resumesAt, true
}

// IsPaused reports whether protection is currently paused by the
// user. This reads the underlying killswitch so an admin-initiated
// pause (hub-shadow) is *also* visible — the caller should consult
// the killswitch snapshot if the cause matters. The kill switch is
// engaged ⇔ protection is paused, so the predicate maps directly.
func (p *Pauser) IsPaused() bool {
	return p.ks.IsEngaged()
}

func (p *Pauser) autoResume() {
	p.mu.Lock()
	if p.timer == nil {
		// Resume() raced ahead of the timer fire; nothing to do.
		p.mu.Unlock()
		return
	}
	p.timer = nil
	p.resumesAt = time.Time{}
	p.mu.Unlock()
	// Disengage the kill switch to resume normal interception, matching
	// the explicit Resume() path above.
	p.ks.Toggle(false, ActorUserResumed)
}

func (p *Pauser) cancelTimerLocked() {
	if p.timer != nil {
		p.timer.Stop()
		p.timer = nil
	}
}

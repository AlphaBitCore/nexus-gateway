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
//
// # Two independent pause sources
//
// The kill switch is shared by two independent callers:
//
//   - Admin (Hub shadow push): engaged via EngageAdmin / DisengageAdmin.
//   - User (menu bar "Pause Protection"): engaged via Pause (= EngageUser) /
//     Resume.
//
// The effective kill-switch state is adminEngaged || userPaused.  A user
// Resume never disengages an active admin brake, and an admin Disengage
// never disengages an active user pause.  The underlying killswitch.Toggle
// is driven only when the combined effective state changes.
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

	mu sync.Mutex

	// adminEngaged tracks whether a Hub-pushed admin kill-switch is active.
	// Set by EngageAdmin, cleared by DisengageAdmin.
	adminEngaged bool

	// userPaused tracks whether the user has initiated a local protection
	// pause via the menu bar. Set by Pause (EngageUser), cleared by Resume
	// and autoResume.
	userPaused bool

	timer     *time.Timer
	resumesAt time.Time

	// now is the time source. Overridable in tests.
	now func() time.Time
}

// New constructs a Pauser that toggles the given killswitch.
func New(ks *killswitch.Switch) *Pauser {
	return &Pauser{ks: ks, now: time.Now}
}

// effectiveLocked returns the combined engaged state (adminEngaged || userPaused).
// Must be called with p.mu held.
func (p *Pauser) effectiveLocked() bool {
	return p.adminEngaged || p.userPaused
}

// EngageAdmin marks the admin kill-switch as active (Hub shadow push).
// Engages the underlying kill switch when the combined state flips to true.
func (p *Pauser) EngageAdmin(actor string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	wasBoth := p.effectiveLocked()
	p.adminEngaged = true
	if !wasBoth {
		p.ks.Toggle(true, actor)
	}
}

// DisengageAdmin clears the admin kill-switch. Disengages the underlying
// kill switch only when userPaused is also false (combined state flips to
// false), so an active user pause continues to block TLS bumping.
func (p *Pauser) DisengageAdmin(actor string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.adminEngaged = false
	if !p.effectiveLocked() {
		p.ks.Toggle(false, actor)
	}
}

// Pause engages protection-pause via the killswitch. When seconds > 0
// an internal timer auto-resumes after that interval; seconds == 0
// pauses indefinitely until Resume is called.
//
// Calling Pause again replaces any pending timer (the new deadline
// wins). Returns the absolute resume time when seconds > 0; the
// zero value when paused indefinitely.
func (p *Pauser) Pause(seconds int) time.Time {
	return p.engageUser(seconds)
}

// EngageUser is the named equivalent of Pause, used where call-site
// clarity about the pause source matters.
func (p *Pauser) EngageUser(seconds int) time.Time {
	return p.engageUser(seconds)
}

func (p *Pauser) engageUser(seconds int) time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.timer != nil {
		p.timer.Stop()
		p.timer = nil
	}

	wasBoth := p.effectiveLocked()
	p.userPaused = true
	// Engage the kill switch to pause protection. Wire semantic is the
	// canonical one — engaged=true means pause/passthrough.
	if !wasBoth {
		p.ks.Toggle(true, ActorUserPaused)
	}

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
// An admin-engaged brake (EngageAdmin) is unaffected.
func (p *Pauser) Resume() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cancelTimerLocked()
	p.resumesAt = time.Time{}
	p.userPaused = false
	// Disengage the kill switch to resume normal interception — but only
	// when the admin brake is not also active.
	if !p.adminEngaged {
		p.ks.Toggle(false, ActorUserResumed)
	}
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

// IsPaused reports whether protection is currently paused (admin or
// user). The kill switch is engaged ⇔ protection is paused, so the
// predicate maps directly.
func (p *Pauser) IsPaused() bool {
	return p.ks.IsEngaged()
}

// IsAdminEngaged reports whether the admin kill-switch is currently
// active independent of user-pause state.
func (p *Pauser) IsAdminEngaged() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.adminEngaged
}

// IsUserPaused reports whether the user has an active local pause
// independent of the admin kill-switch state.
func (p *Pauser) IsUserPaused() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.userPaused
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
	p.userPaused = false
	shouldToggle := !p.adminEngaged
	p.mu.Unlock()

	// Disengage the kill switch to resume normal interception — but only
	// when the admin brake is not also active.
	if shouldToggle {
		p.ks.Toggle(false, ActorUserResumed)
	}
}

func (p *Pauser) cancelTimerLocked() {
	if p.timer != nil {
		p.timer.Stop()
		p.timer = nil
	}
}

package protectionpause

import (
	"log/slog"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/lifecycle/killswitch"
)

func newPauserForTest(t *testing.T) (*Pauser, *killswitch.Switch) {
	t.Helper()
	ks := killswitch.New(slog.Default())
	return New(ks), ks
}

func TestPauser_IndefinitePauseDoesNotScheduleResume(t *testing.T) {
	p, ks := newPauserForTest(t)

	resumesAt := p.Pause(0)
	if !resumesAt.IsZero() {
		t.Errorf("indefinite pause should return zero time, got %v", resumesAt)
	}
	if !p.IsPaused() {
		t.Error("IsPaused() should be true after Pause()")
	}
	if !ks.IsEngaged() {
		t.Error("killswitch should be engaged (IsEngaged=true) after Pause()")
	}
	if _, ok := p.ResumesAt(); ok {
		t.Error("ResumesAt should report no scheduled resume for indefinite pause")
	}
}

func TestPauser_FiniteResumeFires(t *testing.T) {
	p, ks := newPauserForTest(t)

	resumesAt := p.Pause(1)
	if resumesAt.IsZero() {
		t.Fatal("finite pause must return a non-zero resume time")
	}
	if got, ok := p.ResumesAt(); !ok || !got.Equal(resumesAt) {
		t.Errorf("ResumesAt = %v ok=%v, want %v", got, ok, resumesAt)
	}

	// Wait for the timer to fire (Pause(1) → 1s). Allow a small buffer.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !p.IsPaused() {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if p.IsPaused() {
		t.Error("auto-resume timer did not fire within 2 seconds")
	}
	if ks.IsEngaged() {
		t.Error("killswitch should be disengaged after auto-resume")
	}
	if _, ok := p.ResumesAt(); ok {
		t.Error("ResumesAt should be cleared after auto-resume")
	}
}

func TestPauser_ManualResumeCancelsTimer(t *testing.T) {
	p, ks := newPauserForTest(t)

	p.Pause(3600) // far-future deadline
	if !p.IsPaused() {
		t.Fatal("pre-condition: expected paused")
	}

	p.Resume()
	if p.IsPaused() {
		t.Error("Resume() should clear the paused state immediately")
	}
	if ks.IsEngaged() {
		t.Error("killswitch should be disengaged after Resume()")
	}
	if _, ok := p.ResumesAt(); ok {
		t.Error("ResumesAt should be cleared after manual Resume()")
	}
}

func TestPauser_SecondPauseReplacesDeadline(t *testing.T) {
	p, _ := newPauserForTest(t)

	first := p.Pause(3600)
	second := p.Pause(1800)
	if !second.Before(first) {
		t.Errorf("second pause deadline %v should be earlier than first %v", second, first)
	}
	got, ok := p.ResumesAt()
	if !ok || !got.Equal(second) {
		t.Errorf("ResumesAt = %v ok=%v, want %v", got, ok, second)
	}
}

func TestPauser_ResumeWithoutPauseIsNoOp(t *testing.T) {
	p, ks := newPauserForTest(t)
	// pre-condition: not paused
	if p.IsPaused() {
		t.Fatal("fresh pauser should not be paused")
	}
	p.Resume() // must not panic / deadlock
	if ks.IsEngaged() {
		t.Error("killswitch should remain disengaged")
	}
}

func TestPauser_AutoResumeRaceWithManualResume(t *testing.T) {
	// Pin the race-branch in autoResume(): timer fires after manual Resume()
	// has already cleared p.timer. Without the nil guard, autoResume would
	// double-toggle the kill switch (re-engaging it after manual resume).
	p, ks := newPauserForTest(t)

	// Pause for 100ms.
	p.Pause(0) // first take an indefinite pause...
	// Manually call autoResume directly with no timer set — exercises the
	// "Resume() raced ahead of the timer fire" branch.
	p.autoResume()

	// State must be unchanged: still paused (no manual Resume called),
	// killswitch still engaged.
	if !p.IsPaused() {
		t.Error("autoResume with nil timer should not flip state")
	}
	if !ks.IsEngaged() {
		t.Error("killswitch must remain engaged after no-op autoResume")
	}
	// Cleanup
	p.Resume()
}

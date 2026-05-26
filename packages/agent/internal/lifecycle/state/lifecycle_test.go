package state

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	opsmetrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
)

// fakePusher captures every event the Emitter ships so tests can
// inspect the wire-shape (level / event_type / attrs) without standing
// up a thingclient + Hub roundtrip.
type fakePusher struct {
	mu     sync.Mutex
	events []opsmetrics.DiagEvent
	err    error
}

func (f *fakePusher) PushDiagEvent(_ context.Context, evt opsmetrics.DiagEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.events = append(f.events, evt)
	return nil
}

func (f *fakePusher) get() []opsmetrics.DiagEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]opsmetrics.DiagEvent, len(f.events))
	copy(out, f.events)
	return out
}

func newTestEmitter(t *testing.T, p Pusher) *Emitter {
	t.Helper()
	return New(Config{
		Pusher:       p,
		ThingID:      "agent-test-001",
		AgentVersion: "test/0.0.0",
		OSInfo:       map[string]any{"os": "darwin"},
	})
}

func TestEmitter_StartupShapesEventCorrectly(t *testing.T) {
	p := &fakePusher{}
	e := newTestEmitter(t, p)

	e.Startup()

	events := p.get()
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	got := events[0]
	if got.EventType != opsmetrics.EventTypeLifecycle {
		t.Errorf("EventType: want %q, got %q", opsmetrics.EventTypeLifecycle, got.EventType)
	}
	if got.Level != opsmetrics.LevelInfo {
		t.Errorf("Level: want %q, got %q", opsmetrics.LevelInfo, got.Level)
	}
	if got.Source != "agent" {
		t.Errorf("Source: want agent, got %q", got.Source)
	}
	if got.ThingID != "agent-test-001" {
		t.Errorf("ThingID: want agent-test-001, got %q", got.ThingID)
	}
	if got.Attrs["action"] != ActionStartup {
		t.Errorf("action attr: want %q, got %v", ActionStartup, got.Attrs["action"])
	}
	if got.OccurredAt.IsZero() {
		t.Error("OccurredAt should be populated")
	}
	if got.AgentVersion != "test/0.0.0" {
		t.Errorf("AgentVersion: want test/0.0.0, got %q", got.AgentVersion)
	}
}

func TestEmitter_ShutdownIncludesReason(t *testing.T) {
	p := &fakePusher{}
	e := newTestEmitter(t, p)

	e.Shutdown("user_quit_flag")

	events := p.get()
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Attrs["action"] != ActionShutdown {
		t.Errorf("action: want %q, got %v", ActionShutdown, events[0].Attrs["action"])
	}
	if events[0].Attrs["reason"] != "user_quit_flag" {
		t.Errorf("reason: want user_quit_flag, got %v", events[0].Attrs["reason"])
	}
}

func TestEmitter_PausedIncludesDuration(t *testing.T) {
	p := &fakePusher{}
	e := newTestEmitter(t, p)

	e.Paused(900) // 15 minutes

	events := p.get()
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Attrs["durationSec"] != 900 {
		t.Errorf("durationSec: want 900, got %v", events[0].Attrs["durationSec"])
	}
}

func TestEmitter_PausedIndefiniteEncodesZero(t *testing.T) {
	p := &fakePusher{}
	e := newTestEmitter(t, p)

	e.Paused(0)

	events := p.get()
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Attrs["durationSec"] != 0 {
		t.Errorf("indefinite pause should encode durationSec=0, got %v", events[0].Attrs["durationSec"])
	}
}

func TestEmitter_ResumedHasNoExtraAttrs(t *testing.T) {
	p := &fakePusher{}
	e := newTestEmitter(t, p)

	e.Resumed()

	events := p.get()
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	// Only the action attr should be present; we do NOT echo durationSec
	// or any pause-side state back on the resume event.
	if _, hasDuration := events[0].Attrs["durationSec"]; hasDuration {
		t.Error("resumed event should not carry durationSec")
	}
	if events[0].Attrs["action"] != ActionResumed {
		t.Errorf("action: want %q, got %v", ActionResumed, events[0].Attrs["action"])
	}
}

func TestEmitter_SSOLoginCarriesEmail(t *testing.T) {
	p := &fakePusher{}
	e := newTestEmitter(t, p)

	e.SSOLogin("tiebin@example.com")

	events := p.get()
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Attrs["email"] != "tiebin@example.com" {
		t.Errorf("email attr: want tiebin@example.com, got %v", events[0].Attrs["email"])
	}
}

func TestEmitter_SSOLogoutEmitsCorrectAction(t *testing.T) {
	// SSOLogout pairs with SSOLogin and must emit its own action so CP
	// UI can render distinct rows (not collapse logout into login state).
	p := &fakePusher{}
	e := newTestEmitter(t, p)
	e.SSOLogout()
	events := p.get()
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Attrs["action"] != ActionSSOLogout {
		t.Errorf("action: want %q, got %v", ActionSSOLogout, events[0].Attrs["action"])
	}
}

func TestNewEventID_UniquePerCall(t *testing.T) {
	// EventID is the primary key on both local mirror and Hub thing_diag_event;
	// collisions would silently overwrite history. 1000-call uniqueness check.
	seen := make(map[string]bool, 1000)
	for i := 0; i < 1000; i++ {
		id := newEventID()
		if id == "" {
			t.Fatal("newEventID returned empty string")
		}
		if seen[id] {
			t.Fatalf("collision after %d calls: %q", i, id)
		}
		seen[id] = true
	}
}

// fakeRecorder captures local-mirror writes and optionally returns an error.
type fakeRecorder struct {
	mu    sync.Mutex
	calls []string
	err   error
}

func (r *fakeRecorder) RecordLifecycle(id string, _ time.Time, action, _, _ string, _ map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, id+"|"+action)
	return r.err
}

func TestEmitter_RecorderSuccessBothPathsFire(t *testing.T) {
	// When Recorder is wired, every emit must call BOTH the local mirror
	// (RecordLifecycle) and the Hub push. Pin this fan-out so a refactor
	// that gates them on a single failure can't silently break the
	// Activity-tab feed.
	p := &fakePusher{}
	r := &fakeRecorder{}
	e := New(Config{Pusher: p, Recorder: r, ThingID: "t1"})

	e.Startup()
	if len(r.calls) != 1 {
		t.Errorf("recorder called %d times, want 1", len(r.calls))
	}
	if len(p.get()) != 1 {
		t.Errorf("pusher called %d times, want 1", len(p.get()))
	}
}

func TestEmitter_RecorderErrorDoesNotBlockHubPush(t *testing.T) {
	// Critical invariant from the docstring: a SQLCipher write failure
	// must NOT prevent the Hub push from running. The two paths serve
	// different audiences (Activity tab vs CP UI) — one going down
	// can't take the other.
	p := &fakePusher{}
	r := &fakeRecorder{err: errors.New("sqlcipher: disk full")}
	e := New(Config{Pusher: p, Recorder: r, ThingID: "t1"})

	e.Shutdown("test")

	if len(p.get()) != 1 {
		t.Errorf("Hub push must still fire when recorder errors: pushes=%d", len(p.get()))
	}
}

func TestEmitter_NilPusherIsNoOp(t *testing.T) {
	// Pre-enrollment path: cmdRun may construct the Emitter before the
	// thing client exists. The emitter must accept nil and silently no-op.
	e := New(Config{Pusher: nil, ThingID: "x"})
	// Should not panic.
	e.Startup()
	e.Shutdown("test")
	e.Paused(60)
	e.Resumed()
}

func TestEmitter_NilEmitterIsNoOp(t *testing.T) {
	// Callers may declare a *Emitter that never gets initialized (e.g.
	// the daemon's pending-enrollment branch skips New()). Methods on a
	// nil receiver must not panic.
	var e *Emitter
	e.Startup()
	e.Shutdown("test")
	e.Paused(60)
	e.Resumed()
}

func TestEmitter_PushErrorDoesNotPanic(t *testing.T) {
	// Best-effort contract: a push error must NOT escape — lifecycle UX
	// has to remain instantaneous even when the WS outbox is stalled.
	p := &fakePusher{err: errors.New("ws outbox stalled")}
	e := newTestEmitter(t, p)
	e.Paused(60)
	// If Paused() ever propagates the pusher error (e.g. via panic or
	// return value the caller checks), this test catches the regression.
}

func TestActionConstantsAreStable(t *testing.T) {
	// These string literals are the wire identifiers CP UI filters on.
	// A typo / casing drift here is silently breaking the admin's
	// Activity tab without any test failure elsewhere — so we pin them
	// explicitly. To rename: update CP UI side first, then this test.
	cases := map[string]string{
		"ActionStartup":   "agent.startup",
		"ActionShutdown":  "agent.shutdown",
		"ActionPaused":    "agent.paused",
		"ActionResumed":   "agent.resumed",
		"ActionSSOLogin":  "agent.sso_login",
		"ActionSSOLogout": "agent.sso_logout",
	}
	got := map[string]string{
		"ActionStartup":   ActionStartup,
		"ActionShutdown":  ActionShutdown,
		"ActionPaused":    ActionPaused,
		"ActionResumed":   ActionResumed,
		"ActionSSOLogin":  ActionSSOLogin,
		"ActionSSOLogout": ActionSSOLogout,
	}
	for k, want := range cases {
		if got[k] != want {
			t.Errorf("%s = %q, want %q", k, got[k], want)
		}
	}
}

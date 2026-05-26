package diag

import (
	"strings"
	"sync"
	"testing"

	opsmetrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
)

// recordingBuffer captures Insert calls without touching SQL — keeps the
// recovery test focused on the panic-handling contract.
type recordingBuffer struct {
	mu       sync.Mutex
	inserted []opsmetrics.DiagEvent
}

func (r *recordingBuffer) Insert(evt opsmetrics.DiagEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.inserted = append(r.inserted, evt)
	return nil
}

func (r *recordingBuffer) snapshot() []opsmetrics.DiagEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]opsmetrics.DiagEvent, len(r.inserted))
	copy(out, r.inserted)
	return out
}

func TestRecoveryPersistsCrashEvent(t *testing.T) {
	buf := &recordingBuffer{}
	cfg := RecoveryConfig{
		ThingID:      "thing-1",
		Buffer:       buf,
		AgentVersion: "v1.4.2",
		Source:       "main",
	}

	captured := false
	func() {
		defer func() {
			// Outer recover swallows the re-panic so the test continues.
			if r := recover(); r == nil {
				t.Fatalf("expected re-panic from Recover, got nil")
			}
		}()
		defer Recover(cfg, func() { captured = true })
		panic("boom")
	}()

	if !captured {
		t.Fatalf("OnCaptured / finalize hook not invoked")
	}

	events := buf.snapshot()
	if len(events) != 1 {
		t.Fatalf("buffered events = %d, want 1", len(events))
	}
	got := events[0]
	if got.Level != opsmetrics.LevelFatal {
		t.Errorf("level = %q, want fatal", got.Level)
	}
	if got.EventType != opsmetrics.EventTypeCrash {
		t.Errorf("eventType = %q, want crash", got.EventType)
	}
	if !strings.Contains(got.Message, "boom") {
		t.Errorf("message = %q, want contains \"boom\"", got.Message)
	}
	if got.StackTrace == "" {
		t.Errorf("stack trace empty")
	}
	if got.AgentVersion != "v1.4.2" {
		t.Errorf("agentVersion = %q", got.AgentVersion)
	}
	if got.ThingID != "thing-1" {
		t.Errorf("thingID = %q", got.ThingID)
	}
	if got.Source != "main" {
		t.Errorf("source = %q, want main", got.Source)
	}
	if got.MessageHash == "" {
		t.Errorf("messageHash empty")
	}
	if got.RepeatCount != 1 {
		t.Errorf("repeatCount = %d, want 1", got.RepeatCount)
	}
}

func TestRecoveryNoOpWhenNoPanic(t *testing.T) {
	buf := &recordingBuffer{}
	cfg := RecoveryConfig{ThingID: "t1", Buffer: buf, Source: "main"}

	finalizeCalled := false
	func() {
		defer Recover(cfg, func() { finalizeCalled = true })
	}()

	if len(buf.snapshot()) != 0 {
		t.Errorf("buffer should be empty when no panic; got %d", len(buf.snapshot()))
	}
	if finalizeCalled {
		t.Errorf("finalize must not run when there is no panic")
	}
}

func TestRecoveryPerSourceLabel(t *testing.T) {
	buf := &recordingBuffer{}
	cfg := RecoveryConfig{ThingID: "t1", Buffer: buf, Source: "audit-drain"}

	func() {
		defer func() {
			_ = recover()
		}()
		defer Recover(cfg, nil)
		panic("drain explosion")
	}()

	events := buf.snapshot()
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if events[0].Source != "audit-drain" {
		t.Errorf("source = %q, want audit-drain", events[0].Source)
	}
}

func TestRecoveryNilBufferDoesNotCrash(t *testing.T) {
	cfg := RecoveryConfig{ThingID: "t1", Source: "main"} // Buffer omitted

	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatalf("expected re-panic")
			}
		}()
		defer Recover(cfg, nil)
		panic("nil buf")
	}()
}

package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"log/slog"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
)

type memProducer struct {
	mu         sync.Mutex
	messages   []memMsg
	enqueueErr error
}

type memMsg struct {
	queue string
	data  []byte
}

func (p *memProducer) Publish(_ context.Context, _ string, _ []byte) error { return nil }
func (p *memProducer) Enqueue(_ context.Context, queue string, data []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.enqueueErr != nil {
		return p.enqueueErr
	}
	p.messages = append(p.messages, memMsg{queue: queue, data: data})
	return nil
}
func (p *memProducer) Close() error { return nil }
func (p *memProducer) msgs() []memMsg {
	p.mu.Lock()
	defer p.mu.Unlock()
	cp := make([]memMsg, len(p.messages))
	copy(cp, p.messages)
	return cp
}

func TestLog_PublishesToMQ(t *testing.T) {
	prod := &memProducer{}
	logger := slog.Default()
	w := NewWriter(prod, "nexus.event.admin-audit", logger)

	w.LogObserved(context.Background(), Entry{
		ActorID:     "user-admin-1",
		ActorLabel:  "admin@nexus.ai",
		ActorRole:   "super_admin",
		SourceIP:    "10.0.0.1",
		Action:      "UPDATE_HOOK_CONFIG",
		EntityType:  "HookConfig",
		EntityID:    "hook-123",
		BeforeState: map[string]any{"enabled": false},
		AfterState:  map[string]any{"enabled": true},
	})

	msgs := prod.msgs()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}

	m := msgs[0]
	if m.queue != "nexus.event.admin-audit" {
		t.Errorf("queue = %q, want %q", m.queue, "nexus.event.admin-audit")
	}

	var msg mq.AdminAuditMessage
	if err := json.Unmarshal(m.data, &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if msg.ActorID != "user-admin-1" {
		t.Errorf("ActorID = %q, want %q", msg.ActorID, "user-admin-1")
	}
	if msg.Action != "UPDATE_HOOK_CONFIG" {
		t.Errorf("Action = %q, want %q", msg.Action, "UPDATE_HOOK_CONFIG")
	}
	if msg.EntityType != "HookConfig" {
		t.Errorf("EntityType = %q, want %q", msg.EntityType, "HookConfig")
	}
	if msg.ID == "" {
		t.Error("ID should be a generated UUID, got empty")
	}
	if msg.Timestamp.IsZero() {
		t.Error("Timestamp should not be zero")
	}

	beforeJSON, _ := json.Marshal(msg.BeforeState)
	if string(beforeJSON) == "" || string(beforeJSON) == "null" {
		t.Error("BeforeState should not be empty")
	}
}

func TestLog_NoOpWhenNilProducer(t *testing.T) {
	logger := slog.Default()
	w := NewWriter(nil, "nexus.event.admin-audit", logger)

	// Should not panic, and should not call into the failure observer either —
	// the early-return for nil producer is the documented no-op mode (not a
	// failure). Returns nil error.
	called := false
	w = w.WithFailureObserver(func(string) { called = true })
	if err := w.Log(context.Background(), Entry{
		Action:     "DELETE_USER",
		EntityType: "User",
		EntityID:   "user-1",
	}); err != nil {
		t.Errorf("Log on nil-producer writer returned %v; want nil", err)
	}
	if called {
		t.Error("nil-producer no-op path must not invoke FailureObserver")
	}
}

// TestWithFailureObserver verifies the observer is wired in and returns the
// same *Writer for chaining (NewWriter(...).WithFailureObserver(fn)).
func TestWithFailureObserver_SetsHook(t *testing.T) {
	w := NewWriter(&memProducer{}, "q", slog.Default())
	got := w.WithFailureObserver(func(string) {})
	if got != w {
		t.Errorf("WithFailureObserver returned a different *Writer; want same instance for chaining")
	}
	if w.onFail == nil {
		t.Error("onFail not set after WithFailureObserver")
	}

	// nil disables the hook (documented behaviour).
	w2 := w.WithFailureObserver(nil)
	if w2.onFail != nil {
		t.Error("WithFailureObserver(nil) did not clear onFail")
	}
}

// TestLog_MarshalErrorPath verifies that a payload that fails json.Marshal
// (BeforeState/AfterState carrying an unsupported type, e.g. a channel) is
// surfaced as a "marshal" stage failure: Log returns the marshal error, the
// observer is invoked with the entry's Action, the warn log is emitted with
// stage="marshal", and NOTHING is enqueued.
func TestLog_MarshalErrorPath(t *testing.T) {
	prod := &memProducer{}
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	w := NewWriter(prod, "nexus.event.admin-audit", logger)

	var seen []string
	w = w.WithFailureObserver(func(action string) { seen = append(seen, action) })

	// A chan type is not JSON-serialisable — encoding/json returns a
	// *json.UnsupportedTypeError.
	bad := make(chan int)
	err := w.Log(context.Background(), Entry{
		Action:      "UPDATE_HOOK_CONFIG",
		EntityType:  "HookConfig",
		EntityID:    "hook-1",
		BeforeState: bad,
	})
	if err == nil {
		t.Fatal("Log returned nil; want json marshal error")
	}
	var unsupp *json.UnsupportedTypeError
	if !errors.As(err, &unsupp) {
		t.Errorf("Log error = %v; want *json.UnsupportedTypeError", err)
	}
	if len(prod.msgs()) != 0 {
		t.Errorf("producer received %d messages on marshal failure; want 0", len(prod.msgs()))
	}
	if len(seen) != 1 || seen[0] != "UPDATE_HOOK_CONFIG" {
		t.Errorf("observer invocations = %v; want [UPDATE_HOOK_CONFIG]", seen)
	}
	logOut := buf.String()
	if !strings.Contains(logOut, `event=admin_audit_log_publish_failed`) {
		t.Errorf("warn log missing event=admin_audit_log_publish_failed: %s", logOut)
	}
	if !strings.Contains(logOut, `stage=marshal`) {
		t.Errorf("warn log missing stage=marshal: %s", logOut)
	}
	if !strings.Contains(logOut, `action=UPDATE_HOOK_CONFIG`) {
		t.Errorf("warn log missing action=UPDATE_HOOK_CONFIG: %s", logOut)
	}
	if !strings.Contains(logOut, `entityType=HookConfig`) {
		t.Errorf("warn log missing entityType=HookConfig: %s", logOut)
	}
	if !strings.Contains(logOut, `entityId=hook-1`) {
		t.Errorf("warn log missing entityId=hook-1: %s", logOut)
	}
}

// TestLog_EnqueueErrorPath verifies the Enqueue failure stage: producer
// returns an error from Enqueue, Log returns that same error, the observer
// fires with the action, and the warn log carries stage="enqueue".
func TestLog_EnqueueErrorPath(t *testing.T) {
	wantErr := errors.New("nats: connection closed")
	prod := &memProducer{enqueueErr: wantErr}
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	w := NewWriter(prod, "nexus.event.admin-audit", logger)

	var seen []string
	w = w.WithFailureObserver(func(action string) { seen = append(seen, action) })

	err := w.Log(context.Background(), Entry{
		Action:     "DELETE_USER",
		EntityType: "User",
		EntityID:   "user-9",
	})
	if !errors.Is(err, wantErr) {
		t.Errorf("Log error = %v; want %v", err, wantErr)
	}
	if len(seen) != 1 || seen[0] != "DELETE_USER" {
		t.Errorf("observer invocations = %v; want [DELETE_USER]", seen)
	}
	logOut := buf.String()
	if !strings.Contains(logOut, `stage=enqueue`) {
		t.Errorf("warn log missing stage=enqueue: %s", logOut)
	}
	if !strings.Contains(logOut, `action=DELETE_USER`) {
		t.Errorf("warn log missing action=DELETE_USER: %s", logOut)
	}
}

// TestLog_FailureWithoutLoggerOrObserver covers the observeFailure branches
// where logger AND onFail are both nil — failure must still propagate up via
// the returned error and must not panic.
func TestLog_FailureWithoutLoggerOrObserver(t *testing.T) {
	wantErr := errors.New("enqueue down")
	prod := &memProducer{enqueueErr: wantErr}
	w := NewWriter(prod, "q", nil) // logger nil, onFail nil.

	err := w.Log(context.Background(), Entry{Action: "X", EntityType: "T"})
	if !errors.Is(err, wantErr) {
		t.Errorf("Log error = %v; want %v", err, wantErr)
	}
}

// TestLogObserved_SwallowsError verifies the fire-and-forget contract: even
// on Enqueue failure, LogObserved returns nothing and the caller is
// unaffected (failure surfaces only via observer + warn log).
func TestLogObserved_SwallowsError(t *testing.T) {
	prod := &memProducer{enqueueErr: errors.New("boom")}
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	w := NewWriter(prod, "q", logger)
	called := 0
	w = w.WithFailureObserver(func(string) { called++ })

	// Must not panic; must invoke observer.
	w.LogObserved(context.Background(), Entry{Action: "A", EntityType: "T"})
	if called != 1 {
		t.Errorf("observer called %d times; want 1", called)
	}
}

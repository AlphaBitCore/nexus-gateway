package audit

import (
	"bytes"
	"context"
	"errors"
	"github.com/goccy/go-json"
	"strings"
	"sync"
	"testing"

	"log/slog"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/initiator"
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

// TestLog_PropagatesViaToMQ pins the middle link of the via propagation chain: a Via set on
// the Entry (by EntryFor from the in-process initiator context value) must ride on the published
// AdminAuditMessage so the Hub consumer can fold it into the hash chain. A human
// Entry (empty Via) must serialise with via omitted (omitempty) so the wire — and
// the downstream hash — is byte-identical to the pre-via format.
func TestLog_PropagatesViaToMQ(t *testing.T) {
	prod := &memProducer{}
	w := NewWriter(prod, "nexus.event.admin-audit", slog.Default())

	w.LogObserved(context.Background(), Entry{
		ActorID: "user-admin-1", Action: "create", EntityType: "virtual-key", EntityID: "vk-1",
		Via: initiator.ViaAssistant,
	})
	w.LogObserved(context.Background(), Entry{
		ActorID: "user-admin-2", Action: "create", EntityType: "virtual-key", EntityID: "vk-2",
	})

	msgs := prod.msgs()
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}

	var assistantMsg mq.AdminAuditMessage
	if err := json.Unmarshal(msgs[0].data, &assistantMsg); err != nil {
		t.Fatalf("unmarshal assistant msg: %v", err)
	}
	if assistantMsg.Via != initiator.ViaAssistant {
		t.Errorf("assistant msg Via = %q, want %q", assistantMsg.Via, initiator.ViaAssistant)
	}

	// Human row: via must be ABSENT from the JSON (omitempty), not just empty —
	// this is what guarantees the hash chain stays byte-identical for human writes.
	if bytes.Contains(msgs[1].data, []byte(`"via"`)) {
		t.Errorf("human msg JSON should omit the via field, got: %s", msgs[1].data)
	}
	var humanMsg mq.AdminAuditMessage
	if err := json.Unmarshal(msgs[1].data, &humanMsg); err != nil {
		t.Fatalf("unmarshal human msg: %v", err)
	}
	if humanMsg.Via != "" {
		t.Errorf("human msg Via = %q, want empty", humanMsg.Via)
	}
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

// TestLogCritical_SurfacesEnqueueFailure is the fail-closed
// regression: for a security-relevant mutation, an MQ enqueue failure must
// be RETURNED to the caller (so the handler can answer 500) — not swallowed
// like LogObserved. The failure must still be counted via the observer (the
// admin.audit_log_failed_total metric is driven from there).
func TestLogCritical_SurfacesEnqueueFailure(t *testing.T) {
	wantErr := errors.New("nats: connection closed")
	prod := &memProducer{enqueueErr: wantErr}
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	w := NewWriter(prod, "q", logger)
	var seen []string
	w = w.WithFailureObserver(func(action string) { seen = append(seen, action) })

	err := w.LogCritical(context.Background(), Entry{Action: "iam.policy.update", EntityType: "IamPolicy", EntityID: "p-1"})
	if !errors.Is(err, wantErr) {
		t.Fatalf("LogCritical error = %v; want %v (must NOT swallow — fail-closed)", err, wantErr)
	}
	if len(seen) != 1 || seen[0] != "iam.policy.update" {
		t.Errorf("observer invocations = %v; want [iam.policy.update] (metric must still count the failure)", seen)
	}
}

// TestLogCritical_SuccessReturnsNil verifies the happy path: a successful
// publish returns nil so the caller proceeds normally, and the entry lands
// on the queue.
func TestLogCritical_SuccessReturnsNil(t *testing.T) {
	prod := &memProducer{}
	w := NewWriter(prod, "nexus.event.admin-audit", slog.Default())

	if err := w.LogCritical(context.Background(), Entry{Action: "credential.rotate", EntityType: "Credential", EntityID: "cred-7"}); err != nil {
		t.Fatalf("LogCritical returned %v; want nil on success", err)
	}
	if len(prod.msgs()) != 1 {
		t.Fatalf("producer received %d messages; want 1", len(prod.msgs()))
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

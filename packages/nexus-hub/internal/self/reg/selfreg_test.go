package selfreg

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/store"
)

// fakeStore is a thingStore stand-in. Tests configure UpdateLastSeen's return
// per call (queue) and observe UpsertThingEnrollment invocations.
type fakeStore struct {
	mu sync.Mutex

	upsertCalls atomic.Int32

	// updateLastSeenErrs is consumed in order; nil means "ok". When the queue
	// drains, subsequent calls return nil so a stuck test doesn't hang the
	// goroutine in unexpected ways.
	updateLastSeenErrs []error

	// updateLastSeenSignal fires once per UpdateLastSeen call so the test can
	// wait without using sleep loops.
	updateLastSeenSignal chan struct{}

	// lastUpsertParams retains the params from the most recent upsert, so the
	// test can assert ID/Type were forwarded correctly.
	lastUpsertParams store.UpsertThingParams

	// upsertErr lets tests pin an error for the next (or every) Upsert call.
	upsertErr error
	// updateStatusErr is returned by UpdateThingStatus.
	updateStatusErr error
}

func newFakeStore(updateErrs ...error) *fakeStore {
	return &fakeStore{
		updateLastSeenErrs:   updateErrs,
		updateLastSeenSignal: make(chan struct{}, 16),
	}
}

func (f *fakeStore) UpsertThingEnrollment(_ context.Context, p store.UpsertThingParams) error {
	f.mu.Lock()
	f.lastUpsertParams = p
	err := f.upsertErr
	f.mu.Unlock()
	f.upsertCalls.Add(1)
	return err
}

func (f *fakeStore) UpdateLastSeen(_ context.Context, _ string) error {
	f.mu.Lock()
	var err error
	if len(f.updateLastSeenErrs) > 0 {
		err = f.updateLastSeenErrs[0]
		f.updateLastSeenErrs = f.updateLastSeenErrs[1:]
	}
	f.mu.Unlock()

	select {
	case f.updateLastSeenSignal <- struct{}{}:
	default:
	}
	return err
}

func (f *fakeStore) UpdateThingStatus(_ context.Context, _, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.updateStatusErr
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newTestRegistrar wires a SelfRegistrar to a fake store directly, bypassing
// the public New() constructor (which is typed against *store.Store).
func newTestRegistrar(fake *fakeStore) *SelfRegistrar {
	return &SelfRegistrar{
		cfg: Config{
			InstanceID: "hub-test",
			Address:    "127.0.0.1:3060",
			Version:    "test",
		},
		store:  fake,
		logger: discardLogger().With("component", "selfreg"),
	}
}

// waitForCalls waits for fake to record at least `count` UpdateLastSeen calls
// or fails the test on timeout.
func waitForUpdateLastSeenCalls(t *testing.T, fake *fakeStore, count int, within time.Duration) {
	t.Helper()
	deadline := time.After(within)
	seen := 0
	for seen < count {
		select {
		case <-fake.updateLastSeenSignal:
			seen++
		case <-deadline:
			t.Fatalf("timed out waiting for %d UpdateLastSeen calls; saw %d", count, seen)
		}
	}
}

func TestNew_AssignsFields(t *testing.T) {
	// New() with a nil *store.Store is acceptable for assignment-only
	// construction — the store is only dereferenced during Register/Deregister.
	cfg := Config{
		InstanceID:       "hub-x",
		Address:          "https://hub.local",
		SchedulerEnabled: true,
		Version:          "1.2.3",
	}
	r := New(cfg, nil, discardLogger())
	if r == nil {
		t.Fatal("New returned nil")
	}
	if r.cfg.InstanceID != "hub-x" {
		t.Errorf("cfg.InstanceID: %q", r.cfg.InstanceID)
	}
	if r.cfg.SchedulerEnabled != true {
		t.Errorf("cfg.SchedulerEnabled: %v", r.cfg.SchedulerEnabled)
	}
	if r.logger == nil {
		t.Error("logger must be wrapped, not nil")
	}
	// Note: r.store is interface thingStore; a nil *store.Store assigned to
	// it produces a non-nil interface value (Go interface-nil gotcha).
	// We don't assert nil-ness here; the assertion that matters is the
	// other fields landed correctly.
}

func TestRole_DependsOnSchedulerEnabled(t *testing.T) {
	// Role string is observable in the upsert and in logs; drift here would
	// silently change the registry's role column.
	scheduler := &SelfRegistrar{cfg: Config{SchedulerEnabled: true}}
	if got := scheduler.role(); got != "scheduler" {
		t.Errorf("scheduler role: got %q, want scheduler", got)
	}
	def := &SelfRegistrar{cfg: Config{SchedulerEnabled: false}}
	if got := def.role(); got != "default" {
		t.Errorf("default role: got %q, want default", got)
	}
}

func TestRegister_DoUpsertErrorPropagates(t *testing.T) {
	// Initial registration failure must bubble out — startup code keys
	// off this return to fail-fast rather than silently skip the
	// heartbeat loop.
	fake := newFakeStore()
	fake.upsertErr = errors.New("db down")
	r := newTestRegistrar(fake)

	err := r.Register(context.Background())
	if err == nil {
		t.Fatal("Register should propagate upsert error")
	}
}

func TestHeartbeat_GenericErrorLogsAndContinues(t *testing.T) {
	// Non-ErrNotFound failures must NOT trigger re-registration and must
	// NOT abort the loop — they just log and continue, so Hub stays
	// trying. Without this, a transient DB hiccup would kill the loop.
	prev := heartbeatInterval
	heartbeatInterval = 5 * time.Millisecond
	t.Cleanup(func() { heartbeatInterval = prev })

	// Queue a generic error followed by nils so loop continues.
	fake := newFakeStore(errors.New("transient"), nil, nil, nil)
	r := newTestRegistrar(fake)
	if err := r.Register(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Deregister(context.Background()) })

	waitForUpdateLastSeenCalls(t, fake, 3, time.Second)
	// Upsert called once at Register; generic error must NOT trigger re-upsert.
	if got := fake.upsertCalls.Load(); got != 1 {
		t.Errorf("upsert calls after generic error: %d, want 1", got)
	}
}

func TestDeregister_UpdateStatusErrorPropagates(t *testing.T) {
	fake := newFakeStore()
	r := newTestRegistrar(fake)
	if err := r.Register(context.Background()); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.updateStatusErr = errors.New("status update failed")
	fake.mu.Unlock()

	err := r.Deregister(context.Background())
	if err == nil {
		t.Fatal("Deregister should propagate UpdateThingStatus error")
	}
}

func TestDeregister_IdempotentAfterCancelAlready(t *testing.T) {
	// Second Deregister must not panic on the nil s.cancel field after
	// first Deregister already cleared it.
	fake := newFakeStore()
	r := newTestRegistrar(fake)
	if err := r.Register(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := r.Deregister(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Second call: s.cancel is nil now.
	if err := r.Deregister(context.Background()); err != nil {
		t.Errorf("second Deregister should not error: %v", err)
	}
}

// TestRegister_UpsertCalledOnce asserts that Register triggers exactly one
// UpsertThingEnrollment with the correct ID and Type, then starts the
// heartbeat loop.
func TestRegister_UpsertCalledOnce(t *testing.T) {
	prev := heartbeatInterval
	heartbeatInterval = 10 * time.Millisecond
	t.Cleanup(func() { heartbeatInterval = prev })

	fake := newFakeStore()
	r := newTestRegistrar(fake)

	if err := r.Register(context.Background()); err != nil {
		t.Fatalf("Register: %v", err)
	}
	t.Cleanup(func() { _ = r.Deregister(context.Background()) })

	if got := fake.upsertCalls.Load(); got != 1 {
		t.Fatalf("upsert calls = %d, want 1", got)
	}

	fake.mu.Lock()
	gotID := fake.lastUpsertParams.ID
	gotType := fake.lastUpsertParams.Type
	fake.mu.Unlock()

	if gotID != "hub-test" {
		t.Errorf("upsert ID = %q, want %q", gotID, "hub-test")
	}
	if gotType != ThingType {
		t.Errorf("upsert Type = %q, want %q (canonical ThingType)", gotType, ThingType)
	}
}

// TestHeartbeat_NotFound_TriggersReRegister asserts that when UpdateLastSeen
// returns store.ErrNotFound, the heartbeat re-runs the upsert exactly once
// per occurrence (not in a tight retry loop), then resumes normal heartbeats.
func TestHeartbeat_NotFound_TriggersReRegister(t *testing.T) {
	prev := heartbeatInterval
	heartbeatInterval = 10 * time.Millisecond
	t.Cleanup(func() { heartbeatInterval = prev })

	// First UpdateLastSeen: ok (initial heartbeat after Register).
	// Second: ErrNotFound (row pruned).
	// Third onward: ok again (re-register restored the row).
	fake := newFakeStore(nil, store.ErrNotFound, nil, nil)
	r := newTestRegistrar(fake)

	if err := r.Register(context.Background()); err != nil {
		t.Fatalf("Register: %v", err)
	}
	t.Cleanup(func() { _ = r.Deregister(context.Background()) })

	// Wait for: initial Register upsert + 4 heartbeats = enough to observe the
	// ErrNotFound branch and one normal tick after it.
	waitForUpdateLastSeenCalls(t, fake, 4, 2*time.Second)

	// Register did one upsert; the heartbeat self-heal must trigger exactly
	// one more (only on the ErrNotFound tick). Anything > 2 means the
	// heartbeat is upserting on every tick or retrying in a loop.
	if got := fake.upsertCalls.Load(); got != 2 {
		t.Fatalf("upsert calls = %d, want 2 (Register + one self-heal)", got)
	}
}

// TestHeartbeat_NotFound_ReRegisterFails asserts that when UpdateLastSeen
// returns ErrNotFound and the subsequent doUpsert also fails, the loop logs
// a warning and continues (it does NOT exit or crash).
func TestHeartbeat_NotFound_ReRegisterFails(t *testing.T) {
	prev := heartbeatInterval
	heartbeatInterval = 10 * time.Millisecond
	t.Cleanup(func() { heartbeatInterval = prev })

	// Fake that makes re-upsert fail after the initial Register succeeds.
	type countedStore struct {
		*fakeStore
		upsertFailAfter int
	}
	base := newFakeStore(nil, store.ErrNotFound, nil, nil, nil)
	cs := &countedStore{fakeStore: base, upsertFailAfter: 1}

	// Override UpsertThingEnrollment to fail on calls after the first.
	// We do this by setting upsertErr after Register (which clears it after).
	r := &SelfRegistrar{
		cfg: Config{
			InstanceID: "hub-failheal",
			Version:    "test",
		},
		store:  cs,
		logger: discardLogger().With("component", "selfreg"),
	}

	if err := r.Register(context.Background()); err != nil {
		t.Fatalf("Register: %v", err)
	}
	// Now make re-upserts fail.
	cs.mu.Lock()
	cs.upsertErr = errors.New("re-upsert failed")
	cs.mu.Unlock()

	t.Cleanup(func() { _ = r.Deregister(context.Background()) })

	// Wait for ErrNotFound heartbeat + at least one more tick to prove the loop
	// didn't exit after the failed re-upsert.
	waitForUpdateLastSeenCalls(t, base, 3, 2*time.Second)
}

// TestHeartbeat_GenericError_NoReRegister asserts that non-NotFound errors
// from UpdateLastSeen are logged but do NOT trigger a re-register — only the
// row-missing condition warrants the heavier upsert.
func TestHeartbeat_GenericError_NoReRegister(t *testing.T) {
	prev := heartbeatInterval
	heartbeatInterval = 10 * time.Millisecond
	t.Cleanup(func() { heartbeatInterval = prev })

	otherErr := errors.New("connection reset")
	fake := newFakeStore(nil, otherErr, otherErr, nil)
	r := newTestRegistrar(fake)

	if err := r.Register(context.Background()); err != nil {
		t.Fatalf("Register: %v", err)
	}
	t.Cleanup(func() { _ = r.Deregister(context.Background()) })

	waitForUpdateLastSeenCalls(t, fake, 4, 2*time.Second)

	if got := fake.upsertCalls.Load(); got != 1 {
		t.Fatalf("upsert calls = %d, want 1 (only initial Register; generic errors must not re-upsert)", got)
	}
}

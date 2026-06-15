package selfshadow

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/store"
)

// fakeReader is a shadowReader that returns canned Thing rows and
// records UpdateShadowReport invocations so tests can assert versions.
type fakeReader struct {
	mu sync.Mutex

	thing       *store.Thing
	getErr      error
	updateErr   error
	updateCalls atomic.Int32

	lastReported    map[string]any
	lastReportedVer int64
	lastOutcomes    map[string]store.ReportedKeyOutcome
}

func (f *fakeReader) setThing(t *store.Thing) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.thing = t
}

func (f *fakeReader) GetThing(_ context.Context, _ string) (*store.Thing, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return nil, f.getErr
	}
	if f.thing == nil {
		return nil, store.ErrNotFound
	}
	// Deep-ish copy so the test can mutate independently.
	t := *f.thing
	if f.thing.Desired != nil {
		t.Desired = make(map[string]any, len(f.thing.Desired))
		for k, v := range f.thing.Desired {
			t.Desired[k] = v
		}
	}
	if f.thing.Reported != nil {
		t.Reported = make(map[string]any, len(f.thing.Reported))
		for k, v := range f.thing.Reported {
			t.Reported[k] = v
		}
	}
	return &t, nil
}

func (f *fakeReader) UpdateShadowReport(_ context.Context, _ string, reported map[string]any, reportedVer int64, outcomes map[string]store.ReportedKeyOutcome) error {
	f.updateCalls.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.updateErr != nil {
		return f.updateErr
	}
	f.lastReported = reported
	f.lastReportedVer = reportedVer
	f.lastOutcomes = outcomes
	if f.thing != nil {
		f.thing.Reported = reported
		f.thing.ReportedVer = reportedVer
		f.thing.ReportedOutcomes = outcomes
	}
	return nil
}

// recordingHandler records the payloads passed to Apply for assertions.
type recordingHandler struct {
	mu       sync.Mutex
	payloads [][]byte
	errOnce  error
	panicOn  bool
}

func (h *recordingHandler) Apply(_ context.Context, state json.RawMessage) error {
	if h.panicOn {
		panic("intentional handler panic")
	}
	h.mu.Lock()
	h.payloads = append(h.payloads, append([]byte{}, state...))
	err := h.errOnce
	h.errOnce = nil
	h.mu.Unlock()
	return err
}

func (h *recordingHandler) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.payloads)
}

func (h *recordingHandler) last() []byte {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.payloads) == 0 {
		return nil
	}
	return h.payloads[len(h.payloads)-1]
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// stubNotifier is a notifier used in tests that exercise Start without a
// real Postgres pool. Acquire blocks until ctx is cancelled so the
// listen loop stays alive and gives applyAll time to run.
type stubNotifier struct{}

func (stubNotifier) Acquire(ctx context.Context) (pooledListener, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// newTestManager wires a Manager to the supplied fakes without going
// through New (which expects a real *pgxpool.Pool).
func newTestManager(id string, r shadowReader) *Manager {
	return newManager(id, stubNotifier{}, r, discardLogger())
}

// TestApplyAll_DispatchesRegisteredKeys asserts that applyAll fires
// the registered handler for every key present in BOTH thing.desired
// AND the handler registry, and writes reported+reported_ver back.
func TestApplyAll_DispatchesRegisteredKeys(t *testing.T) {
	r := &fakeReader{}
	r.setThing(&store.Thing{
		ID:         "hub-test",
		Type:       "nexus-hub",
		DesiredVer: 7,
		Desired: map[string]any{
			"observability": map[string]any{
				"enabled":      true,
				"endpoint":     "http://otel:4318",
				"serviceName":  "nexus-hub",
				"samplingRate": 0.5,
			},
			"unregistered_key": map[string]any{"foo": "bar"},
		},
	})
	mgr := newTestManager("hub-test", r)

	obs := &recordingHandler{}
	mgr.Register("observability", obs)

	if err := mgr.applyAll(context.Background()); err != nil {
		t.Fatalf("applyAll: %v", err)
	}

	if got := obs.count(); got != 1 {
		t.Fatalf("observability handler calls = %d, want 1", got)
	}

	// Payload must be valid JSON; ensure it round-trips.
	var decoded map[string]any
	if err := json.Unmarshal(obs.last(), &decoded); err != nil {
		t.Fatalf("handler payload not valid JSON: %v (%s)", err, obs.last())
	}
	if decoded["endpoint"] != "http://otel:4318" {
		t.Errorf("handler payload endpoint = %v, want http://otel:4318", decoded["endpoint"])
	}

	if r.updateCalls.Load() != 1 {
		t.Errorf("UpdateShadowReport calls = %d, want 1", r.updateCalls.Load())
	}
	if r.lastReportedVer != 7 {
		t.Errorf("reportedVer = %d, want 7", r.lastReportedVer)
	}
	if _, ok := r.lastReported["observability"]; !ok {
		t.Errorf("reported should echo observability key; got %v", r.lastReported)
	}
}

// TestApplyAll_IdempotentOnSameVersion asserts that re-running
// applyAll without a desired_ver bump is a no-op (no handler dispatch,
// no report write).
func TestApplyAll_IdempotentOnSameVersion(t *testing.T) {
	r := &fakeReader{}
	r.setThing(&store.Thing{
		ID:         "hub-test",
		DesiredVer: 3,
		Desired:    map[string]any{"observability": map[string]any{}},
	})
	mgr := newTestManager("hub-test", r)
	obs := &recordingHandler{}
	mgr.Register("observability", obs)

	if err := mgr.applyAll(context.Background()); err != nil {
		t.Fatalf("first applyAll: %v", err)
	}
	if err := mgr.applyAll(context.Background()); err != nil {
		t.Fatalf("second applyAll: %v", err)
	}

	if got := obs.count(); got != 1 {
		t.Errorf("handler calls = %d, want 1 (second applyAll must short-circuit on appliedVer)", got)
	}
	if got := r.updateCalls.Load(); got != 1 {
		t.Errorf("UpdateShadowReport calls = %d, want 1", got)
	}
}

// TestApplyAll_HandlerPanicRecovered asserts that a panicking handler
// does NOT crash the manager: appliedVer still advances, the report
// still writes, and the manager remains usable for subsequent dispatch.
func TestApplyAll_HandlerPanicRecovered(t *testing.T) {
	r := &fakeReader{}
	r.setThing(&store.Thing{
		ID:         "hub-test",
		DesiredVer: 4,
		Desired: map[string]any{
			"observability": map[string]any{"enabled": false},
			"other_key":     map[string]any{"x": 1},
		},
	})
	mgr := newTestManager("hub-test", r)
	mgr.Register("observability", &recordingHandler{panicOn: true})

	otherCalled := atomic.Int32{}
	mgr.Register("other_key", HandlerFunc(func(_ context.Context, _ json.RawMessage) error {
		otherCalled.Add(1)
		return nil
	}))

	if err := mgr.applyAll(context.Background()); err != nil {
		t.Fatalf("applyAll: %v", err)
	}

	// Panic in one handler MUST NOT prevent the sibling from running.
	if got := otherCalled.Load(); got != 1 {
		t.Errorf("other handler calls = %d, want 1 (panic in sibling must be isolated)", got)
	}
	// Report write still proceeds.
	if r.updateCalls.Load() != 1 {
		t.Errorf("UpdateShadowReport calls = %d, want 1", r.updateCalls.Load())
	}
	if r.lastReportedVer != 4 {
		t.Errorf("reportedVer = %d, want 4", r.lastReportedVer)
	}
}

// TestApplyAll_GetThingError surfaces the error to the caller.
func TestApplyAll_GetThingError(t *testing.T) {
	r := &fakeReader{getErr: errors.New("db down")}
	mgr := newTestManager("hub-test", r)

	err := mgr.applyAll(context.Background())
	if err == nil {
		t.Fatalf("expected error from applyAll; got nil")
	}
	if r.updateCalls.Load() != 0 {
		t.Errorf("UpdateShadowReport must not be called on GetThing failure; got %d", r.updateCalls.Load())
	}
}

// TestApplyAll_SkipsKeysNotInDesired asserts that registering a handler
// for a key that's absent from thing.desired does not dispatch.
func TestApplyAll_SkipsKeysNotInDesired(t *testing.T) {
	r := &fakeReader{}
	r.setThing(&store.Thing{
		ID:         "hub-test",
		DesiredVer: 2,
		Desired:    map[string]any{"observability": map[string]any{"enabled": true}},
	})
	mgr := newTestManager("hub-test", r)

	obs := &recordingHandler{}
	mgr.Register("observability", obs)
	never := &recordingHandler{}
	mgr.Register("never_pushed", never)

	if err := mgr.applyAll(context.Background()); err != nil {
		t.Fatalf("applyAll: %v", err)
	}
	if obs.count() != 1 {
		t.Errorf("observability handler calls = %d, want 1", obs.count())
	}
	if never.count() != 0 {
		t.Errorf("never_pushed handler calls = %d, want 0 (key not in desired)", never.count())
	}
}

// TestApplyAll_HandlerErrorReportedKeyNotEchoed verifies F-0115: a handler
// returning an error does not abort the dispatch round, but its key is NOT
// echoed into reported. Echoing on failure would make the Configuration tab
// show the key as in-sync ("applied") when the apply actually failed; the
// truthful state is out-of-sync, and the failure is recorded in the per-key
// outcome ledger as an ApplyError.
func TestApplyAll_HandlerErrorReportedKeyNotEchoed(t *testing.T) {
	r := &fakeReader{}
	r.setThing(&store.Thing{
		ID:         "hub-test",
		DesiredVer: 9,
		Desired:    map[string]any{"observability": map[string]any{"enabled": true}},
	})
	mgr := newTestManager("hub-test", r)
	obs := &recordingHandler{errOnce: errors.New("apply failed")}
	mgr.Register("observability", obs)

	if err := mgr.applyAll(context.Background()); err != nil {
		t.Fatalf("applyAll: %v", err)
	}
	if obs.count() != 1 {
		t.Errorf("handler must run once even when it returns error; got %d", obs.count())
	}
	if _, ok := r.lastReported["observability"]; ok {
		t.Errorf("reported must NOT echo the desired key on handler error; got %v", r.lastReported)
	}
	oc, ok := r.lastOutcomes["observability"]
	if !ok || oc.ApplyError == nil {
		t.Errorf("outcome ledger must record the apply error; got %+v (present=%v)", oc, ok)
	}
}

// TestApplyAll_UnmanagedDesiredKeyMirrored verifies F-0258: a desired key
// with no registered handler is mirrored verbatim into reported so the
// Configuration tab shows it as in-sync. No handler means the apply is a
// no-op = already applied, so the truthful reported value equals desired.
func TestApplyAll_UnmanagedDesiredKeyMirrored(t *testing.T) {
	r := &fakeReader{}
	r.setThing(&store.Thing{
		ID:         "hub-test",
		DesiredVer: 4,
		Desired: map[string]any{
			"observability": map[string]any{"enabled": true},
			"unmanaged_key": map[string]any{"foo": "bar"},
		},
	})
	mgr := newTestManager("hub-test", r)
	obs := &recordingHandler{}
	mgr.Register("observability", obs) // only "observability" has a handler

	if err := mgr.applyAll(context.Background()); err != nil {
		t.Fatalf("applyAll: %v", err)
	}

	// Managed key echoed after successful apply.
	if _, ok := r.lastReported["observability"]; !ok {
		t.Errorf("managed key must be echoed into reported; got %v", r.lastReported)
	}
	// Unmanaged key mirrored verbatim.
	got, ok := r.lastReported["unmanaged_key"]
	if !ok {
		t.Fatalf("unmanaged key must be mirrored into reported; got %v", r.lastReported)
	}
	gm, _ := got.(map[string]any)
	if gm["foo"] != "bar" {
		t.Errorf("unmanaged key mirrored with wrong value: %v", got)
	}
	// No handler ran for the unmanaged key (no panic, no outcome error).
	if _, ok := r.lastOutcomes["unmanaged_key"]; ok {
		t.Errorf("unmanaged key must not appear in the outcome ledger; got %v", r.lastOutcomes["unmanaged_key"])
	}
}

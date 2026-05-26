package selfshadow

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/store"
)

// notifyStubNotifier is a test notifier that hands out a controllable
// pooledListener. Acquire returns errs from `errsRemaining` first (each
// decrementing); then the next stub conn from `conns`; finally, when
// exhausted, returns acquireErr (if set) or blocks waiting for ctx
// cancellation. This exercises the listen loop's reacquire / continue /
// shutdown branches without a real Postgres.
type notifyStubNotifier struct {
	mu            sync.Mutex
	conns         []*stubConn
	acquireErr    error
	errsRemaining int
	errSentinel   error
}

func (s *notifyStubNotifier) Acquire(ctx context.Context) (pooledListener, error) {
	s.mu.Lock()
	if s.errsRemaining > 0 {
		s.errsRemaining--
		err := s.errSentinel
		s.mu.Unlock()
		return nil, err
	}
	if len(s.conns) == 0 {
		s.mu.Unlock()
		if s.acquireErr != nil {
			return nil, s.acquireErr
		}
		// Block until ctx is cancelled; mimics "no more conns available".
		<-ctx.Done()
		return nil, ctx.Err()
	}
	c := s.conns[0]
	s.conns = s.conns[1:]
	s.mu.Unlock()
	return c, nil
}

// stubConn implements pooledListener using a Go channel for notifications.
type stubConn struct {
	notif    chan *pgconnNotification
	execErr  error
	waitErr  error
	released atomic.Int32
	execCall atomic.Int32
}

func (c *stubConn) Exec(_ context.Context, _ string) error {
	c.execCall.Add(1)
	return c.execErr
}

func (c *stubConn) WaitForNotification(ctx context.Context) (*pgconnNotification, error) {
	if c.waitErr != nil {
		return nil, c.waitErr
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case n := <-c.notif:
		if n == nil {
			return nil, errors.New("stub: notif channel closed")
		}
		return n, nil
	}
}

func (c *stubConn) Release() { c.released.Add(1) }

// TestRecordOutcome_EmptyKeyShortCircuits asserts the documented guard:
// recordOutcome("") must NOT touch m.outcomes. A regression here would
// stamp a phantom "" key into the per-key ledger and confuse Force Resync.
func TestRecordOutcome_EmptyKeyShortCircuits(t *testing.T) {
	mgr := newTestManager("hub-t", &fakeReader{})
	mgr.recordOutcome("", 99, errors.New("any"))

	snap := mgr.outcomesSnapshot()
	if _, ok := snap[""]; ok {
		t.Errorf("outcomes contains an empty-key entry: %v", snap)
	}
	if len(snap) != 0 {
		t.Errorf("expected empty outcomes snapshot, got %v", snap)
	}
}

// TestRecordOutcome_FailureThenSuccessPreservesLastApplied verifies the
// documented invariant: a failure preserves the previous successful
// AppliedAt / AppliedVersion and stamps ApplyError. The next success
// advances AppliedVersion AND clears ApplyError.
func TestRecordOutcome_FailureThenSuccessPreservesLastApplied(t *testing.T) {
	mgr := newTestManager("hub-t", &fakeReader{})

	// First, a successful apply at ver=5.
	mgr.recordOutcome("k", 5, nil)
	snap := mgr.outcomesSnapshot()
	if snap["k"].AppliedVersion == nil || *snap["k"].AppliedVersion != 5 {
		t.Fatalf("after success: AppliedVersion = %v, want 5", snap["k"].AppliedVersion)
	}
	if snap["k"].ApplyError != nil {
		t.Errorf("after success: ApplyError should be nil; got %+v", snap["k"].ApplyError)
	}

	// Then a failure at ver=6. AppliedVersion must NOT regress.
	mgr.recordOutcome("k", 6, errors.New("boom"))
	snap = mgr.outcomesSnapshot()
	if snap["k"].AppliedVersion == nil || *snap["k"].AppliedVersion != 5 {
		t.Errorf("after failure: AppliedVersion must stay at 5; got %v", snap["k"].AppliedVersion)
	}
	if snap["k"].ApplyError == nil || snap["k"].ApplyError.Message != "boom" {
		t.Errorf("after failure: ApplyError = %+v, want Message=boom", snap["k"].ApplyError)
	}

	// And a subsequent success at ver=7 must clear the ApplyError and advance.
	mgr.recordOutcome("k", 7, nil)
	snap = mgr.outcomesSnapshot()
	if snap["k"].AppliedVersion == nil || *snap["k"].AppliedVersion != 7 {
		t.Errorf("after recover: AppliedVersion = %v, want 7", snap["k"].AppliedVersion)
	}
	if snap["k"].ApplyError != nil {
		t.Errorf("after recover: ApplyError must be cleared; got %+v", snap["k"].ApplyError)
	}
}

// TestApplyAll_PreservesPriorReportedKeys asserts the documented carry-over
// behaviour: if thing.reported already contains an unrelated key, applyAll
// must NOT drop it when writing the new reported map. Otherwise admin-edited
// keys would be wiped every time another key changes.
func TestApplyAll_PreservesPriorReportedKeys(t *testing.T) {
	r := &fakeReader{}
	r.setThing(&store.Thing{
		ID:         "hub-test",
		DesiredVer: 2,
		Desired:    map[string]any{"observability": map[string]any{"enabled": true}},
		Reported: map[string]any{
			"some_other_key":  map[string]any{"x": 1},
			"and_yet_another": "value",
			"third_unrelated": true,
		},
	})
	mgr := newTestManager("hub-test", r)
	mgr.Register("observability", &recordingHandler{})

	if err := mgr.applyAll(context.Background()); err != nil {
		t.Fatalf("applyAll: %v", err)
	}
	for _, k := range []string{"some_other_key", "and_yet_another", "third_unrelated"} {
		if _, ok := r.lastReported[k]; !ok {
			t.Errorf("prior reported key %q was dropped on apply; got %v", k, r.lastReported)
		}
	}
	if _, ok := r.lastReported["observability"]; !ok {
		t.Errorf("newly applied key 'observability' missing from reported: %v", r.lastReported)
	}
}

// TestApplyAll_MarshalErrorRecordedAsFailureNoDispatch verifies the marshal-
// failure branch: a key whose desired value cannot be JSON-marshalled is
// skipped (handler NOT called) and the per-key outcome records an error so
// the operator sees the failure in Force-Resync.
func TestApplyAll_MarshalErrorRecordedAsFailureNoDispatch(t *testing.T) {
	// json.Marshal returns an error for NaN/Inf — we craft a desired value
	// that contains math.Inf to force a marshal failure.
	r := &fakeReader{}
	r.setThing(&store.Thing{
		ID:         "hub-test",
		DesiredVer: 11,
		Desired:    map[string]any{"observability": math.Inf(1)},
	})
	mgr := newTestManager("hub-test", r)
	obs := &recordingHandler{}
	mgr.Register("observability", obs)

	if err := mgr.applyAll(context.Background()); err != nil {
		t.Fatalf("applyAll: %v", err)
	}
	if obs.count() != 0 {
		t.Errorf("handler must NOT be dispatched on marshal failure; got %d calls", obs.count())
	}
	snap := mgr.outcomesSnapshot()
	if snap["observability"].ApplyError == nil {
		t.Errorf("expected ApplyError recorded for marshal failure; got %+v", snap["observability"])
	}
}

// TestApplyAll_UpdateShadowReportFailureSurfaces verifies a DB write error
// is wrapped and returned; appliedVer must NOT be advanced, so the next
// applyAll retries instead of silently considering the version applied.
func TestApplyAll_UpdateShadowReportFailureSurfaces(t *testing.T) {
	r := &fakeReader{updateErr: errors.New("pg down")}
	r.setThing(&store.Thing{
		ID:         "hub-test",
		DesiredVer: 4,
		Desired:    map[string]any{"observability": map[string]any{"enabled": true}},
	})
	mgr := newTestManager("hub-test", r)
	mgr.Register("observability", &recordingHandler{})

	err := mgr.applyAll(context.Background())
	if err == nil {
		t.Fatalf("expected error from applyAll on UpdateShadowReport failure; got nil")
	}
	if mgr.appliedVer.Load() == 4 {
		t.Errorf("appliedVer must NOT advance on UpdateShadowReport failure; got %d", mgr.appliedVer.Load())
	}

	// A second call must re-attempt (i.e. handler runs again) since
	// appliedVer is still 0.
	r.mu.Lock()
	r.updateErr = nil
	r.mu.Unlock()
	if err := mgr.applyAll(context.Background()); err != nil {
		t.Fatalf("second applyAll: %v", err)
	}
	if mgr.appliedVer.Load() != 4 {
		t.Errorf("after recovery: appliedVer = %d, want 4", mgr.appliedVer.Load())
	}
}

// TestDispatchOne_NilHandlerIsNoOp covers the defensive nil-handler branch.
// In practice Register never inserts nil, but the dispatch path must not
// panic if a stale nil ever reaches it.
func TestDispatchOne_NilHandlerIsNoOp(t *testing.T) {
	mgr := newTestManager("hub-test", &fakeReader{})
	err := mgr.dispatchOne(context.Background(), "k", nil, json.RawMessage(`{}`))
	if err != nil {
		t.Errorf("dispatchOne with nil handler must return nil; got %v", err)
	}
}

// TestSleepCtx_TimerFires verifies the normal sleep path returns true
// when the timer elapses before context cancellation.
func TestSleepCtx_TimerFires(t *testing.T) {
	if !sleepCtx(context.Background(), 5*time.Millisecond) {
		t.Error("sleepCtx returned false when timer should have fired")
	}
}

// TestSleepCtx_ContextCancelled verifies the ctx-cancel path returns false
// so callers can exit promptly on shutdown.
func TestSleepCtx_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if sleepCtx(ctx, 5*time.Second) {
		t.Error("sleepCtx returned true after ctx was cancelled")
	}
}

// TestStartStop_DispatchesNotificationsForOwnInstance asserts the end-to-end
// listen loop: a NOTIFY payload matching instanceID re-runs applyAll;
// a payload for another instance is dropped. Stop drains the goroutine.
func TestStartStop_DispatchesNotificationsForOwnInstance(t *testing.T) {
	r := &fakeReader{}
	r.setThing(&store.Thing{
		ID:         "hub-self",
		DesiredVer: 1,
		Desired:    map[string]any{"observability": map[string]any{"enabled": true}},
	})

	conn := &stubConn{notif: make(chan *pgconnNotification, 4)}
	n := &notifyStubNotifier{conns: []*stubConn{conn}}
	mgr := newManager("hub-self", n, r, discardLogger())

	obs := &recordingHandler{}
	mgr.Register("observability", obs)

	if err := mgr.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Wait for the initial applyAll synchronously-triggered by Start.
	if obs.count() != 1 {
		t.Errorf("initial applyAll: handler calls = %d, want 1", obs.count())
	}
	if conn.execCall.Load() == 0 {
		// listen() goroutine may need a moment to issue LISTEN.
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) && conn.execCall.Load() == 0 {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if got := conn.execCall.Load(); got == 0 {
		t.Errorf("listen loop did not call Exec(LISTEN); got %d", got)
	}

	// Send a notification for a DIFFERENT instance — must be dropped.
	conn.notif <- &pgconnNotification{Channel: Channel, Payload: "some-other-hub"}
	// Send a wrong-channel notification — must be dropped.
	conn.notif <- &pgconnNotification{Channel: "wrong_channel", Payload: "hub-self"}

	// Now bump desired_ver and send a matching notification — must re-apply.
	r.mu.Lock()
	r.thing.DesiredVer = 2
	r.thing.Desired = map[string]any{"observability": map[string]any{"enabled": false}}
	r.mu.Unlock()
	conn.notif <- &pgconnNotification{Channel: Channel, Payload: "hub-self"}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && obs.count() < 2 {
		time.Sleep(5 * time.Millisecond)
	}
	if obs.count() != 2 {
		t.Errorf("after matching notify: handler calls = %d, want 2", obs.count())
	}

	if err := mgr.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	// Stop is idempotent.
	if err := mgr.Stop(context.Background()); err != nil {
		t.Fatalf("Stop (idempotent): %v", err)
	}

	if conn.released.Load() == 0 {
		t.Error("listener loop did not Release pooled conn on shutdown")
	}
}

// TestListen_AcquireErrorRetriesUntilStop covers the acquire-error path:
// when notifier.Acquire returns err the loop logs, sleeps reacquireBackoff
// then retries — never crashes. Stop must terminate the loop promptly.
func TestListen_AcquireErrorRetriesUntilStop(t *testing.T) {
	r := &fakeReader{getErr: store.ErrNotFound} // initial applyAll absorbs this
	n := &notifyStubNotifier{acquireErr: errors.New("conn refused")}
	mgr := newManager("hub-self", n, r, discardLogger())

	if err := mgr.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Give the loop time to hit Acquire at least once and enter the sleep.
	time.Sleep(50 * time.Millisecond)

	done := make(chan struct{})
	go func() {
		_ = mgr.Stop(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not unblock the listen loop within 2s")
	}
}

// TestListen_ExecLISTENFailureReacquires covers the Exec("LISTEN ...")
// failure branch: when LISTEN itself fails the inner func returns; the
// outer loop reacquires (in our stub, the next conn). Verifies the loop
// recovers without crashing.
func TestListen_ExecLISTENFailureReacquires(t *testing.T) {
	r := &fakeReader{getErr: store.ErrNotFound}
	bad := &stubConn{notif: make(chan *pgconnNotification, 1), execErr: errors.New("LISTEN denied")}
	good := &stubConn{notif: make(chan *pgconnNotification, 1)}
	n := &notifyStubNotifier{conns: []*stubConn{bad, good}}
	mgr := newManager("hub-self", n, r, discardLogger())

	if err := mgr.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && good.execCall.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if good.execCall.Load() == 0 {
		t.Error("after first conn's LISTEN failed, second conn was never used")
	}
	if bad.released.Load() == 0 {
		t.Error("first (failing) conn was not Released")
	}
	_ = mgr.Stop(context.Background())
}

// TestListen_ApplyAllErrorOnNotifyLogged covers the err-branch in the
// listen loop where applyAll(ctx) fails after a matching notification.
// The listener must NOT exit; the next applyAll attempt should still run.
func TestListen_ApplyAllErrorOnNotifyLogged(t *testing.T) {
	// fakeReader.GetThing returns a sentinel error so every applyAll
	// inside listen fails. The listener must keep running.
	r := &fakeReader{getErr: errors.New("transient")}
	conn := &stubConn{notif: make(chan *pgconnNotification, 2)}
	n := &notifyStubNotifier{conns: []*stubConn{conn}}
	mgr := newManager("hub-self", n, r, discardLogger())

	if err := mgr.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Wait for LISTEN.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && conn.execCall.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}

	// Deliver a matching NOTIFY — applyAll inside listen will fail.
	conn.notif <- &pgconnNotification{Channel: Channel, Payload: "hub-self"}
	// Give the loop a moment to process.
	time.Sleep(50 * time.Millisecond)

	// Sanity: the listener is still alive and Stop drains it.
	stopped := make(chan struct{})
	go func() {
		_ = mgr.Stop(context.Background())
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not unblock listen loop after applyAll-on-notify error")
	}
}

// TestListen_WaitForNotificationErrorReacquires covers the wait-error
// branch: when WaitForNotification returns an error mid-loop the conn is
// released and the outer loop reacquires.
func TestListen_WaitForNotificationErrorReacquires(t *testing.T) {
	r := &fakeReader{getErr: store.ErrNotFound}
	bad := &stubConn{notif: make(chan *pgconnNotification), waitErr: errors.New("conn broken")}
	good := &stubConn{notif: make(chan *pgconnNotification)}
	n := &notifyStubNotifier{conns: []*stubConn{bad, good}}
	mgr := newManager("hub-self", n, r, discardLogger())

	if err := mgr.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && good.execCall.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if good.execCall.Load() == 0 {
		t.Error("after first conn's WaitForNotification failed, second conn was never used")
	}
	_ = mgr.Stop(context.Background())
}

// TestListen_AcquireErrorContinuesAfterBackoff covers the `continue`
// branch in listen() after sleepCtx returns true (timer elapsed without
// ctx cancellation). The first Acquire errors; the loop sleeps the
// shortened reacquireBackoff and then continues to a successful Acquire
// that receives a notification — proving the loop recovers from a
// transient pool failure rather than exiting on the first error.
func TestListen_AcquireErrorContinuesAfterBackoff(t *testing.T) {
	prev := reacquireBackoff
	reacquireBackoff = 20 * time.Millisecond
	t.Cleanup(func() { reacquireBackoff = prev })

	r := &fakeReader{}
	r.setThing(&store.Thing{
		ID:         "hub-self",
		DesiredVer: 1,
		Desired:    map[string]any{"observability": map[string]any{"enabled": true}},
	})
	good := &stubConn{notif: make(chan *pgconnNotification, 1)}
	n := &notifyStubNotifier{
		errsRemaining: 1,
		errSentinel:   errors.New("transient pool err"),
		conns:         []*stubConn{good},
	}
	mgr := newManager("hub-self", n, r, discardLogger())
	mgr.Register("observability", &recordingHandler{})

	if err := mgr.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Wait until the good conn's LISTEN fires — proves the `continue`
	// branch executed after the first transient error.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && good.execCall.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if good.execCall.Load() == 0 {
		t.Error("listen loop did not retry after backoff; good conn never received LISTEN")
	}

	_ = mgr.Stop(context.Background())
}

// TestListen_OuterCtxErrReturnsAfterConnFailure covers the outer
// `if ctx.Err() != nil { return }` check at the top of the listen loop.
// This branch fires when the inner func returns (e.g. wait error) AND
// the manager's listenCtx has been cancelled in the meantime — the loop
// must exit cleanly rather than try to reacquire on a dead context.
func TestListen_OuterCtxErrReturnsAfterConnFailure(t *testing.T) {
	r := &fakeReader{getErr: store.ErrNotFound}
	// A single failing conn — when its WaitForNotification errors, the
	// inner func returns. The notifier's mu-locked Acquire then blocks
	// because no more conns are queued and acquireErr is unset, so the
	// outer ctx.Err() check is the only exit path.
	bad := &stubConn{notif: make(chan *pgconnNotification), waitErr: errors.New("conn dead")}
	n := &notifyStubNotifier{conns: []*stubConn{bad}}
	mgr := newManager("hub-self", n, r, discardLogger())

	if err := mgr.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Wait for the bad conn to be Released — that's the signal the
	// inner func has returned and we're about to loop.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && bad.released.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if bad.released.Load() == 0 {
		t.Fatal("inner func did not return after WaitForNotification error")
	}

	// Now cancel — the outer ctx.Err() check must fire.
	stopDone := make(chan struct{})
	go func() {
		_ = mgr.Stop(context.Background())
		close(stopDone)
	}()
	select {
	case <-stopDone:
	case <-time.After(2 * time.Second):
		t.Fatal("listen loop did not exit via outer ctx.Err() check within 2s")
	}
}

// TestStop_BeforeStartIsNoOp asserts Stop on a manager that never called
// Start does not panic and returns nil.
func TestStop_BeforeStartIsNoOp(t *testing.T) {
	mgr := newManager("hub-self", &notifyStubNotifier{}, &fakeReader{}, discardLogger())
	if err := mgr.Stop(context.Background()); err != nil {
		t.Errorf("Stop before Start should return nil; got %v", err)
	}
}

// TestHandlerFunc_AdaptsPlainFunction covers the HandlerFunc adapter so
// callers can use a closure where ReloadHandler is required.
func TestHandlerFunc_AdaptsPlainFunction(t *testing.T) {
	called := atomic.Int32{}
	var lastPayload json.RawMessage
	h := HandlerFunc(func(_ context.Context, state json.RawMessage) error {
		called.Add(1)
		lastPayload = state
		return nil
	})
	if err := h.Apply(context.Background(), json.RawMessage(`{"k":1}`)); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if called.Load() != 1 {
		t.Errorf("HandlerFunc not invoked")
	}
	if string(lastPayload) != `{"k":1}` {
		t.Errorf("payload = %q, want {\"k\":1}", lastPayload)
	}
}

// TestNew_WiresPoolAdapter exercises the production constructor New(...)
// so the poolAdapter wrap path is covered. We pass nil pool / nil store —
// New does no I/O, so this only verifies field plumbing.
func TestNew_WiresPoolAdapter(t *testing.T) {
	mgr := New("hub-prod-id", nil, nil, discardLogger())
	if mgr == nil {
		t.Fatal("New returned nil")
	}
	if mgr.instanceID != "hub-prod-id" {
		t.Errorf("instanceID = %q, want hub-prod-id", mgr.instanceID)
	}
	if _, ok := mgr.notifier.(*poolAdapter); !ok {
		t.Errorf("notifier = %T, want *poolAdapter", mgr.notifier)
	}
}

// TestPoolAdapter_AcquireErrorOnUnreachablePool covers the err-branch of
// poolAdapter.Acquire by pointing it at a pool that fails to dial within
// a short context. The success branch and the poolConnAdapter methods
// touch *pgxpool.Conn directly and can only be exercised against a live
// Postgres — those statements are explicitly NOT covered by unit tests.
func TestPoolAdapter_AcquireErrorOnUnreachablePool(t *testing.T) {
	cfg, err := pgxpool.ParseConfig("postgres://nobody:nopw@127.0.0.1:1/none?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewWithConfig: %v", err)
	}
	defer pool.Close()

	a := &poolAdapter{pool: pool}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	pl, err := a.Acquire(ctx)
	if err == nil {
		if pl != nil {
			pl.Release()
		}
		t.Skip("unexpected successful Acquire against unreachable pool")
	}
	if pl != nil {
		t.Errorf("on error, pooledListener must be nil; got %T", pl)
	}
}

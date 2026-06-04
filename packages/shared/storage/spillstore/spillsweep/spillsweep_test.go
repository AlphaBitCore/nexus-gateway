package spillsweep

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/spillstore"
)

// fakeStore implements spillstore.SpillStore. Only Sweep carries behaviour;
// the rest satisfy the interface. onCall (if set) fires after each Sweep with
// the 1-based call count, letting a test cancel the loop deterministically.
type fakeStore struct {
	mu      sync.Mutex
	calls   int
	cutoffs []time.Time
	deleted int
	err     error
	onCall  func(n int)
}

func (f *fakeStore) Sweep(_ context.Context, olderThan time.Time) (int, error) {
	f.mu.Lock()
	f.calls++
	n := f.calls
	f.cutoffs = append(f.cutoffs, olderThan)
	f.mu.Unlock()
	if f.onCall != nil {
		f.onCall(n)
	}
	return f.deleted, f.err
}

func (f *fakeStore) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeStore) Put(context.Context, io.Reader, int64, spillstore.PutOptions) (audit.SpillRef, error) {
	return audit.SpillRef{}, nil
}
func (f *fakeStore) Get(context.Context, audit.SpillRef) (io.ReadCloser, error) { return nil, nil }
func (f *fakeStore) Delete(context.Context, audit.SpillRef) error               { return nil }
func (f *fakeStore) Stat(context.Context) (spillstore.Stats, error)             { return spillstore.Stats{}, nil }
func (f *fakeStore) Backend() string                                            { return "fake" }

// runWithTimeout runs Run and fails the test if it does not return promptly,
// so a stuck loop surfaces as a failure rather than a hang.
func runWithTimeout(t *testing.T, ctx context.Context, store spillstore.SpillStore, opts Options) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		Run(ctx, store, opts, nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within 2s")
	}
}

func TestRun_NilStore_NoOp(t *testing.T) {
	// A nil store must return immediately without blocking, even with a
	// never-cancelled context.
	runWithTimeout(t, context.Background(), nil, Options{Interval: time.Hour, Retention: time.Hour})
}

func TestRun_NonPositiveRetention_NoOp(t *testing.T) {
	store := &fakeStore{}
	runWithTimeout(t, context.Background(), store, Options{Interval: time.Hour, Retention: 0})
	if store.callCount() != 0 {
		t.Errorf("retention<=0 must not sweep; got %d calls", store.callCount())
	}
}

func TestRun_SweepsOnStartThenOnTick(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	retention := 48 * time.Hour
	store := &fakeStore{
		deleted: 3, // exercise the deleted>0 log branch
		onCall: func(n int) {
			if n >= 2 { // call 1 = on-start, call 2 = first tick
				cancel()
			}
		},
	}
	start := time.Now()
	runWithTimeout(t, ctx, store, Options{Interval: time.Millisecond, Retention: retention})

	if store.callCount() < 2 {
		t.Fatalf("expected >=2 sweeps (on-start + tick), got %d", store.callCount())
	}
	// The cutoff must be ~now-retention.
	store.mu.Lock()
	cutoff := store.cutoffs[0]
	store.mu.Unlock()
	wantLow := start.Add(-retention).Add(-time.Second)
	wantHigh := time.Now().Add(-retention).Add(time.Second)
	if cutoff.Before(wantLow) || cutoff.After(wantHigh) {
		t.Errorf("cutoff %v not within [%v,%v]", cutoff, wantLow, wantHigh)
	}
}

func TestRun_DefaultInterval_CancelStops(t *testing.T) {
	// Interval=0 selects DefaultInterval (6h, which never fires in the test).
	// The on-start sweep cancels the context, so the loop exits via ctx.Done
	// without waiting for a tick. deleted=0 exercises the no-log branch.
	ctx, cancel := context.WithCancel(context.Background())
	store := &fakeStore{
		deleted: 0,
		onCall:  func(n int) { cancel() },
	}
	runWithTimeout(t, ctx, store, Options{Interval: 0, Retention: time.Hour})
	if store.callCount() != 1 {
		t.Errorf("expected exactly 1 (on-start) sweep before cancel, got %d", store.callCount())
	}
}

func TestRun_SweepError_LoggedNotFatal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	store := &fakeStore{
		err:    errors.New("boom"),
		onCall: func(n int) { cancel() },
	}
	// Must not panic; the error is logged and the loop exits on the cancelled ctx.
	runWithTimeout(t, ctx, store, Options{Interval: time.Millisecond, Retention: time.Hour})
	if store.callCount() < 1 {
		t.Error("expected at least the on-start sweep")
	}
}

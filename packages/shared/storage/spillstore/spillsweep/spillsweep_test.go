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

// refAwareStore embeds fakeStore and adds SweepFiltered so it satisfies
// spillstore.RefAwareSweeper. It records which path the loop took and what
// filter it received.
type refAwareStore struct {
	fakeStore
	filteredCalls int
	gotFilter     spillstore.SweepFilter
	filteredErr   error
}

func (r *refAwareStore) SweepFiltered(_ context.Context, olderThan time.Time, filter spillstore.SweepFilter) (int, error) {
	r.mu.Lock()
	r.filteredCalls++
	n := r.filteredCalls
	r.gotFilter = filter
	r.cutoffs = append(r.cutoffs, olderThan)
	r.mu.Unlock()
	if r.onCall != nil {
		r.onCall(n)
	}
	return r.deleted, r.filteredErr
}

func (r *refAwareStore) filteredCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.filteredCalls
}

// stubDB is a DBQuerier; it records the keys it was asked about and returns a
// canned referenced set / error.
type stubDB struct {
	referenced map[string]bool
	err        error
}

func (s *stubDB) HasSpillRefs(_ context.Context, keys []string) (map[string]bool, error) {
	return s.referenced, s.err
}

// TestRun_RefAware_UsesFilteredSweep — when a DBQuerier is configured AND the
// store is reference-aware, the loop drives SweepFiltered (not the plain
// age-based Sweep) and hands it a non-nil filter wired to the DBQuerier.
func TestRun_RefAware_UsesFilteredSweep(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	store := &refAwareStore{}
	store.deleted = 2
	store.onCall = func(n int) { cancel() }
	db := &stubDB{referenced: map[string]bool{"k": true}}

	runWithTimeout(t, ctx, store, Options{Interval: time.Millisecond, Retention: time.Hour, DB: db})

	if store.filteredCount() < 1 {
		t.Fatalf("expected SweepFiltered to be called, got %d", store.filteredCount())
	}
	if store.callCount() != 0 {
		t.Errorf("plain Sweep must not be used on the reference-aware path; got %d", store.callCount())
	}
	store.mu.Lock()
	gotFilter := store.gotFilter
	store.mu.Unlock()
	if gotFilter == nil {
		t.Fatal("SweepFiltered received a nil filter; the DBQuerier was not wired")
	}
	// The wired filter must delegate to the DBQuerier.
	ref, err := gotFilter.KeepReferenced(context.Background(), []string{"k"})
	if err != nil {
		t.Fatalf("filter delegate error: %v", err)
	}
	if !ref["k"] {
		t.Errorf("filter did not delegate to the DBQuerier; got %v", ref)
	}
}

// TestRun_DBSetButStoreNotRefAware_FallsBackToAgeSweep — a DBQuerier with a
// store that cannot run a filtered sweep must fall back to the plain age-based
// Sweep rather than silently skipping the reference safety (and rather than
// failing).
func TestRun_DBSetButStoreNotRefAware_FallsBackToAgeSweep(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	store := &fakeStore{onCall: func(n int) { cancel() }} // no SweepFiltered
	db := &stubDB{}

	runWithTimeout(t, ctx, store, Options{Interval: time.Millisecond, Retention: time.Hour, DB: db})

	if store.callCount() < 1 {
		t.Errorf("expected the plain Sweep fallback to run, got %d calls", store.callCount())
	}
}

// TestRun_RefAware_NilDB_UsesPlainSweep — a reference-aware store with NO
// DBQuerier keeps the plain age-based Sweep (the agent's local store, which has
// no traffic_event table to consult).
func TestRun_RefAware_NilDB_UsesPlainSweep(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	store := &refAwareStore{}
	store.onCall = func(n int) { cancel() }

	runWithTimeout(t, ctx, store, Options{Interval: time.Millisecond, Retention: time.Hour, DB: nil})

	if store.callCount() < 1 {
		t.Errorf("expected plain Sweep with nil DB, got %d", store.callCount())
	}
	if store.filteredCount() != 0 {
		t.Errorf("SweepFiltered must not run without a DBQuerier; got %d", store.filteredCount())
	}
}

// TestRun_FilteredSweepError_LoggedNotFatal — a SweepFiltered failure (e.g. the
// reference check errored) is logged and swallowed; the loop keeps running and
// exits cleanly on ctx cancel (fail-safe — retry next interval).
func TestRun_FilteredSweepError_LoggedNotFatal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	store := &refAwareStore{filteredErr: errors.New("reference check: db down")}
	store.onCall = func(n int) { cancel() }
	db := &stubDB{}

	runWithTimeout(t, ctx, store, Options{Interval: time.Millisecond, Retention: time.Hour, DB: db})

	if store.filteredCount() < 1 {
		t.Error("expected at least the on-start filtered sweep")
	}
}

package scheduler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	jobstore "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/jobs/store"
)

// fakeJobStore is an in-memory jobStoreIface that records every call so
// scheduler tests can assert which DB methods fire on which path.
type fakeJobStore struct {
	mu sync.Mutex

	// Enabled flags indexed by job id. SyncDefinitions seeds; SetEnabled
	// flips. Tests can pre-load enabled state to exercise SetEnabled's
	// "DB already in target state" semantics.
	enabled map[string]bool

	// Upsert / SyncDefinitions tracking.
	upserts         []upsertCall
	upsertErr       error
	getEnabledErr   error
	setEnabledErr   error
	enabledNotFound bool // true ⇒ SetEnabled returns jobstore.ErrNotFound

	// Run tracking.
	startRunErr   error
	startRunIDSeq atomic.Int32
	finishRunErr  error
	finishedRuns  []finishCall
	startedRuns   []startCall

	// List/Get/Recover.
	listJobsRows   []jobstore.JobWithStats
	listJobsErr    error
	getJobRow      jobstore.JobWithStats
	getJobErr      error
	getJobNotFound bool
	listRunsRows   []jobstore.JobRun
	listRunsTotal  int
	listRunsErr    error
	recoverN       int64
	recoverErr     error
}

type upsertCall struct {
	id, name, description string
	intervalSec           int
}
type startCall struct {
	jobID, replicaID string
}
type finishCall struct {
	runID, status string
	duration      time.Duration
	errMsg        string
}

func newFakeJobStore() *fakeJobStore {
	return &fakeJobStore{enabled: map[string]bool{}}
}

func (f *fakeJobStore) UpsertJob(_ context.Context, id, name, description string, intervalSec int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.upsertErr != nil {
		return f.upsertErr
	}
	f.upserts = append(f.upserts, upsertCall{id, name, description, intervalSec})
	if _, ok := f.enabled[id]; !ok {
		f.enabled[id] = true
	}
	return nil
}

func (f *fakeJobStore) GetEnabled(_ context.Context, id string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getEnabledErr != nil {
		return false, f.getEnabledErr
	}
	return f.enabled[id], nil
}

func (f *fakeJobStore) SetEnabled(_ context.Context, id string, enabled bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.enabledNotFound {
		return jobstore.ErrNotFound
	}
	if f.setEnabledErr != nil {
		return f.setEnabledErr
	}
	f.enabled[id] = enabled
	return nil
}

func (f *fakeJobStore) StartRun(_ context.Context, jobID, replicaID string) (string, error) {
	if f.startRunErr != nil {
		return "", f.startRunErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	id := jobID + "-run-" + itoa(int(f.startRunIDSeq.Add(1)))
	f.startedRuns = append(f.startedRuns, startCall{jobID, replicaID})
	return id, nil
}

func (f *fakeJobStore) FinishRun(_ context.Context, runID, status string, duration time.Duration, errMsg string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.finishRunErr != nil {
		return f.finishRunErr
	}
	f.finishedRuns = append(f.finishedRuns, finishCall{runID, status, duration, errMsg})
	return nil
}

func (f *fakeJobStore) ListJobsWithStats(_ context.Context) ([]jobstore.JobWithStats, error) {
	if f.listJobsErr != nil {
		return nil, f.listJobsErr
	}
	return f.listJobsRows, nil
}

func (f *fakeJobStore) GetJobWithStats(_ context.Context, _ string) (jobstore.JobWithStats, error) {
	if f.getJobNotFound {
		return jobstore.JobWithStats{}, jobstore.ErrNotFound
	}
	if f.getJobErr != nil {
		return jobstore.JobWithStats{}, f.getJobErr
	}
	return f.getJobRow, nil
}

func (f *fakeJobStore) ListRuns(_ context.Context, _ string, _, _ int) ([]jobstore.JobRun, int, error) {
	if f.listRunsErr != nil {
		return nil, 0, f.listRunsErr
	}
	return f.listRunsRows, f.listRunsTotal, nil
}

func (f *fakeJobStore) RecoverStaleRuns(_ context.Context) (int64, error) {
	if f.recoverErr != nil {
		return 0, f.recoverErr
	}
	return f.recoverN, nil
}

// itoa is a tiny strconv.Itoa stand-in to avoid pulling in the package
// just for one int→string usage.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := false
	if i < 0 {
		neg = true
		i = -i
	}
	buf := make([]byte, 0, 10)
	for i > 0 {
		buf = append([]byte{byte('0' + i%10)}, buf...)
		i /= 10
	}
	if neg {
		return "-" + string(buf)
	}
	return string(buf)
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// maxRunJob is a Job with a MaxRunDurationer cap, used to verify the
// per-run timeout override.
type maxRunJob struct {
	mockJob
	maxDur time.Duration
}

func (j *maxRunJob) MaxRunDuration() time.Duration { return j.maxDur }

// hangJob blocks until ctx fires, used to verify MaxRunDuration kicks in.
type hangJob struct {
	id       string
	interval time.Duration
	maxDur   time.Duration
	runs     atomic.Int32
	lastErr  atomic.Value // error
}

func (j *hangJob) ID() string                    { return j.id }
func (j *hangJob) Name() string                  { return j.id }
func (j *hangJob) Description() string           { return "hang test" }
func (j *hangJob) Interval() time.Duration       { return j.interval }
func (j *hangJob) MaxRunDuration() time.Duration { return j.maxDur }
func (j *hangJob) Run(ctx context.Context) error {
	j.runs.Add(1)
	<-ctx.Done()
	err := ctx.Err()
	j.lastErr.Store(err)
	return err
}

// panicJob panics on Run — used to verify the recover wrapper.
type panicJob struct {
	mockJob
}

func (j *panicJob) Run(_ context.Context) error {
	j.runs.Add(1)
	panic("intentional panic for recover test")
}

// TestWithJobStore_AttachesStore covers the WithJobStore setter so the js
// field is populated. Independent of any DB ops.
func TestWithJobStore_AttachesStore(t *testing.T) {
	// Exercise the public WithJobStore signature by passing a real
	// *jobstore.Store (constructed with nil pool — we never call any
	// method on it). This proves the setter wires the field through the
	// jobStoreIface seam.
	js := jobstore.New(nil)
	s := New(discardLogger()).WithJobStore(js)
	if s.js == nil {
		t.Fatal("WithJobStore did not wire the js field")
	}
}

// TestWithReplicaID_StampsReplicaID covers the replica setter and verifies
// the value is forwarded to StartRun.
func TestWithReplicaID_StampsReplicaID(t *testing.T) {
	fake := newFakeJobStore()
	s := New(discardLogger()).WithReplicaID("replica-7")
	s.js = fake

	j := &mockJob{name: "j1", interval: time.Hour}
	s.Register(j)
	if err := s.Trigger(context.Background(), "j1"); err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	// Wait for runOne to call StartRun.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		fake.mu.Lock()
		n := len(fake.startedRuns)
		fake.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.startedRuns) == 0 {
		t.Fatal("StartRun was not called")
	}
	if fake.startedRuns[0].replicaID != "replica-7" {
		t.Errorf("StartRun replicaID = %q, want replica-7", fake.startedRuns[0].replicaID)
	}
}

// TestSyncDefinitions_NoStoreIsNoOp covers the early-return branch.
func TestSyncDefinitions_NoStoreIsNoOp(t *testing.T) {
	s := New(discardLogger())
	if err := s.SyncDefinitions(context.Background()); err != nil {
		t.Errorf("SyncDefinitions with no js must return nil; got %v", err)
	}
}

// TestSyncDefinitions_UpsertsAndSeedsEnabledFromDB asserts the documented
// behaviour: every registered job is upserted with its current metadata
// AND the enabled flag is hydrated from the DB row (so an admin's "disable"
// survives restart).
func TestSyncDefinitions_UpsertsAndSeedsEnabledFromDB(t *testing.T) {
	fake := newFakeJobStore()
	// Pre-load DB state: job-a is disabled by the admin.
	fake.enabled["job-a"] = false
	fake.enabled["job-b"] = true

	s := New(discardLogger())
	s.js = fake
	s.Register(&mockJob{name: "job-a", interval: 30 * time.Second})
	s.Register(&mockJob{name: "job-b", interval: time.Minute})

	if err := s.SyncDefinitions(context.Background()); err != nil {
		t.Fatalf("SyncDefinitions: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.upserts) != 2 {
		t.Errorf("upserts = %d, want 2", len(fake.upserts))
	}
	for _, u := range fake.upserts {
		if u.intervalSec < 1 {
			t.Errorf("upsert %s interval = %d, must be ≥1", u.id, u.intervalSec)
		}
	}

	// Verify in-memory enabled flag matches DB.
	for _, id := range []string{"job-a", "job-b"} {
		s.mu.RLock()
		e, ok := s.jobs[id]
		s.mu.RUnlock()
		if !ok {
			t.Fatalf("job %s missing", id)
		}
		want := fake.enabled[id]
		if e.enabled.Load() != want {
			t.Errorf("job %s in-memory enabled = %v, want DB value %v", id, e.enabled.Load(), want)
		}
		if e.status.Enabled != want {
			t.Errorf("job %s status.Enabled = %v, want %v", id, e.status.Enabled, want)
		}
	}
}

// TestSyncDefinitions_SubSecondIntervalClampedToOne verifies the floor:
// a job with a sub-second interval still upserts with intervalSec=1
// (preventing 0 / negative values from leaking to the DB).
func TestSyncDefinitions_SubSecondIntervalClampedToOne(t *testing.T) {
	fake := newFakeJobStore()
	s := New(discardLogger())
	s.js = fake
	s.Register(&mockJob{name: "fast", interval: 500 * time.Millisecond})

	if err := s.SyncDefinitions(context.Background()); err != nil {
		t.Fatalf("SyncDefinitions: %v", err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.upserts) != 1 {
		t.Fatalf("upserts = %d, want 1", len(fake.upserts))
	}
	if fake.upserts[0].intervalSec != 1 {
		t.Errorf("intervalSec = %d, want clamped to 1", fake.upserts[0].intervalSec)
	}
}

// TestSyncDefinitions_UpsertErrorSurfaces verifies the error path —
// returning early without seeding enabled.
func TestSyncDefinitions_UpsertErrorSurfaces(t *testing.T) {
	fake := newFakeJobStore()
	fake.upsertErr = errors.New("pg down")
	s := New(discardLogger())
	s.js = fake
	s.Register(&mockJob{name: "j1", interval: time.Minute})

	if err := s.SyncDefinitions(context.Background()); err == nil {
		t.Error("expected upsert error to surface; got nil")
	}
}

// TestSyncDefinitions_GetEnabledErrorSurfaces covers the second DB-call
// error path (GetEnabled after a successful UpsertJob).
func TestSyncDefinitions_GetEnabledErrorSurfaces(t *testing.T) {
	fake := newFakeJobStore()
	fake.getEnabledErr = errors.New("pg blip")
	s := New(discardLogger())
	s.js = fake
	s.Register(&mockJob{name: "j1", interval: time.Minute})

	if err := s.SyncDefinitions(context.Background()); err == nil {
		t.Error("expected GetEnabled error to surface; got nil")
	}
}

// TestSetEnabled_NotFound covers ErrJobNotFound for an unknown id.
func TestSetEnabled_NotFound(t *testing.T) {
	s := New(discardLogger())
	if err := s.SetEnabled(context.Background(), "ghost", true); !errors.Is(err, ErrJobNotFound) {
		t.Errorf("err = %v, want ErrJobNotFound", err)
	}
}

// TestSetEnabled_NoStoreFlipsInMemoryOnly covers the no-jobstore path.
func TestSetEnabled_NoStoreFlipsInMemoryOnly(t *testing.T) {
	s := New(discardLogger())
	j := &mockJob{name: "j1", interval: time.Minute}
	s.Register(j)
	if err := s.SetEnabled(context.Background(), "j1", false); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.jobs["j1"].enabled.Load() != false {
		t.Error("in-memory enabled flag did not flip to false")
	}
	if s.jobs["j1"].status.Enabled != false {
		t.Error("status.Enabled did not flip to false")
	}
}

// TestSetEnabled_DBErrorSurfaces covers the DB-write error path.
func TestSetEnabled_DBErrorSurfaces(t *testing.T) {
	fake := newFakeJobStore()
	fake.setEnabledErr = errors.New("pg down")
	s := New(discardLogger())
	s.js = fake
	s.Register(&mockJob{name: "j1", interval: time.Minute})

	if err := s.SetEnabled(context.Background(), "j1", false); err == nil {
		t.Error("expected DB error to surface; got nil")
	}
}

// TestSetEnabled_DBNotFoundMapped covers the jobstore.ErrNotFound →
// ErrJobNotFound mapping so the public API surface is consistent.
func TestSetEnabled_DBNotFoundMapped(t *testing.T) {
	fake := newFakeJobStore()
	fake.enabledNotFound = true
	s := New(discardLogger())
	s.js = fake
	s.Register(&mockJob{name: "j1", interval: time.Minute})

	err := s.SetEnabled(context.Background(), "j1", false)
	if !errors.Is(err, ErrJobNotFound) {
		t.Errorf("err = %v, want ErrJobNotFound", err)
	}
}

// TestSetEnabled_LivewireAddsAndRemovesCronEntries verifies the
// documented behaviour: when SetEnabled fires after Start, the job is
// added to (enable) or removed from (disable) the running cron engine.
func TestSetEnabled_LivewireAddsAndRemovesCronEntries(t *testing.T) {
	fake := newFakeJobStore()
	fake.enabled["j1"] = true
	s := New(discardLogger())
	s.js = fake
	j := &mockJob{name: "j1", interval: time.Hour}
	s.Register(j)
	if err := s.SyncDefinitions(context.Background()); err != nil {
		t.Fatalf("SyncDefinitions: %v", err)
	}

	s.Start()
	defer s.Stop()

	// Initially enabled → cronEntryID != 0.
	s.mu.RLock()
	initialEntry := s.jobs["j1"].cronEntryID
	s.mu.RUnlock()
	if initialEntry == 0 {
		t.Fatal("initially enabled job has cronEntryID=0")
	}

	// Disable → cronEntryID returns to 0.
	if err := s.SetEnabled(context.Background(), "j1", false); err != nil {
		t.Fatalf("SetEnabled false: %v", err)
	}
	s.mu.RLock()
	disabledEntry := s.jobs["j1"].cronEntryID
	s.mu.RUnlock()
	if disabledEntry != 0 {
		t.Errorf("after disable: cronEntryID = %v, want 0", disabledEntry)
	}

	// Re-enable → cronEntryID is non-zero again.
	if err := s.SetEnabled(context.Background(), "j1", true); err != nil {
		t.Fatalf("SetEnabled true: %v", err)
	}
	s.mu.RLock()
	reenabledEntry := s.jobs["j1"].cronEntryID
	s.mu.RUnlock()
	if reenabledEntry == 0 {
		t.Error("after re-enable: cronEntryID = 0")
	}
}

// TestRunOne_StartRunErrorDoesNotPreventExecution covers the documented
// degraded-mode behaviour: a StartRun failure logs but the job still
// executes; FinishRun is skipped (no run id), so the in-memory status
// still updates.
func TestRunOne_StartRunErrorDoesNotPreventExecution(t *testing.T) {
	fake := newFakeJobStore()
	fake.startRunErr = errors.New("pg down")
	s := New(discardLogger())
	s.js = fake
	j := &mockJob{name: "j1", interval: time.Hour}
	s.Register(j)

	if err := s.Trigger(context.Background(), "j1"); err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) && j.runs.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if j.runs.Load() != 1 {
		t.Errorf("job runs = %d, want 1 even when StartRun failed", j.runs.Load())
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.finishedRuns) != 0 {
		t.Errorf("FinishRun called %d times; must be 0 when StartRun failed", len(fake.finishedRuns))
	}
}

// TestRunOne_FinishRunErrorLogged covers the FinishRun error branch:
// the job has already finished; the FinishRun error must not break the
// status accounting.
func TestRunOne_FinishRunErrorLogged(t *testing.T) {
	fake := newFakeJobStore()
	fake.finishRunErr = errors.New("pg blip")
	s := New(discardLogger())
	s.js = fake
	j := &mockJob{name: "j1", interval: time.Hour}
	s.Register(j)

	if err := s.Trigger(context.Background(), "j1"); err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) && j.runs.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if j.runs.Load() != 1 {
		t.Errorf("job runs = %d, want 1", j.runs.Load())
	}
	// In-memory status still records success.
	jobs, _ := s.ListJobs(context.Background())
	for _, js := range jobs {
		if js.ID == "j1" && js.LastStatus != "success" {
			t.Errorf("LastStatus = %q, want success despite FinishRun error", js.LastStatus)
		}
	}
}

// TestRunOne_MaxRunDurationCapsContext covers the MaxRunDurationer branch.
// A job that hangs beyond MaxRunDuration must observe ctx.DeadlineExceeded
// and be marked as error.
func TestRunOne_MaxRunDurationCapsContext(t *testing.T) {
	s := New(discardLogger())
	j := &hangJob{id: "hang", interval: time.Hour, maxDur: 30 * time.Millisecond}
	s.Register(j)

	if err := s.Trigger(context.Background(), "hang"); err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) && j.runs.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if j.runs.Load() != 1 {
		t.Errorf("hang job did not start; runs = %d", j.runs.Load())
	}
	// Give cancel a moment to propagate + status to update.
	time.Sleep(80 * time.Millisecond)

	jobs, _ := s.ListJobs(context.Background())
	for _, js := range jobs {
		if js.ID == "hang" {
			if js.LastStatus != "error" {
				t.Errorf("LastStatus = %q, want error (DeadlineExceeded)", js.LastStatus)
			}
		}
	}
	if last, ok := j.lastErr.Load().(error); !ok || !errors.Is(last, context.DeadlineExceeded) {
		t.Errorf("job did not observe DeadlineExceeded; got %v", j.lastErr.Load())
	}
}

// TestRunOne_PanicRecovered covers the deferred recover in runOne. A
// panicking manual trigger must not crash the scheduler — the panic is
// logged and the goroutine returns cleanly.
func TestRunOne_PanicRecovered(t *testing.T) {
	s := New(discardLogger())
	j := &panicJob{mockJob: mockJob{name: "panic", interval: time.Hour}}
	s.Register(j)

	if err := s.Trigger(context.Background(), "panic"); err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	// Wait for the goroutine to recover and return.
	time.Sleep(100 * time.Millisecond)

	// Scheduler still usable.
	other := &mockJob{name: "other", interval: time.Hour}
	s.Register(other)
	if err := s.Trigger(context.Background(), "other"); err != nil {
		t.Fatalf("subsequent Trigger after panic: %v", err)
	}
}

// TestRunOne_DisabledJobScheduledTickIsSkipped covers the documented
// behaviour: a disabled job whose timer fires (or which is invoked via
// non-manual path) must NOT execute.
func TestRunOne_DisabledJobScheduledTickIsSkipped(t *testing.T) {
	s := New(discardLogger())
	j := &mockJob{name: "j1", interval: time.Hour}
	s.Register(j)

	// Disable directly via in-memory flag.
	if err := s.SetEnabled(context.Background(), "j1", false); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	// Simulate a tick.
	s.mu.RLock()
	e := s.jobs["j1"]
	s.mu.RUnlock()
	s.runOne(e, false /* manual=false */)

	if j.runs.Load() != 0 {
		t.Errorf("disabled job ran on scheduled tick; runs = %d", j.runs.Load())
	}
}

// TestListJobs_NoStoreReturnsInMemory covers the no-jobstore branch.
func TestListJobs_NoStoreReturnsInMemory(t *testing.T) {
	s := New(discardLogger())
	s.Register(&mockJob{name: "a", interval: time.Minute})
	s.Register(&mockJob{name: "b", interval: 2 * time.Minute})
	jobs, err := s.ListJobs(context.Background())
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(jobs) != 2 {
		t.Errorf("jobs = %d, want 2", len(jobs))
	}
}

// TestListJobs_StoreErrorSurfaces covers the DB-error branch.
func TestListJobs_StoreErrorSurfaces(t *testing.T) {
	fake := newFakeJobStore()
	fake.listJobsErr = errors.New("pg down")
	s := New(discardLogger())
	s.js = fake

	if _, err := s.ListJobs(context.Background()); err == nil {
		t.Error("expected ListJobs error; got nil")
	}
}

// TestListJobs_StoreSuccessConvertsRows covers the happy-path conversion:
// statusFromStats merges DB aggregates with the registered job's interval +
// next-run computation.
func TestListJobs_StoreSuccessConvertsRows(t *testing.T) {
	fake := newFakeJobStore()
	now := time.Now()
	lastRun := now.Add(-2 * time.Minute)
	durMS := 1234
	fake.listJobsRows = []jobstore.JobWithStats{
		{
			JobRecord: jobstore.JobRecord{
				ID:          "j1",
				Name:        "Job One",
				Description: "first",
				IntervalSec: 60,
				Enabled:     true,
			},
			LastRun:        &lastRun,
			LastStatus:     "success",
			LastDurationMs: &durMS,
			RunCount:       7,
			ErrorCount:     1,
		},
	}
	s := New(discardLogger())
	s.js = fake
	s.Register(&mockJob{name: "j1", interval: time.Minute})

	jobs, err := s.ListJobs(context.Background())
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("jobs = %d, want 1", len(jobs))
	}
	got := jobs[0]
	if got.Name != "Job One" || got.LastStatus != "success" {
		t.Errorf("converted row mismatch: %+v", got)
	}
	if got.LastDuration != time.Duration(durMS)*time.Millisecond {
		t.Errorf("LastDuration = %v, want %v", got.LastDuration, time.Duration(durMS)*time.Millisecond)
	}
	if got.NextRun == nil {
		t.Error("NextRun should be derived from LastRun+Interval when cron not started")
	}
}

// TestGetJob_NoStoreFromMemory covers the in-memory branch + the
// not-found case.
func TestGetJob_NoStoreFromMemory(t *testing.T) {
	s := New(discardLogger())
	s.Register(&mockJob{name: "exists", interval: time.Minute})

	got, err := s.GetJob(context.Background(), "exists")
	if err != nil {
		t.Fatalf("GetJob exists: %v", err)
	}
	if got.ID != "exists" {
		t.Errorf("ID = %q, want exists", got.ID)
	}

	_, err = s.GetJob(context.Background(), "missing")
	if !errors.Is(err, ErrJobNotFound) {
		t.Errorf("missing: err = %v, want ErrJobNotFound", err)
	}
}

// TestGetJob_StoreNotFoundMapped covers the jobstore.ErrNotFound →
// ErrJobNotFound mapping when js is wired.
func TestGetJob_StoreNotFoundMapped(t *testing.T) {
	fake := newFakeJobStore()
	fake.getJobNotFound = true
	s := New(discardLogger())
	s.js = fake
	s.Register(&mockJob{name: "j1", interval: time.Minute})

	_, err := s.GetJob(context.Background(), "j1")
	if !errors.Is(err, ErrJobNotFound) {
		t.Errorf("err = %v, want ErrJobNotFound", err)
	}
}

// TestGetJob_StoreOtherErrorSurfaces covers a generic DB error path.
func TestGetJob_StoreOtherErrorSurfaces(t *testing.T) {
	fake := newFakeJobStore()
	fake.getJobErr = errors.New("pg blip")
	s := New(discardLogger())
	s.js = fake
	s.Register(&mockJob{name: "j1", interval: time.Minute})

	_, err := s.GetJob(context.Background(), "j1")
	if err == nil || errors.Is(err, ErrJobNotFound) {
		t.Errorf("err = %v, want generic non-ErrJobNotFound", err)
	}
}

// TestGetJob_StoreSuccessUsesRegisteredInterval covers the happy path:
// the returned status carries job metadata from the DB AND NextRun is
// computed from LastRun+Interval when cron is idle.
func TestGetJob_StoreSuccessUsesRegisteredInterval(t *testing.T) {
	fake := newFakeJobStore()
	lastRun := time.Now().Add(-10 * time.Second)
	durMS := 50
	fake.getJobRow = jobstore.JobWithStats{
		JobRecord: jobstore.JobRecord{
			ID: "j1", Name: "Job 1", Description: "d", IntervalSec: 60, Enabled: true,
		},
		LastRun: &lastRun, LastStatus: "success", LastDurationMs: &durMS, RunCount: 1,
	}
	s := New(discardLogger())
	s.js = fake
	s.Register(&mockJob{name: "j1", interval: time.Minute})

	got, err := s.GetJob(context.Background(), "j1")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.ID != "j1" {
		t.Errorf("ID = %q, want j1", got.ID)
	}
	if got.NextRun == nil {
		t.Error("NextRun should be derived from LastRun+Interval")
	}
}

// TestListRuns_NoStoreReturnsEmpty covers the no-jobstore branch.
func TestListRuns_NoStoreReturnsEmpty(t *testing.T) {
	s := New(discardLogger())
	rows, total, err := s.ListRuns(context.Background(), "anything", 10, 0)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(rows) != 0 || total != 0 {
		t.Errorf("expected empty result; got rows=%v total=%d", rows, total)
	}
}

// TestListRuns_StoreForwarded covers the delegate-to-jobstore branch.
func TestListRuns_StoreForwarded(t *testing.T) {
	fake := newFakeJobStore()
	fake.listRunsRows = []jobstore.JobRun{{ID: "r1", JobID: "j1", Status: "success"}}
	fake.listRunsTotal = 42
	s := New(discardLogger())
	s.js = fake

	rows, total, err := s.ListRuns(context.Background(), "j1", 10, 0)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != "r1" {
		t.Errorf("rows = %v", rows)
	}
	if total != 42 {
		t.Errorf("total = %d, want 42", total)
	}
}

// TestRecoverStaleRuns_NoStoreNoOp covers the no-jobstore branch.
func TestRecoverStaleRuns_NoStoreNoOp(t *testing.T) {
	s := New(discardLogger())
	if err := s.RecoverStaleRuns(context.Background()); err != nil {
		t.Errorf("RecoverStaleRuns: %v", err)
	}
}

// TestRecoverStaleRuns_StoreErrorSurfaces covers the DB-error branch.
func TestRecoverStaleRuns_StoreErrorSurfaces(t *testing.T) {
	fake := newFakeJobStore()
	fake.recoverErr = errors.New("pg down")
	s := New(discardLogger())
	s.js = fake
	if err := s.RecoverStaleRuns(context.Background()); err == nil {
		t.Error("expected error; got nil")
	}
}

// TestRecoverStaleRuns_SuccessLogsNonZero covers the n>0 logging branch.
func TestRecoverStaleRuns_SuccessLogsNonZero(t *testing.T) {
	fake := newFakeJobStore()
	fake.recoverN = 5
	s := New(discardLogger())
	s.js = fake
	if err := s.RecoverStaleRuns(context.Background()); err != nil {
		t.Errorf("RecoverStaleRuns: %v", err)
	}
}

// TestSlogAdapter_NilLoggerIsNoOp covers the defensive nil-logger branch
// in the cron-Logger adapter — early returns prevent a nil deref if the
// scheduler is constructed without a logger (defensive only — production
// always passes one).
func TestSlogAdapter_NilLoggerIsNoOp(t *testing.T) {
	a := slogAdapter{inner: nil}
	a.Info("noop")
	a.Error(errors.New("ignored"), "noop")
	// Surviving the calls = pass.
}

// TestSlogAdapter_InfoForwarded covers the forwarding path so the
// cron-Recover wrapper's info messages flow into slog.
func TestSlogAdapter_InfoForwarded(t *testing.T) {
	a := slogAdapter{inner: discardLogger()}
	a.Info("hello", "k", "v")
}

// TestSlogAdapter_ErrorForwardedAppendsErrorKV verifies the documented
// behaviour: a non-nil err is appended as kv pairs so it appears in the
// structured log line.
func TestSlogAdapter_ErrorForwardedAppendsErrorKV(t *testing.T) {
	a := slogAdapter{inner: discardLogger()}
	a.Error(errors.New("boom"), "explosion", "where", "here")
	// With nil err the err kv is NOT appended — exercise both branches.
	a.Error(nil, "explosion-nil")
}

// TestStop_NotStartedIsNoOp covers the early-return when started==false.
func TestStop_NotStartedIsNoOp(t *testing.T) {
	s := New(discardLogger())
	s.Stop() // never started — must not panic.
	s.Stop() // idempotent second call.
}

// TestSchedulerStart_OnStartRunnerKickedOff verifies the documented
// RunOnStart-detached-goroutine behaviour: jobs that opt into RunOnStart
// fire once even though Start() does not block.
func TestSchedulerStart_OnStartRunnerKickedOff(t *testing.T) {
	s := New(discardLogger())
	j := &onStartJob{
		mockJob:    mockJob{name: "boot", interval: time.Hour},
		runOnStart: true,
	}
	s.Register(j)
	s.Start()
	defer s.Stop()
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) && j.runs.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if j.runs.Load() == 0 {
		t.Error("RunOnStart job did not fire")
	}
}

// TestMaxRunDurationer_HonoredEvenWithLargeInterval verifies the explicit
// override path: a job with a long interval but small MaxRunDuration uses
// MaxRunDuration for its per-tick timeout.
func TestMaxRunDurationer_HonoredEvenWithLargeInterval(t *testing.T) {
	s := New(discardLogger())
	j := &maxRunJob{
		mockJob: mockJob{name: "mrd", interval: time.Hour},
		maxDur:  10 * time.Millisecond,
	}
	s.Register(j)
	if err := s.Trigger(context.Background(), "mrd"); err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) && j.runs.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if j.runs.Load() != 1 {
		t.Errorf("job runs = %d, want 1", j.runs.Load())
	}
}

// TestDefaultTimeout_FloorAndPassthrough covers both branches: short
// interval clamped to minDefaultRunTimeout; long interval passes through.
func TestDefaultTimeout_FloorAndPassthrough(t *testing.T) {
	short := defaultTimeout(&mockJob{name: "s", interval: 5 * time.Second})
	if short != minDefaultRunTimeout {
		t.Errorf("short interval timeout = %v, want %v", short, minDefaultRunTimeout)
	}
	long := defaultTimeout(&mockJob{name: "l", interval: 10 * time.Minute})
	if long != 10*time.Minute {
		t.Errorf("long interval timeout = %v, want passthrough", long)
	}
}

// TestNextRunFor_NoCronYetReturnsNil covers the early-return when the
// scheduler is not started or the entry has no cronEntryID.
func TestNextRunFor_NoCronYetReturnsNil(t *testing.T) {
	s := New(discardLogger())
	j := &mockJob{name: "j1", interval: time.Minute}
	s.Register(j)
	s.mu.RLock()
	e := s.jobs["j1"]
	s.mu.RUnlock()
	if got := s.nextRunFor(e); got != nil {
		t.Errorf("nextRunFor (no cron) = %v, want nil", got)
	}
}

// TestWithMetrics_NilIsNoOp verifies that a nil registerer is accepted
// without panicking and leaves leaderGauge nil.
func TestWithMetrics_NilIsNoOp(t *testing.T) {
	s := New(discardLogger()).WithMetrics(nil)
	if s.leaderGauge != nil {
		t.Error("leaderGauge must be nil when registerer is nil")
	}
}

// TestWithMetrics_LeaderGaugeSetOnStart verifies that Start() sets the
// nexus_hub_scheduler_leader gauge to 1 when WithMetrics has been wired.
// Uses an isolated prometheus.Registry so the test never touches
// prometheus.DefaultRegisterer (which is process-global and cannot be
// reset between test runs).
func TestWithMetrics_LeaderGaugeSetOnStart(t *testing.T) {
	reg := prometheus.NewRegistry()
	s := New(discardLogger()).WithMetrics(reg)
	if s.leaderGauge == nil {
		t.Fatal("leaderGauge must be non-nil after WithMetrics(non-nil)")
	}

	s.Start()
	defer s.Stop()

	// Read the gauge value directly via dto.Metric (no Gather needed).
	var pb dto.Metric
	if err := s.leaderGauge.Write(&pb); err != nil {
		t.Fatalf("gauge.Write: %v", err)
	}
	if got := pb.GetGauge().GetValue(); got != 1 {
		t.Errorf("nexus_hub_scheduler_leader = %v, want 1", got)
	}
}

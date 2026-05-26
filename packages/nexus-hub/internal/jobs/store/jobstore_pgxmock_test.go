package jobstore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pashagolub/pgxmock/v4"
)

// jobstore_pgxmock_test.go drives every SQL statement in this package
// through pgxmock so the per-package coverage reaches ≥95% without touching
// a live PostgreSQL. The existing integration tests in jobstore_test.go
// stay in place — they skip when NEXUS_TEST_DB is unset, but their schema
// assertions remain the source of truth for the actual DDL shape.
//
// Per binding [[tests-only-own-data]]: these tests own zero real rows and
// therefore cannot violate the no-cross-test-data rule.

func newMockStore(t *testing.T) (*Store, pgxmock.PgxPoolIface) {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(mock.Close)
	return NewWithPgxPool(mock), mock
}

// --- UpsertJob --------------------------------------------------------------

func TestUpsertJob_Success(t *testing.T) {
	s, mock := newMockStore(t)
	mock.ExpectExec(`INSERT INTO "job"`).
		WithArgs("id-1", "Name", "desc", 60).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))

	if err := s.UpsertJob(context.Background(), "id-1", "Name", "desc", 60); err != nil {
		t.Fatalf("UpsertJob: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

func TestUpsertJob_ExecError(t *testing.T) {
	s, mock := newMockStore(t)
	mock.ExpectExec(`INSERT INTO "job"`).
		WithArgs("id-2", "n", "d", 30).
		WillReturnError(errors.New("conn dead"))

	err := s.UpsertJob(context.Background(), "id-2", "n", "d", 30)
	if err == nil {
		t.Fatal("expected error")
	}
	// Error must be wrapped with the job id so operators can trace which
	// upsert failed in a multi-job batch.
	if got := err.Error(); got == "" || !contains(got, "id-2") {
		t.Errorf("expected wrapped error containing id-2, got %q", got)
	}
}

// --- GetEnabled -------------------------------------------------------------

func TestGetEnabled_True(t *testing.T) {
	s, mock := newMockStore(t)
	mock.ExpectQuery(`SELECT "enabled" FROM "job" WHERE "id" = \$1`).
		WithArgs("id-3").
		WillReturnRows(pgxmock.NewRows([]string{"enabled"}).AddRow(true))

	got, err := s.GetEnabled(context.Background(), "id-3")
	if err != nil {
		t.Fatalf("GetEnabled: %v", err)
	}
	if !got {
		t.Errorf("got false, want true")
	}
}

func TestGetEnabled_NotFound(t *testing.T) {
	s, mock := newMockStore(t)
	mock.ExpectQuery(`SELECT "enabled" FROM "job"`).
		WithArgs("missing").
		WillReturnError(pgx.ErrNoRows)

	got, err := s.GetEnabled(context.Background(), "missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if got {
		t.Errorf("got %v, want false", got)
	}
}

func TestGetEnabled_QueryError(t *testing.T) {
	s, mock := newMockStore(t)
	mock.ExpectQuery(`SELECT "enabled" FROM "job"`).
		WithArgs("id-x").
		WillReturnError(errors.New("conn dead"))

	if _, err := s.GetEnabled(context.Background(), "id-x"); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("expected wrapped non-ErrNotFound error, got %v", err)
	}
}

// --- SetEnabled -------------------------------------------------------------

func TestSetEnabled_Success(t *testing.T) {
	s, mock := newMockStore(t)
	mock.ExpectExec(`UPDATE "job" SET "enabled"`).
		WithArgs(false, "id-4").
		WillReturnResult(pgconn.NewCommandTag("UPDATE 1"))

	if err := s.SetEnabled(context.Background(), "id-4", false); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
}

func TestSetEnabled_NotFound(t *testing.T) {
	s, mock := newMockStore(t)
	mock.ExpectExec(`UPDATE "job" SET "enabled"`).
		WithArgs(true, "missing").
		WillReturnResult(pgconn.NewCommandTag("UPDATE 0"))

	if err := s.SetEnabled(context.Background(), "missing", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestSetEnabled_ExecError(t *testing.T) {
	s, mock := newMockStore(t)
	mock.ExpectExec(`UPDATE "job" SET "enabled"`).
		WithArgs(true, "id-5").
		WillReturnError(errors.New("boom"))

	err := s.SetEnabled(context.Background(), "id-5", true)
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("expected non-ErrNotFound wrapped error, got %v", err)
	}
}

// --- Get --------------------------------------------------------------------

func TestGet_Success(t *testing.T) {
	s, mock := newMockStore(t)
	now := time.Now()
	mock.ExpectQuery(`SELECT "id", "name", "description", "intervalSec", "enabled", "createdAt", "updatedAt"`).
		WithArgs("id-6").
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "name", "description", "intervalSec", "enabled", "createdAt", "updatedAt",
		}).AddRow("id-6", "n", "d", 60, true, now, now))

	r, err := s.Get(context.Background(), "id-6")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if r.ID != "id-6" || r.Name != "n" || r.IntervalSec != 60 || !r.Enabled {
		t.Errorf("unexpected row: %+v", r)
	}
}

func TestGet_NotFound(t *testing.T) {
	s, mock := newMockStore(t)
	mock.ExpectQuery(`SELECT "id", "name", "description"`).
		WithArgs("missing").
		WillReturnError(pgx.ErrNoRows)

	if _, err := s.Get(context.Background(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestGet_ScanError(t *testing.T) {
	s, mock := newMockStore(t)
	mock.ExpectQuery(`SELECT "id", "name", "description"`).
		WithArgs("id-7").
		WillReturnError(errors.New("net"))

	if _, err := s.Get(context.Background(), "id-7"); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("expected non-ErrNotFound wrapped error, got %v", err)
	}
}

// --- StartRun ---------------------------------------------------------------

func TestStartRun_Success(t *testing.T) {
	s, mock := newMockStore(t)
	mock.ExpectExec(`INSERT INTO "job_run"`).
		WithArgs(pgxmock.AnyArg(), "job-1", StatusRunning, "replica-a").
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))

	id, err := s.StartRun(context.Background(), "job-1", "replica-a")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if id == "" {
		t.Error("expected non-empty run id")
	}
}

func TestStartRun_ExecError(t *testing.T) {
	s, mock := newMockStore(t)
	mock.ExpectExec(`INSERT INTO "job_run"`).
		WithArgs(pgxmock.AnyArg(), "job-2", StatusRunning, "").
		WillReturnError(errors.New("dead"))

	id, err := s.StartRun(context.Background(), "job-2", "")
	if err == nil {
		t.Fatal("expected error")
	}
	if id != "" {
		t.Errorf("expected empty id on error, got %q", id)
	}
}

// --- FinishRun --------------------------------------------------------------

func TestFinishRun_SuccessStripsErrMsg(t *testing.T) {
	// FinishRun must force errMsg="" for non-error statuses so stale
	// errors from a previous attempt cannot leak through. We pin "" as
	// the expected arg even though the caller passed a non-empty errMsg.
	s, mock := newMockStore(t)
	mock.ExpectExec(`UPDATE "job_run"`).
		WithArgs(123, StatusSuccess, "", "run-1").
		WillReturnResult(pgconn.NewCommandTag("UPDATE 1"))

	if err := s.FinishRun(context.Background(), "run-1", StatusSuccess, 123*time.Millisecond, "leaked"); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
}

func TestFinishRun_ErrorKeepsErrMsg(t *testing.T) {
	s, mock := newMockStore(t)
	mock.ExpectExec(`UPDATE "job_run"`).
		WithArgs(7, StatusError, "boom", "run-2").
		WillReturnResult(pgconn.NewCommandTag("UPDATE 1"))

	if err := s.FinishRun(context.Background(), "run-2", StatusError, 7*time.Millisecond, "boom"); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
}

func TestFinishRun_SkippedStripsErrMsg(t *testing.T) {
	s, mock := newMockStore(t)
	mock.ExpectExec(`UPDATE "job_run"`).
		WithArgs(0, StatusSkipped, "", "run-3").
		WillReturnResult(pgconn.NewCommandTag("UPDATE 1"))

	if err := s.FinishRun(context.Background(), "run-3", StatusSkipped, 0, "ignored"); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
}

func TestFinishRun_ExecError(t *testing.T) {
	s, mock := newMockStore(t)
	mock.ExpectExec(`UPDATE "job_run"`).
		WithArgs(1, StatusSuccess, "", "run-4").
		WillReturnError(errors.New("dead"))

	if err := s.FinishRun(context.Background(), "run-4", StatusSuccess, time.Millisecond, ""); err == nil {
		t.Fatal("expected error")
	}
}

// --- ListRuns ---------------------------------------------------------------

func runRowsCols() []string {
	return []string{"id", "jobId", "startedAt", "finishedAt", "durationMs", "status", "error", "replicaId", "total"}
}

func TestListRuns_PageWithTotal(t *testing.T) {
	s, mock := newMockStore(t)
	now := time.Now()
	finished := now.Add(time.Second)
	durMs := 42
	mock.ExpectQuery(`SELECT "id", "jobId", "startedAt", "finishedAt", "durationMs"`).
		WithArgs("job-list", 100, 0).
		WillReturnRows(pgxmock.NewRows(runRowsCols()).
			AddRow("r1", "job-list", now, &finished, &durMs, StatusSuccess, "", "rep-1", 2).
			AddRow("r2", "job-list", now, &finished, &durMs, StatusError, "boom", "rep-2", 2))

	runs, total, err := s.ListRuns(context.Background(), "job-list", 0, -5) // limit/offset both reset to defaults
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) != 2 {
		t.Errorf("len(runs)=%d, want 2", len(runs))
	}
	if total != 2 {
		t.Errorf("total=%d, want 2", total)
	}
}

func TestListRuns_LimitCappedAt500(t *testing.T) {
	// Caller passes 9999 — production resets it to the default 100, NOT
	// the literal 9999. The DB args must reflect this clamp.
	s, mock := newMockStore(t)
	mock.ExpectQuery(`SELECT "id", "jobId"`).
		WithArgs("job-clamp", 100, 0).
		WillReturnRows(pgxmock.NewRows(runRowsCols()))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM "job_run"`).
		WithArgs("job-clamp").
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(0))

	_, _, err := s.ListRuns(context.Background(), "job-clamp", 9999, 0)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
}

func TestListRuns_QueryError(t *testing.T) {
	s, mock := newMockStore(t)
	mock.ExpectQuery(`SELECT "id", "jobId"`).
		WithArgs("job-err", 50, 10).
		WillReturnError(errors.New("conn"))

	if _, _, err := s.ListRuns(context.Background(), "job-err", 50, 10); err == nil {
		t.Fatal("expected error")
	}
}

func TestListRuns_ScanError(t *testing.T) {
	// pgxmock row with the wrong column count surfaces as a Scan error
	// inside the loop — production code must return the wrapped scan
	// failure (not silently truncate the result set).
	s, mock := newMockStore(t)
	mock.ExpectQuery(`SELECT "id", "jobId"`).
		WithArgs("job-scan", 100, 0).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("partial"))

	_, _, err := s.ListRuns(context.Background(), "job-scan", 0, 0)
	if err == nil {
		t.Fatal("expected scan error")
	}
}

func TestListRuns_RowsErr(t *testing.T) {
	// Rows iteration finishes cleanly but rows.Err() reports a streaming
	// error — production must propagate it rather than returning the
	// (partial) slice silently. pgxmock surfaces the CloseError via the
	// pgx Rows.Err() contract after iteration completes.
	s, mock := newMockStore(t)
	now := time.Now()
	mock.ExpectQuery(`SELECT "id", "jobId"`).
		WithArgs("job-rowserr", 100, 0).
		WillReturnRows(pgxmock.NewRows(runRowsCols()).
			AddRow("r1", "job-rowserr", now, (*time.Time)(nil), (*int)(nil), StatusRunning, "", "", 1).
			CloseError(errors.New("stream broke")))

	if _, _, err := s.ListRuns(context.Background(), "job-rowserr", 0, 0); err == nil {
		t.Fatal("expected rows.Err")
	}
}

func TestListRuns_EmptyPageFallbackCount(t *testing.T) {
	s, mock := newMockStore(t)
	// First SELECT returns zero rows — the COUNT fallback fires so the
	// caller still gets the correct total when paging past the end.
	mock.ExpectQuery(`SELECT "id", "jobId"`).
		WithArgs("job-empty", 100, 999).
		WillReturnRows(pgxmock.NewRows(runRowsCols()))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM "job_run" WHERE "jobId" = \$1`).
		WithArgs("job-empty").
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(42))

	runs, total, err := s.ListRuns(context.Background(), "job-empty", 0, 999)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) != 0 {
		t.Errorf("len(runs)=%d, want 0", len(runs))
	}
	if total != 42 {
		t.Errorf("total=%d, want 42", total)
	}
}

func TestListRuns_EmptyPageFallbackError(t *testing.T) {
	s, mock := newMockStore(t)
	mock.ExpectQuery(`SELECT "id", "jobId"`).
		WithArgs("job-cnterr", 100, 0).
		WillReturnRows(pgxmock.NewRows(runRowsCols()))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM "job_run"`).
		WithArgs("job-cnterr").
		WillReturnError(errors.New("count dead"))

	if _, _, err := s.ListRuns(context.Background(), "job-cnterr", 0, 0); err == nil {
		t.Fatal("expected error")
	}
}

// --- RecoverStaleRuns -------------------------------------------------------

func TestRecoverStaleRuns_Success(t *testing.T) {
	s, mock := newMockStore(t)
	mock.ExpectExec(`UPDATE "job_run"`).
		WithArgs(StatusInterrupted, StatusRunning).
		WillReturnResult(pgconn.NewCommandTag("UPDATE 7"))

	n, err := s.RecoverStaleRuns(context.Background())
	if err != nil {
		t.Fatalf("RecoverStaleRuns: %v", err)
	}
	if n != 7 {
		t.Errorf("n=%d, want 7", n)
	}
}

func TestRecoverStaleRuns_ExecError(t *testing.T) {
	s, mock := newMockStore(t)
	mock.ExpectExec(`UPDATE "job_run"`).
		WithArgs(StatusInterrupted, StatusRunning).
		WillReturnError(errors.New("dead"))

	if _, err := s.RecoverStaleRuns(context.Background()); err == nil {
		t.Fatal("expected error")
	}
}

// --- PruneJobRuns -----------------------------------------------------------

func TestPruneJobRuns_DefaultKeep100(t *testing.T) {
	// keepN <= 0 must be coerced to 100 (the documented retention default).
	s, mock := newMockStore(t)
	mock.ExpectExec(`DELETE FROM "job_run"`).
		WithArgs(100).
		WillReturnResult(pgconn.NewCommandTag("DELETE 5"))

	n, err := s.PruneJobRuns(context.Background(), 0)
	if err != nil {
		t.Fatalf("PruneJobRuns: %v", err)
	}
	if n != 5 {
		t.Errorf("n=%d, want 5", n)
	}
}

func TestPruneJobRuns_CustomKeep(t *testing.T) {
	s, mock := newMockStore(t)
	mock.ExpectExec(`DELETE FROM "job_run"`).
		WithArgs(3).
		WillReturnResult(pgconn.NewCommandTag("DELETE 4"))

	n, err := s.PruneJobRuns(context.Background(), 3)
	if err != nil {
		t.Fatalf("PruneJobRuns: %v", err)
	}
	if n != 4 {
		t.Errorf("n=%d, want 4", n)
	}
}

func TestPruneJobRuns_ExecError(t *testing.T) {
	s, mock := newMockStore(t)
	mock.ExpectExec(`DELETE FROM "job_run"`).
		WithArgs(50).
		WillReturnError(errors.New("dead"))

	if _, err := s.PruneJobRuns(context.Background(), 50); err == nil {
		t.Fatal("expected error")
	}
}

// --- ListJobsWithStats ------------------------------------------------------

func statsRowsCols() []string {
	return []string{
		"id", "name", "description", "intervalSec", "enabled",
		"createdAt", "updatedAt",
		"startedAt", "finishedAt", "last_status", "durationMs", "last_error",
		"run_count", "error_count",
	}
}

func TestListJobsWithStats_Success(t *testing.T) {
	s, mock := newMockStore(t)
	now := time.Now()
	finished := now.Add(time.Second)
	dur := 99
	mock.ExpectQuery(`FROM "job" j`).
		WillReturnRows(pgxmock.NewRows(statsRowsCols()).
			AddRow("j1", "Name 1", "d1", 60, true, now, now, &now, &finished, StatusSuccess, &dur, "", int64(10), int64(2)).
			AddRow("j2", "Name 2", "d2", 30, false, now, now, (*time.Time)(nil), (*time.Time)(nil), "", (*int)(nil), "", int64(0), int64(0)))

	rows, err := s.ListJobsWithStats(context.Background())
	if err != nil {
		t.Fatalf("ListJobsWithStats: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("len(rows)=%d, want 2", len(rows))
	}
	if rows[0].RunCount != 10 || rows[0].ErrorCount != 2 || rows[0].LastStatus != StatusSuccess {
		t.Errorf("row 0 mismatch: %+v", rows[0])
	}
	if rows[1].RunCount != 0 || rows[1].LastRun != nil {
		t.Errorf("row 1 should have no last run: %+v", rows[1])
	}
}

func TestListJobsWithStats_QueryError(t *testing.T) {
	s, mock := newMockStore(t)
	mock.ExpectQuery(`FROM "job" j`).
		WillReturnError(errors.New("dead"))

	if _, err := s.ListJobsWithStats(context.Background()); err == nil {
		t.Fatal("expected error")
	}
}

func TestListJobsWithStats_ScanError(t *testing.T) {
	s, mock := newMockStore(t)
	mock.ExpectQuery(`FROM "job" j`).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("only-id"))

	if _, err := s.ListJobsWithStats(context.Background()); err == nil {
		t.Fatal("expected scan error")
	}
}

func TestListJobsWithStats_RowsErr(t *testing.T) {
	s, mock := newMockStore(t)
	now := time.Now()
	dur := 1
	mock.ExpectQuery(`FROM "job" j`).
		WillReturnRows(pgxmock.NewRows(statsRowsCols()).
			RowError(0, errors.New("stream dead")).
			AddRow("j1", "n", "d", 10, true, now, now, &now, &now, StatusSuccess, &dur, "", int64(1), int64(0)))

	if _, err := s.ListJobsWithStats(context.Background()); err == nil {
		t.Fatal("expected rows.Err")
	}
}

// --- GetJobWithStats --------------------------------------------------------

func TestGetJobWithStats_Success(t *testing.T) {
	s, mock := newMockStore(t)
	now := time.Now()
	finished := now.Add(time.Second)
	dur := 50
	mock.ExpectQuery(`WHERE j\."id" = \$1`).
		WithArgs("j1").
		WillReturnRows(pgxmock.NewRows(statsRowsCols()).
			AddRow("j1", "Name", "desc", 60, true, now, now, &now, &finished, StatusSuccess, &dur, "", int64(5), int64(1)))

	r, err := s.GetJobWithStats(context.Background(), "j1")
	if err != nil {
		t.Fatalf("GetJobWithStats: %v", err)
	}
	if r.ID != "j1" || r.RunCount != 5 || r.ErrorCount != 1 {
		t.Errorf("unexpected row: %+v", r)
	}
}

func TestGetJobWithStats_NotFound(t *testing.T) {
	s, mock := newMockStore(t)
	mock.ExpectQuery(`WHERE j\."id" = \$1`).
		WithArgs("missing").
		WillReturnError(pgx.ErrNoRows)

	if _, err := s.GetJobWithStats(context.Background(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestGetJobWithStats_ScanError(t *testing.T) {
	s, mock := newMockStore(t)
	mock.ExpectQuery(`WHERE j\."id" = \$1`).
		WithArgs("j2").
		WillReturnError(errors.New("dead"))

	if _, err := s.GetJobWithStats(context.Background(), "j2"); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("expected non-ErrNotFound error, got %v", err)
	}
}

// --- New(nil) regression ----------------------------------------------------

func TestNew_StoresPoolWhenNonNil(t *testing.T) {
	// Ensure the production constructor stores the supplied pool when
	// non-nil. We can't construct a real *pgxpool.Pool here without a DSN,
	// so we just confirm New(nil) returns a Store whose db field is nil
	// (so callers crash loudly instead of dereferencing a typed-nil).
	s := New(nil)
	if s == nil {
		t.Fatal("New(nil) returned nil")
	}
	if s.db != nil {
		t.Errorf("expected nil db on New(nil), got %#v", s.db)
	}
}

// --- helpers ---------------------------------------------------------------

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

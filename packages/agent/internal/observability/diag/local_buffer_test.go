package diag

import (
	"bytes"
	"database/sql"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/mutecomm/go-sqlcipher/v4"

	shareddiag "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/diag"
	opsmetrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
)

// newTestLocalBuffer opens an in-memory SQLite database (no encryption — the
// SQLCipher driver runs as plain SQLite when no PRAGMA key is set) and runs
// the pending_diag_event migration. The schema is identical between the
// encrypted and unencrypted builds, so the in-memory DB exercises the
// LocalBuffer logic without requiring a SQLCipher key in tests.
func newTestLocalBuffer(t *testing.T) (*LocalBuffer, *sql.DB) {
	t.Helper()
	db, err := sql.Open("sqlite3", "file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := MigratePendingDiagEvent(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return NewLocalBuffer(db, nil), db
}

func makeDiagEvent(id string, occurred time.Time, level string) opsmetrics.DiagEvent {
	return opsmetrics.DiagEvent{
		ThingID:     "thing-1",
		OccurredAt:  occurred,
		Level:       level,
		EventType:   opsmetrics.EventTypeCrash,
		Source:      "main",
		Message:     "boom-" + id,
		MessageHash: id,
		RepeatCount: 1,
	}
}

func TestInsert_StoresAndPersists(t *testing.T) {
	buf, _ := newTestLocalBuffer(t)
	evt := makeDiagEvent("hash-1", time.Now().UTC(), opsmetrics.LevelFatal)

	if err := buf.Insert(evt); err != nil {
		t.Fatalf("Insert error: %v", err)
	}
	count, err := buf.Pending()
	if err != nil {
		t.Fatalf("Pending error: %v", err)
	}
	if count != 1 {
		t.Errorf("Pending = %d, want 1", count)
	}

	got, err := buf.List(10)
	if err != nil {
		t.Fatalf("List error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("List len = %d, want 1", len(got))
	}
	if got[0].Message != "boom-hash-1" {
		t.Errorf("message round-trip mismatch: %q", got[0].Message)
	}
	if got[0].Level != opsmetrics.LevelFatal {
		t.Errorf("level = %q, want fatal", got[0].Level)
	}
}

func TestList_OrdersByOccurredAt(t *testing.T) {
	buf, _ := newTestLocalBuffer(t)

	base := time.Now().UTC().Truncate(time.Second)
	// Insert in random order; expect ascending by occurred_at.
	if err := buf.Insert(makeDiagEvent("c", base.Add(2*time.Second), opsmetrics.LevelFatal)); err != nil {
		t.Fatal(err)
	}
	if err := buf.Insert(makeDiagEvent("a", base, opsmetrics.LevelFatal)); err != nil {
		t.Fatal(err)
	}
	if err := buf.Insert(makeDiagEvent("b", base.Add(1*time.Second), opsmetrics.LevelFatal)); err != nil {
		t.Fatal(err)
	}

	got, err := buf.List(10)
	if err != nil {
		t.Fatalf("List error: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("List len = %d, want 3", len(got))
	}
	wantOrder := []string{"boom-a", "boom-b", "boom-c"}
	for i, e := range got {
		if e.Message != wantOrder[i] {
			t.Errorf("List[%d].Message = %q, want %q", i, e.Message, wantOrder[i])
		}
	}
}

func TestDelete_RemovesByID(t *testing.T) {
	buf, _ := newTestLocalBuffer(t)

	for i := range 3 {
		evt := makeDiagEvent(fmt.Sprintf("h%d", i), time.Now().UTC().Add(time.Duration(i)*time.Second), opsmetrics.LevelFatal)
		if err := buf.Insert(evt); err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}

	got, err := buf.List(10)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("pre-delete List len = %d, want 3", len(got))
	}

	// Delete the first two by id.
	ids := []string{got[0].ID, got[1].ID}
	if err := buf.Delete(ids); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	count, _ := buf.Pending()
	if count != 1 {
		t.Errorf("post-delete count = %d, want 1", count)
	}
	remaining, _ := buf.List(10)
	if len(remaining) != 1 || remaining[0].ID != got[2].ID {
		t.Errorf("expected only %s to remain, got %+v", got[2].ID, remaining)
	}
}

func TestDelete_EmptyIsNoop(t *testing.T) {
	buf, _ := newTestLocalBuffer(t)
	if err := buf.Delete(nil); err != nil {
		t.Errorf("Delete(nil) returned %v", err)
	}
	if err := buf.Delete([]string{}); err != nil {
		t.Errorf("Delete([]) returned %v", err)
	}
}

func TestIncrAttempts_BumpsCounter(t *testing.T) {
	buf, db := newTestLocalBuffer(t)

	evt := makeDiagEvent("attempt-1", time.Now().UTC(), opsmetrics.LevelFatal)
	if err := buf.Insert(evt); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	got, _ := buf.List(10)
	if len(got) != 1 {
		t.Fatalf("List len = %d", len(got))
	}
	id := got[0].ID

	if err := buf.IncrAttempts([]string{id}); err != nil {
		t.Fatalf("IncrAttempts: %v", err)
	}
	if err := buf.IncrAttempts([]string{id}); err != nil {
		t.Fatalf("IncrAttempts: %v", err)
	}

	var attempts int
	if err := db.QueryRow("SELECT attempts FROM pending_diag_event WHERE id = ?", id).Scan(&attempts); err != nil {
		t.Fatalf("attempts query: %v", err)
	}
	if attempts != 2 {
		t.Errorf("attempts = %d, want 2", attempts)
	}
}

func TestIncrAttempts_EmptyIsNoop(t *testing.T) {
	buf, _ := newTestLocalBuffer(t)
	if err := buf.IncrAttempts(nil); err != nil {
		t.Errorf("IncrAttempts(nil) returned %v", err)
	}
}

func TestSchemaMigrationIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "diag.db")
	dsn := dbPath + "?_journal_mode=WAL&_busy_timeout=5000"

	for i := range 3 {
		db, err := sql.Open("sqlite3", dsn)
		if err != nil {
			t.Fatalf("open #%d: %v", i, err)
		}
		if err := MigratePendingDiagEvent(db); err != nil {
			t.Fatalf("migrate #%d: %v", i, err)
		}
		if err := db.Close(); err != nil {
			t.Fatalf("close #%d: %v", i, err)
		}
	}
}

func TestList_LimitRespected(t *testing.T) {
	buf, _ := newTestLocalBuffer(t)
	base := time.Now().UTC()
	for i := range 5 {
		_ = buf.Insert(makeDiagEvent(fmt.Sprintf("h%d", i), base.Add(time.Duration(i)*time.Second), opsmetrics.LevelFatal))
	}
	got, err := buf.List(2)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("List(2) returned %d rows", len(got))
	}
}

// Sanity check: the local buffer satisfies the shareddiag.LocalBufferInserter
// interface so the agent's main wiring can pass *LocalBuffer directly into
// shareddiag.SlogSinkConfig.
var _ shareddiag.LocalBufferInserter = (*LocalBuffer)(nil)

func TestInsert_PreservesAttrsAndStack(t *testing.T) {
	buf, _ := newTestLocalBuffer(t)
	evt := opsmetrics.DiagEvent{
		ThingID:     "thing-1",
		OccurredAt:  time.Now().UTC(),
		Level:       opsmetrics.LevelFatal,
		EventType:   opsmetrics.EventTypeCrash,
		Source:      "main",
		Message:     "panic: nil map",
		MessageHash: "deadbeef",
		Attrs:       map[string]any{"goroutine": "audit-drain"},
		StackTrace:  "goroutine 1 [running]:\nmain.cmdRun(...)\n",
		RepeatCount: 1,
	}
	if err := buf.Insert(evt); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	got, err := buf.List(1)
	if err != nil || len(got) != 1 {
		t.Fatalf("List: err=%v len=%d", err, len(got))
	}
	if got[0].Attrs["goroutine"] != "audit-drain" {
		t.Errorf("attrs round-trip: %+v", got[0].Attrs)
	}
	if !strings.Contains(got[0].StackTrace, "main.cmdRun") {
		t.Errorf("stack round-trip: %q", got[0].StackTrace)
	}
}

// TestInsert_ZeroOccurredAtBackfillsNow asserts the Insert backfill path: a
// caller that forgets to populate OccurredAt still gets a wall-clock
// timestamp so the row sorts correctly under List's ORDER BY occurred_at.
// Observable behavior: the stored occurred_at is non-empty and parses as a
// timestamp within a recent window.
func TestInsert_ZeroOccurredAtBackfillsNow(t *testing.T) {
	buf, db := newTestLocalBuffer(t)
	before := time.Now().UTC().Add(-time.Second)

	evt := makeDiagEvent("zero", time.Time{}, opsmetrics.LevelFatal)
	if err := buf.Insert(evt); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	var ts string
	if err := db.QueryRow(`SELECT occurred_at FROM pending_diag_event`).Scan(&ts); err != nil {
		t.Fatalf("query: %v", err)
	}
	got, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		t.Fatalf("parse stored ts %q: %v", ts, err)
	}
	if got.Before(before) {
		t.Errorf("backfilled occurred_at = %v, expected >= %v", got, before)
	}
	if time.Since(got) > time.Minute {
		t.Errorf("backfilled occurred_at too old: %v", got)
	}
}

// TestMigratePendingDiagEvent_NilDb covers the nil-db guard.
func TestMigratePendingDiagEvent_NilDb(t *testing.T) {
	if err := MigratePendingDiagEvent(nil); err == nil {
		t.Fatal("expected error for nil db")
	} else if !strings.Contains(err.Error(), "nil db") {
		t.Errorf("error = %v, want nil db", err)
	}
}

// TestMigratePendingDiagEvent_ExecError covers the ExecContext failure
// branch: a closed *sql.DB returns ErrDBClosed on Exec, so the migration
// surfaces a wrapped error rather than silently succeeding.
func TestMigratePendingDiagEvent_ExecError(t *testing.T) {
	db, err := sql.Open("sqlite3", "file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	_ = db.Close()
	err = MigratePendingDiagEvent(db)
	if err == nil {
		t.Fatal("expected error for closed db")
	}
	if !strings.Contains(err.Error(), "create pending_diag_event") {
		t.Errorf("error wrap missing context: %v", err)
	}
}

// TestLocalBuffer_NilGuards exercises every method's nil-buffer / nil-db
// guard so a caller that forgets to wire the dependency gets a descriptive
// error instead of a SIGSEGV. Observable behavior: all six methods return
// a non-nil error mentioning the method name.
func TestLocalBuffer_NilGuards(t *testing.T) {
	var nilBuf *LocalBuffer
	if err := nilBuf.Insert(opsmetrics.DiagEvent{}); err == nil {
		t.Error("Insert(nil buffer) should error")
	} else if !strings.Contains(err.Error(), "Insert") {
		t.Errorf("Insert err = %v", err)
	}
	if _, err := nilBuf.List(10); err == nil {
		t.Error("List(nil buffer) should error")
	} else if !strings.Contains(err.Error(), "List") {
		t.Errorf("List err = %v", err)
	}
	if err := nilBuf.Delete([]string{"x"}); err == nil {
		t.Error("Delete(nil buffer) should error")
	} else if !strings.Contains(err.Error(), "Delete") {
		t.Errorf("Delete err = %v", err)
	}
	if err := nilBuf.IncrAttempts([]string{"x"}); err == nil {
		t.Error("IncrAttempts(nil buffer) should error")
	} else if !strings.Contains(err.Error(), "IncrAttempts") {
		t.Errorf("IncrAttempts err = %v", err)
	}
	if _, err := nilBuf.Pending(); err == nil {
		t.Error("Pending(nil buffer) should error")
	} else if !strings.Contains(err.Error(), "Pending") {
		t.Errorf("Pending err = %v", err)
	}

	// nil db, non-nil buffer: same guard, different code path.
	emptyBuf := &LocalBuffer{}
	if err := emptyBuf.Insert(opsmetrics.DiagEvent{}); err == nil {
		t.Error("Insert(nil db) should error")
	}
	if _, err := emptyBuf.List(10); err == nil {
		t.Error("List(nil db) should error")
	}
	if err := emptyBuf.Delete([]string{"x"}); err == nil {
		t.Error("Delete(nil db) should error")
	}
	if err := emptyBuf.IncrAttempts([]string{"x"}); err == nil {
		t.Error("IncrAttempts(nil db) should error")
	}
	if _, err := emptyBuf.Pending(); err == nil {
		t.Error("Pending(nil db) should error")
	}
}

// TestList_DefaultLimitWhenZero covers the `if limit <= 0` branch: the
// drain caller's contract is that 0 means "use the default". Observable
// behavior: a List(0) call returns all rows (here, 3) without raising
// "invalid limit".
func TestList_DefaultLimitWhenZero(t *testing.T) {
	buf, _ := newTestLocalBuffer(t)
	base := time.Now().UTC()
	for i := range 3 {
		if err := buf.Insert(makeDiagEvent(fmt.Sprintf("d%d", i), base.Add(time.Duration(i)*time.Second), opsmetrics.LevelFatal)); err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}
	got, err := buf.List(0)
	if err != nil {
		t.Fatalf("List(0): %v", err)
	}
	if len(got) != 3 {
		t.Errorf("List(0) returned %d, want 3 (default limit covers them all)", len(got))
	}

	// Negative limit also takes the default branch.
	got2, err := buf.List(-5)
	if err != nil {
		t.Fatalf("List(-5): %v", err)
	}
	if len(got2) != 3 {
		t.Errorf("List(-5) returned %d, want 3", len(got2))
	}
}

// TestList_SkipsCorruptRows asserts the documented "corrupt row -> log +
// continue" behavior in List. We inject a row with non-JSON payload
// bytes alongside two valid rows; List must return only the two valid
// rows (no error), and the logger must record the skip with the corrupt
// row's id so an operator can investigate.
func TestList_SkipsCorruptRows(t *testing.T) {
	db, err := sql.Open("sqlite3", "file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := MigratePendingDiagEvent(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	buf := NewLocalBuffer(db, logger)

	// Two valid rows.
	if err := buf.Insert(makeDiagEvent("v1", time.Now().UTC(), opsmetrics.LevelFatal)); err != nil {
		t.Fatalf("Insert v1: %v", err)
	}
	if err := buf.Insert(makeDiagEvent("v2", time.Now().UTC().Add(2*time.Second), opsmetrics.LevelFatal)); err != nil {
		t.Fatalf("Insert v2: %v", err)
	}
	// Directly write a corrupt row.
	corruptID := "corrupt-row-id"
	if _, err := db.Exec(`INSERT INTO pending_diag_event (id, occurred_at, payload, attempts) VALUES (?, ?, ?, 0)`,
		corruptID, time.Now().UTC().Add(1*time.Second).Format(time.RFC3339Nano), []byte("not-json{{{")); err != nil {
		t.Fatalf("insert corrupt: %v", err)
	}

	got, err := buf.List(10)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("List returned %d rows, want 2 (corrupt skipped)", len(got))
	}
	for _, r := range got {
		if r.ID == corruptID {
			t.Errorf("corrupt row leaked: %s", r.ID)
		}
	}
	if !strings.Contains(logBuf.String(), "skip corrupt pending_diag_event row") {
		t.Errorf("logger did not record skip; got: %s", logBuf.String())
	}
	if !strings.Contains(logBuf.String(), corruptID) {
		t.Errorf("logger did not record corrupt id; got: %s", logBuf.String())
	}
}

// TestList_SkipsCorruptRows_NilLogger covers the nil-logger branch of the
// "corrupt row" handler — same shape as the previous test, but without a
// logger so the inner `if b.log != nil` branch falls through to continue.
func TestList_SkipsCorruptRows_NilLogger(t *testing.T) {
	buf, db := newTestLocalBuffer(t) // nil logger by construction
	if err := buf.Insert(makeDiagEvent("v1", time.Now().UTC(), opsmetrics.LevelFatal)); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO pending_diag_event (id, occurred_at, payload, attempts) VALUES (?, ?, ?, 0)`,
		"corrupt-2", time.Now().UTC().Add(-time.Second).Format(time.RFC3339Nano), []byte("xxx")); err != nil {
		t.Fatalf("insert corrupt: %v", err)
	}
	got, err := buf.List(10)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("List returned %d, want 1", len(got))
	}
}

// TestLocalBuffer_DBErrorsAfterClose forces every SQL-bound branch
// (Insert exec, List query, Delete exec, IncrAttempts exec, Pending
// query) by closing the underlying *sql.DB before calling the method.
// Each method must surface a wrapped error rather than panicking.
func TestLocalBuffer_DBErrorsAfterClose(t *testing.T) {
	db, err := sql.Open("sqlite3", "file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := MigratePendingDiagEvent(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	buf := NewLocalBuffer(db, nil)
	// Seed a row so Delete / IncrAttempts have a non-empty id slice path.
	if err := buf.Insert(makeDiagEvent("seed", time.Now().UTC(), opsmetrics.LevelFatal)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_ = db.Close()

	if err := buf.Insert(makeDiagEvent("x", time.Now().UTC(), opsmetrics.LevelFatal)); err == nil {
		t.Error("Insert on closed db should error")
	} else if !strings.Contains(err.Error(), "insert pending diag") {
		t.Errorf("Insert err missing wrap: %v", err)
	}

	if _, err := buf.List(10); err == nil {
		t.Error("List on closed db should error")
	} else if !strings.Contains(err.Error(), "query pending diag") {
		t.Errorf("List err missing wrap: %v", err)
	}

	if err := buf.Delete([]string{"some-id"}); err == nil {
		t.Error("Delete on closed db should error")
	} else if !strings.Contains(err.Error(), "delete pending diag") {
		t.Errorf("Delete err missing wrap: %v", err)
	}

	if err := buf.IncrAttempts([]string{"some-id"}); err == nil {
		t.Error("IncrAttempts on closed db should error")
	} else if !strings.Contains(err.Error(), "incr attempts") {
		t.Errorf("IncrAttempts err missing wrap: %v", err)
	}

	if _, err := buf.Pending(); err == nil {
		t.Error("Pending on closed db should error")
	} else if !strings.Contains(err.Error(), "count pending diag") {
		t.Errorf("Pending err missing wrap: %v", err)
	}
}

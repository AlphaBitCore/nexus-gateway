// Tests pinning observable behaviour for the SQLite-backed audit queue's
// secondary surface (config snapshots, audit_local + lifecycle CRUD,
// today-stats projection, encryption migration, DrainLoop lifecycle,
// backfill helpers). See queue_test.go for the primary Record/Drain
// path. All tests use t.TempDir() to keep the binding rule "tests must
// only touch their own data" satisfied.
package queue

import (
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/observability/audit/event"
	_ "github.com/mutecomm/go-sqlcipher/v4"
)

// newTempQueue opens an on-disk encrypted Queue rooted in t.TempDir().
// Returned Queue is closed via t.Cleanup. The file path is exposed so
// migration / encryption tests can reach in.
func newTempQueue(t *testing.T) (*Queue, string) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "audit.db")
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	q, err := NewQueue(dbPath, key)
	if err != nil {
		t.Fatalf("NewQueue: %v", err)
	}
	t.Cleanup(func() { _ = q.Close() })
	return q, dbPath
}

func TestComputeTodayStats_NilReceiverReturnsZeroes(t *testing.T) {
	var q *Queue
	in, p, d, us, up := q.ComputeTodayStats()
	if in != 0 || p != 0 || d != 0 || us != nil || up != nil {
		t.Fatalf("nil receiver should return zero/nil tuple, got (%d,%d,%d,%v,%v)", in, p, d, us, up)
	}
}

func TestComputeTodayStats_EmptyTable(t *testing.T) {
	q, _ := newTempQueue(t)
	in, p, d, us, up := q.ComputeTodayStats()
	if in != 0 || p != 0 || d != 0 {
		t.Fatalf("empty table should produce zero counts, got (%d,%d,%d)", in, p, d)
	}
	if us != nil || up != nil {
		t.Fatalf("empty table should produce nil averages, got us=%v up=%v", us, up)
	}
}

func TestComputeTodayStats_BucketsByActionAndComputesAverages(t *testing.T) {
	q, _ := newTempQueue(t)

	// Three inspect rows, two passthrough, one deny — all today.
	make := func(id, action string, latency int, upstreamTotal *int) event.Event {
		e := event.Event{
			ID: id, Timestamp: time.Now(),
			SourceProcess: "p", TargetHost: "h",
			DestIP: "1.2.3.4", DestPort: 443,
			Action: action, LatencyMs: latency, UpstreamTotalMs: upstreamTotal,
		}
		return e
	}
	pi := func(v int) *int { return &v }
	for _, e := range []event.Event{
		make("i1", "inspect", 100, pi(60)),    // overhead 40
		make("i2", "inspect", 200, pi(100)),   // overhead 100
		make("i3", "inspect", 0, nil),         // no upstream — skipped from avg
		make("p1", "passthrough", 50, pi(30)), // overhead 20
		make("p2", "passthrough", 80, pi(20)), // overhead 60
		make("d1", "deny", 5, nil),
	} {
		if err := q.Record(e); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	in, p, d, us, up := q.ComputeTodayStats()
	if in != 3 {
		t.Errorf("inspected count: got %d, want 3", in)
	}
	if p != 2 {
		t.Errorf("passthrough count: got %d, want 2", p)
	}
	if d != 1 {
		t.Errorf("denied count: got %d, want 1", d)
	}
	if us == nil {
		t.Fatalf("avg overhead pointer should not be nil")
	}
	if up == nil {
		t.Fatalf("avg upstream pointer should not be nil")
	}
	// Overhead = max(0, duration - upstream_total) on rows that have
	// upstream_total. Values above: 40, 100, 20, 60 → avg = 55.
	// Upstream avg: 60, 100, 30, 20 → avg = 52.
	if *us != 55 {
		t.Errorf("avg overhead: got %d, want 55", *us)
	}
	if *up != 52 {
		t.Errorf("avg upstream: got %d, want 52", *up)
	}
}

// RecordLocal + PruneAuditLocal

func TestRecordLocal_RoundTrip(t *testing.T) {
	q, _ := newTempQueue(t)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if err := q.RecordLocal("loc-1", now, "h.example.com", "1.2.3.4", 443,
		"inspect", "APPROVE", "ok", "/usr/bin/curl", "alice", 100, 200); err != nil {
		t.Fatalf("RecordLocal: %v", err)
	}
	// Duplicate ID is ignored by INSERT OR IGNORE; assertion is
	// no-error + a single row remains.
	if err := q.RecordLocal("loc-1", now, "h.example.com", "1.2.3.4", 443,
		"inspect", "APPROVE", "ok", "/usr/bin/curl", "alice", 100, 200); err != nil {
		t.Fatalf("RecordLocal duplicate: %v", err)
	}
	var n int
	if err := q.DB().QueryRow("SELECT COUNT(*) FROM audit_local").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("audit_local row count: got %d, want 1 (duplicate must be ignored)", n)
	}
}

func TestPruneAuditLocal_NilReceiverError(t *testing.T) {
	var q *Queue
	if _, err := q.PruneAuditLocal(time.Hour); err == nil {
		t.Fatalf("expected error for nil receiver")
	}
}

func TestPruneAuditLocal_DeletesOlderRows(t *testing.T) {
	q, _ := newTempQueue(t)
	// Insert two rows; manually backdate one row's created_at so prune
	// older-than-1h removes exactly one row.
	now := time.Now().UTC()
	if err := q.RecordLocal("old", now.Format(time.RFC3339Nano), "h", "1.2.3.4", 443, "inspect", "", "", "", "", 0, 0); err != nil {
		t.Fatalf("RecordLocal: %v", err)
	}
	if err := q.RecordLocal("new", now.Format(time.RFC3339Nano), "h", "1.2.3.4", 443, "inspect", "", "", "", "", 0, 0); err != nil {
		t.Fatalf("RecordLocal: %v", err)
	}
	twoHoursAgo := now.Add(-2 * time.Hour).Format(time.RFC3339Nano)
	if _, err := q.DB().Exec("UPDATE audit_local SET created_at = ? WHERE id = ?", twoHoursAgo, "old"); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	deleted, err := q.PruneAuditLocal(time.Hour)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if deleted != 1 {
		t.Errorf("deleted count: got %d, want 1", deleted)
	}
	var n int
	_ = q.DB().QueryRow("SELECT COUNT(*) FROM audit_local").Scan(&n)
	if n != 1 {
		t.Errorf("remaining row count: got %d, want 1", n)
	}
}

// PruneLifecycle + RecordLifecycle + QueryLifecycle

func TestRecordLifecycle_NilReceiverError(t *testing.T) {
	var q *Queue
	if err := q.RecordLifecycle("id", time.Now(), "agent.startup", "msg", "info", nil); err == nil {
		t.Fatalf("expected error for nil receiver")
	}
}

func TestRecordLifecycle_NilAttrsAndAttrsRoundTrip(t *testing.T) {
	q, _ := newTempQueue(t)
	// Nil attrs → attrsJSON.Valid=false → row stores NULL.
	if err := q.RecordLifecycle("ev-nil", time.Now().UTC(), "agent.startup", "started", "info", nil); err != nil {
		t.Fatalf("nil attrs: %v", err)
	}
	// Non-empty attrs round-trip.
	if err := q.RecordLifecycle("ev-attrs", time.Now().UTC(), "sso_login", "user signed in", "info",
		map[string]any{"user": "alice", "method": "oidc"}); err != nil {
		t.Fatalf("attrs: %v", err)
	}
	rows, total, err := q.QueryLifecycle(0, 50)
	if err != nil {
		t.Fatalf("QueryLifecycle: %v", err)
	}
	if total != 2 {
		t.Errorf("total: got %d, want 2", total)
	}
	if len(rows) != 2 {
		t.Fatalf("rows: got %d, want 2", len(rows))
	}
	// Find the attrs row.
	var attrsRow *LifecycleEvent
	for i := range rows {
		if rows[i].ID == "ev-attrs" {
			attrsRow = &rows[i]
		}
	}
	if attrsRow == nil {
		t.Fatalf("ev-attrs missing")
	}
	if attrsRow.Attrs["user"] != "alice" {
		t.Errorf("attrs[user]: got %v, want alice", attrsRow.Attrs["user"])
	}
}

func TestRecordLifecycle_UnmarshalableAttrsError(t *testing.T) {
	q, _ := newTempQueue(t)
	// channels are not JSON-serializable — exercises the json.Marshal
	// error branch in RecordLifecycle.
	err := q.RecordLifecycle("ev-bad", time.Now(), "agent.error", "x", "warn",
		map[string]any{"ch": make(chan int)})
	if err == nil {
		t.Fatalf("expected marshal error")
	}
	if !strings.Contains(err.Error(), "marshal lifecycle attrs") {
		t.Errorf("error message: got %q, want it to wrap marshal failure", err.Error())
	}
}

func TestQueryLifecycle_NilReceiverError(t *testing.T) {
	var q *Queue
	if _, _, err := q.QueryLifecycle(0, 10); err == nil {
		t.Fatalf("expected error for nil receiver")
	}
}

func TestQueryLifecycle_LimitDefaults(t *testing.T) {
	q, _ := newTempQueue(t)
	if err := q.RecordLifecycle("ev-1", time.Now().UTC(), "agent.startup", "", "info", nil); err != nil {
		t.Fatalf("RecordLifecycle: %v", err)
	}
	// limit <= 0 must promote to 50, not return zero rows.
	rows, _, err := q.QueryLifecycle(0, 0)
	if err != nil {
		t.Fatalf("QueryLifecycle: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("expected 1 row with default limit, got %d", len(rows))
	}
}

func TestQueryLifecycle_FallbackTimestampParse(t *testing.T) {
	q, _ := newTempQueue(t)
	// Write a lifecycle row whose occurred_at uses plain RFC3339 (no
	// nano precision) by going through the raw DB handle — exercises
	// the "fall back to non-nano RFC3339" branch.
	if _, err := q.DB().Exec(
		"INSERT INTO lifecycle_event (id, occurred_at, action, message, level) VALUES (?, ?, ?, ?, ?)",
		"ev-rfc3339", "2026-05-17T10:00:00Z", "agent.shutdown", "stopped", "info",
	); err != nil {
		t.Fatalf("insert raw row: %v", err)
	}
	rows, _, err := q.QueryLifecycle(0, 10)
	if err != nil {
		t.Fatalf("QueryLifecycle: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows: %d", len(rows))
	}
	if rows[0].OccurredAt.IsZero() {
		t.Error("OccurredAt should be parsed via RFC3339 fallback, got zero time")
	}
}

func TestPruneLifecycle_NilReceiverError(t *testing.T) {
	var q *Queue
	if _, err := q.PruneLifecycle(time.Hour); err == nil {
		t.Fatalf("expected error for nil receiver")
	}
}

func TestPruneLifecycle_DeletesOlderRows(t *testing.T) {
	q, _ := newTempQueue(t)
	now := time.Now().UTC()
	if err := q.RecordLifecycle("old", now, "agent.startup", "", "info", nil); err != nil {
		t.Fatalf("rec old: %v", err)
	}
	if err := q.RecordLifecycle("new", now, "agent.startup", "", "info", nil); err != nil {
		t.Fatalf("rec new: %v", err)
	}
	twoHoursAgo := now.Add(-2 * time.Hour).Format(time.RFC3339Nano)
	if _, err := q.DB().Exec("UPDATE lifecycle_event SET occurred_at = ? WHERE id = ?", twoHoursAgo, "old"); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	deleted, err := q.PruneLifecycle(time.Hour)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if deleted != 1 {
		t.Errorf("deleted: got %d, want 1", deleted)
	}
}

// DrainLoop + DB() + Close

func TestDB_ReturnsUsableHandle(t *testing.T) {
	q, _ := newTempQueue(t)
	if q.DB() == nil {
		t.Fatalf("DB() returned nil")
	}
	if err := q.DB().Ping(); err != nil {
		t.Errorf("ping via DB(): %v", err)
	}
}

func TestDrainLoop_TickerDrainsThenContextCancelFlushes(t *testing.T) {
	q, _ := newTempQueue(t)
	// Pre-load one event so the first tick has work.
	if err := q.Record(makeEvent("dl-1")); err != nil {
		t.Fatalf("record: %v", err)
	}
	var (
		mu      sync.Mutex
		batches [][]event.Event
	)
	uploadFn := func(events []event.Event) error {
		mu.Lock()
		defer mu.Unlock()
		dup := make([]event.Event, len(events))
		copy(dup, events)
		batches = append(batches, dup)
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	loopDone := make(chan struct{})
	go func() {
		q.DrainLoop(ctx, 10*time.Millisecond, 10, uploadFn)
		close(loopDone)
	}()
	// Wait for at least one tick to drain the preloaded event.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		drained := len(batches) > 0
		mu.Unlock()
		if drained {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	if len(batches) == 0 {
		mu.Unlock()
		t.Fatalf("expected at least one batch from ticker before cancel")
	}
	mu.Unlock()

	// Stage one MORE event right before cancel so the ctx.Done branch
	// has its own drain to flush.
	if err := q.Record(makeEvent("dl-2")); err != nil {
		t.Fatalf("record dl-2: %v", err)
	}
	cancel()
	select {
	case <-loopDone:
	case <-time.After(2 * time.Second):
		t.Fatalf("DrainLoop did not return after context cancel")
	}
	if q.UnsyncedCount() != 0 {
		t.Errorf("expected post-cancel drain to flush all events, got %d unsynced", q.UnsyncedCount())
	}
}

// migrateToEncrypted (via NewQueue) + key access errors

// TestNewQueue_MigratesUnencryptedFile pins the contract:
// opening an existing UNENCRYPTED database file with a non-nil key
// triggers the in-place migration to SQLCipher. After the migration,
// the same key reopens the file successfully and the original rows
// are still readable. A *.unencrypted.backup file is left behind for
// rollback.
func TestNewQueue_MigratesUnencryptedFile(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "audit.db")

	// Phase 1: open WITHOUT encryption + insert one row.
	plain, err := NewQueue(dbPath, nil)
	if err != nil {
		t.Fatalf("plain NewQueue: %v", err)
	}
	if err := plain.Record(makeEvent("migrate-1")); err != nil {
		t.Fatalf("plain Record: %v", err)
	}
	_ = plain.Close()
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("plain db not on disk: %v", err)
	}

	// Phase 2: reopen with a non-nil key — should trigger migration.
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(0xAB)
	}
	enc, err := NewQueue(dbPath, key)
	if err != nil {
		t.Fatalf("encrypted NewQueue (migration): %v", err)
	}
	t.Cleanup(func() { _ = enc.Close() })

	// Backup file should exist.
	if _, err := os.Stat(dbPath + ".unencrypted.backup"); err != nil {
		t.Errorf("expected backup file %q after migration: %v", dbPath+".unencrypted.backup", err)
	}

	// Original row must be retrievable through the encrypted handle.
	got, err := enc.DrainBatch(10)
	if err != nil {
		t.Fatalf("DrainBatch after migration: %v", err)
	}
	if len(got) != 1 || got[0].ID != "migrate-1" {
		t.Errorf("post-migration row missing or wrong; got %d rows", len(got))
	}
}

// TestNewQueue_WrongKeyAfterMigrationFails pins that an already-encrypted
// file refuses a different key (no silent re-migration loop).
func TestNewQueue_WrongKeyOnEncryptedFile(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "audit.db")
	good := make([]byte, 32)
	for i := range good {
		good[i] = byte(i)
	}
	q, err := NewQueue(dbPath, good)
	if err != nil {
		t.Fatalf("initial NewQueue: %v", err)
	}
	_ = q.Close()

	bad := make([]byte, 32)
	for i := range bad {
		bad[i] = byte(0xFF - i)
	}
	q2, err := NewQueue(dbPath, bad)
	// Per current implementation, wrong key triggers the
	// "migrate from unencrypted" branch which fails because the file
	// is already encrypted. That surfaces as a returned error.
	// If it ever succeeds, the audit DB encryption guarantee is broken.
	if err == nil {
		_ = q2.Close()
		t.Fatalf("opening encrypted file with wrong key must fail; got success")
	}
}

// TestMigrateToEncrypted_PlainNotAccessibleErrors covers the explicit
// "plain db not accessible" branch in migrateToEncrypted by pointing
// at a file path that does not exist as a valid sqlite database.
// Note: sqlite3 will lazily create the file, so plain access succeeds
// — instead we point at a directory which fails the SELECT count.
func TestMigrateToEncrypted_PlainPathIsDirectoryErrors(t *testing.T) {
	dir := t.TempDir()
	// Use the temp dir itself as the "db path" — sqlite will fail to
	// open it as a database file.
	key := make([]byte, 32)
	err := migrateToEncrypted(dir, key)
	if err == nil {
		t.Fatalf("expected error when plain path is a directory")
	}
}

// TestMigrateToEncrypted_InstallEncryptedRollback simulates the
// "install encrypted db" failure by making the target path a
// pre-existing directory; the function rolls back via os.Rename.
// We can't easily fault-inject without a seam, but we can verify
// the function returns an error when the source plain file is
// readable but the rename target conflicts.
func TestMigrateToEncrypted_AttachFailsWhenEncryptedPathIsDir(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "plain.db")
	// Create a valid empty sqlite db.
	plain, err := sql.Open("sqlite3", dbPath+"?_journal_mode=WAL")
	if err != nil {
		t.Fatalf("open plain: %v", err)
	}
	if _, err := plain.Exec("CREATE TABLE t (id INTEGER)"); err != nil {
		t.Fatalf("create plain: %v", err)
	}
	_ = plain.Close()

	// Pre-create a DIRECTORY at the .encrypted path so ATTACH fails.
	encPath := dbPath + ".encrypted"
	if err := os.MkdirAll(encPath, 0o755); err != nil {
		t.Fatalf("mkdir encPath: %v", err)
	}
	key := make([]byte, 32)
	err = migrateToEncrypted(dbPath, key)
	if err == nil {
		t.Fatalf("expected ATTACH error when encrypted path is a directory")
	}
}

// TestNewQueue_OpenFailureSurfaces drives the sql.Open error branch
// (lines 61-63) by passing a path that points to an unreadable dir.
// Note: sqlite3 driver is permissive and creates files lazily, so we
// must use a DSN format that the driver rejects.
func TestNewQueue_DSNWithInvalidFlagSurfacesError(t *testing.T) {
	// Empty path with a key forces buildDSN to produce a path-encoded
	// hex-key DSN; an empty dbPath still lets sql.Open succeed (it
	// creates ".") but the subsequent testDBAccess fails — yields a
	// migration attempt. We instead point at the temp dir itself.
	dir := t.TempDir()
	key := make([]byte, 32)
	// dbPath IS the directory — sql.Open succeeds (lazy), Exec/CREATE
	// fails, which exercises the create-tables error branch (264-267).
	q, err := NewQueue(dir, key)
	if err == nil {
		_ = q.Close()
		t.Fatal("expected error when dbPath is a directory")
	}
}

// TestNewQueue_OpenUnencryptedDirPath drives the create-tables error
// branch (lines 264-267) by passing a directory as dbPath without a
// key — bypasses the migration logic and hits the schema CREATE.
func TestNewQueue_CreateTablesErrorOnDirectoryPath(t *testing.T) {
	dir := t.TempDir()
	q, err := NewQueue(dir, nil)
	if err == nil {
		_ = q.Close()
		t.Fatal("expected error when dbPath is a directory")
	}
	if !strings.Contains(err.Error(), "create audit tables") &&
		!strings.Contains(err.Error(), "open audit db") {
		t.Errorf("error should wrap create-tables or open failure; got %v", err)
	}
}

// TestMigrateToEncrypted_BackupRenameFailureRollsBack pins the
// "backup plain db" rename-failure branch. Strategy: make the
// PARENT directory non-writable so os.Rename(dbPath → .backup)
// fails. The function must surface the error and have cleaned up
// .encrypted along the way.
func TestMigrateToEncrypted_BackupRenameFailureSurfaces(t *testing.T) {
	dir := t.TempDir()
	// Build the plain db under a SUB-directory we can chmod 0o500.
	sub := filepath.Join(dir, "ro")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	dbPath := filepath.Join(sub, "plain.db")
	plain, err := sql.Open("sqlite3", dbPath+"?_journal_mode=WAL")
	if err != nil {
		t.Fatalf("open plain: %v", err)
	}
	if _, err := plain.Exec("CREATE TABLE t (id INTEGER)"); err != nil {
		t.Fatalf("create plain: %v", err)
	}
	_ = plain.Close()

	// Drop write permission on the sub-directory; subsequent
	// os.Rename within it fails (Linux/macOS semantics).
	if err := os.Chmod(sub, 0o500); err != nil {
		t.Fatalf("chmod ro: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(sub, 0o755) }) // allow rmdir

	key := make([]byte, 32)
	err = migrateToEncrypted(dbPath, key)
	if err == nil {
		t.Fatalf("expected error when backup rename fails (ro dir)")
	}
}

// encodeBreakdown + encodeTags + decodeTags round-trip / nil paths

func TestEncodeBreakdown_NilAndPopulated(t *testing.T) {
	if got := encodeBreakdown(nil); got != nil {
		t.Errorf("nil map should return nil, got %v", got)
	}
	if got := encodeBreakdown(map[string]int{}); got != nil {
		t.Errorf("empty map should return nil, got %v", got)
	}
	s, ok := encodeBreakdown(map[string]int{"phase": 5}).(string)
	if !ok || !strings.Contains(s, `"phase":5`) {
		t.Errorf("populated map encoded badly: %v", s)
	}
}

func TestEncodeTags_NilAndPopulated(t *testing.T) {
	if got := encodeTags(nil); got != nil {
		t.Errorf("nil tags should return nil")
	}
	if got := encodeTags([]string{}); got != nil {
		t.Errorf("empty tags should return nil")
	}
	s, ok := encodeTags([]string{"pii", "secret"}).(string)
	if !ok || !strings.Contains(s, "pii") {
		t.Errorf("populated tags encoded badly: %v", s)
	}
}

func TestDecodeTags_NullAndMalformedReturnNil(t *testing.T) {
	if got := decodeTags(sql.NullString{Valid: false}); got != nil {
		t.Errorf("invalid NullString should decode to nil")
	}
	if got := decodeTags(sql.NullString{String: "", Valid: true}); got != nil {
		t.Errorf("empty NullString should decode to nil")
	}
	if got := decodeTags(sql.NullString{String: "not-json", Valid: true}); got != nil {
		t.Errorf("malformed JSON should decode to nil")
	}
	got := decodeTags(sql.NullString{String: `["a","b"]`, Valid: true})
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("expected [a b], got %v", got)
	}
}

// NewQueue + sqlite key handling

// TestNewQueue_OpenInMemoryNoKey verifies the buildDSN ":memory:"
// branch (already exercised by other tests via the helper), plus
// the testDBAccess utility on a fresh open.
func TestTestDBAccess_FreshHandleSucceeds(t *testing.T) {
	q, _ := newTempQueue(t)
	if err := testDBAccess(q.DB()); err != nil {
		t.Fatalf("testDBAccess on fresh queue: %v", err)
	}
}

// TestNewQueue_SchemaMigrationsAreIdempotent runs NewQueue twice on
// the same file; the second call must NOT error out on the
// "duplicate column" ALTER attempts.
func TestNewQueue_OpenTwiceIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "idem.db")
	q1, err := NewQueue(dbPath, nil)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	_ = q1.Close()
	q2, err := NewQueue(dbPath, nil)
	if err != nil {
		t.Fatalf("second open (must tolerate duplicate columns): %v", err)
	}
	_ = q2.Close()
}

// TestNewQueue_HexKeyDSNFormat sanity-checks the hex encoding of the
// encryption key by feeding a known prefix and asserting the resulting
// queue is usable (driver consumed the PRAGMA key).
func TestNewQueue_HexKeyIsAcceptedByDriver(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "k.db")
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	q, err := NewQueue(dbPath, key)
	if err != nil {
		t.Fatalf("NewQueue: %v", err)
	}
	defer q.Close() //nolint:errcheck
	if err := q.Record(makeEvent("hk-1")); err != nil {
		t.Fatalf("record under hex key: %v", err)
	}
	// And the on-disk file should not start with the SQLite magic
	// "SQLite format 3\0" — SQLCipher encrypts the header.
	buf := make([]byte, 16)
	f, err := os.Open(dbPath)
	if err != nil {
		t.Fatalf("open db file: %v", err)
	}
	defer f.Close() //nolint:errcheck
	_, _ = f.Read(buf)
	if strings.HasPrefix(string(buf), "SQLite format 3") {
		t.Errorf("expected encrypted header, got plain SQLite magic — hex key %q failed to encrypt", hex.EncodeToString(key))
	}
}

// Unused helper to silence the linter on potential unused-import warnings.
var _ = fmt.Sprintf

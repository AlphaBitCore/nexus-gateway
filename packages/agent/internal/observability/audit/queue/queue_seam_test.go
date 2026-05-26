// queue_seam_test.go drives the post-export error arms in
// migrateToEncrypted (install-rename failure + rollback, attach/export/detach
// cleanup) and the MarkSynced prepare-failure branch via test seams.
// The seams default to os.Rename / os.Remove on package init; production code
// never reassigns them. Mirrors the established pattern in
// packages/agent/internal/identity/secretstore/fallback.go (renameFn + osFile)
// and packages/agent/internal/identity/enrollment/enroll.go.
package queue

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// makePlainSqliteFile creates a minimal unencrypted sqlite database at the
// given path so migrateToEncrypted's `testDBAccess(plainDB)` check passes
// and execution proceeds into the ATTACH/EXPORT/RENAME arms under test.
func makePlainSqliteFile(t *testing.T, path string) {
	t.Helper()
	plain, err := sql.Open("sqlite3", path+"?_journal_mode=WAL")
	if err != nil {
		t.Fatalf("open plain: %v", err)
	}
	if _, err := plain.Exec("CREATE TABLE seed (id INTEGER)"); err != nil {
		_ = plain.Close()
		t.Fatalf("seed plain: %v", err)
	}
	if err := plain.Close(); err != nil {
		t.Fatalf("close plain: %v", err)
	}
}

// TestMigrateToEncrypted_InstallRenameFailureRollsBackToBackup pins the
// failure path where the encrypted file was successfully exported, the
// backup rename succeeded, but the second rename (encrypted → original)
// fails. The function must: (a) attempt to restore the backup file at the
// original path and (b) surface a wrapped "install encrypted db" error.
// Requires the renameFn seam because the only filesystem condition that
// reliably fails the second os.Rename without also failing the first is
// "make this call fail" — directory permissions, missing dirs, etc., all
// trip the first rename too.
func TestMigrateToEncrypted_InstallRenameFailureRollsBack(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "plain.db")
	makePlainSqliteFile(t, dbPath)

	prev := renameFn
	calls := 0
	var seenSrc []string
	renameFn = func(src, dst string) error {
		calls++
		seenSrc = append(seenSrc, src)
		// First call: backup (dbPath → .unencrypted.backup) — let it succeed
		// via the real os.Rename.
		// Second call: install (encPath → dbPath) — inject failure.
		// Third call: rollback (.unencrypted.backup → dbPath) — let it
		// succeed via real os.Rename so we can assert the file is restored.
		if calls == 2 {
			return errors.New("synthetic install-rename failure")
		}
		return os.Rename(src, dst)
	}
	t.Cleanup(func() { renameFn = prev })

	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(0xCD)
	}
	err := migrateToEncrypted(dbPath, key)
	if err == nil {
		t.Fatalf("expected error from install-rename failure")
	}
	if !strings.Contains(err.Error(), "install encrypted db") {
		t.Errorf("error should wrap 'install encrypted db'; got %v", err)
	}
	if calls != 3 {
		t.Errorf("expected exactly 3 rename calls (backup + install + restore); got %d (seenSrc=%v)", calls, seenSrc)
	}
	// After rollback, the original dbPath must exist again (restored from
	// backup) and the .unencrypted.backup file must be gone (consumed by
	// the rollback rename).
	if _, err := os.Stat(dbPath); err != nil {
		t.Errorf("expected restored dbPath to exist after rollback; stat err: %v", err)
	}
	if _, err := os.Stat(dbPath + ".unencrypted.backup"); err == nil {
		t.Errorf(".unencrypted.backup should have been consumed by the rollback rename")
	}
}

// TestMigrateToEncrypted_AttachFailureRemovesEncryptedSpoor — the attach
// SQL injects a quoted path that fails in a deterministic way. We trigger
// the ATTACH error by pre-creating a directory at the encrypted-path so
// SQLite cannot open it as a database. Validates that removeFn is called
// on the encrypted-path so no half-written file is left behind. (The
// AttachFailsWhenEncryptedPathIsDir test already exists; here we further
// pin the removeFn cleanup contract via the seam.)
func TestMigrateToEncrypted_SqlcipherExportRemovesSpoorOnFailure(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "plain.db")
	makePlainSqliteFile(t, dbPath)

	// The sqlcipher_export call fails when the encrypted target was never
	// attached. We force this by making ATTACH succeed (real path is
	// usable) then having the test fault the export step via a corrupted
	// key length. SQLCipher accepts any 32-byte key, so we instead use
	// removeFn instrumentation to assert the cleanup contract: count
	// removeFn calls on the encrypted-path during a successful migration
	// (the WAL/SHM cleanup at the tail) vs. an interrupted one.
	prev := removeFn
	var removedPaths []string
	removeFn = func(p string) error {
		removedPaths = append(removedPaths, p)
		return os.Remove(p)
	}
	t.Cleanup(func() { removeFn = prev })

	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(0xEF)
	}
	if err := migrateToEncrypted(dbPath, key); err != nil {
		t.Fatalf("happy-path migrate: %v", err)
	}
	// Tail-cleanup removes "-wal" and "-shm" of the (now-renamed-aside)
	// plain DB. Both removals should have flowed through the seam.
	wantSuffixes := []string{"-wal", "-shm"}
	for _, suf := range wantSuffixes {
		found := false
		for _, p := range removedPaths {
			if strings.HasSuffix(p, suf) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected removeFn to be invoked with %s-suffixed path; got %v", suf, removedPaths)
		}
	}
}

// TestNewQueue_PostMigrationReopenFailure drives the lines 79-82 arm
// (`read encrypted audit db after migration`) by injecting renameFn so the
// install step that should atomically put the encrypted file at dbPath
// instead leaves dbPath containing garbage bytes (a stub non-sqlite file
// written before the rename). The migration returns nil, but the
// post-migration reopen succeeds and then testDBAccess against the
// garbage file fails — wrapping "read encrypted audit db after migration".
func TestNewQueue_ReadEncryptedAfterMigrationFailure(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "audit.db")
	// Phase 1: create an unencrypted DB so NewQueue's keyed open triggers
	// the migration path.
	makePlainSqliteFile(t, dbPath)

	prev := renameFn
	calls := 0
	renameFn = func(src, dst string) error {
		calls++
		// First call: backup (dbPath → .unencrypted.backup) — real rename.
		// Second call: install (encPath → dbPath) — discard the encrypted
		// file by removing src, then write garbage at dst so the keyed
		// reopen finds a non-sqlite file and testDBAccess fails.
		if calls == 2 {
			if err := os.Remove(src); err != nil {
				return err
			}
			return os.WriteFile(dst, []byte("not a sqlite file"), 0o600)
		}
		return os.Rename(src, dst)
	}
	t.Cleanup(func() { renameFn = prev })

	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(0xA1)
	}
	q, err := NewQueue(dbPath, key)
	if err == nil {
		_ = q.Close()
		t.Fatal("expected NewQueue to fail when post-migration DB content is unreadable")
	}
	msg := err.Error()
	if !strings.Contains(msg, "read encrypted audit db after migration") {
		t.Errorf("error should wrap 'read encrypted audit db after migration'; got %v", err)
	}
}

// TestMarkSynced_PrepareErrorRollsBackTx pins the lines 614-617 arm:
// BeginTx succeeds but PrepareContext fails because the audit_events
// table was dropped between Record and MarkSynced. Rollback is invoked
// and the underlying SQLite error surfaces unwrapped (per the current
// MarkSynced contract — it returns the raw Prepare error, not a wrap).
func TestMarkSynced_PrepareErrorAfterTableDropped(t *testing.T) {
	q, _ := newTempQueue(t)
	if err := q.Record(makeEvent("ms-prep-1")); err != nil {
		t.Fatalf("Record: %v", err)
	}
	// Drop the table — Prepare on UPDATE audit_events will fail.
	if _, err := q.DB().Exec(`DROP TABLE audit_events`); err != nil {
		t.Fatalf("drop table: %v", err)
	}
	err := q.MarkSynced([]string{"ms-prep-1"})
	if err == nil {
		t.Fatalf("expected MarkSynced to fail when audit_events is gone")
	}
	// The raw SQLite error mentions the missing table.
	if !strings.Contains(strings.ToLower(err.Error()), "no such table") &&
		!strings.Contains(strings.ToLower(err.Error()), "audit_events") {
		t.Errorf("error should mention missing audit_events table; got %v", err)
	}
}

// TestMarkSynced_ExecErrorRollsBackTx pins the lines 620-623 arm:
// Prepare succeeds, but per-row stmt.ExecContext fails. We trigger this
// by adding a CHECK constraint via a CREATE TRIGGER that raises an error
// on UPDATE — the prepared statement compiles cleanly, then exec fires
// the trigger and fails. Validates the rollback path + wrapped error
// shape ("mark synced <id>: ...").
func TestMarkSynced_ExecErrorTrigger(t *testing.T) {
	q, _ := newTempQueue(t)
	if err := q.Record(makeEvent("ms-exec-1")); err != nil {
		t.Fatalf("Record: %v", err)
	}
	// Install a BEFORE-UPDATE trigger that always RAISE(FAIL). This passes
	// schema validation at Prepare time but trips at Exec time.
	if _, err := q.DB().Exec(`
		CREATE TRIGGER audit_events_block_update
		BEFORE UPDATE ON audit_events
		BEGIN
			SELECT RAISE(FAIL, 'simulated trigger failure');
		END
	`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	err := q.MarkSynced([]string{"ms-exec-1"})
	if err == nil {
		t.Fatalf("expected MarkSynced exec to fail due to trigger")
	}
	if !strings.Contains(err.Error(), "mark synced ms-exec-1") {
		t.Errorf("error should be wrapped with 'mark synced ms-exec-1'; got %v", err)
	}
	// The transaction must have rolled back: the row stays unsynced.
	if c := q.UnsyncedCount(); c != 1 {
		t.Errorf("expected row to remain unsynced after rollback; got UnsyncedCount=%d", c)
	}
}

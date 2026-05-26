// Coverage for E50BackfillLatencyPhases transaction-level error paths:
// rows.Scan failure, BeginTx failure, ExecContext failure, and Commit failure.
// These four blocks are unreachable with a well-formed SQLite database, so we
// use a minimal database/sql/driver stub registered once via init().
package backfill

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
)

// Minimal controllable driver

// controllers maps DSN names to factory functions that produce fakeConn values
// configured for a specific failure scenario.
var (
	controllersMu sync.Mutex
	controllers   = map[string]func() *fakeConn{}
)

const driverName = "backfill-test-driver"

func init() {
	sql.Register(driverName, &errDriver{})
}

type errDriver struct{}

func (d *errDriver) Open(name string) (driver.Conn, error) {
	controllersMu.Lock()
	factory, ok := controllers[name]
	controllersMu.Unlock()
	if !ok {
		return nil, errors.New("backfill-test-driver: unknown controller: " + name)
	}
	return factory(), nil
}

// fakeConn — configurable driver.Conn

type fakeConn struct {
	// queryRows, when non-nil, is returned by the prepared-statement Query.
	queryRows driver.Rows
	// queryErr, when non-nil, causes the prepared-statement Query to fail.
	queryErr error
	// beginErr, when non-nil, causes Begin to fail.
	beginErr error
	// execErr, when non-nil, causes Exec inside an open transaction to fail.
	execErr error
	// commitErr, when non-nil, causes Commit to fail.
	commitErr error
}

func (c *fakeConn) Prepare(query string) (driver.Stmt, error) {
	return &fakeStmt{conn: c}, nil
}

func (c *fakeConn) Close() error { return nil }

func (c *fakeConn) Begin() (driver.Tx, error) {
	if c.beginErr != nil {
		return nil, c.beginErr
	}
	return &fakeTx{conn: c}, nil
}

// fakeStmt — driver.Stmt

type fakeStmt struct{ conn *fakeConn }

func (s *fakeStmt) Close() error  { return nil }
func (s *fakeStmt) NumInput() int { return -1 }
func (s *fakeStmt) Query(_ []driver.Value) (driver.Rows, error) {
	return s.conn.queryRows, s.conn.queryErr
}
func (s *fakeStmt) Exec(_ []driver.Value) (driver.Result, error) {
	if s.conn.execErr != nil {
		return nil, s.conn.execErr
	}
	return driver.RowsAffected(1), nil
}

// fakeRows — driver.Rows with a deliberate column-count mismatch

// fakeRows emits one row with only one column name. database/sql rejects a
// Scan into 3 destination vars when only 1 column is declared, which triggers
// the rows.Scan error branch at backfill.go:64-67.
type fakeRows struct{ consumed bool }

func (r *fakeRows) Columns() []string { return []string{"id"} }
func (r *fakeRows) Close() error      { return nil }
func (r *fakeRows) Next(dest []driver.Value) error {
	if !r.consumed {
		r.consumed = true
		dest[0] = "row-id"
		return nil
	}
	return io.EOF
}

// threeColRows — driver.Rows with a correct 3-column schema

// threeColRows emits one row whose 3 columns match the SELECT (id, duration_ms,
// hooks_pipeline). A successful Scan lets the code advance past the
// batch-collection loop to the transaction phase — needed to reach the BeginTx,
// ExecContext, and Commit error branches.
type threeColRows struct{ consumed bool }

func (r *threeColRows) Columns() []string { return []string{"id", "duration_ms", "hooks_pipeline"} }
func (r *threeColRows) Close() error      { return nil }
func (r *threeColRows) Next(dest []driver.Value) error {
	if !r.consumed {
		r.consumed = true
		dest[0] = "ev-fake"
		dest[1] = int64(100)
		dest[2] = nil
		return nil
	}
	return io.EOF
}

// fakeTx — driver.Tx

type fakeTx struct{ conn *fakeConn }

func (t *fakeTx) Commit() error {
	if t.conn.commitErr != nil {
		return t.conn.commitErr
	}
	return nil
}
func (t *fakeTx) Rollback() error { return nil }

// openFakeDB opens a *sql.DB backed by the named controller factory.

func openFakeDB(t *testing.T, name string, factory func() *fakeConn) *sql.DB {
	t.Helper()
	controllersMu.Lock()
	controllers[name] = factory
	controllersMu.Unlock()
	db, err := sql.Open(driverName, name)
	if err != nil {
		t.Fatalf("openFakeDB(%q): %v", name, err)
	}
	// One connection ensures the same fakeConn instance is reused across calls.
	db.SetMaxOpenConns(1)
	t.Cleanup(func() {
		_ = db.Close()
		controllersMu.Lock()
		delete(controllers, name)
		controllersMu.Unlock()
	})
	return db
}

// Tests: transaction-level error branches in E50BackfillLatencyPhases

// TestE50Backfill_RowScanError exercises backfill.go:64-67: QueryContext
// returns rows but Scan fails due to a column-count mismatch (the real SQLite
// schema has 3 columns; fakeRows declares 1). The function must close the rows
// cursor and return a "scan row:" wrapped error.
func TestE50Backfill_RowScanError(t *testing.T) {
	db := openFakeDB(t, "scan-err", func() *fakeConn {
		return &fakeConn{queryRows: &fakeRows{}}
	})

	err := E50BackfillLatencyPhases(context.Background(), db, slog.Default())
	if err == nil {
		t.Fatal("expected scan-row error; got nil")
	}
	const wantSubstr = "scan row"
	if !containsSubstr(err.Error(), wantSubstr) {
		t.Errorf("error %q must contain %q", err.Error(), wantSubstr)
	}
}

// TestE50Backfill_BeginTxError exercises backfill.go:76-78: the batch scan
// succeeds (threeColRows) but db.BeginTx fails. The function must return a
// "begin backfill tx:" wrapped error.
func TestE50Backfill_BeginTxError(t *testing.T) {
	beginErr := errors.New("db: begin tx failed")
	db := openFakeDB(t, "begin-tx-err", func() *fakeConn {
		return &fakeConn{
			queryRows: &threeColRows{},
			beginErr:  beginErr,
		}
	})

	err := E50BackfillLatencyPhases(context.Background(), db, slog.Default())
	if err == nil {
		t.Fatal("expected begin-tx error; got nil")
	}
	if !errors.Is(err, beginErr) {
		t.Errorf("error chain must contain beginErr; got: %v", err)
	}
}

// TestE50Backfill_ExecContextError exercises backfill.go:90-93: the
// transaction starts but ExecContext (UPDATE audit_events) fails. The function
// must roll back the transaction and return an "update row:" wrapped error.
func TestE50Backfill_ExecContextError(t *testing.T) {
	execErr := errors.New("db: exec failed")
	db := openFakeDB(t, "exec-err", func() *fakeConn {
		return &fakeConn{
			queryRows: &threeColRows{},
			execErr:   execErr,
		}
	})

	err := E50BackfillLatencyPhases(context.Background(), db, slog.Default())
	if err == nil {
		t.Fatal("expected exec error; got nil")
	}
	if !errors.Is(err, execErr) {
		t.Errorf("error chain must contain execErr; got: %v", err)
	}
}

// TestE50Backfill_CommitError exercises backfill.go:95-97: all UPDATE
// statements execute successfully but Commit fails. The function must return a
// "commit backfill tx:" wrapped error.
func TestE50Backfill_CommitError(t *testing.T) {
	commitErr := errors.New("db: commit failed")
	db := openFakeDB(t, "commit-err", func() *fakeConn {
		return &fakeConn{
			queryRows: &threeColRows{},
			commitErr: commitErr,
		}
	})

	err := E50BackfillLatencyPhases(context.Background(), db, slog.Default())
	if err == nil {
		t.Fatal("expected commit error; got nil")
	}
	if !errors.Is(err, commitErr) {
		t.Errorf("error chain must contain commitErr; got: %v", err)
	}
}

func containsSubstr(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

package diag

import (
	"context"
	"database/sql"
	"fmt"
	"github.com/goccy/go-json"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
)

// pendingDiagEventSchema is the SQLCipher / SQLite schema for the agent's
// crash-safe DiagEvent buffer. Rows persist FATAL events ahead of the
// re-panic so the next agent start can drain them to Hub via the HTTP
// /api/internal/things/diag-events:batch endpoint.
//
// The id column is a TEXT UUID (not BLOB) because the wire-format
// DiagDrainEvent.ID is a string and we want a 1:1 mapping between the
// SQLite row id and the field the Hub acks. Using TEXT also keeps SQLite's
// default ORDER BY semantics intuitive when debugging.
const pendingDiagEventSchema = `
CREATE TABLE IF NOT EXISTS pending_diag_event (
    id          TEXT PRIMARY KEY,
    occurred_at TEXT NOT NULL,
    payload     BLOB NOT NULL,
    attempts    INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_pending_diag_occurred ON pending_diag_event(occurred_at);
`

// MigratePendingDiagEvent applies the pending_diag_event schema to db. The
// statements use IF NOT EXISTS so re-runs on an existing database are
// no-ops. Callers (the agent's queue bootstrapper or a test helper) hold
// the *sql.DB; this function does not own the handle.
func MigratePendingDiagEvent(db *sql.DB) error {
	if db == nil {
		return fmt.Errorf("MigratePendingDiagEvent: nil db")
	}
	if _, err := db.ExecContext(context.Background(), pendingDiagEventSchema); err != nil {
		return fmt.Errorf("create pending_diag_event: %w", err)
	}
	return nil
}

// LocalBuffer is the agent-side crash-safe queue of FATAL DiagEvents. The
// SlogSink writes to it via the LocalBufferInserter interface; the startup
// drain reads from it via List/Delete/IncrAttempts.
//
// All methods are goroutine-safe via *sql.DB's connection pool. There is no
// in-process state beyond the handle.
type LocalBuffer struct {
	db  *sql.DB
	log *slog.Logger
}

// NewLocalBuffer wraps an already-opened *sql.DB (the agent's audit
// SQLCipher handle is reused — see cmd/agent/main.go) so the diag buffer
// shares the same encryption key, journal mode, and connection pool. log
// may be nil.
func NewLocalBuffer(db *sql.DB, log *slog.Logger) *LocalBuffer {
	return &LocalBuffer{db: db, log: log}
}

// pendingDiagPayload is the JSON-serialized form persisted in payload BLOB.
// It carries the full DiagEvent so List can reconstruct the wire envelope
// unchanged, plus we never lose attrs/stackTrace/osInfo/repeatCount on a
// crash.
type pendingDiagPayload struct {
	registry.DiagEvent
}

// Insert persists a single DiagEvent into the pending buffer. The call is
// synchronous: the slog handler invokes it on the goroutine that triggered
// the FATAL log, and the panic-recovery path invokes it before re-panicking,
// so the row must hit the WAL before the process dies.
//
// On duplicate primary key (re-Insert with the same id) the call is a
// no-op — the existing row is preserved.
func (b *LocalBuffer) Insert(evt registry.DiagEvent) error {
	if b == nil || b.db == nil {
		return fmt.Errorf("LocalBuffer.Insert: nil buffer")
	}

	id := uuid.NewString()
	occurredAt := evt.OccurredAt
	if occurredAt.IsZero() {
		occurredAt = time.Now().UTC()
	}

	payload, err := json.Marshal(pendingDiagPayload{DiagEvent: evt})
	if err != nil {
		return fmt.Errorf("marshal diag payload: %w", err)
	}

	_, err = b.db.ExecContext(context.Background(), `
		INSERT OR IGNORE INTO pending_diag_event (id, occurred_at, payload, attempts)
		VALUES (?, ?, ?, 0)`,
		id, occurredAt.UTC().Format(time.RFC3339Nano), payload,
	)
	if err != nil {
		return fmt.Errorf("insert pending diag: %w", err)
	}
	return nil
}

// PendingDiagRow is the partially-decoded result of List: the local row id
// is exposed alongside the embedded DiagEvent so the drain loop can build
// DiagDrainEvent envelopes without re-reading the table.
type PendingDiagRow struct {
	ID string
	registry.DiagEvent
}

// List returns up to limit rows ordered by occurred_at ASC. The drain loop
// uses ASC order so the oldest crash event ships first — useful when the
// buffer accumulated many panics across restarts.
func (b *LocalBuffer) List(limit int) ([]PendingDiagRow, error) {
	if b == nil || b.db == nil {
		return nil, fmt.Errorf("LocalBuffer.List: nil buffer")
	}
	if limit <= 0 {
		limit = 100
	}

	rows, err := b.db.QueryContext(context.Background(), `
		SELECT id, payload
		FROM pending_diag_event
		ORDER BY occurred_at ASC, id ASC
		LIMIT ?`, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("query pending diag: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	out := make([]PendingDiagRow, 0, limit)
	for rows.Next() {
		var (
			id      string
			payload []byte
		)
		if err := rows.Scan(&id, &payload); err != nil {
			return nil, fmt.Errorf("scan pending diag: %w", err)
		}
		var p pendingDiagPayload
		if err := json.Unmarshal(payload, &p); err != nil {
			// Corrupt row — log and skip rather than aborting the drain.
			if b.log != nil {
				b.log.Warn("skip corrupt pending_diag_event row", "id", id, "error", err)
			}
			continue
		}
		out = append(out, PendingDiagRow{ID: id, DiagEvent: p.DiagEvent})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate pending diag: %w", err)
	}
	return out, nil
}

// Delete removes rows by id. Callers pass the id string returned by List.
// Empty/nil id slices are a no-op (matches Queue.MarkSynced semantics).
func (b *LocalBuffer) Delete(ids []string) error {
	if b == nil || b.db == nil {
		return fmt.Errorf("LocalBuffer.Delete: nil buffer")
	}
	if len(ids) == 0 {
		return nil
	}

	placeholders := strings.Repeat("?,", len(ids))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	_, err := b.db.ExecContext(context.Background(), "DELETE FROM pending_diag_event WHERE id IN ("+placeholders+")", args...)
	if err != nil {
		return fmt.Errorf("delete pending diag: %w", err)
	}
	return nil
}

// IncrAttempts increments the attempts counter for the given ids. Used by
// the drain loop when Hub returns an empty acceptedIds list — surfaces the
// stalled rows for diagnostics without removing them.
func (b *LocalBuffer) IncrAttempts(ids []string) error {
	if b == nil || b.db == nil {
		return fmt.Errorf("LocalBuffer.IncrAttempts: nil buffer")
	}
	if len(ids) == 0 {
		return nil
	}

	placeholders := strings.Repeat("?,", len(ids))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	_, err := b.db.ExecContext(context.Background(),
		"UPDATE pending_diag_event SET attempts = attempts + 1 WHERE id IN ("+placeholders+")",
		args...,
	)
	if err != nil {
		return fmt.Errorf("incr attempts: %w", err)
	}
	return nil
}

// Pending returns the row count. Used by health/diagnostic surfaces.
func (b *LocalBuffer) Pending() (int, error) {
	if b == nil || b.db == nil {
		return 0, fmt.Errorf("LocalBuffer.Pending: nil buffer")
	}
	var n int
	if err := b.db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM pending_diag_event").Scan(&n); err != nil {
		return 0, fmt.Errorf("count pending diag: %w", err)
	}
	return n, nil
}

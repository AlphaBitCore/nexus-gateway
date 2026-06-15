package consumer

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// insertNormalizedPayloads writes the sidecar rows for events whose
// producer attached at least one normalized field (request or response).
// Rows with no normalize fields at all are skipped — the FK to
// traffic_event guarantees a parent row exists, but there is no value
// in storing an all-null sidecar.
//
// Durability contract (audit-pipeline architecture §8 / §10.2): the raw
// traffic_event + traffic_event_payload rows MUST survive even if a normalized
// row fails to insert. To guarantee that, the whole sidecar phase runs inside a
// SAVEPOINT (a pgx nested transaction). A statement failure inside the
// savepoint aborts only that subtransaction; ROLLBACK TO SAVEPOINT restores the
// outer tx to a committable state, so the billing/audit rows are never lost to
// a sidecar error. A single pgx.Batch on the OUTER tx (the previous
// implementation) would instead abort the whole batch tx, forcing a rollback of
// every raw row — exactly the bug this guards against.
//
// Fast path: one pipelined pgx.Batch under one savepoint (the common case has
// no failures, so this stays one round-trip of inserts). On any batch error the
// savepoint is rolled back (raw rows safe) and the sidecar is retried
// row-by-row, each in its OWN savepoint, so a single poison row (jsonb
// null-character 22P05 / 22021) is skipped without stranding the rest of the
// batch's sidecars. A non-poison row failure is recorded in the returned error
// (logged + counted by the caller); it still does not roll the raw rows.
func (w *TrafficEventWriter) insertNormalizedPayloads(ctx context.Context, tx pgx.Tx, items []pendingTrafficMessage) error {
	rows := make([]TrafficEventMessage, 0, len(items))
	for _, pm := range items {
		e := pm.event
		if len(e.RequestNormalized) == 0 && len(e.ResponseNormalized) == 0 &&
			e.RequestNormalizeStatus == "" && e.ResponseNormalizeStatus == "" {
			continue
		}
		rows = append(rows, e)
	}
	if len(rows) == 0 {
		return nil
	}

	sp, err := tx.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin normalized savepoint: %w", err)
	}

	if batchErr := w.normalizedBatchInsert(ctx, sp, rows); batchErr == nil {
		// RELEASE SAVEPOINT — the sidecar rows are now part of the outer tx.
		if relErr := sp.Commit(ctx); relErr != nil {
			return fmt.Errorf("release normalized savepoint: %w", relErr)
		}
		return nil
	} else {
		// ROLLBACK TO SAVEPOINT — discards the failed batch, keeps the outer tx
		// (and its raw rows) committable. Then retry row-by-row to isolate the
		// offending row(s).
		if rbErr := sp.Rollback(ctx); rbErr != nil {
			w.logger.Warn("normalized batch savepoint rollback failed", "error", rbErr)
		}
		w.logger.Warn("normalized batch insert failed; retrying row-by-row to isolate poison rows",
			"error", batchErr, "count", len(rows))
	}

	return w.normalizedPerRowInsert(ctx, tx, rows)
}

// normalizedArgs builds the positional INSERT args for one sidecar row, shared
// by the batch and per-row paths so they bind identically.
func normalizedArgs(e TrafficEventMessage) []any {
	ver := e.NormalizeVersion
	if ver == "" {
		ver = "1"
	}
	return []any{
		e.ID,
		nullableJSON(stripNulJSON(e.RequestNormalized)),
		nullableJSON(stripNulJSON(e.ResponseNormalized)),
		nilIfEmpty(e.RequestNormalizeStatus),
		nilIfEmpty(e.ResponseNormalizeStatus),
		nilIfEmpty(e.RequestNormalizeError),
		nilIfEmpty(e.ResponseNormalizeError),
		ver,
		nullableJSON(stripNulJSON(e.RequestRedactionSpans)),
		nullableJSON(stripNulJSON(e.ResponseRedactionSpans)),
	}
}

// normalizedBatchInsert pipelines all sidecar rows in one pgx.Batch. tx here is
// the savepoint (nested) transaction, so an error leaves the outer tx
// recoverable via ROLLBACK TO SAVEPOINT.
func (w *TrafficEventWriter) normalizedBatchInsert(ctx context.Context, tx pgx.Tx, rows []TrafficEventMessage) error {
	batch := &pgx.Batch{}
	for _, e := range rows {
		batch.Queue(insertNormalizedSQL, normalizedArgs(e)...)
	}
	br := tx.SendBatch(ctx, batch)
	defer br.Close() //nolint:errcheck
	for range rows {
		if _, err := br.Exec(); err != nil {
			return fmt.Errorf("exec normalized insert: %w", err)
		}
	}
	return nil
}

// normalizedPerRowInsert is the slow-path fallback: one savepoint per row so a
// single poison row (22P05 / 22021) is skipped while the rest still persist.
func (w *TrafficEventWriter) normalizedPerRowInsert(ctx context.Context, tx pgx.Tx, rows []TrafficEventMessage) error {
	var firstErr error
	for _, e := range rows {
		sp, err := tx.Begin(ctx)
		if err != nil {
			// Cannot open a savepoint — the outer tx is unusable; stop.
			if firstErr == nil {
				firstErr = fmt.Errorf("begin normalized savepoint: %w", err)
			}
			return firstErr
		}

		_, execErr := sp.Exec(ctx, insertNormalizedSQL, normalizedArgs(e)...)
		if execErr != nil {
			if rbErr := sp.Rollback(ctx); rbErr != nil {
				w.logger.Warn("normalized savepoint rollback failed", "id", e.ID, "error", rbErr)
			}
			if isJSONNulPoison(execErr) {
				w.logger.Warn("normalized sidecar poison row skipped (jsonb null character)",
					"id", e.ID, "error", execErr)
				if w.errorsTotal != nil {
					w.errorsTotal.With("db_insert_normalized_poison").Inc()
				}
				continue
			}
			if firstErr == nil {
				firstErr = fmt.Errorf("insert normalized row %s: %w", e.ID, execErr)
			}
			continue
		}

		if relErr := sp.Commit(ctx); relErr != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("release normalized savepoint %s: %w", e.ID, relErr)
			}
		}
	}
	return firstErr
}

const insertNormalizedSQL = `
INSERT INTO traffic_event_normalized (
    traffic_event_id,
    request_normalized, response_normalized,
    request_status, response_status,
    request_error_reason, response_error_reason,
    normalize_version,
    request_redaction_spans, response_redaction_spans
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT (traffic_event_id) DO NOTHING
`

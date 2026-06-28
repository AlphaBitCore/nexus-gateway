// Package backfill provides the latency-phase backfill for the agent's
// local SQLite audit_events table.
package backfill

import (
	"context"
	"database/sql"
	"fmt"
	"github.com/goccy/go-json"
	"log/slog"
)

// E50BackfillLatencyPhases reconstructs request_hooks_ms / response_hooks_ms /
// upstream_total_ms on rows that are missing these fields in the agent's local
// audit_events table. Idempotent and resumable: rows already populated are
// skipped via the `WHERE request_hooks_ms IS NULL OR upstream_total_ms IS NULL`
// predicate.
//
// Designed to run once at agent boot. The caller (cmd/agent/main.go) gates
// this on a `_e50_backfill_done` marker row in the lifecycle_event table so
// subsequent boots skip the scan.
//
// Reconstruction rules:
//
//   - request_hooks_ms  = sum of per-hook latencyMs in hooks_pipeline JSON
//     (request-stage entries)
//   - response_hooks_ms = same for response-stage entries
//   - upstream_total_ms = max(0, duration_ms - request_hooks_ms - response_hooks_ms)
//   - upstream_ttfb_ms  NOT reconstructed (not in older rows)
//   - latency_breakdown NOT written (no long-tail data in older rows)
//
// On Linux/Windows where hooks_pipeline was not persisted in older builds,
// the hook aggregates remain NULL and only upstream_total_ms gets the
// residual estimate.
//
// The db argument should be obtained from queue.Queue.DB(). The Queue.DB()
// accessor exposes the underlying *sql.DB so this package-external function
// can drive the queries without importing the Queue struct directly.
func E50BackfillLatencyPhases(ctx context.Context, db *sql.DB, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	const batchSize = 500
	totalDone := 0

	for {
		rows, err := db.QueryContext(ctx, `
			SELECT id, duration_ms, hooks_pipeline
			FROM   audit_events
			WHERE  request_hooks_ms IS NULL OR upstream_total_ms IS NULL
			LIMIT  ?`, batchSize)
		if err != nil {
			return fmt.Errorf("scan audit_events for backfill: %w", err)
		}

		type pending struct {
			id           string
			durationMs   sql.NullInt64
			hooksRawJSON sql.NullString
		}
		var batch []pending
		for rows.Next() {
			var p pending
			if err := rows.Scan(&p.id, &p.durationMs, &p.hooksRawJSON); err != nil {
				_ = rows.Close()
				return fmt.Errorf("scan row: %w", err)
			}
			batch = append(batch, p)
		}
		_ = rows.Close()
		if len(batch) == 0 {
			break
		}

		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin backfill tx: %w", err)
		}
		for _, p := range batch {
			reqMs, respMs := sumStageLatenciesFromBlob(p.hooksRawJSON)
			upstreamTotal := computeResidualUpstream(p.durationMs, reqMs, respMs)
			if _, err := tx.ExecContext(ctx, `
				UPDATE audit_events
				SET    request_hooks_ms  = COALESCE(request_hooks_ms,  ?),
				       response_hooks_ms = COALESCE(response_hooks_ms, ?),
				       upstream_total_ms = COALESCE(upstream_total_ms, ?)
				WHERE  id = ?`,
				nullableIntFromPtr(reqMs), nullableIntFromPtr(respMs),
				nullableIntFromPtr(upstreamTotal), p.id,
			); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("update row %s: %w", p.id, err)
			}
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit backfill tx: %w", err)
		}
		totalDone += len(batch)
		logger.Info("e50 latency backfill progress", "batch", len(batch), "total", totalDone)

		if len(batch) < batchSize {
			break
		}
	}

	logger.Info("e50 latency backfill complete", "rows_processed", totalDone)
	return nil
}

// sumStageLatenciesFromBlob parses the hooks_pipeline JSON blob and sums
// per-hook latencyMs by stage. Returns (request-side, response-side) — each
// nil when no hook of that stage was present (so the resulting column
// stays NULL rather than 0).
func sumStageLatenciesFromBlob(raw sql.NullString) (*int, *int) {
	if !raw.Valid || raw.String == "" {
		return nil, nil
	}
	var rows []struct {
		Stage     string `json:"stage"`
		LatencyMs int    `json:"latencyMs"`
	}
	if err := json.Unmarshal([]byte(raw.String), &rows); err != nil {
		return nil, nil
	}
	if len(rows) == 0 {
		return nil, nil
	}
	var req, resp int
	var reqSeen, respSeen bool
	for _, r := range rows {
		switch r.Stage {
		case "request", "connection":
			reqSeen = true
			if r.LatencyMs > 0 {
				req += r.LatencyMs
			}
		case "response":
			respSeen = true
			if r.LatencyMs > 0 {
				resp += r.LatencyMs
			}
		}
	}
	var reqOut, respOut *int
	if reqSeen {
		reqOut = &req
	}
	if respSeen {
		respOut = &resp
	}
	return reqOut, respOut
}

// computeResidualUpstream applies the agent-side residual estimate:
// max(0, duration - req_hooks - resp_hooks). Returns nil when duration is
// NULL so the column stays NULL (no honest estimate available).
func computeResidualUpstream(duration sql.NullInt64, reqHooks, respHooks *int) *int {
	if !duration.Valid {
		return nil
	}
	total := int(duration.Int64)
	if reqHooks != nil {
		total -= *reqHooks
	}
	if respHooks != nil {
		total -= *respHooks
	}
	if total < 0 {
		total = 0
	}
	return &total
}

// nullableIntFromPtr produces a sql-driver-friendly value: nil for NULL or
// the underlying int otherwise. Mirrors the local `nullableInt(p *int)`
// helper in queue.go (kept duplicate to avoid widening that file's
// non-test export surface).
func nullableIntFromPtr(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

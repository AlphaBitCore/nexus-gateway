package helpers

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	poolOnce sync.Once
	pool     *pgxpool.Pool
	poolErr  error
)

// DB returns a process-wide pgxpool.Pool against the dev Postgres. The
// pool is built lazily on first call so tests that don't touch the DB
// don't pay the connection cost. Tests should NOT close it — process
// teardown is fine.
func DB(ctx context.Context, env *Env) (*pgxpool.Pool, error) {
	poolOnce.Do(func() {
		cfg, err := pgxpool.ParseConfig(env.PGDSN())
		if err != nil {
			poolErr = fmt.Errorf("parse DSN: %w", err)
			return
		}
		cfg.MaxConns = 4
		cfg.MaxConnLifetime = 5 * time.Minute
		pool, poolErr = pgxpool.NewWithConfig(ctx, cfg)
	})
	return pool, poolErr
}

// CountQuery executes a "SELECT count(*) FROM ..." style query and
// returns the scalar.
func CountQuery(ctx context.Context, db *pgxpool.Pool, sql string, args ...any) (int64, error) {
	var n int64
	if err := db.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// AuditEventRow is the subset of traffic_event columns Phase 2 tests
// inspect. We deliberately keep this narrow — pulling the full row would
// couple every test to migrations of unrelated columns.
type AuditEventRow struct {
	ID                   string
	Source               string
	Path                 string
	StatusCode           int32
	ModelName            string
	RequestHookDecision  string
	ResponseHookDecision string
	Timestamp            time.Time
}

// WaitForRecentAuditEvent polls traffic_event until a row matching the
// predicate appears or the deadline elapses. The audit pipeline in dev
// is async (gateway → MQ → Hub consumer → DB) with typical latency ~10 s
// and worst-case spikes around 30 s, which is why every poll site in the
// test program uses a 45 s deadline by default. Returning (nil, nil) on
// timeout (rather than an error) keeps "no row yet" callable from a
// `if row == nil { ... }` branch.
func WaitForRecentAuditEvent(
	ctx context.Context,
	db *pgxpool.Pool,
	wherePredicate string,
	args []any,
	deadline time.Duration,
) (*AuditEventRow, error) {
	stopAt := time.Now().Add(deadline)
	q := `
		SELECT id, source, path, status_code, model_name,
		       request_hook_decision, response_hook_decision, "timestamp"
		FROM traffic_event
		WHERE "timestamp" > NOW() - INTERVAL '120 seconds'
		  AND ` + wherePredicate + `
		ORDER BY "timestamp" DESC LIMIT 1`
	for {
		var row AuditEventRow
		var modelName, reqDec, respDec *string
		err := db.QueryRow(ctx, q, args...).Scan(
			&row.ID, &row.Source, &row.Path, &row.StatusCode,
			&modelName, &reqDec, &respDec, &row.Timestamp,
		)
		switch {
		case err == nil:
			if modelName != nil {
				row.ModelName = *modelName
			}
			if reqDec != nil {
				row.RequestHookDecision = *reqDec
			}
			if respDec != nil {
				row.ResponseHookDecision = *respDec
			}
			return &row, nil
		case err == pgx.ErrNoRows:
			// fall through to retry
		default:
			return nil, fmt.Errorf("traffic_event poll: %w", err)
		}
		if time.Now().After(stopAt) {
			return nil, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

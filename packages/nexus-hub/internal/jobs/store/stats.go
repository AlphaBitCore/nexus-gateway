package jobstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// JobWithStats combines the `job` definition row with aggregate statistics
// derived from `job_run`. RunCount and ErrorCount are lifetime counts bounded
// only by the retention job; LastRun / LastStatus / LastDurationMs / LastError
// reflect the most recent run irrespective of status.
type JobWithStats struct {
	JobRecord
	LastRun        *time.Time
	LastFinishedAt *time.Time
	LastStatus     string
	LastDurationMs *int
	LastError      string
	RunCount       int64
	ErrorCount     int64
}

// ListJobsWithStats returns every job definition joined with its aggregates
// in a single query. Ordered by job id for stable UI listings.
func (s *Store) ListJobsWithStats(ctx context.Context) ([]JobWithStats, error) {
	rows, err := s.db.Query(ctx, `
		SELECT
			j."id", j."name", j."description", j."intervalSec", j."enabled",
			j."createdAt", j."updatedAt",
			last_run."startedAt",
			last_run."finishedAt",
			COALESCE(last_run."status", '') AS last_status,
			last_run."durationMs",
			COALESCE(last_run."error", '') AS last_error,
			COALESCE(agg.run_count, 0)   AS run_count,
			COALESCE(agg.error_count, 0) AS error_count
		FROM "job" j
		LEFT JOIN LATERAL (
			SELECT "startedAt", "finishedAt", "status", "durationMs", "error"
			FROM "job_run"
			WHERE "jobId" = j."id"
			ORDER BY "startedAt" DESC
			LIMIT 1
		) last_run ON TRUE
		LEFT JOIN (
			SELECT "jobId",
			       COUNT(*) FILTER (WHERE "status" IN ('success','error')) AS run_count,
			       COUNT(*) FILTER (WHERE "status" = 'error')              AS error_count
			FROM "job_run"
			GROUP BY "jobId"
		) agg ON agg."jobId" = j."id"
		ORDER BY j."id" ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("list jobs with stats: %w", err)
	}
	defer rows.Close()

	out := make([]JobWithStats, 0)
	for rows.Next() {
		var r JobWithStats
		if err := rows.Scan(
			&r.ID, &r.Name, &r.Description, &r.IntervalSec, &r.Enabled,
			&r.CreatedAt, &r.UpdatedAt,
			&r.LastRun, &r.LastFinishedAt, &r.LastStatus, &r.LastDurationMs, &r.LastError,
			&r.RunCount, &r.ErrorCount,
		); err != nil {
			return nil, fmt.Errorf("scan job stats: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetJobWithStats fetches a single job + its stats. Returns ErrNotFound if
// the id is unknown.
func (s *Store) GetJobWithStats(ctx context.Context, id string) (JobWithStats, error) {
	var r JobWithStats
	err := s.db.QueryRow(ctx, `
		SELECT
			j."id", j."name", j."description", j."intervalSec", j."enabled",
			j."createdAt", j."updatedAt",
			last_run."startedAt",
			last_run."finishedAt",
			COALESCE(last_run."status", '') AS last_status,
			last_run."durationMs",
			COALESCE(last_run."error", '') AS last_error,
			COALESCE(agg.run_count, 0)   AS run_count,
			COALESCE(agg.error_count, 0) AS error_count
		FROM "job" j
		LEFT JOIN LATERAL (
			SELECT "startedAt", "finishedAt", "status", "durationMs", "error"
			FROM "job_run"
			WHERE "jobId" = j."id"
			ORDER BY "startedAt" DESC
			LIMIT 1
		) last_run ON TRUE
		LEFT JOIN (
			SELECT "jobId",
			       COUNT(*) FILTER (WHERE "status" IN ('success','error')) AS run_count,
			       COUNT(*) FILTER (WHERE "status" = 'error')              AS error_count
			FROM "job_run"
			WHERE "jobId" = $1
			GROUP BY "jobId"
		) agg ON agg."jobId" = j."id"
		WHERE j."id" = $1
	`, id).Scan(
		&r.ID, &r.Name, &r.Description, &r.IntervalSec, &r.Enabled,
		&r.CreatedAt, &r.UpdatedAt,
		&r.LastRun, &r.LastFinishedAt, &r.LastStatus, &r.LastDurationMs, &r.LastError,
		&r.RunCount, &r.ErrorCount,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return JobWithStats{}, ErrNotFound
	}
	if err != nil {
		return JobWithStats{}, fmt.Errorf("get job stats %s: %w", id, err)
	}
	return r, nil
}

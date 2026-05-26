package jobstore

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Run status constants for the `status` column in job_run.
const (
	StatusRunning     = "running"
	StatusSuccess     = "success"
	StatusError       = "error"
	StatusSkipped     = "skipped"
	StatusInterrupted = "interrupted" // set by startup recovery for orphaned runs from a previous process
)

// JobRun is the row shape of the `job_run` table.
type JobRun struct {
	ID         string     `json:"id"`
	JobID      string     `json:"jobId"`
	StartedAt  time.Time  `json:"startedAt"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
	DurationMs *int       `json:"durationMs,omitempty"`
	Status     string     `json:"status"`
	Error      string     `json:"error,omitempty"`
	ReplicaID  string     `json:"replicaId,omitempty"`
}

// StartRun inserts a row with status='running' and returns the generated run
// ID. The scheduler passes this ID to FinishRun when the job completes so the
// same row is updated with duration + outcome. replicaID should be the pod /
// host identifier used for audit trails; empty string is allowed.
func (s *Store) StartRun(ctx context.Context, jobID, replicaID string) (string, error) {
	id := uuid.NewString()
	_, err := s.db.Exec(ctx, `
		INSERT INTO "job_run" ("id", "jobId", "startedAt", "status", "replicaId")
		VALUES ($1, $2, NOW(), $3, NULLIF($4, ''))
	`, id, jobID, StatusRunning, replicaID)
	if err != nil {
		return "", fmt.Errorf("start run %s: %w", jobID, err)
	}
	return id, nil
}

// FinishRun updates the row created by StartRun with the outcome. errMsg is
// stored only when status == StatusError; for success and skipped it is
// forced to empty so stale errors do not leak.
func (s *Store) FinishRun(ctx context.Context, runID string, status string, duration time.Duration, errMsg string) error {
	if status != StatusError {
		errMsg = ""
	}
	ms := int(duration / time.Millisecond)
	_, err := s.db.Exec(ctx, `
		UPDATE "job_run"
		SET "finishedAt" = NOW(),
		    "durationMs" = $1,
		    "status" = $2,
		    "error" = NULLIF($3, '')
		WHERE "id" = $4
	`, ms, status, errMsg, runID)
	if err != nil {
		return fmt.Errorf("finish run %s: %w", runID, err)
	}
	return nil
}

// ListRuns returns the most recent runs for a job, newest first, together
// with the total row count for the job (across all pages). Pagination is
// offset-based because run volume per job is bounded by the retention job.
// The total is computed via COUNT(*) OVER() in the same query to avoid a
// second round-trip; when the page is empty an explicit COUNT is issued so
// the caller still gets an accurate total.
func (s *Store) ListRuns(ctx context.Context, jobID string, limit, offset int) ([]JobRun, int, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := s.db.Query(ctx, `
		SELECT "id", "jobId", "startedAt", "finishedAt", "durationMs",
		       "status", COALESCE("error", ''), COALESCE("replicaId", ''),
		       COUNT(*) OVER() AS total
		FROM "job_run"
		WHERE "jobId" = $1
		ORDER BY "startedAt" DESC
		LIMIT $2 OFFSET $3
	`, jobID, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("list runs %s: %w", jobID, err)
	}
	defer rows.Close()

	out := make([]JobRun, 0)
	total := 0
	for rows.Next() {
		var r JobRun
		if err := rows.Scan(&r.ID, &r.JobID, &r.StartedAt, &r.FinishedAt, &r.DurationMs, &r.Status, &r.Error, &r.ReplicaID, &total); err != nil {
			return nil, 0, fmt.Errorf("scan run: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	if len(out) == 0 {
		// Window-function row was not produced (empty page). Fall back to a
		// dedicated count so pagination still renders "0 of N" correctly
		// when the user pages past the end.
		if err := s.db.QueryRow(ctx, `SELECT COUNT(*) FROM "job_run" WHERE "jobId" = $1`, jobID).Scan(&total); err != nil {
			return nil, 0, fmt.Errorf("count runs %s: %w", jobID, err)
		}
	}
	return out, total, nil
}

// RecoverStaleRuns marks every job_run row still in status='running' as
// StatusInterrupted. This is called once at startup by the scheduler
// instance so that orphaned rows from the previous process do not
// appear as perpetually running in the admin UI.
// Returns the number of rows updated.
func (s *Store) RecoverStaleRuns(ctx context.Context) (int64, error) {
	tag, err := s.db.Exec(ctx, `
		UPDATE "job_run"
		SET "status" = $1, "finishedAt" = NOW()
		WHERE "status" = $2
	`, StatusInterrupted, StatusRunning)
	if err != nil {
		return 0, fmt.Errorf("recover stale runs: %w", err)
	}
	return tag.RowsAffected(), nil
}

// PruneJobRuns deletes older rows per job, keeping only the keepN most recent
// (by startedAt DESC) for each jobId. Returns the number of rows removed.
// keepN <= 0 is treated as 100 to match the default retention policy.
func (s *Store) PruneJobRuns(ctx context.Context, keepN int) (int64, error) {
	if keepN <= 0 {
		keepN = 100
	}
	tag, err := s.db.Exec(ctx, `
		DELETE FROM "job_run"
		WHERE "id" IN (
			SELECT "id" FROM (
				SELECT "id",
				       ROW_NUMBER() OVER (PARTITION BY "jobId" ORDER BY "startedAt" DESC) AS rn
				FROM "job_run"
			) ranked
			WHERE ranked.rn > $1
		)
	`, keepN)
	if err != nil {
		return 0, fmt.Errorf("prune job runs: %w", err)
	}
	return tag.RowsAffected(), nil
}

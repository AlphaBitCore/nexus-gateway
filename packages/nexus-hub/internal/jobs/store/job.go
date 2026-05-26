package jobstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// JobRecord is the row shape of the `job` table.
type JobRecord struct {
	ID          string
	Name        string
	Description string
	IntervalSec int
	Enabled     bool
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// UpsertJob inserts or updates the definition row (name/description/interval).
// The `enabled` column is NOT touched on update — the DB row is the source of
// truth for the toggle, so restarting a replica must not clobber an admin's
// disable action. On first insert it defaults to true per the schema default.
func (s *Store) UpsertJob(ctx context.Context, id, name, description string, intervalSec int) error {
	_, err := s.db.Exec(ctx, `
		INSERT INTO "job" ("id", "name", "description", "intervalSec", "updatedAt")
		VALUES ($1, $2, $3, $4, NOW())
		ON CONFLICT ("id") DO UPDATE SET
			"name" = EXCLUDED."name",
			"description" = EXCLUDED."description",
			"intervalSec" = EXCLUDED."intervalSec",
			"updatedAt" = NOW()
	`, id, name, description, intervalSec)
	if err != nil {
		return fmt.Errorf("upsert job %s: %w", id, err)
	}
	return nil
}

// GetEnabled returns the stored enabled flag. Returns ErrNotFound if the job
// has never been upserted.
func (s *Store) GetEnabled(ctx context.Context, id string) (bool, error) {
	var enabled bool
	err := s.db.QueryRow(ctx, `SELECT "enabled" FROM "job" WHERE "id" = $1`, id).Scan(&enabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, fmt.Errorf("get enabled %s: %w", id, err)
	}
	return enabled, nil
}

// SetEnabled flips the persisted enabled flag. Returns ErrNotFound if the id
// is unknown.
func (s *Store) SetEnabled(ctx context.Context, id string, enabled bool) error {
	tag, err := s.db.Exec(ctx, `UPDATE "job" SET "enabled" = $1, "updatedAt" = NOW() WHERE "id" = $2`, enabled, id)
	if err != nil {
		return fmt.Errorf("set enabled %s: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Get fetches the full definition row.
func (s *Store) Get(ctx context.Context, id string) (JobRecord, error) {
	var r JobRecord
	err := s.db.QueryRow(ctx, `
		SELECT "id", "name", "description", "intervalSec", "enabled", "createdAt", "updatedAt"
		FROM "job" WHERE "id" = $1
	`, id).Scan(&r.ID, &r.Name, &r.Description, &r.IntervalSec, &r.Enabled, &r.CreatedAt, &r.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return JobRecord{}, ErrNotFound
	}
	if err != nil {
		return JobRecord{}, fmt.Errorf("get job %s: %w", id, err)
	}
	return r, nil
}

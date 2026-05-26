package opsmetrics

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	opsmetrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
)

// staticInfoExec is the minimum pgx surface StaticInfoWriter needs (Exec
// only, for the jsonb_set UPDATE). The concrete *pgxpool.Pool satisfies it
// in production; pgxmock's PgxPoolIface satisfies it in unit tests so the
// success / row-missing / transient-err branches can be driven without a
// live PostgreSQL. Mirrors the CopyPool seam pattern above.
type staticInfoExec interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// StaticInfoWriter persists L2 static-identity payloads into
// thing.metadata.staticInfo via jsonb_set, leaving sibling metadata keys
// (deviceTokenHash, diagModeUntil, …) untouched.
//
// Spec §5.6 puts the staticInfo sub-object on the existing thing.metadata
// JSONB column; this writer is the only Hub-side place that mutates it.
type StaticInfoWriter struct {
	pool staticInfoExec
}

// NewStaticInfoWriter returns a StaticInfoWriter bound to the given pool.
// A nil pool is accepted and produces a no-op writer — UpsertStaticInfo
// returns "static info writer not configured" without panicking. Wiring
// callers in DB-less unit-test setups rely on this; the production path
// always supplies a non-nil pool.
func NewStaticInfoWriter(pool *pgxpool.Pool) *StaticInfoWriter {
	if pool == nil {
		return newStaticInfoWriterWithPool(nil)
	}
	return newStaticInfoWriterWithPool(pool)
}

// newStaticInfoWriterWithPool is the internal constructor that accepts the
// staticInfoExec interface so tests can inject a pgxmock pool via the
// test-only seam.
func newStaticInfoWriterWithPool(pool staticInfoExec) *StaticInfoWriter {
	return &StaticInfoWriter{pool: pool}
}

// UpsertStaticInfo writes info under metadata.staticInfo for the given
// thing.id. Returns ErrNotFound when the row does not exist (so the WS
// handler can log + drop without spamming on transient mis-routes).
func (w *StaticInfoWriter) UpsertStaticInfo(ctx context.Context, thingID string, info opsmetrics.StaticInfo) error {
	if w == nil || w.pool == nil {
		return fmt.Errorf("opsmetrics: static info writer not configured")
	}
	if thingID == "" {
		return fmt.Errorf("opsmetrics: empty thingID")
	}
	payload, err := json.Marshal(info)
	if err != nil {
		return fmt.Errorf("marshal static info: %w", err)
	}
	const q = `
		UPDATE thing
		SET metadata   = jsonb_set(COALESCE(metadata, '{}'::jsonb), '{staticInfo}', $2::jsonb, true),
		    updated_at = NOW()
		WHERE id = $1
	`
	tag, err := w.pool.Exec(ctx, q, thingID, payload)
	if err != nil {
		return fmt.Errorf("update static info: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("opsmetrics: thing %q not found", thingID)
	}
	return nil
}

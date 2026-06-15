package hub

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ledgerKeyPrefix namespaces the per-(thingType, configKey) propagation rows
// inside the shared system_metadata table. Using the existing durable KV table
// (one row per tracked key, PRIMARY KEY on `key`) keeps the backstop crash- and
// restart-safe without a dedicated migration, and the per-row UPSERT makes
// concurrent admin writes to different keys collision-free.
const ledgerKeyPrefix = "propagation_ledger:"

// LedgerPgxPool is the minimal pgx surface the propagation ledger needs.
// *pgxpool.Pool satisfies it directly; pgxmock satisfies it in tests.
type LedgerPgxPool interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Ledger is the Control Plane's durable record of which security-sensitive
// Category-B config keys it intends the data plane to have, versus which it has
// confirmed Hub accepted. It is the version-based reconcile backstop for
// A Category-B push (credentials / virtual_keys / routing_rules /
// quota_* / providers / models) carries no full state into thing.desired
// (the gateway reloads from the CP DB on a bare version bump), so the
// content-diff reconcile loop (configreconcile) structurally cannot heal it.
//
// Instead, every Category-B write bumps the key's intended sequence before the
// push and stamps the acked sequence only after Hub confirms the push. When a
// push fails (Hub restart/blip during the admin save) the row is left with
// acked < intended, and ReconcilePending re-pushes it on the next tick — so a
// missed push self-heals without the admin retrying, closing the silent-
// staleness loop that the 502 escalation surfaced but did not fully shut.
type Ledger struct {
	pool LedgerPgxPool
}

// NewLedger constructs a Ledger over the supplied pool. A nil pool yields a nil
// Ledger so callers can treat "no DB" as "no backstop" uniformly.
func NewLedger(pool LedgerPgxPool) *Ledger {
	if pool == nil {
		return nil
	}
	return &Ledger{pool: pool}
}

// PendingKey is one (thingType, configKey) whose last push was not confirmed.
type PendingKey struct {
	ThingType   string
	ConfigKey   string
	IntendedSeq int64
}

func ledgerKey(thingType, configKey string) string {
	return ledgerKeyPrefix + thingType + ":" + configKey
}

// RecordIntent atomically increments the intended sequence for (thingType,
// configKey) and returns the new value. It is called immediately before a
// Category-B push so the row reflects the admin's intent even if the push then
// fails. The increment runs entirely server-side (no read-modify-write race)
// so concurrent writes to the same key produce distinct, monotonic sequences.
func (l *Ledger) RecordIntent(ctx context.Context, thingType, configKey string) (int64, error) {
	if l == nil {
		return 0, nil
	}
	var seq int64
	err := l.pool.QueryRow(ctx, `
		INSERT INTO system_metadata (key, value, updated_at, updated_by)
		VALUES ($1, jsonb_build_object('thingType', $2::text, 'configKey', $3::text, 'intended', 1, 'acked', 0), NOW(), 'cp-propagation')
		ON CONFLICT (key) DO UPDATE SET
			value = jsonb_set(
				system_metadata.value,
				'{intended}',
				to_jsonb(COALESCE((system_metadata.value->>'intended')::bigint, 0) + 1)
			),
			updated_at = NOW()
		RETURNING (value->>'intended')::bigint
	`, ledgerKey(thingType, configKey), thingType, configKey).Scan(&seq)
	if err != nil {
		return 0, fmt.Errorf("propagation ledger: record intent %s/%s: %w", thingType, configKey, err)
	}
	return seq, nil
}

// MarkAcked records that the push for (thingType, configKey) reached at least
// `seq`. The GREATEST guard keeps acked monotonic so a slow reconcile re-push
// confirming an older sequence can never regress past a newer admin write.
func (l *Ledger) MarkAcked(ctx context.Context, thingType, configKey string, seq int64) error {
	if l == nil {
		return nil
	}
	_, err := l.pool.Exec(ctx, `
		UPDATE system_metadata
		SET value = jsonb_set(value, '{acked}', to_jsonb(GREATEST(COALESCE((value->>'acked')::bigint, 0), $2::bigint))),
			updated_at = NOW()
		WHERE key = $1
	`, ledgerKey(thingType, configKey), seq)
	if err != nil {
		return fmt.Errorf("propagation ledger: mark acked %s/%s: %w", thingType, configKey, err)
	}
	return nil
}

// ListPending returns every tracked key whose acked sequence lags its intended
// sequence — i.e. whose last push was never confirmed by Hub.
func (l *Ledger) ListPending(ctx context.Context) ([]PendingKey, error) {
	if l == nil {
		return nil, nil
	}
	rows, err := l.pool.Query(ctx, `
		SELECT value->>'thingType', value->>'configKey', (value->>'intended')::bigint
		FROM system_metadata
		WHERE key LIKE $1
		  AND COALESCE((value->>'intended')::bigint, 0) > COALESCE((value->>'acked')::bigint, 0)
	`, ledgerKeyPrefix+"%")
	if err != nil {
		return nil, fmt.Errorf("propagation ledger: list pending: %w", err)
	}
	defer rows.Close()

	var out []PendingKey
	for rows.Next() {
		var p PendingKey
		if err := rows.Scan(&p.ThingType, &p.ConfigKey, &p.IntendedSeq); err != nil {
			return nil, fmt.Errorf("propagation ledger: scan pending: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("propagation ledger: iterate pending: %w", err)
	}
	return out, nil
}

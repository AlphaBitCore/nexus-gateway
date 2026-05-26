package alerting

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// RaiserPool is the minimum pool surface the Raiser needs: it must support
// QueryRow (used for the device-group filter check) and Begin so pgx.BeginFunc
// can wrap the row-locking transaction. *pgxpool.Pool satisfies it in
// production; pgxmock's PgxPoolIface satisfies it in unit tests so the full
// Raise path can be exercised without a live Postgres.
type RaiserPool interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Begin(ctx context.Context) (pgx.Tx, error)
}

// RaiseInput is the payload that callers hand to Raiser.Raise.
//
// The Raiser resolves the rule by RuleID, enforces the target dedup window,
// persists either an INSERT (first firing) or UPDATE (duplicate), and — on
// a fresh INSERT — hands the alert to the Dispatcher.
type RaiseInput struct {
	RuleID      string
	TargetKey   string
	TargetLabel string
	Severity    Severity
	Message     string
	Details     map[string]any
	// FiredAt is optional; when zero the Raiser stamps time.Now().UTC().
	FiredAt time.Time
}

// Dispatcher delivers a fired alert to configured channels. The Raiser calls
// Dispatch asynchronously (in a new goroutine with a detached context) so a
// slow or failing dispatch never blocks alert persistence.
type Dispatcher interface {
	Dispatch(ctx context.Context, a Alert)
}

// Raiser owns the Raise/Resolve lifecycle for alerts. It serialises
// concurrent Raise calls for the same (rule, target) through a row-locking
// transaction so the dedup invariant ("at most one FIRING per rule+target")
// is preserved.
type Raiser struct {
	pool       RaiserPool
	store      *Store
	dispatcher Dispatcher
	logger     *slog.Logger
}

// NewRaiser constructs a Raiser. The dispatcher may be nil for tests that
// only care about persistence. Fired alerts reach configured channels
// (Slack/webhook/email) synchronously via the in-process Dispatcher —
// there is no MQ fan-out on this path.
func NewRaiser(pool *pgxpool.Pool, store *Store, d Dispatcher, logger *slog.Logger) *Raiser {
	return newRaiserWithPool(pool, store, d, logger)
}

// NewRaiserWithPool is the test-only constructor accepting any RaiserPool
// (typically pgxmock.PgxPoolIface). Production code goes through NewRaiser.
func NewRaiserWithPool(pool RaiserPool, store *Store, d Dispatcher, logger *slog.Logger) *Raiser {
	return newRaiserWithPool(pool, store, d, logger)
}

func newRaiserWithPool(pool RaiserPool, store *Store, d Dispatcher, logger *slog.Logger) *Raiser {
	if logger == nil {
		logger = slog.Default()
	}
	return &Raiser{pool: pool, store: store, dispatcher: d, logger: logger}
}

// Raise is the single entry point for producers that want to fire an alert.
// It is idempotent per (RuleID, TargetKey) while an alert is FIRING: repeat
// calls increment duplicateCount instead of creating a second row. Once the
// existing row transitions to ACKNOWLEDGED or RESOLVED, the next Raise
// creates a new row and triggers a new dispatch.
//
// A disabled rule is silently dropped (no row, no dispatch, no error).
// An unknown rule returns an error.
func (r *Raiser) Raise(ctx context.Context, in RaiseInput) error {
	if in.FiredAt.IsZero() {
		in.FiredAt = time.Now().UTC()
	}

	rule, err := r.store.GetRule(ctx, in.RuleID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return fmt.Errorf("raise: unknown ruleId %q", in.RuleID)
		}
		return fmt.Errorf("raise: load rule %q: %w", in.RuleID, err)
	}
	if !rule.Enabled {
		// Rule present but disabled — silently drop.
		return nil
	}

	// Per-group alert routing: when the rule carries a group_id_filter,
	// drop firings whose target isn't a member of that DeviceGroup.
	// Matches both static memberships (DeviceGroupMembership, respecting
	// expiry) and smart cache rows (device_group_membership_cache).
	// Target-key format for device alerts is `thing:<thingID>` (see
	// jobs/thing_offline_alerts.go); strip the prefix before matching.
	// Non-device alerts (quota, audit, system) with a group filter set
	// don't fire at all — interpreted as "only route this rule when it
	// has a device target", which is the intended semantics.
	if rule.GroupIDFilter != nil && *rule.GroupIDFilter != "" {
		deviceID := strings.TrimPrefix(in.TargetKey, "thing:")
		if deviceID == in.TargetKey {
			// No `thing:` prefix → not a device-target alert.
			return nil
		}
		var inGroup bool
		err := r.pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM "DeviceGroupMembership"
				WHERE "groupId" = $1 AND "deviceId" = $2
				  AND (expires_at IS NULL OR expires_at > NOW())
				UNION
				SELECT 1 FROM device_group_membership_cache
				WHERE group_id = $1 AND device_id = $2
			)
		`, *rule.GroupIDFilter, deviceID).Scan(&inGroup)
		if err != nil {
			return fmt.Errorf("raise: group filter check: %w", err)
		}
		if !inGroup {
			return nil
		}
	}

	var inserted *Alert
	err = pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		// Serialise concurrent Raise calls for the same (rule, target) even
		// when no prior row exists. A plain SELECT ... FOR UPDATE can only
		// lock existing rows, so multiple goroutines racing on a fresh
		// target would each see "no row" and INSERT. Advisory locks key off
		// an arbitrary string so they serialise even in that case; the lock
		// is scoped to the transaction and released automatically on
		// COMMIT/ROLLBACK.
		if _, err := tx.Exec(ctx,
			`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
			in.RuleID+":"+in.TargetKey,
		); err != nil {
			return fmt.Errorf("advisory lock: %w", err)
		}

		existing, err := r.store.FindLatestByRuleTargetAnyStateTx(ctx, tx, in.RuleID, in.TargetKey)
		if err != nil {
			return err
		}
		now := in.FiredAt

		// Suppress fresh INSERT + dispatch when EITHER:
		//   (a) a FIRING row already exists for this (rule, target) — dedup
		//       into that row so duplicateCount tracks repeat fires; OR
		//   (b) AlertRule.cooldownSec is set and the most recent fire — in
		//       any state including RESOLVED — happened within the cooldown
		//       window. Operators set cooldown to silence repeat noise even
		//       across an ack/resolve cycle, and the gate must survive
		//       process restarts; in-memory engine cooldown (alerteval) is
		//       a perf shortcut, this DB-backed check is the source of
		//       truth and covers state-poll jobs that bypass the engine.
		if existing != nil {
			inFiring := existing.State == StateFiring
			inCooldown := rule.CooldownSec > 0 &&
				now.Sub(existing.FiredAt) < time.Duration(rule.CooldownSec)*time.Second
			if inFiring || inCooldown {
				_, err := tx.Exec(ctx, `
					UPDATE "Alert"
					SET "lastSeenAt"     = $1,
					    "duplicateCount" = "duplicateCount" + 1
					WHERE id = $2`,
					now, existing.ID,
				)
				return err
			}
		}

		// No prior row, or the latest is past cooldown — insert fresh.
		details, err := json.Marshal(in.Details)
		if err != nil {
			return fmt.Errorf("marshal details: %w", err)
		}
		var id string
		err = tx.QueryRow(ctx, `
			INSERT INTO "Alert" (
				id, "ruleId", "sourceType", "targetKey", "targetLabel",
				severity, state, message, details,
				"firedAt", "lastSeenAt", "duplicateCount"
			) VALUES (
				gen_random_uuid()::text,
				$1,$2,$3,$4,
				$5::"AlertSeverity", 'FIRING'::"AlertState", $6, $7,
				$8,$9,1
			)
			RETURNING id`,
			in.RuleID, rule.SourceType, in.TargetKey, in.TargetLabel,
			dbSeverity(in.Severity), in.Message, details,
			now, now,
		).Scan(&id)
		if err != nil {
			return err
		}
		inserted = &Alert{
			ID:             id,
			RuleID:         in.RuleID,
			SourceType:     rule.SourceType,
			TargetKey:      in.TargetKey,
			TargetLabel:    in.TargetLabel,
			Severity:       in.Severity,
			State:          StateFiring,
			Message:        in.Message,
			Details:        in.Details,
			FiredAt:        now,
			LastSeenAt:     now,
			DuplicateCount: 1,
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("raise: persist alert: %w", err)
	}

	if inserted != nil && r.dispatcher != nil {
		go r.dispatcher.Dispatch(context.Background(), *inserted)
	}
	return nil
}

// Resolve clears all FIRING and ACKNOWLEDGED rows for the given (ruleID,
// targetKey). It is used by producers whose condition has cleared (e.g. a
// provider came back online) and by manual operator-driven resolves that
// target a whole alert stream rather than a single row.
func (r *Raiser) Resolve(ctx context.Context, ruleID, targetKey, reason string) error {
	n, err := r.store.ResolveByRuleTarget(ctx, ruleID, targetKey, reason)
	if err != nil {
		return fmt.Errorf("resolve: %w", err)
	}
	if n > 0 && r.logger != nil {
		r.logger.Info("alert resolved",
			"ruleId", ruleID,
			"targetKey", targetKey,
			"reason", reason,
			"rows", n,
		)
	}
	return nil
}

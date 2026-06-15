package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// UpdateShadowReport merges the reported state, advances reported_ver and
// merges the per-key reported_outcomes ledger. Outcomes nil/empty is allowed —
// older callers (or shadow_reports from a process whose OutcomeTracker hasn't
// recorded anything yet) result in an empty merge (no change), not a wipe of
// prior state. Hub-internal correlation with process_started_at distinguishes
// "fresh process, no apply yet" from "stale ledger".
//
// Per-key merge: both reported and reported_outcomes are folded in
// with the jsonb concatenation operator `||`, which replaces matched top-level
// keys and preserves the rest. A Thing that reports a SINGLE changed key (the
// per-key dispatch path in thingclient) therefore updates only that
// key's reported state without clobbering sibling keys — the precondition for
// true per-key apply+report. An empty `{}` reported map is a no-op merge, so a
// hold/partial report that applied no keys can never wipe prior reported state.
// A full connect-snapshot report carries every current key and refreshes each
// via `||`. Note `||` only adds/replaces top-level keys — it never PRUNES one,
// so a key that was removed from desired lingers in reported until the row is
// rewritten on reconnect; this is harmless because drift is computed from
// reported_ver vs desired_ver (drift.go), not from the reported key set, so a
// stale reported key cannot manufacture drift.
func (s *Store) UpdateShadowReport(ctx context.Context, id string, reported map[string]any, reportedVer int64, outcomes map[string]ReportedKeyOutcome) error {
	reportedJSON, err := json.Marshal(reported)
	if err != nil {
		return fmt.Errorf("marshal reported: %w", err)
	}
	outcomesJSON, err := json.Marshal(outcomes)
	if err != nil {
		return fmt.Errorf("marshal reported_outcomes: %w", err)
	}
	if len(outcomesJSON) == 0 || string(outcomesJSON) == "null" {
		outcomesJSON = []byte("{}")
	}

	// Monotonic guard composed with per-key merge: a delayed /
	// duplicate / older report (WS↔HTTP fallback interleave, or a report landing
	// on a second replica) must never roll reported_ver — or the reported
	// content/outcomes it stamps — backwards. A regression manufactures
	// desired_ver != reported_ver out of thin air, flapping the Thing into drift
	// and triggering re-push churn until the next genuine report self-corrects.
	// reported_ver advances via GREATEST so it is strictly non-decreasing.
	//
	// When this report is at least as new as the stored version ($3 >=
	// reported_ver, equal allowed so a same-version re-report can refresh
	// content) the incoming keys are MERGED key-by-key via `||`
	// (COALESCE-guarded so a NULL column starts from '{}'), preserving sibling
	// keys the report did not carry — this is what lets a per-key delta report
	// only its one changed key. A stale report ($3 < reported_ver) is dropped
	// entirely. The drift-clear arm keys off the effective (post-GREATEST)
	// version so a stale report can neither clear drift nor un-clear it. The row
	// is always matched on id so 0-rows still means "Thing not found", not
	// "stale report".
	tag, err := s.db.Exec(ctx, `
		UPDATE thing
		SET reported = CASE WHEN $3 >= reported_ver THEN COALESCE(reported, '{}'::jsonb) || $2::jsonb ELSE reported END,
		    reported_outcomes = CASE WHEN $3 >= reported_ver THEN COALESCE(reported_outcomes, '{}'::jsonb) || $4::jsonb ELSE reported_outcomes END,
		    reported_ver = GREATEST(reported_ver, $3),
		    last_seen_at = NOW(), updated_at = NOW(),
		    status = CASE WHEN status = 'drift' AND GREATEST(reported_ver, $3) >= desired_ver THEN 'online' ELSE status END
		WHERE id = $1
	`, id, reportedJSON, reportedVer, outcomesJSON)
	if err != nil {
		return fmt.Errorf("update shadow: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// AcquireConfigVersionLock takes a transaction-scoped advisory lock keyed by
// Thing type. It MUST be the first statement issued inside any transaction that
// allocates or bumps thing.desired_ver for a type — both the type-fanout path
// (UpdateConfig → UpdateDesiredForType) and the single-Thing override path
// (SetOverride / ClearOverride → WriteDesiredAndBumpVer) — so concurrent
// version allocations for the same type serialize.
//
// Why this is required: UpdateDesiredForType allocates the next version
// via a CTE scalar `SELECT COALESCE(MAX(desired_ver),0)+1`. Under READ COMMITTED
// two concurrent same-type writers both read MAX=N; the second blocks on the
// first's row locks and, after the first commits, Postgres' EvalPlanQual
// re-applies the UPDATE's SET clause but does NOT recompute the CTE scalar — so
// both rows land desired_ver=N+1. The override path bumps with a self-relative
// `desired_ver = desired_ver + 1` (immune to that re-eval) but a concurrent
// UpdateConfig still computes its MAX from a snapshot that predates the
// override's commit, colliding the two on the affected Thing. Identical versions
// make the second config_changed frame a silent client-side no-op
// (DesiredVer <= reportedVer in thingclient.applyConfig), permanently dropping
// that key until a process restart.
//
// Holding a per-type advisory lock forces the second writer to wait until the
// first commits, after which its fresh MAX read sees N+1 and allocates N+2 —
// distinct, consecutive, strictly-increasing versions. Acquiring it FIRST
// (before any row locks) gives every version-bumping transaction the same
// lock-acquisition order (advisory → rows), which also prevents a deadlock
// between the type-fanout path (which row-locks thing_config_template in
// UpsertConfigTemplate) and the override path (which FOR-SHARE-locks the same
// template rows in recomputeDesiredTx).
//
// hashtextextended(text, int8) returns a bigint, matching the single-key
// pg_advisory_xact_lock(bigint) overload (mirrors the proven form in
// internal/alerts/engine/raiser.go). The lock auto-releases on COMMIT/ROLLBACK.
func (s *Store) AcquireConfigVersionLock(ctx context.Context, tx pgx.Tx, thingType string) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, thingType); err != nil {
		return fmt.Errorf("acquire config version lock for %s: %w", thingType, err)
	}
	return nil
}

// UpdateDesiredForType updates desired JSON for all Things of a type (in a transaction)
// and bumps each row's desired_ver to a single new value shared across the type:
//
//	next = COALESCE(MAX(desired_ver) WHERE type=$thingType), 0) + 1
//
// The per-type version bump is serialized by AcquireConfigVersionLock, which the
// caller (Manager.UpdateConfig) takes as the first statement of the transaction;
// without it two concurrent same-type writers collide on the same desired_ver.
// See that helper's doc for the full mechanism.
//
// Rationale: thing_config_template.version is per (type, config_key) and is not
// comparable across keys or to the Thing client's global reported_ver. The
// WebSocket config_changed fan-out is one payload per type, so every Thing of
// that type must see the same monotonic desired_ver that exceeds any prior
// reported_ver after the Thing has caught up.
//
// templateVer is the template row version from UpsertConfigTemplate; it is kept
// for call-site symmetry but is not written to thing.desired_ver.
func (s *Store) UpdateDesiredForType(ctx context.Context, tx pgx.Tx, thingType, configKey string, state any, templateVer int64) (rowsAffected int64, shadowDesiredVer int64, err error) {
	_ = templateVer
	stateJSON, err := json.Marshal(state)
	if err != nil {
		return 0, 0, fmt.Errorf("marshal state: %w", err)
	}

	rows, err := tx.Query(ctx, `
WITH next AS (
	SELECT COALESCE(MAX(desired_ver), 0) + 1 AS v
	FROM thing
	WHERE type = $1
)
UPDATE thing AS t
SET desired = jsonb_set(COALESCE(t.desired, '{}'::jsonb), ARRAY[$2::text], $3::jsonb),
    desired_ver = next.v,
    updated_at = NOW()
FROM next
WHERE t.type = $1
RETURNING t.id, t.desired_ver
`, thingType, configKey, stateJSON)
	if err != nil {
		return 0, 0, fmt.Errorf("update desired for type: %w", err)
	}
	defer rows.Close()

	// Collect affected thing IDs so we can emit one NOTIFY per row in
	// the same tx. pg_notify is committed atomically with the UPDATE,
	// so a rollback discards both. Hub selfshadow listeners on each
	// affected Thing's instance filter by id and re-read thing.desired.
	//
	// The notify loop runs ONLY for thingType == "nexus-hub". pg_notify on
	// the config_changed channel is consumed exclusively by the nexus-hub
	// selfshadow LISTEN path (selfshadow.Manager), which lives only in the
	// nexus-hub process and filters by its own instanceID. Every other type
	// (agent, ai-gateway, compliance-proxy, control-plane) receives config
	// changes via the WebSocket broadcast in manager.broadcastConfigChanged,
	// never via pg_notify — so emitting one pg_notify per row for those types
	// is wasted work that runs inside the row-locked tx and extends the
	// lock-hold window. For a 10k-agent fleet that is 10k sequential
	// no-op NOTIFYs under lock.
	var n int64
	notifyIDs := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id, &shadowDesiredVer); err != nil {
			return 0, 0, fmt.Errorf("scan desired_ver: %w", err)
		}
		notifyIDs = append(notifyIDs, id)
		n++
	}
	if err := rows.Err(); err != nil {
		return 0, 0, fmt.Errorf("iterate update results: %w", err)
	}
	// Close the rows cursor explicitly before issuing further queries
	// on the same tx. pgx serializes a single connection; leaving the
	// rows iterator open while Execing pg_notify would deadlock.
	rows.Close()
	if thingType == "nexus-hub" {
		for _, id := range notifyIDs {
			if err := notifyConfigChanged(ctx, tx, id); err != nil {
				return 0, 0, err
			}
		}
	}
	return n, shadowDesiredVer, nil
}

// WriteDesiredAndBumpVer atomically replaces thing.desired with the supplied
// merged map and increments thing.desired_ver by one, returning the new
// desired_ver. Used by the override write/clear path (Hub Manager
// SetOverride / ClearOverride and the override-expiry job): the caller has
// already recomputed the merged shadow state (templates ⊕ overrides) and
// passes it in here so the SQL stays in one tx with the override row mutation.
//
// Unlike UpdateDesiredForType this is a single-Thing, not type-fanout, write —
// per-Thing overrides only affect one row. The caller must run this inside a
// pgx.Tx so the override CRUD and the merge cache write commit together.
func (s *Store) WriteDesiredAndBumpVer(ctx context.Context, tx pgx.Tx, thingID string, merged map[string]any) (int64, error) {
	mergedJSON, err := json.Marshal(merged)
	if err != nil {
		return 0, fmt.Errorf("marshal merged desired: %w", err)
	}
	if merged == nil {
		// json.Marshal(nil) returns "null"; we must store an empty JSON object so
		// downstream readers (thingclient pull, applied-config diff,
		// json.Unmarshal into map[string]any) get an empty map instead of a nil
		// map and surface a sane "no keys configured" state.
		mergedJSON = []byte("{}")
	}

	var newVer int64
	err = tx.QueryRow(ctx, `
		UPDATE thing
		   SET desired     = $2::jsonb,
		       desired_ver = desired_ver + 1,
		       updated_at  = NOW()
		 WHERE id = $1
		RETURNING desired_ver
	`, thingID, mergedJSON).Scan(&newVer)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, ErrNotFound
		}
		return 0, fmt.Errorf("write desired and bump ver for %s: %w", thingID, err)
	}
	// Emit selfshadow notification inside the same tx so commit/rollback
	// stays atomic with the UPDATE. Hub instances LISTENing on
	// config_changed pick this up and apply the override delta.
	if err := notifyConfigChanged(ctx, tx, thingID); err != nil {
		return 0, err
	}
	return newVer, nil
}

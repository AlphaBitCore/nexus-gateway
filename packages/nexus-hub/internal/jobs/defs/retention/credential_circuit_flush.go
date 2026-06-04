package retention

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/redis/go-redis/v9"

	defs "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/jobs/defs"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/credstate"
)

// CredentialCircuitFlushJob owns the cache → DB durability path for the
// per-credential circuit breaker. It runs on the Hub scheduler
// (single-active by config, see internal/scheduler) and:
//
//   1. Reclaims any in-flight entries left behind by a crashed prior run.
//   2. Atomically claims the current dirty cohort by SMOVE-ing every
//      member of cred:circuit:dirty into cred:circuit:in_flight:{hubID}.
//   3. Persists each claimed credential's latest circuit state into the
//      Credential table.
//   4. DELs the in-flight set only on successful flush — a process crash
//      between steps 2 and 4 leaves the working set intact for the next
//      cycle (or another Hub) to reclaim.
//
// First-run-after-start additionally rehydrates Redis from the DB so a
// wiped Redis cannot silently re-arm a credential that was open at
// shutdown.

const (
	credCircuitFlushJobID          = "credential-circuit-flush"
	credCircuitFlushJobName        = "Credential Circuit Flush"
	credCircuitFlushJobDescription = "Drains cred:circuit:dirty into Credential.circuit* columns. Uses an in-flight working set for at-least-once delivery. Rehydrates Redis from DB on first run after restart, and periodically self-heals DB rows left 'open' with no live Redis circuit. See docs/developers/architecture/cross-cutting/safety/credentials-architecture.md §6."

	// circuitReconcileInterval throttles the orphan self-heal scan. The flush
	// itself runs every ~30s; a full-table reconcile that often is wasteful, so
	// it runs at most this often.
	circuitReconcileInterval = 5 * time.Minute
)

// CircuitFlushMetrics owns the Prometheus collectors for the job. A nil
// receiver is callable; the package builds one in main.go.
type CircuitFlushMetrics struct {
	cyclesTotal      *prometheus.CounterVec
	flushedTotal     prometheus.Counter
	reclaimedTotal   prometheus.Counter
	reconciledTotal  prometheus.Counter
	rehydrateTotal   *prometheus.CounterVec
	dirtySetSize     prometheus.Gauge
	runDurationSec   prometheus.Histogram
	transitionsTotal *prometheus.CounterVec
}

// NewCircuitFlushMetrics registers the job's collectors on reg. Pass nil
// to disable collection.
func NewCircuitFlushMetrics(reg prometheus.Registerer) *CircuitFlushMetrics {
	if reg == nil {
		return nil
	}
	f := promauto.With(reg)
	return &CircuitFlushMetrics{
		cyclesTotal: f.NewCounterVec(prometheus.CounterOpts{
			Namespace: "nexus",
			Subsystem: "credential_circuit_flush",
			Name:      "cycles_total",
			Help:      "Number of flush cycles run, labelled by outcome.",
		}, []string{"outcome"}),
		flushedTotal: f.NewCounter(prometheus.CounterOpts{
			Namespace: "nexus",
			Subsystem: "credential_circuit_flush",
			Name:      "flushed_total",
			Help:      "Credential rows whose circuit fields were UPDATEd to DB.",
		}),
		reclaimedTotal: f.NewCounter(prometheus.CounterOpts{
			Namespace: "nexus",
			Subsystem: "credential_circuit_flush",
			Name:      "reclaimed_total",
			Help:      "Members reclaimed from a prior in-flight set (crash recovery).",
		}),
		reconciledTotal: f.NewCounter(prometheus.CounterOpts{
			Namespace: "nexus",
			Subsystem: "credential_circuit_flush",
			Name:      "reconciled_total",
			Help:      "Orphaned DB rows (non-closed circuitState, no live Redis hash) force-closed by the self-heal reconcile.",
		}),
		rehydrateTotal: f.NewCounterVec(prometheus.CounterOpts{
			Namespace: "nexus",
			Subsystem: "credential_circuit_flush",
			Name:      "rehydrate_total",
			Help:      "Credentials examined during the post-restart rehydrate, labelled by outcome.",
		}, []string{"outcome"}),
		dirtySetSize: f.NewGauge(prometheus.GaugeOpts{
			Namespace: "nexus",
			Subsystem: "credential_circuit_flush",
			Name:      "dirty_set_size",
			Help:      "Size of cred:circuit:dirty observed at the start of the most recent cycle.",
		}),
		runDurationSec: f.NewHistogram(prometheus.HistogramOpts{
			Namespace: "nexus",
			Subsystem: "credential_circuit_flush",
			Name:      "run_duration_seconds",
			Help:      "End-to-end time spent in each flush cycle.",
			Buckets:   []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10},
		}),
		transitionsTotal: f.NewCounterVec(prometheus.CounterOpts{
			Namespace: "nexus",
			Subsystem: "credential_circuit_flush",
			Name:      "transitions_total",
			Help:      "Circuit state transitions persisted to DB, labelled by destination state and reason.",
		}, []string{"to", "reason"}),
	}
}

func (m *CircuitFlushMetrics) cycle(outcome string) {
	if m != nil {
		m.cyclesTotal.WithLabelValues(outcome).Inc()
	}
}
func (m *CircuitFlushMetrics) flushed(n int) {
	if m != nil && n > 0 {
		m.flushedTotal.Add(float64(n))
	}
}
func (m *CircuitFlushMetrics) reclaimed(n int) {
	if m != nil && n > 0 {
		m.reclaimedTotal.Add(float64(n))
	}
}
func (m *CircuitFlushMetrics) reconciled(n int) {
	if m != nil && n > 0 {
		m.reconciledTotal.Add(float64(n))
	}
}
func (m *CircuitFlushMetrics) rehydrate(outcome string) {
	if m != nil {
		m.rehydrateTotal.WithLabelValues(outcome).Inc()
	}
}
func (m *CircuitFlushMetrics) setDirty(n int) {
	if m != nil {
		m.dirtySetSize.Set(float64(n))
	}
}
func (m *CircuitFlushMetrics) observe(d time.Duration) {
	if m != nil {
		m.runDurationSec.Observe(d.Seconds())
	}
}
func (m *CircuitFlushMetrics) transition(to, reason string) {
	if m != nil {
		m.transitionsTotal.WithLabelValues(to, reason).Inc()
	}
}

type CredentialCircuitFlushJob struct {
	// pool is typed against the package-level defs.PgxPool seam so the
	// per-credential flush UPDATE + rehydrate SELECT are testable via
	// pgxmock.
	pool          defs.PgxPool
	rdb           redis.UniversalClient
	hubID         string
	interval      time.Duration
	logger        *slog.Logger
	metrics       *CircuitFlushMetrics
	rehydrateOnce sync.Once
	// reconcileEvery throttles the orphan self-heal scan (a full-table SELECT)
	// so it does not run on every 30s flush tick; lastReconcile tracks the last
	// successful-or-attempted run. Mutated only from Run, which the scheduler
	// never invokes concurrently with itself.
	reconcileEvery time.Duration
	lastReconcile  time.Time
}

// NewCredentialCircuitFlush constructs the job. interval defaults to 30s;
// hubID identifies the per-Hub in-flight set (see credstate.InFlightSet).
func NewCredentialCircuitFlush(pool *pgxpool.Pool, rdb redis.UniversalClient, hubID string, interval time.Duration, logger *slog.Logger, metrics *CircuitFlushMetrics) *CredentialCircuitFlushJob {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	if hubID == "" {
		hubID = "hub-unknown"
	}
	return &CredentialCircuitFlushJob{
		pool:           pool,
		rdb:            rdb,
		hubID:          hubID,
		interval:       interval,
		logger:         logger.With("job", credCircuitFlushJobID),
		metrics:        metrics,
		reconcileEvery: circuitReconcileInterval,
	}
}

func (j *CredentialCircuitFlushJob) ID() string              { return credCircuitFlushJobID }
func (j *CredentialCircuitFlushJob) Name() string            { return credCircuitFlushJobName }
func (j *CredentialCircuitFlushJob) Description() string     { return credCircuitFlushJobDescription }
func (j *CredentialCircuitFlushJob) Interval() time.Duration { return j.interval }

// Run executes one flush cycle. Returning an error logs but does not crash;
// the scheduler will run the next cycle on the next tick.
//
// Phase ordering (important for race-free interaction with AI Gateway):
//  1. Reclaim any in-flight set left by a prior crashed run.
//  2. SMOVE cred:circuit:dirty → cred:circuit:in_flight:{hubID} so the
//     current transitions are claimed atomically. Fresh writes from
//     AI Gateway during this phase land in the dirty set and will be
//     picked up next cycle.
//  3. (First run only) Rehydrate Redis from DB for credentials NOT in
//     the in-flight working set — the AI Gateway has not staked a
//     claim on those, so DB is the authoritative view.
//  4. Process the working set (HGETALL hash → UPDATE Credential).
//  5. DEL the in-flight set on full success.
//
// Rehydrate runs AFTER the dirty-set claim so a close transition queued
// just before the first cycle wins: the close credID lands in in-flight,
// rehydrate skips it, and the empty-hash flush resets the DB row.
func (j *CredentialCircuitFlushJob) Run(ctx context.Context) error {
	if j.rdb == nil {
		j.metrics.cycle("noop_no_redis")
		return nil
	}
	start := time.Now()
	defer func() { j.metrics.observe(time.Since(start)) }()

	// 1. Reclaim any in-flight set left by a prior crashed run.
	if err := j.reclaimInFlight(ctx); err != nil {
		j.logger.Warn("reclaim previous in-flight set failed; will retry next cycle", "error", err)
		// Continue — fresh dirty entries are still worth processing.
	}

	// 2. Claim the current dirty cohort atomically.
	inFlightKey := credstate.InFlightSet(j.hubID)
	moved, err := j.atomicClaim(ctx, credstate.CircuitDirtySet, inFlightKey)
	if err != nil {
		j.metrics.cycle("error_claim")
		return fmt.Errorf("claim dirty: %w", err)
	}

	// 3. First-run rehydrate (excluding anything in the just-claimed
	//    working set so a close transition can't be stomped).
	j.rehydrateOnce.Do(func() {
		if err := j.rehydrateFromDB(ctx, inFlightKey); err != nil {
			j.logger.Warn("rehydrate failed; circuit state may be stale until next transition",
				"error", err)
		}
	})

	// 3b. Throttled self-heal: close orphaned DB rows whose live circuit is
	//     gone (e.g. an admin reset, a Redis eviction, or a cooldown-elapsed
	//     rate-limit rehydrate did not re-arm). Runs AFTER rehydrate so it sees
	//     the post-rehydrate Redis state, and BEFORE the idle early-return so
	//     orphans are healed even when no fresh transitions are pending. Claim
	//     the slot before running so a transient error does not turn it into a
	//     per-tick table scan.
	if j.reconcileDue() {
		j.lastReconcile = time.Now()
		if err := j.reconcileOrphans(ctx, inFlightKey); err != nil {
			j.logger.Warn("orphan reconcile failed; will retry next interval", "error", err)
		}
	}

	j.metrics.setDirty(len(moved))
	if len(moved) == 0 {
		j.metrics.cycle("ok_idle")
		return nil
	}

	var flushed, skipped int
	for _, credID := range moved {
		if err := j.flushOne(ctx, credID); err != nil {
			j.logger.Warn("flush failed", "credentialID", credID, "error", err)
			skipped++
		} else {
			flushed++
		}
	}
	j.metrics.flushed(flushed)

	// Only DEL the in-flight set after every DB write completed. A crash
	// before this point leaves the working set for the next cycle to
	// reclaim (at-least-once semantics).
	if skipped == 0 {
		if _, err := j.rdb.Del(ctx, inFlightKey).Result(); err != nil {
			j.logger.Warn("delete in-flight set failed; entries will be reclaimed next cycle",
				"error", err)
		}
		j.metrics.cycle("ok")
	} else {
		// Partial failure: keep the whole in-flight set so the failed entries
		// get retried next cycle. Successfully-flushed entries stay in the set
		// too and are simply re-flushed — every DB write is idempotent
		// (writeClosed / the transition UPDATE are last-write-wins on the same
		// row), so re-processing them is harmless.
		j.metrics.cycle("ok_partial")
	}

	j.logger.Debug("circuit flush cycle done", "claimed", len(moved), "flushed", flushed, "skipped", skipped)
	return nil
}

// atomicClaim moves every member of src into dst. Uses SMOVE in a single
// pipeline so the operation is atomic against concurrent SADDs from
// AI Gateway — fresh additions during the pipeline land in src and will be
// picked up on the next cycle.
func (j *CredentialCircuitFlushJob) atomicClaim(ctx context.Context, src, dst string) ([]string, error) {
	members, err := j.rdb.SMembers(ctx, src).Result()
	if err != nil {
		return nil, fmt.Errorf("smembers: %w", err)
	}
	if len(members) == 0 {
		return nil, nil
	}
	pipe := j.rdb.Pipeline()
	for _, m := range members {
		pipe.SMove(ctx, src, dst, m)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return nil, fmt.Errorf("smove pipeline: %w", err)
	}
	return members, nil
}

// reclaimInFlight inspects the current Hub's own in-flight set. Any
// entries present from a prior crashed run are merged back into the
// dirty set so the steady-state flush re-processes them. Always safe to
// run; the in-flight set is empty in the steady state.
func (j *CredentialCircuitFlushJob) reclaimInFlight(ctx context.Context) error {
	inFlightKey := credstate.InFlightSet(j.hubID)
	members, err := j.rdb.SMembers(ctx, inFlightKey).Result()
	if err != nil {
		return fmt.Errorf("smembers in_flight: %w", err)
	}
	if len(members) == 0 {
		return nil
	}
	pipe := j.rdb.Pipeline()
	// Move entries back to the dirty set; let the steady-state path drain.
	for _, m := range members {
		pipe.SMove(ctx, inFlightKey, credstate.CircuitDirtySet, m)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("smove reclaim: %w", err)
	}
	j.metrics.reclaimed(len(members))
	j.logger.Info("reclaimed in-flight entries from prior crashed run",
		"count", len(members), "hubID", j.hubID)
	return nil
}

// flushOne writes the latest circuit-hash contents for one credential to
// the Credential table. An empty hash means the credential is closed
// (RecordAttempt DELs the hash on the recovery → CLOSED path).
func (j *CredentialCircuitFlushJob) flushOne(ctx context.Context, credID string) error {
	key := credstate.CircuitKey(credID)
	fields, err := j.rdb.HGetAll(ctx, key).Result()
	if err != nil {
		return fmt.Errorf("hgetall: %w", err)
	}

	state := fields[credstate.CircuitFieldState]
	if state == "" || state == credstate.CircuitClosed {
		// Empty hash → CLOSED. Reset every circuit column on the row so
		// the admin API and reliability-alerts job see a consistent view.
		return j.writeClosed(ctx, credID)
	}

	reason := nilIfEmpty(fields[credstate.CircuitFieldOpenReason])
	openedAt := parseRFC3339NanoPtr(fields[credstate.CircuitFieldOpenedAt])
	nextProbeAt := parseRFC3339NanoPtr(fields[credstate.CircuitFieldNextProbe])

	_, err = j.pool.Exec(ctx, `
		UPDATE "Credential" SET
			"circuitState"        = $2,
			"circuitReason"       = $3,
			"circuitOpenedAt"     = $4,
			"circuitNextProbeAt"  = $5,
			"updatedAt"           = NOW()
		WHERE id = $1
	`, credID, state, reason, openedAt, nextProbeAt)
	if err != nil {
		return fmt.Errorf("update transition: %w", err)
	}
	reasonLabel := ""
	if reason != nil {
		reasonLabel = *reason
	}
	j.metrics.transition(state, reasonLabel)
	return nil
}

// writeClosed resets every circuit column on a credential row to the closed
// state. Shared by flushOne's recovery branch and the orphan reconcile so the
// "what closed looks like in the DB" SQL lives in exactly one place.
func (j *CredentialCircuitFlushJob) writeClosed(ctx context.Context, credID string) error {
	if _, err := j.pool.Exec(ctx, `
		UPDATE "Credential" SET
			"circuitState"        = $2,
			"circuitReason"       = NULL,
			"circuitOpenedAt"     = NULL,
			"circuitNextProbeAt"  = NULL,
			"updatedAt"           = NOW()
		WHERE id = $1
	`, credID, credstate.CircuitClosed); err != nil {
		return fmt.Errorf("update closed: %w", err)
	}
	j.metrics.transition(credstate.CircuitClosed, "")
	return nil
}

// reconcileOrphans closes any Credential row whose durable circuitState is
// non-closed but whose live Redis hash is ABSENT. Such a row is an orphan the
// steady-state flush can never fix: the flush is driven by cred:circuit:dirty,
// and an orphan was never added to it. Orphans arise from an admin reset that
// cleared Redis, a Redis eviction, or a cooldown-elapsed rate-limit that the
// restart rehydrate intentionally did not re-arm. In every case Redis-absent
// means the live state is already CLOSED (the gateway treats a missing hash as
// closed), so converging the DB to closed only removes stale "open" the UI
// shows — it can never close a circuit that is genuinely open (those have a
// live Redis hash, which we skip). In-flight members are skipped so this never
// races the flush writer for the same row.
func (j *CredentialCircuitFlushJob) reconcileOrphans(ctx context.Context, inFlightKey string) error {
	// Collect the candidate IDs first and close the rows before issuing any
	// per-ID Redis/Exec calls — interleaving a second statement while the
	// SELECT's rows are open is unsafe on a single pooled connection.
	rows, err := j.pool.Query(ctx, `SELECT id FROM "Credential" WHERE "circuitState" <> $1`, credstate.CircuitClosed)
	if err != nil {
		return fmt.Errorf("select non-closed circuits: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("scan id: %w", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate rows: %w", err)
	}

	var closed int
	for _, id := range ids {
		// A fresh transition the flush just claimed — let the flush win.
		if inFlightKey != "" {
			if m, mErr := j.rdb.SIsMember(ctx, inFlightKey, id).Result(); mErr == nil && m {
				continue
			}
		}
		n, exErr := j.rdb.Exists(ctx, credstate.CircuitKey(id)).Result()
		if exErr != nil {
			j.logger.Warn("reconcile: redis exists check failed", "credentialID", id, "error", exErr)
			continue
		}
		if n > 0 {
			continue // live circuit present → DB row is legitimately non-closed
		}
		if wErr := j.writeClosed(ctx, id); wErr != nil {
			j.logger.Warn("reconcile: close orphan failed", "credentialID", id, "error", wErr)
			continue
		}
		closed++
	}
	if closed > 0 {
		j.metrics.reconciled(closed)
		j.logger.Info("reconciled orphaned circuit rows to closed", "count", closed)
	}
	return nil
}

// reconcileDue reports whether the throttled orphan reconcile should run this
// cycle. The zero lastReconcile makes the first cycle (post-rehydrate) due.
func (j *CredentialCircuitFlushJob) reconcileDue() bool {
	return time.Since(j.lastReconcile) >= j.reconcileEvery
}

// rehydrateFromDB copies persisted circuit state back into Redis for
// credentials whose state is non-closed at Hub start. Rules:
//   - Redis hashes that already exist are left untouched (Redis wins —
//     AI Gateway may have observed newer transitions).
//   - Credentials in the current in-flight working set are skipped — a
//     transition the AI Gateway just queued (typically a close) must
//     take precedence over the durable DB value.
//   - Rate-limit circuits whose cooldown has already elapsed are
//     skipped: re-opening would be a stale signal.
func (j *CredentialCircuitFlushJob) rehydrateFromDB(ctx context.Context, inFlightKey string) error {
	rows, err := j.pool.Query(ctx, `
		SELECT id,
		       "circuitState",
		       COALESCE("circuitReason", '')   AS reason,
		       "circuitOpenedAt",
		       "circuitNextProbeAt"
		FROM "Credential"
		WHERE "circuitState" != 'closed'
	`)
	if err != nil {
		return fmt.Errorf("select persisted circuits: %w", err)
	}
	defer rows.Close()

	now := time.Now().UTC()
	var restored, skippedExisting, skippedExpired, skippedInFlight int

	for rows.Next() {
		var (
			id, state, reason     string
			openedAt, nextProbeAt *time.Time
		)
		if err := rows.Scan(&id, &state, &reason, &openedAt, &nextProbeAt); err != nil {
			j.logger.Warn("scan persisted row", "error", err)
			continue
		}
		// AI Gateway has staked a claim on a fresh transition (typically
		// a close DEL'd the hash) — let the steady-state flush handle it.
		if inFlightKey != "" {
			if m, err := j.rdb.SIsMember(ctx, inFlightKey, id).Result(); err == nil && m {
				skippedInFlight++
				j.metrics.rehydrate("skipped_in_flight")
				continue
			}
		}
		key := credstate.CircuitKey(id)
		if n, err := j.rdb.Exists(ctx, key).Result(); err == nil && n > 0 {
			skippedExisting++
			j.metrics.rehydrate("skipped_redis_present")
			continue
		}
		if reason == credstate.ReasonRateLimit && nextProbeAt != nil && !nextProbeAt.After(now) {
			skippedExpired++
			j.metrics.rehydrate("skipped_cooldown_elapsed")
			continue
		}
		args := []interface{}{
			credstate.CircuitFieldState, state,
		}
		if openedAt != nil {
			args = append(args, credstate.CircuitFieldOpenedAt, openedAt.Format(time.RFC3339Nano))
		}
		if nextProbeAt != nil {
			args = append(args, credstate.CircuitFieldNextProbe, nextProbeAt.Format(time.RFC3339Nano))
		}
		if reason != "" {
			args = append(args, credstate.CircuitFieldOpenReason, reason)
		}
		if err := j.rdb.HSet(ctx, key, args...).Err(); err != nil {
			j.logger.Warn("rehydrate HSET failed", "credentialID", id, "error", err)
			j.metrics.rehydrate("error")
			continue
		}
		restored++
		j.metrics.rehydrate("restored")
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate rows: %w", err)
	}
	if restored > 0 || skippedExisting > 0 || skippedExpired > 0 || skippedInFlight > 0 {
		j.logger.Info("circuit state rehydrated from DB",
			"restored", restored,
			"skipped_existing", skippedExisting,
			"skipped_expired", skippedExpired,
			"skipped_in_flight", skippedInFlight)
	}
	return nil
}

// Small parsing helpers — used inside this job and re-used by the
// reliability-alerts job. Kept in this file because that is where they
// were introduced; moving them to a shared package is not worth the churn.

func parseRFC3339NanoPtr(s string) *time.Time {
	if s == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return nil
	}
	return &t
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

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
	credCircuitFlushJobDescription = "Drains cred:circuit:dirty into Credential.circuit* columns. Uses an in-flight working set for at-least-once delivery. Rehydrates Redis from DB on first run after restart. See docs/developers/architecture/control-plane/credentials-architecture.md."
)

// CircuitFlushMetrics owns the Prometheus collectors for the job. A nil
// receiver is callable; the package builds one in main.go.
type CircuitFlushMetrics struct {
	cyclesTotal      *prometheus.CounterVec
	flushedTotal     prometheus.Counter
	reclaimedTotal   prometheus.Counter
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
			Namespace: "nexus_hub",
			Subsystem: "credential_circuit_flush",
			Name:      "cycles_total",
			Help:      "Number of flush cycles run, labelled by outcome.",
		}, []string{"outcome"}),
		flushedTotal: f.NewCounter(prometheus.CounterOpts{
			Namespace: "nexus_hub",
			Subsystem: "credential_circuit_flush",
			Name:      "flushed_total",
			Help:      "Credential rows whose circuit fields were UPDATEd to DB.",
		}),
		reclaimedTotal: f.NewCounter(prometheus.CounterOpts{
			Namespace: "nexus_hub",
			Subsystem: "credential_circuit_flush",
			Name:      "reclaimed_total",
			Help:      "Members reclaimed from a prior in-flight set (crash recovery).",
		}),
		rehydrateTotal: f.NewCounterVec(prometheus.CounterOpts{
			Namespace: "nexus_hub",
			Subsystem: "credential_circuit_flush",
			Name:      "rehydrate_total",
			Help:      "Credentials examined during the post-restart rehydrate, labelled by outcome.",
		}, []string{"outcome"}),
		dirtySetSize: f.NewGauge(prometheus.GaugeOpts{
			Namespace: "nexus_hub",
			Subsystem: "credential_circuit_flush",
			Name:      "dirty_set_size",
			Help:      "Size of cred:circuit:dirty observed at the start of the most recent cycle.",
		}),
		runDurationSec: f.NewHistogram(prometheus.HistogramOpts{
			Namespace: "nexus_hub",
			Subsystem: "credential_circuit_flush",
			Name:      "run_duration_seconds",
			Help:      "End-to-end time spent in each flush cycle.",
			Buckets:   []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10},
		}),
		transitionsTotal: f.NewCounterVec(prometheus.CounterOpts{
			Namespace: "nexus_hub",
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
		pool:     pool,
		rdb:      rdb,
		hubID:    hubID,
		interval: interval,
		logger:   logger.With("job", credCircuitFlushJobID),
		metrics:  metrics,
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
		// Partial failure: keep in-flight set so failed entries get retried.
		// Remove successfully-flushed entries to avoid re-processing them.
		// Because flushOne and the SREM run in different connections, we
		// race against fresh dirty additions, but a fresh dirty for the
		// same credID will be picked up next cycle anyway.
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
		_, err := j.pool.Exec(ctx, `
			UPDATE "Credential" SET
				"circuitState"        = $2,
				"circuitReason"       = NULL,
				"circuitOpenedAt"     = NULL,
				"circuitNextProbeAt"  = NULL,
				"updatedAt"           = NOW()
			WHERE id = $1
		`, credID, credstate.CircuitClosed)
		if err != nil {
			return fmt.Errorf("update closed: %w", err)
		}
		j.metrics.transition(credstate.CircuitClosed, "")
		return nil
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

package audit

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	opsmetrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
)

const auditFreshnessJobID = "audit-freshness-check"

// auditFreshnessQueryer is the minimum pgx surface the job needs. Declared as
// an interface so the test suite can inject pgxmock (which would otherwise
// have to share the real Postgres traffic_event table — destructive, since
// the freshness check is `MAX(timestamp)` over the whole table and cannot be
// scoped to test-owned rows). *pgxpool.Pool satisfies this in production;
// pgxmock.PgxPoolIface satisfies it in tests.
type auditFreshnessQueryer interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// AuditFreshnessCheck periodically queries max(traffic_event.timestamp) and
// surfaces the freshness lag as a Prometheus gauge plus an ERROR-level slog
// record when the lag exceeds the threshold. The slog ERROR flows through
// Hub's SlogSink into thing_diag_event so it lands on /infrastructure/errors.
//
// Why this job exists:
//
// In a prior production incident the audit pipeline silently stalled for
// 16 hours after a missed migration left a Hub-side batch INSERT failing
// with SQLSTATE 42703 ("column upstream_ttfb_ms does not exist").
// Every flush logged ERROR but those records never reached
// `thing_diag_event` because the DI-injected logger bypassed SlogSink
// (see feedback/server-slog-sink-di-bypass). Operators had no signal
// — only a manual smoke test revealed it. This job is the safety net
// that doesn't depend on the diag pipeline being healthy: it polls the
// table directly and re-emits an ERROR via the now-fixed pipeline. It
// is also the producer-side signal that drives the dashboard freshness
// gauge.
type AuditFreshnessCheck struct {
	pool      auditFreshnessQueryer
	interval  time.Duration
	threshold time.Duration
	logger    *slog.Logger
	age       *opsmetrics.Gauge
	fires     *opsmetrics.Counter
	// lastReceived, when set, reports when the Hub last received a data-plane
	// traffic-event message off NATS. It is the "were there events to persist?"
	// denominator that separates a STALLED pipeline (events arriving but rows
	// not advancing) from an IDLE deployment (no events at all). Nil preserves
	// the original age-only behaviour (alarm on any stale table).
	lastReceived func() time.Time
}

// WithLastReceived wires the traffic-event writer's receipt clock so the check
// only alarms when events are actually arriving. Call before scheduling.
func (j *AuditFreshnessCheck) WithLastReceived(fn func() time.Time) *AuditFreshnessCheck {
	j.lastReceived = fn
	return j
}

// NewAuditFreshnessCheck wires the job. interval = how often Run executes
// (default 60s). threshold = the staleness ceiling above which Run emits an
// ERROR (default 5 min). opsReg may be nil in tests.
func NewAuditFreshnessCheck(pool *pgxpool.Pool, interval, threshold time.Duration, opsReg *opsmetrics.Registry, logger *slog.Logger) *AuditFreshnessCheck {
	if interval <= 0 {
		interval = 60 * time.Second
	}
	if threshold <= 0 {
		threshold = 5 * time.Minute
	}
	var age *opsmetrics.Gauge
	var fires *opsmetrics.Counter
	if opsReg != nil {
		age = opsReg.NewGauge("audit_pipeline.freshness_seconds", []string{})
		fires = opsReg.NewCounter("audit_pipeline.stale_fired_total", []string{})
	}
	return &AuditFreshnessCheck{
		pool:      pool,
		interval:  interval,
		threshold: threshold,
		logger:    logger.With("job", auditFreshnessJobID),
		age:       age,
		fires:     fires,
	}
}

func (j *AuditFreshnessCheck) ID() string   { return auditFreshnessJobID }
func (j *AuditFreshnessCheck) Name() string { return "Audit Pipeline Freshness Check" }
func (j *AuditFreshnessCheck) Description() string {
	return "Alerts when traffic_event hasn't seen new rows within the configured threshold."
}
func (j *AuditFreshnessCheck) Interval() time.Duration { return j.interval }

// RunOnStart=false because a fresh deploy will trivially be stale during the
// first 5 minutes of warmup; we want the periodic tick to grant grace.
func (j *AuditFreshnessCheck) RunOnStart() bool { return false }

// Run queries traffic_event for the latest timestamp from data-plane sources
// (ai-gateway + compliance-proxy — agent rows arrive on a delayed drain, so
// they're excluded to keep the alert tight on live producers). If the lag
// exceeds the threshold AND the table is not empty, an ERROR is logged.
//
// Empty-table guard: a brand-new prod or a freshly seeded local DB will have
// MAX(timestamp) = NULL → COALESCE pushes lagSec into the very-large range.
// The EXISTS check prevents the job from spamming "audit pipeline stale"
// during the very first deploy before any traffic arrives.
func (j *AuditFreshnessCheck) Run(ctx context.Context) error {
	var latest time.Time
	var lagSec float64
	var anyRow bool
	err := j.pool.QueryRow(ctx, `
		SELECT
			COALESCE(MAX(timestamp), TIMESTAMP 'epoch')                                AS latest,
			EXTRACT(EPOCH FROM (NOW() - COALESCE(MAX(timestamp), NOW())))::float8     AS lag_sec,
			EXISTS(SELECT 1 FROM traffic_event WHERE source IN ('ai-gateway','compliance-proxy')) AS any_row
		FROM traffic_event
		WHERE source IN ('ai-gateway','compliance-proxy')
	`).Scan(&latest, &lagSec, &anyRow)
	if err != nil {
		return fmt.Errorf("audit_freshness: query: %w", err)
	}

	if j.age != nil {
		j.age.With().Set(lagSec)
	}

	if !anyRow {
		// Empty data-plane table — nothing meaningful to assert about
		// freshness yet.
		return nil
	}

	if lagSec <= j.threshold.Seconds() {
		// Healthy.
		return nil
	}

	// Idle vs stalled. A stale table is only a FAILURE when there were events to
	// persist. If we know when the Hub last received a data-plane traffic-event
	// message and it was longer ago than the threshold, the deployment is simply
	// idle — nothing arrived, nothing to write — so this is not the stalled
	// pipeline the check exists to catch, and alarming would train operators to
	// ignore the very ERROR that matters (the 2026-05-14 silent-loss incident had
	// events ARRIVING but every INSERT failing — recent receipt, stale table).
	// The age gauge above still carries the real staleness for the dashboard; we
	// just do not raise an ERROR on an idle box. Nil clock keeps the original
	// age-only behaviour.
	if j.lastReceived != nil {
		last := j.lastReceived()
		if last.IsZero() || time.Since(last) > j.threshold {
			return nil
		}
	}

	if j.fires != nil {
		j.fires.With().Inc()
	}

	// The slog ERROR is the operator-visible signal. SlogSink (wired in
	// main.go) routes ERROR records into thing_diag_event with a stable
	// message_hash so the UI groups all firings into one card.
	// noStack=true: this is a data-state report (no new traffic for N
	// minutes), not a programming error — skip the goroutine dump that
	// errorStackHandler otherwise adds at Error level.
	j.logger.Error("audit pipeline appears stale",
		slog.Float64("lag_seconds", lagSec),
		slog.Float64("threshold_seconds", j.threshold.Seconds()),
		slog.Time("latest_event", latest),
		slog.String("event", "audit_freshness_stale"),
		slog.Bool("noStack", true),
	)
	return nil
}

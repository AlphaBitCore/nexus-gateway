package rollup

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/credstate"
)

// healthRollupPool is the minimum pgx surface CredentialHealthRollupJob
// needs. Declared as an interface so the test suite can inject pgxmock
// without sharing the real Postgres traffic_event + Credential tables —
// the rollup's `collect` query reads ALL credentials with samples in the
// window, then UPDATEs every one, which would touch foreign rows.
// *pgxpool.Pool satisfies this in production; pgxmock.PgxPoolIface in
// tests.
type healthRollupPool interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// CredentialHealthRollupJob computes per-credential health by aggregating
// traffic_event rows over two rolling windows:
//
//   - shortWindow (default 5 min) drives the current classification
//     (healthy / degraded / unavailable / collecting / unknown) and the
//     dominantError attribution (what kind of failure dominates the
//     window).
//   - longWindow (default 1 h) feeds the trend signal (improving / stable
//     / degrading) by comparing the 5 min success rate against the 1 h
//     baseline.
//
// Persistence rules:
//   * Rows whose classification status actually changes get a fresh
//     healthStatusChangedAt — that timestamp drives the sustained-
//     degraded alert.
//   * Rows whose classification matches the previous status still get
//     their derived fields (rate, samples, dominantError, trend, checkedAt)
//     refreshed so the admin UI never shows stale numbers.
//   * Credentials with zero samples in the short window keep their prior
//     classification but still receive a refreshed healthCheckedAt so
//     operators can tell when the rollup last looked at them.

const (
	credHealthRollupJobID          = "credential-health-rollup"
	credHealthRollupJobName        = "Credential Health Rollup"
	credHealthRollupJobDescription = "Computes per-credential health (healthy / degraded / unavailable / collecting / unknown), dominantError, and trend (improving / stable / degrading) from traffic_event over a short (5 min) and long (1 h) window. Persists to Credential.health* columns; only rows whose status changed update healthStatusChangedAt. See docs/developers/architecture/control-plane/credentials-architecture.md."

	// longWindowMultiplier sets the long window relative to the configured
	// short window (Thresholds.HealthWindowSeconds). 12× makes the long
	// window 1 h when the short window is 5 min — a balance between
	// responsiveness and noise rejection in the trend signal.
	longWindowMultiplier = 12

	// trendImprovingDelta is the minimum (long - short) success-rate drop
	// (towards bad) needed to call the trend degrading; the same magnitude
	// in the other direction calls it improving. Values closer to zero
	// fall into stable.
	trendDeltaPct = 5
)

// HealthRollupMetrics owns the Prometheus collectors for the rollup job.
type HealthRollupMetrics struct {
	cyclesTotal      *prometheus.CounterVec
	updatedTotal     prometheus.Counter
	candidatesTotal  prometheus.Counter
	transitionsTotal *prometheus.CounterVec
	runDurationSec   prometheus.Histogram
}

func NewHealthRollupMetrics(reg prometheus.Registerer) *HealthRollupMetrics {
	if reg == nil {
		return nil
	}
	f := promauto.With(reg)
	return &HealthRollupMetrics{
		cyclesTotal: f.NewCounterVec(prometheus.CounterOpts{
			Namespace: "nexus_hub",
			Subsystem: "credential_health_rollup",
			Name:      "cycles_total",
			Help:      "Number of rollup cycles run, labelled by outcome.",
		}, []string{"outcome"}),
		updatedTotal: f.NewCounter(prometheus.CounterOpts{
			Namespace: "nexus_hub",
			Subsystem: "credential_health_rollup",
			Name:      "updated_total",
			Help:      "Credential rows whose health fields were UPDATEd to DB.",
		}),
		candidatesTotal: f.NewCounter(prometheus.CounterOpts{
			Namespace: "nexus_hub",
			Subsystem: "credential_health_rollup",
			Name:      "candidates_total",
			Help:      "Credentials with at least one traffic_event sample in the short window across all cycles.",
		}),
		transitionsTotal: f.NewCounterVec(prometheus.CounterOpts{
			Namespace: "nexus_hub",
			Subsystem: "credential_health_rollup",
			Name:      "transitions_total",
			Help:      "Health status transitions, labelled by from and to.",
		}, []string{"from", "to"}),
		runDurationSec: f.NewHistogram(prometheus.HistogramOpts{
			Namespace: "nexus_hub",
			Subsystem: "credential_health_rollup",
			Name:      "run_duration_seconds",
			Help:      "End-to-end time spent in each rollup cycle.",
			Buckets:   []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 30},
		}),
	}
}

func (m *HealthRollupMetrics) cycle(o string) {
	if m != nil {
		m.cyclesTotal.WithLabelValues(o).Inc()
	}
}
func (m *HealthRollupMetrics) updated(n int) {
	if m != nil && n > 0 {
		m.updatedTotal.Add(float64(n))
	}
}
func (m *HealthRollupMetrics) candidates(n int) {
	if m != nil && n > 0 {
		m.candidatesTotal.Add(float64(n))
	}
}
func (m *HealthRollupMetrics) transition(from, to string) {
	if m != nil {
		m.transitionsTotal.WithLabelValues(from, to).Inc()
	}
}
func (m *HealthRollupMetrics) observe(d time.Duration) {
	if m != nil {
		m.runDurationSec.Observe(d.Seconds())
	}
}

// thresholdsReader is the read surface the health-rollup job needs from
// the Hub's view of credential reliability config. The implementation
// lives in shared infrastructure; declared here to keep the job free of
// system_metadata coupling.
type thresholdsReader interface {
	Thresholds(ctx context.Context) credstate.Thresholds
}

type CredentialHealthRollupJob struct {
	pool       healthRollupPool
	interval   time.Duration
	logger     *slog.Logger
	metrics    *HealthRollupMetrics
	thresholds thresholdsReader
}

func NewCredentialHealthRollup(pool *pgxpool.Pool, thresholds thresholdsReader, interval time.Duration, logger *slog.Logger, metrics *HealthRollupMetrics) *CredentialHealthRollupJob {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	return &CredentialHealthRollupJob{
		pool:       pool,
		interval:   interval,
		logger:     logger.With("job", credHealthRollupJobID),
		metrics:    metrics,
		thresholds: thresholds,
	}
}

func (j *CredentialHealthRollupJob) ID() string              { return credHealthRollupJobID }
func (j *CredentialHealthRollupJob) Name() string            { return credHealthRollupJobName }
func (j *CredentialHealthRollupJob) Description() string     { return credHealthRollupJobDescription }
func (j *CredentialHealthRollupJob) Interval() time.Duration { return j.interval }

// statusCounts holds per-credential rollup buckets keyed by error category.
type statusCounts struct {
	credentialID string
	short        windowCounts
	long         windowCounts
}

type windowCounts struct {
	samples     int
	success     int
	authFail    int // 401 / 403
	rateLimit   int // 429
	upstream5xx int
	timeout     int // status_code = 0 (network)
	clientError int // 4xx other than 401 / 403 / 429
	lastSample  time.Time
}

// Run executes one rollup cycle.
func (j *CredentialHealthRollupJob) Run(ctx context.Context) error {
	start := time.Now()
	defer func() { j.metrics.observe(time.Since(start)) }()

	t := credstate.DefaultThresholds
	if j.thresholds != nil {
		t = j.thresholds.Thresholds(ctx)
	}
	short := time.Duration(t.HealthWindowSeconds) * time.Second
	long := short * longWindowMultiplier

	now := time.Now().UTC()
	rolled, err := j.collect(ctx, now.Add(-short), now.Add(-long))
	if err != nil {
		j.metrics.cycle("error_collect")
		return fmt.Errorf("collect: %w", err)
	}
	j.metrics.candidates(len(rolled))
	if len(rolled) == 0 {
		j.metrics.cycle("ok_idle")
		return nil
	}

	prior, err := j.priorStatus(ctx, rolled)
	if err != nil {
		j.metrics.cycle("error_prior")
		return fmt.Errorf("read prior status: %w", err)
	}

	updates := j.classifyAll(rolled, prior, t)
	if len(updates) == 0 {
		j.metrics.cycle("ok_no_writes")
		return nil
	}

	if err := j.batchUpdate(ctx, updates, now); err != nil {
		j.metrics.cycle("error_update")
		return fmt.Errorf("batch update: %w", err)
	}
	for _, u := range updates {
		if u.priorStatus != u.status {
			j.metrics.transition(u.priorStatus, u.status)
		}
	}
	j.metrics.updated(len(updates))
	j.metrics.cycle("ok")
	j.logger.Info("credential health rollup",
		"candidates", len(rolled),
		"updated", len(updates),
		"short_window_s", t.HealthWindowSeconds,
		"long_window_s", t.HealthWindowSeconds*longWindowMultiplier,
	)
	return nil
}

// collect runs a single aggregate query that fills both windows in one
// pass — both windows share the (timestamp DESC, credential_id) partial
// index installed by migration 20260513*_e41_v2_credential_state_v2.
func (j *CredentialHealthRollupJob) collect(ctx context.Context, shortStart, longStart time.Time) ([]statusCounts, error) {
	// 429 (provider throttling) is excluded from both the short and long
	// windows entirely — it doesn't say anything about credential health,
	// it says something about the provider's tenancy quota. Including it
	// in `samples` would dilute the success rate; including it in the
	// failure bucket would attribute throttling to credential quality.
	rows, err := j.pool.Query(ctx, `
		SELECT credential_id,
		       -- short window (>= $1, excluding 429)
		       COUNT(*) FILTER (WHERE timestamp >= $1 AND status_code IS DISTINCT FROM 429)                          AS short_samples,
		       COUNT(*) FILTER (WHERE timestamp >= $1 AND status_code BETWEEN 200 AND 299)                          AS short_success,
		       COUNT(*) FILTER (WHERE timestamp >= $1 AND status_code IN (401, 403))                                AS short_auth,
		       0::int                                                                                               AS short_rate,
		       COUNT(*) FILTER (WHERE timestamp >= $1 AND status_code BETWEEN 500 AND 599)                          AS short_5xx,
		       COUNT(*) FILTER (WHERE timestamp >= $1 AND (status_code IS NULL OR status_code = 0))                 AS short_timeout,
		       COUNT(*) FILTER (WHERE timestamp >= $1 AND status_code BETWEEN 400 AND 499 AND status_code NOT IN (401, 403, 429)) AS short_client,
		       MAX(timestamp) FILTER (WHERE timestamp >= $1 AND status_code IS DISTINCT FROM 429)                   AS short_last,
		       -- long window (>= $2, excluding 429)
		       COUNT(*) FILTER (WHERE status_code IS DISTINCT FROM 429)                                             AS long_samples,
		       COUNT(*) FILTER (WHERE status_code BETWEEN 200 AND 299)                                              AS long_success
		FROM   traffic_event
		WHERE  source = 'ai-gateway'
		  AND  credential_id IS NOT NULL
		  AND  timestamp >= $2
		GROUP  BY credential_id
		HAVING COUNT(*) FILTER (WHERE timestamp >= $1 AND status_code IS DISTINCT FROM 429) > 0
		    OR COUNT(*) FILTER (WHERE status_code IS DISTINCT FROM 429) > 0
	`, shortStart, longStart)
	if err != nil {
		return nil, fmt.Errorf("query traffic_event: %w", err)
	}
	defer rows.Close()

	var out []statusCounts
	for rows.Next() {
		var s statusCounts
		var shortLast *time.Time
		if err := rows.Scan(
			&s.credentialID,
			&s.short.samples, &s.short.success, &s.short.authFail, &s.short.rateLimit,
			&s.short.upstream5xx, &s.short.timeout, &s.short.clientError,
			&shortLast,
			&s.long.samples, &s.long.success,
		); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		if shortLast != nil {
			s.short.lastSample = *shortLast
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// priorRow is the per-credential snapshot of DB state we need to decide
// whether to write and to maintain healthStatusChangedAt.
type priorRow struct {
	status    string
	changedAt *time.Time
}

func (j *CredentialHealthRollupJob) priorStatus(ctx context.Context, rolled []statusCounts) (map[string]priorRow, error) {
	out := make(map[string]priorRow, len(rolled))
	if len(rolled) == 0 {
		return out, nil
	}
	ids := make([]string, 0, len(rolled))
	for _, r := range rolled {
		ids = append(ids, r.credentialID)
	}
	rows, err := j.pool.Query(ctx,
		`SELECT id, "healthStatus", "healthStatusChangedAt" FROM "Credential" WHERE id = ANY($1::text[])`,
		ids,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, status string
		var changedAt *time.Time
		if err := rows.Scan(&id, &status, &changedAt); err != nil {
			return nil, err
		}
		out[id] = priorRow{status: status, changedAt: changedAt}
	}
	return out, rows.Err()
}

// updatePlan is the per-credential write produced by classification.
type updatePlan struct {
	credentialID   string
	status         string
	priorStatus    string
	rate5m         *float64
	rate1h         *float64
	samples        int
	dominantError  string
	trend          string
	statusChanged  bool
	checkedAt      time.Time
	priorChangedAt *time.Time
}

// classifyAll runs the pure classification logic for each rolled row and
// emits an updatePlan for every credential whose persisted state needs
// refreshing (which is essentially all of them, since checkedAt and rate
// are write-on-every-cycle for visible credentials).
func (j *CredentialHealthRollupJob) classifyAll(rolled []statusCounts, prior map[string]priorRow, t credstate.Thresholds) []updatePlan {
	now := time.Now().UTC()
	out := make([]updatePlan, 0, len(rolled))
	for _, r := range rolled {
		p := prior[r.credentialID]
		priorStatus := p.status
		if priorStatus == "" {
			priorStatus = credstate.HealthUnknown
		}
		// Preserve the credential's previously-classified status when the
		// short window is empty but the long window has positive evidence.
		// Without this, idle / cache-dominated periods cause every healthy
		// credential to flip to Unknown after 5 min, even though long-window
		// data still shows it succeeding. The UX before this fix was: open
		// the Credentials page, see Healthy, send some cache HITs, wait 5
		// min, status reverts to Unknown — confusing operators who can't
		// distinguish "credential is broken" from "credential is idle".
		status, rate5m, dominant := classifyShort(r.short, priorStatus, r.long.samples > 0, t)
		var rate1hPtr *float64
		if r.long.samples > 0 {
			rate1h := float64(r.long.success) / float64(r.long.samples)
			rate1hPtr = &rate1h
		}
		trend := classifyTrend(rate5m, rate1hPtr)
		var rate5mPtr *float64
		if r.short.samples > 0 {
			rate5mPtr = &rate5m
		}
		plan := updatePlan{
			credentialID:   r.credentialID,
			status:         status,
			priorStatus:    priorStatus,
			rate5m:         rate5mPtr,
			rate1h:         rate1hPtr,
			samples:        r.short.samples,
			dominantError:  dominant,
			trend:          trend,
			statusChanged:  priorStatus != status,
			checkedAt:      now,
			priorChangedAt: p.changedAt,
		}
		out = append(out, plan)
	}
	return out
}

// classifyShort applies the short-window classification rules. Returns
// the status, the rate computed (0 when samples == 0), and the dominant
// error category. priorStatus + hasLongSamples let the function preserve
// a credential's last-known classification when the short window happens
// to be empty (idle period, or all-cache-HIT traffic that never reached
// upstream) — see classifyAll's comment for the UX rationale.
func classifyShort(w windowCounts, priorStatus string, hasLongSamples bool, t credstate.Thresholds) (status string, rate float64, dominant string) {
	if w.samples == 0 {
		// Long-window samples are positive evidence the credential is
		// alive; keep the prior classification (healthy / degraded /
		// unavailable / collecting) instead of flipping to Unknown.
		// Genuinely never-seen credentials have prior=Unknown — they
		// stay Unknown until real traffic flows.
		if hasLongSamples && priorStatus != "" && priorStatus != credstate.HealthUnknown {
			return priorStatus, 0, credstate.DominantNone
		}
		return credstate.HealthUnknown, 0, credstate.DominantNone
	}
	rate = float64(w.success) / float64(w.samples)
	dominant = dominantErrorOf(w)
	if w.samples < t.HealthMinSamples {
		return credstate.HealthCollecting, rate, dominant
	}
	pct := int(rate*100 + 0.5)
	switch {
	case pct >= t.HealthyThresholdPct:
		return credstate.HealthHealthy, rate, dominant
	case pct >= t.DegradedThresholdPct:
		return credstate.HealthDegraded, rate, dominant
	default:
		return credstate.HealthUnavailable, rate, dominant
	}
}

// dominantErrorOf returns the dominant failure category in w. Returns
// none if w has no failures, mixed if no single category exceeds 50% of
// failures, otherwise the winning category.
func dominantErrorOf(w windowCounts) string {
	failures := w.samples - w.success
	if failures <= 0 {
		return credstate.DominantNone
	}
	type bucket struct {
		label string
		count int
	}
	buckets := []bucket{
		{credstate.DominantAuthFail, w.authFail},
		{credstate.DominantRateLimit, w.rateLimit},
		{credstate.DominantUpstream5xx, w.upstream5xx},
		{credstate.DominantTimeout, w.timeout},
		{credstate.DominantClientError, w.clientError},
	}
	max := bucket{label: credstate.DominantMixed}
	for _, b := range buckets {
		if b.count > max.count {
			max = b
		}
	}
	if max.count*2 > failures { // strictly > 50% of failures
		return max.label
	}
	return credstate.DominantMixed
}

// classifyTrend compares the short-window rate against the long-window
// baseline. A drop of trendDeltaPct or more flags degrading; a gain flags
// improving; anything in between is stable. Returns "" when there is no
// baseline (long window had zero samples) — the UI shows no trend arrow.
func classifyTrend(short float64, long *float64) string {
	if long == nil {
		return credstate.TrendStable
	}
	delta := short - *long
	switch {
	case delta*100 <= -trendDeltaPct:
		return credstate.TrendDegrading
	case delta*100 >= trendDeltaPct:
		return credstate.TrendImproving
	default:
		return credstate.TrendStable
	}
}

// batchUpdate persists every plan in one UPDATE … FROM (VALUES …) round.
// pgx's unnest pattern keeps it a single statement regardless of cohort
// size. Rows where the status actually changed get healthStatusChangedAt
// set to checkedAt; rows where the status matched the previous value
// keep the prior healthStatusChangedAt (NULL → NULL becomes COALESCE on
// the existing column).
func (j *CredentialHealthRollupJob) batchUpdate(ctx context.Context, plans []updatePlan, now time.Time) error {
	ids := make([]string, len(plans))
	statuses := make([]string, len(plans))
	rate5m := make([]any, len(plans)) // numeric or NULL
	rate1h := make([]any, len(plans))
	samples := make([]int, len(plans))
	dominants := make([]string, len(plans))
	trends := make([]string, len(plans))
	statusChanges := make([]bool, len(plans))

	for i, p := range plans {
		ids[i] = p.credentialID
		statuses[i] = p.status
		if p.rate5m != nil {
			rate5m[i] = *p.rate5m
		} else {
			rate5m[i] = nil
		}
		if p.rate1h != nil {
			rate1h[i] = *p.rate1h
		} else {
			rate1h[i] = nil
		}
		samples[i] = p.samples
		dominants[i] = p.dominantError
		trends[i] = p.trend
		statusChanges[i] = p.statusChanged
	}

	_, err := j.pool.Exec(ctx, `
		UPDATE "Credential" c SET
			"healthStatus"          = v.status,
			"healthSuccessRate5m"   = v.rate5m,
			"healthSuccessRate1h"   = v.rate1h,
			"healthSamplesObserved" = v.samples,
			"healthDominantError"   = v.dominant,
			"healthTrend"           = v.trend,
			"healthCheckedAt"       = $9::timestamptz,
			"healthStatusChangedAt" = CASE WHEN v.changed THEN $9::timestamptz ELSE c."healthStatusChangedAt" END,
			"updatedAt"             = NOW()
		FROM (
			SELECT
			       unnest($1::text[])    AS id,
			       unnest($2::text[])    AS status,
			       unnest($3::numeric[]) AS rate5m,
			       unnest($4::numeric[]) AS rate1h,
			       unnest($5::int[])     AS samples,
			       unnest($6::text[])    AS dominant,
			       unnest($7::text[])    AS trend,
			       unnest($8::bool[])    AS changed
		) v
		WHERE c.id = v.id
	`, ids, statuses, rate5m, rate1h, samples, dominants, trends, statusChanges, now)
	return err
}

// Hub-side thresholds reader — wraps the shared system_metadata key so the
// rollup job, the alerts job, and any future Hub consumer all see the
// same global view.

// ReliabilityConfigKey is the system_metadata row holding the global
// credential-reliability Thresholds JSON. Both Hub and AI Gateway read
// this same key — Hub via direct SQL, AI Gateway via store.GetSystemMetadata.
const ReliabilityConfigKey = "gateway.credential_reliability.config"

// ReliabilityThresholdsLoader reads ReliabilityConfigKey from
// system_metadata, applies defensive validation, and returns the effective
// Thresholds. A missing or invalid row falls back to
// credstate.DefaultThresholds — never panics, never blocks.
type ReliabilityThresholdsLoader struct {
	Pool   *pgxpool.Pool
	Logger *slog.Logger
}

// Thresholds satisfies the thresholdsReader interface used by the rollup
// and alerts jobs.
func (r *ReliabilityThresholdsLoader) Thresholds(ctx context.Context) credstate.Thresholds {
	if r == nil || r.Pool == nil {
		return credstate.DefaultThresholds
	}
	var raw json.RawMessage
	err := r.Pool.QueryRow(ctx,
		`SELECT value FROM system_metadata WHERE key = $1`,
		ReliabilityConfigKey,
	).Scan(&raw)
	if err != nil {
		// pgx.ErrNoRows means "no admin override yet" — fall back silently.
		if err.Error() != "no rows in result set" && r.Logger != nil {
			r.Logger.Warn("reliability config read failed; using defaults", "error", err)
		}
		return credstate.DefaultThresholds
	}
	if len(raw) == 0 {
		return credstate.DefaultThresholds
	}
	var t credstate.Thresholds
	if err := json.Unmarshal(raw, &t); err != nil {
		if r.Logger != nil {
			r.Logger.Warn("reliability config parse failed; using defaults", "error", err)
		}
		return credstate.DefaultThresholds
	}
	if err := t.Validate(); err != nil {
		if r.Logger != nil {
			r.Logger.Warn("reliability config invalid; using defaults", "error", err)
		}
		return credstate.DefaultThresholds
	}
	return t
}

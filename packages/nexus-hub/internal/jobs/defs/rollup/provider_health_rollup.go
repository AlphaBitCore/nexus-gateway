package rollup

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	defs "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/jobs/defs"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	providerHealthRollupJobID          = "provider-health-rollup"
	providerHealthRollupJobName        = "Provider Health Rollup"
	providerHealthRollupJobDescription = "Recomputes ProviderHealth (error rate, avg latency, sample count, status) from traffic_event over a 30-minute rolling window. Replaces the AI Gateway in-process HealthTracker DB flush."

	providerHealthWindow            = 30 * time.Minute
	providerHealthDegradedThreshold = 0.05
	providerHealthUnavailThreshold  = 0.25

	// providerHealthMinSamples is the evidence a verdict against a provider
	// needs before it is worth stating.
	//
	// A rate over a handful of requests is not a rate. With no floor, one
	// failed request in a thirty-minute window put the provider at 100% error
	// and marked it unavailable — which fired the provider.unavailable alert
	// and, in the UI, condemned a provider that nothing was wrong with. An
	// operator then had nothing to click: this status is DERIVED from the
	// window and cannot be reset by hand, so it looked stuck until the window
	// rolled the request off.
	//
	// Twenty means the unavailable threshold needs at least six failures
	// rather than one, which is a signal instead of an accident. Below the
	// floor no adverse verdict is stated at all; sampleCount is written
	// regardless, so a reader can see the verdict rests on little traffic.
	providerHealthMinSamples = 20
)

// ProviderHealthRollupJob queries the last 30 minutes of ai-gateway traffic
// events, computes per-provider health metrics, and upserts the ProviderHealth
// table. Running in Hub means: (a) restarts don't clear state, (b) multi-
// instance deployments see an aggregate view, (c) the status page and the
// Usage tab share a common data source (traffic_event).
type ProviderHealthRollupJob struct {
	// pool is typed against the package-level defs.PgxPool seam so the
	// traffic_event SELECT + per-provider UPSERT chain is unit-testable
	// without sharing real traffic data.
	pool     defs.PgxPool
	interval time.Duration
	logger   *slog.Logger
}

func NewProviderHealthRollup(pool *pgxpool.Pool, interval time.Duration, logger *slog.Logger) *ProviderHealthRollupJob {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	return &ProviderHealthRollupJob{
		pool:     pool,
		interval: interval,
		logger:   logger.With("job", providerHealthRollupJobID),
	}
}

func (j *ProviderHealthRollupJob) ID() string              { return providerHealthRollupJobID }
func (j *ProviderHealthRollupJob) Name() string            { return providerHealthRollupJobName }
func (j *ProviderHealthRollupJob) Description() string     { return providerHealthRollupJobDescription }
func (j *ProviderHealthRollupJob) Interval() time.Duration { return j.interval }

type providerHealthRow struct {
	providerID    string
	providerName  string
	total         int
	errors        int
	avgLatencyMs  int
	lastRequestAt time.Time
	lastErrorAt   *time.Time
}

func (j *ProviderHealthRollupJob) Run(ctx context.Context) error {
	windowStart := time.Now().UTC().Add(-providerHealthWindow)

	rows, err := j.collect(ctx, windowStart)
	if err != nil {
		return fmt.Errorf("collect: %w", err)
	}
	for _, r := range rows {
		if err := j.upsert(ctx, r, windowStart); err != nil {
			j.logger.Error("upsert provider health", "providerId", r.providerID, "error", err)
		}
	}
	// Runs whether or not anything had traffic — an empty window is exactly
	// when every remaining verdict is stale, and the early return that used to
	// sit here skipped that case.
	if err := j.clearStaleVerdicts(ctx, windowStart); err != nil {
		j.logger.Error("clear stale provider health", "error", err)
	}
	j.logger.Info("provider health rollup complete", "providers", len(rows))
	return nil
}

// clearStaleVerdicts resets any provider the current window has nothing to say
// about.
//
// The collect query GROUPs traffic BY provider, so it only ever upserts
// providers that appear IN the window. Without this, a provider that stops
// receiving traffic keeps whatever status it last got forever — and because the
// status is derived, an operator cannot clear it by hand. Seen in production:
// google-gemini frozen at `unavailable` on a sampleCount of 1, hours after the
// single failed request that produced it, while later runs updated the
// providers that did have traffic and left that row alone.
//
// The reset lands on `healthy` with a zeroed sample count for the same reason
// the evidence floor does: no traffic in the window is not evidence of failure,
// `healthy` is the weaker of the two claims available, and the row still
// carries sampleCount so a reader can see the verdict rests on nothing. This
// table feeds the UI and alerts only — routing uses the gateway's own
// in-process window — so a cleared verdict cannot misroute traffic.
func (j *ProviderHealthRollupJob) clearStaleVerdicts(ctx context.Context, windowStart time.Time) error {
	_, err := j.pool.Exec(ctx, `
		UPDATE "ProviderHealth"
		   SET status             = 'healthy',
		       "rollingErrorRate" = 0,
		       "sampleCount"      = 0,
		       "windowStart"      = $1,
		       "updatedAt"        = NOW()
		 WHERE "windowStart" < $1
		   AND (status <> 'healthy' OR "sampleCount" <> 0)
	`, windowStart)
	return err
}

func (j *ProviderHealthRollupJob) collect(ctx context.Context, windowStart time.Time) ([]providerHealthRow, error) {
	// Use routed_provider_id when available (actual provider that handled the
	// request after routing rules), falling back to provider_id (original target).
	q := `
		SELECT
			COALESCE(routed_provider_id, provider_id)                              AS pid,
			COALESCE(routed_provider_name, provider_name,
				COALESCE(routed_provider_id, provider_id)::text)                   AS pname,
			COUNT(*)                                                               AS total,
			COUNT(*) FILTER (WHERE status_code >= 400)                            AS errors,
			COALESCE(AVG(latency_ms)::int, 0)                                      AS avg_latency_ms,
			MAX(timestamp)                                                         AS last_request_at,
			MAX(timestamp) FILTER (WHERE status_code >= 400)                      AS last_error_at
		FROM traffic_event
		WHERE source = 'ai-gateway'
		  AND timestamp >= $1
		  AND (provider_id IS NOT NULL OR routed_provider_id IS NOT NULL)
		GROUP BY 1, 2
	`
	pgRows, err := j.pool.Query(ctx, q, windowStart)
	if err != nil {
		return nil, fmt.Errorf("query traffic_event: %w", err)
	}
	defer pgRows.Close()

	var result []providerHealthRow
	for pgRows.Next() {
		var r providerHealthRow
		if err := pgRows.Scan(&r.providerID, &r.providerName, &r.total, &r.errors,
			&r.avgLatencyMs, &r.lastRequestAt, &r.lastErrorAt); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		result = append(result, r)
	}
	return result, pgRows.Err()
}

// healthStatus is the verdict, separated from the write so the thresholds and
// the evidence floor can be exercised without a database.
func healthStatus(total int, errorRate float64) string {
	if total < providerHealthMinSamples {
		// Not enough traffic to state a rate. Saying "healthy" here is the
		// weaker claim of the two available, and the row carries sampleCount
		// so a reader can see what the verdict rests on.
		return "healthy"
	}
	switch {
	case errorRate > providerHealthUnavailThreshold:
		return "unavailable"
	case errorRate > providerHealthDegradedThreshold:
		return "degraded"
	}
	return "healthy"
}

func (j *ProviderHealthRollupJob) upsert(ctx context.Context, r providerHealthRow, windowStart time.Time) error {
	errorRate := 0.0
	if r.total > 0 {
		errorRate = float64(r.errors) / float64(r.total)
	}

	status := healthStatus(r.total, errorRate)

	_, err := j.pool.Exec(ctx, `
		INSERT INTO "ProviderHealth"
			(id, "providerId", provider, status,
			 "rollingErrorRate", "avgLatencyMs",
			 "lastRequestAt", "lastErrorAt",
			 "windowStart", "sampleCount",
			 "createdAt", "updatedAt")
		VALUES
			(gen_random_uuid(), $1, $2, $3,
			 $4, $5,
			 $6, $7,
			 $8, $9,
			 now(), now())
		ON CONFLICT ("providerId") DO UPDATE SET
			provider          = $2,
			status            = $3,
			"rollingErrorRate" = $4,
			"avgLatencyMs"    = $5,
			"lastRequestAt"   = $6,
			"lastErrorAt"     = CASE WHEN $7 IS NOT NULL THEN $7 ELSE "ProviderHealth"."lastErrorAt" END,
			"windowStart"     = $8,
			"sampleCount"     = $9,
			"updatedAt"       = now()
	`, r.providerID, r.providerName, status,
		errorRate, r.avgLatencyMs,
		r.lastRequestAt, r.lastErrorAt,
		windowStart, r.total)
	return err
}

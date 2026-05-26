package health

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	defs "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/jobs/defs"
)


const (
	cacheQualityJobID          = "cache-quality-monitor"
	cacheQualityJobName        = "Cache Quality Monitor"
	cacheQualityJobDescription = "Detects elevated error rates in normaliser-modified requests over the past 30 minutes and auto-reverts all active rules to dry_run_always when the error rate exceeds 3× baseline, preventing normaliser-induced quality regressions."

	// errorRateMultiplierThreshold: if normalised-error-rate > baseline * N, trigger revert.
	errorRateMultiplierThreshold = 3.0

	// minNormalisedRequests is the minimum number of normalised requests needed
	// to trigger the comparison (avoids false positives on very low volume).
	minNormalisedRequests = 20
)

// CacheQualityMonitorJob periodically checks whether normaliser-modified
// requests have a significantly elevated error rate compared to the baseline,
// and auto-reverts all rules to dry_run_always if so.
type CacheQualityMonitorJob struct {
	// pool is typed against the package-level defs.PgxPool seam so the
	// stats-query + revert-cascade are unit-testable via pgxmock.
	pool     defs.PgxPool
	interval time.Duration
	logger   *slog.Logger
}

// NewCacheQualityMonitor constructs the job. interval defaults to 5 minutes.
func NewCacheQualityMonitor(pool *pgxpool.Pool, interval time.Duration, logger *slog.Logger) *CacheQualityMonitorJob {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	return &CacheQualityMonitorJob{
		pool:     pool,
		interval: interval,
		logger:   logger.With("job", cacheQualityJobID),
	}
}

func (j *CacheQualityMonitorJob) ID() string              { return cacheQualityJobID }
func (j *CacheQualityMonitorJob) Name() string            { return cacheQualityJobName }
func (j *CacheQualityMonitorJob) Description() string     { return cacheQualityJobDescription }
func (j *CacheQualityMonitorJob) Interval() time.Duration { return j.interval }

func (j *CacheQualityMonitorJob) Run(ctx context.Context) error {
	window := time.Now().UTC().Add(-30 * time.Minute)

	// Count normalised requests and their error rate in the last 30 minutes.
	// A request is "normalised" if the normaliser touched it (strip or inject).
	var totalNorm, errorNorm, totalAll, errorAll int64
	err := j.pool.QueryRow(ctx, `
		SELECT
			COUNT(*) FILTER (WHERE (normalized_strip_count > 0 OR cache_marker_injected > 0)),
			COUNT(*) FILTER (WHERE (normalized_strip_count > 0 OR cache_marker_injected > 0)
			                   AND status_code >= 400),
			COUNT(*),
			COUNT(*) FILTER (WHERE status_code >= 400)
		FROM traffic_event
		WHERE timestamp >= $1
	`, window).Scan(&totalNorm, &errorNorm, &totalAll, &errorAll)
	if err != nil {
		return fmt.Errorf("cache quality monitor: stats query: %w", err)
	}

	j.logger.Debug("cache quality stats",
		"window_start", window,
		"total_norm", totalNorm,
		"error_norm", errorNorm,
		"total_all", totalAll,
		"error_all", errorAll,
	)

	if totalNorm < minNormalisedRequests {
		return nil // not enough data; skip
	}

	normErrorRate := float64(errorNorm) / float64(totalNorm)
	var baselineErrorRate float64
	if totalAll > 0 {
		baselineErrorRate = float64(errorAll) / float64(totalAll)
	}

	// No regression if baseline itself is zero or normErrorRate is not alarming.
	if baselineErrorRate == 0 {
		baselineErrorRate = 0.01 // floor to avoid division/zero-comparison issues
	}
	if normErrorRate <= baselineErrorRate*errorRateMultiplierThreshold {
		return nil // within acceptable range
	}

	j.logger.Warn("cache quality regression detected — reverting normaliser rules to dry-run",
		"normalised_error_rate", normErrorRate,
		"baseline_error_rate", baselineErrorRate,
		"multiplier", normErrorRate/baselineErrorRate,
	)

	return j.revertToDryRun(ctx)
}

// revertToDryRun iterates every `cache_adapter_config` row that carries rule
// overrides and sets `dry_run_always=true` on each rule whose `enabled=true`.
// Rules now live under `cache_adapter_config.config.rules` (one row per
// adapter_type) instead of nested in a single legacy
// `system_metadata['prompt_cache']` blob. The CP-side reconcile job picks up
// the resulting drift within 60s and propagates the new config to the gateway
// via Hub.NotifyConfigChange; this fail-safe does not need to push directly.
func (j *CacheQualityMonitorJob) revertToDryRun(ctx context.Context) error {
	rows, err := j.pool.Query(ctx,
		`SELECT adapter_type, config FROM cache_adapter_config WHERE config ? 'rules'`)
	if err != nil {
		return fmt.Errorf("cache quality monitor: read cache_adapter_config: %w", err)
	}
	type pendingUpdate struct {
		adapterType string
		marshalled  []byte
	}
	var pending []pendingUpdate
	for rows.Next() {
		var adapterType string
		var raw []byte
		if err := rows.Scan(&adapterType, &raw); err != nil {
			rows.Close()
			return fmt.Errorf("cache quality monitor: scan row: %w", err)
		}
		var cfg map[string]any
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return fmt.Errorf("cache quality monitor: unmarshal cache_adapter_config[%s]: %w", adapterType, err)
		}
		rulesRaw, ok := cfg["rules"]
		if !ok {
			continue
		}
		rules, ok := rulesRaw.(map[string]any)
		if !ok {
			continue
		}
		changedHere := false
		for ruleID, ruleRaw := range rules {
			ruleMap, ok := ruleRaw.(map[string]any)
			if !ok {
				continue
			}
			if en, ok := ruleMap["enabled"]; ok {
				if enBool, ok := en.(bool); ok && enBool {
					ruleMap["dry_run_always"] = true
					changedHere = true
					j.logger.Info("cache quality monitor: setting dry_run_always",
						"adapter_type", adapterType, "rule_id", ruleID)
				}
			}
		}
		if !changedHere {
			continue
		}
		updated, err := json.Marshal(cfg)
		if err != nil {
			return fmt.Errorf("cache quality monitor: marshal cache_adapter_config[%s]: %w", adapterType, err)
		}
		pending = append(pending, pendingUpdate{adapterType: adapterType, marshalled: updated})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("cache quality monitor: row iteration: %w", err)
	}
	rows.Close()

	if len(pending) == 0 {
		j.logger.Info("cache quality monitor: no enabled rules to revert")
		return nil
	}

	for _, p := range pending {
		if _, err := j.pool.Exec(ctx, `
			UPDATE cache_adapter_config
			SET config = $2, updated_at = NOW(), updated_by = 'cache-quality-monitor'
			WHERE adapter_type = $1
		`, p.adapterType, p.marshalled); err != nil {
			return fmt.Errorf("cache quality monitor: write cache_adapter_config[%s]: %w", p.adapterType, err)
		}
	}

	j.logger.Warn("cache quality monitor: normaliser rules reverted to dry-run mode; review and re-enable manually",
		"adapters_updated", len(pending))
	return nil
}

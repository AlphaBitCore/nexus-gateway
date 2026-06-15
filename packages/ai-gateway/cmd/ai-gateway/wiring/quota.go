// quota.go — quota engine + policy cache wiring.
package wiring

import (
	"context"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/quota"
)

// InitQuota constructs and seeds the policy cache and quota engine.
// Returns (nil, nil) when db is nil — quota enforcement is skipped in
// degraded mode without a database. metrics may be nil (observability
// disabled); production boot passes a registered *quota.Metrics.
func InitQuota(ctx context.Context, db *store.DB, rdb redis.UniversalClient, logger *slog.Logger, metrics *quota.Metrics) (*quota.Engine, *quota.PolicyCache) {
	if db == nil {
		return nil, nil
	}
	policyCache := quota.NewPolicyCache(db.Pool, logger)
	if err := policyCache.Load(ctx); err != nil {
		// A boot-time policy-load failure must NOT leave the engine
		// silently enforcing nothing forever. Boot still proceeds (so a DB hiccup
		// doesn't brick the gateway), but a background loop retries Load with
		// backoff until the first success, so the unenforced (fail-open) window
		// self-heals in seconds instead of persisting until an admin edits a
		// quota policy. Engine.Check meanwhile emits quota_check_failopen_total
		// {reason="policy_cache_unloaded"} so the window is alertable.
		logger.Error("failed to load quota policies; retrying in background until loaded", "error", err)
		go retryPolicyLoadUntilLoaded(ctx, policyCache, logger)
	}
	usageCache := quota.NewUsageCache(rdb, logger)
	// Re-seed the current period of every period type actually in use (daily /
	// weekly / monthly), not just monthly, so non-monthly counters survive a
	// restart.
	if err := usageCache.Backfill(ctx, db.Pool, policyCache.ActivePeriodTypes(), logger); err != nil {
		logger.Error("usage cache backfill failed", "error", err)
	}
	quotaEngine := quota.NewEngine(policyCache, usageCache, logger, metrics)
	return quotaEngine, policyCache
}

// retryPolicyLoadUntilLoaded re-attempts PolicyCache.Load with exponential
// backoff (1s → 2s → … → 60s cap) until the first success or ctx cancellation.
// It exits as soon as the cache reports Loaded, so the unenforced
// fail-open window after a boot-time DB failure is bounded to seconds rather than
// persisting until an admin happens to edit a quota policy. Ongoing reloads after
// the first success are driven by the existing Hub config-change broadcast.
func retryPolicyLoadUntilLoaded(ctx context.Context, cache *quota.PolicyCache, logger *slog.Logger) {
	backoff := time.Second
	const maxBackoff = 60 * time.Second
	for {
		if cache.Loaded() {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if err := cache.Load(ctx); err != nil {
			logger.Error("quota policy cache reload retry failed", "error", err, "next_retry", backoff.String())
			if backoff < maxBackoff {
				backoff *= 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
			}
			continue
		}
		logger.Info("quota policy cache recovered after boot-time load failure")
		return
	}
}

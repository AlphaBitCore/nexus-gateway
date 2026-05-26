// quota.go — quota engine + policy cache wiring.
package wiring

import (
	"context"
	"log/slog"

	"github.com/redis/go-redis/v9"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/quota"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
)

// InitQuota constructs and seeds the policy cache and quota engine.
// Returns (nil, nil) when db is nil — quota enforcement is skipped in
// degraded mode without a database.
func InitQuota(ctx context.Context, db *store.DB, rdb redis.UniversalClient, logger *slog.Logger) (*quota.Engine, *quota.PolicyCache) {
	if db == nil {
		return nil, nil
	}
	policyCache := quota.NewPolicyCache(db.Pool, logger)
	if err := policyCache.Load(ctx); err != nil {
		logger.Error("failed to load quota policies", "error", err)
	}
	usageCache := quota.NewUsageCache(rdb, logger)
	if err := usageCache.Backfill(ctx, db.Pool, logger); err != nil {
		logger.Error("usage cache backfill failed", "error", err)
	}
	quotaEngine := quota.NewEngine(policyCache, usageCache, logger)
	return quotaEngine, policyCache
}

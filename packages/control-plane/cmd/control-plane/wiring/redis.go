package wiring

import (
	"context"
	"log/slog"

	"github.com/redis/go-redis/v9"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/cmd/control-plane/config"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/redisfactory"
)

// InitRedis builds a [redis.UniversalClient] from the universal Redis
// configuration (yaml + env) and pings it. Returns (nil, no-op-closer, nil)
// when no addrs are configured or the ping fails — CP degrades gracefully
// to no-Redis mode (sessions and IAM cache fall back to in-memory paths).
// The caller must always call the closer on shutdown.
func InitRedis(ctx context.Context, cfg *config.Config, logger *slog.Logger) (redis.UniversalClient, func(), error) {
	noop := func() {}
	env := redisfactory.LoadEnv()
	// Merge env over yaml to make the "no addrs anywhere" check meaningful —
	// if env supplied addrs we should still try to connect even when yaml is
	// empty. The factory's own merge does the same, but we need the merged
	// view here for the empty-check.
	merged := cfg.Redis
	if env.Addrs != nil {
		merged.Addrs = env.Addrs
	}
	if len(merged.Addrs) == 0 {
		logger.Info("Redis not configured (no addrs in yaml or env) — running without Redis")
		return nil, noop, nil
	}
	client, err := redisfactory.New(cfg.Redis, env, logger)
	if err != nil {
		return nil, noop, err
	}
	if err := client.Ping(ctx).Err(); err != nil {
		logger.Warn("Redis ping failed — running without Redis", "error", err)
		_ = client.Close()
		return nil, noop, nil
	}
	logger.Info("Redis connected")
	return client, func() { _ = client.Close() }, nil
}

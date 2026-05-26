package wiring

import (
	"context"
	"log/slog"

	"github.com/redis/go-redis/v9"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/config"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/redisfactory"
)

// InitRedis builds a [redis.UniversalClient] from the universal Redis
// configuration (yaml + env) and pings it. Returns nil when no addrs are
// configured anywhere — callers fall back to local-only mode (response
// cache and rate-limit degrade to in-memory). Returns nil also when the
// ping fails so the rest of the gateway can still serve traffic.
func InitRedis(ctx context.Context, cfg *config.Config) redis.UniversalClient {
	env := redisfactory.LoadEnv()
	merged := cfg.Redis
	if env.Addrs != nil {
		merged.Addrs = env.Addrs
	}
	if len(merged.Addrs) == 0 {
		return nil
	}
	rdb, err := redisfactory.New(cfg.Redis, env, slog.Default())
	if err != nil {
		slog.Warn("redis factory failed, continuing without redis", "error", err)
		return nil
	}
	if err := rdb.Ping(ctx).Err(); err != nil {
		slog.Warn("redis connection failed, continuing without redis", "error", err)
		_ = rdb.Close()
		return nil
	}
	slog.Info("redis connected", "addrs", merged.Addrs)
	return rdb
}

package wiring

import (
	"context"
	"log/slog"

	"github.com/redis/go-redis/v9"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/config"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/redisfactory"
)

// InitRedis builds a [redis.UniversalClient] from the universal Redis
// configuration (yaml + env) and pings it. Returns nil (not an error) when
// Redis is unavailable so callers can degrade gracefully — Hub operates
// without Redis when the cache is down. The returned client must be closed
// by the caller.
func InitRedis(ctx context.Context, cfg *config.HubConfig, logger *slog.Logger) (redis.UniversalClient, error) {
	client, err := redisfactory.New(cfg.Redis, redisfactory.LoadEnv(), logger)
	if err != nil {
		return nil, err
	}
	if err := client.Ping(ctx).Err(); err != nil {
		logger.Warn("redis ping failed, continuing without Redis", "error", err)
		_ = client.Close()
		return nil, nil //nolint:nilerr // intentional: Redis is optional
	}
	logger.Info("redis connected")
	return client, nil
}

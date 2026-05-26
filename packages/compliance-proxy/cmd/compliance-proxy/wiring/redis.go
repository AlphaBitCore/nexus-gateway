// Package wiring assembles compliance-proxy subsystems from config.
// Each file in this package initialises one bounded sub-system and
// exports a single Init<Subsystem>(...) function consumed by main.go.
package wiring

import (
	"context"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/cmd/compliance-proxy/config"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/redisfactory"
)

// InitRedis builds a [redis.UniversalClient] from the universal Redis
// configuration (yaml + env) and pings it. Returns nil when no addrs are
// configured anywhere or the ping fails — compliance-proxy degrades to a
// local-only cert cache without Redis.
func InitRedis(cfg *config.Config, logger *slog.Logger) redis.UniversalClient {
	env := redisfactory.LoadEnv()
	merged := cfg.Redis
	if env.Addrs != nil {
		merged.Addrs = env.Addrs
	}
	if len(merged.Addrs) == 0 {
		return nil
	}
	client, err := redisfactory.New(cfg.Redis, env, logger)
	if err != nil {
		slog.Error("Redis factory failed, continuing with local-only cert cache", "error", err)
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		slog.Warn("Redis not available at startup, continuing with local-only cert cache", "error", err)
		_ = client.Close() // release connection pool goroutines and FDs
		return nil
	}
	slog.Info("Redis connected", "addrs", merged.Addrs)
	return client
}

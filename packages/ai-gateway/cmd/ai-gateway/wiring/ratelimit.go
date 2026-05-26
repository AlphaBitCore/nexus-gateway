// ratelimit.go — local + Redis rate limiter wiring.
package wiring

import (
	"log/slog"

	"github.com/redis/go-redis/v9"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/ratelimit"
)

// InitRateLimiter creates a distributed (Redis-backed) or local-only limiter.
func InitRateLimiter(rdb redis.UniversalClient, logger *slog.Logger) *ratelimit.Limiter {
	if rdb != nil {
		return ratelimit.New(rdb, logger)
	}
	return ratelimit.NewLocalOnly(logger)
}

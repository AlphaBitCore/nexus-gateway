package wiring

import (
	"log/slog"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/iam"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/users/iamstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/store"
	"github.com/redis/go-redis/v9"
)

// InitIAM builds the IAM engine backed by Postgres and, optionally, Redis for
// caching. Returns nil when db is nil so callers can skip IAM-gated routes
// gracefully.
func InitIAM(db *store.DB, redisClient redis.UniversalClient, logger *slog.Logger) *iam.Engine {
	if db == nil {
		return nil
	}
	var opts []iam.EngineOption
	if redisClient != nil {
		opts = append(opts, iam.WithRedis(redisClient))
	}
	return iam.NewEngine(iamstore.New(db.InternalPool()), logger, opts...)
}

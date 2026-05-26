package wiring

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/access"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/config/cache"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/config/loaders"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/domain"
)

// RegisterCacheLoaders registers the CategoryInterceptionDomains,
// CategoryAllowlists, and CategoryObservability loaders on the cache manager,
// eagerly loads the domain allowlist from DB, and swaps it onto the access
// checker. Returns the domain engine ready for use.
//
// No-op when configDB is nil (compliance disabled / no DB configured).
func RegisterCacheLoaders(
	configDB *sql.DB,
	cacheManager *cache.Manager,
	domainEngine *domain.Engine,
	accessChecker *access.Checker,
	logger *slog.Logger,
) {
	if configDB == nil {
		return
	}

	cacheManager.RegisterLoader(cache.CategoryInterceptionDomains, func(ctx context.Context) (interface{}, error) {
		domains, err := loaders.LoadInterceptionDomainsFull(ctx, configDB)
		if err != nil {
			return nil, err
		}
		if err := domainEngine.Swap(domains); err != nil {
			return nil, fmt.Errorf("swap domain engine: %w", err)
		}
		return domains, nil
	})
	cacheManager.RegisterLoader(cache.CategoryAllowlists, func(ctx context.Context) (interface{}, error) {
		domains, err := loaders.LoadInterceptionDomainsFull(ctx, configDB)
		if err != nil {
			return nil, err
		}
		if err := domainEngine.Swap(domains); err != nil {
			return nil, fmt.Errorf("swap domain engine: %w", err)
		}
		return domainEngine.AllowlistEntries(), nil
	})
	cacheManager.RegisterLoader(cache.CategoryObservability, func(ctx context.Context) (interface{}, error) {
		return loaders.LoadObservabilityConfig(ctx, configDB)
	})

	// Eagerly load DB domain entries on startup and merge with YAML.
	initCtx, initCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer initCancel()
	if dbEntries, err := cacheManager.Get(initCtx, cache.CategoryAllowlists); err == nil {
		if entries, ok := dbEntries.([]string); ok {
			accessChecker.SwapDomainAllowlist(entries, logger)
		}
	} else {
		slog.Warn("failed to load initial domain allowlist from DB, using YAML only", "error", err)
	}
}

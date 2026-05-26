// semantic.go — wires the L2 semantic cache subsystem into boot.go.
//
// All components are nil-safe: when the Redis client is not a *redis.Client
// (e.g. Sentinel), SemanticReader and SemanticWriter remain nil and the proxy
// silently skips L2.
package wiring

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/budget"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/freshness"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/semantic"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/embeddings"
)

// SemanticDeps groups the L2 semantic cache subsystem returned by InitSemantic.
type SemanticDeps struct {
	ConfigCache    *semantic.ConfigCache
	IndexLifecycle *semantic.IndexLifecycle
	Reader         *semantic.Reader
	Writer         *semantic.Writer
	BudgetTracker  *budget.Tracker
	Detector       *freshness.Detector
}

// InitSemantic constructs all L2 semantic cache components.
//
// rdb must be a *redis.Client (not a cluster or Sentinel client); when it is
// nil or the type assertion fails, Reader and Writer are left nil and L2 is
// silently disabled. httpClient is used for embedding calls. namespace is the
// Prometheus metric namespace (e.g. "nexus").
func InitSemantic(
	rdb redis.UniversalClient,
	httpClient *http.Client,
	namespace string,
	logger *slog.Logger,
) SemanticDeps {
	// Freshness detector is always constructed — it operates on compiled
	// regexp patterns, independent of Redis.  Initial rule set is empty;
	// the configdispatch callback populates it when Hub pushes
	// response_cache.time_sensitive_patterns.
	detector, detectorErr := freshness.NewDetector(nil, logger, namespace, prometheus.DefaultRegisterer)
	if detectorErr != nil {
		logger.Warn("semantic cache: freshness detector init failed; continuing without freshness check",
			"error", detectorErr)
		detector = nil
	}

	// Downcast UniversalClient to *redis.Client; degrade if it's Sentinel/Cluster.
	rdbClient, ok := rdb.(*redis.Client)
	if rdb == nil || !ok || rdbClient == nil {
		logger.Warn("semantic cache: Redis client is not *redis.Client; L2 disabled")
		return SemanticDeps{
			ConfigCache: semantic.NewConfigCache(),
			Detector:    detector,
		}
	}

	// Circuit breaker registry — one breaker per (providerID, modelID) pair per
	// response-cache-architecture.md §3.3. Thresholds: 10 consecutive failures
	// within 60 s → open; 30 s cooldown.
	cbRegistry := semantic.NewCircuitBreakerRegistry(
		10,             // failure threshold
		60*time.Second, // failure window
		30*time.Second, // half-open after
		logger,
		namespace,
	)

	// Embedding client — shared HTTP client + prometheus instruments.
	embedClient := embeddings.NewClient(httpClient, logger, namespace)

	// Singleflight deduplicates concurrent embedding calls with identical
	// (model, input) so we don't flood the embedding provider.
	sf := semantic.NewEmbeddingSingleflight(embedClient, cbRegistry, 0 /* use default 100ms */, logger)

	// ConfigCache: atomic in-process snapshot; populated by the Hub shadow callback.
	cfgCache := semantic.NewConfigCache()

	// Valkey client wrapper (index management + KV store).
	client := semantic.NewClient(rdbClient, logger, namespace, nil)

	// IndexLifecycle: watches fingerprint changes and calls EnsureIndex.
	lifecycle := semantic.NewIndexLifecycle(cfgCache, client, logger)

	// Metrics instruments shared between reader and writer.
	metrics := semantic.NewMetrics(namespace)

	// Reader wires the PoisonList (negative-feedback path). When an admin POSTs
	// /api/admin/cache/semantic-feedback the CP writes "nexus:l2:poison:<vk>:<entryKey>";
	// the Reader's IsPoisoned check (lookup.go Step 5) consults that key on every
	// FT.SEARCH hit and downgrades a would-be HIT to "skip_poisoned".
	// Using NewReader (no-poison constructor) would silently substitute nopPoisonList
	// and disable the feature — NewReaderWithPoison must be used here.
	poison := semantic.NewRedisPoisonList(rdbClient)
	reader := semantic.NewReaderWithPoison(cfgCache, client, sf, metrics, poison)

	// Writer orchestrates the L2 write-back flow.
	writer := semantic.NewWriter(cfgCache, client, sf, logger, 0 /* defaultMaxEntryBytes */, metrics)

	// Budget tracker: per-route daily embedding cost ceiling via Redis INCRBYFLOAT.
	budgetTracker := budget.NewTracker(rdbClient, logger, namespace)

	return SemanticDeps{
		ConfigCache:    cfgCache,
		IndexLifecycle: lifecycle,
		Reader:         reader,
		Writer:         writer,
		BudgetTracker:  budgetTracker,
		Detector:       detector,
	}
}

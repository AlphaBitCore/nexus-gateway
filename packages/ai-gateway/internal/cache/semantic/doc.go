// Package semantic implements the L2 semantic (approximate-match) response
// cache for the AI Gateway.
//
// # L1 / L2 split
//
// The response cache has two cooperating tiers in one Valkey instance:
//
//   - L1 Extract (exact-match): canonical-request SHA-256 → full response.
//     Implemented in packages/ai-gateway/internal/cache/core and
//     packages/ai-gateway/internal/cache/stream.
//   - L2 Semantic (approximate-match): embedding KNN → full response when
//     cosine similarity ≥ threshold. Implemented in this package.
//
// A request walks L1 first; on L1 miss + freshness OK + admin opt-in it
// walks L2; on L2 miss it falls through to the broker → upstream provider.
// The L2 read path (FT.SEARCH) is wired in S4; this package covers the
// write path, the in-process config snapshot, and the index lifecycle.
//
// # Fingerprint discipline
//
// Every semantic cache entry is stamped with the L1 embedding fingerprint
// (sha256(provider:model:dim)) observed at HSET time. FT.SEARCH queries
// filter on the current L1 fingerprint so stale entries written under a
// previous embedding model are invisible during blue/green index swaps
// (see response-cache-architecture.md §3.5.2).
//
// # Blue/green index naming
//
// The Valkey vector index name is a versioned string stored in
// semantic_cache_config.redis_index_name (default "nexus:semantic-cache:v1").
// When admin changes the embedding model, the ConfigStore increments the
// version suffix to "v2", "v3", etc., ensuring no in-flight FT.SEARCH is
// ever issued against a name that disappears between DROP and CREATE.
// This package's IndexLifecycle observer calls EnsureIndex on the new name
// when a fingerprint change is detected; the Hub-side flush job (S3d) calls
// DropIndex on the old name after the swap propagates to all gateway pods.
//
// # Components
//
//   - ConfigCache: in-process atomic snapshot of semantic_cache_config.
//   - CircuitBreaker: per-(provider, model) embedding failure protection.
//   - EmbeddingSingleflight: deduplicates concurrent embed calls sharing
//     the same input.
//   - Client: Valkey FT.CREATE / HSET / FT.DROPINDEX operations.
//   - IndexLifecycle: reacts to ConfigSnapshot changes and calls EnsureIndex.
//   - Writer: orchestrates embed + HSET on L1-miss write path.
//   - Metrics: Prometheus counters and histograms.
package semantic

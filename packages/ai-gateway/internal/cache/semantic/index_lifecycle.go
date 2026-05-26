package semantic

import (
	"context"
	"log/slog"
	"sync"
)

// IndexLifecycle observes ConfigSnapshot changes and ensures that the Valkey
// vector index exists whenever the embedding fingerprint OR the index name
// changes.
//
// Why both: the fingerprint is sha256(provider:model:dim) — it captures
// changes to the embedding *shape* but NOT to the user-facing index name.
// Admins can rename redis_index_name (e.g. v6 → v14) to force a blue/green
// swap without changing provider/model/dim; if we keyed only on fingerprint
// the rename would never call FT.CREATE and the new index would never
// materialise.
//
// Responsibility split:
//   - This observer: calls EnsureIndex on the NEW index name when it detects
//     a fingerprint change OR an index-name change (or when first-call).
//   - Hub-side flush job consumer (S3d): calls DropIndex on the OLD index name
//     after the blue/green swap is confirmed. The observer does NOT drop the old
//     index because draining in-flight FT.SEARCH readers safely requires the Hub
//     job orchestration (see response-cache-architecture.md §3.5).
type IndexLifecycle struct {
	cache  *ConfigCache
	client *Client
	log    *slog.Logger

	mu              sync.Mutex
	lastFingerprint string // protected by mu
	lastIndexName   string // protected by mu — second dedup key alongside lastFingerprint
}

// NewIndexLifecycle constructs an IndexLifecycle observer.
func NewIndexLifecycle(cache *ConfigCache, client *Client, log *slog.Logger) *IndexLifecycle {
	return &IndexLifecycle{
		cache:  cache,
		client: client,
		log:    log,
	}
}

// OnConfigSnapshot is invoked by the Hub shadow callback in ai-gateway main
// with the latest ConfigSnapshot. It detects fingerprint changes and calls
// EnsureIndex on the new index. If the new fingerprint equals the old one, it
// is a no-op.
//
// EnsureIndex is idempotent so repeated calls with the same fingerprint are
// safe (they will hit the "index already exists" path and log at debug).
//
// Errors from EnsureIndex are logged at WARN but do not propagate to the
// caller — the shadow callback must not block.
func (l *IndexLifecycle) OnConfigSnapshot(ctx context.Context, snap ConfigSnapshot) {
	// Update the in-process cache unconditionally so the hot path always
	// has the latest snapshot even if EnsureIndex is not needed.
	l.cache.Set(snap)

	if !snap.Enabled || snap.Fingerprint == "" || snap.RedisIndexName == "" {
		l.log.Debug("semantic/lifecycle: snapshot not actionable; skipping EnsureIndex",
			"enabled", snap.Enabled,
			"fingerprint", snap.Fingerprint,
			"indexName", snap.RedisIndexName,
		)
		return
	}

	l.mu.Lock()
	lastFP := l.lastFingerprint
	lastIdx := l.lastIndexName
	if snap.Fingerprint == lastFP && snap.RedisIndexName == lastIdx {
		l.mu.Unlock()
		l.log.Debug("semantic/lifecycle: fingerprint and indexName unchanged; skipping EnsureIndex",
			"fingerprint", snap.Fingerprint,
			"indexName", snap.RedisIndexName,
		)
		return
	}
	l.lastFingerprint = snap.Fingerprint
	l.lastIndexName = snap.RedisIndexName
	l.mu.Unlock()

	// Fingerprint or indexName changed (or first call): ensure the new index
	// exists. Renames with an unchanged fingerprint hit this path too — that
	// is the whole reason we key on both: an admin rename like
	// "nexus:semantic-cache:v6" → "nexus:semantic-cache:v14" must trigger
	// FT.CREATE on the new name even though provider/model/dim are identical.
	l.log.Info("semantic/lifecycle: fingerprint or indexName changed; ensuring index",
		"oldFingerprint", lastFP,
		"newFingerprint", snap.Fingerprint,
		"oldIndexName", lastIdx,
		"newIndexName", snap.RedisIndexName,
		"dim", snap.EmbeddingDimension,
	)

	if err := l.client.EnsureIndex(ctx, snap.RedisIndexName, snap.EmbeddingDimension); err != nil {
		l.log.Warn("semantic/lifecycle: EnsureIndex failed",
			"indexName", snap.RedisIndexName,
			"fingerprint", snap.Fingerprint,
			"error", err,
		)
	}
}

package semantic

import (
	"context"
	"fmt"
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
// An EnsureIndex failure is RETURNED as well as un-recorded. The un-record
// alone was not enough: the config handler above this reported success anyway,
// so the shadow advanced its reported version past this key and short-circuited
// every later push of it — the "next snapshot" the rollback waits for never
// arrived, short of a process restart or a manual re-sync. Returning it leaves
// the key un-reported so the loader's retry timer arms, and stops Config Sync
// from showing a dead L2 index as applied and converged.
func (l *IndexLifecycle) OnConfigSnapshot(ctx context.Context, snap ConfigSnapshot) error {
	// Update the in-process cache unconditionally so the hot path always
	// has the latest snapshot even if EnsureIndex is not needed.
	l.cache.Set(snap)

	if !snap.Enabled || snap.Fingerprint == "" || snap.RedisIndexName == "" {
		l.log.Debug("semantic/lifecycle: snapshot not actionable; skipping EnsureIndex",
			"enabled", snap.Enabled,
			"fingerprint", snap.Fingerprint,
			"indexName", snap.RedisIndexName,
		)
		return nil
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
		return nil
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
		// UN-RECORD the fingerprint. It was recorded above, before the call,
		// which deduplicates concurrent snapshots — but it also meant that one
		// failed EnsureIndex marked this fingerprint as handled forever: every
		// later snapshot carrying it took the "unchanged; skipping" early
		// return, so the index was never created and the semantic cache stayed
		// dead for the lifetime of the process. A transient Redis blip or a
		// momentarily-unavailable search module was enough.
		//
		// Only roll back if nothing has advanced past us in the meantime;
		// otherwise a newer snapshot's record would be clobbered by an older
		// call's failure.
		l.mu.Lock()
		if l.lastFingerprint == snap.Fingerprint && l.lastIndexName == snap.RedisIndexName {
			l.lastFingerprint = lastFP
			l.lastIndexName = lastIdx
		}
		l.mu.Unlock()

		// ERROR, not WARN: from here until a retry succeeds the L2 semantic
		// cache is silently off, and the hot path has no other way to say so.
		l.log.Error("semantic/lifecycle: EnsureIndex failed; L2 semantic cache is unavailable until the next config snapshot retries it",
			"indexName", snap.RedisIndexName,
			"fingerprint", snap.Fingerprint,
			"error", err,
		)
		return fmt.Errorf("semantic/lifecycle: EnsureIndex %q: %w", snap.RedisIndexName, err)
	}
	return nil
}

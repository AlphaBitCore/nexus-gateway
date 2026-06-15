// configcache.go wires the agent's offline config fallback. Every shadow
// key the daemon applies is mirrored into a local SQLCipher-backed
// config_cache table (persist-on-apply); at boot the daemon replays that
// cache through the same per-key appliers so an agent that starts while
// Hub is unreachable enforces its last-known policy instead of starting
// fail-open with empty resolvers. A successful Hub pull later in startup
// supersedes whatever was restored.
package main

import (
	"context"
	"database/sql"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/sync/shadow"
)

// configCacheStaleAfter is the age past which a restored cache entry is
// flagged for an informational warning at boot. The daemon keeps enforcing
// the last-known policy past this horizon — staleness fails open, never
// closed. The age measures time since the last config CHANGE (cache writes
// only on a real apply), so a long-stable policy will read as old without
// implying any loss of Hub contact.
const configCacheStaleAfter = 7 * 24 * time.Hour

// cachePersist wraps a per-key applier so that, after a successful apply,
// the raw applied bytes are mirrored into config_cache. getCache is
// late-bound: it returns nil until the cache is opened on the audit
// queue's SQLCipher DB (after the queue is constructed in cmdRun), at
// which point every subsequent apply persists.
//
// A no-op payload is NOT cached. The per-key appliers treat an empty /
// "null" / "{}" state as a no-op that leaves in-memory policy unchanged
// (Hub emits these on early ticks before Cat B aggregation lands), so
// persisting one would overwrite the last real value — and the next
// offline boot would then replay a no-op blob and start with empty
// policy. An explicit empty ARRAY (e.g. {"hookConfigs":[]}) is NOT a
// no-op: it is an authoritative clear and IS cached.
func cachePersist(key string, inner rawApply, getCache func() *shadow.Cache, logger *slog.Logger) rawApply {
	return func(ctx context.Context, raw []byte, ver int64) ([]byte, error) {
		out, err := inner(ctx, raw, ver)
		if err != nil || isNoopShadowState(raw) {
			return out, err
		}
		if c := getCache(); c != nil {
			// Copy: the loader may reuse the backing array after apply.
			buf := append([]byte(nil), raw...)
			if perr := c.Save(key, buf, ver); perr != nil {
				logger.Warn("config_cache persist failed", "key", key, "error", perr)
			}
		}
		return out, err
	}
}

// isNoopShadowState mirrors the empty-state guard the per-key shadow
// appliers use (see AgentPipeline.Apply*ShadowState). It MUST stay in
// lockstep with them: a payload the appliers no-op on must not be cached,
// or the offline cache would drift from the enforced in-memory policy.
func isNoopShadowState(raw []byte) bool {
	return len(raw) == 0 || string(raw) == "null" || string(raw) == "{}"
}

// openAndRestoreConfigCache opens the offline config cache on db (the audit
// queue's SQLCipher DB, so applied policy is encrypted at rest alongside the
// audit log), publishes it through configCache so the loader's per-key
// persist wrappers mirror every subsequent apply, and replays the cached
// entries through their live appliers. Replaying now brings enforcement up
// immediately — well before platform interception starts and before the
// first Hub pull; a reachable Hub supersedes it via the config-startup
// refresh, while an unreachable Hub leaves the agent on last-known policy
// (stale but enforced — never fail-closed). An open failure only disables
// offline restore.
func openAndRestoreConfigCache(ctx context.Context, db *sql.DB, configCache *atomic.Pointer[shadow.Cache], appliers map[string]rawApply, logger *slog.Logger) {
	if cache, cacheErr := shadow.NewCache(db); cacheErr != nil {
		logger.Warn("config_cache open failed; offline restore disabled", "error", cacheErr)
	} else {
		configCache.Store(cache)
		restoreCachedConfig(ctx, cache, appliers, logger)
	}
}

// restoreCachedConfig replays the last-known config persisted to
// config_cache, applying each key through its live applier so the agent
// enforces immediately at boot rather than starting with empty policy.
// Entries whose last update is older than configCacheStaleAfter are still
// applied — the daemon never fail-closes on stale policy — but trigger one
// informational warning (the timestamp reflects the last config CHANGE, so
// a long-stable policy is expected to read as old; the warning states the
// age without asserting Hub is unreachable). appliers maps each config key
// to its unwrapped applier (the restore path must not re-persist what it
// just read back).
func restoreCachedConfig(ctx context.Context, cache *shadow.Cache, appliers map[string]rawApply, logger *slog.Logger) {
	if cache == nil {
		return
	}
	entries, err := cache.LoadAll()
	if err != nil {
		logger.Warn("config_cache restore: load failed", "error", err)
		return
	}
	if len(entries) == 0 {
		return
	}
	var restored, staleKeys int
	var oldest time.Time
	for _, e := range entries {
		apply, ok := appliers[e.Key]
		if !ok {
			// A key cached by a prior build the current daemon no longer
			// registers — skip rather than fail the whole restore.
			continue
		}
		if _, aerr := apply(ctx, []byte(e.State), e.Version); aerr != nil {
			logger.Warn("config_cache restore: apply failed", "key", e.Key, "error", aerr)
			continue
		}
		restored++
		if time.Since(e.UpdatedAt) > configCacheStaleAfter {
			staleKeys++
		}
		if oldest.IsZero() || e.UpdatedAt.Before(oldest) {
			oldest = e.UpdatedAt
		}
	}
	logger.Info("restored config from local cache", "keys", restored)
	if staleKeys > 0 {
		logger.Warn("restored cached config older than the staleness grace period; still enforcing last-known policy",
			"staleKeys", staleKeys,
			"oldestAgeHours", int(time.Since(oldest).Hours()),
			"graceHours", int(configCacheStaleAfter.Hours()),
		)
	}
}

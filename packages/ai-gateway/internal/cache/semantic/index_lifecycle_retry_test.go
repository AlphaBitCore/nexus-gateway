package semantic

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/semantic/internal/testredis"
)

// OnConfigSnapshot recorded the fingerprint BEFORE calling EnsureIndex and only
// warned on failure. So one failed call marked that fingerprint handled for
// good: every later snapshot carrying it took the "unchanged; skipping" early
// return, the index was never created, and the L2 semantic cache stayed dead for
// the lifetime of the process. A transient Redis blip was enough, and nothing
// retried.
//
// The failure here is REVERSIBLE — miniredis.SetError makes every command fail
// and clearing it restores the server — so the arm can show the recovery, not
// just the absence of the record.
func TestIndexLifecycle_RetriesAfterAFailedEnsureIndex(t *testing.T) {
	_, rdb, mr, cleanup := testredis.NewMiniValkeyWithServer(t)
	defer cleanup()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	lc := NewIndexLifecycle(NewConfigCache(), NewClient(rdb, log, "test", nil), log)

	snap := ConfigSnapshot{
		Enabled:             true,
		EmbeddingProviderID: "openai",
		EmbeddingModelID:    "text-embedding-3-small",
		EmbeddingDimension:  4,
		Fingerprint:         "fp-retry",
		RedisIndexName:      "nexus:semantic-cache:retry",
	}

	// Redis is unhappy — the transient condition.
	mr.SetError("LOADING Redis is loading the dataset in memory")
	lc.OnConfigSnapshot(context.Background(), snap)

	lc.mu.Lock()
	recorded := lc.lastFingerprint
	lc.mu.Unlock()
	if recorded == snap.Fingerprint {
		t.Fatalf("the fingerprint was recorded as handled despite EnsureIndex failing — every later snapshot will skip it and the semantic cache stays dead for the process lifetime")
	}

	// Redis recovers. The SAME snapshot must now be acted on, not skipped.
	mr.SetError("")
	lc.OnConfigSnapshot(context.Background(), snap)

	lc.mu.Lock()
	recorded = lc.lastFingerprint
	idx := lc.lastIndexName
	lc.mu.Unlock()
	if recorded != snap.Fingerprint || idx != snap.RedisIndexName {
		t.Errorf("after recovery lastFingerprint=%q lastIndexName=%q, want %q/%q — the retry did not happen",
			recorded, idx, snap.Fingerprint, snap.RedisIndexName)
	}
}

// The snapshot must still reach the hot path even when EnsureIndex fails: L1
// exact-match runs on every Redis topology and folds vary_by into its key, so it
// has to learn the fleet scope regardless of whether the L2 index materialised.
func TestIndexLifecycle_FailedEnsureIndexStillPublishesTheSnapshot(t *testing.T) {
	_, rdb, mr, cleanup := testredis.NewMiniValkeyWithServer(t)
	defer cleanup()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cache := NewConfigCache()
	lc := NewIndexLifecycle(cache, NewClient(rdb, log, "test", nil), log)

	mr.SetError("LOADING Redis is loading the dataset in memory")
	lc.OnConfigSnapshot(context.Background(), ConfigSnapshot{
		Enabled:             true,
		EmbeddingProviderID: "openai",
		EmbeddingModelID:    "text-embedding-3-small",
		EmbeddingDimension:  4,
		Fingerprint:         "fp-publish",
		RedisIndexName:      "nexus:semantic-cache:publish",
		VaryBy:              "org",
	})

	got := cache.Get()
	if got.Fingerprint != "fp-publish" {
		t.Errorf("ConfigCache.Fingerprint = %q, want fp-publish — the hot path did not learn the new scope", got.Fingerprint)
	}
	if got.VaryBy != "org" {
		t.Errorf("ConfigCache.VaryBy = %q, want org", got.VaryBy)
	}
}

// The sibling: a SUCCESSFUL EnsureIndex still records, so "never record" would
// not satisfy the arms above — it would make every snapshot re-issue FT.CREATE.
func TestIndexLifecycle_SuccessRecordsAndThenSkips(t *testing.T) {
	lc, _, cleanup := newTestLifecycle(t)
	defer cleanup()

	snap := ConfigSnapshot{
		Enabled:             true,
		EmbeddingProviderID: "openai",
		EmbeddingModelID:    "text-embedding-3-small",
		EmbeddingDimension:  4,
		Fingerprint:         "fp-ok",
		RedisIndexName:      "nexus:semantic-cache:ok",
	}
	lc.OnConfigSnapshot(context.Background(), snap)

	lc.mu.Lock()
	fp, idx := lc.lastFingerprint, lc.lastIndexName
	lc.mu.Unlock()
	if fp != "fp-ok" || idx != "nexus:semantic-cache:ok" {
		t.Fatalf("lastFingerprint=%q lastIndexName=%q, want fp-ok/nexus:semantic-cache:ok", fp, idx)
	}
}

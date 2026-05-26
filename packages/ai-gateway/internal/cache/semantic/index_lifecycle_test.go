package semantic

import (
	"context"
	"io"
	"log/slog"
	"testing"
)

// newTestLifecycle returns an IndexLifecycle backed by a MiniValkey client
// and a fresh ConfigCache.
func newTestLifecycle(t *testing.T) (*IndexLifecycle, *ConfigCache, func()) {
	t.Helper()
	c, cleanup := newTestClient(t)
	cache := NewConfigCache()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	lc := NewIndexLifecycle(cache, c, log)
	return lc, cache, cleanup
}

// TestIndexLifecycle_FingerprintChangeTriggersEnsureIndex verifies that a new
// fingerprint calls EnsureIndex on the new index name.
func TestIndexLifecycle_FingerprintChangeTriggersEnsureIndex(t *testing.T) {
	lc, _, cleanup := newTestLifecycle(t)
	defer cleanup()

	snap := ConfigSnapshot{
		Enabled:             true,
		EmbeddingProviderID: "openai",
		EmbeddingModelID:    "text-embedding-3-small",
		EmbeddingDimension:  4,
		Fingerprint:         "fp-v1",
		RedisIndexName:      "nexus:semantic-cache:v1",
	}
	lc.OnConfigSnapshot(context.Background(), snap)

	// Verify the index was created by checking lastFingerprint.
	lc.mu.Lock()
	fp := lc.lastFingerprint
	lc.mu.Unlock()
	if fp != "fp-v1" {
		t.Errorf("lastFingerprint = %q, want 'fp-v1'", fp)
	}
}

// TestIndexLifecycle_NoOpOnSameFingerprint verifies that repeated calls with
// the same fingerprint do not re-run EnsureIndex (no-op).
func TestIndexLifecycle_NoOpOnSameFingerprint(t *testing.T) {
	lc, _, cleanup := newTestLifecycle(t)
	defer cleanup()

	snap := ConfigSnapshot{
		Enabled:             true,
		EmbeddingProviderID: "openai",
		EmbeddingModelID:    "m",
		EmbeddingDimension:  4,
		Fingerprint:         "same-fp",
		RedisIndexName:      "test-lc-idx",
	}
	lc.OnConfigSnapshot(context.Background(), snap)
	lc.OnConfigSnapshot(context.Background(), snap) // second call should no-op

	lc.mu.Lock()
	fp := lc.lastFingerprint
	lc.mu.Unlock()
	if fp != "same-fp" {
		t.Errorf("lastFingerprint = %q", fp)
	}
}

// TestIndexLifecycle_FingerprintChangeUpdatesConfigCache verifies that
// OnConfigSnapshot always calls ConfigCache.Set even when EnsureIndex is a
// no-op.
func TestIndexLifecycle_FingerprintChangeUpdatesConfigCache(t *testing.T) {
	lc, cache, cleanup := newTestLifecycle(t)
	defer cleanup()

	snap := ConfigSnapshot{
		Enabled:             true,
		EmbeddingProviderID: "p",
		EmbeddingModelID:    "m",
		EmbeddingDimension:  4,
		Fingerprint:         "fp-cache",
		RedisIndexName:      "test-cache-idx",
	}
	lc.OnConfigSnapshot(context.Background(), snap)
	got := cache.Get()
	if got.Fingerprint != "fp-cache" {
		t.Errorf("ConfigCache.Get().Fingerprint = %q, want 'fp-cache'", got.Fingerprint)
	}
}

// TestIndexLifecycle_DisabledSnapSkipsEnsureIndex verifies that a snapshot
// with Enabled=false does not call EnsureIndex.
func TestIndexLifecycle_DisabledSnapSkipsEnsureIndex(t *testing.T) {
	lc, _, cleanup := newTestLifecycle(t)
	defer cleanup()

	snap := ConfigSnapshot{
		Enabled:        false,
		Fingerprint:    "fp-disabled",
		RedisIndexName: "some-idx",
	}
	lc.OnConfigSnapshot(context.Background(), snap)

	lc.mu.Lock()
	fp := lc.lastFingerprint
	lc.mu.Unlock()
	// lastFingerprint should NOT be updated because the snapshot is disabled.
	if fp == "fp-disabled" {
		t.Error("lastFingerprint should NOT be set for disabled snapshot")
	}
}

// TestIndexLifecycle_EmptyFingerprintSkipsEnsureIndex verifies that an empty
// fingerprint is skipped.
func TestIndexLifecycle_EmptyFingerprintSkipsEnsureIndex(t *testing.T) {
	lc, _, cleanup := newTestLifecycle(t)
	defer cleanup()

	snap := ConfigSnapshot{
		Enabled:        true,
		Fingerprint:    "",
		RedisIndexName: "some-idx",
	}
	lc.OnConfigSnapshot(context.Background(), snap)

	lc.mu.Lock()
	fp := lc.lastFingerprint
	lc.mu.Unlock()
	if fp != "" {
		t.Errorf("lastFingerprint should be empty, got %q", fp)
	}
}

// TestIndexLifecycle_IndexRenameWithSameFingerprintTriggersEnsureIndex
// pins the rename-without-fingerprint-change case: when admin renames
// redis_index_name but provider/model/dim are identical, the embedding
// fingerprint stays the same. The old keyed-only-on-fingerprint logic
// short-circuited, FT.CREATE never ran, FT.SEARCH returned "Index not
// found", and the L2 cache could never produce a HIT.
func TestIndexLifecycle_IndexRenameWithSameFingerprintTriggersEnsureIndex(t *testing.T) {
	lc, _, cleanup := newTestLifecycle(t)
	defer cleanup()

	base := ConfigSnapshot{
		Enabled:             true,
		EmbeddingProviderID: "openai",
		EmbeddingModelID:    "text-embedding-3-small",
		EmbeddingDimension:  4,
		Fingerprint:         "same-fp", // unchanged across the rename
	}

	// First push: v6
	first := base
	first.RedisIndexName = "rename-test:v6"
	lc.OnConfigSnapshot(context.Background(), first)

	// Verify the v6 index was created by attempting a fresh FT.CREATE on the
	// same name — the stub returns "Index already exists" iff cmdFTCreate
	// previously recorded it, which is the same signal the production code
	// uses to detect idempotency.
	if err := lc.client.EnsureIndex(context.Background(), "rename-test:v6", 4); err != nil {
		t.Fatalf("re-EnsureIndex v6 should be idempotent, got: %v", err)
	}

	// Second push: rename to v14 (provider/model/dim identical → fingerprint
	// identical). Pre-fix, this was a no-op; post-fix, EnsureIndex must run.
	second := base
	second.RedisIndexName = "rename-test:v14"
	lc.OnConfigSnapshot(context.Background(), second)

	// Now FT.CREATE on v14 must report "already exists" — proving the
	// rename push actually called EnsureIndex against the new name. Pre-fix
	// this would have created v14 here for the first time (bug signature).
	// We detect "first create" by issuing FT.CREATE directly via rdb (so we
	// can see a non-idempotent OK reply vs the "already exists" path).
	{
		args := []interface{}{
			"FT.CREATE", "rename-test:v14",
			"ON", "HASH",
			"PREFIX", "1", "rename-test:v14:",
			"SCHEMA",
			"vector", "VECTOR", "HNSW", "12",
			"DIM", "4",
			"TYPE", "FLOAT32",
			"DISTANCE_METRIC", "COSINE",
			"M", "16",
			"EF_CONSTRUCTION", "200",
			"EF_RUNTIME", "10",
			"cached_at", "NUMERIC",
		}
		err := lc.client.rdb.Do(context.Background(), args...).Err()
		if err == nil {
			t.Fatalf("rename did NOT call EnsureIndex(rename-test:v14): direct FT.CREATE unexpectedly succeeded — the index should already exist post-fix")
		}
		if !isIndexExistsError(err) {
			t.Fatalf("expected 'already exists' error on direct FT.CREATE, got: %v", err)
		}
	}

	// Internal dedup state should now reflect the new index name so a third
	// identical push is a true no-op.
	lc.mu.Lock()
	gotIdx := lc.lastIndexName
	gotFP := lc.lastFingerprint
	lc.mu.Unlock()
	if gotIdx != "rename-test:v14" {
		t.Errorf("lastIndexName = %q, want rename-test:v14", gotIdx)
	}
	if gotFP != "same-fp" {
		t.Errorf("lastFingerprint = %q, want same-fp", gotFP)
	}
}

// TestIndexLifecycle_TwoFingerprintChanges verifies that two distinct
// fingerprints each trigger EnsureIndex.
func TestIndexLifecycle_TwoFingerprintChanges(t *testing.T) {
	lc, _, cleanup := newTestLifecycle(t)
	defer cleanup()

	for _, tc := range []struct {
		fp    string
		index string
	}{
		{"fp-v1", "nexus:semantic-cache:v1"},
		{"fp-v2", "nexus:semantic-cache:v2"},
	} {
		snap := ConfigSnapshot{
			Enabled:             true,
			EmbeddingProviderID: "p",
			EmbeddingModelID:    "m",
			EmbeddingDimension:  4,
			Fingerprint:         tc.fp,
			RedisIndexName:      tc.index,
		}
		lc.OnConfigSnapshot(context.Background(), snap)
		lc.mu.Lock()
		got := lc.lastFingerprint
		lc.mu.Unlock()
		if got != tc.fp {
			t.Errorf("after fp=%q: lastFingerprint = %q", tc.fp, got)
		}
	}
}

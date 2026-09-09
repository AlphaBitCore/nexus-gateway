package configdispatch

import (
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/semantic"
)

// deadValkey returns a client pointed at a port nothing listens on, so
// EnsureIndex fails the way a Valkey blip or an unavailable search module makes
// it fail. The dial timeouts are tiny because the failure is the point.
func deadValkey(t *testing.T) *semantic.Client {
	t.Helper()
	rdb := redis.NewClient(&redis.Options{
		Addr:         "127.0.0.1:1",
		DialTimeout:  50 * time.Millisecond,
		ReadTimeout:  50 * time.Millisecond,
		WriteTimeout: 50 * time.Millisecond,
		MaxRetries:   -1,
	})
	t.Cleanup(func() { _ = rdb.Close() })
	return semantic.NewClient(rdb, slog.New(slog.NewTextHandler(io.Discard, nil)), "test", nil)
}

// TestHandler_SemanticCacheConfig_EnsureIndexFailureIsReported pins the
// reporting contract that the lifecycle's own retry depends on.
//
// index_lifecycle.go rolls the fingerprint back on an EnsureIndex failure so
// "the next snapshot tries again". That promise is only kept if the failure
// reaches the config loader: a handler that answers success has its key marked
// reported, the shadow advances reportedVer to desiredVer, and every later push
// of that version — including the full re-apply on WS reconnect — is
// short-circuited. The retry then needs a process restart, an admin edit that
// bumps the version, or a manual re-sync.
//
// Meanwhile the L2 index does not exist: every read answers
// ErrSearchUnavailable while the writer, which is not gated on index existence,
// keeps paying for an embedding per request. And Config Sync shows the key as
// applied and converged — a failed operation reported as a successful one,
// which is the class this branch fixed twice elsewhere.
func TestHandler_SemanticCacheConfig_EnsureIndexFailureIsReported(t *testing.T) {
	d := newTestDeps(t)
	cc := semantic.NewConfigCache()
	d.SemanticIndexLifecycle = semantic.NewIndexLifecycle(cc, deadValkey(t),
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	providerID, modelID, dim := "prov-1", "model-1", 768
	raw, err := json.Marshal(semanticCacheConfigBlob{
		Enabled:              true,
		EmbeddingProviderID:  &providerID,
		EmbeddingModelID:     &modelID,
		EmbeddingDimension:   &dim,
		EmbeddingFingerprint: "fp-openai-text-embedding-3-small-768",
		RedisIndexName:       "nexus:semantic-cache:v1",
		Threshold:            0.92,
		VaryBy:               "vk",
	})
	if err != nil {
		t.Fatalf("marshalling the fixture: %v", err)
	}

	if applyErr := applyKey(t, d, "semantic_cache.config", raw); applyErr == nil {
		t.Fatal("the handler reported a successful apply while EnsureIndex failed — the loader marks " +
			"the key reported, the shadow advances past this version, and the lifecycle's fingerprint " +
			"rollback can never be retried")
	}

	// The hot-path snapshot must still be updated. The two are deliberately
	// decoupled: L1 exact-match runs on every Redis topology and folds vary_by
	// into its key, so it must learn the fleet scope even when the L2 index
	// could not be created. Reporting the failure must not cost that.
	if snap := cc.Get(); !snap.Enabled || snap.VaryBy != "vk" {
		t.Fatalf("the hot-path snapshot was not updated: enabled=%v varyBy=%q", snap.Enabled, snap.VaryBy)
	}
}

// TestHandler_SemanticCacheConfig_HealthyApplyStillReportsSuccess is the other
// half: only a real failure may be reported as one. A handler that returns an
// error on a healthy apply would leave the key permanently un-reported and the
// node permanently out of sync on the Config Sync page.
func TestHandler_SemanticCacheConfig_HealthyApplyStillReportsSuccess(t *testing.T) {
	d := newTestDeps(t)
	cc := semantic.NewConfigCache()
	d.SemanticIndexLifecycle = semantic.NewIndexLifecycle(cc, deadValkey(t),
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	providerID, modelID, dim := "prov-1", "model-1", 768
	// No fingerprint and no index name: not actionable, so EnsureIndex is never
	// reached and the apply is genuinely successful even against a dead Valkey.
	raw, err := json.Marshal(semanticCacheConfigBlob{
		Enabled:             true,
		EmbeddingProviderID: &providerID,
		EmbeddingModelID:    &modelID,
		EmbeddingDimension:  &dim,
		Threshold:           0.92,
		VaryBy:              "vk",
	})
	if err != nil {
		t.Fatalf("marshalling the fixture: %v", err)
	}

	if applyErr := applyKey(t, d, "semantic_cache.config", raw); applyErr != nil {
		t.Fatalf("a snapshot that needs no index must apply cleanly: %v", applyErr)
	}
}

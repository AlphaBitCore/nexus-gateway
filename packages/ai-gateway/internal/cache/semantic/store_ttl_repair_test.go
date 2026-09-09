package semantic

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/semantic/internal/testredis"
)

// deadlineBetweenWrites reproduces the one failure mode that matters here: the
// caller's deadline lands BETWEEN the HSET and the PEXPIRE.
//
// That is not contrived. proxy_l2.go gives the whole L2 write a detached 5s
// budget, and that budget also covers the embedding round-trip to the provider
// (writer.go calls Embed before StoreEntry), so a slow embedding leaves very
// little of it for the two Valkey commands.
//
// The hook cancels the caller's context after the HSET lands and fails the
// PEXPIRE, then records the context the compensating DEL was issued on. If that
// context is the caller's, the delete cannot possibly succeed — and the entry
// stays in Valkey with no expiry, holding a prompt and a response that neither
// the retention job nor an erasure request can reach, because both operate on
// tables.
type deadlineBetweenWrites struct {
	cancel    context.CancelFunc
	delSeen   bool
	delCtxErr error
}

func (h *deadlineBetweenWrites) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *deadlineBetweenWrites) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func (h *deadlineBetweenWrites) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		switch cmd.Name() {
		case "hset":
			err := next(ctx, cmd)
			h.cancel() // the caller's budget runs out here
			return err
		case "pexpire":
			err := errors.New("context deadline exceeded")
			cmd.SetErr(err)
			return err
		case "del":
			h.delSeen = true
			h.delCtxErr = ctx.Err()
		}
		return next(ctx, cmd)
	}
}

func ttlRepairFixture(t *testing.T) (*Client, *redis.Client, *deadlineBetweenWrites, StoreInput, string, func()) {
	t.Helper()
	_, rdb, cleanup := testredis.NewMiniValkey(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := NewClient(rdb, log, "test", nil)

	const indexName = "ttl-repair-idx"
	if err := c.EnsureIndex(context.Background(), indexName, 4); err != nil {
		t.Fatalf("EnsureIndex: %v", err)
	}
	in := StoreInput{
		VKScope:          "v1:vk:abc",
		UpstreamProvider: "openai",
		UpstreamModel:    "gpt-4o-mini",
		ResponseKind:     "response",
		Fingerprint:      "fp-ttl-repair",
		EmbeddingInput:   "what is my account number",
		Embedding:        []float32{0.1, 0.2, 0.3, 0.4},
		ResponseBody:     []byte(`{"id":"resp-1","choices":[{"message":{"content":"it is 4051-9930"}}]}`),
		TTL:              5 * time.Minute,
	}
	h := &deadlineBetweenWrites{}
	rdb.AddHook(h)
	return c, rdb, h, in, indexName, cleanup
}

// TestStoreEntry_TTLRepairSurvivesTheCallersDeadline pins the repair. The
// compensating delete exists precisely for the case where the caller's context
// died, so riding that same context guaranteed it could not land.
func TestStoreEntry_TTLRepairSurvivesTheCallersDeadline(t *testing.T) {
	c, rdb, h, in, indexName, cleanup := ttlRepairFixture(t)
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	defer cancel()

	err := c.StoreEntry(ctx, indexName, in, 0)
	if err == nil {
		t.Fatal("StoreEntry reported success for an entry it discarded — the writer counts that " +
			"as IncWrite(\"ok\") and Stored: true, so the metric and the audit flag both claim a cache " +
			"entry that does not exist, after paying for the embedding")
	}
	if !errors.Is(err, ErrValkeyUnavailable) {
		t.Errorf("the error must classify as a Valkey failure so translateStoreError gives it a "+
			"skip reason; got %v", err)
	}

	if !h.delSeen {
		t.Fatal("no compensating DEL was issued at all")
	}
	if h.delCtxErr != nil {
		t.Fatalf("the compensating DEL rode a context that was already dead (%v), so it could not "+
			"land and the entry is left in Valkey with no expiry", h.delCtxErr)
	}

	// The measurement that could show presence: the key must be gone, and the
	// probe must be able to see a key that IS there — the sibling test below
	// establishes that on the healthy path.
	if n, qerr := rdb.Exists(context.Background(), entryKey(indexName, in)).Result(); qerr != nil {
		t.Fatalf("checking the key: %v", qerr)
	} else if n != 0 {
		t.Fatal("the entry survived with no expiry — it holds prompt and response text that " +
			"neither retention nor an erasure request can reach, because both operate on tables")
	}
}

// TestStoreEntry_HealthyWriteKeepsItsEntry is the positive control for the
// assertion above: the same fixture, without the failure, leaves a key that
// Exists reports — so "the key is gone" above is a measurement, not an artefact
// of looking in the wrong place.
func TestStoreEntry_HealthyWriteKeepsItsEntry(t *testing.T) {
	_, rdb, cleanup := testredis.NewMiniValkey(t)
	defer cleanup()
	c := NewClient(rdb, slog.New(slog.NewTextHandler(io.Discard, nil)), "test", nil)
	ctx := context.Background()

	const indexName = "ttl-repair-control-idx"
	if err := c.EnsureIndex(ctx, indexName, 4); err != nil {
		t.Fatalf("EnsureIndex: %v", err)
	}
	in := StoreInput{
		VKScope:          "v1:vk:abc",
		UpstreamProvider: "openai",
		UpstreamModel:    "gpt-4o-mini",
		ResponseKind:     "response",
		Fingerprint:      "fp-control",
		EmbeddingInput:   "what is my account number",
		Embedding:        []float32{0.1, 0.2, 0.3, 0.4},
		ResponseBody:     []byte(`{"id":"resp-1","choices":[]}`),
		TTL:              5 * time.Minute,
	}

	if err := c.StoreEntry(ctx, indexName, in, 0); err != nil {
		t.Fatalf("StoreEntry on a healthy path: %v", err)
	}
	if n, err := rdb.Exists(ctx, entryKey(indexName, in)).Result(); err != nil || n != 1 {
		t.Fatalf("the healthy path must leave a key the probe can see: exists=%d err=%v", n, err)
	}
	if ttl, err := rdb.PTTL(ctx, entryKey(indexName, in)).Result(); err != nil || ttl <= 0 {
		t.Fatalf("the healthy path must leave an expiry: ttl=%v err=%v", ttl, err)
	}
}

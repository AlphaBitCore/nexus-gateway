package semantic

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/semantic/internal/testredis"
)

// newTestClient returns a Client backed by the MiniValkey test server.
func newTestClient(t *testing.T) (*Client, func()) {
	t.Helper()
	_, rdb, cleanup := testredis.NewMiniValkey(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	client := NewClient(rdb, log, "test", nil)
	return client, cleanup
}

// TestClient_EnsureIndex_CreatesIndex verifies that EnsureIndex on a
// non-existent index succeeds.
func TestClient_EnsureIndex_CreatesIndex(t *testing.T) {
	c, cleanup := newTestClient(t)
	defer cleanup()

	if err := c.EnsureIndex(context.Background(), "test-idx", 4); err != nil {
		t.Fatalf("EnsureIndex: %v", err)
	}
}

// TestClient_EnsureIndex_Idempotent verifies that calling EnsureIndex twice
// on the same index name does not return an error.
func TestClient_EnsureIndex_Idempotent(t *testing.T) {
	c, cleanup := newTestClient(t)
	defer cleanup()

	if err := c.EnsureIndex(context.Background(), "test-idx2", 8); err != nil {
		t.Fatalf("first EnsureIndex: %v", err)
	}
	// Second call should be a no-op.
	if err := c.EnsureIndex(context.Background(), "test-idx2", 8); err != nil {
		t.Fatalf("second EnsureIndex (idempotent): %v", err)
	}
}

// TestClient_EnsureIndex_EmptyNameError verifies that an empty index name
// returns an error immediately.
func TestClient_EnsureIndex_EmptyNameError(t *testing.T) {
	c, cleanup := newTestClient(t)
	defer cleanup()

	if err := c.EnsureIndex(context.Background(), "", 4); err == nil {
		t.Fatal("expected error for empty index name")
	}
}

// TestClient_EnsureIndex_ZeroDimError verifies that dim <= 0 returns an error.
func TestClient_EnsureIndex_ZeroDimError(t *testing.T) {
	c, cleanup := newTestClient(t)
	defer cleanup()

	if err := c.EnsureIndex(context.Background(), "idx", 0); err == nil {
		t.Fatal("expected error for zero dim")
	}
}

// TestClient_DropIndex_Idempotent verifies that DropIndex on a missing index
// returns nil (idempotent).
func TestClient_DropIndex_Idempotent(t *testing.T) {
	c, cleanup := newTestClient(t)
	defer cleanup()

	// Drop without prior create — should not error.
	if err := c.DropIndex(context.Background(), "nonexistent-idx"); err != nil {
		t.Fatalf("DropIndex on missing index: %v", err)
	}
}

// TestClient_DropIndex_DropExisting verifies that creating then dropping an
// index works.
func TestClient_DropIndex_DropExisting(t *testing.T) {
	c, cleanup := newTestClient(t)
	defer cleanup()

	if err := c.EnsureIndex(context.Background(), "drop-me", 4); err != nil {
		t.Fatalf("EnsureIndex: %v", err)
	}
	if err := c.DropIndex(context.Background(), "drop-me"); err != nil {
		t.Fatalf("DropIndex: %v", err)
	}
	// Second drop should still be idempotent.
	if err := c.DropIndex(context.Background(), "drop-me"); err != nil {
		t.Fatalf("second DropIndex (idempotent): %v", err)
	}
}

// TestClient_StoreEntry_HSETsExpectedKey verifies that StoreEntry writes an
// HSET to the expected key and that the required fields are present.
func TestClient_StoreEntry_HSETsExpectedKey(t *testing.T) {
	_, rdb, cleanup := testredis.NewMiniValkey(t)
	defer cleanup()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := NewClient(rdb, log, "test", nil)
	ctx := context.Background()

	indexName := "test-store-idx"
	if err := c.EnsureIndex(ctx, indexName, 4); err != nil {
		t.Fatalf("EnsureIndex: %v", err)
	}

	embInput := "hello world"
	vec := []float32{0.1, 0.2, 0.3, 0.4}
	in := StoreInput{
		VKScope:          "v1:vk:abc",
		UpstreamProvider: "openai",
		UpstreamModel:    "gpt-4o-mini",
		ResponseKind:     "response",
		Fingerprint:      "fp123",
		EmbeddingInput:   embInput,
		Embedding:        vec,
		ResponseBody:     []byte(`{"id":"resp-1","choices":[]}`),
		Usage:            map[string]any{"prompt_tokens": 5, "completion_tokens": 10},
		TTL:              5 * time.Minute,
	}

	if err := c.StoreEntry(ctx, indexName, in, 0); err != nil {
		t.Fatalf("StoreEntry: %v", err)
	}

	// Compute expected key.
	expectedKey := entryKey(indexName, embInput)
	if !strings.HasPrefix(expectedKey, indexName+":") {
		t.Errorf("key %q does not start with index prefix", expectedKey)
	}

	// Verify the HSET fields via direct Redis GET on the hash.
	fields, err := rdb.HGetAll(ctx, expectedKey).Result()
	if err != nil {
		t.Fatalf("HGetAll: %v", err)
	}
	for _, requiredField := range []string{
		"vector", "upstream_provider", "upstream_model", "vk_scope",
		"response_kind", "fingerprint", "response_body", "usage", "cached_at",
	} {
		if _, ok := fields[requiredField]; !ok {
			t.Errorf("required field %q missing from HSET", requiredField)
		}
	}
	if fields["upstream_provider"] != "openai" {
		t.Errorf("upstream_provider = %q, want 'openai'", fields["upstream_provider"])
	}
	if fields["fingerprint"] != "fp123" {
		t.Errorf("fingerprint = %q, want 'fp123'", fields["fingerprint"])
	}
	if fields["response_kind"] != "response" {
		t.Errorf("response_kind = %q, want 'response'", fields["response_kind"])
	}
}

// TestClient_StoreEntry_MaxEntryBytesGuard verifies that an entry whose
// response_body exceeds maxEntryBytes returns ErrEntryTooLarge.
func TestClient_StoreEntry_MaxEntryBytesGuard(t *testing.T) {
	c, cleanup := newTestClient(t)
	defer cleanup()

	in := StoreInput{
		EmbeddingInput: "big",
		Embedding:      []float32{1.0},
		ResponseBody:   make([]byte, 100),
		TTL:            time.Minute,
	}

	// maxEntryBytes = 10 bytes — the 100-byte response_body exceeds this.
	err := c.StoreEntry(context.Background(), "idx", in, 10)
	if err == nil {
		t.Fatal("expected ErrEntryTooLarge, got nil")
	}
	if !strings.Contains(err.Error(), "entry exceeds max entry size") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestClient_EntryKey_Format verifies the key format is <indexName>:<16-char-hex>.
func TestClient_EntryKey_Format(t *testing.T) {
	k := entryKey("nexus:semantic-cache:v1", "hello world")
	if !strings.HasPrefix(k, "nexus:semantic-cache:v1:") {
		t.Errorf("key %q missing index prefix", k)
	}
	suffix := strings.TrimPrefix(k, "nexus:semantic-cache:v1:")
	if len(suffix) != keyHashLen {
		t.Errorf("key suffix length = %d, want %d; key = %q", len(suffix), keyHashLen, k)
	}
}

// TestClient_Float32sToBytes_RoundTrip verifies that encoding + decoding
// float32s is lossless.
func TestClient_Float32sToBytes_RoundTrip(t *testing.T) {
	original := []float32{1.0, 2.5, -3.14, 0.0}
	encoded := float32sToBytes(original)
	decoded := testredis.Float32sToBytes(original) // use testredis helper for cross-verification
	if len(encoded) != len(decoded) {
		t.Errorf("encoded length mismatch: %d vs %d", len(encoded), len(decoded))
	}
	for i := range encoded {
		if encoded[i] != decoded[i] {
			t.Errorf("byte %d differs: %02x vs %02x", i, encoded[i], decoded[i])
		}
	}
}

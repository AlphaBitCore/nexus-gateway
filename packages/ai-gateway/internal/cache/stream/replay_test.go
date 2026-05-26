package streamcache

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	cache "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/core"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
)

func intPtr(v int) *int { return &v }

func TestReplaySubscription_EmitsAllChunksThenEOF(t *testing.T) {
	entry := &cache.StreamEntry{
		Provider: "openai", Model: "gpt-4o",
		Chunks: []cache.ChunkRecord{
			{Delta: "hello "},
			{Delta: "world"},
			{Done: true, Usage: &provcore.Usage{
				PromptTokens:     intPtr(5),
				CompletionTokens: intPtr(2),
			}},
		},
		CachedAt: time.Now().UTC(),
	}
	sub := NewReplaySubscription(entry, nil)
	ctx := context.Background()

	var got []string
	var doneSeen bool
	for {
		chunk, err := sub.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if chunk.Delta != "" {
			got = append(got, chunk.Delta)
		}
		if chunk.Done {
			doneSeen = true
		}
	}
	if !doneSeen {
		t.Fatal("Done chunk not seen")
	}
	if len(got) != 2 || got[0] != "hello " || got[1] != "world" {
		t.Fatalf("delta sequence mismatch: %v", got)
	}
	if err := sub.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestReplaySubscription_CloseIdempotent(t *testing.T) {
	entry := &cache.StreamEntry{Chunks: []cache.ChunkRecord{{Done: true}}}
	sub := NewReplaySubscription(entry, nil)
	if err := sub.Close(); err != nil {
		t.Fatal(err)
	}
	if err := sub.Close(); err != nil {
		t.Fatal("Close not idempotent")
	}
}

func TestReplaySubscription_AfterCloseReturnsEOF(t *testing.T) {
	entry := &cache.StreamEntry{Chunks: []cache.ChunkRecord{{Delta: "a"}, {Done: true}}}
	sub := NewReplaySubscription(entry, nil)
	_ = sub.Close()
	_, err := sub.Next(context.Background())
	if !errors.Is(err, io.EOF) {
		t.Fatalf("expected io.EOF after Close, got %v", err)
	}
}

func TestReplaySubscription_ContextCancelled(t *testing.T) {
	entry := &cache.StreamEntry{Chunks: []cache.ChunkRecord{{Delta: "a"}, {Done: true}}}
	sub := NewReplaySubscription(entry, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := sub.Next(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestReplaySubscription_RawBytesRoundTrip(t *testing.T) {
	// HIT replay must surface the upstream's exact SSE frame so byte-
	// equivalent output is preserved (full envelope, finish_reason, etc.).
	rawA := []byte(`data: {"id":"chatcmpl-x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"a"}}]}` + "\n\n")
	rawB := []byte(`data: {"id":"chatcmpl-x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"b"}}]}` + "\n\n")
	entry := &cache.StreamEntry{
		Provider: "openai", Model: "gpt-4o",
		Chunks: []cache.ChunkRecord{
			{Delta: "a", RawBytes: rawA},
			{Delta: "b", RawBytes: rawB},
			{Done: true},
		},
		CachedAt: time.Now().UTC(),
	}
	sub := NewReplaySubscription(entry, nil)
	defer func() { _ = sub.Close() }()

	ctx := context.Background()
	c1, err := sub.Next(ctx)
	if err != nil {
		t.Fatalf("Next #1: %v", err)
	}
	if string(c1.RawBytes) != string(rawA) {
		t.Fatalf("RawBytes #1: got %q, want %q", c1.RawBytes, rawA)
	}
	c2, err := sub.Next(ctx)
	if err != nil {
		t.Fatalf("Next #2: %v", err)
	}
	if string(c2.RawBytes) != string(rawB) {
		t.Fatalf("RawBytes #2: got %q, want %q", c2.RawBytes, rawB)
	}
}

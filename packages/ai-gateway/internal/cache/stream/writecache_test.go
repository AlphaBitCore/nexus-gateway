package streamcache

import (
	"context"
	"errors"
	"github.com/goccy/go-json"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	cache "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/core"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/alicebob/miniredis/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"
)

// newTestCache spins up a miniredis-backed *cache.Cache.
func newTestCacheForStreamcache(t *testing.T) (*cache.Cache, *miniredis.Miniredis) {
	t.Helper()
	s, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	rdb := redis.NewClient(&redis.Options{Addr: s.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	c := cache.New(rdb, cache.Config{
		Enabled: true,
		TTL:     time.Minute,
		Prefix:  "test:" + t.Name(),
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return c, s
}

// drainSub fully consumes a ChunkSubscription, returning the deltas in
// order. It treats EOF and ProviderError as terminal but does not
// distinguish — tests that need the error inspect it via a second
// Next call.
func drainSub(t *testing.T, sub ChunkSubscription) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var got []string
	for {
		c, err := sub.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			// surface raw error code for debugging but stop the loop.
			t.Logf("Next err: %v", err)
			break
		}
		if c.Delta != "" {
			got = append(got, c.Delta)
		}
	}
	return got
}

// TestRegistry_NoDedup_EachCallerLeadsAndStillFills verifies the WithDedup(false)
// path (cache.broker=false): same-key concurrent callers do NOT coalesce — each
// is its own leader (leaderFn runs per caller) — yet the cache is STILL filled.
// This is the #7 fix: broker=false previously meant the response was never
// written, so an admin-enabled L1 tier silently never filled. Contrast
// TestRegistry_ConcurrentSecondSubscriberJoins (dedup=true: one leader, joiners).
func TestRegistry_NoDedup_EachCallerLeadsAndStillFills(t *testing.T) {
	c, _ := newTestCacheForStreamcache(t)
	reg := NewRegistry(c, discardLogger(), nil, WithDedup(false))

	var leaderCalls atomic.Int32
	leaderFn := func(ctx context.Context) (provcore.StreamSession, *CacheMeta, error) {
		leaderCalls.Add(1)
		return &fakeSession{chunks: []provcore.Chunk{
			{Delta: "hi"},
			{Done: true},
		}}, &CacheMeta{Provider: "openai", Model: "gpt-4o", IsStream: true}, nil
	}

	s1, first1, err := reg.Subscribe(context.Background(), "k-nodedup", leaderFn)
	if err != nil {
		t.Fatal(err)
	}
	s2, first2, err := reg.Subscribe(context.Background(), "k-nodedup", leaderFn)
	if err != nil {
		t.Fatal(err)
	}

	if !first1 || !first2 {
		t.Fatalf("no-dedup: every caller must be its own leader; got first1=%v first2=%v", first1, first2)
	}
	if n := leaderCalls.Load(); n != 2 {
		t.Fatalf("no-dedup: leaderFn must run per caller; got %d, want 2", n)
	}

	drainSub(t, s1)
	drainSub(t, s2)
	_ = s1.Close()
	_ = s2.Close()
	reg.Wait() // let both pump goroutines flush writeCache

	if entry := c.LookupStream(context.Background(), "k-nodedup"); entry == nil {
		t.Fatal("no-dedup must still fill the cache; LookupStream returned nil")
	}
}

// TestBroker_WriteCache_StreamEntry_PersistsTimeline drives a real
// miniredis-backed cache so writeCache walks the StoreStream path on
// normal termination. Observable behaviour:
//   - the StreamEntry round-trips with the original chunks + usage
//   - writes_total{kind="stream",reason="ok"} == 1
func TestBroker_WriteCache_StreamEntry_PersistsTimeline(t *testing.T) {
	c, _ := newTestCacheForStreamcache(t)
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	usage := &provcore.Usage{PromptTokens: intPtr(7), CompletionTokens: intPtr(3)}
	sess := &fakeSession{chunks: []provcore.Chunk{
		{Delta: "hel", RawBytes: []byte("data: hel\n\n")},
		{Delta: "lo", RawBytes: []byte("data: lo\n\n")},
		{Done: true, Usage: usage},
	}}
	meta := CacheMeta{Provider: "openai", Model: "gpt-4o", IsStream: true, UpstreamHeaders: map[string][]string{"X-Foo": {"bar"}}}

	key := "k-stream"
	b := newBroker(context.Background(), key, meta, c, discardLogger(), m, func() {})
	go b.pump(sess)

	sub := b.subscribe()
	got := drainSub(t, sub)
	_ = sub.Close()

	if len(got) != 2 || got[0] != "hel" || got[1] != "lo" {
		t.Fatalf("deltas: got %v", got)
	}

	// Wait for pump to flush writeCache + close.
	<-b.pumpDone

	// Observable: round-trip the cache.
	entry := c.LookupStream(context.Background(), key)
	if entry == nil {
		t.Fatal("LookupStream returned nil; cache write missing")
	}
	if entry.Provider != "openai" || entry.Model != "gpt-4o" {
		t.Errorf("meta mismatch: %+v", entry)
	}
	if len(entry.Chunks) != 3 {
		t.Fatalf("chunks: got %d want 3", len(entry.Chunks))
	}
	if entry.Chunks[0].Delta != "hel" || entry.Chunks[1].Delta != "lo" {
		t.Errorf("delta sequence mismatch: %+v", entry.Chunks)
	}
	if entry.Usage.PromptTokens == nil || *entry.Usage.PromptTokens != 7 {
		t.Errorf("usage prompt: %+v", entry.Usage)
	}
	if vs := entry.UpstreamHeaders["X-Foo"]; len(vs) != 1 || vs[0] != "bar" {
		t.Errorf("UpstreamHeaders: %+v", entry.UpstreamHeaders)
	}
	// Observable: prometheus counter incremented.
	if got := testutil.ToFloat64(m.WritesTotal.WithLabelValues("stream", "ok")); got != 1 {
		t.Errorf("writes_total{stream,ok}: got %v want 1", got)
	}
}

// TestBroker_WriteCache_StreamEntry_InvokesL2Callback asserts that on a
// clean streaming termination the broker invokes OnStreamCachePersisted with
// the bare []ChunkRecord JSON timeline (what ToCacheStreamEntry decodes) and
// the final usage, so the proxy can mirror the stream into the L2 semantic
// cache as a response_kind=stream entry.
func TestBroker_WriteCache_StreamEntry_InvokesL2Callback(t *testing.T) {
	c, _ := newTestCacheForStreamcache(t)
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	usage := &provcore.Usage{PromptTokens: intPtr(7), CompletionTokens: intPtr(3)}
	sess := &fakeSession{chunks: []provcore.Chunk{
		{Delta: "hel", RawBytes: []byte("data: hel\n\n")},
		{Delta: "lo", RawBytes: []byte("data: lo\n\n")},
		{Done: true, Usage: usage},
	}}

	gotCh := make(chan []byte, 1)
	var gotUsage provcore.Usage
	meta := CacheMeta{
		Provider: "openai", Model: "gpt-4o", IsStream: true,
		OnStreamCachePersisted: func(body []byte, u provcore.Usage) {
			gotUsage = u
			gotCh <- body
		},
	}

	b := newBroker(context.Background(), "k-l2", meta, c, discardLogger(), m, func() {})
	go b.pump(sess)
	sub := b.subscribe()
	_ = drainSub(t, sub)
	_ = sub.Close()
	<-b.pumpDone

	var body []byte
	select {
	case body = <-gotCh:
	case <-time.After(2 * time.Second):
		t.Fatal("OnStreamCachePersisted was not invoked on clean stream termination")
	}

	var records []cache.ChunkRecord
	if err := json.Unmarshal(body, &records); err != nil {
		t.Fatalf("callback body is not a []ChunkRecord JSON: %v", err)
	}
	if len(records) != 3 || records[0].Delta != "hel" || records[1].Delta != "lo" || !records[2].Done {
		t.Errorf("callback timeline mismatch: %+v", records)
	}
	if gotUsage.PromptTokens == nil || *gotUsage.PromptTokens != 7 {
		t.Errorf("callback usage mismatch: %+v", gotUsage)
	}
}

// TestBroker_WriteCache_StreamEntry_NoL2CallbackOnIncompleteStream asserts the
// callback does NOT fire when the stream ends without a Done frame (io.EOF):
// writeCache is never reached, so no L2 mirror is attempted for a partial
// timeline.
func TestBroker_WriteCache_StreamEntry_NoL2CallbackOnIncompleteStream(t *testing.T) {
	c, _ := newTestCacheForStreamcache(t)
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	sess := &fakeSession{chunks: []provcore.Chunk{{Delta: "partial", RawBytes: []byte("data: partial\n\n")}}}
	fired := false
	meta := CacheMeta{
		Provider: "openai", Model: "gpt-4o", IsStream: true,
		OnStreamCachePersisted: func([]byte, provcore.Usage) { fired = true },
	}
	b := newBroker(context.Background(), "k-l2-eof", meta, c, discardLogger(), m, func() {})
	go b.pump(sess)
	sub := b.subscribe()
	_ = drainSub(t, sub)
	_ = sub.Close()
	<-b.pumpDone
	if fired {
		t.Error("OnStreamCachePersisted fired on incomplete (no-Done) stream")
	}
}

// TestBroker_WriteCache_StreamEntry_NoL2CallbackOnProviderError asserts the L2
// mirror callback does NOT fire when the upstream stream terminates with an
// error before a Done frame: pump takes the rb.Fail path and returns before
// writeCache, so a partial/errored timeline is never persisted to L2 (the
// "an errored stream is never cached" invariant).
func TestBroker_WriteCache_StreamEntry_NoL2CallbackOnProviderError(t *testing.T) {
	c, _ := newTestCacheForStreamcache(t)
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	sess := &errorSession{
		first: provcore.Chunk{Delta: "partial", RawBytes: []byte("data: partial\n\n")},
		err:   &provcore.ProviderError{Code: provcore.CodeUpstreamError, Message: "upstream exploded"},
	}
	fired := false
	meta := CacheMeta{
		Provider: "openai", Model: "gpt-4o", IsStream: true,
		OnStreamCachePersisted: func([]byte, provcore.Usage) { fired = true },
	}
	b := newBroker(context.Background(), "k-l2-err", meta, c, discardLogger(), m, func() {})
	go b.pump(sess)
	sub := b.subscribe()
	_ = drainSub(t, sub)
	_ = sub.Close()
	<-b.pumpDone
	if fired {
		t.Error("OnStreamCachePersisted fired on a provider-error stream")
	}
}

// TestBroker_WriteCache_ResponseEntry_PersistsCanonical asserts the
// non-stream (IsStream=false) path: one terminal Done chunk whose
// Delta carries the canonical JSON body is persisted via StoreResponse.
func TestBroker_WriteCache_ResponseEntry_PersistsCanonical(t *testing.T) {
	c, _ := newTestCacheForStreamcache(t)
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	canonical := []byte(`{"id":"chatcmpl-z","choices":[{"message":{"content":"hi"}}]}`)
	usage := &provcore.Usage{PromptTokens: intPtr(5)}
	sess := &fakeSession{chunks: []provcore.Chunk{
		{Done: true, Delta: string(canonical), Usage: usage},
	}}
	meta := CacheMeta{Provider: "openai", Model: "gpt-4o", IsStream: false}

	key := "k-response"
	b := newBroker(context.Background(), key, meta, c, discardLogger(), m, func() {})
	go b.pump(sess)
	sub := b.subscribe()
	_ = drainSub(t, sub)
	_ = sub.Close()
	<-b.pumpDone

	entry := c.LookupResponse(context.Background(), key)
	if entry == nil {
		t.Fatal("LookupResponse returned nil")
	}
	// The cache field is json.RawMessage; compare strings.
	if string(entry.CanonicalResponse) != string(canonical) {
		t.Errorf("canonical body mismatch: got %s want %s", entry.CanonicalResponse, canonical)
	}
	if entry.Usage.PromptTokens == nil || *entry.Usage.PromptTokens != 5 {
		t.Errorf("usage mismatch: %+v", entry.Usage)
	}
	if got := testutil.ToFloat64(m.WritesTotal.WithLabelValues("response", "ok")); got != 1 {
		t.Errorf("writes_total{response,ok}: got %v want 1", got)
	}
}

// TestBroker_WriteCache_NoCache_IsSilent asserts the b.cache == nil
// fast-return path — neither panics nor records a write metric.
func TestBroker_WriteCache_NoCache_IsSilent(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	sess := &fakeSession{chunks: []provcore.Chunk{{Done: true}}}
	meta := CacheMeta{IsStream: true}
	b := newBroker(context.Background(), "k-nil-cache", meta, nil, discardLogger(), m, func() {})
	go b.pump(sess)
	sub := b.subscribe()
	_ = drainSub(t, sub)
	_ = sub.Close()
	<-b.pumpDone
	for _, reason := range []string{"ok", "too_large", "encode_error"} {
		for _, kind := range []string{"stream", "response"} {
			if got := testutil.ToFloat64(m.WritesTotal.WithLabelValues(kind, reason)); got != 0 {
				t.Errorf("writes_total{%s,%s}: got %v want 0 (nil cache should not write)", kind, reason, got)
			}
		}
	}
}

// TestBroker_WriteCache_ResponseEntry_EmptyCollectedIsSkipped covers
// the `len(collected) == 0 || !last.Done` early-return in the response
// branch. Path: IsStream=false but the upstream emits no chunks at all
// (io.EOF without a Done frame). writeCache must not be invoked, but
// the synthetic terminal AppendTerminal makes subscribers see EOF.
func TestBroker_WriteCache_ResponseEntry_EmptyCollectedIsSkipped(t *testing.T) {
	c, _ := newTestCacheForStreamcache(t)
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	sess := &fakeSession{chunks: nil} // immediate io.EOF
	meta := CacheMeta{IsStream: false}
	b := newBroker(context.Background(), "k-empty", meta, c, discardLogger(), m, func() {})
	go b.pump(sess)
	sub := b.subscribe()
	_ = drainSub(t, sub)
	_ = sub.Close()
	<-b.pumpDone
	if entry := c.LookupResponse(context.Background(), "k-empty"); entry != nil {
		t.Errorf("expected no cache entry for empty response, got %+v", entry)
	}
	if got := testutil.ToFloat64(m.WritesTotal.WithLabelValues("response", "ok")); got != 0 {
		t.Errorf("writes_total{response,ok}: got %v want 0", got)
	}
}

// TestBroker_WriteCache_StreamEntry_TooLarge exercises the
// ErrCacheEntryTooLarge branch by configuring an absurdly small
// maxEntryBytes. Observable: no entry in cache, writes_total{stream,too_large} == 1.
func TestBroker_WriteCache_StreamEntry_TooLarge(t *testing.T) {
	s, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	rdb := redis.NewClient(&redis.Options{Addr: s.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	c := cache.New(rdb, cache.Config{
		Enabled:       true,
		TTL:           time.Minute,
		Prefix:        "test:too-large",
		MaxEntryBytes: 16, // any non-trivial entry blows this
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	sess := &fakeSession{chunks: []provcore.Chunk{
		{Delta: "lots and lots of bytes"},
		{Done: true},
	}}
	meta := CacheMeta{Provider: "openai", Model: "gpt-4o", IsStream: true}
	b := newBroker(context.Background(), "k-too-large", meta, c, discardLogger(), m, func() {})
	go b.pump(sess)
	sub := b.subscribe()
	_ = drainSub(t, sub)
	_ = sub.Close()
	<-b.pumpDone

	if entry := c.LookupStream(context.Background(), "k-too-large"); entry != nil {
		t.Errorf("oversize entry should not have been written: %+v", entry)
	}
	if got := testutil.ToFloat64(m.WritesTotal.WithLabelValues("stream", "too_large")); got != 1 {
		t.Errorf("writes_total{stream,too_large}: got %v want 1", got)
	}
}

// TestBroker_WriteCache_ResponseEntry_TooLarge — same as above for
// the non-stream branch.
func TestBroker_WriteCache_ResponseEntry_TooLarge(t *testing.T) {
	s, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	rdb := redis.NewClient(&redis.Options{Addr: s.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	c := cache.New(rdb, cache.Config{
		Enabled:       true,
		TTL:           time.Minute,
		Prefix:        "test:response-too-large",
		MaxEntryBytes: 8,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	sess := &fakeSession{chunks: []provcore.Chunk{
		{Done: true, Delta: `{"a":"this body easily exceeds the 8 byte cap"}`},
	}}
	meta := CacheMeta{Provider: "openai", Model: "gpt-4o", IsStream: false}
	b := newBroker(context.Background(), "k-resp-toolarge", meta, c, discardLogger(), m, func() {})
	go b.pump(sess)
	sub := b.subscribe()
	_ = drainSub(t, sub)
	_ = sub.Close()
	<-b.pumpDone

	if entry := c.LookupResponse(context.Background(), "k-resp-toolarge"); entry != nil {
		t.Errorf("oversize response should not have been written: %+v", entry)
	}
	if got := testutil.ToFloat64(m.WritesTotal.WithLabelValues("response", "too_large")); got != 1 {
		t.Errorf("writes_total{response,too_large}: got %v want 1", got)
	}
}

// TestBroker_WriteCache_StreamEntry_StoreError exercises the generic
// "cache stream store error" branch by shutting miniredis down before
// the broker tries to write — the Redis client now returns a connection
// error which is neither ErrCacheEntryTooLarge nor JSON-marshal.
func TestBroker_WriteCache_StreamEntry_StoreError(t *testing.T) {
	s, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	rdb := redis.NewClient(&redis.Options{Addr: s.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	c := cache.New(rdb, cache.Config{
		Enabled: true,
		TTL:     time.Minute,
		Prefix:  "test:store-err",
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	// Block the pump until after we shut down miniredis so writeCache
	// hits a network error rather than racing with the live server.
	gate := make(chan struct{})
	gated := &gatedSession{
		base: &fakeSession{chunks: []provcore.Chunk{
			{Delta: "x"}, {Done: true},
		}},
		gate: gate,
	}

	meta := CacheMeta{Provider: "openai", Model: "gpt-4o", IsStream: true}
	b := newBroker(context.Background(), "k-store-err", meta, c, discardLogger(), m, func() {})
	go b.pump(gated)

	// Kill the redis server before allowing pump to finish.
	s.Close()
	close(gate)

	sub := b.subscribe()
	_ = drainSub(t, sub)
	_ = sub.Close()
	<-b.pumpDone

	// Observable: encode_error counter ticked. The exact path inside
	// cache.StoreStream returns the connection error, which broker.go
	// maps to {kind=stream,reason=encode_error}.
	if got := testutil.ToFloat64(m.WritesTotal.WithLabelValues("stream", "encode_error")); got != 1 {
		t.Errorf("writes_total{stream,encode_error}: got %v want 1", got)
	}
}

// TestBroker_WriteCache_ResponseEntry_StoreError — mirror of the
// stream case for the non-stream branch.
func TestBroker_WriteCache_ResponseEntry_StoreError(t *testing.T) {
	s, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	rdb := redis.NewClient(&redis.Options{Addr: s.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	c := cache.New(rdb, cache.Config{
		Enabled: true,
		TTL:     time.Minute,
		Prefix:  "test:store-err-resp",
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	gate := make(chan struct{})
	gated := &gatedSession{
		base: &fakeSession{chunks: []provcore.Chunk{
			{Done: true, Delta: `{"ok":1}`},
		}},
		gate: gate,
	}
	meta := CacheMeta{Provider: "openai", Model: "gpt-4o", IsStream: false}
	b := newBroker(context.Background(), "k-store-err-resp", meta, c, discardLogger(), m, func() {})
	go b.pump(gated)

	s.Close()
	close(gate)

	sub := b.subscribe()
	_ = drainSub(t, sub)
	_ = sub.Close()
	<-b.pumpDone

	if got := testutil.ToFloat64(m.WritesTotal.WithLabelValues("response", "encode_error")); got != 1 {
		t.Errorf("writes_total{response,encode_error}: got %v want 1", got)
	}
}

// gatedSession defers serving the next chunk until gate is closed.
// Useful to drive a deterministic write-error sequence: kill the
// dependency, then close the gate.
type gatedSession struct {
	base    *fakeSession
	gate    chan struct{}
	calls   atomic.Int32
	closed  atomic.Bool
	allowed atomic.Bool
}

func (g *gatedSession) Next(ctx context.Context) (provcore.Chunk, error) {
	// First call returns immediately so the broker can stamp the first chunk.
	if g.calls.Add(1) == 1 {
		return g.base.Next(ctx)
	}
	if !g.allowed.Load() {
		<-g.gate
		g.allowed.Store(true)
	}
	return g.base.Next(ctx)
}

func (g *gatedSession) Close() error { g.closed.Store(true); return nil }

// TestFinalUsage_ReturnsLatestNonNil covers the finalUsage scan ordering.
// Asserts: (1) scans from the tail; (2) skips nil entries; (3) zero
// provcore.Usage{} fallback when nothing was emitted.
func TestFinalUsage_ReturnsLatestNonNil(t *testing.T) {
	u1 := provcore.Usage{PromptTokens: intPtr(1)}
	u2 := provcore.Usage{PromptTokens: intPtr(2)}

	got := finalUsage([]provcore.Chunk{
		{Usage: &u1},
		{Usage: nil},
		{Usage: &u2}, // most recent non-nil — should win
		{Usage: nil},
	})
	if got.PromptTokens == nil || *got.PromptTokens != 2 {
		t.Errorf("expected most recent non-nil usage (2), got %+v", got)
	}

	// No usage anywhere → zero-value provcore.Usage{}.
	got = finalUsage([]provcore.Chunk{{}, {}, {}})
	if got.PromptTokens != nil || got.CompletionTokens != nil {
		t.Errorf("expected zero-value usage, got %+v", got)
	}

	// Empty slice → zero-value.
	got = finalUsage(nil)
	if got.PromptTokens != nil {
		t.Errorf("expected zero-value on empty input, got %+v", got)
	}
}

// TestBroker_FinalUsage_PersistedThroughCache asserts the
// observable end-to-end: the most recent non-nil Usage from the
// chunk timeline lands in the cached StreamEntry.Usage.
func TestBroker_FinalUsage_PersistedThroughCache(t *testing.T) {
	c, _ := newTestCacheForStreamcache(t)
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	u1 := &provcore.Usage{PromptTokens: intPtr(11)}
	u2 := &provcore.Usage{PromptTokens: intPtr(22)}
	sess := &fakeSession{chunks: []provcore.Chunk{
		{Delta: "a", Usage: u1},
		{Delta: "b"},            // nil usage
		{Done: true, Usage: u2}, // tail wins
	}}
	meta := CacheMeta{Provider: "openai", Model: "gpt-4o", IsStream: true}
	b := newBroker(context.Background(), "k-usage", meta, c, discardLogger(), m, func() {})
	go b.pump(sess)
	sub := b.subscribe()
	_ = drainSub(t, sub)
	_ = sub.Close()
	<-b.pumpDone

	entry := c.LookupStream(context.Background(), "k-usage")
	if entry == nil {
		t.Fatal("entry missing")
	}
	if entry.Usage.PromptTokens == nil || *entry.Usage.PromptTokens != 22 {
		t.Errorf("expected tail usage (22), got %+v", entry.Usage)
	}
}

// TestRegistry_NilMetaTreatedAsError — leaderFn returning (sess, nil, nil)
// must close sess and surface a descriptive error to the caller. Also
// asserts that no broker is registered for the key.
func TestRegistry_NilMetaTreatedAsError(t *testing.T) {
	reg := NewRegistry(nil, discardLogger(), nil)
	var sessClosed atomic.Bool
	leader := func(ctx context.Context) (provcore.StreamSession, *CacheMeta, error) {
		return &recordingSession{onClose: func() { sessClosed.Store(true) }}, nil, nil
	}
	sub, first, err := reg.Subscribe(context.Background(), "k-nil-meta", leader)
	if err == nil {
		t.Fatal("expected error for nil CacheMeta")
	}
	if sub != nil {
		t.Errorf("sub should be nil, got %+v", sub)
	}
	if !first {
		t.Errorf("first should be true on error path")
	}
	if !sessClosed.Load() {
		t.Error("session was not closed on nil-meta error path")
	}
	if _, ok := reg.lookup("k-nil-meta"); ok {
		t.Error("broker should NOT be registered after nil-meta error")
	}
}

type recordingSession struct {
	onClose func()
}

func (r *recordingSession) Next(ctx context.Context) (provcore.Chunk, error) {
	return provcore.Chunk{}, io.EOF
}
func (r *recordingSession) Close() error {
	if r.onClose != nil {
		r.onClose()
	}
	return nil
}

// TestBrokerSub_NextAfterCloseReturnsEOF covers the s.closed.Load()
// fast path on Next() — the brokerSub equivalent of the replaySub test
// already in replay_test.go.
func TestBrokerSub_NextAfterCloseReturnsEOF(t *testing.T) {
	sess := &fakeSession{chunks: []provcore.Chunk{{Delta: "a"}, {Done: true}}}
	meta := CacheMeta{IsStream: true}
	b := newBroker(context.Background(), "k-sub-eof", meta, nil, discardLogger(), nil, func() {})
	go b.pump(sess)

	sub := b.subscribe()
	_ = sub.Close()
	_, err := sub.Next(context.Background())
	if !errors.Is(err, io.EOF) {
		t.Errorf("expected io.EOF after sub.Close(), got %v", err)
	}
}

// TestNewBroker_NilLoggerAndOnCloseUseSafeDefaults exercises the two
// nil-guard branches in newBroker (log==nil → slog.Default; onClose==nil
// → no-op). The pump must still run and decrement broker_active.
func TestNewBroker_NilLoggerAndOnCloseUseSafeDefaults(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	sess := &fakeSession{chunks: []provcore.Chunk{{Done: true}}}
	meta := CacheMeta{IsStream: true}
	// nil logger, nil onClose
	b := newBroker(context.Background(), "k-defaults", meta, nil, nil, m, nil)
	if b.log == nil {
		t.Fatal("log should default to slog.Default()")
	}
	if b.onClose == nil {
		t.Fatal("onClose should default to no-op")
	}
	// IncBrokerActive ran at construct; confirm gauge==1.
	if got := testutil.ToFloat64(m.BrokerActive); got != 1 {
		t.Errorf("broker_active after newBroker: got %v want 1", got)
	}

	go b.pump(sess)
	sub := b.subscribe()
	_ = drainSub(t, sub)
	_ = sub.Close()
	<-b.pumpDone

	if got := testutil.ToFloat64(m.BrokerActive); got != 0 {
		t.Errorf("broker_active after pump exit: got %v want 0", got)
	}
}

// TestNewRegistry_NilLoggerUsesDefault covers the log==nil branch in
// NewRegistry. Observable: subscribe works and panics nowhere.
func TestNewRegistry_NilLoggerUsesDefault(t *testing.T) {
	r := NewRegistry(nil, nil, nil)
	if r.log == nil {
		t.Fatal("log should default to slog.Default()")
	}
	leader := func(ctx context.Context) (provcore.StreamSession, *CacheMeta, error) {
		return &fakeSession{chunks: []provcore.Chunk{{Done: true}}}, &CacheMeta{IsStream: true}, nil
	}
	sub, first, err := r.Subscribe(context.Background(), "k", leader)
	if err != nil {
		t.Fatal(err)
	}
	if !first {
		t.Fatal("first should be true")
	}
	_ = sub.Close()
}

// TestStreamEntry_RoundTripJSONEqual sanity-checks that the cached
// StreamEntry deserialises to the same shape that producing it
// emitted — guards against an accidental encoder/decoder drift.
func TestStreamEntry_RoundTripJSONEqual(t *testing.T) {
	c, _ := newTestCacheForStreamcache(t)
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	sess := &fakeSession{chunks: []provcore.Chunk{
		{Delta: "x"}, {Done: true, Usage: &provcore.Usage{PromptTokens: intPtr(1)}},
	}}
	meta := CacheMeta{Provider: "openai", Model: "gpt-4o", IsStream: true}
	b := newBroker(context.Background(), "k-rt", meta, c, discardLogger(), m, func() {})
	go b.pump(sess)
	sub := b.subscribe()
	_ = drainSub(t, sub)
	_ = sub.Close()
	<-b.pumpDone

	entry := c.LookupStream(context.Background(), "k-rt")
	if entry == nil {
		t.Fatal("entry missing")
	}
	raw, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded cache.StreamEntry
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(decoded.Chunks) != len(entry.Chunks) {
		t.Errorf("chunk count mismatch on round-trip: %d vs %d", len(decoded.Chunks), len(entry.Chunks))
	}
}

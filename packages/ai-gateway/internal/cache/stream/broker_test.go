package streamcache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
)

// fakeSession is a programmable StreamSession for tests.
type fakeSession struct {
	chunks []provcore.Chunk
	idx    int
	mu     sync.Mutex
	closed bool
}

func (f *fakeSession) Next(ctx context.Context) (provcore.Chunk, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.idx >= len(f.chunks) {
		return provcore.Chunk{}, io.EOF
	}
	c := f.chunks[f.idx]
	f.idx++
	return c, nil
}

func (f *fakeSession) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

// blockingSession blocks Next() until ctx is cancelled OR until
// release is closed (whichever comes first). Used to simulate slow
// upstreams for ref-count cancel tests.
type blockingSession struct {
	release chan struct{}
	closed  atomic.Bool
}

func (b *blockingSession) Next(ctx context.Context) (provcore.Chunk, error) {
	select {
	case <-ctx.Done():
		return provcore.Chunk{}, ctx.Err()
	case <-b.release:
		return provcore.Chunk{}, io.EOF
	}
}

func (b *blockingSession) Close() error { b.closed.Store(true); return nil }

// errorSession returns first then errs.
type errorSession struct {
	first  provcore.Chunk
	err    error
	served bool
	closed atomic.Bool
}

func (e *errorSession) Next(ctx context.Context) (provcore.Chunk, error) {
	if !e.served {
		e.served = true
		return e.first, nil
	}
	return provcore.Chunk{}, e.err
}

func (e *errorSession) Close() error { e.closed.Store(true); return nil }

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestBroker_NormalTermination_DeliversChunks(t *testing.T) {
	sess := &fakeSession{chunks: []provcore.Chunk{
		{Delta: "hi"},
		{Done: true},
	}}
	meta := CacheMeta{Provider: "openai", Model: "gpt-4o", IsStream: true}
	closed := make(chan struct{})
	b := newBroker(context.Background(), "k1", meta, nil /*cache*/, discardLogger(), nil /*metrics*/, func() { close(closed) })
	go b.pump(sess)

	sub := b.subscribe()
	defer func() { _ = sub.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var got []string
	for {
		c, err := sub.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if c.Delta != "" {
			got = append(got, c.Delta)
		}
	}
	if len(got) != 1 || got[0] != "hi" {
		t.Fatalf("got %v", got)
	}

	// onClose fires after pump exits.
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("onClose never fired")
	}
}

func TestBroker_RefCountZeroCancelsUpstream(t *testing.T) {
	sess := &blockingSession{release: make(chan struct{})}
	meta := CacheMeta{Provider: "openai", Model: "gpt-4o", IsStream: true}
	closed := make(chan struct{})
	b := newBroker(context.Background(), "k2", meta, nil, discardLogger(), nil, func() { close(closed) })
	go b.pump(sess)

	sub := b.subscribe()
	// Subscriber leaves immediately; no other subscribers.
	if err := sub.Close(); err != nil {
		t.Fatal(err)
	}

	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		close(sess.release)
		t.Fatal("pump did not terminate after ref hit zero")
	}
}

func TestBroker_TwoSubscribers_BothReceiveAllChunks(t *testing.T) {
	sess := &fakeSession{chunks: []provcore.Chunk{
		{Delta: "a"}, {Delta: "b"}, {Done: true},
	}}
	meta := CacheMeta{IsStream: true}
	b := newBroker(context.Background(), "k3", meta, nil, discardLogger(), nil, func() {})

	s1 := b.subscribe()
	defer func() { _ = s1.Close() }()
	s2 := b.subscribe()
	defer func() { _ = s2.Close() }()

	go b.pump(sess)

	drain := func(s ChunkSubscription) []string {
		var out []string
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		for {
			c, err := s.Next(ctx)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return append(out, "ERR:"+err.Error())
			}
			if c.Delta != "" {
				out = append(out, c.Delta)
			}
		}
		return out
	}

	var wg sync.WaitGroup
	var g1, g2 []string
	wg.Add(2)
	go func() { defer wg.Done(); g1 = drain(s1) }()
	go func() { defer wg.Done(); g2 = drain(s2) }()
	wg.Wait()

	want := []string{"a", "b"}
	for _, got := range [][]string{g1, g2} {
		if len(got) != len(want) {
			t.Fatalf("len mismatch: got %v", got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("element %d: got %s want %s", i, got[i], want[i])
			}
		}
	}
}

func TestBroker_LateSubscriberReplaysFromZero(t *testing.T) {
	sess := &fakeSession{chunks: []provcore.Chunk{
		{Delta: "a"}, {Delta: "b"}, {Done: true},
	}}
	meta := CacheMeta{IsStream: true}
	b := newBroker(context.Background(), "k4", meta, nil, discardLogger(), nil, func() {})
	donePump := make(chan struct{})
	go func() { b.pump(sess); close(donePump) }()

	// Wait for pump to finish.
	<-donePump

	// Late subscribe — the broker's pump has finished, but ringbuffer
	// still holds chunks. Late join should still see all of them.
	sub := b.subscribe()
	defer func() { _ = sub.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var got []string
	for {
		c, err := sub.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if c.Delta != "" {
			got = append(got, c.Delta)
		}
	}
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("late join got %v", got)
	}
}

func TestBroker_UpstreamErrorBroadcasts(t *testing.T) {
	sess := &errorSession{
		first: provcore.Chunk{Delta: "preview"},
		err:   &provcore.ProviderError{Code: provcore.CodeUpstreamError, Message: "boom"},
	}
	meta := CacheMeta{IsStream: true}
	b := newBroker(context.Background(), "k5", meta, nil, discardLogger(), nil, func() {})
	go b.pump(sess)

	sub := b.subscribe()
	defer func() { _ = sub.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	chunk, err := sub.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if chunk.Delta != "preview" {
		t.Fatalf("got %s", chunk.Delta)
	}

	_, err = sub.Next(ctx)
	pe := &provcore.ProviderError{}
	ok := errors.As(err, &pe)
	if !ok {
		t.Fatalf("expected ProviderError, got %T: %v", err, err)
	}
	if pe.Code != provcore.CodeUpstreamError {
		t.Fatalf("unexpected code: %s", pe.Code)
	}
}

func TestBroker_SubscriberCloseIsIdempotent(t *testing.T) {
	sess := &fakeSession{chunks: []provcore.Chunk{{Done: true}}}
	meta := CacheMeta{IsStream: true}
	b := newBroker(context.Background(), "k6", meta, nil, discardLogger(), nil, func() {})
	go b.pump(sess)

	sub := b.subscribe()
	if err := sub.Close(); err != nil {
		t.Fatal(err)
	}
	// Second Close must not panic, must not double-decrement.
	if err := sub.Close(); err != nil {
		t.Fatal(err)
	}
}

// Registry tests

func TestRegistry_FirstSubscriberInvokesLeaderFn(t *testing.T) {
	reg := NewRegistry(nil, discardLogger(), nil)
	var leaderCalls atomic.Int32
	leaderFn := func(ctx context.Context) (provcore.StreamSession, *CacheMeta, error) {
		leaderCalls.Add(1)
		return &fakeSession{chunks: []provcore.Chunk{{Done: true}}}, &CacheMeta{IsStream: true}, nil
	}
	sub, first, err := reg.Subscribe(context.Background(), "rk1", leaderFn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Close() }()
	if !first {
		t.Fatal("expected isFirstSubscriber=true")
	}
	if leaderCalls.Load() != 1 {
		t.Fatalf("leaderFn called %d times; expected 1", leaderCalls.Load())
	}
}

func TestRegistry_ConcurrentSecondSubscriberJoins(t *testing.T) {
	reg := NewRegistry(nil, discardLogger(), nil)
	var leaderCalls atomic.Int32
	// Use a slow leaderFn (blockingSession) so the second Subscribe
	// arrives while the first broker is still pumping.
	leaderFn := func(ctx context.Context) (provcore.StreamSession, *CacheMeta, error) {
		leaderCalls.Add(1)
		return &blockingSession{release: make(chan struct{})}, &CacheMeta{IsStream: true}, nil
	}
	s1, first1, err := reg.Subscribe(context.Background(), "rk2", leaderFn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s1.Close() }()

	s2, first2, err := reg.Subscribe(context.Background(), "rk2", leaderFn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s2.Close() }()

	if !first1 {
		t.Fatal("first sub not flagged first")
	}
	if first2 {
		t.Fatal("second sub incorrectly flagged first")
	}
	if leaderCalls.Load() != 1 {
		t.Fatalf("leaderFn called %d times; expected 1", leaderCalls.Load())
	}
}

func TestRegistry_BrokerDeregistersOnTerminate(t *testing.T) {
	reg := NewRegistry(nil, discardLogger(), nil)
	leaderFn := func(ctx context.Context) (provcore.StreamSession, *CacheMeta, error) {
		return &fakeSession{chunks: []provcore.Chunk{{Done: true}}}, &CacheMeta{IsStream: true}, nil
	}
	s1, _, err := reg.Subscribe(context.Background(), "rk3", leaderFn)
	if err != nil {
		t.Fatal(err)
	}

	// Drain to terminal so the broker writes (no cache, no-op) and onClose fires.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for {
		_, err := s1.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	_ = s1.Close()

	// Wait up to 1s for deregistration.
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if _, exists := reg.lookup("rk3"); !exists {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("broker not deregistered after pump terminate")
}

func TestRegistry_AfterTerminateNewSubscribeStartsFreshBroker(t *testing.T) {
	reg := NewRegistry(nil, discardLogger(), nil)
	var leaderCalls atomic.Int32
	leaderFn := func(ctx context.Context) (provcore.StreamSession, *CacheMeta, error) {
		leaderCalls.Add(1)
		return &fakeSession{chunks: []provcore.Chunk{{Done: true}}}, &CacheMeta{IsStream: true}, nil
	}

	// First subscribe + drain + close => broker terminates and deregisters.
	s1, _, err := reg.Subscribe(context.Background(), "rk4", leaderFn)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for {
		_, err := s1.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	_ = s1.Close()

	// Wait for deregistration.
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if _, exists := reg.lookup("rk4"); !exists {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Now subscribe again with the same key — should start a fresh broker.
	s2, first2, err := reg.Subscribe(context.Background(), "rk4", leaderFn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s2.Close() }()
	if !first2 {
		t.Fatal("expected isFirstSubscriber=true on fresh broker")
	}
	if leaderCalls.Load() != 2 {
		t.Fatalf("leaderFn calls = %d; expected 2 (one per broker)", leaderCalls.Load())
	}
}

func TestRegistry_LeaderFnErrorPropagates(t *testing.T) {
	reg := NewRegistry(nil, discardLogger(), nil)
	wantErr := errors.New("upstream open failed")
	leaderFn := func(ctx context.Context) (provcore.StreamSession, *CacheMeta, error) {
		return nil, nil, wantErr
	}
	sub, first, err := reg.Subscribe(context.Background(), "rk5", leaderFn)
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected wantErr, got %v", err)
	}
	if sub != nil {
		t.Fatal("expected nil sub on leaderFn error")
	}
	// Whether first or not is implementation-defined for the error case; the
	// plan says first=true is acceptable. Don't assert on it strictly.
	_ = first
}

// TestRegistry_DifferentKeysSubscribeConcurrently is the regression test
// for the perf regression where r.mu was held across leaderFn, serialising
// every Subscribe regardless of cache key. With the fix (pendingBroker
// placeholder + r.mu released across leaderFn), N different-key Subscribe
// calls launched in parallel must complete in ~one leaderFn duration, not
// N × duration.
func TestRegistry_DifferentKeysSubscribeConcurrently(t *testing.T) {
	reg := NewRegistry(nil, discardLogger(), nil)
	const n = 10
	const leaderDelay = 200 * time.Millisecond

	// Each leaderFn sleeps to model an upstream TTFB roundtrip.
	leaderFn := func(_ context.Context) (provcore.StreamSession, *CacheMeta, error) {
		time.Sleep(leaderDelay)
		return &fakeSession{chunks: []provcore.Chunk{{Done: true}}}, &CacheMeta{IsStream: true}, nil
	}

	start := time.Now()
	var wg sync.WaitGroup
	results := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			key := fmt.Sprintf("diffkey-%d", idx)
			sub, _, err := reg.Subscribe(context.Background(), key, leaderFn)
			results[idx] = err
			if sub != nil {
				_ = sub.Close()
			}
		}(i)
	}
	wg.Wait()
	elapsed := time.Since(start)

	for i, err := range results {
		if err != nil {
			t.Errorf("Subscribe[%d] failed: %v", i, err)
		}
	}

	// Serial worst case: n * leaderDelay = 2s. Parallel best case: ~leaderDelay = 200ms.
	// Give scheduler headroom but stay well under the serial bound.
	upper := 3 * leaderDelay // 600ms — proves we are NOT serial.
	if elapsed > upper {
		t.Errorf("Subscribe for %d different keys took %v; expected < %v (serial baseline would be %v) — Registry.mu likely re-introduced over leaderFn",
			n, elapsed, upper, time.Duration(n)*leaderDelay)
	}
}

// TestRegistry_SameKeyJoinerWaitsWithoutSerialising is the orthogonal
// half of the fix: same-key callers MUST still dedupe to one leaderFn
// (the design intent of the broker), but they MUST do so by waiting on
// the pendingBroker.done channel, NOT by blocking other goroutines.
// This test asserts (a) leaderFn fires once across many same-key
// callers and (b) all joiners successfully receive subscriptions.
func TestRegistry_SameKeyJoinerWaitsWithoutSerialising(t *testing.T) {
	reg := NewRegistry(nil, discardLogger(), nil)
	const n = 5

	var leaderCalls atomic.Int32
	leaderFn := func(_ context.Context) (provcore.StreamSession, *CacheMeta, error) {
		leaderCalls.Add(1)
		time.Sleep(50 * time.Millisecond)
		// Return a session that stays open so the broker is still alive
		// for joiners arriving after the first leaderFn returns.
		return &blockingSession{release: make(chan struct{})}, &CacheMeta{IsStream: true}, nil
	}

	var wg sync.WaitGroup
	subs := make([]ChunkSubscription, n)
	firstCount := atomic.Int32{}
	for i := range n {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			s, first, err := reg.Subscribe(context.Background(), "shared-key", leaderFn)
			if err != nil {
				t.Errorf("Subscribe[%d] failed: %v", idx, err)
				return
			}
			subs[idx] = s
			if first {
				firstCount.Add(1)
			}
		}(i)
	}
	wg.Wait()
	for _, s := range subs {
		if s != nil {
			_ = s.Close()
		}
	}

	if got := leaderCalls.Load(); got != 1 {
		t.Errorf("leaderFn called %d times; expected exactly 1 (same-key dedupe)", got)
	}
	if got := firstCount.Load(); got != 1 {
		t.Errorf("isFirstSubscriber=true returned %d times; expected exactly 1", got)
	}
}

// TestRegistry_SameKeyLeaderFailedTriggersRetry asserts that when the
// leader's leaderFn fails, same-key joiners waiting on the pending
// entry do NOT receive a sub. The test relies on the implementation
// detail that joiners loop after pendingBroker.done closes and re-enter
// as new leaders themselves (acceptable for the error path — each
// caller's request can independently re-attempt).
func TestRegistry_SameKeyLeaderFailedTriggersRetry(t *testing.T) {
	reg := NewRegistry(nil, discardLogger(), nil)
	wantErr := errors.New("upstream down")

	var attempts atomic.Int32
	leaderFn := func(_ context.Context) (provcore.StreamSession, *CacheMeta, error) {
		attempts.Add(1)
		time.Sleep(30 * time.Millisecond)
		return nil, nil, wantErr
	}

	var wg sync.WaitGroup
	results := make([]error, 3)
	for i := range 3 {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, _, err := reg.Subscribe(context.Background(), "errkey", leaderFn)
			results[idx] = err
		}(i)
	}
	wg.Wait()

	for i, err := range results {
		if !errors.Is(err, wantErr) {
			t.Errorf("Subscribe[%d]: expected %v, got %v", i, wantErr, err)
		}
	}
	// At least one leaderFn fired; up to 3 are acceptable (waiters may
	// re-enter as leaders after the first failure). What matters is
	// that the error surfaced to every caller.
	if got := attempts.Load(); got < 1 {
		t.Errorf("leaderFn called %d times; expected >= 1", got)
	}
}

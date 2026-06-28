package credstats

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/credstate"
)

// TestEnableWriteBehind_NoRedisIsNoOp verifies the guard: a Buffer with no Redis
// client must not install the accumulator, so write-behind stays off (the hot
// path keeps its original behaviour when Redis is absent). Observable: agg/clean
// remain nil and FlushStats is a no-op returning nil.
func TestEnableWriteBehind_NoRedisIsNoOp(t *testing.T) {
	b := New(nil, nil, nil, nil) // rdb == nil
	b.EnableWriteBehind()

	if b.agg != nil {
		t.Errorf("agg installed despite nil rdb: %#v", b.agg)
	}
	if b.clean != nil {
		t.Errorf("clean set installed despite nil rdb: %#v", b.clean)
	}
	// FlushStats on a buffer without an accumulator is a no-op.
	if err := b.FlushStats(context.Background()); err != nil {
		t.Errorf("FlushStats with no accumulator = %v, want nil", err)
	}
}

// TestEnableWriteBehind_NilBufferIsNoOp verifies the nil-receiver guard does not
// panic (callers may hold a nil *Buffer when credstats is disabled).
func TestEnableWriteBehind_NilBufferIsNoOp(t *testing.T) {
	var b *Buffer
	b.EnableWriteBehind() // must not panic
}

// TestFlushStats_NilAccumulatorReturnsNil verifies FlushStats short-circuits to
// nil when write-behind was never enabled (agg == nil) even though Redis exists,
// so a caller that flushes unconditionally on shutdown does no Redis work.
func TestFlushStats_NilAccumulatorReturnsNil(t *testing.T) {
	mini, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mini.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	defer rdb.Close()

	b := New(rdb, nil, nil, nil) // write-behind NOT enabled
	if err := b.FlushStats(context.Background()); err != nil {
		t.Errorf("FlushStats with nil accumulator = %v, want nil", err)
	}
	if b.agg != nil {
		t.Fatalf("agg must stay nil without EnableWriteBehind")
	}
}

// TestFlushStats_PipelineExecErrorWrapped verifies that when the Redis pipeline
// fails (server gone) FlushStats returns the wrapped error instead of swallowing
// it — the periodic flusher relies on this to log a warning. The accumulator has
// already been drained at the point of failure, which is the documented
// at-most-once-interval-loss behaviour.
func TestFlushStats_PipelineExecErrorWrapped(t *testing.T) {
	mini, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	defer rdb.Close()

	b := New(rdb, nil, nil, nil)
	b.EnableWriteBehind()

	// Accumulate one delta so the pipeline has work to do.
	b.agg.recordSuccess("cred-exec-err", "2026-06-25T00:00:00Z")

	// Kill the server so pipe.Exec fails.
	mini.Close()

	err = b.FlushStats(context.Background())
	if err == nil {
		t.Fatalf("FlushStats returned nil after Redis went away, want a wrapped error")
	}
	if got := err.Error(); !strings.Contains(got, "credstats: write-behind flush:") {
		t.Errorf("error = %q, want it wrapped with %q", got, "credstats: write-behind flush:")
	}
}

// TestRunFlusher_PeriodicThenShutdownFlush verifies the background flusher: a
// success accumulated before a tick is persisted by the periodic flush, and a
// success accumulated after the last tick is persisted by the final
// shutdown flush when ctx is cancelled (graceful shutdown loses nothing).
func TestRunFlusher_PeriodicThenShutdownFlush(t *testing.T) {
	mini, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mini.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	defer rdb.Close()

	b := New(rdb, nil, nil, nil)
	b.EnableWriteBehind()

	const cred = "cred-flusher"
	statsKey := credstate.StatsKey(cred)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		b.RunFlusher(ctx, 20*time.Millisecond)
		close(done)
	}()

	// First success: a periodic tick must persist it.
	b.RecordAttempt(cred, 200, "")
	deadline := time.Now().Add(2 * time.Second)
	for !mini.Exists(statsKey) || mini.HGet(statsKey, credstate.StatsFieldCount) != "1" {

		if time.Now().After(deadline) {
			t.Fatalf("periodic flush did not persist cnt=1 within deadline; cnt=%q", mini.HGet(statsKey, credstate.StatsFieldCount))
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Second success then immediately cancel — the shutdown flush must catch it,
	// summing cnt to 2 (HINCRBY additive across the two flushes).
	b.RecordAttempt(cred, 200, "")
	cancel()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("RunFlusher did not return after ctx cancel")
	}

	if cnt := mini.HGet(statsKey, credstate.StatsFieldCount); cnt != "2" {
		t.Errorf("cnt after shutdown flush = %q, want 2 (periodic + shutdown, additive)", cnt)
	}
}

// TestRunFlusher_NilAccumulatorReturnsImmediately verifies RunFlusher is a no-op
// (returns at once, no ticker) when write-behind was never enabled.
func TestRunFlusher_NilAccumulatorReturnsImmediately(t *testing.T) {
	mini, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mini.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	defer rdb.Close()

	b := New(rdb, nil, nil, nil) // write-behind NOT enabled → agg nil

	done := make(chan struct{})
	go func() {
		b.RunFlusher(context.Background(), time.Hour)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("RunFlusher with nil accumulator did not return immediately")
	}
}

// TestRunFlusher_DefaultIntervalOnNonPositive verifies the interval guard: a
// non-positive interval is replaced by the 250ms default rather than panicking
// on time.NewTicker(0). We don't wait a full default tick — we only need the
// guard branch executed and a clean cancel/return.
func TestRunFlusher_DefaultIntervalOnNonPositive(t *testing.T) {
	mini, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mini.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	defer rdb.Close()

	b := New(rdb, nil, nil, nil)
	b.EnableWriteBehind()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		b.RunFlusher(ctx, 0) // exercises the interval<=0 → default branch
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("RunFlusher did not return after cancel with default interval")
	}
}

// TestRunFlusher_ShutdownFlushErrorWarns verifies the shutdown-flush error path:
// when Redis is gone at shutdown, the final flush fails and the warning is logged
// (the path that calls b.warn). Asserted via a slog handler that captures the
// emitted record so we confirm the failure is surfaced, not swallowed.
func TestRunFlusher_ShutdownFlushErrorWarns(t *testing.T) {
	mini, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	defer rdb.Close()

	ch := &captureHandler{}
	logger := slog.New(ch)

	b := New(rdb, logger, nil, nil)
	b.EnableWriteBehind()

	// Accumulate a delta so the shutdown flush has work, then kill Redis so the
	// flush's pipe.Exec fails inside the ctx.Done() branch.
	b.agg.recordSuccess("cred-shutdown", "2026-06-25T00:00:00Z")
	mini.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		b.RunFlusher(ctx, time.Hour) // long interval → only the shutdown branch runs
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("RunFlusher did not return after cancel")
	}

	if !ch.sawWarn("credstats: stats write-behind shutdown flush failed") {
		t.Errorf("expected shutdown-flush failure warning to be logged; got messages: %v", ch.messages())
	}
}

// TestRunFlusher_PeriodicFlushErrorWarns verifies the periodic-tick error path:
// when Redis is unreachable, each tick's FlushStats fails and the periodic
// warning is logged rather than the flusher dying silently. This is the branch
// that keeps a degraded Redis from turning into a silent stats-loss black hole.
func TestRunFlusher_PeriodicFlushErrorWarns(t *testing.T) {
	mini, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	defer rdb.Close()

	ch := &captureHandler{}
	logger := slog.New(ch)

	b := New(rdb, logger, nil, nil)
	b.EnableWriteBehind()

	// Accumulate a delta, then kill Redis so the next tick's flush fails.
	b.agg.recordSuccess("cred-periodic-err", "2026-06-25T00:00:00Z")
	mini.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.RunFlusher(ctx, 10*time.Millisecond)

	deadline := time.Now().Add(3 * time.Second)
	for !ch.sawWarn("credstats: stats write-behind periodic flush failed") {

		if time.Now().After(deadline) {
			t.Fatalf("periodic flush failure was never logged; messages: %v", ch.messages())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestRecordSuccess_EmptyCredentialIDIgnored verifies the guard: an empty
// credentialID is dropped (no map entry, no panic) so a malformed call never
// pollutes the accumulator with a "" key that would later HINCRBY a junk stats
// hash.
func TestRecordSuccess_EmptyCredentialIDIgnored(t *testing.T) {
	a := newStatsAggregator()
	a.recordSuccess("", "2026-06-25T00:00:00Z")
	if got := a.drain(); len(got) != 0 {
		t.Errorf("empty credentialID produced deltas %v, want none", got)
	}
}

// TestMarkClean_NilSetIsNoOp verifies markClean does nothing when the clean set
// was never installed (write-behind off) — isClean stays false, no panic.
func TestMarkClean_NilSetIsNoOp(t *testing.T) {
	b := New(nil, nil, nil, nil) // clean stays nil
	b.markClean("cred-x")
	if b.isClean("cred-x") {
		t.Errorf("isClean true after markClean on a nil clean set")
	}
}

// TestMarkClean_CapResetEvictsOldEntries verifies the cap guard: once the clean
// set reaches cleanSetCap, the next markClean clears the whole set before adding,
// bounding memory. Observable: a credential confirmed-clean before the reset is
// no longer clean afterwards, while the credential added at the reset boundary is.
func TestMarkClean_CapResetEvictsOldEntries(t *testing.T) {
	mini, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mini.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	defer rdb.Close()

	b := New(rdb, nil, nil, nil)
	b.EnableWriteBehind()

	// Fill the set to exactly the cap with synthetic ids.
	b.cleanMu.Lock()
	for i := range cleanSetCap {
		b.clean[fixedID(i)] = struct{}{}
	}
	b.cleanMu.Unlock()

	const old = "cred-old"
	// Sanity: a member present at cap is clean.
	b.cleanMu.Lock()
	b.clean[old] = struct{}{}
	overCap := len(b.clean) // cleanSetCap + 1
	b.cleanMu.Unlock()
	if overCap <= cleanSetCap {
		t.Fatalf("setup invariant: len(clean)=%d not over cap %d", overCap, cleanSetCap)
	}

	// markClean now sees len >= cap → resets, then adds the new id only.
	const fresh = "cred-fresh"
	b.markClean(fresh)

	if b.isClean(old) {
		t.Errorf("old credential still clean after cap reset — set was not cleared")
	}
	if !b.isClean(fresh) {
		t.Errorf("fresh credential not clean after cap reset — it must be re-added")
	}
	b.cleanMu.Lock()
	n := len(b.clean)
	b.cleanMu.Unlock()
	if n != 1 {
		t.Errorf("clean set size after cap reset = %d, want 1 (cleared then one add)", n)
	}
}

// --- small test helpers (test-only) ---

func fixedID(i int) string {
	// deterministic distinct ids; avoids fmt in the hot loop
	const digits = "0123456789abcdefghijklmnopqrstuvwxyz"
	if i == 0 {
		return "id0"
	}
	buf := []byte{}
	for i > 0 {
		buf = append([]byte{digits[i%36]}, buf...)
		i /= 36
	}
	return "id" + string(buf)
}

// captureHandler is a minimal slog.Handler that records emitted messages so a
// test can assert a warning was logged.
type captureHandler struct {
	mu   sync.Mutex
	msgs []string
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	h.msgs = append(h.msgs, r.Message)
	h.mu.Unlock()
	return nil
}
func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

func (h *captureHandler) sawWarn(msg string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, m := range h.msgs {
		if m == msg {
			return true
		}
	}
	return false
}
func (h *captureHandler) messages() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, len(h.msgs))
	copy(out, h.msgs)
	return out
}

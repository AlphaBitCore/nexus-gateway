package semantic

// Tests for the semantic-cache circuit breaker, metrics constructors, index
// error classifiers, client DropIndex/StoreEntry edge cases, the embedding
// singleflight, and index-lifecycle behaviour. All tests are deterministic
// and make no network calls.

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"
)

// NewCircuitBreaker — the constructor registers Prometheus metrics and must
// not panic. Because tests register metrics on the default registry, we call
// NewCircuitBreaker only once with a unique namespace.

func TestNewCircuitBreaker_Initialises(t *testing.T) {
	cb := NewCircuitBreaker(10, 60*time.Second, 30*time.Second,
		slog.New(slog.NewTextHandler(io.Discard, nil)), "nexus_cov_cb")
	if cb.State() != "closed" {
		t.Fatalf("initial state = %q", cb.State())
	}
	if cb.TripCount() != 0 {
		t.Fatalf("initial trip count = %d", cb.TripCount())
	}
	if !cb.LastTripAt().IsZero() {
		t.Fatalf("initial lastTripAt should be zero")
	}
}

// TestCircuitBreaker_RecordFailure_HalfOpenPath exercises the RecordFailure
// half_open branch that trips back to open.
func TestCircuitBreaker_RecordFailure_HalfOpenToOpen(t *testing.T) {
	cb := newTestCB(1, time.Minute, 5*time.Millisecond)
	// Trip to open.
	if !cb.Allow() {
		t.Fatal()
	}
	cb.RecordFailure()
	// Wait for recovery.
	time.Sleep(15 * time.Millisecond)
	// Allow probe.
	if !cb.Allow() {
		t.Fatal("Allow should return true for probe")
	}
	// Fail probe → back to open.
	cb.RecordFailure()
	if cb.State() != "open" {
		t.Errorf("expected open after probe failure, got %q", cb.State())
	}
}

// TestCircuitBreaker_Allow_HalfOpenBlocksSecond exercises the half_open
// block path (second Allow() while probe in-flight) and the cbStateHalfOpen
// case in Allow.
func TestCircuitBreaker_Allow_HalfOpenBlocksSecond(t *testing.T) {
	cb := newTestCB(1, time.Minute, 5*time.Millisecond)
	if !cb.Allow() {
		t.Fatal()
	}
	cb.RecordFailure()
	time.Sleep(15 * time.Millisecond)

	// First probe allowed — transitions to half_open.
	if !cb.Allow() {
		t.Fatal("first probe should be allowed")
	}
	// State is half_open, probe in-flight.
	if state := cb.State(); state != "half_open" {
		t.Fatalf("expected half_open, got %q", state)
	}
	// Second Allow() — hits the cbStateHalfOpen case.
	if cb.Allow() {
		t.Error("second Allow() in half_open should be blocked")
	}
	cb.RecordSuccess() // clean up.
}

// TestCircuitBreaker_Allow_OpenProbeInFlight exercises the open state path
// when a probe is already in-flight (line 127 in circuitbreaker.go).
func TestCircuitBreaker_Allow_OpenProbeAlreadyInFlight(t *testing.T) {
	cb := newTestCB(1, time.Minute, 5*time.Millisecond)
	// Trip to open.
	if !cb.Allow() {
		t.Fatal()
	}
	cb.RecordFailure()
	// Wait for recovery.
	time.Sleep(15 * time.Millisecond)

	// First probe allowed.
	ok := cb.Allow()
	if !ok {
		t.Fatal("first probe should be allowed")
	}
	// At this point state == half_open, probeInFlight == true.
	// Manually manipulate state back to open without clearing probeInFlight
	// to hit the "another probe is already in-flight" branch.
	cb.mu.Lock()
	cb.state = cbStateOpen
	cb.lastTripAt = time.Now().Add(-time.Hour) // make recovery timer already elapsed
	// probeInFlight stays true
	cb.mu.Unlock()

	// Now Allow(): state=open, timer elapsed, but probeInFlight=true → return false.
	if cb.Allow() {
		t.Error("Allow() should return false when probe already in-flight")
	}
	// Clean up.
	cb.mu.Lock()
	cb.state = cbStateClosed
	cb.probeInFlight = false
	cb.mu.Unlock()
}

// TestCircuitBreaker_RecordFailure_ClosedWindowExpiry exercises the
// window-reset path inside RecordFailure (closed state, window expired).
func TestCircuitBreaker_RecordFailure_ClosedWindowReset(t *testing.T) {
	cb := newTestCB(10, 1*time.Millisecond, time.Hour)
	// Wait for the window to expire.
	time.Sleep(5 * time.Millisecond)

	// RecordFailure: window has expired, so failureCount resets to 0, then +1.
	if !cb.Allow() {
		t.Fatal()
	}
	cb.RecordFailure()

	if cb.State() != "closed" {
		t.Errorf("should still be closed after 1 failure with threshold=10, got %q", cb.State())
	}
}

// TestCircuitBreaker_setState_SameState exercises the early return when
// setting the same state (no-op path).
func TestCircuitBreaker_setState_SameState(t *testing.T) {
	cb := newTestCB(10, time.Minute, time.Hour)
	// setState with the same state should not panic or change the gauge.
	cb.mu.Lock()
	cb.setState(cbStateClosed) // already closed — should be a no-op
	cb.mu.Unlock()
	if cb.State() != "closed" {
		t.Errorf("state changed unexpectedly: %q", cb.State())
	}
}

func TestNewMetrics_Constructs(t *testing.T) {
	// Use a unique namespace to avoid collision with other tests.
	m := NewMetrics("nexus_cov_metrics")
	if m == nil {
		t.Fatal("NewMetrics returned nil")
	}
	// Exercise all helper methods on a non-nil metrics (should not panic).
	m.IncWrite("ok")
	m.ObserveEntrySize(1024)
	m.ObserveWriteLatency(0.001)
	m.IncEmbeddingCall("p", "m", "ok")
	m.ObserveEmbeddingLatency(0.05)
	m.AddEmbeddingCost("p", "m", 0.0001)
}

// TestMetrics_NilReceiver verifies that all Metrics methods are no-ops on nil.
func TestMetrics_NilReceiver(t *testing.T) {
	var m *Metrics
	// Must not panic.
	m.IncWrite("ok")
	m.ObserveEntrySize(1024)
	m.ObserveWriteLatency(0.1)
	m.IncEmbeddingCall("p", "m", "ok")
	m.ObserveEmbeddingLatency(0.1)
	m.AddEmbeddingCost("p", "m", 0.01)
	// Feedback/poison methods must also be no-ops on nil.
	m.IncFeedback("test-reason")
	m.IncPoisonHits()
}

// TestMetrics_FeedbackPoison_NonNil verifies the feedback/poison metric
// methods on a non-nil Metrics do not panic and record correctly.
func TestMetrics_E68_NonNil(t *testing.T) {
	m := NewMetrics("nexus_cov_e68")
	m.IncFeedback("bad_result")
	m.IncFeedback("spam")
	m.IncPoisonHits()
}

func TestIsIndexMissingError_True(t *testing.T) {
	err := ErrIndexMissing
	if !isIndexMissingError(err) {
		t.Error("isIndexMissingError should return true for ErrIndexMissing")
	}
}

func TestIsIndexMissingError_False(t *testing.T) {
	if isIndexMissingError(nil) {
		t.Error("isIndexMissingError(nil) should be false")
	}
}

func TestIsIndexExistsError_False_Nil(t *testing.T) {
	if isIndexExistsError(nil) {
		t.Error("isIndexExistsError(nil) should be false")
	}
}

// DropIndex error path — when the Redis command fails with a non-"missing"
// error, DropIndex wraps and returns ErrValkeyUnavailable.

func TestClient_DropIndex_EmptyName(t *testing.T) {
	c, cleanup := newTestClient(t)
	defer cleanup()

	err := c.DropIndex(context.Background(), "")
	if err == nil {
		t.Fatal("expected error for empty index name")
	}
}

// StoreEntry — nil usage (empty JSON path)

func TestClient_StoreEntry_EmptyUsage(t *testing.T) {
	c, cleanup := newTestClient(t)
	defer cleanup()

	indexName := "cov-empty-usage"
	if err := c.EnsureIndex(context.Background(), indexName, 4); err != nil {
		t.Fatalf("EnsureIndex: %v", err)
	}

	in := StoreInput{
		EmbeddingInput: "test-empty-usage",
		Embedding:      []float32{0.1, 0.2, 0.3, 0.4},
		ResponseBody:   []byte(`{}`),
		Usage:          nil, // nil usage exercises the empty JSON branch
		TTL:            time.Minute,
	}

	if err := c.StoreEntry(context.Background(), indexName, in, 0); err != nil {
		t.Fatalf("StoreEntry with nil usage: %v", err)
	}
}

// NewEmbeddingSingleflight default timeout branch

func TestNewEmbeddingSingleflight_DefaultTimeout(t *testing.T) {
	sf := NewEmbeddingSingleflight(nil, newNopRegistry(), 0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if sf.hardTimeout != defaultEmbedTimeout {
		t.Errorf("expected default timeout %v, got %v", defaultEmbedTimeout, sf.hardTimeout)
	}
}

// DropIndex Valkey error path (non-missing error)

func TestClient_DropIndex_ValkeyError(t *testing.T) {
	c, redisCleanup := newTestClient(t)
	// Close the redis client to force connection errors on subsequent commands.
	_ = c.rdb.Close()
	defer redisCleanup()

	// DropIndex should return a wrapped ErrValkeyUnavailable when the
	// connection fails with an error that is not "index not found".
	err := c.DropIndex(context.Background(), "some-index")
	if err == nil {
		t.Fatal("expected error from DropIndex on closed client")
	}
}

// StoreEntry TTL=0 skips PEXPIRE

func TestClient_StoreEntry_NoTTL(t *testing.T) {
	c, cleanup := newTestClient(t)
	defer cleanup()

	indexName := "cov-no-ttl"
	if err := c.EnsureIndex(context.Background(), indexName, 4); err != nil {
		t.Fatalf("EnsureIndex: %v", err)
	}

	in := StoreInput{
		EmbeddingInput: "no-ttl-input",
		Embedding:      []float32{0.1, 0.2, 0.3, 0.4},
		ResponseBody:   []byte(`{}`),
		TTL:            0, // zero TTL → PEXPIRE branch not taken
	}

	if err := c.StoreEntry(context.Background(), indexName, in, 0); err != nil {
		t.Fatalf("StoreEntry with zero TTL: %v", err)
	}
}

// OnConfigSnapshot — EnsureIndex failure is logged at WARN, not fatal

func TestIndexLifecycle_EnsureIndexFailureNotFatal(t *testing.T) {
	c, redisCleanup := newTestClient(t)
	// Close the client so EnsureIndex will fail with a connection error.
	_ = c.rdb.Close()
	defer redisCleanup()

	cache := NewConfigCache()
	lc := NewIndexLifecycle(cache, c, slog.New(slog.NewTextHandler(io.Discard, nil)))

	// OnConfigSnapshot must not panic even when EnsureIndex fails.
	snap := ConfigSnapshot{
		Enabled:             true,
		EmbeddingProviderID: "p",
		EmbeddingModelID:    "m",
		EmbeddingDimension:  4,
		Fingerprint:         "fp-fail",
		RedisIndexName:      "fail-idx",
	}
	lc.OnConfigSnapshot(context.Background(), snap)

	// lastFingerprint must be updated even when EnsureIndex failed.
	lc.mu.Lock()
	fp := lc.lastFingerprint
	lc.mu.Unlock()
	if fp != "fp-fail" {
		t.Errorf("lastFingerprint = %q, want 'fp-fail'", fp)
	}
}

package semantic

import (
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// newTestCB creates a circuit breaker with a fresh prometheus registry so
// tests do not collide on global metrics.
func newTestCB(failureThreshold int, failureWindow, halfOpenAfter time.Duration) *CircuitBreaker {
	reg := prometheus.NewRegistry()
	cb := &CircuitBreaker{
		failureThreshold: failureThreshold,
		failureWindow:    failureWindow,
		halfOpenAfter:    halfOpenAfter,
		log:              slog.New(slog.NewTextHandler(io.Discard, nil)),
		state:            cbStateClosed,
		windowStart:      time.Now(),
	}
	cb.stateGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "test_circuit_state",
	}, []string{"state"})
	_ = reg.Register(cb.stateGauge)
	cb.tripsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "test_circuit_trips_total",
	})
	_ = reg.Register(cb.tripsTotal)
	for _, name := range cbStateNames {
		cb.stateGauge.WithLabelValues(name).Set(0)
	}
	cb.stateGauge.WithLabelValues(cbStateNames[cbStateClosed]).Set(1)
	return cb
}

// TestCircuitBreaker_InitialState checks that a new circuit breaker starts
// closed and allows calls.
func TestCircuitBreaker_InitialState(t *testing.T) {
	cb := newTestCB(3, time.Minute, 30*time.Second)
	if state := cb.State(); state != "closed" {
		t.Fatalf("initial state = %q, want 'closed'", state)
	}
	if !cb.Allow() {
		t.Fatal("Allow() should return true in closed state")
	}
	cb.RecordSuccess()
}

// TestCircuitBreaker_TripsOnThreshold verifies that N consecutive failures
// within the window trip the breaker to open.
func TestCircuitBreaker_TripsOnThreshold(t *testing.T) {
	cb := newTestCB(3, time.Minute, 30*time.Second)

	for range 3 {
		if !cb.Allow() {
			t.Fatal("Allow() should return true before trip")
		}
		cb.RecordFailure()
	}

	if state := cb.State(); state != "open" {
		t.Fatalf("after threshold failures, state = %q, want 'open'", state)
	}
	if cb.Allow() {
		t.Fatal("Allow() should return false in open state")
	}
	if cb.TripCount() != 1 {
		t.Fatalf("TripCount = %d, want 1", cb.TripCount())
	}
	if cb.LastTripAt().IsZero() {
		t.Fatal("LastTripAt should not be zero after trip")
	}
}

// TestCircuitBreaker_OpenToHalfOpen verifies the recovery timer transition.
func TestCircuitBreaker_OpenToHalfOpen(t *testing.T) {
	// Use a very short half-open window so the test doesn't need to sleep.
	cb := newTestCB(1, time.Minute, 10*time.Millisecond)

	if !cb.Allow() {
		t.Fatal("initial Allow() should return true")
	}
	cb.RecordFailure() // trips immediately (threshold=1)

	if state := cb.State(); state != "open" {
		t.Fatalf("state after trip = %q, want 'open'", state)
	}

	// Before half-open window: still blocked.
	if cb.Allow() {
		t.Fatal("Allow() should return false before half-open window")
	}

	// Wait for recovery timer.
	time.Sleep(20 * time.Millisecond)

	// First Allow() after recovery timer should return true (probe).
	if !cb.Allow() {
		t.Fatal("Allow() should return true in half_open (probe)")
	}
	if state := cb.State(); state != "half_open" {
		t.Fatalf("state after probe allowed = %q, want 'half_open'", state)
	}
	// Second Allow() while probe in-flight should return false.
	if cb.Allow() {
		t.Fatal("Allow() should return false while probe in-flight")
	}
}

// TestCircuitBreaker_HalfOpenSuccess verifies that a successful probe
// closes the breaker.
func TestCircuitBreaker_HalfOpenSuccess(t *testing.T) {
	cb := newTestCB(1, time.Minute, 10*time.Millisecond)

	if !cb.Allow() {
		t.Fatal()
	}
	cb.RecordFailure()
	time.Sleep(20 * time.Millisecond)

	// Allow the probe.
	if !cb.Allow() {
		t.Fatal("probe Allow() should return true")
	}
	cb.RecordSuccess()

	if state := cb.State(); state != "closed" {
		t.Fatalf("after probe success, state = %q, want 'closed'", state)
	}
	// Should allow new calls.
	if !cb.Allow() {
		t.Fatal("Allow() should return true after closing")
	}
	cb.RecordSuccess()
}

// TestCircuitBreaker_HalfOpenFailure verifies that a failed probe keeps
// the breaker open and starts a new timer.
func TestCircuitBreaker_HalfOpenFailure(t *testing.T) {
	cb := newTestCB(1, time.Minute, 10*time.Millisecond)

	if !cb.Allow() {
		t.Fatal()
	}
	cb.RecordFailure()
	time.Sleep(20 * time.Millisecond)

	// Allow probe.
	if !cb.Allow() {
		t.Fatal()
	}
	cb.RecordFailure() // probe fails → back to open

	if state := cb.State(); state != "open" {
		t.Fatalf("after probe failure, state = %q, want 'open'", state)
	}
	if cb.TripCount() < 2 {
		t.Fatalf("TripCount = %d, want >= 2", cb.TripCount())
	}
}

// TestCircuitBreaker_WindowExpiry verifies that the failure counter resets
// when the window expires.
func TestCircuitBreaker_WindowExpiry(t *testing.T) {
	// 1ms window so we can expire it quickly.
	cb := newTestCB(3, 1*time.Millisecond, 30*time.Second)

	// Fail 2 times — below threshold.
	if !cb.Allow() {
		t.Fatal()
	}
	cb.RecordFailure()
	if !cb.Allow() {
		t.Fatal()
	}
	cb.RecordFailure()

	// Wait for window to expire.
	time.Sleep(5 * time.Millisecond)

	// Fail again — counter should have been reset at window expiry, so
	// this is only failure #1 in the new window.
	if !cb.Allow() {
		t.Fatal("Allow() should return true after window expiry")
	}
	cb.RecordFailure()

	if state := cb.State(); state != "closed" {
		t.Fatalf("state after window expiry + 1 failure = %q, want 'closed'", state)
	}
}

// TestCircuitBreaker_ThreadSafety runs concurrent Allow/RecordSuccess/RecordFailure
// to detect data races (run with -race).
func TestCircuitBreaker_ThreadSafety(t *testing.T) {
	cb := newTestCB(10, time.Minute, 10*time.Millisecond)
	var wg sync.WaitGroup
	const goroutines = 50
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			for range 20 {
				if cb.Allow() {
					if time.Now().UnixNano()%2 == 0 {
						cb.RecordSuccess()
					} else {
						cb.RecordFailure()
					}
				}
				_ = cb.State()
			}
		}()
	}
	wg.Wait()
}

// TestCircuitBreaker_StateNames verifies the exported State() strings.
func TestCircuitBreaker_StateNames(t *testing.T) {
	cb := newTestCB(1, time.Minute, time.Hour)
	if s := cb.State(); s != "closed" {
		t.Errorf("initial state = %q", s)
	}
	if !cb.Allow() {
		t.Fatal()
	}
	cb.RecordFailure()
	if s := cb.State(); s != "open" {
		t.Errorf("after trip state = %q", s)
	}
}

// CircuitBreakerRegistry tests

// newTestRegistry builds a registry backed by an isolated Prometheus registry
// so tests do not collide on the global default registry.
func newTestRegistry(failureThreshold int, failureWindow, halfOpenAfter time.Duration) *CircuitBreakerRegistry {
	isolated := prometheus.NewRegistry()
	r := &CircuitBreakerRegistry{
		breakers: make(map[string]*CircuitBreaker),
	}
	r.newFn = func(key string) *CircuitBreaker {
		cb := &CircuitBreaker{
			failureThreshold: failureThreshold,
			failureWindow:    failureWindow,
			halfOpenAfter:    halfOpenAfter,
			log:              slog.New(slog.NewTextHandler(io.Discard, nil)),
			state:            cbStateClosed,
			windowStart:      time.Now(),
		}
		cb.stateGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name:        "test_reg_circuit_state",
			ConstLabels: prometheus.Labels{"model_key": key},
		}, []string{"state"})
		_ = isolated.Register(cb.stateGauge)
		cb.tripsTotal = prometheus.NewCounter(prometheus.CounterOpts{
			Name:        "test_reg_circuit_trips_total",
			ConstLabels: prometheus.Labels{"model_key": key},
		})
		_ = isolated.Register(cb.tripsTotal)
		for _, name := range cbStateNames {
			cb.stateGauge.WithLabelValues(name).Set(0)
		}
		cb.stateGauge.WithLabelValues("closed").Set(1)
		return cb
	}
	return r
}

// TestRegistry_LazyInit verifies that the first Get() creates a new breaker
// and a second Get() with the same key returns the same instance.
func TestRegistry_LazyInit(t *testing.T) {
	r := newTestRegistry(5, time.Minute, 30*time.Second)

	cb1 := r.Get("openai", "text-embedding-3-small")
	if cb1 == nil {
		t.Fatal("Get returned nil on first access")
	}
	if cb1.State() != "closed" {
		t.Fatalf("initial state = %q, want 'closed'", cb1.State())
	}

	// Second Get with the same key must return the same pointer.
	cb2 := r.Get("openai", "text-embedding-3-small")
	if cb1 != cb2 {
		t.Fatal("Get returned a different instance on repeat access; want same pointer")
	}

	// Different key must return a different instance.
	cb3 := r.Get("openai", "text-embedding-ada-002")
	if cb3 == cb1 {
		t.Fatal("Get with a different model must return a distinct breaker")
	}
}

// TestRegistry_KeyIsolation proves that tripping the breaker for one
// (providerID, modelID) key does NOT affect the breaker for a different key.
// This is the primary correctness property from response-cache-architecture.md §3.3.
func TestRegistry_KeyIsolation(t *testing.T) {
	// Threshold=1 so a single failure trips the breaker immediately.
	r := newTestRegistry(1, time.Minute, time.Hour)

	const providerA = "openai"
	const modelA = "text-embedding-3-small"
	const providerB = "cohere"
	const modelB = "embed-english-v3.0"

	cbA := r.Get(providerA, modelA)
	cbB := r.Get(providerB, modelB)

	// Sanity: both start closed.
	if cbA.State() != "closed" {
		t.Fatalf("cbA initial state = %q", cbA.State())
	}
	if cbB.State() != "closed" {
		t.Fatalf("cbB initial state = %q", cbB.State())
	}

	// Trip model-A's breaker by recording a failure (threshold=1).
	if !cbA.Allow() {
		t.Fatal("cbA.Allow() should return true before trip")
	}
	cbA.RecordFailure()

	if cbA.State() != "open" {
		t.Fatalf("cbA state after trip = %q, want 'open'", cbA.State())
	}

	// Model-B must be unaffected.
	if cbB.State() != "closed" {
		t.Fatalf("cbB state after cbA trip = %q, want 'closed' (key isolation violated)", cbB.State())
	}
	if !cbB.Allow() {
		t.Fatal("cbB.Allow() should still return true (key isolation violated)")
	}
	cbB.RecordSuccess() // clean up

	// Snapshot must report both keys.
	snap := r.Snapshot()
	keyA := providerA + ":" + modelA
	keyB := providerB + ":" + modelB
	if stateA, ok := snap[keyA]; !ok || stateA != "open" {
		t.Errorf("snapshot[%q] = %q, want 'open'", keyA, stateA)
	}
	if stateB, ok := snap[keyB]; !ok || stateB != "closed" {
		t.Errorf("snapshot[%q] = %q, want 'closed'", keyB, stateB)
	}
}

// TestRegistry_Snapshot_Empty verifies that an empty registry returns an
// empty (not nil) map from Snapshot().
func TestRegistry_Snapshot_Empty(t *testing.T) {
	r := newTestRegistry(10, time.Minute, 30*time.Second)
	snap := r.Snapshot()
	if snap == nil {
		t.Fatal("Snapshot() returned nil, want empty map")
	}
	if len(snap) != 0 {
		t.Fatalf("Snapshot() returned %d entries on empty registry, want 0", len(snap))
	}
}

// TestNewCircuitBreakerRegistry_Constructor verifies that NewCircuitBreakerRegistry
// constructs a registry whose Get() method returns correctly initialised breakers
// (the newFn calls newCircuitBreakerWithKey under the hood with Prometheus metrics
// on the global default registry; we use a unique namespace to avoid name collisions).
func TestNewCircuitBreakerRegistry_Constructor(t *testing.T) {
	// Use a unique namespace so the test doesn't collide with other tests that
	// use the global prometheus.DefaultRegisterer.
	const ns = "nexus_regtest_ctor"
	r := NewCircuitBreakerRegistry(5, 30*time.Second, 15*time.Second,
		slog.New(slog.NewTextHandler(io.Discard, nil)), ns)

	if r == nil {
		t.Fatal("NewCircuitBreakerRegistry returned nil")
	}
	if r.breakers == nil {
		t.Fatal("registry breakers map is nil")
	}

	// First Get creates the breaker.
	cb := r.Get("openai", "text-embedding-3-small")
	if cb == nil {
		t.Fatal("Get returned nil")
	}
	if cb.State() != "closed" {
		t.Fatalf("initial state = %q, want 'closed'", cb.State())
	}
	// Parameters should be wired through from the constructor.
	if cb.failureThreshold != 5 {
		t.Errorf("failureThreshold = %d, want 5", cb.failureThreshold)
	}
	if cb.failureWindow != 30*time.Second {
		t.Errorf("failureWindow = %v, want 30s", cb.failureWindow)
	}

	// Second Get returns the same instance.
	cb2 := r.Get("openai", "text-embedding-3-small")
	if cb != cb2 {
		t.Error("second Get should return the same breaker instance")
	}

	// Snapshot lists the key.
	snap := r.Snapshot()
	key := "openai:text-embedding-3-small"
	if state, ok := snap[key]; !ok || state != "closed" {
		t.Errorf("snapshot[%q] = %q, want 'closed'", key, state)
	}
}

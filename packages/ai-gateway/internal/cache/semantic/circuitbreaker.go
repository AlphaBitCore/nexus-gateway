package semantic

import (
	"log/slog"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// cbState is the internal circuit-breaker state.
type cbState int

const (
	cbStateClosed   cbState = iota // normal operation
	cbStateOpen                    // failures tripped the breaker; skip all calls
	cbStateHalfOpen                // probe allowed; one call in-flight
)

// cbStateNames maps state → Prometheus label string.
var cbStateNames = map[cbState]string{
	cbStateClosed:   "closed",
	cbStateOpen:     "open",
	cbStateHalfOpen: "half_open",
}

// CircuitBreaker protects the embedding call path against a persistent
// provider outage. When the breaker is open every L2-eligible request
// stamps GatewayCacheSkipReasonEmbeddingCircuitOpen and falls through to
// the broker without firing an embedding HTTP call.
//
// State machine:
//
//	closed → (10 consecutive failures within 60s) → open
//	open   → (30s elapsed)                        → half_open
//	half_open → (next Allow returned true + RecordSuccess) → closed
//	half_open → (next Allow returned true + RecordFailure) → open (new 30s timer)
//
// Thresholds are tunable via the constructor. Defaults match
// response-cache-architecture.md §3.3 (10 failures / 60s window / 30s recovery).
type CircuitBreaker struct {
	mu sync.Mutex

	failureThreshold int
	failureWindow    time.Duration
	halfOpenAfter    time.Duration

	state         cbState
	failureCount  int
	windowStart   time.Time
	lastTripAt    time.Time
	tripCount     int
	probeInFlight bool // half_open: true when a probe call is in-flight

	log *slog.Logger

	// Prometheus gauge: one series per state, value=1 for the active state.
	stateGauge *prometheus.GaugeVec
	tripsTotal prometheus.Counter
}

// NewCircuitBreaker constructs a circuit breaker with tunable thresholds.
// namespace is the Prometheus namespace string (e.g. "nexus").
func NewCircuitBreaker(
	failureThreshold int,
	failureWindow time.Duration,
	halfOpenAfter time.Duration,
	log *slog.Logger,
	namespace string,
) *CircuitBreaker {
	cb := &CircuitBreaker{
		failureThreshold: failureThreshold,
		failureWindow:    failureWindow,
		halfOpenAfter:    halfOpenAfter,
		log:              log,
		state:            cbStateClosed,
		windowStart:      time.Now(),
	}
	cb.stateGauge = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "cache_semantic_circuit_state",
		Help:      "Semantic cache embedding circuit-breaker state (1 = active state). Labels: state ∈ {closed, open, half_open}.",
	}, []string{"state"})
	cb.tripsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "cache_embedding_circuit_trips_total",
		Help:      "Total number of times the embedding circuit breaker tripped to open.",
	})
	// Initialise all state gauges to 0 so every series appears in /metrics.
	for _, name := range cbStateNames {
		cb.stateGauge.WithLabelValues(name).Set(0)
	}
	cb.stateGauge.WithLabelValues(cbStateNames[cbStateClosed]).Set(1)
	return cb
}

// Allow reports whether the caller may proceed with an embedding call.
// It also handles the open→half_open transition when the recovery timer
// has elapsed.
//
// Callers MUST pair every Allow()==true call with exactly one
// RecordSuccess or RecordFailure.
func (cb *CircuitBreaker) Allow() bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	switch cb.state {
	case cbStateClosed:
		// Reset window if it has expired.
		if time.Since(cb.windowStart) > cb.failureWindow {
			cb.failureCount = 0
			cb.windowStart = time.Now()
		}
		return true

	case cbStateOpen:
		if time.Since(cb.lastTripAt) < cb.halfOpenAfter {
			return false
		}
		// Recovery timer elapsed — transition to half_open and allow one probe.
		if !cb.probeInFlight {
			cb.setState(cbStateHalfOpen)
			cb.probeInFlight = true
			return true
		}
		return false // another probe is already in-flight

	case cbStateHalfOpen:
		// Probe is in-flight; block all other callers.
		return false

	default:
		return false
	}
}

// RecordSuccess records a successful embedding call. When in half_open,
// it transitions the breaker back to closed. In closed state it resets the
// consecutive failure counter.
func (cb *CircuitBreaker) RecordSuccess() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.failureCount = 0
	cb.windowStart = time.Now()
	cb.probeInFlight = false
	if cb.state != cbStateClosed {
		cb.setState(cbStateClosed)
		cb.log.Info("cache/semantic: circuit breaker closed after successful probe")
	}
}

// RecordFailure records a failed embedding call. In closed state it
// increments the consecutive failure counter and trips when the threshold
// is reached. In half_open state it immediately trips back to open.
func (cb *CircuitBreaker) RecordFailure() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.probeInFlight = false

	switch cb.state {
	case cbStateClosed:
		// Reset window on expiry.
		if time.Since(cb.windowStart) > cb.failureWindow {
			cb.failureCount = 0
			cb.windowStart = time.Now()
		}
		cb.failureCount++
		if cb.failureCount >= cb.failureThreshold {
			cb.trip()
		}

	case cbStateHalfOpen:
		// Probe failed — stay open for another halfOpenAfter period.
		cb.trip()
	}
}

// State returns a human-readable state string: "closed" | "open" | "half_open".
func (cb *CircuitBreaker) State() string {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	return cbStateNames[cb.state]
}

// TripCount returns the total number of times the breaker has tripped.
func (cb *CircuitBreaker) TripCount() int {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	return cb.tripCount
}

// LastTripAt returns the time of the most recent trip. Returns the zero
// time if the breaker has never tripped.
func (cb *CircuitBreaker) LastTripAt() time.Time {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	return cb.lastTripAt
}

// trip transitions the breaker to open. Must be called with cb.mu held.
func (cb *CircuitBreaker) trip() {
	cb.lastTripAt = time.Now()
	cb.tripCount++
	cb.failureCount = 0
	cb.tripsTotal.Inc()
	cb.setState(cbStateOpen)
	cb.log.Warn("cache/semantic: circuit breaker tripped to open",
		"tripCount", cb.tripCount,
		"failureThreshold", cb.failureThreshold,
	)
}

// setState sets cb.state and updates the Prometheus gauge. Must be called
// with cb.mu held.
func (cb *CircuitBreaker) setState(s cbState) {
	if cb.state == s {
		return
	}
	// Zero out old state gauge.
	if name, ok := cbStateNames[cb.state]; ok {
		cb.stateGauge.WithLabelValues(name).Set(0)
	}
	cb.state = s
	// Activate new state gauge.
	if name, ok := cbStateNames[cb.state]; ok {
		cb.stateGauge.WithLabelValues(name).Set(1)
	}
}

// CircuitBreakerRegistry — per-(providerID, modelID) keyed breaker map

// CircuitBreakerRegistry holds one CircuitBreaker per (providerID, modelID)
// pair, keyed as "providerID:modelID". This ensures that a persistent failure
// on one embedding model does not open the breaker for other models/providers
// (per response-cache-architecture.md §3.3: "per-(provider, model) scope").
type CircuitBreakerRegistry struct {
	mu       sync.Mutex
	breakers map[string]*CircuitBreaker
	newFn    func(key string) *CircuitBreaker // constructor closure (params baked in)
}

// NewCircuitBreakerRegistry constructs a registry. All breakers created by
// the registry share the same threshold/window/halfOpenAfter parameters.
// Each breaker gets a distinct Prometheus label from the key
// ("providerID:modelID") so per-model trip counts and state are observable.
func NewCircuitBreakerRegistry(
	failureThreshold int,
	failureWindow time.Duration,
	halfOpenAfter time.Duration,
	log *slog.Logger,
	namespace string,
) *CircuitBreakerRegistry {
	r := &CircuitBreakerRegistry{
		breakers: make(map[string]*CircuitBreaker),
	}
	r.newFn = func(key string) *CircuitBreaker {
		return newCircuitBreakerWithKey(failureThreshold, failureWindow, halfOpenAfter, log, namespace, key)
	}
	return r
}

// Get returns the CircuitBreaker for the given (providerID, modelID) pair,
// creating it on first access.
func (r *CircuitBreakerRegistry) Get(providerID, modelID string) *CircuitBreaker {
	key := providerID + ":" + modelID
	r.mu.Lock()
	defer r.mu.Unlock()
	if cb, ok := r.breakers[key]; ok {
		return cb
	}
	cb := r.newFn(key)
	r.breakers[key] = cb
	return cb
}

// Snapshot returns a map of key → state string for status panels.
func (r *CircuitBreakerRegistry) Snapshot() map[string]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]string, len(r.breakers))
	for k, cb := range r.breakers {
		out[k] = cb.State()
	}
	return out
}

// newCircuitBreakerWithKey is like NewCircuitBreaker but tags Prometheus metrics
// with a "model_key" label so multiple breakers in the same process can coexist
// on the default registry without name collision.
func newCircuitBreakerWithKey(
	failureThreshold int,
	failureWindow time.Duration,
	halfOpenAfter time.Duration,
	log *slog.Logger,
	namespace string,
	key string,
) *CircuitBreaker {
	cb := &CircuitBreaker{
		failureThreshold: failureThreshold,
		failureWindow:    failureWindow,
		halfOpenAfter:    halfOpenAfter,
		log:              log,
		state:            cbStateClosed,
		windowStart:      time.Now(),
	}
	cb.stateGauge = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace:   namespace,
		Name:        "cache_semantic_circuit_state_per_model",
		Help:        "Semantic cache embedding circuit-breaker state per (provider, model). Labels: model_key, state.",
		ConstLabels: prometheus.Labels{"model_key": key},
	}, []string{"state"})
	cb.tripsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace:   namespace,
		Name:        "cache_embedding_circuit_trips_per_model_total",
		Help:        "Total circuit-breaker trips per (provider, model).",
		ConstLabels: prometheus.Labels{"model_key": key},
	})
	for _, name := range cbStateNames {
		cb.stateGauge.WithLabelValues(name).Set(0)
	}
	cb.stateGauge.WithLabelValues(cbStateNames[cbStateClosed]).Set(1)
	return cb
}

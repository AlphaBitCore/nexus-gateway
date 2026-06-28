package semantic

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/embeddings"
	"github.com/prometheus/client_golang/prometheus"
)

// newNopRegistry returns a CircuitBreakerRegistry whose breakers always allow
// (very high failure threshold). Uses an isolated prometheus registerer so
// test runs don't collide on the global default registry.
func newNopRegistry() *CircuitBreakerRegistry {
	isolatedReg := prometheus.NewRegistry()
	r := &CircuitBreakerRegistry{
		breakers: make(map[string]*CircuitBreaker),
	}
	r.newFn = func(key string) *CircuitBreaker {
		cb := &CircuitBreaker{
			failureThreshold: 1000,
			failureWindow:    time.Hour,
			halfOpenAfter:    time.Hour,
			log:              slog.New(slog.NewTextHandler(io.Discard, nil)),
			state:            cbStateClosed,
			windowStart:      time.Now(),
		}
		// Use isolated registry so tests don't fight over the global Prometheus default.
		cb.stateGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name:        "nop_registry_state",
			ConstLabels: prometheus.Labels{"model_key": key},
		}, []string{"state"})
		_ = isolatedReg.Register(cb.stateGauge)
		cb.tripsTotal = prometheus.NewCounter(prometheus.CounterOpts{
			Name:        "nop_registry_trips_total",
			ConstLabels: prometheus.Labels{"model_key": key},
		})
		_ = isolatedReg.Register(cb.tripsTotal)
		for _, name := range cbStateNames {
			cb.stateGauge.WithLabelValues(name).Set(0)
		}
		cb.stateGauge.WithLabelValues("closed").Set(1)
		return cb
	}
	return r
}

// buildTestEmbeddingServer creates an httptest server that:
//   - returns a valid float32 embedding of dimension dim.
//   - increments callCount on each request.
//   - waits for delay before responding (for timeout tests).
func buildTestEmbeddingServer(t *testing.T, callCount *atomic.Int64, delay time.Duration, dim int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount.Add(1)
		if delay > 0 {
			time.Sleep(delay)
		}
		// Build a float32 vector of size dim, all zeros.
		vecPart := ""
		for i := range dim {
			if i > 0 {
				vecPart += ","
			}
			vecPart += "0.0"
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"object":"embedding","index":0,"embedding":[` + vecPart + `]}],"model":"text-embedding-3-small","usage":{"prompt_tokens":5,"total_tokens":5}}`))
	}))
}

// TestSingleflight_100ConcurrentCallsCoalesce verifies that 100 concurrent
// Embed calls with the same input result in exactly 1 upstream HTTP call.
func TestSingleflight_100ConcurrentCallsCoalesce(t *testing.T) {
	var callCount atomic.Int64
	srv := buildTestEmbeddingServer(t, &callCount, 20*time.Millisecond, 4)
	defer srv.Close()

	client := embeddings.NewClient(
		&http.Client{Timeout: 5 * time.Second},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		"nexus_test",
	)
	reg := newNopRegistry()
	sf := NewEmbeddingSingleflight(client, reg, 5*time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))

	const concurrency = 100
	req := embeddings.Request{Model: "text-embedding-3-small", Input: "hello world", Dimensions: 4}

	var wg sync.WaitGroup
	results := make([]embeddings.Response, concurrency)
	errs := make([]error, concurrency)

	wg.Add(concurrency)
	for i := range concurrency {
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = sf.Embed(context.Background(), "openai", srv.URL, "text-embedding-3-small", "", req)
		}(i)
	}
	wg.Wait()

	// Verify no errors.
	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: unexpected error: %v", i, err)
		}
	}

	// The key assertion: exactly 1 upstream call regardless of concurrency.
	if n := callCount.Load(); n != 1 {
		t.Errorf("upstream call count = %d, want 1 (singleflight coalescing broken)", n)
	}
}

// TestSingleflight_HardTimeoutEnforced verifies that when the embedding server
// takes longer than hardTimeout, callers receive an error and the leader is
// aborted at the deadline.
func TestSingleflight_HardTimeoutEnforced(t *testing.T) {
	var callCount atomic.Int64
	// Server delays 200ms; hard timeout is 50ms.
	srv := buildTestEmbeddingServer(t, &callCount, 200*time.Millisecond, 4)
	defer srv.Close()

	client := embeddings.NewClient(
		&http.Client{Timeout: 5 * time.Second},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		"nexus_test2",
	)
	reg := newNopRegistry()
	sf := NewEmbeddingSingleflight(client, reg, 50*time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil)))

	req := embeddings.Request{Model: "text-embedding-3-small", Input: "slow", Dimensions: 4}

	_, err := sf.Embed(context.Background(), "openai", srv.URL, "text-embedding-3-small", "", req)
	if err == nil {
		t.Fatal("expected error from hard timeout, got nil")
	}
}

// TestSingleflight_JoinerCancelDoesNotKillLeader verifies that when a joiner
// cancels its context, the leader's work continues and other joiners still
// get the result.
func TestSingleflight_JoinerCancelDoesNotKillLeader(t *testing.T) {
	var callCount atomic.Int64
	// Server delays 80ms — joiner will cancel after 10ms, but leader still
	// finishes.
	srv := buildTestEmbeddingServer(t, &callCount, 80*time.Millisecond, 4)
	defer srv.Close()

	client := embeddings.NewClient(
		&http.Client{Timeout: 5 * time.Second},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		"nexus_test3",
	)
	reg := newNopRegistry()
	sf := NewEmbeddingSingleflight(client, reg, 2*time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))

	req := embeddings.Request{Model: "text-embedding-3-small", Input: "patient", Dimensions: 4}

	// Joiner with a short context.
	joinerCtx, joinerCancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer joinerCancel()

	// Leader (background, long context) — start it first so it claims the slot.
	leaderDone := make(chan struct{})
	var leaderErr error
	go func() {
		defer close(leaderDone)
		_, leaderErr = sf.Embed(context.Background(), "openai", srv.URL, "text-embedding-3-small", "", req)
	}()

	// Give the leader a moment to register.
	time.Sleep(5 * time.Millisecond)

	// Joiner cancels early.
	_, joinerErrGot := sf.Embed(joinerCtx, "openai", srv.URL, "text-embedding-3-small", "", req)
	// The joiner should get a context error.
	if joinerErrGot == nil || !errors.Is(joinerErrGot, context.DeadlineExceeded) {
		t.Logf("joiner error (expected context.DeadlineExceeded): %v", joinerErrGot)
	}

	// Wait for leader to finish.
	<-leaderDone

	// Leader should succeed.
	if leaderErr != nil {
		t.Errorf("leader error: %v", leaderErr)
	}
	// Exactly 1 upstream call.
	if n := callCount.Load(); n != 1 {
		t.Errorf("upstream call count = %d, want 1", n)
	}
}

// TestSingleflight_CircuitOpenReturnsImmediately verifies that when the
// circuit breaker is open, Embed returns ErrCircuitOpen without firing an
// HTTP call.
func TestSingleflight_CircuitOpenReturnsImmediately(t *testing.T) {
	var callCount atomic.Int64
	srv := buildTestEmbeddingServer(t, &callCount, 0, 4)
	defer srv.Close()

	client := embeddings.NewClient(
		&http.Client{Timeout: 5 * time.Second},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		"nexus_test4",
	)
	reg := newNopRegistry()
	// Pre-trip the breaker for the specific (provider, model) key so the next
	// Embed call hits ErrCircuitOpen immediately.
	cb := reg.Get("openai-tripped", "m")
	cb.mu.Lock()
	cb.setState(cbStateOpen)
	cb.lastTripAt = time.Now()
	cb.mu.Unlock()

	sf := NewEmbeddingSingleflight(client, reg, 5*time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))

	req := embeddings.Request{Model: "m", Input: "x", Dimensions: 4}
	_, err := sf.Embed(context.Background(), "openai-tripped", srv.URL, "m", "", req)
	if !errors.Is(err, ErrCircuitOpen) {
		t.Errorf("expected ErrCircuitOpen, got: %v", err)
	}
	if callCount.Load() != 0 {
		t.Error("upstream call should not have fired when circuit is open")
	}
}

// TestSingleflight_DifferentInputsDontCoalesce verifies that calls with
// different inputs are NOT coalesced — each gets its own upstream call.
func TestSingleflight_DifferentInputsDontCoalesce(t *testing.T) {
	var callCount atomic.Int64
	srv := buildTestEmbeddingServer(t, &callCount, 10*time.Millisecond, 4)
	defer srv.Close()

	client := embeddings.NewClient(
		&http.Client{Timeout: 5 * time.Second},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		"nexus_test5",
	)
	reg := newNopRegistry()
	sf := NewEmbeddingSingleflight(client, reg, 5*time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))

	var wg sync.WaitGroup
	for _, input := range []string{"alpha", "beta", "gamma"} {
		wg.Add(1)
		go func(inp string) {
			defer wg.Done()
			req := embeddings.Request{Model: "m", Input: inp, Dimensions: 4}
			_, _ = sf.Embed(context.Background(), "openai", srv.URL, "m", "", req)
		}(input)
	}
	wg.Wait()

	if n := callCount.Load(); n != 3 {
		t.Errorf("upstream call count = %d, want 3 (one per distinct input)", n)
	}
}

// TestSingleflight_JoinersDoNotTouchBreaker is the regression guard.
// A failing leader with N identical-input joiners must leave the breaker with
// exactly ONE recorded failure: the leader's. The prior bug had each joiner
// call RecordSuccess() before the leader resolved, which reset failureCount to
// 0 on every joiner — diluting the failure window and delaying (or preventing)
// the breaker from tripping under an identical-input burst.
func TestSingleflight_JoinersDoNotTouchBreaker(t *testing.T) {
	var callCount atomic.Int64
	// Server returns 500 after a 60ms delay so joiners attach while the leader
	// is still in flight, then the leader's call fails.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount.Add(1)
		time.Sleep(60 * time.Millisecond)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
	}))
	defer srv.Close()

	client := embeddings.NewClient(
		&http.Client{Timeout: 5 * time.Second},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		"nexus_test_breaker",
	)
	reg := newNopRegistry()
	sf := NewEmbeddingSingleflight(client, reg, 5*time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))

	const concurrency = 20
	req := embeddings.Request{Model: "m", Input: "shared-input", Dimensions: 4}

	var wg sync.WaitGroup
	wg.Add(concurrency)
	for range concurrency {
		go func() {
			defer wg.Done()
			_, _ = sf.Embed(context.Background(), "openai-brk", srv.URL, "m", "", req)
		}()
	}
	wg.Wait()

	// Exactly one upstream call (coalesced) → exactly one real outcome.
	if n := callCount.Load(); n != 1 {
		t.Fatalf("upstream call count = %d, want 1", n)
	}

	cb := reg.Get("openai-brk", "m")
	cb.mu.Lock()
	fc := cb.failureCount
	cb.mu.Unlock()
	// The leader failed once; no joiner success-reset should have wiped it.
	if fc != 1 {
		t.Errorf("breaker failureCount = %d, want 1 (joiners must not record success/failure)", fc)
	}
}

// TestEmbeddingKey_ModelIncluded verifies that embeddingKey includes the
// model so cross-model inputs produce different keys.
func TestEmbeddingKey_ModelIncluded(t *testing.T) {
	k1 := embeddingKey("model-a", "hello")
	k2 := embeddingKey("model-b", "hello")
	if k1 == k2 {
		t.Error("keys for different models should differ")
	}
}

// TestEmbeddingKey_Deterministic verifies that the same (model, input) always
// produces the same key.
func TestEmbeddingKey_Deterministic(t *testing.T) {
	k1 := embeddingKey("m", "hello world")
	k2 := embeddingKey("m", "hello world")
	if k1 != k2 {
		t.Error("embeddingKey is not deterministic")
	}
}

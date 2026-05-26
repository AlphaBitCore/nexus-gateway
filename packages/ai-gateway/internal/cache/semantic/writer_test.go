package semantic

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/embeddings"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	"github.com/prometheus/client_golang/prometheus"
)

// writerNSCounter generates unique Prometheus namespace suffixes per writer
// test to avoid global registry collisions when multiple tests call
// embeddings.NewClient (which uses promauto against the default registry).
var writerNSCounter atomic.Int64

// uniqueEmbNS returns a Prometheus-safe namespace unique across all calls
// in this test binary run.
func uniqueEmbNS() string {
	n := writerNSCounter.Add(1)
	return fmt.Sprintf("nexusw%d", n)
}

// newTestWriter builds all the components needed for a Writer test.
// It returns the writer, a ConfigCache pre-configured with a valid snapshot,
// and a cleanup function.
func newTestWriter(t *testing.T, srvURL string, dim int) (*Writer, *ConfigCache, func()) {
	t.Helper()
	c, redisCleanup := newTestClient(t)

	httpClient := &http.Client{Timeout: 5 * time.Second}
	// Use a unique namespace per call to avoid global registry collisions.
	embClient := embeddings.NewClient(httpClient, slog.New(slog.NewTextHandler(io.Discard, nil)), uniqueEmbNS())
	reg := newNopRegistry()
	sf := NewEmbeddingSingleflight(embClient, reg, 2*time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))

	cache := NewConfigCache()
	indexName := "writer-test-idx"
	_ = srvURL // provided for override purposes; index is created here regardless
	if err := c.EnsureIndex(context.Background(), indexName, dim); err != nil {
		t.Fatalf("EnsureIndex: %v", err)
	}

	snap := ConfigSnapshot{
		Enabled:             true,
		EmbeddingProviderID: "openai",
		EmbeddingModelID:    "text-embedding-3-small",
		EmbeddingDimension:  dim,
		Fingerprint:         "fp-writer",
		RedisIndexName:      indexName,
	}
	cache.Set(snap)

	w := NewWriter(cache, c, sf, slog.New(slog.NewTextHandler(io.Discard, nil)), 0, nil)
	return w, cache, redisCleanup
}

// buildDimEmbeddingServer creates a test HTTP server returning a float32 vector
// of `dim` dimensions. When dim == 0, returns HTTP 500.
func buildDimEmbeddingServer(t *testing.T, dim int, callCount *atomic.Int64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if callCount != nil {
			callCount.Add(1)
		}
		if dim == 0 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"internal error"}}`))
			return
		}
		vecPart := ""
		for i := range dim {
			if i > 0 {
				vecPart += ","
			}
			vecPart += "0.1"
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"object":"embedding","index":0,"embedding":[` + vecPart + `]}],"model":"text-embedding-3-small","usage":{"prompt_tokens":5,"total_tokens":5}}`))
	}))
}

// buildWriteReq builds a WriteRequest pointing at the given server URL.
func buildWriteReq(srvURL, embInput string) WriteRequest {
	return WriteRequest{
		VKScope:          "v1:vk:test",
		UpstreamProvider: "openai",
		UpstreamModel:    "gpt-4o-mini",
		ResponseKind:     "response",
		EmbeddingInput:   embInput,
		ResponseBody:     []byte(`{"id":"r1","choices":[]}`),
		Usage:            map[string]any{"prompt_tokens": 5},
		TTL:              5 * time.Minute,
		ProviderBaseURL:  srvURL,
		EmbeddingAPIKey:  "",
	}
}

// Happy path

func TestWriter_Write_StoredOnSuccess(t *testing.T) {
	dim := 4
	srv := buildDimEmbeddingServer(t, dim, nil)
	defer srv.Close()

	w, _, cleanup := newTestWriter(t, srv.URL, dim)
	defer cleanup()

	// Patch writer's singleflight to use the correct server URL.
	// (newTestWriter builds an SF with no server wired; we need to give the
	// right URL to Write via the WriteRequest.)
	req := buildWriteReq(srv.URL, "hello world")
	result, err := w.Write(context.Background(), req)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !result.Stored {
		t.Errorf("expected Stored=true, got %+v", result)
	}
	if result.Skipped {
		t.Errorf("expected Skipped=false, got %+v", result)
	}
}

// Skip reason: semantic_unavailable (EffectiveEnabled == false)

func TestWriter_Write_SkipDisabled_KillSwitch(t *testing.T) {
	w, cache, cleanup := newTestWriter(t, "", 4)
	defer cleanup()

	snap := cache.Get()
	snap.Enabled = false
	cache.Set(snap)

	result, err := w.Write(context.Background(), buildWriteReq("http://unused", "x"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Skipped || result.SkipReason != audit.GatewayCacheSkipReasonSemanticUnavailable {
		t.Errorf("expected skip_disabled, got %+v", result)
	}
}

func TestWriter_Write_SkipDisabled_NoProvider(t *testing.T) {
	w, cache, cleanup := newTestWriter(t, "", 4)
	defer cleanup()

	snap := cache.Get()
	snap.EmbeddingProviderID = ""
	cache.Set(snap)

	result, err := w.Write(context.Background(), buildWriteReq("http://unused", "x"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Skipped || result.SkipReason != audit.GatewayCacheSkipReasonSemanticUnavailable {
		t.Errorf("expected skip_disabled, got %+v", result)
	}
}

// Skip reason: embedding_circuit_open

func TestWriter_Write_SkipEmbeddingCircuitOpen(t *testing.T) {
	dim := 4
	srv := buildDimEmbeddingServer(t, dim, nil)
	defer srv.Close()

	c, redisCleanup := newTestClient(t)
	defer redisCleanup()

	httpClient := &http.Client{Timeout: 5 * time.Second}
	embClient := embeddings.NewClient(httpClient, slog.New(slog.NewTextHandler(io.Discard, nil)), uniqueEmbNS())

	// Build a registry with a pre-tripped breaker for the (provider, model) key
	// that Writer.Write will resolve (EmbeddingProviderID="p", EmbeddingModelID="m").
	isolatedReg := prometheus.NewRegistry()
	cbReg := &CircuitBreakerRegistry{
		breakers: make(map[string]*CircuitBreaker),
	}
	cbReg.newFn = func(key string) *CircuitBreaker {
		cb := &CircuitBreaker{
			failureThreshold: 1000,
			failureWindow:    time.Hour,
			halfOpenAfter:    time.Hour,
			log:              slog.New(slog.NewTextHandler(io.Discard, nil)),
			state:            cbStateOpen,
			lastTripAt:       time.Now(),
			windowStart:      time.Now(),
		}
		cb.stateGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name:        "cb_open_state_co",
			ConstLabels: prometheus.Labels{"model_key": key},
		}, []string{"state"})
		_ = isolatedReg.Register(cb.stateGauge)
		cb.tripsTotal = prometheus.NewCounter(prometheus.CounterOpts{
			Name:        "cb_open_trips_co",
			ConstLabels: prometheus.Labels{"model_key": key},
		})
		_ = isolatedReg.Register(cb.tripsTotal)
		for _, name := range cbStateNames {
			cb.stateGauge.WithLabelValues(name).Set(0)
		}
		cb.stateGauge.WithLabelValues("open").Set(1)
		return cb
	}

	sf := NewEmbeddingSingleflight(embClient, cbReg, 2*time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))

	cache := NewConfigCache()
	indexName := "writer-cb-idx"
	_ = c.EnsureIndex(context.Background(), indexName, dim)
	cache.Set(ConfigSnapshot{
		Enabled: true, EmbeddingProviderID: "p", EmbeddingModelID: "m",
		EmbeddingDimension: dim, Fingerprint: "fp", RedisIndexName: indexName,
	})

	w := NewWriter(cache, c, sf, slog.New(slog.NewTextHandler(io.Discard, nil)), 0, nil)

	result, err := w.Write(context.Background(), buildWriteReq(srv.URL, "x"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Skipped || result.SkipReason != audit.GatewayCacheSkipReasonEmbeddingCircuitOpen {
		t.Errorf("expected embedding_circuit_open, got %+v", result)
	}
}

// Skip reason: embedding_timeout

func TestWriter_Write_SkipEmbeddingTimeout(t *testing.T) {
	// Server delays 300ms; hard timeout is 50ms.
	slowSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.1,0.2,0.3,0.4]}],"model":"m","usage":{"prompt_tokens":1,"total_tokens":1}}`))
	}))
	defer slowSrv.Close()

	c, redisCleanup := newTestClient(t)
	defer redisCleanup()

	httpClient := &http.Client{Timeout: 5 * time.Second}
	embClient := embeddings.NewClient(httpClient, slog.New(slog.NewTextHandler(io.Discard, nil)), uniqueEmbNS())
	reg := newNopRegistry()
	// Hard timeout of 50ms — shorter than the server's 300ms delay.
	sf := NewEmbeddingSingleflight(embClient, reg, 50*time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil)))

	cache := NewConfigCache()
	indexName := "timeout-idx"
	_ = c.EnsureIndex(context.Background(), indexName, 4)
	cache.Set(ConfigSnapshot{
		Enabled: true, EmbeddingProviderID: "p", EmbeddingModelID: "m",
		EmbeddingDimension: 4, Fingerprint: "fp", RedisIndexName: indexName,
	})

	w := NewWriter(cache, c, sf, slog.New(slog.NewTextHandler(io.Discard, nil)), 0, nil)

	result, err := w.Write(context.Background(), buildWriteReq(slowSrv.URL, "slow-input"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Skipped {
		t.Errorf("expected Skipped=true, got %+v", result)
	}
	// May be embedding_timeout or embedding_provider_error depending on how the
	// context error is classified.
	if result.SkipReason != audit.GatewayCacheSkipReasonEmbeddingTimeout &&
		result.SkipReason != audit.GatewayCacheSkipReasonEmbeddingProviderError {
		t.Errorf("expected embedding_timeout or embedding_provider_error, got %+v", result)
	}
}

// Skip reason: embedding_provider_error

func TestWriter_Write_SkipEmbeddingProviderError(t *testing.T) {
	// Server returns 500.
	srv := buildDimEmbeddingServer(t, 0, nil)
	defer srv.Close()

	c, redisCleanup := newTestClient(t)
	defer redisCleanup()

	httpClient := &http.Client{Timeout: 5 * time.Second}
	embClient := embeddings.NewClient(httpClient, slog.New(slog.NewTextHandler(io.Discard, nil)), uniqueEmbNS())
	reg := newNopRegistry()
	sf := NewEmbeddingSingleflight(embClient, reg, 2*time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))

	cache := NewConfigCache()
	indexName := "pe-idx"
	_ = c.EnsureIndex(context.Background(), indexName, 4)
	cache.Set(ConfigSnapshot{
		Enabled: true, EmbeddingProviderID: "p", EmbeddingModelID: "m",
		EmbeddingDimension: 4, Fingerprint: "fp", RedisIndexName: indexName,
	})

	w := NewWriter(cache, c, sf, slog.New(slog.NewTextHandler(io.Discard, nil)), 0, nil)
	result, err := w.Write(context.Background(), buildWriteReq(srv.URL, "err-input"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Skipped {
		t.Errorf("expected Skipped=true, got %+v", result)
	}
	if result.SkipReason != audit.GatewayCacheSkipReasonEmbeddingProviderError &&
		result.SkipReason != audit.GatewayCacheSkipReasonEmbeddingTimeout {
		t.Errorf("expected embedding_provider_error, got %+v", result)
	}
}

// Skip reason: embedding_dim_mismatch

func TestWriter_Write_SkipEmbeddingDimMismatch(t *testing.T) {
	// Server returns a 4-dim vector, but ConfigSnapshot says dim=8.
	srv := buildDimEmbeddingServer(t, 4, nil)
	defer srv.Close()

	c, redisCleanup := newTestClient(t)
	defer redisCleanup()

	httpClient := &http.Client{Timeout: 5 * time.Second}
	embClient := embeddings.NewClient(httpClient, slog.New(slog.NewTextHandler(io.Discard, nil)), uniqueEmbNS())
	reg := newNopRegistry()
	sf := NewEmbeddingSingleflight(embClient, reg, 2*time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))

	cache := NewConfigCache()
	indexName := "dm-idx"
	_ = c.EnsureIndex(context.Background(), indexName, 8)
	cache.Set(ConfigSnapshot{
		Enabled:             true,
		EmbeddingProviderID: "p",
		EmbeddingModelID:    "m",
		EmbeddingDimension:  8, // mismatch — server returns 4
		Fingerprint:         "fp",
		RedisIndexName:      indexName,
	})

	w := NewWriter(cache, c, sf, slog.New(slog.NewTextHandler(io.Discard, nil)), 0, nil)
	result, err := w.Write(context.Background(), buildWriteReq(srv.URL, "dim-mismatch"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Skipped || result.SkipReason != audit.GatewayCacheSkipReasonEmbeddingDimMismatch {
		t.Errorf("expected embedding_dim_mismatch, got %+v", result)
	}
}

// Skip reason: entry too large (ErrEntryTooLarge — no specific audit reason)

func TestWriter_Write_SkipEntryTooLarge(t *testing.T) {
	dim := 4
	srv := buildDimEmbeddingServer(t, dim, nil)
	defer srv.Close()

	c, redisCleanup := newTestClient(t)
	defer redisCleanup()

	httpClient := &http.Client{Timeout: 5 * time.Second}
	embClient := embeddings.NewClient(httpClient, slog.New(slog.NewTextHandler(io.Discard, nil)), uniqueEmbNS())
	reg := newNopRegistry()
	sf := NewEmbeddingSingleflight(embClient, reg, 2*time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))

	cache := NewConfigCache()
	indexName := "tl-idx"
	_ = c.EnsureIndex(context.Background(), indexName, dim)
	cache.Set(ConfigSnapshot{
		Enabled: true, EmbeddingProviderID: "p", EmbeddingModelID: "m",
		EmbeddingDimension: dim, Fingerprint: "fp", RedisIndexName: indexName,
	})

	// maxEntryBytes = 1 byte — any real response_body will exceed this.
	w := NewWriter(cache, c, sf, slog.New(slog.NewTextHandler(io.Discard, nil)), 1, nil)

	req := buildWriteReq(srv.URL, "too-large")
	req.ResponseBody = make([]byte, 100)
	result, err := w.Write(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Skipped {
		t.Errorf("expected Skipped=true for oversized entry, got %+v", result)
	}
}

// translateEmbedError unit tests — one per error type

func TestTranslateEmbedError_CircuitOpen(t *testing.T) {
	r := translateEmbedError(ErrCircuitOpen)
	if r != audit.GatewayCacheSkipReasonEmbeddingCircuitOpen {
		t.Errorf("got %q, want embedding_circuit_open", r)
	}
}

func TestTranslateEmbedError_Timeout(t *testing.T) {
	r := translateEmbedError(embeddings.ErrEmbeddingTimeout)
	if r != audit.GatewayCacheSkipReasonEmbeddingTimeout {
		t.Errorf("got %q, want embedding_timeout", r)
	}
}

func TestTranslateEmbedError_DimMismatch(t *testing.T) {
	r := translateEmbedError(embeddings.ErrEmbeddingDimMismatch)
	if r != audit.GatewayCacheSkipReasonEmbeddingDimMismatch {
		t.Errorf("got %q, want embedding_dim_mismatch", r)
	}
}

func TestTranslateEmbedError_ProviderError(t *testing.T) {
	r := translateEmbedError(embeddings.ErrEmbeddingProviderError)
	if r != audit.GatewayCacheSkipReasonEmbeddingProviderError {
		t.Errorf("got %q, want embedding_provider_error", r)
	}
}

func TestTranslateEmbedError_OtherError(t *testing.T) {
	r := translateEmbedError(errors.New("unexpected"))
	// Falls back to timeout mapping for unknown errors.
	if r != audit.GatewayCacheSkipReasonEmbeddingTimeout {
		t.Errorf("got %q, want embedding_timeout fallback", r)
	}
}

// translateStoreError unit tests — one per error type

func TestTranslateStoreError_EntryTooLarge(t *testing.T) {
	reason, label := translateStoreError(ErrEntryTooLarge)
	if reason != "" {
		t.Errorf("reason = %q, want empty (no audit constant)", reason)
	}
	if label != "skip_oversize" {
		t.Errorf("label = %q, want 'skip_oversize'", label)
	}
}

func TestTranslateStoreError_ValkeyUnavailable(t *testing.T) {
	reason, label := translateStoreError(ErrValkeyUnavailable)
	if reason != audit.GatewayCacheSkipReasonValkeyUnavailable {
		t.Errorf("reason = %q", reason)
	}
	if label != "skip_valkey_unavailable" {
		t.Errorf("label = %q", label)
	}
}

func TestTranslateStoreError_Other(t *testing.T) {
	reason, label := translateStoreError(errors.New("unknown"))
	if reason != audit.GatewayCacheSkipReasonSemanticSearchError {
		t.Errorf("reason = %q", reason)
	}
	if label != "skip_search_error" {
		t.Errorf("label = %q", label)
	}
}

// skipReasonToMetricLabel unit tests — covers all branches

func TestSkipReasonToMetricLabel_AllBranches(t *testing.T) {
	cases := []struct {
		reason audit.GatewayCacheSkipReason
		want   string
	}{
		{audit.GatewayCacheSkipReasonSemanticUnavailable, "skip_disabled"},
		{audit.GatewayCacheSkipReasonEmbeddingTimeout, "skip_embedding_timeout"},
		{audit.GatewayCacheSkipReasonEmbeddingCircuitOpen, "skip_embedding_circuit"},
		{audit.GatewayCacheSkipReasonEmbeddingDimMismatch, "skip_embedding_dim_mismatch"},
		{audit.GatewayCacheSkipReasonEmbeddingProviderError, "skip_embedding_error"},
		{audit.GatewayCacheSkipReasonValkeyUnavailable, "skip_valkey_unavailable"},
		{audit.GatewayCacheSkipReasonSemanticSearchError, "skip_search_error"},
		{"unknown_reason", "skip_unknown"},
	}
	for _, tc := range cases {
		t.Run(string(tc.reason), func(t *testing.T) {
			got := skipReasonToMetricLabel(tc.reason)
			if got != tc.want {
				t.Errorf("skipReasonToMetricLabel(%q) = %q, want %q", tc.reason, got, tc.want)
			}
		})
	}
}

// embedErrorToOutcome unit tests

func TestEmbedErrorToOutcome_CircuitOpen(t *testing.T) {
	if got := embedErrorToOutcome(ErrCircuitOpen); got != "circuit_open" {
		t.Errorf("got %q, want 'circuit_open'", got)
	}
}

func TestEmbedErrorToOutcome_Other(t *testing.T) {
	if got := embedErrorToOutcome(errors.New("x")); got != "error" {
		t.Errorf("got %q, want 'error'", got)
	}
}

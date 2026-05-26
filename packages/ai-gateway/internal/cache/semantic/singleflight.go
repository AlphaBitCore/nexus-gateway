package semantic

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/embeddings"
)

// defaultEmbedTimeout is the hard timeout for a leader embedding call per
// response-cache-architecture.md §3.11. A joiner context cancellation does
// NOT cancel the leader; the leader is bounded by this timeout on an
// independent context.
//
// 5s matches a realistic OpenAI/Gemini embedding TTFB upper bound (p99
// is typically ~600ms; we allow generous headroom for first-call cold
// path and per-region jitter). Values much lower cause L2 lookups to
// time out on cold cache (observable via
// nexus_cache_l2_lookups_total{outcome="skip_embedding_timeout"}).
const defaultEmbedTimeout = 5 * time.Second

// inflight tracks a single in-flight embedding call.
type inflight struct {
	done   chan struct{}
	result embeddings.Response
	err    error
}

// EmbeddingSingleflight deduplicates concurrent embedding calls that share
// the same input. When N goroutines call Embed with the same (model, input),
// only one HTTP call is issued to the embedding provider; all N callers share
// the leader's result.
//
// Cancellation semantics (per response-cache-architecture.md §3.3):
//   - The leader runs on an independent context bounded by hardTimeout, NOT
//     the caller's context. A single impatient client cannot cancel work that
//     99 others are waiting for.
//   - Joiners block until the leader finishes OR their own ctx.Done fires,
//     whichever comes first. When a joiner disconnects, the leader continues.
//   - If the leader exceeds hardTimeout every joiner receives the timeout
//     error.
//
// Circuit breaker: the registry selects the per-(providerID, modelID) breaker
// on each Embed call. If the breaker returns Allow()==false, Embed returns
// ErrCircuitOpen immediately without creating a leader call.
type EmbeddingSingleflight struct {
	client      *embeddings.Client
	cbRegistry  *CircuitBreakerRegistry
	hardTimeout time.Duration
	log         *slog.Logger

	mu   sync.Mutex
	map_ map[string]*inflight // keyed by sha256(model+":"+input)
}

// NewEmbeddingSingleflight constructs a singleflight wrapper. hardTimeout
// defaults to defaultEmbedTimeout (100ms) when zero.
func NewEmbeddingSingleflight(
	client *embeddings.Client,
	cbRegistry *CircuitBreakerRegistry,
	hardTimeout time.Duration,
	log *slog.Logger,
) *EmbeddingSingleflight {
	if hardTimeout <= 0 {
		hardTimeout = defaultEmbedTimeout
	}
	return &EmbeddingSingleflight{
		client:      client,
		cbRegistry:  cbRegistry,
		hardTimeout: hardTimeout,
		log:         log,
		map_:        make(map[string]*inflight),
	}
}

// ErrCircuitOpen is returned by Embed when the circuit breaker is open.
var ErrCircuitOpen = errors.New("embeddings/singleflight: circuit breaker open")

// Embed calls the embedding model for the given input. Concurrent calls
// with identical (model, input) share one underlying HTTP request.
//
// Parameters:
//   - ctx: caller's context. Used only for joiner wait cancellation; the
//     leader's underlying HTTP call is bounded by hardTimeout.
//   - providerID: the embedding provider identifier; used to select the
//     correct per-(provider, model) circuit breaker from the registry.
//   - providerBaseURL, model, apiKey: forwarded to embeddings.Client.Embed.
//   - req: embedding request; req.Input is the deduplication key.
//
// The expectedDim passed to embeddings.Client.Embed is 0 — the caller is
// responsible for verifying dimension after the call (the dimension check
// happens in the Writer where the ConfigSnapshot.EmbeddingDimension is
// available).
func (sf *EmbeddingSingleflight) Embed(
	ctx context.Context,
	providerID, providerBaseURL, model, apiKey string,
	req embeddings.Request,
) (embeddings.Response, error) {
	// Resolve the per-(providerID, model) circuit breaker and check before
	// acquiring the map lock so hot-path rejection is as cheap as possible.
	cb := sf.cbRegistry.Get(providerID, model)
	if !cb.Allow() {
		return embeddings.Response{}, ErrCircuitOpen
	}

	key := embeddingKey(model, req.Input)

	sf.mu.Lock()
	if fl, ok := sf.map_[key]; ok {
		// Joiner path: another goroutine is already in-flight.
		sf.mu.Unlock()
		// The circuit breaker Allow() above counted this as "allowed" — but
		// a joiner never actually fires an HTTP call. Record the breaker
		// slot as a success immediately so the failure-window is not
		// incorrectly widened (joiners share the leader's real outcome via
		// the fl.err channel).
		cb.RecordSuccess()
		select {
		case <-fl.done:
			if fl.err != nil {
				return embeddings.Response{}, fl.err
			}
			return fl.result, nil
		case <-ctx.Done():
			return embeddings.Response{}, ctx.Err()
		}
	}

	// Leader path: create a new inflight record.
	fl := &inflight{done: make(chan struct{})}
	sf.map_[key] = fl
	sf.mu.Unlock()

	// Run the embedding call on an independent context so that caller
	// cancellation cannot abort work that joiners are waiting for.
	leaderCtx, cancel := context.WithTimeout(context.Background(), sf.hardTimeout)
	go func() {
		defer cancel()
		defer func() {
			sf.mu.Lock()
			delete(sf.map_, key)
			sf.mu.Unlock()
			close(fl.done)
		}()

		resp, err := sf.client.Embed(leaderCtx, providerBaseURL, model, apiKey, req, 0)
		fl.result = resp
		fl.err = err

		if err != nil {
			cb.RecordFailure()
			sf.log.Warn("cache/semantic: embedding leader call failed",
				"model", model, "providerID", providerID, "error", err)
		} else {
			cb.RecordSuccess()
		}
	}()

	// Original caller waits for the leader (also bounded by its own ctx).
	select {
	case <-fl.done:
		if fl.err != nil {
			return embeddings.Response{}, fl.err
		}
		return fl.result, nil
	case <-ctx.Done():
		return embeddings.Response{}, ctx.Err()
	}
}

// embeddingKey returns sha256(model + ":" + input) as a hex string. The
// model is included so that a cross-model swap never reuses stale embeddings
// computed for a different vector space.
func embeddingKey(model, input string) string {
	h := sha256.New()
	h.Write([]byte(model))
	h.Write([]byte(":"))
	h.Write([]byte(input))
	return fmt.Sprintf("%x", h.Sum(nil))
}

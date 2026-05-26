package streamcache

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	cache "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/core"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// Registry is the global broker map keyed by cache key. Concurrent
// Subscribe calls for the same key dedupe to one broker so the
// gateway issues exactly one upstream call per cache key per
// in-flight window.
//
// A broker self-deregisters when its pump terminates (via the
// onClose callback set by Registry); a subsequent Subscribe for
// the same key after deregistration starts a fresh broker.
//
// Concurrency model: r.mu protects the brokers + pending maps but is
// NEVER held across the leaderFn call (which blocks on the upstream
// HTTP roundtrip for ~hundreds of ms).
// Same-key joiners while leaderFn is in flight wait on the pending
// entry's done channel without holding r.mu; different-key calls
// proceed concurrently.
type Registry struct {
	mu      sync.Mutex
	brokers map[string]*Broker
	pending map[string]*pendingBroker
	cache   *cache.Cache
	log     *slog.Logger
	metrics *Metrics
}

// pendingBroker tracks an in-flight leaderFn call so other Subscribe
// calls for the same key can wait without blocking different-key
// callers. Set once by the leader goroutine before close(done); fields
// must not be mutated by joiners.
type pendingBroker struct {
	done   chan struct{} // closed when leaderFn returns
	broker *Broker       // set when leaderFn succeeded
	err    error         // set when leaderFn returned an error (or meta==nil)
}

// NewRegistry constructs an empty Registry. c may be nil to disable
// cache writes (the broker becomes pure fan-out). m may be nil to
// disable Prometheus instrumentation.
func NewRegistry(c *cache.Cache, log *slog.Logger, m *Metrics) *Registry {
	if log == nil {
		log = slog.Default()
	}
	return &Registry{
		brokers: make(map[string]*Broker),
		pending: make(map[string]*pendingBroker),
		cache:   c,
		log:     log,
		metrics: m,
	}
}

// Subscribe returns a ChunkSubscription for the given key plus
// isFirstSubscriber: true means this caller triggered leaderFn (the
// proxy handler stamps audit.CacheStatus = MISS); false means the
// caller joined an existing broker (audit.CacheStatus = HIT_LIVE).
//
// Concurrency: r.mu is held only across the map-lookup / map-update
// fast-path sections — never across leaderFn. A first caller for a
// key inserts a pendingBroker placeholder under r.mu, releases the
// lock, runs leaderFn, then re-acquires r.mu to finalise. Same-key
// callers that arrive while leaderFn is in flight wait on the
// placeholder's done channel without holding the registry mutex, so
// they do not block different-key callers.
//
// On leaderFn error (or meta==nil) the placeholder is removed under
// r.mu before close(done); same-key waiters then loop and either
// observe a freshly-created broker (if a sibling call won the race)
// or take the leader role themselves.
//
// leaderFn returning nil CacheMeta is treated as an error: the
// registry closes the session and returns an error rather than store
// a malformed broker.
func (r *Registry) Subscribe(ctx context.Context, key string, leaderFn LeaderFn) (ChunkSubscription, bool, error) {
	for {
		r.mu.Lock()

		if existing, ok := r.brokers[key]; ok {
			r.mu.Unlock()
			return existing.subscribe(), false, nil
		}

		if pend, ok := r.pending[key]; ok {
			// Same-key leaderFn already in flight elsewhere. Wait on
			// its done channel WITHOUT holding r.mu so different-key
			// callers (and the leader goroutine itself) can proceed.
			r.mu.Unlock()
			select {
			case <-pend.done:
			case <-ctx.Done():
				return nil, false, ctx.Err()
			}
			// Loop: by now brokers[key] is either set (sibling won
			// leader, we join) or absent (leader failed; we re-enter
			// as the new leader).
			continue
		}

		// Place an empty pending entry so concurrent same-key callers
		// know a leader is in flight. Release r.mu BEFORE running
		// leaderFn so it does not serialise different-key callers.
		pend := &pendingBroker{done: make(chan struct{})}
		r.pending[key] = pend
		r.mu.Unlock()

		sess, meta, err := leaderFn(ctx)

		r.mu.Lock()
		// Remove the placeholder regardless of outcome — either we
		// promote it to a broker entry below or we surface the error
		// to all waiters.
		delete(r.pending, key)

		if err != nil {
			pend.err = err
			r.mu.Unlock()
			close(pend.done)
			return nil, true, err
		}
		if meta == nil {
			pend.err = errors.New("streamcache: leaderFn returned nil CacheMeta")
			r.mu.Unlock()
			close(pend.done)
			_ = sess.Close()
			return nil, true, pend.err
		}

		// onClose captures b by reference (the variable, not its
		// current value). By the time the broker's pump calls onClose,
		// b has been assigned below, so the pointer-identity check is
		// valid and a new broker for the same key cannot be evicted
		// by the old broker's onClose.
		var b *Broker
		onClose := func() {
			r.mu.Lock()
			if cur, ok := r.brokers[key]; ok && cur == b {
				delete(r.brokers, key)
			}
			r.mu.Unlock()
		}

		b = newBroker(context.Background(), key, *meta, r.cache, r.log, r.metrics, onClose)
		r.brokers[key] = b
		pend.broker = b
		r.mu.Unlock()
		close(pend.done)

		go b.pump(sess)
		return b.subscribe(), true, nil
	}
}

// lookup is used by tests to inspect registry state without racing.
func (r *Registry) lookup(key string) (*Broker, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.brokers[key]
	return b, ok
}

// LeaderFn opens the upstream and returns a chunk source. The broker
// pumps Next() until io.EOF or *provcore.ProviderError. CacheMeta is
// used to construct the right kind of cache entry on success; the
// broker captures it once at create time so cache writes do not have
// to re-read routing state.
type LeaderFn func(ctx context.Context) (provcore.StreamSession, *CacheMeta, error)

// CacheMeta is the per-key metadata used by the broker to construct
// a cache entry when the upstream stream terminates normally.
type CacheMeta struct {
	Provider string // cache entry's Provider field
	Model    string // ProviderModelID
	IsStream bool   // true => StreamEntry; false => ResponseEntry (single terminal chunk)
	// UpstreamHeaders captures the HTTP response headers observed by the leader
	// when the upstream session opened. Persisted verbatim; the active
	// forward-header allowlist is applied at HIT replay time, so a config
	// change immediately changes what headers surface to the client without
	// requiring invalidation. May be nil for tests that don't care about
	// response-header forwarding.
	UpstreamHeaders map[string][]string
	// OriginWireShape encodes both the ingress endpoint kind and body
	// format; tagged so cross-ingress reshape can decide whether to
	// re-encode or serve verbatim. See cache/core.ResponseEntry.
	OriginWireShape typology.WireShape
}

// Broker is per-cache-key, owns one upstream pump goroutine, has a
// chunk ringbuffer, and is reference-counted by subscribers. There is
// no "leader" concept — the upstream context is owned by the broker
// itself, not by any subscriber. Subscribers are equal peers; the
// upstream context is cancelled only when ref-count hits zero before
// the stream has terminated.
type Broker struct {
	key     string
	meta    CacheMeta
	rb      *RingBuffer
	cache   *cache.Cache
	log     *slog.Logger
	metrics *Metrics

	upstreamCtx    context.Context
	upstreamCancel context.CancelFunc

	refCount atomic.Int32
	pumpDone chan struct{} // closed when pump exits

	closeOnce sync.Once
	onClose   func()
}

// newBroker constructs a broker but does NOT start the pump. The
// caller (Registry, in Task 7) is responsible for `go b.pump(session)`
// after acquiring the upstream session via LeaderFn.
//
// parentCtx is the registry-level lifetime; the broker derives a
// cancellable upstream ctx from it. m may be nil to disable metrics.
func newBroker(parentCtx context.Context, key string, meta CacheMeta, c *cache.Cache, log *slog.Logger, m *Metrics, onClose func()) *Broker {
	if log == nil {
		log = slog.Default()
	}
	if onClose == nil {
		onClose = func() {}
	}
	upCtx, cancel := context.WithCancel(parentCtx)
	b := &Broker{
		key:            key,
		meta:           meta,
		rb:             NewRingBuffer(),
		cache:          c,
		log:            log,
		metrics:        m,
		upstreamCtx:    upCtx,
		upstreamCancel: cancel,
		pumpDone:       make(chan struct{}),
		onClose:        onClose,
	}
	b.metrics.IncBrokerActive()
	return b
}

// pump reads from the upstream session into the ringbuffer until io.EOF
// (or a Done=true chunk) or *provcore.ProviderError. On normal
// termination, persists the timeline to cache. On error, broadcasts
// to subscribers and skips cache write. Always calls onClose exactly
// once on exit.
func (b *Broker) pump(session provcore.StreamSession) {
	defer close(b.pumpDone)
	defer func() { _ = session.Close() }()
	defer b.upstreamCancel() // release derived ctx resources
	defer b.metrics.DecBrokerActive()
	defer b.closeOnce.Do(b.onClose)

	var collected []provcore.Chunk
	for {
		chunk, err := session.Next(b.upstreamCtx)
		if errors.Is(err, io.EOF) {
			// Source exhausted without an explicit Done=true chunk.
			// Synthesize a terminal frame so subscribers see EOF
			// rather than parking indefinitely. Do NOT write cache
			// for an incomplete stream.
			b.rb.AppendTerminal(provcore.Chunk{Done: true})
			return
		}
		if err != nil {
			// Could be ctx.Err() (broker was cancelled because ref=0) or
			// a *provcore.ProviderError (upstream failure). Either way:
			// broadcast and exit; do not write cache.
			b.rb.Fail(err)
			return
		}

		if chunk.Done {
			collected = append(collected, chunk)
			b.rb.AppendTerminal(chunk)
			b.writeCache(collected)
			return
		}
		collected = append(collected, chunk)
		b.rb.Append(chunk)
	}
}

// writeCache persists the collected chunks to the cache. Streaming
// brokers persist a StreamEntry; non-streaming brokers persist a
// ResponseEntry built from the single terminal chunk's Delta (which
// the non-streaming wrapper at Task 11 will populate with the
// canonical response JSON).
func (b *Broker) writeCache(collected []provcore.Chunk) {
	if b.cache == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	usage := finalUsage(collected)

	if b.meta.IsStream {
		records := make([]cache.ChunkRecord, len(collected))
		for i, c := range collected {
			records[i] = cache.ChunkRecord{
				Delta:          c.Delta,
				ReasoningDelta: c.ReasoningDelta,
				ToolCallDeltas: c.ToolCallDeltas,
				Usage:          c.Usage,
				Done:           c.Done,
				NativeEvent:    c.NativeEvent,
				RawBytes:       append([]byte(nil), c.RawBytes...),
			}
		}
		entry := &cache.StreamEntry{
			Provider:        b.meta.Provider,
			Model:           b.meta.Model,
			Chunks:          records,
			Usage:           usage,
			CachedAt:        time.Now().UTC(),
			UpstreamHeaders: b.meta.UpstreamHeaders,
			OriginWireShape: b.meta.OriginWireShape,
		}
		n, err := b.cache.StoreStream(ctx, b.key, entry)
		if err != nil {
			if errors.Is(err, cache.ErrCacheEntryTooLarge) {
				b.log.Info("cache stream entry too large; skipping write", "key", b.key)
				b.metrics.RecordWrite("stream", "too_large", n)
			} else {
				b.log.Warn("cache stream store error", "key", b.key, "error", err)
				b.metrics.RecordWrite("stream", "encode_error", 0)
			}
		} else {
			b.metrics.RecordWrite("stream", "ok", n)
		}
		return
	}

	// Non-streaming: the wrapper (Task 11) emits a single Done chunk
	// whose Delta carries the canonical response JSON.
	if len(collected) == 0 || !collected[len(collected)-1].Done {
		return
	}
	entry := &cache.ResponseEntry{
		Provider:          b.meta.Provider,
		Model:             b.meta.Model,
		CanonicalResponse: []byte(collected[len(collected)-1].Delta),
		Usage:             usage,
		CachedAt:          time.Now().UTC(),
		UpstreamHeaders:   b.meta.UpstreamHeaders,
		OriginWireShape:   b.meta.OriginWireShape,
	}
	n, err := b.cache.StoreResponse(ctx, b.key, entry)
	if err != nil {
		if errors.Is(err, cache.ErrCacheEntryTooLarge) {
			b.log.Info("cache response entry too large; skipping write", "key", b.key)
			b.metrics.RecordWrite("response", "too_large", n)
		} else {
			b.log.Warn("cache response store error", "key", b.key, "error", err)
			b.metrics.RecordWrite("response", "encode_error", 0)
		}
	} else {
		b.metrics.RecordWrite("response", "ok", n)
	}
}

// finalUsage returns the most recent non-nil Usage from the chunk
// timeline. provcore.Usage fields are *int so a zero-value Usage
// is distinguishable from "no usage emitted".
func finalUsage(chunks []provcore.Chunk) provcore.Usage {
	for i := len(chunks) - 1; i >= 0; i-- {
		if chunks[i].Usage != nil {
			return *chunks[i].Usage
		}
	}
	return provcore.Usage{}
}

// subscribe attaches a new subscriber. Caller MUST call sub.Close()
// when done so ref-count decrements; broker cancels upstream when
// ref hits zero before pump terminates.
func (b *Broker) subscribe() ChunkSubscription {
	b.refCount.Add(1)
	b.metrics.IncSubscribers()
	return &brokerSub{broker: b}
}

type brokerSub struct {
	broker *Broker
	idx    int
	closed atomic.Bool
}

func (s *brokerSub) Next(ctx context.Context) (provcore.Chunk, error) {
	if s.closed.Load() {
		return provcore.Chunk{}, io.EOF
	}
	chunk, next, err := s.broker.rb.Read(ctx, s.idx)
	if err == nil {
		s.idx = next
	}
	return chunk, err
}

func (s *brokerSub) Close() error {
	if s.closed.Swap(true) {
		return nil
	}
	s.broker.metrics.DecSubscribers()
	if s.broker.refCount.Add(-1) == 0 {
		// No subscribers left. If the pump is still running, cancel
		// the upstream so it exits without writing cache.
		select {
		case <-s.broker.pumpDone:
			// Pump already finished; nothing to cancel.
		default:
			s.broker.upstreamCancel()
		}
	}
	return nil
}

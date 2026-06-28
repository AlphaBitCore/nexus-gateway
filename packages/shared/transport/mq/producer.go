// Package mq also provides a NATS JetStream implementation of the Producer and
// Consumer interfaces. Import this package with a blank identifier to register
// the "nats" driver:
//
//	import _ "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
package mq

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// jsConn is one pooled NATS connection plus its JetStream context. Each pool
// member has an INDEPENDENT async-publish ack barrier (PublishAsyncComplete), so
// publishing across the pool pipelines concurrently instead of serialising on one
// connection's drain-to-zero barrier.
type jsConn struct {
	nc *nats.Conn
	js jetstream.JetStream
}

// NATSProducer implements Producer using Core NATS (topics) and JetStream (queues),
// backed by a POOL of connections so high-volume async publishes (the audit
// pipeline) are not bottlenecked on a single connection's serial ack barrier.
type NATSProducer struct {
	conns   []jsConn
	rr      atomic.Uint64 // round-robin cursor for single-message publishes
	logger  *slog.Logger
	metrics *Metrics
}

const (
	// asyncMaxPending bounds in-flight (unacked) JetStream async publishes per
	// connection — the ceiling on memory held by un-acked records. Set above
	// asyncPublishChunk so a single chunk never blocks PublishAsync.
	asyncMaxPending = 2048
	// asyncPublishChunk is the max messages fired before waiting for the batch
	// to complete. Bounds in-flight to <= asyncMaxPending and lets an
	// arbitrarily large batch (e.g. a backed-up audit buffer) drain in chunks.
	asyncPublishChunk = 512
	// asyncPublishTimeout evicts a stuck async future by time so a server-side
	// stream stall (no TCP disconnect) cannot leave futures pending forever —
	// otherwise PublishAsyncComplete never closes and later chunks block their
	// whole ctx window until a reconnect clears them.
	asyncPublishTimeout = 5 * time.Second
	// defaultPublishPoolSize is the connection-pool size when NATSConfig leaves it
	// unset. Each connection has an independent async-publish ack barrier, so the
	// pool sets how many audit publish batches are in flight to NATS concurrently.
	defaultPublishPoolSize = 24
)

// NewNATSProducer opens a POOL of NATS connections (NATSConfig.PublishPoolSize, or
// defaultPublishPoolSize) and initialises a JetStream context per connection.
// Lifecycle callbacks are factored into newConnectionHandlers — see that helper
// for semantics (Disconnect→WARN + watchdog timer, Reconnect→INFO + cancel,
// Closed→ERROR, AsyncErr→ERROR, sustained-Disconnect→ERROR after threshold).
func NewNATSProducer(cfg NATSConfig, logger *slog.Logger, metrics *Metrics) (*NATSProducer, error) {
	poolSize := cfg.PublishPoolSize
	// Env override for tuning the connection pool without a config/rebuild cycle.
	if v, err := strconv.Atoi(os.Getenv("MQ_NATS_PUBLISH_POOL_SIZE")); err == nil && v > 0 {
		poolSize = v
	}
	if poolSize <= 0 {
		poolSize = defaultPublishPoolSize
	}
	p := &NATSProducer{logger: logger, metrics: metrics, conns: make([]jsConn, 0, poolSize)}
	for i := range poolSize {
		onDisconnect, onReconnect, onClosed, onAsyncErr := newConnectionHandlers(
			"producer", disconnectWatchdogThreshold, logger,
		)
		nc, err := nats.Connect(cfg.URL,
			nats.RetryOnFailedConnect(true),
			nats.MaxReconnects(-1),
			nats.ReconnectWait(2*time.Second),
			nats.DisconnectErrHandler(onDisconnect),
			nats.ReconnectHandler(onReconnect),
			nats.ClosedHandler(onClosed),
			nats.ErrorHandler(onAsyncErr),
		)
		if err != nil {
			_ = p.Close() // close any connections already opened
			return nil, fmt.Errorf("natsmq: connect %s (pool %d/%d): %w", cfg.URL, i+1, poolSize, err)
		}
		js, err := jetstream.New(nc,
			jetstream.WithPublishAsyncMaxPending(asyncMaxPending),
			jetstream.WithPublishAsyncTimeout(asyncPublishTimeout),
		)
		if err != nil {
			nc.Close()
			_ = p.Close()
			return nil, fmt.Errorf("natsmq: jetstream init (pool %d/%d): %w", i+1, poolSize, err)
		}
		p.conns = append(p.conns, jsConn{nc: nc, js: js})
	}
	return p, nil
}

// pick returns the next pool connection round-robin (for single-message ops).
func (p *NATSProducer) pick() jsConn {
	return p.conns[int(p.rr.Add(1))%len(p.conns)]
}

// PoolSize reports how many independent publish connections the producer holds,
// so a parallel caller (the audit Writer's publish workers) can pin one worker per
// connection — each connection has its own ack barrier, so worker N never contends
// on worker M's PublishAsyncComplete.
func (p *NATSProducer) PoolSize() int { return len(p.conns) }

// MaxPayload returns the broker-enforced maximum message size (bytes) negotiated
// on the connection. A publish larger than this is rejected by the server, so a
// caller assembling messages (e.g. the audit spill-recovery sweeper) uses it to
// avoid building a record/frame the broker can never accept. Returns 0 if no
// connection is established.
func (p *NATSProducer) MaxPayload() int64 {
	if len(p.conns) == 0 || p.conns[0].nc == nil {
		return 0
	}
	return p.conns[0].nc.MaxPayload()
}

// EnqueueBatchAsyncOn publishes a batch on a SPECIFIC pool connection (connIdx mod
// pool size). The audit Writer runs one publish worker per connection and pins each
// worker to its own index, so the per-connection ack barriers run concurrently
// instead of serialising on a single flush loop. Safe for concurrent use ACROSS
// distinct connIdx; two callers must not share a connIdx (each conn's
// PublishAsyncComplete is shared state). errs aligns with batch.
func (p *NATSProducer) EnqueueBatchAsyncOn(ctx context.Context, queue string, batch [][]byte, connIdx int) ([]error, error) {
	errs := make([]error, len(batch))
	conn := p.conns[((connIdx%len(p.conns))+len(p.conns))%len(p.conns)]
	p.enqueueOn(ctx, conn, queue, batch, errs)
	return errs, nil
}

// Publish sends a message to a topic using Core NATS (fire-and-forget, broadcast).
func (p *NATSProducer) Publish(_ context.Context, topic string, data []byte) error {
	if err := p.pick().nc.Publish(topic, data); err != nil {
		p.metrics.ErrorsTotal.Inc()
		return fmt.Errorf("natsmq: publish %s: %w", topic, err)
	}
	p.metrics.PublishedTotal.Inc()
	return nil
}

// Enqueue sends a message to a queue using JetStream (persistent, at-least-once).
func (p *NATSProducer) Enqueue(ctx context.Context, queue string, data []byte) error {
	if _, err := p.pick().js.Publish(ctx, queue, data); err != nil {
		p.metrics.ErrorsTotal.Inc()
		return fmt.Errorf("natsmq: enqueue %s: %w", queue, err)
	}
	p.metrics.EnqueuedTotal.Inc()
	return nil
}

// EnqueueBatchAsync publishes a batch to a JetStream queue using async publish,
// returning a per-message error slice aligned with batch (nil = acked). The
// whole batch drains in ~1 ack round-trip instead of len(batch) sequential
// publish-and-acks — the sync Enqueue's drain ceiling was publishConcurrency ÷
// ack-RTT, which let a slow/absent stream back the buffer up into multi-GB heap.
//
// The batch is fired in chunks of asyncPublishChunk so in-flight stays bounded
// by asyncMaxPending (a backed-up audit buffer can be tens of thousands of
// records). Within a chunk PublishAsync never blocks (chunk <= maxPending). A
// chunk waits for completion or ctx; messages whose future has not resolved by
// then are returned as errors so the caller re-buffers or spills them — no
// silent loss. The returned top-level error is reserved for a fire-time failure
// that aborts the whole batch; per-message fates are in the slice.
//
// This is an OPTIONAL capability beyond the mq.Producer interface; callers
// type-assert for it and fall back to per-message Enqueue when absent.
//
// This single-call form publishes the whole batch on ONE round-robin pool
// connection. Throughput parallelism comes from the audit Writer running multiple
// publish workers that each call EnqueueBatchAsyncOn pinned to a distinct
// connection — one serial flush loop calling this form would not pipeline across
// the pool. errs aligns with batch.
func (p *NATSProducer) EnqueueBatchAsync(ctx context.Context, queue string, batch [][]byte) ([]error, error) {
	errs := make([]error, len(batch))
	p.enqueueOn(ctx, p.pick(), queue, batch, errs)
	return errs, nil
}

// enqueueOn publishes one shard on a single pool connection, chunked by
// asyncPublishChunk with a per-chunk ack barrier. errs aligns with batch (the
// caller passes a sub-slice so writes land at the correct global offset).
func (p *NATSProducer) enqueueOn(ctx context.Context, conn jsConn, queue string, batch [][]byte, errs []error) {
	for start := 0; start < len(batch); start += asyncPublishChunk {
		end := min(start+asyncPublishChunk, len(batch))
		futures := make([]jetstream.PubAckFuture, end-start)
		for i := start; i < end; i++ {
			paf, err := conn.js.PublishAsync(queue, batch[i])
			if err != nil {
				errs[i] = fmt.Errorf("natsmq: enqueue async %s: %w", queue, err)
				p.metrics.ErrorsTotal.Inc()
				continue
			}
			futures[i-start] = paf
		}
		select {
		case <-conn.js.PublishAsyncComplete():
		case <-ctx.Done():
		}
		for k, paf := range futures {
			if paf == nil {
				continue // fire-time error already recorded
			}
			i := start + k
			select {
			case <-paf.Ok():
				p.metrics.EnqueuedTotal.Inc()
			case err := <-paf.Err():
				errs[i] = fmt.Errorf("natsmq: enqueue async %s: %w", queue, err)
				p.metrics.ErrorsTotal.Inc()
			default:
				// Not resolved by the ctx deadline → treat as failed so the
				// caller retries/spills; a duplicate on a later retry is
				// deduped downstream by request id (at-least-once contract).
				errs[i] = fmt.Errorf("natsmq: enqueue async %s: %w", queue, ctx.Err())
				p.metrics.ErrorsTotal.Inc()
			}
		}
		if ctx.Err() != nil {
			for i := end; i < len(batch); i++ {
				errs[i] = fmt.Errorf("natsmq: enqueue async %s: %w", queue, ctx.Err())
				p.metrics.ErrorsTotal.Inc()
			}
			return
		}
	}
}

// Close flushes pending messages and closes every pooled NATS connection.
func (p *NATSProducer) Close() error {
	for _, c := range p.conns {
		if c.nc == nil {
			continue
		}
		if err := c.nc.FlushTimeout(5 * time.Second); err != nil {
			p.logger.Warn("natsmq: flush timeout on close", "error", err)
		}
		c.nc.Close()
	}
	return nil
}

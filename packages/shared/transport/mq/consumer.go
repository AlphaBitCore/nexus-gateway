package mq

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// MaxDeliver is the JetStream per-consumer delivery cap applied to every
// durable pull consumer created by Consume. Once a message is delivered this
// many times JetStream removes it for the consumer immediately — MaxAge does
// NOT grant a grace period past exhaustion. Exported as the single source of
// truth so a downstream consumer that dead-letters on its own redelivery
// threshold (the Hub traffic writer's redeliveryThresholdAttempts) can pin that
// threshold STRICTLY BELOW this cap with a compile-time assertion instead of a
// second, drift-prone magic literal.
const MaxDeliver = 5

// fetchMaxBytes bounds a single JetStream pull by BYTES rather than message
// count. The Hub shares one NATS connection across all event consumers, and the
// server drops a connection as a Slow Consumer once its outbound buffer exceeds
// ~64 MiB. Audit frames vary hugely in size — a 1M-context request/response can
// be multi-MB and large single records ship as their own message — so a
// count-based Fetch(N) of large frames could queue far more than 64 MiB on the
// shared connection and wedge the drain (observed: repeated "Slow Consumer
// Detected" + consumer-disconnect EOF loops with rows landing far behind the
// publish rate). A byte bound keeps each pull's in-flight bytes predictable
// regardless of message size; 16 MiB leaves ample headroom under 64 MiB even with
// the db-writer + alerting ai-traffic consumers both pulling at once.
const fetchMaxBytes = 16 << 20 // 16 MiB

// defaultMaxAckPending is the delivered-but-unacked window for the pull consumers.
// Far above JetStream's 1000 default so a high-throughput audit drain is not
// stalled waiting for acks while disk and CPU sit idle. The unacked messages live
// in the stream (memory by default), so the bound is RAM, not disk.
const defaultMaxAckPending = 20000

// maxAckPending returns the configured in-flight window (NEXUS_MQ_MAX_ACK_PENDING),
// defaulting to defaultMaxAckPending. A non-positive / unparseable value keeps the
// default.
func maxAckPending() int {
	if v := strings.TrimSpace(os.Getenv("NEXUS_MQ_MAX_ACK_PENDING")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultMaxAckPending
}

// NATSConsumer implements Consumer using Core NATS (topics) and JetStream (queues).
type NATSConsumer struct {
	nc      *nats.Conn
	js      jetstream.JetStream
	logger  *slog.Logger
	metrics *Metrics

	mu   sync.Mutex
	subs []*nats.Subscription // active Core NATS subscriptions
}

// NewConsumer connects to NATS and initialises JetStream for consuming.
// Shares lifecycle-callback semantics with NewProducer — see
// newConnectionHandlers for details, including the disconnect-duration
// watchdog that escalates Disconnect→WARN to ERROR after threshold.
func NewNATSConsumer(cfg NATSConfig, logger *slog.Logger, metrics *Metrics) (*NATSConsumer, error) {
	onDisconnect, onReconnect, onClosed, onAsyncErr := newConnectionHandlers(
		"consumer", disconnectWatchdogThreshold, logger,
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
		return nil, fmt.Errorf("natsmq: consumer connect %s: %w", cfg.URL, err)
	}

	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("natsmq: consumer jetstream init: %w", err)
	}

	return &NATSConsumer{nc: nc, js: js, logger: logger, metrics: metrics}, nil
}

// Subscribe receives all messages published to a topic via Core NATS (broadcast).
// Blocks until ctx is cancelled.
func (c *NATSConsumer) Subscribe(ctx context.Context, topic string, handler MessageHandler) error {
	sub, err := c.nc.Subscribe(topic, func(nmsg *nats.Msg) {
		msg := &Message{
			Subject:      nmsg.Subject,
			Data:         nmsg.Data,
			Timestamp:    time.Now(),                  // Core NATS carries no server timestamp
			Ack:          func() error { return nil }, // no-op for fire-and-forget
			Nak:          func() error { return nil },
			NakWithDelay: func(time.Duration) error { return nil },
		}
		if err := handler(ctx, msg); err != nil {
			c.logger.Warn("natsmq: topic handler error", "topic", topic, "error", err)
			c.metrics.ErrorsTotal.Inc()
		}
		c.metrics.ConsumedTotal.Inc()
	})
	if err != nil {
		return fmt.Errorf("natsmq: subscribe %s: %w", topic, err)
	}

	c.mu.Lock()
	c.subs = append(c.subs, sub)
	c.mu.Unlock()

	<-ctx.Done()
	_ = sub.Unsubscribe()
	return ctx.Err()
}

// Consume receives messages from a JetStream queue as part of a consumer group.
// Uses a durable pull consumer; blocks until ctx is cancelled.
func (c *NATSConsumer) Consume(ctx context.Context, queue, group string, handler MessageHandler) error {
	stream, err := c.resolveStream(ctx, queue)
	if err != nil {
		return fmt.Errorf("natsmq: resolve stream for %s: %w", queue, err)
	}

	cons, err := stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		// One JetStream durable per (stream, name). Reusing the same group
		// string across nexus.event.* subjects would overwrite FilterSubject on
		// NEXUS_EVENTS and route messages to the wrong handler.
		Durable:       jetstreamDurableName(group, queue),
		FilterSubject: queue,
		AckPolicy:     jetstream.AckExplicitPolicy,
		// MaxDeliver caps total deliveries; once exhausted JetStream removes the
		// message for this consumer immediately — MaxAge does NOT grant a grace
		// period past exhaustion. Consumers that dead-letter on a redelivery cap
		// MUST trip their own threshold strictly below MaxDeliver (see the Hub
		// traffic writer's redeliveryThresholdAttempts) so the DLQ/disk-fallback
		// path still has delivery budget left to retry on.
		MaxDeliver: MaxDeliver,
		AckWait:    30 * time.Second,
		// MaxAckPending bounds how many messages may be delivered-but-unacked at
		// once. The JetStream default is 1000, which throttles a high-throughput
		// drain: with ~14 KB audit frames that is only ~14 MiB in flight, so the
		// pull workers stall waiting for acks long before the disk or CPU is busy
		// (observed: hub at ~55% CPU and disk at ~25% while the stream still backed
		// up and overflowed to drop). A large window lets the byte-bounded fetch
		// (fetchMaxBytes) and the parallel drain workers keep PostgreSQL fed; the
		// in-flight messages live in the (memory) stream, so size it against RAM via
		// NEXUS_MQ_MAX_ACK_PENDING.
		MaxAckPending: maxAckPending(),
	})
	if err != nil {
		return fmt.Errorf("natsmq: create consumer %s/%s: %w", queue, group, err)
	}

	emptyStreak := 0
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		batch, err := cons.FetchBytes(fetchMaxBytes, jetstream.FetchMaxWait(5*time.Second))
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			c.logger.Warn("natsmq: fetch error", "queue", queue, "error", err)
			// Treat a fetch error like an empty round so a broker hiccup backs off
			// instead of hot-looping.
			emptyStreak++
			if !sleepCtx(ctx, idleBackoff(emptyStreak)) {
				return ctx.Err()
			}
			continue
		}

		got := 0
		for nmsg := range batch.Messages() {
			got++
			var ts time.Time
			var numDelivered uint64
			if meta, err := nmsg.Metadata(); err == nil {
				ts = meta.Timestamp
				numDelivered = meta.NumDelivered
			} else {
				ts = time.Now()
			}

			msg := &Message{
				Subject:      nmsg.Subject(),
				Data:         nmsg.Data(),
				Timestamp:    ts,
				NumDelivered: numDelivered,
				Ack:          func() error { return nmsg.Ack() },
				Nak:          func() error { return nmsg.Nak() },
				NakWithDelay: func(d time.Duration) error { return nmsg.NakWithDelay(d) },
			}

			err := handler(ctx, msg)
			switch {
			case err == nil:
				if ackErr := nmsg.Ack(); ackErr != nil {
					c.logger.Error("natsmq: ack failed", "error", ackErr)
					c.metrics.ErrorsTotal.Inc()
				} else {
					c.metrics.AckedTotal.Inc()
				}
			case IsDeferAck(err):
				// Handler owns ack/nak; do nothing here.
				c.metrics.DeferredTotal.Inc()
			default:
				c.logger.Warn("natsmq: queue handler error, naking",
					"queue", queue, "group", group, "error", err)
				if nakErr := nmsg.Nak(); nakErr != nil {
					c.logger.Error("natsmq: nak failed", "error", nakErr)
				}
				c.metrics.NakedTotal.Inc()
			}
			c.metrics.ConsumedTotal.Inc()
		}

		if err := batch.Error(); err != nil && ctx.Err() == nil {
			c.logger.Warn("natsmq: batch error", "queue", queue, "error", err)
		}

		// Adaptive idle backoff: a consumer whose subject is quiet (e.g. the
		// low-frequency JWT-revocation topic) otherwise hot-polls a 16 MiB pull
		// every fetch window, churning allocations into a multi-core GC storm on an
		// otherwise idle process. Backing off when a round returns nothing drops an
		// idle consumer to ~zero CPU, while a busy subject (the audit drain — always
		// has data) keeps emptyStreak at 0 and never sleeps, so throughput is
		// unchanged. Revocation tolerates the added seconds of delivery latency.
		if got == 0 {
			emptyStreak++
			if !sleepCtx(ctx, idleBackoff(emptyStreak)) {
				return ctx.Err()
			}
		} else {
			emptyStreak = 0
		}
	}
}

// Close drains and closes the NATS connection, stopping all subscriptions.
func (c *NATSConsumer) Close() error {
	c.mu.Lock()
	for _, sub := range c.subs {
		_ = sub.Unsubscribe()
	}
	c.subs = nil
	c.mu.Unlock()

	c.nc.Close()
	return nil
}

// resolveStream maps a queue subject to its JetStream stream name.
// Streams are created by Hub's EnsureStreams at startup.
func (c *NATSConsumer) resolveStream(ctx context.Context, queue string) (jetstream.Stream, error) {
	name := streamName(queue)
	s, err := c.js.Stream(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("stream %q not found (run EnsureStreams at Hub startup): %w", name, err)
	}
	return s, nil
}

// StreamFillFraction reports the named stream's used bytes as a fraction of its
// MaxBytes cap (0..1), with ok=false on any query error or when the stream is
// uncapped (MaxBytes <= 0, no meaningful fraction). The audit drain uses it to
// override its CPU-yield throttle to full-speed as the backlog approaches the cap,
// turning the sustained-saturation wedge into bounded catch-up. Cheap enough to
// call on a low-frequency timer; callers should not call it per message.
func (c *NATSConsumer) StreamFillFraction(ctx context.Context, stream string) (float64, bool) {
	s, err := c.js.Stream(ctx, stream)
	if err != nil {
		return 0, false
	}
	info, err := s.Info(ctx)
	if err != nil {
		return 0, false
	}
	maxBytes := info.Config.MaxBytes
	if maxBytes <= 0 {
		return 0, false // uncapped stream → no fill fraction
	}
	return float64(info.State.Bytes) / float64(maxBytes), true
}

// jetstreamDurableName builds a unique JetStream durable consumer name for
// this (consumer group, subject) pair. NATS enforces one durable definition
// per name on a stream; sharing only `group` across nexus.event.* filters
// causes CreateOrUpdateConsumer to clobber FilterSubject.
func jetstreamDurableName(group, queue string) string {
	if group == "" {
		group = "mq-consumer"
	}
	slug := strings.ReplaceAll(queue, ".", "_")
	return group + "__" + slug
}

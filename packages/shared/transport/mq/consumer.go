package mq

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

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
			Subject:   nmsg.Subject,
			Data:      nmsg.Data,
			Timestamp: time.Now(),                  // Core NATS carries no server timestamp
			Ack:       func() error { return nil }, // no-op for fire-and-forget
			Nak:       func() error { return nil },
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
		MaxDeliver:    5,
		AckWait:       30 * time.Second,
	})
	if err != nil {
		return fmt.Errorf("natsmq: create consumer %s/%s: %w", queue, group, err)
	}

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		batch, err := cons.Fetch(10, jetstream.FetchMaxWait(5*time.Second))
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			c.logger.Warn("natsmq: fetch error", "queue", queue, "error", err)
			continue
		}

		for nmsg := range batch.Messages() {
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

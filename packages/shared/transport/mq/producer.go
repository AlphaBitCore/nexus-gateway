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
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// NATSProducer implements Producer using Core NATS (topics) and JetStream (queues).
type NATSProducer struct {
	nc      *nats.Conn
	js      jetstream.JetStream
	logger  *slog.Logger
	metrics *Metrics
}

// NewProducer connects to NATS and initialises JetStream. Lifecycle
// callbacks are factored into newConnectionHandlers — see that helper for
// semantics (Disconnect→WARN + watchdog timer, Reconnect→INFO + cancel,
// Closed→ERROR, AsyncErr→ERROR, sustained-Disconnect→ERROR after threshold).
func NewNATSProducer(cfg NATSConfig, logger *slog.Logger, metrics *Metrics) (*NATSProducer, error) {
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
		return nil, fmt.Errorf("natsmq: connect %s: %w", cfg.URL, err)
	}

	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("natsmq: jetstream init: %w", err)
	}

	return &NATSProducer{nc: nc, js: js, logger: logger, metrics: metrics}, nil
}

// Publish sends a message to a topic using Core NATS (fire-and-forget, broadcast).
func (p *NATSProducer) Publish(_ context.Context, topic string, data []byte) error {
	if err := p.nc.Publish(topic, data); err != nil {
		p.metrics.ErrorsTotal.Inc()
		return fmt.Errorf("natsmq: publish %s: %w", topic, err)
	}
	p.metrics.PublishedTotal.Inc()
	return nil
}

// Enqueue sends a message to a queue using JetStream (persistent, at-least-once).
func (p *NATSProducer) Enqueue(ctx context.Context, queue string, data []byte) error {
	if _, err := p.js.Publish(ctx, queue, data); err != nil {
		p.metrics.ErrorsTotal.Inc()
		return fmt.Errorf("natsmq: enqueue %s: %w", queue, err)
	}
	p.metrics.EnqueuedTotal.Inc()
	return nil
}

// Close flushes pending messages and closes the NATS connection.
func (p *NATSProducer) Close() error {
	if err := p.nc.FlushTimeout(5 * time.Second); err != nil {
		p.logger.Warn("natsmq: flush timeout on close", "error", err)
	}
	p.nc.Close()
	return nil
}

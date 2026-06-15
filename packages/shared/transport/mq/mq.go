// Package mq provides a pluggable message queue abstraction with two
// messaging patterns: topic (broadcast) and queue (competing consumers).
//
// Interfaces live in this root package; implementations are in sub-packages
// (nats, memory). Services depend only on the interfaces here.
package mq

import (
	"context"
	"time"
)

// Producer sends messages to topics (broadcast) and queues (competing consumers).
type Producer interface {
	// Publish sends a message to a topic. All subscribers receive it.
	// Fire-and-forget: no persistence, no delivery guarantee.
	Publish(ctx context.Context, topic string, data []byte) error

	// Enqueue sends a message to a queue. One consumer in the group receives it.
	// Persistent: messages survive consumer restarts and are delivered at-least-once.
	Enqueue(ctx context.Context, queue string, data []byte) error

	// Close flushes pending messages and releases resources.
	Close() error
}

// Consumer receives messages from topics and queues.
type Consumer interface {
	// Subscribe receives all messages published to a topic (broadcast).
	// The handler is called for each message. Blocks until ctx is cancelled.
	Subscribe(ctx context.Context, topic string, handler MessageHandler) error

	// Consume receives messages from a queue as part of a consumer group.
	// Messages are distributed among consumers in the same group (competing).
	// Blocks until ctx is cancelled.
	Consume(ctx context.Context, queue string, group string, handler MessageHandler) error

	// Close stops all active subscriptions and releases resources.
	Close() error
}

// MessageHandler processes a received message.
//
//   - Return nil: the consumer auto-acks.
//   - Return mq.ErrDeferAck: the handler will call msg.Ack() or msg.Nak()
//     later. Consumer does not auto-ack.
//   - Return any other error: the consumer auto-naks for redelivery.
type MessageHandler func(ctx context.Context, msg *Message) error

// Message represents a received message.
type Message struct {
	// Subject is the topic or queue name the message was received from.
	Subject string

	// Data is the raw message payload.
	Data []byte

	// Timestamp is when the message was published.
	Timestamp time.Time

	// NumDelivered is the broker's count of how many times this exact
	// message has been delivered to a consumer (1 = first delivery, 2+ =
	// redelivery after a previous Nak / ack timeout). Consumers use it to
	// detect poison-pill messages that should land on the dead-letter
	// queue rather than redeliver-forever. Zero when the underlying
	// broker doesn't expose it (best-effort).
	NumDelivered uint64

	// Ack confirms successful processing. Called automatically if the handler
	// returns nil. Callers may call it explicitly to ack before returning.
	Ack func() error

	// Nak rejects the message for redelivery. Called automatically if the
	// handler returns an error. Callers may call it explicitly to nak before
	// returning.
	Nak func() error

	// NakWithDelay rejects the message for redelivery but instructs the broker
	// to wait at least the given delay before re-delivering it. A bare Nak
	// re-delivers as fast as the broker can, which burns the per-message
	// MaxDeliver budget in a tight loop during a sustained downstream outage
	// (e.g. a DB failover) — consumers that DLQ on a redelivery cap should use
	// NakWithDelay so the budget spans the outage instead of being exhausted in
	// seconds. Best-effort: brokers that cannot honour a per-message delay fall
	// back to a plain Nak. Non-nil for the JetStream queue path; a no-op for the
	// fire-and-forget topic path.
	NakWithDelay func(delay time.Duration) error
}

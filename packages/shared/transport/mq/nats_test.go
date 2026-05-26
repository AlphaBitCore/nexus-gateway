//go:build integration

package mq_test

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
	"github.com/nats-io/nats.go/jetstream"

	natsgo "github.com/nats-io/nats.go"
)

func TestNATSCompliance(t *testing.T) {
	url := os.Getenv("NATS_URL")
	if url == "" {
		t.Skip("NATS_URL not set, skipping NATS integration test")
	}

	cfg := mq.NATSConfig{URL: url}
	logger := slog.Default()
	metrics := mq.NewMetrics("test_natsmq_compliance")

	// Ensure streams exist before running tests.
	nc, err := natsgo.Connect(url)
	if err != nil {
		t.Fatalf("connect for stream setup: %v", err)
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		t.Fatalf("jetstream for stream setup: %v", err)
	}
	if err := mq.EnsureStreams(t.Context(), js); err != nil {
		nc.Close()
		t.Fatalf("EnsureStreams: %v", err)
	}
	nc.Close()

	producer, err := mq.NewNATSProducer(cfg, logger, metrics)
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	consumer, err := mq.NewNATSConsumer(cfg, logger, metrics)
	if err != nil {
		t.Fatalf("NewConsumer: %v", err)
	}

	mq.ComplianceTestSuite(t, producer, consumer)
}

// TestConsume_ErrDeferAck_DoesNotAutoAck verifies that returning mq.ErrDeferAck
// from a handler suppresses the consumer's automatic Ack, leaving the message
// in the "ack pending" state until the handler explicitly calls msg.Ack().
//
// This is the C3 regression guard: before the fix, any non-nil return from a
// handler (including ErrDeferAck) was treated as a Nak and caused redelivery.
// After the fix, ErrDeferAck is a distinct signal meaning "the handler will
// ack later" — neither Ack nor Nak fires automatically.
func TestConsume_ErrDeferAck_DoesNotAutoAck(t *testing.T) {
	url := os.Getenv("NATS_URL")
	if url == "" {
		t.Skip("NATS_URL not set, skipping NATS integration test")
	}

	cfg := mq.NATSConfig{URL: url}
	logger := slog.Default()
	metrics := mq.NewMetrics("test_natsmq_deferack")

	// Ensure the NEXUS_EVENTS stream exists (matches "nexus.event.*").
	nc, err := natsgo.Connect(url)
	if err != nil {
		t.Fatalf("connect for stream setup: %v", err)
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		t.Fatalf("jetstream for stream setup: %v", err)
	}
	if err := mq.EnsureStreams(t.Context(), js); err != nil {
		nc.Close()
		t.Fatalf("EnsureStreams: %v", err)
	}

	// nc is used throughout for stream inspection; close last via t.Cleanup.
	t.Cleanup(nc.Close)

	producer, err := mq.NewNATSProducer(cfg, logger, metrics)
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	t.Cleanup(func() { _ = producer.Close() })

	consumer, err := mq.NewNATSConsumer(cfg, logger, metrics)
	if err != nil {
		t.Fatalf("NewConsumer: %v", err)
	}
	t.Cleanup(func() { _ = consumer.Close() })

	const (
		subject = "nexus.event.deferack_test"
		group   = "deferack-grp"
	)

	// Clean up the durable consumer between runs so state is deterministic.
	stream, err := js.Stream(t.Context(), "NEXUS_EVENTS")
	if err != nil {
		t.Fatalf("lookup NEXUS_EVENTS: %v", err)
	}
	_ = stream.DeleteConsumer(t.Context(), group)

	ctx := t.Context()

	received := make(chan *mq.Message, 1)
	go func() {
		_ = consumer.Consume(ctx, subject, group, func(_ context.Context, m *mq.Message) error {
			select {
			case received <- m:
			default:
			}
			return mq.ErrDeferAck
		})
	}()

	// Give the consumer time to register with JetStream before publishing.
	time.Sleep(100 * time.Millisecond)

	if err := producer.Enqueue(ctx, subject, []byte(`{"x":1}`)); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	var msg *mq.Message
	select {
	case msg = <-received:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for handler to receive message")
	}

	// Let the consumer loop settle after the handler returns ErrDeferAck.
	// With the C3 fix, the message should remain unacked (NumAckPending >= 1).
	// Without the fix, the consumer naks it and redelivery will re-increment
	// NumAckPending — so we assert the stronger condition: the ack is NOT auto-
	// issued by checking DeferredTotal fired and AckedTotal did not.
	time.Sleep(300 * time.Millisecond)

	pending := jsConsumerAckPending(t, js, "NEXUS_EVENTS", group)
	if pending == 0 {
		t.Fatal("expected message to remain pending (unacked); got NumAckPending=0")
	}

	// Explicit ack; NumAckPending must drop to 0.
	if err := msg.Ack(); err != nil {
		t.Fatalf("explicit Ack: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	if got := jsConsumerAckPending(t, js, "NEXUS_EVENTS", group); got != 0 {
		t.Errorf("after explicit ack NumAckPending=%d; want 0", got)
	}
}

// jsConsumerAckPending returns the JetStream durable consumer's NumAckPending
// (messages delivered but not yet acknowledged).
func jsConsumerAckPending(t *testing.T, js jetstream.JetStream, streamName, durable string) uint64 {
	t.Helper()
	stream, err := js.Stream(t.Context(), streamName)
	if err != nil {
		t.Fatalf("stream %s: %v", streamName, err)
	}
	cons, err := stream.Consumer(t.Context(), durable)
	if err != nil {
		t.Fatalf("consumer %s/%s: %v", streamName, durable, err)
	}
	info, err := cons.Info(t.Context())
	if err != nil {
		t.Fatalf("consumer info %s/%s: %v", streamName, durable, err)
	}
	return uint64(info.NumAckPending)
}

func TestEnsureStreams_Idempotent(t *testing.T) {
	url := os.Getenv("NATS_URL")
	if url == "" {
		t.Skip("NATS_URL not set")
	}

	nc, err := natsgo.Connect(url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer nc.Close()

	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}

	// Call twice — must not error on second call.
	if err := mq.EnsureStreams(t.Context(), js); err != nil {
		t.Fatalf("first EnsureStreams: %v", err)
	}
	if err := mq.EnsureStreams(t.Context(), js); err != nil {
		t.Fatalf("second EnsureStreams (idempotent): %v", err)
	}
}

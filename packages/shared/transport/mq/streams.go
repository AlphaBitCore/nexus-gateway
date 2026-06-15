package mq

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Setup connects to NATS, calls EnsureStreams to create required JetStream
// streams, and disconnects. Intended for Hub startup before any
// producer/consumer is active. Short-lived connection — does not persist.
func Setup(ctx context.Context, natsURL string) error {
	nc, err := nats.Connect(natsURL)
	if err != nil {
		return fmt.Errorf("natsmq: setup connect %s: %w", natsURL, err)
	}
	defer nc.Close()

	js, err := jetstream.New(nc)
	if err != nil {
		return fmt.Errorf("natsmq: setup jetstream init: %w", err)
	}

	return EnsureStreams(ctx, js)
}

// EnsureStreams creates the required JetStream streams if they do not exist.
// Idempotent — safe to call on every startup (uses CreateOrUpdateStream).
// Should be called once at Hub startup before any producer/consumer is active.
func EnsureStreams(ctx context.Context, js jetstream.JetStream) error {
	streams := []jetstream.StreamConfig{
		{
			// NEXUS_EVENTS: all traffic and audit events from ai-gateway,
			// compliance-proxy, agent, and control-plane (admin audit).
			// InterestPolicy: messages retained until ALL defined consumers
			// have acked — enables multiple consumer groups (hub-db-writer +
			// hub-alerting) to each receive every message (Kafka-style fan-out).
			// MaxBytes 8 GiB: covers ~8h of sustained perf-test load at
			// ~1 GiB/h while staying well within the 7.6 GiB host RAM so
			// FileStorage does not pressure the kernel page cache against
			// postgres / Go heap. Pair with server-level
			// js_max_file_store: 32GB in /etc/nats/nats-server.conf.
			// DiscardOld: a stalled consumer cannot pin the stream and
			// trigger NATS "insufficient_resources" publish errors.
			// MaxAge 6h: with healthy consumer drainage,
			// events older than 6h are already written to traffic_event /
			// admin_audit; shorter MaxAge means a wedged consumer
			// auto-recovers faster once the wedge is fixed.
			Name:      "NEXUS_EVENTS",
			Subjects:  []string{"nexus.event.>"},
			Retention: jetstream.InterestPolicy,
			MaxAge:    6 * time.Hour,
			MaxBytes:  8 * 1024 * 1024 * 1024,
			Discard:   jetstream.DiscardOld,
			Storage:   jetstream.FileStorage,
		},
		{
			// NEXUS_AUTH: auth-plane events (token revocation today, room for
			// future auth coordination subjects). InterestPolicy so every RS
			// replica's consumer group receives every event independently.
			Name:      "NEXUS_AUTH",
			Subjects:  []string{"nexus.auth.>"},
			Retention: jetstream.InterestPolicy,
			MaxAge:    24 * time.Hour,
			MaxBytes:  256 * 1024 * 1024,
			Discard:   jetstream.DiscardOld,
			Storage:   jetstream.FileStorage,
		},
	}

	for _, cfg := range streams {
		if _, err := js.CreateOrUpdateStream(ctx, cfg); err != nil {
			return fmt.Errorf("natsmq: ensure stream %s: %w", cfg.Name, err)
		}
	}
	return nil
}

// streamName maps a queue subject to its JetStream stream name.
//
//	nexus.event.* → NEXUS_EVENTS
//	nexus.auth.*  → NEXUS_AUTH
//	(other)       → NEXUS_DEFAULT
func streamName(queue string) string {
	switch {
	case strings.HasPrefix(queue, "nexus.event."):
		return "NEXUS_EVENTS"
	case strings.HasPrefix(queue, "nexus.auth."):
		return "NEXUS_AUTH"
	default:
		return "NEXUS_DEFAULT"
	}
}

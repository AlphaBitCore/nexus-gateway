package mq

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// defaultEventsMaxBytes is the fallback NEXUS_EVENTS cap used only when total RAM
// cannot be read (non-Linux / restricted /proc). The normal default is auto-sized
// to a fraction of RAM (see eventsMaxBytesAuto). The stream is the audit
// side-path's burst buffer (the producer publishes full-speed and the Hub drains
// lazily), so it is sized to absorb a long burst.
const defaultEventsMaxBytes int64 = 8 * 1024 * 1024 * 1024 // 8 GiB

// eventsMaxBytesRAMFraction is the share of total RAM the NEXUS_EVENTS burst
// buffer auto-sizes to when NEXUS_EVENTS_MAX_BYTES is unset or "auto".
const eventsMaxBytesRAMFraction = 0.15

// meminfoPath is the kernel memory-stats file totalMemoryBytes parses. A package
// var (not a literal) so a test can point it at a fixture and exercise auto-sizing
// on a non-Linux host. Production never reassigns it.
var meminfoPath = "/proc/meminfo"

// parseByteSize parses a human byte size ("8GB", "512MB", "1073741824") into
// bytes. Returns fallback for an empty or unparseable value. Recognises KB/MB/GB
// (powers of 1024) and a bare integer (bytes); case-insensitive, trailing "B"
// optional.
func parseByteSize(s string, fallback int64) int64 {
	s = strings.TrimSpace(strings.ToUpper(s))
	if s == "" {
		return fallback
	}
	mult := int64(1)
	switch {
	case strings.HasSuffix(s, "GB"):
		mult, s = 1024*1024*1024, strings.TrimSuffix(s, "GB")
	case strings.HasSuffix(s, "MB"):
		mult, s = 1024*1024, strings.TrimSuffix(s, "MB")
	case strings.HasSuffix(s, "KB"):
		mult, s = 1024, strings.TrimSuffix(s, "KB")
	case strings.HasSuffix(s, "B"):
		s = strings.TrimSuffix(s, "B")
	}
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || n <= 0 {
		return fallback
	}
	return n * mult
}

// eventsMaxBytes returns the NEXUS_EVENTS stream cap. Default (unset or "auto") is
// auto-sized to eventsMaxBytesRAMFraction of total RAM; an explicit size
// ("32GB", "1073741824") overrides. Reads NEXUS_EVENTS_MAX_BYTES first, then the
// NEXUS_STREAM_MAX_BYTES alias. When auto-sizing it logs a WARN with the chosen
// value and how to pin it, so operators see what the box defaulted to.
func eventsMaxBytes() int64 {
	v := strings.TrimSpace(os.Getenv("NEXUS_EVENTS_MAX_BYTES"))
	if v == "" {
		v = strings.TrimSpace(os.Getenv("NEXUS_STREAM_MAX_BYTES"))
	}
	if v != "" && !strings.EqualFold(v, "auto") {
		return parseByteSize(v, eventsMaxBytesAuto())
	}
	n := eventsMaxBytesAuto()
	slog.Warn("NEXUS_EVENTS audit-stream cap auto-sized from total RAM",
		"chosen_bytes", n,
		"chosen_gib", float64(n)/(1<<30),
		"ram_fraction", eventsMaxBytesRAMFraction,
		"override", "set NEXUS_EVENTS_MAX_BYTES=<size> (e.g. 32GB) in the service env "+
			"(/etc/nexus/nexus.env or your deployment env) to pin a fixed value")
	return n
}

// eventsMaxBytesAuto sizes the NEXUS_EVENTS cap to a fraction of total RAM, with a
// 1 GiB floor so a small box still gets a usable burst buffer. Falls back to the
// fixed defaultEventsMaxBytes when total RAM cannot be read.
func eventsMaxBytesAuto() int64 {
	total := totalMemoryBytes()
	if total == 0 {
		return defaultEventsMaxBytes
	}
	n := int64(float64(total) * eventsMaxBytesRAMFraction)
	const floor = 1 << 30 // 1 GiB
	if n < floor {
		n = floor
	}
	return n
}

// totalMemoryBytes reads MemTotal from meminfoPath (Linux /proc/meminfo). Returns
// 0 when it cannot be read (non-Linux / restricted), so callers fall back to a
// fixed default rather than guessing the machine wrong.
func totalMemoryBytes() uint64 {
	f, err := os.Open(meminfoPath)
	if err != nil {
		return 0
	}
	defer f.Close() //nolint:errcheck
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		fields := strings.Fields(line) // "MemTotal:" "<kB>" "kB"
		if len(fields) >= 2 {
			if kb, err := strconv.ParseUint(fields[1], 10, 64); err == nil {
				return kb * 1024
			}
		}
		return 0
	}
	return 0
}

// eventsStorage selects the NEXUS_EVENTS stream's storage tier. Default is
// MEMORY: the audit stream is a delay-tolerant burst buffer (the producer
// publishes full-speed, the Hub drains lazily to PostgreSQL), so on a single box
// whose disk write bandwidth is the bottleneck, keeping that buffer in RAM frees
// the disk for the durable PG + WAL writes instead of writing every ~18 KB audit
// message to the volume twice.
//
// No-loss SCOPE (read carefully — this is not unconditional no-loss): at the
// MaxBytes cap the DiscardNew policy fails NEW publishes, which the producer routes
// to its durable on-disk NDJSON spill — so the stream-full overflow is no-loss. But
// with the in-memory tier a NATS broker restart/crash drops records the producer
// already published-and-reclaimed (they are in RAM, not in the producer spill, and
// not yet drained to PG). The overflow→spill path does NOT cover that broker-bounce
// window. NEXUS_EVENTS_STORAGE=file selects a durable file-backed stream that
// survives a broker restart, at the cost of the steady-state disk writes.
func eventsStorage() jetstream.StorageType {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("NEXUS_EVENTS_STORAGE")), "file") {
		return jetstream.FileStorage
	}
	return jetstream.MemoryStorage
}

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
			// MaxBytes (env NEXUS_EVENTS_MAX_BYTES, default 8 GiB): the audit
			// side-path's burst buffer on local disk. The producer publishes
			// full-speed and the Hub drains lazily (delay-tolerant), so the stream
			// is sized to absorb a long burst; raise it on a large disk. Pair with
			// server-level js_max_file_store in nats-server.conf.
			// DiscardNew (NOT DiscardOld): durable audit is a no-loss product +
			// compliance requirement, so the stream must NEVER silently discard an
			// un-acked message. At MaxBytes, NEW publishes fail with a resource
			// error, which the producer routes to its durable on-disk spill (see
			// the audit Writer's handlePublishFailure) — no silent loss, and the
			// request path is not blocked (the publish happens on the async drain).
			// DiscardOld would instead drop the OLDEST un-acked audit rows the
			// moment a slow/stalled consumer let the stream reach the cap.
			// MaxAge 6h: with healthy drainage, older events are already in
			// traffic_event / admin_audit; a shorter MaxAge auto-recovers a wedge.
			Name:      "NEXUS_EVENTS",
			Subjects:  []string{"nexus.event.>"},
			Retention: jetstream.InterestPolicy,
			MaxAge:    6 * time.Hour,
			MaxBytes:  eventsMaxBytes(),
			Discard:   jetstream.DiscardNew,
			Storage:   eventsStorage(),
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
		if err := ensureStream(ctx, js, cfg); err != nil {
			return fmt.Errorf("natsmq: ensure stream %s: %w", cfg.Name, err)
		}
	}
	return nil
}

// ensureStream creates or updates one stream, handling the one config field
// JetStream treats as immutable: storage tier. CreateOrUpdateStream cannot change
// an existing stream's storage type (it returns "stream configuration update can
// not change storage type"), so a deployment that flips NEXUS_EVENTS_STORAGE
// against an already-created stream would otherwise fail Hub startup. The audit
// stream is a transient burst buffer — the durable stores are PostgreSQL and the
// producer's on-disk spill — so when the desired storage differs from the live
// stream we delete and recreate it, dropping only the un-drained in-flight buffer.
// That is acceptable for a deliberate operator storage switch and is logged loudly.
func ensureStream(ctx context.Context, js jetstream.JetStream, cfg jetstream.StreamConfig) error {
	if existing, err := js.Stream(ctx, cfg.Name); err == nil {
		if info, ierr := existing.Info(ctx); ierr == nil && info.Config.Storage != cfg.Storage {
			slog.Warn("NEXUS stream storage tier changed; deleting and recreating "+
				"(un-drained in-flight buffer is dropped — durable copies remain in "+
				"PostgreSQL and the producer spill)",
				"stream", cfg.Name,
				"from", info.Config.Storage.String(),
				"to", cfg.Storage.String())
			if derr := js.DeleteStream(ctx, cfg.Name); derr != nil {
				return fmt.Errorf("delete for storage-tier change: %w", derr)
			}
		}
	}
	_, err := js.CreateOrUpdateStream(ctx, cfg)
	return err
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

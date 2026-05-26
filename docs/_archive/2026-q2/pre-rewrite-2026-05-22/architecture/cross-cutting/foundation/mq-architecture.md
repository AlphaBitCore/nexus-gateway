---
doc: mq-architecture
area: cross-cutting
service: foundation
tier: 1
updated: 2026-05-20
---

# MQ Architecture

> **Tier 1 architecture doc.** Read this before touching `packages/shared/transport/mq/**`, any service's MQ producer or consumer, the NATS JetStream subject layout, or the MQ ↔ HTTP/WS decision for a new event type.

MQ at Nexus is **NATS JetStream** today, behind a pluggable interface (`packages/shared/transport/mq`). The MQ carries **bulk events** — traffic, audit, ops metrics, alerts. It is deliberately **not** the config-sync transport (that is Hub WebSocket + HTTP, documented in `thing-config-sync-architecture.md`).

---

## 1. The `shared/mq` interface

Two role-based interfaces (`packages/shared/transport/mq/mq.go:14-40`), with two messaging patterns each — **topic** (broadcast) and **queue** (competing consumers):

```go
type Producer interface {
    // Publish broadcasts to a topic; fire-and-forget, no persistence guarantee.
    Publish(ctx context.Context, topic string, data []byte) error
    // Enqueue places on a queue; at-least-once delivery, persistent.
    Enqueue(ctx context.Context, queue string, data []byte) error
    Close() error
}

type Consumer interface {
    // Subscribe receives every message on a topic (broadcast).
    Subscribe(ctx context.Context, topic string, handler MessageHandler) error
    // Consume reads from a queue as part of a consumer group (competing).
    Consume(ctx context.Context, queue string, group string, handler MessageHandler) error
    Close() error
}

type MessageHandler func(ctx context.Context, msg *Message) error
```

Headers / Ack / Driver types do not exist at the interface boundary — explicit `Ack()` / `Nak()` callbacks ride on the `Message` struct, used only when a handler returns `mq.ErrDeferAck`. There is no separate `Driver` factory type; the NATS JetStream implementation lives inline in the same `mq` package (`factory.go`, `streams.go`, `producer.go`, `consumer.go`). No `mq/natsmq` or `mq/memmq` subpackage today; integration tests spin up a real NATS server in-process (`nats_test.go`).

The interface intentionally hides JetStream-specific concepts (delivery policies, max-in-flight, ack-wait windows) — driver-specific tuning happens inside the package, never at call sites.

## 2. Stream layout (NATS JetStream)

Two streams are provisioned by `EnsureStreams` in `packages/shared/transport/mq/streams.go`:

| Stream | Subjects | Retention | MaxBytes |
|---|---|---|---|
| `NEXUS_EVENTS` | `nexus.event.>` | `InterestPolicy`, `MaxAge = 6h` | 8 GiB |
| `NEXUS_AUTH` | `nexus.auth.>` | `InterestPolicy`, `MaxAge = 24h` | 256 MiB |

Anything that doesn't match `nexus.event.*` or `nexus.auth.*` lands on the fallback `NEXUS_DEFAULT` stream via `streamName()` (`streams.go:108-117`).

## 3. Subject taxonomy

The actual subjects used in code today (all under `nexus.event.>`):

| Subject | Emitter | Sink in Hub |
|---|---|---|
| `nexus.event.ai-traffic` | AI Gateway | traffic-event writer → `traffic_event` |
| `nexus.event.compliance` | Compliance Proxy | traffic-event writer → `traffic_event` |
| `nexus.event.agent` | Agent forwarder (via Hub `AuditUpload` HTTP fallback re-enqueue) | traffic-event writer → `traffic_event` |
| `nexus.event.admin-audit` | Control Plane | AdminAuditLog writer → `AdminAuditLog` (hash-chained) |
| `nexus.event.diag` | Any service (slog sink) | diag writer → `ThingDiagEvent` |
| `nexus.event.alert` | Hub alert raiser | alert dispatcher → channels |
| `nexus.event.exemption` | Hub (`internal_things.go`) | (no Hub-side consumer today — re-enqueued for future downstream replay) |

The auth-plane subject family (`nexus.auth.*`) carries token revocation events: `nexus.auth.revocation` (`packages/control-plane/internal/identity/authserver/revocation/publisher.go`) consumed by the JWT verifier's `MQRevocationChecker` (cross-ref `jwt-verifier-architecture.md` §6).

The Hub-side consumer-to-source map is encoded in `packages/nexus-hub/internal/alerts/eval/engine.go:33-36` (alert evaluator source list) and `packages/nexus-hub/internal/jobs/consumer/siem.go` `SIEMQueues` (forwarder source list).

## 4. Consumer groups (Hub-coordinated)

The Hub owns the durable consumers for each stream. Consumers run in `packages/nexus-hub/internal/jobs/consumer/` and dispatch by subject:

- Traffic writer — subscribes `nexus.event.ai-traffic`, `nexus.event.compliance`, `nexus.event.agent`; writes `traffic_event` + handles spill refs.
- AdminAuditLog writer — subscribes `nexus.event.admin-audit`; writes `AdminAuditLog` (hash-chained — see `audit-pipeline-architecture.md`).
- Alert eval engine — subscribes the same four `nexus.event.*` subjects as data sources for the rule aggregators.
- SIEM bridge consumer — subscribes the same four `nexus.event.*` subjects to fan events out to external SIEMs.

Each consumer is a pull consumer (not push) so we control backpressure. On message handler failure, the consumer NAKs and retries; persistent failures surface via the stream-lag alert rules.

## 5. Dual-write & dedup

Some event types are emitted via **both** MQ and HTTP fallback (when MQ is unreachable from the emitting Thing — e.g., an agent on a flaky network). Dedup is required so the Hub sink does not double-write:

- Every event carries a stable `id` field generated by the emitter (`TrafficEventMessage.ID` at `packages/shared/transport/mq/messages.go:14`).
- Each Hub-side INSERT uses `ON CONFLICT (id) DO NOTHING` (`packages/nexus-hub/internal/jobs/consumer/traffic.go`) — the PK doubles as the dedup key. JetStream's `MaxDeliver: 5` + `AckExplicitPolicy` (`packages/shared/transport/mq/consumer.go:94-103`) caps redelivery storms; the ON CONFLICT catches anything that still slips through (e.g. dual-write via HTTP fallback).
- There is no shared `dedup` Go package today.
- The metric counter dedup at `packages/shared/core/metrics/registry/dedup.go` is a different layer entirely (per-Thing telemetry rate-limit), not MQ message dedup.

**Do not skip the natural-key UNIQUE on a dual-write path** — without it duplicate rows are silently committed and downstream rollups double-count.

## 6. MQ vs HTTP/WS — the decision

A persistent question: when does an event go via MQ vs HTTP vs WS?

| Carrier | Use for | Why |
|---|---|---|
| **Hub WS change-signal** | Config-sync notification | Low latency, small payload, server-to-Thing |
| **HTTP (CP ↔ Hub)** | Admin CRUD, shadow read/write, body overflow presign | Synchronous, request/response semantics |
| **HTTP fallback (Thing → Hub)** | Audit upload when MQ unreachable | Last-resort durability |
| **MQ** | Bulk events (traffic, audit, ops metrics, alerts) | High throughput, decoupled producer/consumer, durable replay |

There is **one specific exception** in production today: `metrics_sample` payloads go over the WS link from Thing to Hub, not MQ. This is deliberate — the samples are KB-scale and benefit from the WS link's low-latency back-channel. Memory `feedback_metrics_sample_ws_is_fine` records the binding: **do not migrate `metrics_sample` to HTTP without observed outbox drops in prod**. The theoretical bulk-data concern doesn't hold for these payloads; prod shows zero drops across all four server Things.

## 7. Message envelopes

There is no single universal envelope; each event type has its own typed Go struct under `packages/shared/transport/mq/messages.go` (traffic events, agent-audit events, admin-audit events, diag events, etc.). Common fields across most envelopes include `traceId`, `nexusRequestId`, `thingId`, and `emittedAt`; the per-type payload follows. Consumers must tolerate unknown fields so new fields can be added without breaking older readers.

## 8. Failure modes & backpressure

| Failure | Behavior |
|---|---|
| Stream not provisioned at boot | Hub auto-provisions on start; consumers wait until ready. |
| Consumer falls behind | Lag metric (per-consumer) → alert; stream retention is sized for 24 h hot so a few hours of lag is recoverable. |
| Consumer panics | Recover + log + NAK; after N consecutive panics, consumer is quarantined and an alert fires. |
| Producer can't reach NATS | For agent: HTTP fallback (`POST /api/internal/things/audit` → Hub `AuditUpload` → Hub re-enqueues onto MQ). Other services log the failure and drop; there is no service-side persistent outbox today. |
| Duplicate from dual-write | Dedup (§5). |
| Repeated handler failure | After `MaxDeliver: 5` JetStream stops redelivery (the message is effectively dropped — no DLQ stream today). Manual replay tooling lives in `tools/db-migrate/manual-scripts/` for retroactive backfills (used for the 2026-05-14 audit-gap incident). |

## 9. Driver seam

The `Driver` / `Producer` / `Consumer` interfaces in `packages/shared/transport/mq/mq.go` are the seam between MQ-using code and the NATS implementation. Consumers of `shared/mq` should depend on the interfaces, not on JetStream types — `main.go` (driver wiring) is the only place that imports JetStream-specific symbols. This preserves the option to add an in-memory or alternative driver later without touching call sites.

## 10. Adding a new stream / subject

Checklist:

1. Add the subject convention to §3 above.
2. Declare the stream in `packages/shared/transport/mq/streams.go` (retention, max-msgs, max-bytes).
3. If consumed in Hub, wire the consumer + sink in `packages/nexus-hub/internal/jobs/consumer/`.
4. Add Prometheus counters: published count, ack lag, NAK count, DLQ depth.
5. Add an alert rule for DLQ depth and consumer lag.
6. Ensure the sink has a natural-key UNIQUE constraint if the stream supports dual-write (HTTP fallback path).
7. Document the event payload schema in the relevant Tier-1 doc (e.g., new audit event → `audit-pipeline-architecture.md`).

## 11. Sources

- `packages/shared/transport/mq/` — interface + NATS JetStream driver (flat single-package: `streams.go`, `factory.go`, `producer.go`, `consumer.go`, `ack.go`, …).
- `packages/nexus-hub/internal/jobs/consumer/` — Hub-side consumers (`admin_audit.go`, `batch.go`, `manager.go`, …).

## 12. Cross-references

- `thing-config-sync-architecture.md` — config-sync is NOT on MQ.
- `audit-pipeline-architecture.md` — audit event stream consumers.
- `trace-id-propagation-architecture.md` — envelope `trace_id` semantics.
- `alerting-architecture.md` — alert evaluation consumes `nexus.traffic` and `nexus.ops_metrics`.
- `multi-endpoint-coordination-architecture.md` — flows that fan out through MQ.

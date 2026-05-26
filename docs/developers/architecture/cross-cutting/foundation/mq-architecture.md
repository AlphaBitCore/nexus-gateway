# MQ Architecture

The Nexus MQ layer is a thin **producer / consumer abstraction** over NATS JetStream, used for **traffic events**, **admin-audit events**, **agent auto-exemption uploads**, **auth-token revocation broadcasts**, and **inter-Hub config-change signals** — and nothing else. Everything else that *looks* like it could be on MQ (config push, kill-switch, metrics samples, alert envelopes) deliberately is not. This doc explains why, what is on the bus, and what each pattern guarantees.

Anchor packages:

- `packages/shared/transport/mq/` — driver abstraction + NATS JetStream implementation + stream definitions
- `packages/nexus-hub/internal/jobs/consumer/` — the three Hub-side consumer groups (`hub-db-writer`, `hub-siem`, `hub-alerting`)
- `packages/nexus-hub/internal/fleet/manager/`, `packages/nexus-hub/internal/ws/signal.go` — inter-Hub broadcast subject (`nexus.hub.signal`)
- `packages/control-plane/internal/identity/authserver/revocation/` + `packages/control-plane/internal/identity/jwt/mqrevocation.go` — auth revocation publisher and consumer
- `packages/{ai-gateway,compliance-proxy,control-plane}/cmd/*/wiring/` — per-service producer / consumer construction

## 1. Why MQ at all, and why not the alternatives

Nexus is **Hub-centric** for control-plane state: every config change, kill switch, IAM policy, and Thing-shadow update flows admin → CP → Hub → WebSocket push to Things. There is no NATS pub/sub for *config invalidation*. So why is there an MQ at all?

Three properties only MQ gives us:

1. **Short-term decoupling of bursty event paths from synchronous user requests.** AI Gateway serves an HTTP request, captures a traffic event (cost, tokens, normalized text, cache classification), and must return to the user *before* Hub finishes writing the event to PostgreSQL. The producer-side `Enqueue` returns in a millisecond regardless of `pgxpool` saturation, jobs-architecture rollup contention, or temporary Hub unavailability. With a synchronous HTTP write to Hub, every DB hiccup would surface as user-visible request latency.

2. **Kafka-style fan-out from one producer to multiple independent consumer groups.** A single traffic event must reach the `hub-db-writer` (persistence), `hub-siem` (forwarder), and `hub-alerting` (real-time rule evaluation) groups, each at its own rate, with independent retry semantics. JetStream's `InterestPolicy` retention does exactly that: the message is retained until *all* defined consumers have acked. Adding a new consumer group is configuration, not a producer change.

3. **At-least-once durability across consumer restarts.** Hub deploys, restarts, pgxpool blips — the events queued during a multi-minute outage replay automatically on reconnect. JetStream file storage + `DiscardOld` cap means a wedged consumer cannot pin the stream forever, but a healthy consumer that briefly disconnects loses nothing.

Why not the alternatives:

- **Redis pub/sub** — chosen for Nexus's session/IAM/cache/quota layer (no `Subscribe` for control coordination). Pub/sub has no persistence, no consumer groups, no fan-out semantics, and no at-least-once delivery. Bursty traffic-event load would drop on the floor whenever Hub disconnects, and SIEM would lose forensic events. The "no Redis pub/sub" rule is CI-enforced in pre-commit (`no Redis pub/sub` gate).
- **HTTP push to Hub** — eliminates one network hop in the happy path but couples producer latency to Hub availability, and forces every producer to implement its own retry / spool / backpressure scheme. We do use HTTP for `metrics_sample` (§7) and alert envelopes (§7) because their delivery semantics are different and the volume is two orders of magnitude lower.
- **Kafka** — viable but operationally heavier than NATS JetStream for a single-region deployment with sub-TB retention. The `Config.Driver` enum reserves `"kafka"` for future use (`packages/shared/transport/mq/config.go`); registry-driven swap-in costs one factory registration.

## 2. The driver abstraction: two semantics in one interface

`packages/shared/transport/mq/mq.go` exposes two pairs of methods on `Producer` / `Consumer`, each with deliberately different guarantees:

| Producer call | Consumer call | Backing | Persistence | Delivery |
|---|---|---|---|---|
| `Publish(ctx, topic, data)` | `Subscribe(ctx, topic, handler)` | Core NATS | None — fire-and-forget | Best-effort broadcast to all live subscribers |
| `Enqueue(ctx, queue, data)` | `Consume(ctx, queue, group, handler)` | NATS JetStream | File-backed, durable per stream | At-least-once, distributed across the group |

Choosing the right pair is the most consequential decision in any new MQ wiring:

- **`Publish` / `Subscribe`** is correct when the message is a *signal* with no long-term value: "a Thing's config changed at this Hub, other Hubs should reload from DB" — if you miss the signal, the next periodic reconciliation tick or the next explicit `force-resync` covers you. Used only for `nexus.hub.signal` (§5).
- **`Enqueue` / `Consume`** is correct when the message *is* the data: a traffic event, an admin audit row, a revocation, an agent exemption upload. Losing one is observable downstream.

### `ErrDeferAck` — batching contract

`ack.go` defines a sentinel error that the JetStream consumer recognises:

```go
return mq.ErrDeferAck    // → consumer does NOT auto-ack; handler will call msg.Ack() / msg.Nak() later
return nil               // → consumer auto-acks immediately on return
return err               // → consumer auto-naks for redelivery (up to MaxDeliver)
```

`TrafficEventWriter` and `AdminAuditWriter` use this to batch multiple messages into one DB transaction: the handler validates each message, queues it on an in-process flush buffer, and returns `ErrDeferAck`. When the batch flushes (size threshold or max-latency timer), the writer iterates `msg.Ack()` for every successfully persisted row and `msg.Nak()` for any that failed. This is the only way to achieve **ack-after-DB-commit** semantics with batched writes — without it, a writer crash between MQ-ack and DB-flush would silently drop events.

Consumers that do not recognise `ErrDeferAck` (Redis driver, memory driver in tests) treat it as a generic error and Nak. NATS JetStream is the only driver in use today; the contract is what makes the batch flush path safe.

## 3. Streams: two of them, on purpose

`packages/shared/transport/mq/streams.go` defines exactly two JetStream streams; `EnsureStreams(ctx, js)` runs at Hub startup and is idempotent (`CreateOrUpdateStream`).

| Stream | Subjects | Retention | Max age | Max bytes | Storage |
|---|---|---|---|---|---|
| `NEXUS_EVENTS` | `nexus.event.>` | `InterestPolicy` | 6 hours | 8 GiB | File |
| `NEXUS_AUTH` | `nexus.auth.>` | `InterestPolicy` | 24 hours | 256 MiB | File |

The capacity envelopes are calibrated to the production single-node deployment (~7.6 GiB host RAM) so file storage does not pressure the kernel page cache against PostgreSQL and the Go heap. The matching server-level cap is `js_max_file_store: 32GB` in `/etc/nats/nats-server.conf` — the streams stay well under that.

### Why `InterestPolicy` rather than `WorkQueuePolicy`

`InterestPolicy` retains every message until *all defined consumers* have acked. `WorkQueuePolicy` deletes a message as soon as any consumer acks. The choice is what enables Kafka-style fan-out: `hub-db-writer`, `hub-siem`, and `hub-alerting` are three independent consumer groups that each must receive every traffic event. With `WorkQueuePolicy`, whichever group fetched first would delete the message for the others.

### Why `DiscardOld` rather than `DiscardNew`

A stalled consumer is a known failure mode (DB hung, SIEM endpoint timing out). `DiscardOld` means a stall trims the oldest messages from the stream and the producer keeps publishing — no `insufficient_resources` publish errors backing up onto user-facing request paths. The 6-hour `MaxAge` keeps the worst-case backlog bounded even if no consumer drains: events older than 6 h are already written to `traffic_event` / `admin_audit` by a healthy `hub-db-writer`, so the loss surface during a *truly* wedged consumer is the events written during the wedge itself.

### `streamName` and the `NEXUS_DEFAULT` fallback

`streamName(queue)` in `streams.go` does string-prefix routing: `nexus.event.*` → `NEXUS_EVENTS`, `nexus.auth.*` → `NEXUS_AUTH`, anything else → `NEXUS_DEFAULT`. The fallback exists for tests and future subjects that have not been formally promoted into a real stream config; production code must not rely on it, and `EnsureStreams` does not create a `NEXUS_DEFAULT` — any consumer hitting it gets a "stream not found" error at `resolveStream` time. The fallback's only job is to make the lookup total.

## 4. Subject inventory

Seven active subjects. The first six are JetStream queues; `nexus.hub.signal` is Core NATS broadcast.

| Subject | Stream | Producer | Consumer group(s) | Wire shape |
|---|---|---|---|---|
| `nexus.event.ai-traffic` | `NEXUS_EVENTS` | `packages/ai-gateway/internal/platform/audit/audit.go` (per-request) | `hub-db-writer`, `hub-siem`, `hub-alerting` | `mq.TrafficEventMessage` |
| `nexus.event.compliance` | `NEXUS_EVENTS` | `packages/compliance-proxy/internal/audit/mq_writer.go` (per CONNECT) | `hub-db-writer`, `hub-siem`, `hub-alerting` | `mq.TrafficEventMessage` |
| `nexus.event.agent` | `NEXUS_EVENTS` | Hub re-enqueue from agent HTTP upload (`packages/nexus-hub/internal/fleet/handler/hubapi/internal_things.go` `AuditUpload`) | `hub-db-writer`, `hub-siem`, `hub-alerting` | `mq.TrafficEventMessage` |
| `nexus.event.admin-audit` | `NEXUS_EVENTS` | `packages/control-plane/internal/platform/audit/writer.go` (per admin mutation) | `hub-db-writer`, `hub-siem`, `hub-alerting` | `mq.AdminAuditMessage` |
| `nexus.event.exemption` | `NEXUS_EVENTS` | Hub re-enqueue from agent HTTP upload (`internal_things.go` `ExemptionUpload`) | `hub-db-writer` only | `{kind, thingId, host, reason, expiresAt}` inline (`packages/nexus-hub/internal/jobs/consumer/exemption.go`) |
| `nexus.auth.revocation` | `NEXUS_AUTH` | `packages/control-plane/internal/identity/authserver/revocation/publisher.go` | per-instance (one group per CP / AI-gateway / proxy instance) | `revocation.Event` (`scope`, `targetJti` / `targetUserId` / `targetDeviceId` / `targetSessionId`, `reason`, `issuedAt`) |
| `nexus.hub.signal` | (Core NATS — no JS) | Hub fleet manager (`packages/nexus-hub/internal/fleet/manager/config.go`, `drift.go`) | each Hub instance's WS bridge (`packages/nexus-hub/internal/ws/signal.go`) | `{action, sourceHub, thingType, configKey, state, version, thingId?, desired?, force?}` |

### Why agent traffic + exemption are *re-enqueued* by Hub, not produced by the agent

Agents do not hold NATS credentials and do not have direct MQ network reach (see thing-model.md). Every byte of agent-emitted state arrives at Hub via authenticated HTTP — `POST /api/internal/things/audit` (handler `AuditUpload`) for traffic, `POST /api/internal/things/exemption` (handler `ExemptionUpload`) for TLS-bump auto-exemptions. The HTTP handler validates the upload (auth header, payload shape, CHECK-constraint hygiene like stripping empty-string `usageExtractionStatus` before downstream `traffic_event_*` CHECKs reject it) and then calls `MQProducer.Enqueue(...)`. From the consumer's perspective the wire shape on `nexus.event.agent` is identical to `nexus.event.ai-traffic` / `.compliance`, which is what lets the same `TrafficEventWriter` and `SIEMForwarder` code paths handle all three.

### The wire shapes are stable contracts

`packages/shared/transport/mq/messages.go` is the single source of truth for `TrafficEventMessage` and `AdminAuditMessage`. New fields must be `omitempty` so older producers stay wire-compatible; renaming a JSON tag is a breaking change. The hash-chained admin audit (`previousHash` / `integrityHash`) is computed **Hub-side** by the `AdminAuditWriter` (`packages/nexus-hub/internal/jobs/consumer/admin_audit.go` invoking `chain.NextHash` from `packages/nexus-hub/internal/traffic/chain/chain.go`) — the wire format intentionally carries no hash so a CP replica cannot fork the chain.

## 5. Three consumer-group patterns

Pick the pattern by asking: *who needs to see each message, and how many physical readers will there be of that role?*

### Pattern A — work-queue inside a group: multiple workers, one logical reader

One group string, many worker instances inside the group. JetStream distributes messages across the group; each message goes to exactly one worker. Adding a worker scales throughput without duplicating work.

**Live example: `dbWriterGroup = "hub-db-writer"`** (`packages/nexus-hub/internal/jobs/consumer/traffic.go`). All three Hub-side DB writers — `TrafficEventWriter` (three traffic subjects), `AdminAuditWriter` (admin-audit), `ExemptionConsumer` (exemption) — share this group string. If we ran two Hub instances, each subject would still be processed by exactly one Hub at a time per subject; the other Hub's writer for that subject is a hot spare.

Why a shared group across different writers is safe here: each writer uses a distinct `FilterSubject`, and `jetstreamDurableName(group, queue)` (`consumer.go`) builds the JetStream durable as `"hub-db-writer__nexus_event_admin-audit"` etc. — one durable per (group, subject) pair. Sharing `group` without per-subject sanitisation would clobber `FilterSubject` and silently route admin-audit messages into the traffic writer. This was discovered the hard way; the sanitiser is the load-bearing line.

### Pattern B — Kafka-style fan-out: multiple independent groups, each reads everything

Each group is a *role*; messages on a subject are delivered to one worker per group, but every group sees every message. This is what `InterestPolicy` retention exists for.

**Live example: the three Hub roles on `nexus.event.{ai-traffic, compliance, agent, admin-audit}`** — `hub-db-writer` persists, `hub-siem` forwards to the SIEM bridge, `hub-alerting` evaluates real-time rules. Each role consumes every message exactly once, independently retried, with independent ack progress. Adding a fourth role (e.g. a streaming analytics processor) is one new consumer registration; no producer change.

`nexus.event.exemption` deliberately uses only `hub-db-writer` — there is no SIEM forwarding or alerting use case for individual exemption uploads (the admin review at `/compliance/exemptions` is the audit surface). Adding it later costs one more `Consume(ctx, "nexus.event.exemption", "hub-siem", ...)` call; nothing else changes.

### Pattern C — per-instance broadcast: every instance is its own group

The group name embeds an instance identifier so each instance gets its own durable consumer. Every instance receives every message. Use this when each instance needs to update local state (revocation bloom filter, in-memory shadow cache) from the same event stream.

**Live example: `cp-revocation-<sanitized-thingID>`** (`packages/control-plane/cmd/control-plane/wiring/jwt.go`). Every CP instance subscribes to `nexus.auth.revocation` under a unique group; each instance independently applies revocations to its in-memory bloom filter + JTI set (`packages/control-plane/internal/identity/jwt/mqrevocation.go`). The same pattern is what the prior single-group design got wrong: with one shared group, instance A would steal a revocation event from instance B and B's bloom filter would silently miss it.

`nexus.hub.signal` is the Core-NATS analogue of this pattern: a `Subscribe` (not `Consume`) per Hub instance, no durable, no retention. Each Hub's WS bridge receives every signal and broadcasts `config_changed` to its locally-attached WebSocket Things. The subscriber filters out signals where `sig.SourceHub == hubID` to avoid loopback in the publisher's own pool.

## 6. Failure semantics

### At-least-once + bounded redelivery

The JetStream consumer in `consumer.go` configures:

- `AckPolicy: AckExplicitPolicy` — no auto-ack at the JS layer; the consumer code controls ack timing
- `MaxDeliver: 5` — a message that fails to ack after 5 deliveries is dropped (with a NATS server-level log; no DLQ today)
- `AckWait: 30 * time.Second` — if the handler does not ack within 30 s, JS redelivers

`MaxDeliver: 5` is the budget that prevents poison-pill loops from filling the stream. Five retries on a 30-second AckWait is ~2.5 min of total redelivery before drop; that is long enough for transient DB hiccups and short enough that a structurally-bad message (wrong JSON shape, CHECK-constraint violation that no retry can fix) does not loop forever. Today there is no dead-letter queue — the assumption is that a `MaxDeliver`-exhausted message has been seen multiple times in `hub-db-writer` ERROR logs, surfaced via the diag-event triage pipeline, and a human will pull it from logs if forensics are needed.

### Ack-after-DB-commit via `ErrDeferAck`

The traffic + admin-audit writers `return ErrDeferAck` from their per-message handler and instead enqueue the message on an in-memory batch buffer. The batch flush (size threshold or max-latency timer) opens a single DB transaction, attempts the bulk insert, and:

- On success: iterates the batch and calls `msg.Ack()` on each entry — messages remain "in-flight" from JS's perspective until the DB commit succeeds.
- On failure: iterates and calls `msg.Nak()` on each entry — JS redelivers per `MaxDeliver` budget.

If the writer process crashes between MQ-ack and DB-commit, the un-acked messages redeliver after `AckWait` expires. This is the strongest delivery guarantee an at-least-once system can give — exactly-once would require an idempotency key the DB checks on insert, and we explicitly don't do that for traffic events (each event has a producer-generated unique `id`, and downstream re-ingestion against a UNIQUE constraint would generate spurious ERROR rows under retry).

### NATS reconnect watchdog

`connection_handlers.go` wires per-callback logging for `Disconnect`, `Reconnect`, `Closed`, and `AsyncErr`. Disconnect starts a watchdog timer; if reconnection does not happen within `disconnectWatchdogThreshold`, the WARN escalates to ERROR — this is what surfaces a persistent NATS outage in the diag-event triage pipeline rather than letting the producer/consumer churn silently. The connection itself is configured with `MaxReconnects: -1` and `ReconnectWait: 2s`, so the client keeps trying forever.

## 7. Explicitly *not* on MQ

A recurring confusion is "should X go on MQ?". The default answer is **no**. Four things look like MQ candidates but are intentionally on other transports.

| Carried over | Why not MQ |
|---|---|
| `metrics_sample` (per-Thing health snapshots) | Travels via the thingclient WebSocket (HTTP fallback) — same connection that already exists for Cat A/B config push. Adding MQ would mean every Thing holds NATS credentials, which violates the "agent has no DB or MQ credentials" boundary. The volume is small (one batch per heartbeat per Thing) and loss is acceptable (the next heartbeat carries fresh state). |
| Alert envelopes (raised alerts from data-plane services to Hub's `/api/v1/alerts/raise`) | HTTP POST with local-disk **spool** fallback (`packages/nexus-hub/internal/alerts/client/client.go` `Fire`). Alerts are sparse, latency-sensitive ("page someone now"), and the spool's at-most-once-by-default + bounded-disk-bytes profile is a better match than JetStream's at-least-once + ack-explicit model. An alert delivered twice creates two pages; a duplicate-prone bus is the wrong tool. |
| Kill switch + every config change | Hub shadow (Cat A inline) + Hub WS push (Cat B loader pull). Reasons: every Thing needs to see every config change, the receiver list is dynamic (Things come and go), the message is authoritative-state not an event, and the Hub-as-source-of-truth model means a Thing that misses a push catches up on next pull. MQ does not improve any of these properties. |
| Inter-service direct calls (CP → Hub HTTP API for shadow writes, AI Gateway → Hub for credential lookups) | Synchronous HTTP — the caller needs the result or the error code. MQ would force every call site into a request-reply pattern with timeout handling, with no offsetting benefit. |

Two formerly-existed-now-removed MQ subjects, removed for cause:

- `nexus.event.alert` — Hub used to enqueue this from the alert raiser; no consumer was ever wired in any service. Removed 2026-05-24 (MQ-C3); alerts already flow through the HTTP raise path above.
- `nexus.event.diag` — Hub used to enqueue this from the diag-event writer; no consumer was ever wired. Removed 2026-05-24 (MQ-C3); diag events are persisted directly to `thing_diag_event` and read via the runtime-introspection HTTP surface.

## 8. Operations

### Stream creation

Hub startup calls `mq.Setup(ctx, natsURL)` from `packages/nexus-hub/cmd/nexus-hub/wiring/mq.go`. `Setup` opens a short-lived NATS connection, calls `EnsureStreams(ctx, js)` (which uses `CreateOrUpdateStream` per stream — idempotent and safe on every boot), then disconnects. Other services (AI Gateway, Compliance Proxy, Control Plane) **never** call `Setup` or `EnsureStreams`; they assume the streams exist and a missing stream surfaces as a `resolveStream` error at first `Consume`, which is the correct failure mode for a misconfigured environment.

### Durable consumer names

`jetstreamDurableName(group, queue)` builds names like `hub-db-writer__nexus_event_admin-audit`. The format is `<group>__<sanitised-queue>` where `.` becomes `_` (JetStream durable names do not accept dots, slashes, colons, or spaces). The Control Plane's revocation group additionally pre-sanitises the embedded thing ID via `sanitizeForJetStreamDurable(thingID)` because `cpThingID` contains a hostname-derived suffix that may include characters JS rejects.

The invariant: any new consumer group string must be safe to embed in a durable name *after* `jetstreamDurableName`'s `.` → `_` substitution. The compiled durable name appears in NATS server logs and `nats consumer info` output, so cryptic groups make on-call harder; `hub-db-writer` / `hub-siem` / `hub-alerting` / `cp-revocation-<thingID>` are the canonical names worth preserving.

### Migration / capacity changes

Stream re-sizing (raising `MaxBytes`, adjusting `MaxAge`) is a Hub-restart operation: `EnsureStreams` calls `CreateOrUpdateStream`, JetStream applies the new config in-place without dropping messages. Switching `Retention` from `InterestPolicy` to `WorkQueuePolicy` is **not** a hot-update — JetStream will reject the update with `cannot change retention` and the stream must be torn down and re-created. There is no live production case for switching retention; the doc-anchored choice is `InterestPolicy` for both streams.

### Reading the stream during incidents

NATS CLI on the Hub box: `nats stream info NEXUS_EVENTS` for current message count, byte size, and consumer ack lag; `nats consumer report NEXUS_EVENTS` for per-group `Pending` (un-acked count, the lag signal) and `Redelivered` (poison-pill signal). A `Pending` value that grows without bound usually means the DB writer is stalled (check `packages/nexus-hub/internal/jobs/consumer/traffic.go` flush metrics); a `Redelivered` value that grows usually means a structurally-bad message that needs to be located in writer ERROR logs.

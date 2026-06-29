# MQ Architecture

The Nexus MQ layer is a thin **producer / consumer abstraction** over NATS JetStream, used for **traffic events**, **admin-audit events**, **agent auto-exemption uploads**, **auth-token revocation broadcasts**, and **inter-Hub config-change signals** — and nothing else. Everything else that *looks* like it could be on MQ (config push, kill-switch, metrics samples, alert envelopes) deliberately is not. This doc explains why, what is on the bus, and what each pattern guarantees.

Anchor packages:

- `packages/shared/transport/mq/` — driver abstraction + NATS JetStream implementation + stream definitions
- `packages/nexus-hub/internal/jobs/consumer/` — the two Hub-side consumer groups (`hub-db-writer`, `hub-alerting`)
- `packages/nexus-hub/internal/fleet/manager/`, `packages/nexus-hub/internal/ws/signal.go` — inter-Hub broadcast subject (`nexus.hub.signal`)
- `packages/control-plane/internal/identity/authserver/revocation/` + `packages/control-plane/internal/identity/jwt/mqrevocation.go` — auth revocation publisher and consumer
- `packages/{ai-gateway,compliance-proxy,control-plane}/cmd/*/wiring/` — per-service producer / consumer construction

## 1. Why MQ at all, and why not the alternatives

Nexus is **Hub-centric** for control-plane state: every config change, kill switch, IAM policy, and Thing-shadow update flows admin → CP → Hub → WebSocket push to Things. There is no NATS pub/sub for *config invalidation*. So why is there an MQ at all?

Three properties only MQ gives us:

1. **Short-term decoupling of bursty event paths from synchronous user requests.** AI Gateway serves an HTTP request, captures a traffic event (cost, tokens, normalized text, cache classification), and must return to the user *before* Hub finishes writing the event to PostgreSQL. The producer-side `Enqueue` returns in a millisecond regardless of `pgxpool` saturation, jobs-architecture rollup contention, or temporary Hub unavailability. With a synchronous HTTP write to Hub, every DB hiccup would surface as user-visible request latency.

2. **Kafka-style fan-out from one producer to multiple independent consumer groups.** A single traffic event must reach the `hub-db-writer` (persistence) and `hub-alerting` (real-time rule evaluation) groups, each at its own rate, with independent retry semantics. JetStream's `InterestPolicy` retention does exactly that: the message is retained until *all* defined consumers have acked. Adding a new consumer group is configuration, not a producer change.

3. **At-least-once durability across consumer restarts.** Hub deploys, restarts, pgxpool blips — the events queued during a multi-minute outage replay automatically on reconnect. A healthy consumer that briefly disconnects loses nothing, because JetStream retains the un-acked messages until the consumer comes back and acks. `NEXUS_EVENTS` uses `DiscardNew` + a large cap so a wedge never silently drops audit (the producer spills to its own durable on-disk store instead — see below). The default storage tier is **memory** (see §3), so this consumer-restart replay holds, but a **broker-process restart** drops published-but-undrained events that are still only in RAM; operators who need durability across a broker bounce set `NEXUS_EVENTS_STORAGE=file`.

Why not the alternatives:

- **Redis pub/sub** — chosen for Nexus's session/IAM/cache/quota layer (no `Subscribe` for control coordination). Pub/sub has no persistence, no consumer groups, no fan-out semantics, and no at-least-once delivery. Bursty traffic-event load would drop on the floor whenever Hub disconnects, and forensic / billing events would be lost. The "no Redis pub/sub" rule is CI-enforced in pre-commit (`no Redis pub/sub` gate).
- **HTTP push to Hub** — eliminates one network hop in the happy path but couples producer latency to Hub availability, and forces every producer to implement its own retry / spool / backpressure scheme. We do use HTTP for `metrics_sample` (§7) and alert envelopes (§7) because their delivery semantics are different and the volume is two orders of magnitude lower.
- **Kafka** — viable but operationally heavier than NATS JetStream for a single-region deployment with sub-TB retention. The `Config.Driver` enum reserves `"kafka"` for future use (`packages/shared/transport/mq/config.go`); registry-driven swap-in costs one factory registration.

## 2. The driver abstraction: two semantics in one interface

`packages/shared/transport/mq/mq.go` exposes two pairs of methods on `Producer` / `Consumer`, each with deliberately different guarantees:

| Producer call | Consumer call | Backing | Persistence | Delivery |
|---|---|---|---|---|
| `Publish(ctx, topic, data)` | `Subscribe(ctx, topic, handler)` | Core NATS | None — fire-and-forget | Best-effort broadcast to all live subscribers |
| `Enqueue(ctx, queue, data)` | `Consume(ctx, queue, group, handler)` | NATS JetStream | Per-stream storage tier (see §3): `NEXUS_EVENTS` memory by default with a durable producer-side spill, `NEXUS_AUTH` file-backed | At-least-once, distributed across the group |

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
| `NEXUS_EVENTS` | `nexus.event.>` | `InterestPolicy` | 6 hours | auto (≈15% RAM) | Memory (default; `file` opt-in) |
| `NEXUS_AUTH` | `nexus.auth.>` | `InterestPolicy` | 24 hours | 256 MiB | File |

`NEXUS_EVENTS` defaults to **memory** storage because the audit stream is a delay-tolerant burst buffer (the producer publishes full-speed, the Hub drains lazily to PostgreSQL): on a single box whose data-disk write bandwidth is the bottleneck, keeping the buffer in RAM frees the disk for the durable PostgreSQL + WAL writes — the single largest single-box throughput lever — instead of writing every audit message to the volume twice. Its `MaxBytes` defaults to **`auto` = 15% of total RAM** (read from `/proc/meminfo` `MemTotal`, with a 1 GiB floor and an 8 GiB fallback when RAM cannot be read); at boot the Hub logs a WARN with the chosen byte value and how to pin it. Override with `NEXUS_EVENTS_MAX_BYTES=<size>` (e.g. `32GB`; alias `NEXUS_STREAM_MAX_BYTES`) for the cap and `NEXUS_EVENTS_STORAGE=file` for the durable file-backed tier. Because the memory-tier cap is committed to RAM rather than disk, on a large box `auto` reserves a large RAM share (e.g. ~38 GiB on a 256 GiB host) — size `GOMEMLIMIT` / the cgroup memory limit with that reservation in mind, or pin `NEXUS_EVENTS_MAX_BYTES` to a fixed value. `NEXUS_AUTH` stays file-backed (durable token-revocation coordination, low volume). The matching server-level cap `js_max_file_store: 32GB` in `/etc/nats/nats-server.conf` bounds the file-backed tier; for the memory default, the relevant server-level cap is `js_max_memory_store`.

### Why `InterestPolicy` rather than `WorkQueuePolicy`

`InterestPolicy` retains every message until *all defined consumers* have acked. `WorkQueuePolicy` deletes a message as soon as any consumer acks. The choice is what enables Kafka-style fan-out: `hub-db-writer` and `hub-alerting` are two independent consumer groups that each must receive every traffic event. With `WorkQueuePolicy`, whichever group fetched first would delete the message for the others.

### Why `DiscardNew` rather than `DiscardOld`

A stalled consumer is a known failure mode (DB hung, alert evaluator wedged), so `NEXUS_EVENTS` uses `DiscardNew`: at the cap, NEW publishes fail with `insufficient_resources` and the producer routes the rejected record to its **durable on-disk spill** (`handlePublishFailure`), rather than `DiscardOld` silently trimming the oldest un-acked audit rows. This does not back up onto user-facing request paths — the publish + spill run on the audit Writer's async drain, never the request goroutine — and the stream is sized large (`MaxBytes` = `NEXUS_EVENTS_MAX_BYTES`, default `auto` ≈ 15% of RAM) so the spill fallback is reached only under a genuine sustained wedge. The 6-hour `MaxAge` still bounds the worst-case backlog (events older than 6 h are already written to `traffic_event` / `admin_audit` by a healthy `hub-db-writer`). (`NEXUS_AUTH` keeps `DiscardOld` — auth-coordination events are transient, not an audit record.)

**No-loss scope (important with the memory default).** The no-loss guarantee is precise, not blanket. It holds for two cases: (1) **consumer restart** — JetStream retains un-acked messages until the `hub-db-writer` reconnects and acks; and (2) **stream-full overflow** — `DiscardNew` rejects the publish and the producer reclaims the record into its durable on-disk spill, which the spill-recovery sweeper later replays to PostgreSQL. It does **not** hold for a **NATS broker-process restart/crash on the memory-backed stream**: events the producer has already published (and reclaimed as durable from its own perspective) but the Hub has not yet drained to PostgreSQL live only in the broker's RAM, so they are lost on a broker bounce — they are not in the producer spill (that only catches *rejected* publishes). For strict durability across a broker restart, set `NEXUS_EVENTS_STORAGE=file`, which trades the steady-state disk writes for on-disk persistence of the in-flight buffer. PostgreSQL remains the durable system of record regardless; only the transient in-flight window is exposed.

### `streamName` and the `NEXUS_DEFAULT` fallback

`streamName(queue)` in `streams.go` does string-prefix routing: `nexus.event.*` → `NEXUS_EVENTS`, `nexus.auth.*` → `NEXUS_AUTH`, anything else → `NEXUS_DEFAULT`. The fallback exists for tests and future subjects that have not been formally promoted into a real stream config; production code must not rely on it, and `EnsureStreams` does not create a `NEXUS_DEFAULT` — any consumer hitting it gets a "stream not found" error at `resolveStream` time. The fallback's only job is to make the lookup total.

## 4. Subject inventory

Seven active subjects. The first six are JetStream queues; `nexus.hub.signal` is Core NATS broadcast.

| Subject | Stream | Producer | Consumer group(s) | Wire shape |
|---|---|---|---|---|
| `nexus.event.ai-traffic` | `NEXUS_EVENTS` | `packages/ai-gateway/internal/platform/audit/audit.go` (per-request) | `hub-db-writer`, `hub-alerting` | `mq.TrafficEventMessage` |
| `nexus.event.compliance` | `NEXUS_EVENTS` | `packages/compliance-proxy/internal/audit/mq_writer.go` (per CONNECT) | `hub-db-writer`, `hub-alerting` | `mq.TrafficEventMessage` |
| `nexus.event.agent` | `NEXUS_EVENTS` | Hub re-enqueue from agent HTTP upload (`packages/nexus-hub/internal/fleet/handler/hubapi/internal_things.go` `AuditUpload`) | `hub-db-writer`, `hub-alerting` | `mq.TrafficEventMessage` |
| `nexus.event.admin-audit` | `NEXUS_EVENTS` | `packages/control-plane/internal/platform/audit/writer.go` (per admin mutation) | `hub-db-writer`, `hub-alerting` | `mq.AdminAuditMessage` |
| `nexus.event.exemption` | `NEXUS_EVENTS` | Hub re-enqueue from agent HTTP upload (`internal_things.go` `ExemptionUpload`) | `hub-db-writer` only | `{kind, thingId, host, reason, expiresAt}` inline (`packages/nexus-hub/internal/observability/consumer/exemption.go`) |
| `nexus.auth.revocation` | `NEXUS_AUTH` | `packages/control-plane/internal/identity/authserver/revocation/publisher.go` | per-instance (one group per CP / AI-gateway / proxy instance) | `revocation.Event` (`scope`, `targetJti` / `targetUserId` / `targetDeviceId` / `targetSessionId`, `reason`, `issuedAt`) |
| `nexus.hub.signal` | (Core NATS — no JS) | Hub fleet manager (`packages/nexus-hub/internal/fleet/manager/config.go`, `drift.go`) | each Hub instance's WS bridge (`packages/nexus-hub/internal/ws/signal.go`) | `{action, sourceHub, thingType, configKey, state, version, thingId?, desired?, force?}` |

### Why agent traffic + exemption are *re-enqueued* by Hub, not produced by the agent

Agents do not hold NATS credentials and do not have direct MQ network reach (see thing-model.md). Every byte of agent-emitted state arrives at Hub via authenticated HTTP — `POST /api/internal/things/audit` (handler `AuditUpload`) for traffic, `POST /api/internal/things/exemption` (handler `ExemptionUpload`) for TLS-bump auto-exemptions. The HTTP handler validates the upload (auth header, payload shape, CHECK-constraint hygiene like stripping empty-string `usageExtractionStatus` before downstream `traffic_event_*` CHECKs reject it) and then calls `MQProducer.Enqueue(...)`. From the consumer's perspective the wire shape on `nexus.event.agent` is identical to `nexus.event.ai-traffic` / `.compliance`, which is what lets the same `TrafficEventWriter` code path handle all three.

### The wire shapes are stable contracts

`packages/shared/transport/mq/messages.go` is the single source of truth for `TrafficEventMessage` and `AdminAuditMessage`. New fields must be `omitempty` so older producers stay wire-compatible; renaming a JSON tag is a breaking change. The hash-chained admin audit (`previousHash` / `integrityHash`) is computed **Hub-side** by the `AdminAuditWriter` (`packages/nexus-hub/internal/observability/consumer/admin_audit.go` invoking `chain.NextHash` from `packages/nexus-hub/internal/traffic/chain/chain.go`) — the wire format intentionally carries no hash so a CP replica cannot fork the chain.

## 5. Three consumer-group patterns

Pick the pattern by asking: *who needs to see each message, and how many physical readers will there be of that role?*

### Pattern A — work-queue inside a group: multiple workers, one logical reader

One group string, many worker instances inside the group. JetStream distributes messages across the group; each message goes to exactly one worker. Adding a worker scales throughput without duplicating work.

**Live example: `dbWriterGroup = "hub-db-writer"`** (`packages/nexus-hub/internal/observability/consumer/traffic.go`). All three Hub-side DB writers — `TrafficEventWriter` (three traffic subjects), `AdminAuditWriter` (admin-audit), `ExemptionConsumer` (exemption) — share this group string. If we ran two Hub instances, each subject would still be processed by exactly one Hub at a time per subject; the other Hub's writer for that subject is a hot spare.

Why a shared group across different writers is safe here: each writer uses a distinct `FilterSubject`, and `jetstreamDurableName(group, queue)` (`consumer.go`) builds the JetStream durable as `"hub-db-writer__nexus_event_admin-audit"` etc. — one durable per (group, subject) pair. Sharing `group` without per-subject sanitisation would clobber `FilterSubject` and silently route admin-audit messages into the traffic writer; the sanitiser is the load-bearing line.

### Pattern B — Kafka-style fan-out: multiple independent groups, each reads everything

Each group is a *role*; messages on a subject are delivered to one worker per group, but every group sees every message. This is what `InterestPolicy` retention exists for.

**Live example: the two Hub roles on `nexus.event.{ai-traffic, compliance, agent, admin-audit}`** — `hub-db-writer` persists, `hub-alerting` evaluates real-time rules. Each role consumes every message exactly once, independently retried, with independent ack progress. Adding a third role (e.g. a streaming analytics processor) is one new consumer registration; no producer change.

`nexus.event.exemption` deliberately uses only `hub-db-writer` — there is no alerting use case for individual exemption uploads (the admin review at `/compliance/exemptions` is the audit surface). Adding it later costs one more `Consume(ctx, "nexus.event.exemption", "hub-alerting", ...)` call; nothing else changes.

### Pattern C — per-instance broadcast: every instance is its own group

The group name embeds an instance identifier so each instance gets its own durable consumer. Every instance receives every message. Use this when each instance needs to update local state (revocation bloom filter, in-memory shadow cache) from the same event stream.

**Live example: `cp-revocation-<sanitized-thingID>`** (`packages/control-plane/cmd/control-plane/wiring/jwt.go`). Every CP instance subscribes to `nexus.auth.revocation` under a unique group; each instance independently applies revocations to its in-memory bloom filter + JTI set (`packages/control-plane/internal/identity/jwt/mqrevocation.go`). A single shared group would be wrong here: instance A would steal a revocation event from instance B and B's bloom filter would silently miss it.

`nexus.hub.signal` is the Core-NATS analogue of this pattern: a `Subscribe` (not `Consume`) per Hub instance, no durable, no retention. Each Hub's WS bridge receives every signal and broadcasts `config_changed` to its locally-attached WebSocket Things. The subscriber filters out signals where `sig.SourceHub == hubID` to avoid loopback in the publisher's own pool.

## 6. Failure semantics

### At-least-once + bounded redelivery

The JetStream consumer in `consumer.go` configures:

- `AckPolicy: AckExplicitPolicy` — no auto-ack at the JS layer; the consumer code controls ack timing
- `MaxDeliver: 5` — once 5 deliveries are exhausted JetStream removes the message for that consumer immediately (the `MaxAge`/`MaxBytes` retention window does **not** grant a grace period past exhaustion)
- `AckWait: 30 * time.Second` — if the handler does not ack within 30 s, JS redelivers

`MaxDeliver: 5` is the budget that prevents poison-pill loops from filling the stream. A consumer that dead-letters on a redelivery cap MUST trip its own threshold **strictly below** `MaxDeliver` so the dead-letter write happens on a non-final delivery while budget remains — the Hub traffic writer does exactly this (`redeliveryThresholdAttempts = 3`; see `audit-pipeline-architecture.md` §10.1 for its DB-backed + on-disk DLQ). Consumers should also Nak with a delay rather than bare — the `Message.NakWithDelay(d)` hook (wired to JetStream's `NakWithDelay` for the queue path, a no-op on the fire-and-forget topic path) lets a consumer back off so a sustained downstream outage does not burn the whole budget in ~25-30s.

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

There is no MQ subject for alerts or diag events: alerts flow through the HTTP raise path above, and diag events are persisted directly to `thing_diag_event` and read via the runtime-introspection HTTP surface.

## 8. Operations

### Stream creation

Hub startup calls `mq.Setup(ctx, natsURL)` from `packages/nexus-hub/cmd/nexus-hub/wiring/mq.go`. `Setup` opens a short-lived NATS connection, calls `EnsureStreams(ctx, js)` (which uses `CreateOrUpdateStream` per stream — idempotent and safe on every boot), then disconnects. Other services (AI Gateway, Compliance Proxy, Control Plane) **never** call `Setup` or `EnsureStreams`; they assume the streams exist and a missing stream surfaces as a `resolveStream` error at first `Consume`, which is the correct failure mode for a misconfigured environment.

### Durable consumer names

`jetstreamDurableName(group, queue)` builds names like `hub-db-writer__nexus_event_admin-audit`. The format is `<group>__<sanitised-queue>` where `.` becomes `_` (JetStream durable names do not accept dots, slashes, colons, or spaces). The Control Plane's revocation group additionally pre-sanitises the embedded thing ID via `sanitizeForJetStreamDurable(thingID)` because `cpThingID` contains a hostname-derived suffix that may include characters JS rejects.

The invariant: any new consumer group string must be safe to embed in a durable name *after* `jetstreamDurableName`'s `.` → `_` substitution. The compiled durable name appears in NATS server logs and `nats consumer info` output, so cryptic groups make on-call harder; `hub-db-writer` / `hub-alerting` / `cp-revocation-<thingID>` are the canonical names worth preserving.

### Migration / capacity changes

Stream re-sizing (raising `MaxBytes`, adjusting `MaxAge`) is a Hub-restart operation: `EnsureStreams` calls `CreateOrUpdateStream`, and JetStream applies these mutable changes in-place without dropping messages.

Two fields are **immutable** in JetStream and cannot be changed by an in-place `CreateOrUpdateStream`:

- **`Retention`** — switching `InterestPolicy` ↔ `WorkQueuePolicy` is rejected with `cannot change retention`; the stream must be torn down and re-created. There is no live production case for switching retention; the doc-anchored choice is `InterestPolicy` for both streams.
- **`Storage`** — switching `NEXUS_EVENTS` between `memory` and `file` (i.e. toggling `NEXUS_EVENTS_STORAGE`) is rejected with err `10052` "stream configuration update can not change storage type". Because an in-place update cannot apply a storage-type change, `EnsureStreams` detects a storage-type mismatch against the existing stream and **deletes + recreates** the stream (logging a WARN) rather than failing boot. This drops the transient in-flight buffer at that moment, which is acceptable because PostgreSQL and the producer-side spill are the durable stores; the consumer simply resumes against the freshly-created stream. So flipping `NEXUS_EVENTS_STORAGE` is a delete-and-recreate, **not** a hot in-place update — plan the flip for a low-traffic window so the dropped in-flight buffer is minimal.

### Reading the stream during incidents

NATS CLI on the Hub box: `nats stream info NEXUS_EVENTS` for current message count, byte size, and consumer ack lag; `nats consumer report NEXUS_EVENTS` for per-group `Pending` (un-acked count, the lag signal) and `Redelivered` (poison-pill signal). A `Pending` value that grows without bound usually means the DB writer is stalled (check `packages/nexus-hub/internal/observability/consumer/traffic.go` flush metrics); a `Redelivered` value that grows usually means a structurally-bad message that needs to be located in writer ERROR logs.

# Multi-endpoint coordination architecture

## §1 — Scope

A Nexus deployment can run more than one Hub instance — typically for HA, capacity, or regional placement. Every Hub is independent: it owns its own set of WS-connected Things, its own NATS subscriptions, and its own PostgreSQL connection pool. Things partition across Hubs at connection time and stay with whichever Hub they dialed.

This raises a coordination problem. When the admin updates a config key, the update reaches whichever Hub the Control Plane happens to be talking to (call it Hub A). The Thing that needs the new config may be connected to Hub B. Without an additional channel, Hub B has no way to learn about the update and the Thing's `config_changed` push never fires.

This doc covers the two channels that solve that problem:

- **Cross-Hub MQ fanout** on NATS subject `nexus.hub.signal` — Hub A publishes; every other Hub subscribes; receivers push `config_changed` to whichever Things they have locally connected.
- **Hub-self PostgreSQL LISTEN/NOTIFY** on channel `config_changed` — every Hub replica listens; the writer's transaction emits `pg_notify(config_changed, thingID)`; each replica filters by its own `hub.id` so it only applies updates that target itself.

Sibling docs cover adjacent territory but do not duplicate this one:

- [[service-call-framework]] (A06) — single-Hub WS+HTTP transport, auth, the single `config_changed` message type.
- [[thing-config-sync-architecture]] (A02) — config payload semantics (Cat A vs Cat B, configKey registry, desired/reported version model).
- [[mq-architecture]] (A08) — NATS JetStream layer in general: the other subjects, consumer durability, retention. A07 only uses one specific subject (`nexus.hub.signal`).
- [[jobs-architecture]] (A09) — the scheduler that runs the drift-reconciliation job named in §7.

## §2 — Two coordination channels (overview)

| Channel | Transport | What it solves |
|---|---|---|
| `nexus.hub.signal` | NATS subject (best-effort, non-durable) | Peer Hubs learn about config changes initiated on a different Hub, then push to their local Things. |
| `config_changed` | PostgreSQL LISTEN/NOTIFY channel | Hub itself (registered as a Thing) learns about its own desired-state changes, across every Hub replica. |

The two channels are orthogonal in purpose. A single admin update can fire both: NOTIFY for the Hub-self case (Hub-as-Thing), `nexus.hub.signal` for the peer-Hub case (other Hubs' Things).

## §3 — Cross-Hub MQ fanout (`nexus.hub.signal`)

### 3.1 — Payload

The `HubSignal` struct in `packages/nexus-hub/internal/fleet/manager/config.go` carries:

| Field | Meaning |
|---|---|
| `Action` | the operation kind (e.g. `config_changed`) |
| `SourceHub` | the publishing Hub's `hub.id` — used by subscribers to drop their own echoes |
| `ThingType` | the Thing type the update targets (e.g. `agent`, `ai-gateway`) |
| `ConfigKey` | the configKey being updated |
| `State` | the new config value |
| `Version` | the new `thing.desired_ver` |
| `ThingID` | optional — when set, the receiver sends to one Thing; when absent, broadcasts to the type |
| `Force` | bypasses the client's version-equality short-circuit (see §6) |

### 3.2 — Publisher

`publishHubSignal` in `packages/nexus-hub/internal/fleet/manager/config.go` runs after every `UpdateConfig` transaction commits. The publish is fire-and-forget — failures are logged but do not roll back the DB write or interrupt the in-process WS broadcast.

### 3.3 — Subscriber

`SubscribeHubSignals` in `packages/nexus-hub/internal/ws/signal.go` is started once per Hub at boot. The handler:

1. Filters out messages whose `SourceHub` matches the local `hub.id` — without this filter, every Hub would echo every other Hub's signals back to itself and the cluster would loop.
2. Marshals the signal into the same `ConfigChangedMessage` envelope a local update would have produced.
3. Dispatches to the WS pool:
   - `ThingID` set → `pool.Send(thingID)` for a one-Thing delivery.
   - `ThingID` empty → `pool.Broadcast(thingType)` for type-wide delivery.

A Thing not currently connected to this Hub is a silent drop at this layer — the drift reconciliation job in §7 is the safety net.

## §4 — Hub-self LISTEN/NOTIFY (`config_changed`)

### 4.1 — Why Hub can't use thingclient on itself

The Hub *is* the WebSocket broker. It can't dial its own WS endpoint to learn about its own desired-state changes — there's no client/server split for the Hub-as-Thing case. PostgreSQL LISTEN/NOTIFY is the alternative: every Hub replica connects to the same Postgres, so a `pg_notify` from any writer reaches every listener atomically with the underlying transaction.

The doc-comment at the top of `packages/nexus-hub/internal/self/shadow/manager.go` records this rationale.

### 4.2 — Channel + payload

`ConfigChangedChannel` is the constant `"config_changed"`, declared in `packages/nexus-hub/internal/fleet/shadow/shadow_notify.go`. The NOTIFY payload is the affected `thingID` — small enough to fit inside the 8 KB Postgres notification limit, large enough to identify the row that changed.

### 4.3 — NOTIFY trigger

`notifyConfigChanged` in `packages/nexus-hub/internal/fleet/shadow/shadow_notify.go` issues `pg_notify(config_changed, thingID)` from within the same transaction that updated `thing.desired`. Postgres delivers notifications only after the transaction commits, so listeners never see uncommitted state.

### 4.4 — LISTEN loop

The selfshadow manager in `packages/nexus-hub/internal/self/shadow/manager.go`:

- `Start` runs `applyAll` once (catch up on anything written while the manager was down) then launches the `listen` goroutine.
- `listen` acquires a pooled Postgres connection, issues `LISTEN config_changed`, and blocks on notification arrival.
- Every notification's payload is compared against `m.instanceID` (this Hub's `hub.id`). A mismatch is dropped — Hub A has no reason to apply a desired-state change targeted at Hub B's Thing row.
- A match triggers `applyAll`, which fetches the Hub's own `thing` row, walks the registered `ReloadHandler` set per configKey, invokes each handler, records the outcome, echoes `desired` into `reported`, and commits.

## §5 — Fanout direction matrix

A single admin update — admin → CP → Hub A's `UpdateConfig` — fires **three fanouts in parallel** after the DB transaction commits:

1. **(a) `broadcastConfigChanged`** — Hub A's in-process WS push to every Thing of the target type that is currently connected to Hub A.
2. **(b) PG NOTIFY** — emitted inside the same transaction via `notifyConfigChanged`; every Hub replica's `listen` goroutine receives it and filters by its own `hub.id`.
3. **(c) `publishHubSignal`** — NATS publish on `nexus.hub.signal`; every peer Hub's `SubscribeHubSignals` handler receives it, filters out its own echo, and fans it out to its local WS pool.

Who picks up which path:

| Target | Path |
|---|---|
| A Thing connected to Hub A | (a) — direct local WS push |
| A Thing connected to Hub B | (c) — Hub B receives MQ signal, looks up its local pool, WS pushes |
| Hub A itself (as a Thing) | (b) — Hub A's self-listener matches its own `hub.id` |
| Hub B itself (as a Thing) | (b) — Hub B's self-listener matches its own `hub.id` |

The Hub-as-Thing case never travels (c); the MQ signal subscriber's filter would drop a self-targeted message anyway because the `SourceHub` matches. PG NOTIFY is the only path that closes the Hub-self loop.

## §6 — `RePushConfigKey` (admin force-replay)

### 6.1 — Why Force=true exists

A normal `config_changed` push that carries the same `DesiredVer` the Thing already reported is a no-op on the client — the version-equality short-circuit in `OnConfigChanged` returns immediately. Admin "Re-sync this key" actions need to bypass that short-circuit so the Thing actually re-runs its apply logic even when the DB version did not advance.

`Force=true` on the `HubSignal` is the bypass flag. The Thing-side handler checks `Force` and runs the apply path regardless of version equality, emits a fresh `shadow_report`, and re-stamps the outcomes ledger.

### 6.2 — Delivery path

`RePushConfigKey` in `packages/nexus-hub/internal/fleet/manager/drift.go` (delegating to `rePushConfigKeyForThing`):

1. If `ws.IsConnected(thing.ID)` on this Hub, deliver directly via `ws.Send(thingID, msg)`. Log event: `resync_key_ws`.
2. Otherwise, publish a `HubSignal` with `Force=true` and `ThingID=...` on `nexus.hub.signal`. Whichever peer Hub holds the connection picks it up via §3.3 and forwards. Log event: `resync_key_signal`.
3. If neither path is available (no local connection and no MQ), return `ErrNoDeliveryPath`. Callers (admin endpoint, override-expiry job) surface this to the operator.

### 6.3 — Callers

- Admin "Re-sync this key" action exposed via `packages/nexus-hub/internal/fleet/handler/hubapi/hub_api.go`.
- Override-expiry cleanup job (when a temporary override expires, its replacement is force-pushed so the Thing reapplies the now-canonical value).

## §7 — Drift reconciliation safety net

Neither `nexus.hub.signal` nor `config_changed` NOTIFY is durable. NATS without JetStream durability drops messages if no subscriber is connected at publish time; Postgres NOTIFY only reaches sessions whose LISTEN was active when the NOTIFY committed. The reconciliation job in `packages/nexus-hub/internal/jobs/defs/drift/drift.go` is the backstop.

- Job ID: `config-drift-check`. Scheduled by the Hub scheduler (cadence configured per [[jobs-architecture]]).
- Behaviour: queries the DB for Things where `reported_ver < desired_ver`, calls `RePushConfig` on each, and retries up to 3 times with a 5-minute TTL tracked in Redis. A Thing that exhausts retries gets marked with status `drift` and surfaces on the Nodes admin page.
- This is the only mechanism that recovers from a dropped signal or a NOTIFY race; without it a transient NATS outage during a config write would leave a permanent gap.

## §8 — Failure modes

| Trigger | Behaviour |
|---|---|
| Hub partitioned from NATS | Locally-connected Things keep working (WS push is in-process). Peer Hubs' Things stop receiving updates initiated from this Hub. Updates initiated elsewhere stop reaching this Hub's Things. Drift detector recovers when partition heals. |
| Two Hubs race on the same config update | DB serializes the writes; one wins the `thing.desired_ver` increment. Both Hubs publish to `nexus.hub.signal`. Subscribers drop their own echo via `SourceHub` filter; the surviving signal triggers one push per Thing. |
| `nexus.hub.signal` message dropped | No durability — the message is gone. Drift detector picks up the gap within one scheduler tick. |
| PG NOTIFY missed (listener was reconnecting) | Same recovery: drift detector + `applyAll` on next `Start` catches up. |
| Thing reconnects to a different Hub | Each Hub's `pool.Remove`/`pool.Add` is local; no cross-Hub state migration is required or attempted. The new Hub receives subsequent signals naturally; the old Hub silently drops nothing-to-deliver. |

The recurring theme: **best-effort fast path + DB-driven reconciliation backstop**. Neither channel attempts at-least-once delivery; durability lives in the DB and the drift job catches what slipped.

## §9 — Connection routing (no client-side load balancing)

The Thing's `HubURL` is a single static string, set at boot from `registry.nexusHubUrl` in the service yaml (see [[service-bootstrap-config-architecture]]). The thingclient library does not implement client-side load balancing, failover between candidate Hub URLs, or service discovery.

Multi-Hub HA is therefore an operator concern, not a client-library concern. Typical deployments put a DNS A-record or an L4 load balancer in front of the N Hubs and point every Thing's `HubURL` at the VIP. From the Thing's perspective there is one Hub URL; from the cluster's perspective the LB decides which Hub each connection lands on.

A Thing that needs to migrate to a different Hub URL requires a config change and a restart. There is no in-flight Hub-URL switch.

## §10 — What's not in this doc

- Single-Hub WS+HTTP transport, auth, message lanes — [[service-call-framework]] (A06).
- Config payload semantics (Cat A vs Cat B, configKey registry, desired/reported version model) — [[thing-config-sync-architecture]] (A02).
- Other NATS subjects, JetStream consumer durability, retention policies — [[mq-architecture]] (A08).
- Scheduler details for the `config-drift-check` job (interval, leader election, dedupe across Hub replicas) — [[jobs-architecture]] (A09).
- The Hub's own internal handler / route registration / wiring — [[nexus-hub-internals-architecture]].

## References

- `packages/nexus-hub/internal/ws/signal.go` — `SubscribeHubSignals` + `nexus.hub.signal` subject constant.
- `packages/nexus-hub/internal/fleet/manager/config.go` — `HubSignal` struct, `publishHubSignal`, `UpdateConfig` (the 3-way fanout entry point).
- `packages/nexus-hub/internal/fleet/manager/drift.go` — `RePushConfigKey` + `rePushConfigKeyForThing` + `ErrNoDeliveryPath`.
- `packages/nexus-hub/internal/fleet/shadow/shadow_notify.go` — `notifyConfigChanged` + `ConfigChangedChannel` constant.
- `packages/nexus-hub/internal/self/shadow/manager.go` — `Start`, `listen`, `applyAll`, `ReloadHandler` registry; doc-comment explains LISTEN/NOTIFY rationale.
- `packages/nexus-hub/internal/jobs/defs/drift/drift.go` — `config-drift-check` reconciliation job.
- `packages/nexus-hub/cmd/nexus-hub/wiring/self.go` — selfshadow boot wiring.
- `packages/nexus-hub/internal/config/config.go` — `HubIdentity` struct (`hub.id` field).
- `packages/nexus-hub/internal/fleet/handler/hubapi/hub_api.go` — admin "Re-sync this key" endpoint that calls `RePushConfigKey`.

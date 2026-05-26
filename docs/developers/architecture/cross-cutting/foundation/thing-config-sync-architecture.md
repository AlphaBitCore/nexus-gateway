# Thing config sync architecture

## §1 — Scope

This document covers the **end-to-end flow** of a config change: from the admin click in the Control Plane UI to the moment a target Thing's `OnConfigChanged` callback has applied the new state and Hub has recorded the report. The **data model** (Thing entity, shadow columns, `thing_config_template` / `thing_config_override` cascade, Type A vs Type B configKey semantics, three-channel transport, callback contract) lives in `foundation/thing-model.md` — this doc links there rather than redefining.

A02 (this doc) is the **sequence**; A01 (`thing-model.md`) is the **state**.

What's specifically covered here:
- The 5-stage canonical write→push→apply→report→ack flow.
- Per-stage code anchors: CP admin handler → CP `hubclient.NotifyConfigChange` → Hub `manager.Manager.UpdateConfig` → Hub `pool.Broadcast` / NATS `nexus.hub.signal` → Thing-side `configloader.Loader` → Hub `manager.HandleShadowReport`.
- The **two distinct write paths** (template vs override) and how they differ in fanout semantics.
- Cross-Hub fanout via the `nexus.hub.signal` NATS subject + source-Hub skip rule.
- The agent's per-key HTTP pull subpath (`/api/internal/things/config/<key>?type=agent`) that backs Type B keys, including the three-step dispatch inside Hub's `SingleConfigPull`.
- HTTP fallback (`/api/internal/things/heartbeat` + `/api/internal/things/config`) for Things whose WebSocket is down.
- Hub-self consumption via PostgreSQL LISTEN/NOTIFY (`selfshadow.Manager`).
- Admin "Re-sync this key" replays via the `Force` flag.
- Drift detection + recovery + retry-on-apply-failure invariants.

## §2 — Canonical write→apply→report flow

Five stages, end-to-end. Per-key Type A (config blob is the state) and Type B (invalidation signal — state lives in a dedicated table) are explicitly different in stages 3 and 4 (see §4 and §6).

1. **Admin → CP admin handler**. The CP UI submits to an admin endpoint (e.g. `/api/admin/...` for the specific surface). The handler validates the payload, checks the override blacklist when applicable (`packages/shared/schemas/configtypes/policy/override_policy.go` `IsOverridable`), and stamps an audit row.
2. **CP → Hub via `hubclient.NotifyConfigChange`**. CP packages a `ConfigChangeRequest{ThingType, ConfigKey, State, Action, ActorID, ActorName, SourceIP}` and POSTs to Hub with Bearer service-token auth (`INTERNAL_SERVICE_TOKEN`). The client retries up to 3 times with exponential backoff on transient errors. The 4 server Things and the agent never call this — only CP does. Source: `packages/control-plane/internal/platform/hub/client.go` `Client.NotifyConfigChange`.
3. **Hub `manager.Manager.UpdateConfig`** runs the 6-step write (template path) inside a single `pgx.Tx`:
   - Step 1: `UpsertConfigTemplate` bumps `thing_config_template.version`.
   - Step 2: `UpdateDesiredForType` `jsonb_set`s the new state into `thing.desired` for **every Thing of the type at once**, sets `desired_ver` to `COALESCE(MAX(desired_ver), 0)+1` (per-type monotonic), and emits `pg_notify(config_changed, thingID)` for every affected row.
   - Step 4: `InsertConfigChangeEvent` writes the audit row.
   - Tx commit. Then post-commit best-effort:
   - Step 3: Redis cache the desired key (`nexus:desired:<type>:<configKey>`).
   - Step 5: `broadcastConfigChanged` builds a `ConfigChangedMessage` and calls `pool.Broadcast(thingType, ...)`.
   - Step 6: `publishHubSignal` marshals a `HubSignal` envelope and `mq.Publish` it to NATS subject `nexus.hub.signal`.
   - Source: `packages/nexus-hub/internal/fleet/manager/config.go` `UpdateConfig`.
4. **Hub → Thing push** happens via two parallel channels:
   - **Local WebSocket**: every Thing of the matching type currently connected to *this* Hub replica gets the frame (`pool.Broadcast`).
   - **Cross-Hub NATS**: peer Hub replicas receive the `nexus.hub.signal` and broadcast onto their own local pools (see §3).
5. **Thing applies + reports**. The Thing's `OnConfigChanged` callback (registered through `configloader.Loader.Handler()`) decides Type A or Type B per key (§4), applies, and emits `shadow_report` over WS. Hub's `manager.HandleShadowReport` UPDATEs `thing.reported` + `thing.reported_ver` + `thing.reported_outcomes` and acks. The admin UI's Nodes / Configuration tab then renders the convergence (drift cleared, version equal).

## §3 — Cross-Hub fanout (NATS `nexus.hub.signal`)

Hub runs as multiple replicas in production. A config write hits exactly one replica (whichever CP's HTTP client load-balances to). For Things connected to other replicas, the write Hub fans out via the NATS subject `nexus.hub.signal`.

**Subject + envelope**:
- Subject: `nexus.hub.signal` (single subject for all config signals).
- Envelope: `manager.HubSignal{Action, SourceHub, ThingType, ConfigKey, State, Version, ThingID, Force}`. `Action` is currently always `"config_changed"`. `ThingID` empty means "broadcast to all Things of `ThingType`"; non-empty targets a single Thing (used by admin replay — §7).
- Source: `packages/nexus-hub/internal/fleet/manager/config.go` `HubSignal`.

**Subscriber**: every Hub replica runs `ws.SubscribeHubSignals` (`packages/nexus-hub/internal/ws/signal.go`). On receipt:
- **Source-Hub skip**: `if sig.SourceHub == hubID { return nil }`. The originating Hub deliberately doesn't re-process its own publish; it already pushed locally via `pool.Broadcast` in step 5 of `UpdateConfig`.
- Build a `ConfigChangedMessage` from the signal.
- Route to local pool: `pool.Send(thingID, ...)` if `ThingID` is set, else `pool.Broadcast(thingType, ...)`.

**Failure mode**: NATS publish is best-effort (`m.logger.Warn("publish hub signal failed", "error", err)` on error). A NATS outage means peer Hubs don't see the new config until the affected Things' next reconnect or HTTP-fallback heartbeat poll (§6).

## §4 — Frames on the wire

Two structs cover all Hub→Thing config push traffic.

**`ConnectedMessage`** (sent on every successful WS handshake, before `Run` begins):
- Source: `packages/nexus-hub/internal/ws/server.go` (struct sent at `ws.handleConnect`).
- Fields: `Type` (= `"connected"`), `HubID`, `Desired map[string]any` (full per-key snapshot, value is `{state, version}`), `DesiredVer`.
- Built from `manager.RegisterThing`'s `RegisterResponse{Desired, DesiredVer}`.
- This is the **full-snapshot apply** the thingclient relies on at reconnect (`packages/shared/transport/thingclient/client.go` `connectWS`).

**`ConfigChangedMessage`** (per-key delta, sent both by local broadcast and by NATS subscriber relay):
- Source: `packages/nexus-hub/internal/fleet/manager/config.go` `ConfigChangedMessage`.
- Fields: `Type` (= `"config_changed"`), `ConfigKey`, `State json.RawMessage`, `DesiredVer`, `Force bool` (omitempty).
- **No `Desired` full-map field** — `config_changed` is strictly per-key delta. Full snapshots only travel on the "connected" frame.
- `Force=true` is set exclusively by admin re-sync replays (§7).

## §5 — Template write vs override write

Both paths land in the same `OnConfigChanged` callback on the Thing side, but they have **different fanout semantics on the Hub side**.

**Template write** (`Manager.UpdateConfig` → `UpdateDesiredForType`):
- Affects: every Thing of one `ThingType` simultaneously (the new state is `jsonb_set`'d into every `thing` row of that type).
- pg_notify: one NOTIFY per affected row.
- Post-commit fanout: WS `pool.Broadcast(thingType, ...)` + NATS `publishHubSignal` (so peer Hubs reach their own Things).
- Cross-Hub coverage: ✓ — Things on any Hub replica get the change promptly.

**Override write** (`Manager.SetOverride` / `ClearOverride`):
- Affects: a single Thing (per-Thing whole-key replacement, computed by re-merging templates ⊕ override).
- Tx body: `UpsertOverride` (or `DeleteOverride`) + `recomputeDesiredTx` + `WriteDesiredAndBumpVer` + `insertAdminAuditLog`. `WriteDesiredAndBumpVer` emits one pg_notify for that thing's ID inside the Tx.
- Post-commit fanout: **`m.RePushConfigKey(thingID, configKey)`** runs at the end of both `SetOverride` and `ClearOverride` (`packages/nexus-hub/internal/fleet/manager/override.go`). `RePushConfigKey` is the same single-Thing push helper §10 uses for admin "Re-sync this key" replays: WS-local first (`m.ws.Send(thingID, ...)`), NATS fallback (`HubSignal{ThingID: thingID, ...}`) when the Thing is connected to a peer Hub. The override push always sets `Force=true` so the Thing's `applyConfig` short-circuit doesn't drop a change at the same `DesiredVer`.
- Cross-Hub coverage: ✓ — overrides reach the target Thing on the same single-Thing fanout path as admin re-sync replays, regardless of which Hub holds the WS connection.
- Push failure (`ErrNoDeliveryPath` or NATS publish error) is logged at WARN as `override_push_failed` but does **not** roll back the override write. The drift detection job (§11) re-converges any Thing whose `reported_ver` lags behind `desired_ver`.

In short: **override has full push semantics identical to template fanout**, plus `Force=true` so the receiver always re-applies + re-reports. The pg_notify on `config_changed` exists as the Hub-self LISTEN trigger (§9) but is **not** the propagation mechanism for the target Thing.

## §6 — Thing-side apply via `configloader.Loader`

Every Thing wires its receivers through `packages/shared/transport/configloader/configloader.go`. The Loader owns the dispatch table, the typed-or-raw decoding, and the `needsPull` HTTP-fetch escape hatch. Each service's `cmd/<svc>/configdispatch/` builds its Loader from per-key applier closures and then installs `tc.OnConfigChanged(l.Handler())`.

Three registration APIs:
- **`Register[V any]`** — generic, auto-unmarshal into `V`. Used for typed Type A keys.
- **`RegisterRaw`** — raw `[]byte` apply closure. Used when the receiver wants to control decoding.
- **`RegisterRawPull`** — raw `[]byte` apply, but Hub pushes a `{needsPull: true}` stub instead of the real state. The Loader detects the stub and calls an external puller closure (HTTP fetch) to get the real bytes before invoking the apply. Currently only used by the agent (§7).

`Loader.Handler()` returns a function compatible with `thingclient.Client.OnConfigChanged`. The thingclient invariants from A01 §7 (`applyConfig`'s no-callback skip, version-equality skip, error-no-report) apply identically.

Per-service wiring sites:
- AI Gateway: `packages/ai-gateway/cmd/ai-gateway/configdispatch/configdispatch.go`.
- Control Plane: `packages/control-plane/cmd/control-plane/configdispatch/`.
- Compliance Proxy: `packages/compliance-proxy/cmd/compliance-proxy/configdispatch/`.
- Agent: `packages/agent/cmd/agent/configdispatch.go` (plus `configappliers.go` for the per-key apply functions).

**#115 — `streaming_compliance` handler pattern (all three services)**:
the shadow handler routes the raw payload directly through
`streampolicy.Store.ApplyShadowState(ctx, raw)`. No per-service DB
re-read and no per-server setter wrapper. The Store is hot-swappable;
SSE-hot-path readers call `Store.Get()` and atomically see the new
policy. Three-service alignment — agent, compliance-proxy, and
ai-gateway use the same `*streampolicy.Store` instance seeded via
`streampolicy.BootStore` at boot (`packages/shared/transport/streaming/policy/store.go`).

## §7 — Agent HTTP-pull subpath

The agent is the only Thing that uses `RegisterRawPull`. For each Cat B key (the agent set: `exemptions`, `hooks`, `interception_domains`, `payload_capture`, `streaming_compliance`, `installed_rule_packs`, `user_context`, `killswitch`), Hub pushes a `{needsPull: true}` stub over WS; the Loader then calls the HTTP puller closure.

**Puller URL** (`packages/agent/cmd/agent/configdispatch.go` `agentPullConfig`):

    GET https://<hub>/api/internal/things/config/<configKey>?type=agent

With `Authorization: Bearer <deviceToken>` and `X-Thing-Id: <thingID>` headers.

**Hub-side handler** (`packages/nexus-hub/internal/fleet/handler/hubapi/internal_things.go` `SingleConfigPull`) — two-branch dispatch:
- **Cat B loader path**: if `(thingType, configKey)` has a registered Cat B loader (`CatBRegistry`), invoke `loader.Load(ctx, thingID)` and return `{configKey, state, version, source: "loader"}`. Loaders aggregate the authoritative payload from CP-owned business tables (e.g. `HookConfig`, `compliance_exemption_grant`); they read DB rows that CP writes, so the agent gets the live CP-side state.
- **Template fallback**: no loader registered → `GetConfigTemplate(thingType, configKey)` and return `{configKey, state, version, source: "template"}`. This covers Cat A inline keys and Hub instances that haven't wired the Cat B registry.

**Loader inventory** (`packages/nexus-hub/internal/compliance/catbagent/`, one file per agent Cat B key): `exemptions.go`, `hook_config.go`, `installed_rule_packs.go`, `interception_domains.go`, `payload_capture.go`, `streaming_compliance.go`, `user_context.go`. Wired into the storage layer at `packages/nexus-hub/cmd/nexus-hub/wiring/storage.go`.

**Why agent doesn't read PostgreSQL directly**: the agent never holds DB credentials and never has a DB route. Every byte of agent state comes from Hub over HTTP.

## §8 — HTTP fallback (server-side Things)

When a server Thing's WebSocket is down, it falls back to HTTP polling. Source: `packages/shared/transport/thingclient/http.go`.

- **Heartbeat**: `POST /api/internal/things/heartbeat` carries `heartbeatRequest{id, status:"online", reportedVer}`. Hub returns `heartbeatResponse{ack, desiredVer, desired (optional map)}`. The Thing compares its `reportedVer` against the returned `desiredVer`; mismatch triggers a config pull.
- **Config pull**: `GET /api/internal/things/config?type=<thingType>&id=<thingID>` (`BulkConfigPull` on Hub). Returns `{configs: {key: {state, version}}, desiredVer}`. Critically, when `id` is supplied, per-key `version` = the Thing's `thing.desired_ver` (per-type monotonic) — so the same comparison the WS path uses still works.
- **Shadow report**: `POST /api/internal/things/shadow` carries the same `{reported, reportedVer, reportedOutcomes}` payload as the WS `shadow_report` frame — byte-for-byte compatible parsing on Hub.

Prometheus counter `httpFallbackReqs{type=...}` tracks each kind (`register`, `heartbeat`, `shadow`, `config_pull`).

## §9 — Hub-self special path (`selfshadow.Manager`)

Hub itself is a `Thing` (one row in `thing` for each Hub replica's `instanceID`). It does not run a WebSocket client pointed at itself; it *is* the broker. Instead, Hub consumes its own row via PostgreSQL LISTEN/NOTIFY.

- **Channel**: `config_changed` (constant defined in `packages/nexus-hub/internal/fleet/shadow/shadow_notify.go` `ConfigChangedChannel`).
- **Emitter**: every Hub write that mutates `thing.desired` calls `notifyConfigChanged(tx, thingID)` inside the same `pgx.Tx`. This applies to both `UpdateDesiredForType` (template path) and `WriteDesiredAndBumpVer` (override path). pg_notify is delivered only on commit, so rollback discards.
- **Consumer**: `packages/nexus-hub/internal/self/shadow/manager.go` `Manager`. Each Hub replica runs one. It LISTENs, parses the payload (which is just the `thingID`), and filters to act only when `thingID == m.instanceID` — i.e. **only when the changed Thing is this Hub's own row**.
- **Dispatch**: matching notifications trigger registered `ReloadHandler`s. Outcomes mirror thingclient: `recordOutcome(key, ver, err)` tracks the last successful `appliedAt`/`appliedVersion` per key and the most recent error.

The pg_notify channel is therefore a *Hub-internal* mechanism. It does **not** push other Things' updates — those go through WS broadcast + NATS as described in §3 and §5.

## §10 — Force-resync admin replay

Admins can re-push a single key to a single Thing even when nothing has changed — useful for recovering a drifted `thing.reported` without bumping the template version.

- **Entry point**: CP admin endpoint → Hub `manager.Manager.RePushConfigKey(thingID, configKey)`. Source: `packages/nexus-hub/internal/fleet/manager/drift.go`.
- **Build**: `rePushConfigKeyForThing` reads `thing.desired[configKey]`, marshals it, and constructs a `ConfigChangedMessage` with `Force: true`.
- **Delivery preference** (also covers cross-Hub):
  - If the target Thing is locally connected (`m.ws.IsConnected(thingID)` and `m.ws.Send(thingID, ...)` succeeds), deliver over WS directly.
  - Else publish a `HubSignal{Action: "config_changed", ThingID, Force: true, ...}` to `nexus.hub.signal` so the peer Hub that holds the WS connection can deliver.
- **Thing-side effect**: with `Force=true` on the wire, `applyConfig`'s `desiredVer <= reportedVer` short-circuit (A01 §7) is bypassed. The callback runs and emits a fresh `shadow_report` even at the same version. Without `Force`, an admin replay at the same `DesiredVer` would be silently dropped on the client.

## §11 — Drift detection + recovery

- **Triggers**:
  - HTTP fallback heartbeat returning a `desiredVer` greater than the Thing's `reportedVer` (Thing-driven discovery; §8).
  - Periodic drift job: `packages/nexus-hub/internal/jobs/defs/drift/drift.go`. Scans `thing` for `(status = 'online' AND desired_ver != reported_ver)` rows and flips them to `status = 'drift'` via `UpdateThingStatus`.
- **Auto-recovery**: the next successful `HandleShadowReport` that catches `reported_ver` up to `desired_ver` flips `status` back to `'online'` via the SQL `CASE WHEN status = 'drift' AND $3 >= desired_ver THEN 'online' ELSE status END` in the registry update (`packages/nexus-hub/internal/fleet/store/thing_registry.go`).
- **Manual recovery**: admin uses the "Re-sync this key" action (§10).
- **Apply-failure retry**: if the Thing's callback returns an error (A01 §7), it does **not** send a `shadow_report`. The next config push or the next reconnect snapshot triggers a fresh attempt — there is no per-failure retry timer on either side, just the next-event reconvergence loop.

## §12 — Status state machine writers

`thing.status` ∈ `{enrolled, online, offline, drift, revoked}`. Each value is written by a specific code path:

- `enrolled` — default at insert (`tools/db-migrate/schema.prisma` `Thing.status` default).
- `online` — set by callers of `UpsertThingEnrollmentWithDesiredVer` who pass `Status: "online"`. Live call sites: `manager.RegisterThing` (HTTP enrollment + WS connect) and `selfreg.Manager` (Hub's own row at boot). Hub flips it via the upsert's `ON CONFLICT` path on every successful registration, which also resets `process_started_at` + clears `reported_outcomes` when the prior status was not `online`.
- `offline` — two writers:
  - `manager.Manager.MarkOffline` (called from `ws.Server` on WS disconnect, `packages/nexus-hub/internal/fleet/manager/enrichment.go`).
  - `store.MarkStaleOffline` (called from the periodic `jobs/defs/drift/stale_thing` job for Things whose `last_seen_at` exceeds a per-type threshold).
- `drift` — set by `jobs/defs/drift/drift.go` `UpdateThingStatus(thingID, "drift")` when a periodic scan detects `desired_ver != reported_ver` while `status='online'`.
- `revoked` — written by admin-driven revocation flows (out of scope here; covered alongside enrollment in the agent-enrollment doc).

## References

- CP admin → Hub bridge — `packages/control-plane/internal/platform/hub/client.go`
- Hub config write orchestrator — `packages/nexus-hub/internal/fleet/manager/config.go`, `override.go`, `drift.go`
- Hub fleet store — `packages/nexus-hub/internal/fleet/store/thing_registry.go`
- Hub shadow store — `packages/nexus-hub/internal/fleet/shadow/`
- Hub overrides store — `packages/nexus-hub/internal/fleet/overrides/`
- Hub agent Cat B loaders — `packages/nexus-hub/internal/compliance/catbagent/`
- Hub agent Cat B wiring — `packages/nexus-hub/cmd/nexus-hub/wiring/storage.go`
- Hub HTTP device-facing API — `packages/nexus-hub/internal/fleet/handler/hubapi/internal_things.go`
- Hub WebSocket broker — `packages/nexus-hub/internal/ws/`
- Hub NATS signal subscriber — `packages/nexus-hub/internal/ws/signal.go`
- Hub-self LISTEN consumer — `packages/nexus-hub/internal/self/shadow/manager.go`
- Thing client (WS / HTTP fallback / shadow report) — `packages/shared/transport/thingclient/`
- Shared config-loader abstraction — `packages/shared/transport/configloader/`
- Per-service configdispatch wiring — `packages/{ai-gateway,control-plane,compliance-proxy,agent}/cmd/<svc>/configdispatch/`
- Override blacklist — `packages/shared/schemas/configtypes/policy/override_policy.go`
- Periodic drift / offline-sweep jobs — `packages/nexus-hub/internal/jobs/defs/drift/`
- Data model (Thing, shadow, template/override, Type A/B, callback contract, terminology) — `docs/developers/architecture/cross-cutting/foundation/thing-model.md`

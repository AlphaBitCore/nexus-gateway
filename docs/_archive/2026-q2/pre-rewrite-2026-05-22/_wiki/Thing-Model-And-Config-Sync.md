# Thing Model And Config Sync

*Audience: contributors working on any service that registers with Hub, reads shadow config, or touches the config-sync flow.*

The Thing model is the central abstraction of the Nexus Gateway platform. Every managed entity — each of the four server services and every Desktop Agent install — is a Thing (a node in the Hub-coordinated service mesh): a row in Hub's `thing` table with a typed extension, a per-entity config shadow, and a pull-only config-sync contract. Configuration flows from the admin UI through Hub to each Thing; Things pull their config on boot and on each change-signal. Hub never pushes full state. The pull path and the boot path share the same callbacks and apply contract, eliminating the "initial load vs live update" divergence problem. This page explains the data model, the shadow structure, the category system, and the terminology boundary that separates internal vocabulary from user-facing surfaces.

---

## The Thing abstraction

Five Thing types exist in the platform. All five participate in the same registry, shadow, and heartbeat contract.

| Thing type | Extension table | User-facing label |
|---|---|---|
| Hub (self) | `thing_service` | Platform Hub |
| Control Plane | `thing_service` | Control Plane service |
| AI Gateway | `thing_service` | AI Gateway service |
| Compliance Proxy | `thing_service` | Compliance Proxy service |
| Desktop Agent | `thing_agent` | node (device) |

Backend services share the `thing_service` extension (fields: `service_kind`, `process_uid`, `version`). Agents use `thing_agent` (fields: `device_id`, `os`, `os_version`, `hostname`, `user_id`).

Hub is itself a Thing in its own registry. This buys uniform observability and a single config model across all five types. Adding a new managed entity type means adding an extension table, not building a parallel system.

**Identifiers.** Each Thing has a stable `thing_id` (opaque, globally unique — used in URLs, MQ envelopes, audit rows). Agents additionally have a `device_id` (shown to admins; the platform uses `thing_id` internally for the same device). Both identifiers are stable across restarts.

**Status.** A Thing's `status` is one of `online`, `degraded`, or `offline`. Status is computed by Hub (not stamped by the Thing itself) from heartbeat timestamps and reported-state content. A `degraded` status means recent heartbeats arrived but reported state shows partial apply failures.

## Shadow: desired, reported, and drift

Each Thing has a **per-key** shadow stored as JSONB columns directly on the `thing` row in PostgreSQL. There is no separate `thing_shadow` table — earlier architecture doc revisions named one; the shadow has always been on the `thing` row.

Three JSONB columns:

- `desired` — full desired-state snapshot: what the admin wants applied. Written only by Hub.
- `reported` — full applied-config snapshot last reported by the Thing, including per-key apply errors and `processStartedAt` (E27 outcomes).
- `reported_outcomes` — per-config-key outcome ledger: `{ key: { appliedAt, appliedVersion, applyError } }`.

A snapshot looks like:

```
desired:  { hooks/v: 17, routing/v: 42, killswitch/v: 3, agent_settings/v: 8 }
reported: { hooks/v: 17, routing/v: 41, killswitch/v: 3, agent_settings/v: 8,
            routing/applyError: "schema validation failed at field X",
            routing/processStartedAt: "2026-05-21T14:00:00Z" }
```

The gap between `desired.version` and `reported.version` for a key is **drift**. Drift is surfaced in the Control Plane UI on the "Config Sync" page (the user-facing term — see §Terminology below). Drift is a normal transient state while a change propagates; it becomes an alert when it persists beyond a per-Thing-type threshold.

Versions are monotonic per key. The Thing reports the version it has **applied**, not the version it intends to apply. This closes the apply-receipt loop: Hub sees exactly which version each Thing has running.

## Category A / B / C key classification

Each shadow key belongs to one category, declared in `thing_config_template`:

| Category | Wire shape in shadow | Propagation latency | Typical keys |
|---|---|---|---|
| **Cat A — inline** | Full value carried in the shadow JSON | Milliseconds — rides the change-signal directly | Kill switch toggle, emergency-passthrough flags |
| **Cat B — pull-on-signal** | Only `{ version, needsPull: true }` in the shadow | Sub-second — signal + explicit pull round-trip | Hook config, routing rules, agent settings, credentials |
| **Cat C — template-fallback** | Template default + per-Thing override, pulled same as Cat B | Sub-second | Defaults from `thing_config_template`; per-Thing overrides via `thing_config_override` |

**Cat B invariant (binding).** Every Cat B key MUST carry `needsPull: true` in the shadow. A registered Cat B key without that flag is invisible to the pull path — the Thing never fetches it. This caused the #91 production bug: four registered Cat B keys were missing from agent shadows; the agents never pulled them and the apply receipts never closed.

**Cat A examples** — kill switch and emergency-passthrough flags are Cat A because they must propagate in milliseconds for incident response. The full bypass config blob riding the change-signal is intentional: sub-second global bypass is the design requirement.

## The pull-only change-signal flow

The full end-to-end config propagation flow, from admin save to Thing apply:

1. Admin saves a change in the Control Plane UI.
2. The Control Plane validates the change and forwards the write to Hub (`POST /api/hub/shadow/...`).
3. Hub validates, persists to PostgreSQL, and increments the key's version counter.
4. Hub looks up all Things affected by this key and emits a change-signal over each Thing's existing WebSocket session. The signal carries only a minimal payload identifying which keys changed — not the full config values.
5. Each Thing receives the signal and pulls the new values for the changed Cat B keys from Hub (`GET /api/hub/shadow/...`). Cat A values rode the signal itself.
6. Each Thing validates the payload against its local schema.
7. Each Thing applies via an atomic-pointer swap (`atomic.Pointer[...]`) — no blocking of in-flight requests during config reload.
8. Each Thing stamps the reported state: `{ version, applied_at, processStartedAt, applyError: nil }` on success, or `applyError` on failure with the previous snapshot retained.

Hub sees the reported stamp on the next heartbeat and computes the new drift state. If `applyError` is set, the admin sees the error on the Config Sync page.

**WebSocket primary, HTTP fallback.** Change-signals and heartbeats run over WebSocket. When the WS link is down, Things fall back to HTTP heartbeat and HTTP pulls. No Redis pub/sub is involved — the Valkey instance is a cache only.

**Cold-start.** On boot, a Thing:
1. Connects to Hub (mTLS for agents; bootstrap token for server services).
2. Receives its full shadow (Cat A inline values + a `needsPull` list for Cat B + Cat C template-resolved defaults).
3. Pulls all Cat B keys.
4. Applies and reports.
5. Enters the live change-signal loop.

Cold-start uses the same callbacks and apply contract as live updates. There is no separate "initial load" code path — this is why the pull-only model eliminates cold-start divergence.

## Apply path and failure handling

The `OnConfigChanged` callback registered per key is responsible for:
- Validating the payload (idempotent — safe to re-run on the same version).
- Atomic-pointer swap of the in-memory snapshot.
- Returning success or failure (which becomes the reported stamp).

Callbacks are wired in service `main.go` files (`packages/<service>/cmd/<service>/main.go`). They must be registered before the WebSocket session is established; change-signals for a key arrive only if the corresponding callback was registered first.

If apply fails:
- `applyError` is set on the reported stamp.
- The previous in-memory snapshot is kept (the old config remains active).
- Hub surfaces the failure on the Config Sync page as drift.
- No alert is fired for a single failure — the staleness-threshold alert is what escalates.

## Three independent paths (audit invariant)

When adding or changing a shadow key, three independent code paths must stay aligned:

1. **`packages/control-plane/internal/platform/configreconcile/`** — the CP-side drift watchdog. Periodically compares the Control Plane's source-of-truth config tables against `thing.desired.<key>` and re-emits `Hub.NotifyConfigChange` to heal divergence. Runs in the Control Plane (not Hub). Wired at `packages/control-plane/cmd/control-plane/wiring/reconcile.go`.

2. **`tools/db-migrate/seed/seed.ts`** — the canonical Prisma seed for fresh development DBs. Factory defaults for `thing_config_template` rows live here (or in `seed/data/seed-baseline.sql` for large baseline data).

3. **`tools/db-migrate/prisma/migrations/**`** — durable schema migrations that ship with each release.

Auditing only the migrations directory produces false positives: `configreconcile` and `seed.ts` can drift independently. Always check all three paths when reviewing a shadow key addition or change.

## Status model

A Thing's `status` field is computed by Hub from heartbeat data — not stamped by the Thing itself. This keeps the source of truth for health state in one place.

| Status | Condition |
|---|---|
| `online` | Recent heartbeat or active WebSocket session; all reported keys are at desired versions. |
| `degraded` | Heartbeat received but reported state shows partial apply failures (one or more keys have `applyError` set). |
| `offline` | No heartbeat and no WebSocket session for more than the staleness threshold (per Thing type). |

Status transitions are computed by Hub. The Thing reports `last_seen` and its applied-config version map; Hub decides the bucket. Degraded status surfaces as a warning on the Nodes page in the CP UI — the admin can inspect which config keys failed to apply and why.

## Failure modes and recovery

| Failure | Behavior |
|---|---|
| WS link drops | Thing falls back to HTTP heartbeat; Cat B pulls use HTTP. |
| Cat B key missing `needsPull: true` | Pull never triggers; key never reaches the Thing. Fix: ensure `needsPull: true` in `thing_config_template`. |
| Apply error | Reported `applyError` set; previous snapshot retained; Hub surfaces drift on Config Sync page. |
| Hub down at boot | Thing waits; does not serve traffic with an empty config (fail-closed cold-start). |
| Hub down after boot | Thing keeps last applied config; no new config propagates until reconnect. |
| Concurrent writes to same key | Atomic versioning + monotonic counters ensure convergence; ordering across keys not guaranteed but consistent per-key. |

## Terminology boundary (binding)

The Thing model is an **internal** architecture kernel. The vocabulary on its inside (code, DB, developer docs) is deliberately different from the vocabulary on its outside (admin UI, API responses, product docs, error messages). The CI script `npm run check:terminology` (via `scripts/check-terminology.sh`) enforces this boundary in `docs/users/`.

| Internal (code / DB / contributor docs) | User-facing (UI / API / product docs) |
|---|---|
| Thing | node / service / device (by context) |
| Shadow | config sync |
| desired | target config |
| reported | applied config |
| drift | out of sync |
| Cat A / Cat B / Cat C | (not surfaced) |
| pull-only model | (not surfaced) |

Concepts pages and contributor-audience wiki pages (including this one) may use internal vocabulary with a first-mention gloss. Feature pages, operator docs, and getting-started pages must use the right (user-facing) column exclusively.

## Sources

The Thing model lives across several packages. When contributing to the config sync flow, these are the load-bearing files:

- `packages/shared/transport/thingclient/` — the client library used by every Thing. Handles WS connection, heartbeat, Cat B pull, `OnConfigChanged` callback dispatch, reported-state stamp.
- `packages/shared/schemas/configtypes/` — hand-maintained Go type definitions for shadow keys. Not code-generated; must be kept in sync with `thing_config_template` rows manually.
- `packages/nexus-hub/internal/fleet/manager/` + `internal/fleet/store/` — Hub-side Thing Registry + status computation engine.
- `packages/nexus-hub/internal/fleet/shadow/` — shadow store + change-signal fan-out to affected Things.
- `packages/nexus-hub/internal/fleet/handler/hubapi/` — shadow CRUD HTTP API (e.g., `hub_api_overrides.go`).
- `packages/control-plane/internal/platform/configreconcile/` — CP-side drift watchdog (wired at `wiring/reconcile.go`).
- `tools/db-migrate/prisma/schema.prisma` — `Thing`, `ThingService`, `ThingAgent`, `thing_config_template`, `thing_config_override` model definitions.
- `tools/db-migrate/seed/seed.ts` + `seed/data/seed-baseline.sql` — canonical seed including default `thing_config_template` rows.

When adding a new shadow key, the standard change involves: (1) add the configKey constant to `packages/shared/schemas/configkey/`, (2) add the type to `packages/shared/schemas/configtypes/`, (3) update `thing_config_template` in a migration and in `seed.ts`, (4) register the `OnConfigChanged` callback in the consuming service's `main.go`, and (5) verify the CP-side `configreconcile` watcher covers the new key.

## The #91 production incident — Cat B invariant in practice

The #91 production bug is worth understanding as a concrete example of the Cat B invariant:

**What happened.** Four Cat B keys were registered in `thing_config_template` for `thing_agent` (the Desktop Agent). However, the shadow rows for these keys were missing the `needsPull: true` flag. When agents received change-signals, the `thingclient` pull logic checked `needsPull` — finding it absent, it skipped the pull. The keys never reached the agents. Apply receipts never closed the loop. The CP UI Config Sync page showed perpetual drift for these keys on all enrolled agents.

**Root cause.** The `thing_config_template` migration added the rows but omitted `needsPull: true` in the JSONB config blob. The seed was not checked against the migration, so the inconsistency was not caught until production traffic revealed the symptom.

**Lesson.** When reviewing a migration that adds or changes a `thing_config_template` row for a Cat B key, always verify: (a) `needsPull: true` is present, (b) the same row appears in `seed.ts` with the same flag, (c) the CP-side `configreconcile` watcher covers the key so any drift from the above is healed automatically.

## Configuration template vs per-Thing override

Two tables govern what a Thing receives in its `desired` shadow:

- `thing_config_template` — per-(type, config_key) defaults. When Hub fan-outs the shadow to a Thing, it starts from the template defaults for that Thing type.
- `thing_config_override` — per-Thing overrides merged into `desired` by Hub before push. Allows an individual service instance or agent to diverge from the fleet default without changing the template.

Overrides are rare in practice. The current primary use case is per-agent `trafficUploadLevel` (e.g., a specific agent on a high-risk device set to `all` while the fleet default is `processed`).

## Valkey (Redis) role in the Thing model

A common misconception: "Valkey must be involved in config sync, since it's a cache." Valkey is explicitly **not** in the config propagation path. The complete list of what Valkey holds for the Thing model:

| Key pattern | What it holds | TTL |
|---|---|---|
| `thing:desired:<thing_id>:<key>` | Desired-state cache for fast shadow reads | 5 minutes |
| `session:<token_hash>` | Admin session state | Configurable (default 24h) |
| `iam_cache:<user_id>:<org_id>` | IAM policy evaluation cache | 60 seconds |
| `quota:<vk_id>:<window>` | VK rate-limit and cost counters | Window duration |
| `cert:<domain>` | TLS cert cache for Compliance Proxy | 1 hour |

The `thing:desired:<thing_id>:<key>` cache is a read-accelerator for Hub's shadow API — so that every Cat B pull does not hit PostgreSQL. It does not replace PostgreSQL as the authoritative shadow store. If this cache is evicted or Valkey is restarted, Hub simply re-reads from PostgreSQL.

Config propagation flows: PostgreSQL → Hub process memory → WebSocket change-signal → Thing. Valkey is a sidecar cache, not a bus.

## heartbeat and drift detection

Hub detects Thing drift and health degradation through the heartbeat mechanism. Each Thing's `thingclient` sends a heartbeat every 10 seconds (configurable). The heartbeat payload contains:

```json
{
  "thing_id": "...",
  "reported": {
    "hooks/v": 17,
    "routing/v": 42,
    "killswitch/v": 3,
    "agent_settings/v": 8
  },
  "last_seen": "2026-05-21T14:30:00Z"
}
```

Hub's `thing.staleness_sweep` job (every 30 seconds) evaluates each Thing:
1. Checks `last_seen` against the staleness threshold (per Thing type: server services 30s, agents 60s).
2. If `last_seen` is stale → transition to `offline`.
3. If `last_seen` is fresh → compare `reported` versions against `desired` versions from the shadow.
4. If any `reported.version < desired.version` with no in-progress apply → compute drift age.
5. If drift age > alert threshold → status = `degraded`.

The status computed here is what the CP UI Nodes page shows. Operators who see a `degraded` node can click through to the Config Sync tab for that node and see exactly which key(s) have stale versions and what `applyError` (if any) the Thing reported.

## Cat A deep-dive: kill switch and emergency passthrough

Kill switch and emergency passthrough are the two Cat A keys in active use. Their Cat A classification means:

1. The full bypass config blob is serialized into the change-signal payload.
2. The `thingclient` applies the Cat A values as soon as the change-signal arrives — no separate pull round-trip.
3. Sub-second propagation even to the slowest Thing, because the change-signal itself carries everything needed to apply.

The blob shape for emergency passthrough (simplified):

```json
{
  "killswitch": {
    "global": { "enabled": true, "bypassHooks": true, "bypassCache": false, "bypassNormalize": false, "expiresAt": "2026-05-21T15:00:00Z" },
    "adapters": [],
    "providers": []
  }
}
```

When `enabled: true` arrives on the change-signal, the AI Gateway atomically swaps its in-memory bypass config and every subsequent request checks the bypass flags before the hook pipeline. The atomic-pointer swap ensures no request sees a partial state (e.g., `enabled: true` with `bypassHooks: false` still in memory from the previous state).

When the Hub `kill_switch.reconcile` job fires and detects `expiresAt < now`, it sets `enabled: false`, writes the update to PostgreSQL, and emits a new change-signal. The data-plane services apply the `enabled: false` blob exactly the same way they applied `enabled: true`. Enforcement resumes automatically — no admin action required.

## Identifiers: `thing_id` vs `device_id`

Desktop Agents have two identifiers that serve different purposes:

| Identifier | Scope | Who sets it | Where it appears |
|---|---|---|---|
| `thing_id` | Universal — same as all server Things | Hub generates on first enrollment | MQ envelopes, audit rows, URL path params, config pull calls |
| `device_id` | Agent-only — the admin-visible node identifier | Agent generates (UUID v4) and submits with the enrollment CSR | CP UI Nodes page, `thing_agent.device_id` column, user-facing node lists |

The platform uses `thing_id` internally for all operations — routing change-signals, building audit rows, computing drift. `device_id` is displayed to admins because it is human-memorable and stable across re-enrollment (the device generates it once and persists it to the platform keystore). `thing_id` changes on each enrollment cycle.

This means an admin who wants to identify "the laptop in accounting" uses `device_id`; code that routes a config update to that device uses `thing_id`. Both identifiers are stable across agent restarts — only re-enrollment changes them (and re-enrollment changes `thing_id` while preserving `device_id`).

## Adding a new shadow key — standard procedure

When adding a new config key to any Thing type, the change must be consistent across five locations. The `configreconcile` watcher, the seed, and the migration are the three independent paths that all drift independently; checking only one of them causes the other two to diverge silently.

**Standard five-step procedure:**

1. Add the configKey constant to `packages/shared/schemas/configkey/keys.go` (+ update `ValidByThingType` and `TypedRegistry`).
2. Add the Go type definition to `packages/shared/schemas/configtypes/<area>/`. This struct is the schema contract for the key's JSON value.
3. Write a Prisma migration under `tools/db-migrate/prisma/migrations/` that `INSERT`s the `thing_config_template` row. For Cat B keys: ensure the JSONB blob contains `"needsPull": true`.
4. Mirror the same row in `tools/db-migrate/seed/seed.ts` (or `seed/data/seed-baseline.sql`). This keeps fresh-DB setups consistent with migration-upgraded DBs.
5. Register the `OnConfigChanged` callback in the consuming service's `main.go` (or `wiring/config.go`). Optionally: update the CP-side `configreconcile` watcher to cover the new key so drift is auto-healed.

After adding the key, verify on a local stack:
```bash
# Confirm the key is in the shadow after seed
psql $DB_URL -c "SELECT thing_id, desired FROM thing WHERE thing_type='ai-gateway' LIMIT 1;" | jq '.desired'

# Trigger a config change and confirm the Thing receives it
# The AI Gateway log should show: "config key applied: <key> version=1"
```

## WebSocket session lifecycle

The `thingclient` library manages the Hub connection lifecycle for each registered Thing:

1. **Connect.** On boot, the service calls `thingclient.Connect(hubURL, token, callbacks)`. The client establishes a WebSocket connection over TLS (mTLS for agents; bearer token for server services).
2. **Full shadow pull.** Hub sends the current full shadow snapshot for the Thing over the established WebSocket. The `thingclient` processes each key: Cat A values are applied immediately; Cat B keys trigger explicit HTTP pulls.
3. **Heartbeat loop.** After initial shadow sync, the `thingclient` sends heartbeats at a configured interval (default 10s). Each heartbeat carries the latest `reported` state map (`{ key: version }`).
4. **Change-signal receive.** When Hub emits a change-signal (key name + new version), the `thingclient` fires the registered `OnConfigChanged` callback for the key.
5. **Reconnect on drop.** If the WebSocket drops, the `thingclient` backs off exponentially and reconnects. On reconnect, Hub resends the full shadow snapshot — the Thing re-applies any keys that drifted during the disconnect.

The HTTP fallback path is identical in behavior: the `thingclient` polls for the full shadow snapshot over HTTP when the WebSocket is unavailable.

## Shadow versioning and convergence guarantee

Config key versions are monotonic per `(thing_id, config_key)` pair. The guarantee:

- Hub only ever increments versions. A version never goes backward.
- Each Thing reports the version it has **applied**, not the version it has received.
- Hub computes drift as `max(desired.version) > max(reported.version)` per key per Thing.
- If a Thing receives the same version twice (e.g., reconnect after a drop), the apply is idempotent — the callback is invoked but the result is the same as the first apply.

Cross-key ordering is not guaranteed. If an admin saves two keys in quick succession, the Things may apply them in any order. Services that have cross-key invariants (e.g., "hooks must be loaded before routing_rules for semantic correctness") must handle this at the callback level — typically by designing the keys to be independently meaningful.

## On-device config snapshot pattern

Every service that holds a Cat B config key maintains an `atomic.Pointer` to its current snapshot:

```go
// In a service's config holder:
var currentHooks atomic.Pointer[HookConfig]

// OnConfigChanged callback:
func applyHooksConfig(newConfig *HookConfig) error {
    if err := validate(newConfig); err != nil { return err }
    currentHooks.Store(newConfig)   // atomic swap — never blocks requests
    return nil
}

// In request handler:
hooks := currentHooks.Load()  // always returns a non-nil pointer (initial pull completed before traffic begins)
```

This pattern ensures:
1. Config reload never acquires a lock that could block in-flight requests.
2. Every request sees a consistent config snapshot (not a partial update).
3. If `validate` fails, the old snapshot stays active — no partial application.

## Reported outcomes and E27 audit

The `reported_outcomes` JSONB column (added in E27) records per-key apply outcomes including `processStartedAt`. This enables the Config Sync page to show not just whether a key is applied, but when the Thing last tried to apply it and whether the last apply succeeded.

A Thing stamps `processStartedAt` when it begins applying a key and `appliedAt` + `applyError` (nil or string) when the apply completes. This two-timestamp pattern lets Hub distinguish "never applied" (no `processStartedAt`), "apply in progress" (has `processStartedAt`, no `appliedAt`), "apply succeeded" (has both, `applyError` nil), and "apply failed" (has both, `applyError` non-nil).

---

## Canonical docs

- [`thing-model.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/cross-cutting/foundation/thing-model.md) — Thing data model, extension tables, storage schema, identifiers, status model, terminology boundary table
- [`thing-config-sync-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/cross-cutting/foundation/thing-config-sync-architecture.md) — pull-only sync mechanics, Cat A/B/C classification, apply path, failure modes, three-path audit invariant

**Adjacent wiki pages**: [Architecture Overview](Architecture-Overview) · [The Five Services](The-Five-Services) · [Control Plane Vs Data Plane](Control-Plane-Vs-Data-Plane) · [Fail Open Posture](Fail-Open-Posture) · [Hub Coordination](Hub-Coordination)

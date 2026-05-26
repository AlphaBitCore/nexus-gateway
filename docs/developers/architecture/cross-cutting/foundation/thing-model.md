# Thing model

## §1 — Purpose and scope

The Thing model is Nexus Gateway's internal service / device coordination kernel. Every running participant — the four backend services (`nexus-hub`, `control-plane`, `ai-gateway`, `compliance-proxy`) and every desktop `agent` — registers with Hub as a **Thing** and carries a **shadow** (Hub-managed desired state, Thing-reported applied state) for the lifetime of its process.

This document covers what the kernel is on disk **today**:

- the `thing` table + 1:1 `thing_service` / `thing_agent` extensions
- the shadow columns (`desired`, `reported`, `reported_outcomes`, `desired_ver`, `reported_ver`)
- the `thing_config_template` (per-type canonical state) + `thing_config_override` (per-Thing whole-key replacement) cascade, with the override blacklist
- Type A vs Type B configKey semantics (the two ways a key's `state` is interpreted)
- the three transport channels (WebSocket / HTTP / NATS JetStream) and how server vs agent Things use them differently
- the `OnConfigChangedFunc` callback contract every Thing must implement
- the Hub-side `fleet/` package layout that owns the registry, shadow, and overrides
- the version invariants that keep apply-once + idempotent across reconnects
- the **canonical** internal-vs-user terminology mapping (§10)

Vocabulary note: "Thing" and "shadow" are internal-narrative words. No code path imports an IoT SDK; the kernel is plain Go + PostgreSQL + NATS. The user-facing UI must translate to "node / config sync" — see §10.

## §2 — Thing entity (DB schema)

The `thing` table at `tools/db-migrate/schema.prisma` is the registry. Every Thing has a single row keyed by `id`.

**Five Thing types** (schema comment at `schema.prisma`, closed set enforced at `packages/shared/schemas/configkey/configkey_test.go`):

| `type` | `id` shape | Extension row |
|---|---|---|
| `nexus-hub` | yaml-configured id or `{hostname}-{type}-{port}` | `thing_service` |
| `control-plane` | same | `thing_service` |
| `ai-gateway` | same | `thing_service` |
| `compliance-proxy` | same | `thing_service` |
| `agent` | random UUID issued at first enroll; **same hardware reuses the existing `id`** via `physical_id` UNIQUE (see below) | `thing_agent` |

**Status state machine** (`schema.prisma`): `enrolled` → `online` ⇆ `offline` / `drift` / `revoked`. Default on insert is `enrolled`. Writers per value: `online` is set by `manager.RegisterThing` (called from HTTP enrollment + every WS connect) and by Hub's own `selfreg.Manager` at boot — the upsert's `ON CONFLICT` `CASE WHEN EXCLUDED.status='online' AND thing.status<>'online'` block also resets `process_started_at` and clears `reported_outcomes` whenever this transition fires; `offline` is set by `manager.MarkOffline` (called on WS disconnect) and by the periodic `jobs/defs/drift/stale_thing` job (`MarkStaleOffline`); `drift` is set by the periodic `jobs/defs/drift/drift` job when `desired_ver != reported_ver` on an `online` row and auto-flips back to `online` on the next catching-up `HandleShadowReport`; `revoked` is set by admin-driven revocation flows.

**Auth and connection** (`schema.prisma`): `auth_type` ∈ {`bearer`, `mtls`, `apikey`} default `bearer`; `conn_protocol` ∈ {`http`, `websocket`} default `http`. (The schema string also lists `mqtt` for historical reasons; no live code path produces it — confirmed by `grep -rE 'mqtt|MQTT' packages/ --include='*.go'` returning zero non-test hits.)

**Promoted identity columns** (`schema.prisma`): `hostname`, `primary_ip`, `os`, `os_version`, `tags`, `physical_id` were lifted out of the `metadata` jsonb so list / detail UIs can render and filter without crawling JSON. `physical_id` semantics differ by type:

- **agent**: 32-hex SHA-256 of `IOPlatformUUID + serial + MAC + cpu brand`, computed by `packages/shared/core/metrics/platform/fingerprint.go` (`computeFingerprintSignalsFn`, with `hardwareUUID` reading `IOPlatformUUID` from `ioreg` on darwin). A partial UNIQUE constraint `WHERE type='agent' AND physical_id IS NOT NULL` (migration `tools/db-migrate/migrations/20260521000000_thing_physical_id_column/`) is what makes the agent's `thing.id` stable across reinstalls — at first enroll Hub issues a random UUID; on re-enroll from the same hardware Hub matches the fingerprint and returns the existing row's `id`. Without this constraint the `thing.id` would be regenerated on every reinstall (schema comment notes "random thing.id; same Mac re-enrolls produce different IDs without this"). Hardware changes that perturb any of the four fingerprint inputs (motherboard swap, new primary NIC, OS-level UUID reset) WILL produce a new fingerprint and therefore a new `thing.id` — the stability is "stable per hardware identity", not "permanent".
- **services**: yaml-configured `id` or auto-derived `{hostname}-{type}-{port}`. The partial UNIQUE deliberately does not cover them; their PK is already deterministic.

**Indexes** (`schema.prisma`): `(type, status)` for fleet-view filters; `(status, last_seen_at)` for the offline-sweep job.

**ThingService extension** (`schema.prisma`, 1:1 with parent): `role` ∈ {api, scheduler, canary, worker} default `default`, `metrics_url` (Prometheus scrape), `management_url` (admin HTTP base).

**ThingAgent extension** (`schema.prisma`, 1:1 with parent): `cert_serial UNIQUE`, `cert_expires_at`, `previous_cert_serial`, `cert_renewed_at`, `sysinfo`, `trust_level Int default 0`, `current_assignment_id`. Trust level semantics at `schema.prisma`: `0=unknown, 1=enrolled, 2=identified (user linked), 3=compliant (cert valid + user linked)`.

**EnrollmentToken** (`schema.prisma`) carries the one-time secret that turns into a `thing` row; status flows `pending` → `used` | `expired` | `revoked`. The token is stored as `token_hash` (SHA-256 of the raw secret) so the DB never holds the bearer value.

## §3 — Shadow protocol (DB columns)

Six columns on `thing` carry the shadow state (`schema.prisma`):

| Column | Type | Written by | Read by |
|---|---|---|---|
| `desired` | jsonb default `{}` | Hub on every admin write — template path via `manager.UpdateConfig` → `UpdateDesiredForType` (per-type fanout, bumps `desired_ver` to `COALESCE(MAX(desired_ver),0)+1` across the whole type); override path via `manager.SetOverride`/`ClearOverride` → `WriteDesiredAndBumpVer` (per-Thing, `desired_ver+=1`) | Thing on connect (full snapshot via `ConnectedMessage.Desired`) + Thing's `OnConfigChanged` callback |
| `reported` | jsonb default `{}` | Hub when Thing sends `shadow_report` | Admin UI Nodes page; drift detection |
| `reported_outcomes` | jsonb default `{}` | Hub when Thing sends `shadow_report` with the `reportedOutcomes` field | Admin UI per-key apply-error / last-good-version indicators |
| `desired_ver` | BigInt default 0 | Hub, incremented on every push | Compared with `reported_ver` to detect drift |
| `reported_ver` | BigInt default 0 | Hub when Thing sends `shadow_report` | Same as above |
| `process_started_at` | timestamptz | Hub, captured on the offline→online transition | UI uptime; used to interpret `reported_outcomes` correctly across restarts |

`reported_outcomes` is `{key: {appliedAt, appliedVersion, applyError}}`. It is **reset to `{}` on Thing process restart** and repopulated by the next successful apply. Correlate with `process_started_at` to distinguish "fresh process, no apply yet" from "applied successfully a while ago" (`schema.prisma`).

`thing.desired` is the **merged wire-format cache** — it is recomputed by Hub whenever a template state, an override, or an override expiry changes. The cascade rule and its single source of truth are §4.

## §4 — ThingConfigTemplate + ThingConfigOverride cascade

The merged `thing.desired` is assembled from two tables.

**`thing_config_template`** (`schema.prisma`) carries the canonical per-(type, key) desired state. Composite PK `(type, config_key)`. Columns: `state` jsonb default `{}`, `version` BigInt default 1 (monotonic, incremented on every admin write), `updated_at`, `updated_by`. One row per legal `(thing_type, config_key)` tuple (see §5 for the legal set).

**`thing_config_override`** (`schema.prisma`) carries per-Thing whole-key replacements. Composite PK `(thing_id, config_key)`. Columns: `state` jsonb (REQUIRED, no default — admins must hand-write the override state), `template_ver_at_set` BigInt (snapshotted at override creation; the template_ver staleness predicate is `current template.version > template_ver_at_set`), `set_by`, `set_at`, `reason` varchar(500) (DB-level CHECK + handler validation), `expires_at` (NULL = permanent; non-NULL must satisfy `expires_at > set_at` via DB CHECK), `emergency_override` bool default `false` (true for break-glass writes — `configKey == "killswitch"` or `reason` starts with `break-glass:`).

**Cascade rule** (comment at `schema.prisma`):

```
thing.desired[k] = override[thing_id, k]  if present
                 = template[type, k]      otherwise
```

`thing.desired` is the merged result; the two source tables stay separate so override audit and template authorship don't intermix.

**Override blacklist** (`packages/shared/schemas/configtypes/policy/override_policy.go`): the CP admin handler MUST reject `400 BadRequest` on any attempt to override the following config keys:

- `credentials` — provider credentials are governed centrally; per-Thing divergence multiplies leak surface and breaks rotation semantics
- `virtual_keys` — VK is tenant identity / billing principal; the product requires globally consistent VK state

The blacklist is unexported on purpose; the contract is enforced via `IsOverridable(key) bool` (`override_policy.go`), with `IsBlacklisted` and `BlacklistedKeys` for the inverse predicate and read-only enumeration. Adding entries is a deliberate policy change and must update SDD + spec in the same PR (`override_policy.go`).

**Audit trail**: `ConfigChangeEvent` (`schema.prisma`) is insert-only. Written by Hub's config update handler when CP pushes a change. Fields cover `thing_type`, `config_key`, `action`, `actor_id`, `actor_name`, `new_state`, `new_version`, `source_ip`, `emergency_override`. The audit query path uses three indexes: `(thing_type, timestamp)`, `(config_key, timestamp)`, `(actor_id, timestamp)`.

## §5 — Type A vs Type B configKey semantics

Every configKey is one of two types. The distinction governs how the Thing's `OnConfigChanged` callback (§7) interprets the wire payload.

**Type A — state IS the config.** `ConfigState.State` carries the full desired blob; the callback applies it directly. Constants are listed at `packages/shared/schemas/configkey/configkey.go`:

`log_level`, `killswitch`, `ai_guard`, `cache`, `gateway_passthrough`, `agent_settings`, `diag_mode`, `onboarding`, `payload_capture` (agent variant only — ai-gateway and compliance-proxy receivers ignore the pushed state and re-read from `system_metadata` `payload_capture.config`, per the comment at `configkey.go`), `observability` (effectively Type B everywhere — every receiver re-reads from `system_metadata` `observability.config`, per `configkey.go`), `response_cache.time_sensitive_patterns`, `semantic_cache.config`, `response_cache.extract_config`.

**Type B — invalidation channel.** `ConfigState.State` is `null` / `{}`; the version bump is just a "go reload" signal. The actual data lives in a dedicated DB table named after the key. Constants at `configkey.go`:

`providers`, `models`, `credentials`, `routing_rules`, `virtual_keys` (carries a structured payload `{op:"invalidate", ids:[...]}` so the gateway can scope eviction instead of full reload — `configkey.go`), `quota_policies`, `quota_overrides`, `organizations`, `interception_domains`, `hooks`, `exemptions`, `streaming_compliance`, `credential_reliability`, `siem`, `installed_rule_packs`, `user_context`.

**Reload mechanics differ between service Things and agent Things:**

- **Service Thing** receives a Type B `config_changed` → reads the corresponding table from the same PostgreSQL it is already connected to.
- **Agent Thing** receives a Type B `config_changed` carrying a `{needsPull: true}` stub. The agent's `configloader.Loader` (registered via `RegisterRawPull`) detects the stub and calls an HTTP puller (`packages/agent/cmd/agent/configdispatch.go` `agentPullConfig`) → `GET /api/internal/things/config/<configKey>?type=agent` with `Authorization: Bearer <deviceToken>` and `X-Thing-Id: <thingID>` headers. The Hub-side handler is `SingleConfigPull` (`packages/nexus-hub/internal/fleet/handler/hubapi/internal_things.go`), which dispatches Cat B loader → template fallback. Loaders live in `packages/nexus-hub/internal/compliance/catbagent/` (one file per agent Type B key: `exemptions.go`, `hook_config.go`, `installed_rule_packs.go`, `interception_domains.go`, `payload_capture.go`, `streaming_compliance.go`, `user_context.go`); they are wired into the storage layer at `packages/nexus-hub/cmd/nexus-hub/wiring/storage.go`. (`packages/agent/internal/sync/shadow/snapshot.go` defines a `ConfigSnapshot` struct that is retained for offline-fallback persistence to local SQLCipher via `SaveConfigSnapshot`/`LoadLatestConfigSnapshot`; it is **not** the live-pull wire format.)

The agent never connects to PostgreSQL directly. Every byte of agent state comes from Hub over HTTP.

**Legal `(type, config_key)` tuples** are closed by `ValidByThingType` at `packages/shared/schemas/configkey/validation.go`:

| Thing type | Allowed config keys |
|---|---|
| `nexus-hub` | `log_level`, `observability` |
| `control-plane` | `log_level`, `observability` |
| `ai-gateway` | `log_level`, `observability`, `cache`, `ai_guard`, `gateway_passthrough`, `payload_capture`, `credential_reliability`, `providers`, `models`, `credentials`, `routing_rules`, `virtual_keys`, `quota_policies`, `quota_overrides`, `organizations`, `hooks`, `response_cache.time_sensitive_patterns`, `semantic_cache.config`, `response_cache.extract_config` |
| `compliance-proxy` | `log_level`, `observability`, `killswitch`, `onboarding`, `payload_capture`, `streaming_compliance`, `interception_domains`, `hooks`, `exemptions` |
| `agent` | `agent_settings`, `diag_mode`, `exemptions`, `hooks`, `interception_domains`, `payload_capture`, `streaming_compliance`, `killswitch`, `installed_rule_packs`, `user_context` |

`AuditTemplateRows` at `validation.go` scans `thing_config_template` at Hub startup and logs `WARN` per orphan tuple but does not fail boot — orphans can exist temporarily during a multi-PR migration.

`TypedRegistry` at `packages/shared/schemas/configkey/typed.go` maps Type A configKeys to the Go struct backing their state JSON. Currently every entry is `json.RawMessage` as a placeholder; typed-struct migration is per-key as receivers adopt shared types in `packages/shared/schemas/configtypes/`.

> **Terminology note**: the codebase uses "Type A / Type B" (`configkey.go`) and "Category A / Category B" (`shadow.go` callback docstring) interchangeably for the same concept. This document uses **Type A / Type B**; treat any "Category A/B" comment as a synonym.

## §6 — Three-channel transport

The kernel uses three distinct channels with different data and different owners. This is not a "primary / fallback" relationship — each channel carries a specific kind of traffic.

**WebSocket** (`packages/shared/transport/thingclient/client.go`). Bidirectional, persistent, primary for control traffic:

- Hub → Thing: `connected` (full shadow snapshot on connect, carrying `Desired` + `DesiredVer`), `config_changed` (per-key delta, carrying `ConfigKey` + `State` + `DesiredVer`). The hubMessage envelope can in principle carry either shape; emitter code paths today split cleanly — `connected` uses the snapshot shape, `config_changed` uses the per-key shape. The `Force` flag rides on `config_changed` to drive admin-triggered "Re-sync this key" replays where Hub does not bump the version but still wants the Thing to re-apply.
- Thing → Hub: `shadow_report` (per-key reported state + reported_ver + reported_outcomes), `shadow_report_break_glass` (extends shadow_report with `Reason`, `SourceIP`, `ActorTokenID`, `KeyVersions` for emergency overrides — `client.go`), and periodic heartbeat / metrics-sample frames.

**HTTP fallback** (`packages/shared/transport/thingclient/http.go`). Used when WS is unreachable; carries the same config + heartbeat traffic. Client mode is one of `ModeDisconnected | ModeWSConnecting | ModeWSConnected | ModeHTTPFallback` (`client.go`). Hub's device-token auth path requires every HTTP request to carry an `X-Thing-Id` header — without it the request is rejected `401 X-Thing-Id header required for device token auth` (per the comment at `http.go`).

**HTTP `UploadAudit` + `GET /api/agent/config`** — agent-only. The agent never publishes directly to NATS; instead it uploads audit / traffic-event batches via HTTP to Hub, and pulls Type B snapshots via `GET /api/agent/config`. The contract is asserted at `packages/shared/transport/thingclient/mq.go`:

```go
if c.cfg.MQProducer == nil {
    return fmt.Errorf("thingclient: MQ producer not configured (agent Things should use UploadAudit)")
}
```

**NATS JetStream** (`packages/shared/transport/mq/`). Server Things only. `Client.PublishEvent(ctx, queue, data)` at `mq.go` enqueues directly to the configured MQProducer; on failure the event lands in a bounded ring buffer for retry. Hub itself consumes the audit / traffic-event streams and is also the producer that re-publishes agent-uploaded HTTP batches into the same streams — that is how agent audit data joins the rest of the pipeline without the agent ever holding a NATS credential.

## §7 — Callback contract

Every Thing registers an `OnConfigChangedFunc` before `Start()` (type at `packages/shared/transport/thingclient/shadow.go`, registration method at `shadow.go`):

```go
type OnConfigChangedFunc func(desired map[string]ConfigState) (reported map[string]ConfigState, err error)
```

Where `ConfigState` is (`packages/shared/transport/thingclient/client.go`):

```go
type ConfigState struct {
    State   json.RawMessage `json:"state"`
    Version int64           `json:"version"`
}
```

**The callback's contract** (per the docstring at `shadow.go`, with Type A/B branching from §5):

1. Iterate `desired` map.
2. For each Type A key: apply directly from `ConfigState.State`.
3. For each Type B key: compare `ConfigState.Version` against the receiver's last-applied version for that key. On change:
   - Service Thing: read the corresponding DB table directly.
   - Agent Thing: detect the `{needsPull: true}` stub via the `configloader.RegisterRawPull` path and issue `GET /api/internal/things/config/<configKey>?type=agent` to Hub.
4. Build the `reported` map reflecting what was actually applied.
5. Return the `reported` map. Return an error **only** if the apply fundamentally failed; partial applies should still return the partial `reported` map (the Hub stamp on `reported_outcomes` is per-key, so a partial success is recorded accurately).

The callback is called synchronously on the client's internal goroutine; the receiver must not block on long IO (do that asynchronously and return the partial reported map immediately).

`applyConfig` at `shadow.go` enforces three apply invariants:

- **No callback registered → skip + log WARN** ("Config changed but no OnConfigChanged callback registered", `shadow.go`).
- **`desiredVer <= reportedVer` → skip + log "config_already_applied"** unless the `Force` flag is set on the Hub message (`shadow.go`, `Force` semantics at `client.go`).
- **callback returns error → log error, increment `configApplies("failure")`, do NOT send shadow_report** (`shadow.go`). The next push or reconnect snapshot will retry.

## §8 — Hub-side fleet packages

Everything Hub does with Things lives under `packages/nexus-hub/internal/fleet/`:

| Subpackage | Responsibility |
|---|---|
| `manager/` | Thing registry — `register.go` (enrollment intake), `query.go` (list / get), `config.go`, `enrichment.go` (metadata-from-heartbeat promotion), `trust_level.go`, `override_test.go` |
| `shadow/` | Shadow resolution + per-key push — `config_resolve.go` (template+override merge), `config_template.go` (template CRUD), `config_change_event.go` (audit write), `shadow_notify.go` (per-key WS push to a Thing), `handler.go` (admin shadow surface) |
| `overrides/` | `thing_config_override` CRUD — `thing_config_override.go`, `override_state.go`, `handler.go` |
| `handler/` | HTTP entry points; `handler/hubapi/` is the device-facing API surface |
| `smartgroup/` | Predicate-driven device-group definitions (`packages/shared/policy/device.Predicate` over a closed device-attribute set). Used by the bulk-by-group admin ops (force-refresh / rotate-cert) and by alert / exemption scoping. Device groups do **not** carry per-key config payloads — config flows are `thing_config_template` (fleet default) ⊕ `thing_config_override` (per-Thing) only. |
| `store/` | DB layer for the fleet domain |

Agent Type B loaders live in a sibling tree: `packages/nexus-hub/internal/compliance/catbagent/` (§5), wired in `packages/nexus-hub/cmd/nexus-hub/wiring/storage.go`.

## §9 — Version invariants

The shadow protocol's correctness comes from a few invariants enforced by Hub + Thing in lockstep:

- **`desired_ver` is monotonic on the Hub side.** Every admin write that changes `thing_config_template` or `thing_config_override` recomputes `thing.desired` and bumps `desired_ver`. The bump is atomic with the write.
- **`reported_ver` is monotonic on the Thing side.** Each successful callback round increments the Thing's local counter; the next `shadow_report` carries the new value. Hub trusts the Thing's monotonicity (it accepts only `>= current reported_ver`).
- **Apply predicate is `desiredVer > reportedVer`.** Equal versions mean "already applied"; smaller means a stale message overtook a fresher one. Equal-version applies are skipped (`shadow.go`).
- **`Force = true` bypasses the predicate.** Used by admin "Re-sync this key" replays where the template version isn't changing but the Thing's reported value drifted. The replay still goes through the same callback and still emits a `shadow_report` (`client.go`).
- **WS reconnect = full snapshot.** On every successful WS handshake Hub sends the entire desired map at the current `desired_ver`; the Thing's first action after a reconnect is to compare and either apply or skip. There is no partial-state recovery.
- **HTTP fallback heartbeat = version compare.** When the WS link is down the Thing polls Hub HTTP at heartbeat cadence, sending its `reported_ver`; Hub returns the desired snapshot only if its `desired_ver` is greater.

## §10 — Terminology boundary (canonical)

CLAUDE.md's "IoT terminology boundary" rule treats this section as the canonical mapping table. Enforcement is `npm run check:terminology` (script: `scripts/check-terminology.sh`).

| Internal (code, DB column, dev arch doc) | User-facing (admin UI copy, locales, product docs, public API responses) |
|---|---|
| Thing | node |
| Shadow | config sync |
| desired | target config |
| reported | applied config |
| drift | out of sync |

**Internal usage is allowed in:**

- Go code (struct names, method names, package paths — e.g. `Thing`, `ThingService`, `OnConfigChangedFunc`).
- DB column names — the `thing` table, the `thing_config_template` / `thing_config_override` tables, the `desired` / `reported` / `desired_ver` / `reported_ver` columns.
- Developer architecture docs (`docs/developers/architecture/**`).

**User-facing surfaces MUST use the right-hand column:**

- Admin UI copy (every CP-UI component, every `t()` call in `packages/control-plane-ui/src/`).
- Agent UI copy (every Agent-UI component, every `t()` call in `packages/agent/ui/`).
- Product docs (`docs/users/product/**`, `docs/users/features/**`).
- Public + admin API response field names (use `applied_config`, not `reported`; `target_config`, not `desired`).

The same mapping is referenced by `.cursor/rules/iot-terminology-boundary.mdc`; this doc is its source.

## References

- Schema — `tools/db-migrate/schema.prisma`
- Thing client — `packages/shared/transport/thingclient/`
- ConfigKey constants + validation — `packages/shared/schemas/configkey/`
- Override blacklist — `packages/shared/schemas/configtypes/policy/override_policy.go`
- Typed config payloads — `packages/shared/schemas/configtypes/`
- Hub fleet (registry, shadow, overrides) — `packages/nexus-hub/internal/fleet/`
- Hub agent Cat B loaders — `packages/nexus-hub/internal/compliance/catbagent/`
- Hub agent Cat B wiring — `packages/nexus-hub/cmd/nexus-hub/wiring/storage.go`
- Agent shadow snapshot parser — `packages/agent/internal/sync/shadow/snapshot.go`
- Terminology guard — `scripts/check-terminology.sh`
- Pre-edit rule (binding) — `CLAUDE.md` § "Pre-edit reading (3-doc rule)" + § "IoT terminology boundary"

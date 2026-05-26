---
doc: thing-model
area: cross-cutting
service: foundation
tier: 1
updated: 2026-05-20
---

# Thing Model — Internal Architecture Specification

> **Tier 1 architecture doc.** Read this before touching `packages/shared/transport/thingclient/**`, `packages/shared/schemas/configtypes/**`, the Hub Thing Registry, or any code that reads/writes the `thing` family of tables. For the runtime config-sync flow (Cat A/B keys, change-signal, pull-only), read `thing-config-sync-architecture.md` next.

This doc covers the **data model** and **terminology boundary**. Operational mechanics (how a Thing pulls, applies, and reports) live in `thing-config-sync-architecture.md`. Enrollment crypto (token → CSR → device cert) lives in `agent-enrollment-architecture.md`.

---

## 1. The Thing abstraction

Every managed entity in Nexus Gateway is a **Thing**: a row in the `thing` table with optional typed extension rows. There are five Thing types:

| Thing type | Extension table | Role |
|---|---|---|
| Hub (self) | `thing_service` | The platform ops center is itself a Thing (so the same registry / shadow contract applies uniformly). |
| Control Plane | `thing_service` | Admin API / BFF. |
| AI Gateway | `thing_service` | `/v1/*` traffic. |
| Compliance Proxy | `thing_service` | Transparent TLS proxy. |
| Agent | `thing_agent` | Desktop endpoint binary (macOS / Windows / Linux). |

Backend services share the `thing_service` extension (fields: `role`, `metrics_url`, `management_url`). Agents use `thing_agent` (fields: `cert_serial`, `cert_expires_at`, `previous_cert_serial`, `cert_renewed_at`, `sysinfo`, `trust_level`, `current_assignment_id`). The cross-cutting identity columns (`hostname`, `primary_ip`, `os`, `os_version`, `version`, `tags`) were promoted in Phase 1 of the thing-identity refactor to the parent `Thing` row so list/detail UIs do not have to join.

The unified registry — one model for all five types — is deliberate. It buys uniform observability, a single config shadow contract, and one enrollment + revocation lifecycle. Adding a new managed entity type means adding an extension table, not building a parallel system.

## 2. Identifiers

- `Thing.id` — globally unique. For services: `"<type>-<label>"` (e.g. `ai-gateway-ip-172-31-1-117.ec2.internal-3050`); for agents: UUID. Used in URLs, MQ envelopes, audit rows. Stable across restarts.
- `Thing.type` — `nexus-hub` | `control-plane` | `ai-gateway` | `compliance-proxy` | `agent`. Determines which `thing_config_template` rows apply.
- `Thing.natural_key` — typed identity per Thing type (e.g. service host:port, agent device UUID). Used by Hub to dedupe re-enrollments.

## 3. Status model

A Thing's `status` field (`Thing.status`) takes one of the values declared inline in `tools/db-migrate/schema.prisma`:

| Status | Meaning |
|---|---|
| `enrolled` | Initial state after registration — has never reported. |
| `online` | Recent heartbeat or active WebSocket session. |
| `offline` | No heartbeat / no WS for more than the staleness threshold (per Thing type). |
| `drift` | Reported state lags desired state beyond the drift retry budget. |
| `revoked` | Credentials revoked — Hub rejects any further traffic. |

Status transitions are computed in Hub, not stamped by the Thing. The Thing reports `last_seen_at` and the per-key `reported` / `reported_outcomes` snapshots; Hub decides the bucket via the stale-thing-sweep + drift-check jobs. This keeps the truth-of-status in one place.

## 4. Enrollment (header)

A Thing joins the registry through enrollment:

- **Backend services** boot with the shared `INTERNAL_SERVICE_TOKEN` env var (Bearer-auth for `thingclient` to Hub) and a `Thing.type` hint; Hub creates the `thing` row on first contact (self-register).
- **Agents** receive a one-time enrollment token from the admin UI; the agent submits a CSR; Hub signs it with its self-issued ECDSA P-256 CA and returns a device cert.

Full mechanics — token issuance, single-use enforcement, CSR validation, revocation — are in `agent-enrollment-architecture.md`.

## 5. Shadow (overview)

Each Thing has a **device shadow** — a Hub-side record of:

- **Desired state** — what the admin wants applied (hook configs, routing rules, kill-switch, agent settings, prompt-cache config, …).
- **Reported state** — what the Thing currently has applied. Includes per-key apply timestamp and per-key apply error (E27 outcomes: `applyError` + `processStartedAt`).

The gap between desired and reported is **drift**. Drift is observable in the CP "Config Sync" surface (user-facing terminology — see §10).

The shadow is keyed and versioned per **config key**. Keys fall into two categories (A inline vs B pull-on-signal) detailed in `thing-config-sync-architecture.md` §3.

## 6. Heartbeat & reported-state stamping

Things heartbeat over WebSocket while connected and over HTTP when the WS link is down. Each heartbeat includes:

- `applied_config` version map (per key).
- `apply_error` (per key, optional).
- `process_started_at` (per key — when the apply began, useful for distinguishing in-flight from finished applies).

Hub validates the report against the desired-state version and updates the drift surface.

## 7. Auditing the template ↔ OnConfigChanged invariant (binding)

When a new Thing-type field needs to participate in the shadow, three independent paths must stay aligned:

1. `packages/control-plane/internal/platform/configreconcile/` — **CP-side drift watchdog**. Periodically compares the Control Plane's source-of-truth config tables against the corresponding `thing.desired.<key>` and re-emits `Hub.NotifyConfigChange` to heal any divergence. Runs in CP, not Hub. Owns the *runtime repair* side of the invariant.
2. `tools/db-migrate/seed/seed.ts` — canonical Prisma seed used for fresh dev DBs and the seed-baseline snapshot. Owns the *factory defaults* side of the invariant.
3. `tools/db-migrate/migrations/**` — durable schema migrations. Owns the *durable schema* side of the invariant.

Auditing only `migrations/` produces false positives — `configreconcile` and `seed.ts` can drift independently. Audit #7 hit exactly this; see `feedback_thing_config_template_audit_paths`. Always check all three when reviewing a "shadow added/changed a key" change.

## 8. The #91 prod bug

A reminder that the pull-only invariant is binding: prod agent shadows were missing four registered Cat B keys, so the agents never pulled them and the apply receipt never closed the loop. Root cause: a registered Cat B key without `needsPull: true` is invisible to the pull path. Every Cat B key must carry `needsPull: true`. See `thing-config-sync-architecture.md` §3.

## 9. Storage

The Thing tables live in PostgreSQL:

- `thing` — core row (`id`, `type`, `status`, `last_seen_at`, `enrolled_at`, `updated_at`). **Shadow lives ON this row** as three JSONB columns:
  - `desired` — full desired-state snapshot pushed by Hub to this Thing.
  - `reported` — full applied-config snapshot last reported by the Thing, plus dynamic heartbeat data.
  - `reported_outcomes` — per-config-key outcome ledger `{ key: { appliedAt, appliedVersion, applyError } }` (E27).
  - Monotonic counters `desired_ver` / `reported_ver` close the per-key apply receipt loop.
- `thing_service` — backend service extension.
- `thing_agent` — desktop agent extension.
- `thing_config_template` — per-(type, config_key) canonical state (Cat A inline JSON; Cat B null). The Cat A/B classification is implicit — whether a `CatBLoader` is registered for the (type, key) tuple on the Hub side (`packages/nexus-hub/internal/storage/store/catb_loader.go`).
- `thing_config_override` — per-Thing overrides merged into `desired` by Hub before push.

There is **no** `thing_shadow` / `thing_shadow_reported` table — earlier revisions of this doc named them; the shadow has always been JSONB columns on the `thing` row itself. See `tools/db-migrate/schema.prisma` for the canonical definitions (`model Thing`).

## 10. Terminology boundary (binding)

The Thing Model is an **internal** architecture kernel. The vocabulary on its inside (this doc, code, DB) is **not** the vocabulary on its outside (admin UI, admin API responses, product docs, error messages).

| Internal (code / DB / dev docs) | External (UI / API / product docs / errors) |
|---|---|
| Thing | node / service / device (pick by context) |
| Shadow | config sync |
| desired | target config |
| reported | applied config |
| drift | out of sync |
| Cat A / Cat B | (do not surface) |
| pull-only | (do not surface) |

If you find an external surface using internal vocabulary, fix it. The `scripts/check-terminology.sh` CI guard enforces this for product / UI text.

## 11. Sources

- `packages/shared/transport/thingclient/` — client library used by every Thing to connect, heartbeat, and pull config.
- `packages/shared/schemas/configtypes/` — hand-maintained Go type definitions for shadow keys.
- `packages/nexus-hub/internal/fleet/manager/` + `internal/fleet/store/` — Hub-side Thing Registry + status engine.
- `packages/nexus-hub/internal/fleet/shadow/` — shadow store + change-signal dispatch.
- `packages/nexus-hub/internal/fleet/handler/hubapi/` — shadow CRUD HTTP API (`hub_api_overrides.go` etc.).
- `tools/db-migrate/schema.prisma` — `thing*` tables (`Thing`, `ThingService`, `ThingAgent`, `thing_config_template`, `thing_config_override`).
- `tools/db-migrate/seed/seed.ts` + `seed/data/seed-baseline.sql` — canonical seed (including default template rows).

## 12. Cross-references

- `thing-config-sync-architecture.md` — runtime config flow.
- `agent-enrollment-architecture.md` — enrollment crypto.
- `multi-endpoint-coordination-architecture.md` — golden flows across Things.
- `service-call-framework.md` — CP ↔ Hub HTTP API contracts.
- `mq-architecture.md` — MQ envelope conventions used for events emitted by Things.
